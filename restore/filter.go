package restore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nullstone-io/pg-snapshot/pg"
)

// availableExtensionsSql lists what the target instance is able to create.
//
// pg_available_extensions reads the control files on the server's own filesystem, so it answers
// the question that matters -- what this instance can install -- rather than what is installed in
// some particular database.
const availableExtensionsSql = `SELECT name FROM pg_catalog.pg_available_extensions`

// replicationDescs are dropped from every restore regardless of the target.
//
// Replication topology belongs to the environment it runs in. A production publication feeding a
// data warehouse must not be recreated in a lower environment even where privileges would allow
// it, and they generally do not: ALTER PUBLICATION ... ADD TABLES IN SCHEMA requires superuser,
// which no managed Postgres grants.
//
// New snapshots do not carry these at all -- DumpSchema passes --no-publications and
// --no-subscriptions. The filter is what lets an artifact taken before that still restore.
var replicationDescs = map[string]bool{
	pg.DescPublication:        true,
	pg.DescPublicationTable:   true,
	pg.DescPublicationSchemas: true,
	pg.DescSubscription:       true,
}

// PlanSchemaFilter decides which archive entries the target cannot or should not replay, and writes
// a pg_restore --use-list file with those entries commented out. It returns an empty path when
// there is nothing to filter.
//
// Two categories are handled, and they are filtered for different reasons:
//
//   - Extensions the target cannot create. Managed Postgres installs its own -- google_vacuum_mgmt
//     on AlloyDB, the various aws_* on RDS -- and a dump carries a CREATE EXTENSION for each. This
//     is decided against the target, because one artifact is restored into several environments
//     and which extensions exist is a property of each of them.
//   - Publications and subscriptions, always. That one is a property of neither end; it is simply
//     not something a snapshot should carry.
func PlanSchemaFilter(ctx context.Context, db pg.Querier, archivePath string, log *slog.Logger) (string, error) {
	toc, err := pg.ListArchive(ctx, archivePath)
	if err != nil {
		return "", err
	}

	unsupported, err := unsupportedExtensions(ctx, db, pg.ArchiveExtensions(toc))
	if err != nil {
		return "", err
	}

	dropped := map[string][]string{}
	filtered := pg.CommentOut(toc, func(e pg.TocEntry) bool {
		switch {
		case e.Desc == pg.DescExtension && unsupported[e.Name]:
			dropped[e.Desc] = append(dropped[e.Desc], e.Name)
			return true
		case replicationDescs[e.Desc]:
			dropped[e.Desc] = append(dropped[e.Desc], e.Name)
			return true
		}
		return false
	})

	if len(dropped) < 1 {
		return "", nil
	}

	// Loud rather than silent. Skipping is right for a provider's own extension and for
	// replication, and wrong for an extension the schema actually uses -- only the operator can
	// tell which. If it is the latter, the objects depending on it fail on their own a moment
	// later, so nothing is lost quietly.
	for desc, names := range dropped {
		log.Warn("skipping archive entries", "type", desc, "names", names,
			"reason", reasonFor(desc))
	}

	listPath := archivePath + ".list"
	body := strings.Join(filtered, "\n")
	if err := os.WriteFile(listPath, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("error writing restore list: %w", err)
	}
	return listPath, nil
}

func reasonFor(desc string) string {
	if desc == pg.DescExtension {
		return "not present in pg_available_extensions on the target instance"
	}
	return "replication topology belongs to the environment, not the snapshot"
}

func unsupportedExtensions(ctx context.Context, db pg.Querier, wanted []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(wanted) < 1 {
		return out, nil
	}

	available, err := availableExtensions(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, name := range wanted {
		if !available[name] {
			out[name] = true
		}
	}
	return out, nil
}

func availableExtensions(ctx context.Context, db pg.Querier) (map[string]bool, error) {
	rows, err := db.Query(ctx, availableExtensionsSql)
	if err != nil {
		return nil, fmt.Errorf("error reading available extensions: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("error reading available extensions: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}
