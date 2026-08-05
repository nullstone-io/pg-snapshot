package acc

import (
	"fmt"
	"testing"

	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/restore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two schema-independent publication forms are the ones worth being exact about: they store no
// table references and resolve membership at decode time, so carrying them across a swap has to
// survive migrations that changed the schema underneath.
func TestCarryPublicationSchemaIndependentForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		ddl  string
	}{
		{"for all tables", "FOR ALL TABLES"},
		{"for tables in schema", "FOR TABLES IN SCHEMA public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := Connect(t)
			target, staging, cleanup := twoDatabases(t, ctx, pool, "pgsnap_acc_pub_form")
			defer cleanup()

			targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)

			// The target as it exists before the restore: a publication and the schema it was
			// written against
			execInDatabase(t, ctx, targetURL,
				`CREATE TABLE original(id int PRIMARY KEY)`,
				fmt.Sprintf(`CREATE PUBLICATION carried %s`, tc.ddl))

			// Staging as the restore leaves it: the same schema plus a table a migration added
			execInDatabase(t, ctx, stagingURL,
				`CREATE TABLE original(id int PRIMARY KEY)`,
				`CREATE TABLE added_by_migration(id int PRIMARY KEY)`)

			require.NoError(t, (restore.Publications{Dir: t.TempDir(), Log: discardLogger()}).
				Carry(ctx, targetURL, stagingURL))

			stagingPool, err := pg.Open(ctx, stagingURL, 1)
			require.NoError(t, err)
			defer stagingPool.Close()

			var covered []string
			rows, err := stagingPool.Query(ctx,
				`SELECT tablename FROM pg_publication_tables WHERE pubname = 'carried' ORDER BY 1`)
			require.NoError(t, err)
			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))
				covered = append(covered, name)
			}
			rows.Close()

			// The point of these forms: the migration's table is replicated without anyone having
			// to update the publication
			assert.Equal(t, []string{"added_by_migration", "original"}, covered)
		})
	}
}

// The enumerated form is the secondary path. It has to be carried exactly -- column lists and row
// filters included -- and it has to report the coverage it cannot have.
func TestCarryPublicationEnumeratedForm(t *testing.T) {
	pool, ctx := Connect(t)
	target, staging, cleanup := twoDatabases(t, ctx, pool, "pgsnap_acc_pub_enum")
	defer cleanup()

	targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)

	execInDatabase(t, ctx, targetURL,
		`CREATE TABLE orders(id int PRIMARY KEY, tenant text, secret text)`,
		`CREATE PUBLICATION carried FOR TABLE orders (id, tenant) WHERE (tenant = 'acme')
		   WITH (publish = 'insert,update', publish_via_partition_root = true)`)

	execInDatabase(t, ctx, stagingURL,
		`CREATE TABLE orders(id int PRIMARY KEY, tenant text, secret text)`,
		`CREATE TABLE added_by_migration(id int PRIMARY KEY)`)

	log, logged := captureLogger()
	publications := restore.Publications{Dir: t.TempDir(), Log: log}
	require.NoError(t, publications.Carry(ctx, targetURL, stagingURL))

	stagingPool, err := pg.Open(ctx, stagingURL, 1)
	require.NoError(t, err)
	defer stagingPool.Close()

	// Every attribute survives, which is why this uses pg_dump rather than a hand-written serializer
	var attnames []string
	var rowfilter string
	require.NoError(t, stagingPool.QueryRow(ctx,
		`SELECT attnames, rowfilter FROM pg_publication_tables
		 WHERE pubname = 'carried' AND tablename = 'orders'`).Scan(&attnames, &rowfilter))
	assert.Equal(t, []string{"id", "tenant"}, attnames)
	assert.Contains(t, rowfilter, "acme")

	var insert, update, del, viaRoot bool
	require.NoError(t, stagingPool.QueryRow(ctx,
		`SELECT pubinsert, pubupdate, pubdelete, pubviaroot FROM pg_publication
		 WHERE pubname = 'carried'`).Scan(&insert, &update, &del, &viaRoot))
	assert.True(t, insert)
	assert.True(t, update)
	assert.False(t, del, "publish parameters must be carried, not defaulted")
	assert.True(t, viaRoot)

	// The migration's table is outside an enumerated list, and nothing else would say so. The
	// warning is the whole behaviour here, so assert it was actually emitted rather than that the
	// call merely returned.
	require.NoError(t, publications.ReportDrift(ctx, stagingPool))
	assert.Contains(t, logged.String(), "publication does not cover every table")
	assert.Contains(t, logged.String(), "added_by_migration")
	assert.NotContains(t, logged.String(), "orders",
		"the table the publication does cover must not be reported as missing")
}

// The schema-independent forms cover everything by construction, so reporting drift for them would
// be noise on every restore.
func TestReportDriftSilentForSchemaIndependentForms(t *testing.T) {
	pool, ctx := Connect(t)
	target, staging, cleanup := twoDatabases(t, ctx, pool, "pgsnap_acc_pub_nodrift")
	defer cleanup()

	targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)

	execInDatabase(t, ctx, targetURL,
		`CREATE TABLE original(id int PRIMARY KEY)`,
		`CREATE PUBLICATION carried FOR TABLES IN SCHEMA public`)
	execInDatabase(t, ctx, stagingURL,
		`CREATE TABLE original(id int PRIMARY KEY)`,
		`CREATE TABLE added_by_migration(id int PRIMARY KEY)`)

	log, logged := captureLogger()
	publications := restore.Publications{Dir: t.TempDir(), Log: log}
	require.NoError(t, publications.Carry(ctx, targetURL, stagingURL))

	stagingPool, err := pg.Open(ctx, stagingURL, 1)
	require.NoError(t, err)
	defer stagingPool.Close()

	require.NoError(t, publications.ReportDrift(ctx, stagingPool))
	assert.NotContains(t, logged.String(), "does not cover every table")
}

// A publication naming a table the restored schema does not have must fail, and it must fail here
// rather than after the swap -- which is the reason this step runs while the target is untouched.
func TestCarryPublicationFailsOnMissingTable(t *testing.T) {
	pool, ctx := Connect(t)
	target, staging, cleanup := twoDatabases(t, ctx, pool, "pgsnap_acc_pub_missing")
	defer cleanup()

	targetURL, stagingURL := dbURL(t, target), dbURL(t, staging)

	execInDatabase(t, ctx, targetURL,
		`CREATE TABLE dropped_by_migration(id int PRIMARY KEY)`,
		`CREATE PUBLICATION carried FOR TABLE dropped_by_migration`)

	execInDatabase(t, ctx, stagingURL, `CREATE TABLE kept(id int PRIMARY KEY)`)

	err := (restore.Publications{Dir: t.TempDir(), Log: discardLogger()}).
		Carry(ctx, targetURL, stagingURL)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "carried")
}

// A slot is bound to a database OID and the swap does not move it. Rebinding is drop-and-recreate,
// and what has to hold is that the new slot belongs to the database that is now the target.
func TestRebindReplicationSlot(t *testing.T) {
	pool, ctx := Connect(t)

	target := "pgsnap_acc_slot_target"
	backup := "pgsnap_acc_slot_backup"
	drop := func() {
		pool.Exec(ctx, `SELECT pg_drop_replication_slot('pgsnap_acc_slot')`)
		for _, db := range []string{target, backup} {
			pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, db))
		}
	}
	drop()
	t.Cleanup(drop)

	// `backup` stands in for the database the swap renamed away, still owning the slot
	require.NoError(t, execAll(ctx, pool,
		fmt.Sprintf(`CREATE DATABASE %s`, backup),
		fmt.Sprintf(`CREATE DATABASE %s`, target),
	))

	backupURL := dbURL(t, backup)
	backupPool, err := pg.Open(ctx, backupURL, 1)
	require.NoError(t, err)
	_, err = backupPool.Exec(ctx,
		`SELECT pg_create_logical_replication_slot('pgsnap_acc_slot', 'pgoutput')`)
	require.NoError(t, err)
	backupPool.Close()

	slotter := restore.Slots{Admin: pool, Log: discardLogger()}

	captured, err := slotter.Capture(ctx, backup)
	require.NoError(t, err)
	require.Len(t, captured, 1)
	assert.Equal(t, "pgoutput", captured[0].Plugin, "the plugin has to be carried, not assumed")

	slotter.Rebind(ctx, dbURL(t, target), captured)

	var database string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT database FROM pg_replication_slots WHERE slot_name = 'pgsnap_acc_slot'`).
		Scan(&database))
	assert.Equal(t, target, database, "the slot must now belong to the swapped-in database")
}

// Rebinding is best-effort by design: after the swap the database is live and correct, and a slot
// that cannot be recreated is a broken pipeline rather than a broken restore.
func TestRebindReportsRatherThanFails(t *testing.T) {
	pool, ctx := Connect(t)

	target := "pgsnap_acc_slot_absent"
	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, target))
	}
	drop()
	t.Cleanup(drop)

	require.NoError(t, execAll(ctx, pool, fmt.Sprintf(`CREATE DATABASE %s`, target)))

	// A plugin the server does not have: the create fails, and Rebind has no error to return
	assert.NotPanics(t, func() {
		restore.Slots{Admin: pool, Log: discardLogger()}.Rebind(ctx, dbURL(t, target),
			[]restore.Slot{{Name: "pgsnap_acc_missing_plugin", Plugin: "no_such_plugin"}})
	})

	var slots int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'pgsnap_acc_missing_plugin'`).
		Scan(&slots))
	assert.Equal(t, 0, slots)
}

// CanCreate gates the pre-swap warning, so it has to reflect the actual role rather than assume.
func TestSlotsCanCreate(t *testing.T) {
	pool, ctx := Connect(t)

	ok, err := restore.Slots{Admin: pool}.CanCreate(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "the acceptance suite connects as a superuser")
}
