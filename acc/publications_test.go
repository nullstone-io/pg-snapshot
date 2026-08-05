package acc

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/restore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Production instances carry replication topology -- a publication feeding a data warehouse, say --
// and pg_dump emits it like any other object. Recreating it in a lower environment is wrong on its
// own terms, and it fails anyway: ALTER PUBLICATION ... ADD TABLES IN SCHEMA needs superuser, which
// no managed Postgres grants.
//
// New snapshots do not carry publications at all. This covers the artifact taken before that.
func TestRestoreFiltersPublicationsFromOlderArtifact(t *testing.T) {
	pool, ctx := Connect(t)

	source := "pgsnap_acc_pub_src"
	target := "pgsnap_acc_pub_dst"

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

	require.NoError(t, execAll(ctx, sourcePool,
		`CREATE TABLE public.widgets (id int PRIMARY KEY, name text NOT NULL)`,
		`CREATE PUBLICATION pgsnap_acc_pub FOR TABLES IN SCHEMA public`,
	))

	// Dumped without --no-publications on purpose: this is the shape of an artifact taken before
	// DumpSchema started excluding them, which is what the restore-side filter exists for.
	archive := filepath.Join(t.TempDir(), "schema.dump")
	require.NoError(t, legacyDumpSchema(ctx, sourceURL, archive))

	toc, err := pg.ListArchive(ctx, archive)
	require.NoError(t, err)
	require.True(t, hasPublicationEntry(toc),
		"the archive must carry a publication, or the test proves nothing")

	targetPool, err := pg.Open(ctx, targetURL, 1)
	require.NoError(t, err)
	defer targetPool.Close()

	listPath, err := restore.PlanSchemaFilter(ctx, targetPool, archive, discardLogger())
	require.NoError(t, err)
	require.NotEmpty(t, listPath, "a publication in the archive must produce a filter list")

	require.NoError(t, pg.RestoreSection(ctx, targetURL, archive, pg.RestoreOptions{
		Section:  pg.SectionPreData,
		ListPath: listPath,
	}), "pre-data must succeed with the publication filtered out")

	var publications int
	require.NoError(t, targetPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_publication WHERE pubname = 'pgsnap_acc_pub'`).Scan(&publications))
	assert.Equal(t, 0, publications, "the publication must not have been recreated")

	var tables int
	require.NoError(t, targetPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'widgets'`).
		Scan(&tables))
	assert.Equal(t, 1, tables, "the schema itself must still be restored")
}

// New snapshots never carry publications, so the filter has nothing to do for them.
func TestDumpSchemaOmitsPublications(t *testing.T) {
	pool, ctx := Connect(t)

	source := "pgsnap_acc_pub_omit"
	drop := func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, source))
	}
	drop()
	t.Cleanup(drop)

	require.NoError(t, execAll(ctx, pool, fmt.Sprintf(`CREATE DATABASE %s`, source)))

	sourceURL, err := withDatabase(URL(), source)
	require.NoError(t, err)

	sourcePool, err := pg.Open(ctx, sourceURL, 1)
	require.NoError(t, err)
	defer sourcePool.Close()

	require.NoError(t, execAll(ctx, sourcePool,
		`CREATE TABLE public.widgets (id int PRIMARY KEY)`,
		`CREATE PUBLICATION pgsnap_acc_pub_omit FOR TABLES IN SCHEMA public`,
	))

	archive := filepath.Join(t.TempDir(), "schema.dump")
	require.NoError(t, pg.DumpSchema(ctx, sourceURL, archive, ""))

	toc, err := pg.ListArchive(ctx, archive)
	require.NoError(t, err)
	assert.False(t, hasPublicationEntry(toc), "DumpSchema must not emit publications")
}

func hasPublicationEntry(toc []string) bool {
	for _, line := range toc {
		e, ok := pg.ParseTocEntry(line)
		if !ok {
			continue
		}
		switch e.Desc {
		case pg.DescPublication, pg.DescPublicationTable, pg.DescPublicationSchemas:
			return true
		}
	}
	return false
}

// legacyDumpSchema reproduces DumpSchema as it was before it excluded replication objects.
func legacyDumpSchema(ctx context.Context, url, outPath string) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--format=custom", "--schema-only", "--no-owner", "--no-acl", "--no-comments",
		"--file="+outPath, url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_dump: %w\n%s", err, out)
	}
	return nil
}
