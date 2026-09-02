package acc

import (
	"fmt"
	"testing"

	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/restore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot is dumped --no-acl and the staging database is born with no grants, so every other
// consumer's access lives in the target and only in the target. This is the test that shows a
// reader role still reads after the swap -- and reads the table a migration added, which the
// target's explicit grants can say nothing about.
func TestCarryPrivileges(t *testing.T) {
	pool, ctx := Connect(t)

	const owner, reader = "pgsnap_acc_priv_owner", "pgsnap_acc_priv_reader"

	target, staging, dropDatabases := twoDatabases(t, ctx, pool, "pgsnap_acc_priv")
	// DROP ROLE refuses while the role still holds privileges in any database, so the databases
	// have to go first
	dropRoles := func() {
		for _, r := range []string{owner, reader} {
			pool.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, r))
		}
	}
	dropRoles()
	t.Cleanup(func() {
		dropDatabases()
		dropRoles()
	})

	// Both databases are owned by the owner role, exactly as the restore creates staging: it is
	// what lets the owner create in public on postgres 15+, and what makes pg_database_owner
	// resolve to a role the carry can borrow
	require.NoError(t, execAll(ctx, pool,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'acc-password'`, owner),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'acc-password'`, reader),
		fmt.Sprintf(`ALTER DATABASE %s OWNER TO %s`, target, owner),
		fmt.Sprintf(`ALTER DATABASE %s OWNER TO %s`, staging, owner),
	))

	targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)
	asOwner := func(url string) string {
		u, err := withUser(url, owner, "acc-password")
		require.NoError(t, err)
		return u
	}
	asReader := func(url string) string {
		u, err := withUser(url, reader, "acc-password")
		require.NoError(t, err)
		return u
	}

	// The target as it exists before the restore: the schema, and every kind of grant the reader
	// was given on it
	execInDatabase(t, ctx, asOwner(targetURL),
		`CREATE TABLE original(id int PRIMARY KEY, secret text)`,
		`CREATE TABLE dropped_by_migration(id int)`,
		`CREATE SEQUENCE counter`,
		`CREATE FUNCTION answer() RETURNS int LANGUAGE sql AS 'SELECT 42'`,
		`CREATE SCHEMA reports`,
		`CREATE TABLE reports.summary(id int)`,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, reader),
		fmt.Sprintf(`GRANT SELECT (id) ON original TO %s`, reader),
		fmt.Sprintf(`GRANT SELECT ON dropped_by_migration TO %s`, reader),
		fmt.Sprintf(`GRANT USAGE ON SEQUENCE counter TO %s`, reader),
		fmt.Sprintf(`GRANT EXECUTE ON FUNCTION answer() TO %s WITH GRANT OPTION`, reader),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA reports TO %s`, reader),
		fmt.Sprintf(`GRANT SELECT ON reports.summary TO %s`, reader),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT ON TABLES TO %s`, owner, reader),
	)

	// Staging as the restore leaves it: the same schema, less a table the migration dropped, plus
	// one it added -- and no grants at all
	execInDatabase(t, ctx, asOwner(stagingURL),
		`CREATE TABLE original(id int PRIMARY KEY, secret text)`,
		`INSERT INTO original VALUES (1, 'hidden')`,
		`CREATE TABLE added_by_migration(id int)`,
		`CREATE SEQUENCE counter`,
		`CREATE FUNCTION answer() RETURNS int LANGUAGE sql AS 'SELECT 42'`,
		`CREATE SCHEMA reports`,
		`CREATE TABLE reports.summary(id int)`,
	)

	log, logged := captureLogger()
	require.NoError(t, (restore.Privileges{Log: log}).
		Carry(ctx, poolFor(t, ctx, targetURL), poolFor(t, ctx, stagingURL)))

	// The dropped table's grant has nowhere to go, and nothing else would say so
	assert.Contains(t, logged.String(), "were not carried")
	assert.Contains(t, logged.String(), "public.dropped_by_migration")

	readerPool, err := pg.Open(ctx, asReader(stagingURL), 1)
	require.NoError(t, err)
	defer readerPool.Close()

	t.Run("explicit grants are carried exactly", func(t *testing.T) {
		var id int
		require.NoError(t, readerPool.QueryRow(ctx, `SELECT id FROM original`).Scan(&id),
			"the column-level SELECT must be carried")

		var secret string
		err := readerPool.QueryRow(ctx, `SELECT secret FROM original`).Scan(&secret)
		require.Error(t, err, "a column-level grant must not be widened to the whole table")
		assert.Contains(t, err.Error(), "permission denied")

		var n int64
		require.NoError(t, readerPool.QueryRow(ctx, `SELECT nextval('counter')`).Scan(&n),
			"USAGE on the sequence must be carried")

		var count int
		require.NoError(t, readerPool.QueryRow(ctx, `SELECT count(*) FROM reports.summary`).Scan(&count),
			"USAGE on a second schema and SELECT on its table must both be carried")
	})

	t.Run("grant option is carried", func(t *testing.T) {
		stagingPool := poolFor(t, ctx, stagingURL)
		var acl string
		require.NoError(t, stagingPool.QueryRow(ctx,
			`SELECT proacl::text FROM pg_proc WHERE proname = 'answer'`).Scan(&acl))
		assert.Contains(t, acl, reader+"=X*/")
	})

	t.Run("a table the migration added gets the default privileges", func(t *testing.T) {
		var count int
		require.NoError(t, readerPool.QueryRow(ctx, `SELECT count(*) FROM added_by_migration`).Scan(&count),
			"the target's ALTER DEFAULT PRIVILEGES rule is what should cover a table it never saw")
	})

	t.Run("default privileges apply after the restore", func(t *testing.T) {
		// Created as the owner, since default privileges attach at creation by the creating role
		execInDatabase(t, ctx, asOwner(stagingURL), `CREATE TABLE created_later(id int)`)

		var count int
		require.NoError(t, readerPool.QueryRow(ctx, `SELECT count(*) FROM created_later`).Scan(&count),
			"the rule itself must be carried, not only its effect on existing tables")
	})
}

// A database with nothing granted is the common case for a fresh environment, and it has to be a
// no-op rather than an error.
func TestCarryPrivilegesNothingToCarry(t *testing.T) {
	pool, ctx := Connect(t)
	target, staging, cleanup := twoDatabases(t, ctx, pool, "pgsnap_acc_priv_empty")
	defer cleanup()

	targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)
	execInDatabase(t, ctx, targetURL, `CREATE TABLE original(id int PRIMARY KEY)`)
	execInDatabase(t, ctx, stagingURL, `CREATE TABLE original(id int PRIMARY KEY)`)

	log, logged := captureLogger()
	require.NoError(t, (restore.Privileges{Log: log}).
		Carry(ctx, poolFor(t, ctx, targetURL), poolFor(t, ctx, stagingURL)))
	assert.NotContains(t, logged.String(), "were not carried")
}

// A global default-privilege rule attaches to objects as they are created, so carrying it before
// the schema restore is what makes every restored table come out already granted. This is the
// production shape: applications' own rules grant the database owner everything they create, and
// the restored copy has to reproduce that at creation time, not patch it afterwards.
func TestCarryDefaultsBeforeSchema(t *testing.T) {
	pool, ctx := Connect(t)

	const owner, reader = "pgsnap_acc_defaults_owner", "pgsnap_acc_defaults_reader"

	target, staging, dropDatabases := twoDatabases(t, ctx, pool, "pgsnap_acc_defaults")
	dropRoles := func() {
		for _, r := range []string{owner, reader} {
			pool.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, r))
		}
	}
	dropRoles()
	t.Cleanup(func() {
		dropDatabases()
		dropRoles()
	})

	require.NoError(t, execAll(ctx, pool,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'acc-password'`, owner),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'acc-password'`, reader),
		fmt.Sprintf(`ALTER DATABASE %s OWNER TO %s`, target, owner),
		fmt.Sprintf(`ALTER DATABASE %s OWNER TO %s`, staging, owner),
	))

	targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)
	ownerStagingURL, err := withUser(stagingURL, owner, "acc-password")
	require.NoError(t, err)
	readerStagingURL, err := withUser(stagingURL, reader, "acc-password")
	require.NoError(t, err)

	// The target's rule is global -- no IN SCHEMA -- which is the form postgres-access writes
	execInDatabase(t, ctx, targetURL,
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT SELECT ON TABLES TO %s`, owner, reader),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE ON SEQUENCES TO %s`, owner, reader))

	// Staging is empty, exactly as it is when this phase runs in a restore
	log, logged := captureLogger()
	require.NoError(t, (restore.Privileges{Log: log}).
		CarryDefaults(ctx, poolFor(t, ctx, targetURL), poolFor(t, ctx, stagingURL)))
	assert.Contains(t, logged.String(), "default privileges carried")

	// Stands in for pg_restore: objects created by the owner after the rule was applied
	execInDatabase(t, ctx, ownerStagingURL,
		`CREATE TABLE restored(id int PRIMARY KEY)`,
		`CREATE SEQUENCE restored_seq`)

	readerPool, err := pg.Open(ctx, readerStagingURL, 1)
	require.NoError(t, err)
	defer readerPool.Close()

	var count int
	require.NoError(t, readerPool.QueryRow(ctx, `SELECT count(*) FROM restored`).Scan(&count),
		"a table created after the global rule was carried must already be readable")

	var n int64
	err = readerPool.QueryRow(ctx, `SELECT nextval('restored_seq')`).Scan(&n)
	require.Error(t, err, "a schema-scoped rule cannot run before the schema restore and must wait for Carry")

	// The later carry picks up the schema-scoped rule, and must not trip over the global one
	require.NoError(t, (restore.Privileges{Log: log}).
		Carry(ctx, poolFor(t, ctx, targetURL), poolFor(t, ctx, stagingURL)))
	require.NoError(t, readerPool.QueryRow(ctx, `SELECT nextval('restored_seq')`).Scan(&n),
		"the schema-scoped rule is applied by hand to what already exists")
}
