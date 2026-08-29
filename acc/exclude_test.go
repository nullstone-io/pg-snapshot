package acc

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/scrub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pg_dump takes an ACCESS SHARE lock on every table it dumps, --schema-only included, and that
// lock needs SELECT on the table. One table the snapshot role cannot read therefore kills the
// dump of all the others: the failure is a single LOCK TABLE naming every table at once.
//
// mode: skip is the answer, and this is the test that says so -- the same dump, by the same role,
// against the same database, failing without the exclusion and succeeding with it. It is a claim
// about what pg_dump does with --exclude-table, so it takes a real pg_dump to settle.
func TestDumpSchemaExcludesUnreadableTable(t *testing.T) {
	pool, ctx := Connect(t)

	database := "pgsnap_acc_exclude"
	role := "pgsnap_acc_exclude_reader"

	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, database))
		pool.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	}
	drop()
	t.Cleanup(drop)

	// Its own database rather than a schema in the shared one: pg_dump reads the whole database,
	// so another test's fixture would be another table this role cannot read
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, database))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'acc-password'`, role))
	require.NoError(t, err)

	ownerURL := dbURL(t, database)

	// public.restricted is owned by another role and never granted -- the shape of the table that
	// turns up in a production database owned by a service that predates the snapshot role
	execInDatabase(t, ctx, ownerURL,
		`CREATE TABLE public.readable (id int PRIMARY KEY, body text)`,
		`CREATE TABLE public.restricted (id int PRIMARY KEY, body text)`,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, role),
		fmt.Sprintf(`GRANT SELECT ON public.readable TO %s`, role),
	)

	readerURL, err := withUser(ownerURL, role, "acc-password")
	require.NoError(t, err)

	t.Run("without the exclusion the whole dump fails", func(t *testing.T) {
		err := pg.DumpSchema(ctx, readerURL, filepath.Join(t.TempDir(), "schema.dump"), "", nil)

		require.Error(t, err, "this is the failure mode mode: skip exists to resolve")
		assert.Contains(t, err.Error(), "permission denied")
		assert.Contains(t, err.Error(), "restricted")
	})

	t.Run("with it the rest of the schema dumps", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "schema.dump")
		require.NoError(t, pg.DumpSchema(ctx, readerURL, path, "",
			[]string{pg.ExcludeTablePattern("public", "restricted")}))

		toc, err := pg.ListArchive(ctx, path)
		require.NoError(t, err)
		listing := strings.Join(toc, "\n")

		// The table is gone from the artifact, not merely unlocked: a restore of this dump
		// produces a database without it, which is what mode: skip promises
		assert.Contains(t, listing, "readable")
		assert.NotContains(t, listing, "restricted")
	})

	readerPool := poolFor(t, ctx, readerURL)
	runPreflight := func(mode scrub.TableMode) (*export.Report, error) {
		return export.Preflight{
			Introspector: export.Introspector{DB: readerPool},
			Config: scrub.Config{Version: 1, Tables: map[string]scrub.TableConfig{
				"public.restricted": {Mode: mode},
			}},
			Salt: "acc-salt",
		}.Run(ctx)
	}

	// The incident these two modes came out of. A single skip mode meant preflight had to guess
	// which one the user wanted, guessed "no access needed", and handed the table to pg_dump
	// anyway -- a clean preflight followed by a dump that died on it milliseconds later.
	t.Run("preflight reports the unreadable table under skip-data", func(t *testing.T) {
		report, err := runPreflight(scrub.TableModeSkipData)
		require.NoError(t, err)

		err = report.Err()
		require.Error(t, err, "a table whose structure is still dumped still needs SELECT")
		assert.Contains(t, err.Error(), "public.restricted")
		assert.Contains(t, err.Error(), "mode: skip", "the message has to name the mode that works")
	})

	t.Run("preflight passes under skip", func(t *testing.T) {
		report, err := runPreflight(scrub.TableModeSkip)
		require.NoError(t, err)
		require.NoError(t, report.Err(),
			"an excluded table is never dumped or copied, so its grants stop mattering")

		assert.Equal(t, []string{"public.restricted"}, report.ExcludedTables())
		assert.Equal(t, []string{`"public"."restricted"`}, report.ExcludeTablePatterns())
	})
}

// Excluding a table does not excuse the objects that point at it: pg_dump still emits every one of
// them, and pg_restore is where a foreign key to a table that is not there finally fails. That is
// an export, an upload and a restore after the mistake was made, so preflight refuses it up front.
func TestPreflightRejectsExcludedTableWithDependents(t *testing.T) {
	pool, ctx := Connect(t)

	schema := "pgsnap_acc_excl_deps"
	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	}
	drop()
	t.Cleanup(drop)

	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE SCHEMA %s;
		CREATE TABLE %s.keep (id int PRIMARY KEY);
		CREATE TABLE %s.child (id int PRIMARY KEY, keep_id int REFERENCES %s.keep (id));
		CREATE VIEW %s.keep_summary AS SELECT count(*) AS total FROM %s.keep;`,
		schema, schema, schema, schema, schema, schema))
	require.NoError(t, err)

	preflight := func(mode scrub.TableMode) (*export.Report, error) {
		return export.Preflight{
			Introspector: export.Introspector{DB: pool},
			Config: scrub.Config{Version: 1, Tables: map[string]scrub.TableConfig{
				schema + ".keep": {Mode: mode},
			}},
			Salt: "acc-salt",
		}.Run(ctx)
	}

	t.Run("skip is refused while anything still depends on the table", func(t *testing.T) {
		report, err := preflight(scrub.TableModeSkip)
		require.Error(t, err, "the restore would fail on the dependents, an artifact too late")
		require.NotNil(t, report)

		assert.Contains(t, err.Error(), schema+".keep")
		assert.Contains(t, err.Error(), schema+".child", "the foreign key's table")
		assert.Contains(t, err.Error(), schema+".keep_summary", "the view over it")
		assert.Contains(t, err.Error(), "skip-data", "the mode that keeps the dependents valid")
	})

	// The same fixture, the other mode: the table's structure stays, so nothing is dangling
	t.Run("skip-data leaves the structure, so the dependents hold", func(t *testing.T) {
		report, err := preflight(scrub.TableModeSkipData)
		require.NoError(t, err)
		require.NoError(t, report.Err())
		assert.Empty(t, report.ExcludedTables())
	})
}
