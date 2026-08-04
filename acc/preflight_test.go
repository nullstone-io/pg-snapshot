package acc

import (
	"fmt"
	"testing"

	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/scrub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A scrub rule naming a table that is not in the database has to fail the snapshot, for the same
// reason a rule naming a dropped column does: the user believes it is in force and it is not.
//
// It is also the check that catches a connection pointed at the wrong database. Nothing else does
// -- pg_class is world-readable, so an unprivileged role connected to an empty database reads zero
// tables and every other check passes vacuously.
func TestPreflightRejectsUnknownTable(t *testing.T) {
	pool, ctx := Connect(t)

	schema := "pgsnap_acc_preflight"
	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	}
	drop()
	t.Cleanup(drop)

	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE SCHEMA %s;
		CREATE TABLE %s.present (id int PRIMARY KEY, body text);`, schema, schema))
	require.NoError(t, err)

	preflight := export.Preflight{
		Introspector: export.Introspector{DB: pool},
		Salt:         "acc-salt",
		Config: scrub.Config{Version: 1, Tables: map[string]scrub.TableConfig{
			schema + ".present": {Columns: map[string]string{"body": "redact"}},
			schema + ".absent":  {Columns: map[string]string{"body": "null"}},
		}},
	}

	report, err := preflight.Run(ctx)
	require.Error(t, err, "a rule on a table that does not exist must fail the snapshot")
	assert.Contains(t, err.Error(), schema+".absent")

	// The database name is in the message because the likeliest cause is not a dropped table
	// but a connection to the wrong database
	require.NotNil(t, report)
	assert.NotEmpty(t, report.Database)
	assert.Contains(t, err.Error(), report.Database)

	// The rule that does resolve is not implicated
	assert.NotContains(t, err.Error(), schema+".present")
}

// An empty plan silently produces a valid, empty snapshot. Restoring one over a lower environment
// replaces a working database with nothing, so the export refuses to write it.
func TestPreflightRejectsEmptyDatabase(t *testing.T) {
	pool, ctx := Connect(t)

	// A role with no grants still reads every table out of pg_class, so an empty *database* is
	// the only way to reach an empty plan -- which is exactly the case being guarded.
	database := "pgsnap_acc_empty"
	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, database))
	}
	drop()
	t.Cleanup(drop)

	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, database))
	require.NoError(t, err)

	emptyURL, err := withDatabase(URL(), database)
	require.NoError(t, err)

	emptyPool, err := pg.Open(ctx, emptyURL, 1)
	require.NoError(t, err)
	defer emptyPool.Close()

	preflight := export.Preflight{
		Introspector: export.Introspector{DB: emptyPool},
		Salt:         "acc-salt",
		Config:       scrub.Config{Version: 1},
	}

	report, err := preflight.Run(ctx)
	require.Error(t, err, "an empty database must not produce a successful snapshot")
	assert.Contains(t, err.Error(), "no tables")
	assert.Contains(t, err.Error(), "POSTGRES_URL",
		"the message has to name the thing the operator should check")

	require.NotNil(t, report)
	assert.Equal(t, database, report.Database)
}
