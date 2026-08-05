package restore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nullstone-io/pg-snapshot/pg"
)

// Publications carries an environment's publications across the swap.
//
// A publication lives in pg_publication, a per-database catalog, so the rename takes it with the
// database it belongs to and the replacement comes up with none. Without this, every restore
// silently breaks whatever was replicating out of the environment.
//
// What is carried is the *target's* own publications, never production's. The snapshot excludes
// publications entirely (see pg.DumpSchema), because production's replication topology has no
// business running in a lower environment.
type Publications struct {
	// Dir is where the intermediate archive and list file are written
	Dir string

	Log *slog.Logger
}

func (p Publications) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// Carry copies the publications defined in `fromURL` into `toURL`.
//
// Run this *after* the migration step. A publication applied before migrations is not merely
// premature, it is destructive: dropping a published table silently removes it from the publication
// with no error, and dropping a column named in a publication's column list fails outright, so the
// migration itself breaks.
//
// Run it *before* the swap. Publications are per-database, so the copy still sitting in the target
// is not a name conflict, and a failure here leaves the target exactly as it was.
//
// The replay deliberately does not pass --role. Creating a publication for all tables or for a
// schema is a superuser check, and the restore role holds instance-admin membership that the object
// owner does not; issuing SET ROLE would drop those privileges on the floor.
func (p Publications) Carry(ctx context.Context, fromURL, toURL string) error {
	archive := filepath.Join(p.Dir, "publications.dump")
	if err := pg.DumpPublications(ctx, fromURL, archive); err != nil {
		return fmt.Errorf("error reading publications from the target: %w", err)
	}
	defer os.Remove(archive)

	toc, err := pg.ListArchive(ctx, archive)
	if err != nil {
		return err
	}

	names := publicationNames(toc)
	if len(names) < 1 {
		return nil
	}

	listPath := archive + ".list"
	body := strings.Join(pg.KeepOnly(toc, pg.IsPublication), "\n")
	if err := os.WriteFile(listPath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("error writing publication list: %w", err)
	}
	defer os.Remove(listPath)

	p.log().Info("carrying publications", "publications", names)

	if err := pg.RestoreSection(ctx, toURL, archive, pg.RestoreOptions{
		Section:  pg.SectionPostData,
		ListPath: listPath,
	}); err != nil {
		return fmt.Errorf("error recreating publications %v: %w", names, err)
	}
	return nil
}

// publicationNames lists the publications a table of contents defines, in a stable order.
func publicationNames(toc []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, line := range toc {
		e, ok := pg.ParseTocEntry(line)
		if !ok || e.Desc != pg.DescPublication || seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// driftSql finds publications that name their tables explicitly and do not cover everything.
//
// FOR ALL TABLES and FOR TABLES IN SCHEMA resolve their membership at decode time, so a table
// created by a later migration is picked up on its own and neither form can drift. Only an
// enumerated FOR TABLE list is frozen at the moment it was written.
const driftSql = `
SELECT p.pubname,
       array_agg(format('%I.%I', t.schemaname, t.tablename) ORDER BY t.schemaname, t.tablename)
FROM pg_catalog.pg_publication p
CROSS JOIN LATERAL (
  SELECT n.nspname AS schemaname, c.relname AS tablename
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relkind IN ('r', 'p')
    AND NOT c.relispartition
    AND n.nspname NOT LIKE 'pg\_%'
    AND n.nspname <> 'information_schema'
    AND NOT EXISTS (
      SELECT 1 FROM pg_catalog.pg_publication_tables pt
      WHERE pt.pubname = p.pubname
        AND pt.schemaname = n.nspname
        AND pt.tablename = c.relname
    )
) t
WHERE NOT p.puballtables
  AND NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_publication_namespace pn WHERE pn.pnpubid = p.oid
  )
GROUP BY p.pubname`

// ReportDrift warns about publications that no longer cover every table.
//
// It reports rather than repairs. An enumerated FOR TABLE list is a deliberate statement about what
// to replicate, and quietly extending it would be its own kind of wrong -- but a table added by a
// migration silently falling outside replication is worth saying out loud.
func (p Publications) ReportDrift(ctx context.Context, db pg.Querier) error {
	rows, err := db.Query(ctx, driftSql)
	if err != nil {
		return fmt.Errorf("error checking publication coverage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var missing []string
		if err := rows.Scan(&name, &missing); err != nil {
			return fmt.Errorf("error checking publication coverage: %w", err)
		}
		p.log().Warn("publication does not cover every table",
			"publication", name,
			"missing", missing,
			"reason", "the publication names its tables explicitly, so tables added since it was "+
				"defined are not replicated")
	}
	return rows.Err()
}
