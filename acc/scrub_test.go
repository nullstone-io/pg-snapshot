package acc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/scrub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The projections are assembled as SQL strings, so the only way to know postgres accepts them is
// to hand them to postgres.
func TestScrubbedExport(t *testing.T) {
	pool, ctx := Connect(t)

	schema := "pgsnap_acc"
	_, err := pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	})

	_, err = pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s.users (
			id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email       varchar(30) NOT NULL UNIQUE,
			last_name   text,
			ssn         text,
			notes       text,
			-- generated columns are refused by COPY and must be excluded from the projection
			display     text GENERATED ALWAYS AS (last_name || ' <' || email || '>') STORED
		)`, schema))
	require.NoError(t, err)

	_, err = pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.users (email, last_name, ssn, notes) VALUES
			('a@example.com', 'Alpha', '111-11-1111', 'first'),
			('b@example.com', 'Beta',  '222-22-2222', 'second')`, schema))
	require.NoError(t, err)

	tables, err := export.Introspector{DB: pool}.Tables(ctx)
	require.NoError(t, err)

	cfg := scrub.Config{Version: 1, Tables: map[string]scrub.TableConfig{
		schema + ".users": {Columns: map[string]string{
			"email":     "email",
			"ssn":       "null",
			"last_name": "md5",
			"notes":     "redact",
		}},
	}}

	var projection *scrub.Projection
	for _, tbl := range export.Exportable(tables) {
		if tbl.Schema == schema && tbl.Name == "users" {
			projection, err = scrub.BuildProjection(tbl, cfg, "acc-salt")
			require.NoError(t, err)
		}
	}
	require.NotNil(t, projection, "users table was not discovered by introspection")

	// Identity columns are exported; generated columns are not
	assert.Contains(t, projection.Columns, "id")
	assert.NotContains(t, projection.Columns, "display")

	var out bytes.Buffer
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	_, err = conn.Conn().PgConn().CopyTo(ctx, &out, projection.CopyOut())
	require.NoError(t, err, "postgres rejected the scrubbed projection")

	body := out.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, lines, 2)

	// Nothing sensitive survives
	assert.NotContains(t, body, "111-11-1111")
	assert.NotContains(t, body, "222-22-2222")
	assert.NotContains(t, body, "a@example.com")
	assert.NotContains(t, body, "Alpha")
	assert.NotContains(t, body, "first")

	// The email transform fits its varchar(30) and stays unique, so the UNIQUE index still builds
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		require.Len(t, fields, 5)
		assert.LessOrEqual(t, len(fields[1]), 30)
		assert.Contains(t, fields[1], "@example.invalid")
		assert.Equal(t, `\N`, fields[3], "ssn should be null")
		assert.Equal(t, "REDACTED", fields[4])
	}
	assert.NotEqual(t, strings.Split(lines[0], "\t")[1], strings.Split(lines[1], "\t")[1],
		"email transform must stay unique or the UNIQUE index fails during post-data")
}

// The whole design rests on row_security = off turning a filtered read into a hard error rather
// than a silently short export.
func TestRowSecurityFailsLoud(t *testing.T) {
	pool, ctx := Connect(t)

	schema := "pgsnap_acc_rls"
	role := "pgsnap_acc_reader"

	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		pool.Exec(ctx, fmt.Sprintf(`DROP OWNED BY %s`, role))
		pool.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	}
	drop()
	t.Cleanup(drop)

	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema))
	require.NoError(t, err)

	_, err = pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s.secrets (id int PRIMARY KEY, body text);
		INSERT INTO %s.secrets VALUES (1, 'classified');
		ALTER TABLE %s.secrets ENABLE ROW LEVEL SECURITY;`, schema, schema, schema))
	require.NoError(t, err)

	// A role shaped like the one the snapshot module mints: SELECT through an ordinary grant,
	// no ownership, no pg_read_all_data. That is what leaves RLS functioning as a real control.
	_, err = pool.Exec(ctx, fmt.Sprintf(`
		CREATE ROLE %s LOGIN PASSWORD 'acc-password';
		GRANT USAGE ON SCHEMA %s TO %s;
		GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s;`, role, schema, role, schema, role))
	require.NoError(t, err)

	readerURL, err := withUser(URL(), role, "acc-password")
	require.NoError(t, err)

	readerPool, err := pg.Open(ctx, readerURL, 1)
	require.NoError(t, err)
	defer readerPool.Close()

	conn, err := readerPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	// Without this the read silently returns zero rows and the snapshot looks successful
	t.Run("without the guard, RLS silently filters", func(t *testing.T) {
		var out bytes.Buffer
		_, err := conn.Conn().PgConn().CopyTo(ctx, &out,
			fmt.Sprintf(`COPY (SELECT * FROM %s.secrets) TO STDOUT`, schema))

		require.NoError(t, err)
		assert.Empty(t, strings.TrimSpace(out.String()),
			"this is the failure mode the guard exists to prevent: no error, no rows")
	})

	t.Run("with the guard, it is a hard error", func(t *testing.T) {
		_, err := conn.Exec(ctx, `SET row_security = off`)
		require.NoError(t, err)

		var out bytes.Buffer
		_, err = conn.Conn().PgConn().CopyTo(ctx, &out,
			fmt.Sprintf(`COPY (SELECT * FROM %s.secrets) TO STDOUT`, schema))

		require.Error(t, err, "a filtered read must fail rather than export fewer rows")
		assert.Contains(t, err.Error(), "row-level security")
		assert.NotContains(t, out.String(), "classified")
	})

	// And the preflight has to see it coming, before any data moves
	t.Run("preflight reports it with a remediation", func(t *testing.T) {
		report, err := export.Preflight{
			Introspector: export.Introspector{DB: readerPool},
			Config:       scrub.Config{Version: 1},
			Salt:         "acc-salt",
		}.Run(ctx)
		require.NoError(t, err)

		err = report.Err()
		require.Error(t, err)
		assert.Contains(t, err.Error(), schema+".secrets")
		assert.Contains(t, err.Error(), "No data was exported.")
		assert.Contains(t, err.Error(), "table_owner_roles")
	})
}
