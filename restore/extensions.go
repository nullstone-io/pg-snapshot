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

// PlanExtensions decides which of an archive's extensions the target cannot create, and writes a
// pg_restore --use-list file with those entries commented out. It returns an empty path when every
// extension is available and no filtering is needed.
//
// Managed Postgres installs its own extensions -- google_vacuum_mgmt on AlloyDB, and the various
// aws_* on RDS -- and a dump carries a CREATE EXTENSION for each. Restoring a production snapshot
// into a different engine, or a different edition of the same one, then fails on an extension that
// belongs to the instance rather than to the application's schema.
//
// The decision is made here, against the target, rather than at snapshot time: one artifact can be
// restored into several environments, and which extensions exist is a property of each of them.
func PlanExtensions(ctx context.Context, db pg.Querier, archivePath string, log *slog.Logger) (string, error) {
	toc, err := pg.ListArchive(ctx, archivePath)
	if err != nil {
		return "", err
	}

	wanted := pg.ArchiveExtensions(toc)
	if len(wanted) < 1 {
		return "", nil
	}

	available, err := availableExtensions(ctx, db)
	if err != nil {
		return "", err
	}

	skip := map[string]bool{}
	missing := make([]string, 0)
	for _, name := range wanted {
		if !available[name] && !skip[name] {
			skip[name] = true
			missing = append(missing, name)
		}
	}
	if len(missing) < 1 {
		return "", nil
	}

	// Loud rather than silent: skipping is right for a provider's own extension and wrong for one
	// the schema actually uses, and only the operator can tell which this is. If it is the latter,
	// the objects depending on it fail on their own a moment later.
	log.Warn("skipping extensions the target cannot create",
		"extensions", missing,
		"reason", "not present in pg_available_extensions on the target instance")

	listPath := archivePath + ".list"
	body := strings.Join(pg.CommentOutExtensions(toc, skip), "\n")
	if err := os.WriteFile(listPath, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("error writing restore list: %w", err)
	}
	return listPath, nil
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
