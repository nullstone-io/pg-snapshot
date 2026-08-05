package acc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A production snapshot carries CREATE EXTENSION for whatever the managed instance installed for
// itself -- google_vacuum_mgmt on AlloyDB, aws_* on RDS -- and restoring it into a different engine
// fails on an extension that was never part of the application's schema.
//
// The fix filters those entries out of the archive's table of contents. What has to hold is that
// --use-list and --section compose: the commented entry is skipped and every other entry in the
// section still runs. That is a claim about pg_restore, so it needs a real pg_restore.
func TestUseListSkipsExtensionsWithoutDroppingTheSection(t *testing.T) {
	pool, ctx := Connect(t)

	source := "pgsnap_acc_ext_src"
	target := "pgsnap_acc_ext_dst"

	drop := func() {
		for _, db := range []string{source, target} {
			pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, db))
		}
	}
	drop()
	t.Cleanup(drop)

	require.NoError(t, execAll(ctx, pool,
		fmt.Sprintf(`CREATE DATABASE %s`, source),
		fmt.Sprintf(`CREATE DATABASE %s`, target),
	))

	sourceURL, err := withDatabase(URL(), source)
	require.NoError(t, err)
	targetURL, err := withDatabase(URL(), target)
	require.NoError(t, err)

	sourcePool, err := pg.Open(ctx, sourceURL, 1)
	require.NoError(t, err)
	defer sourcePool.Close()

	// pgcrypto stands in for the provider extension: available here, so the test can dump it,
	// then filtered as though the target could not create it
	require.NoError(t, execAll(ctx, sourcePool,
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE public.widgets (id int PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE public.gadgets (id int PRIMARY KEY)`,
	))

	archive := filepath.Join(t.TempDir(), "schema.dump")
	require.NoError(t, pg.DumpSchema(ctx, sourceURL, archive, ""))

	toc, err := pg.ListArchive(ctx, archive)
	require.NoError(t, err)
	require.Contains(t, pg.ArchiveExtensions(toc), "pgcrypto",
		"the dump must carry the extension, or the test proves nothing")

	listPath := archive + ".list"
	filtered := pg.CommentOut(toc, func(e pg.TocEntry) bool {
		return e.Desc == pg.DescExtension && e.Name == "pgcrypto"
	})
	require.NoError(t, os.WriteFile(listPath, []byte(strings.Join(filtered, "\n")), 0o600))

	require.NoError(t, pg.RestoreSection(ctx, targetURL, archive, pg.RestoreOptions{
		Section:  pg.SectionPreData,
		ListPath: listPath,
	}), "pre-data must succeed with the extension entry commented out")

	targetPool, err := pg.Open(ctx, targetURL, 1)
	require.NoError(t, err)
	defer targetPool.Close()

	var extensions int
	require.NoError(t, targetPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pgcrypto'`).Scan(&extensions))
	assert.Equal(t, 0, extensions, "the filtered extension must not have been created")

	// The section is not merely skipped wholesale -- everything else in it still ran
	var tables int
	require.NoError(t, targetPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'
		   AND tablename IN ('widgets', 'gadgets')`).Scan(&tables))
	assert.Equal(t, 2, tables, "every other pre-data entry must still be restored")

	// And a NOT NULL from the pre-data section survives, so it is the real definition rather
	// than an empty table that happens to exist
	var notNull bool
	require.NoError(t, targetPool.QueryRow(ctx,
		`SELECT attnotnull FROM pg_attribute
		 WHERE attrelid = 'public.widgets'::regclass AND attname = 'name'`).Scan(&notNull))
	assert.True(t, notNull)
}

func execAll(ctx context.Context, pool *pgxpool.Pool, stmts ...string) error {
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}
