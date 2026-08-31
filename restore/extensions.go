package restore

import (
	"context"
	"fmt"

	"github.com/nullstone-io/pg-snapshot/pg"
)

// extensionTablesSql lists the tables that belong to an extension in the connected database --
// the 'e' dependency CREATE EXTENSION records for each member relation.
const extensionTablesSql = `
SELECT n.nspname || '.' || c.relname
FROM pg_catalog.pg_depend d
JOIN pg_catalog.pg_class c ON c.oid = d.objid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE d.classid = 'pg_class'::regclass
  AND d.refclassid = 'pg_extension'::regclass
  AND d.deptype = 'e'
  AND c.relkind IN ('r', 'p')`

// ExtensionTables reports the tables owned by extensions in the staging database, by qualified
// name.
//
// New snapshots never carry these -- the exporter excludes extension members -- so this is what
// lets an artifact taken before that still restore: CREATE EXTENSION already populated the table,
// and loading the snapshot's copy on top is a duplicate key.
func ExtensionTables(ctx context.Context, db pg.Querier) (map[string]bool, error) {
	rows, err := db.Query(ctx, extensionTablesSql)
	if err != nil {
		return nil, fmt.Errorf("error reading extension tables: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("error reading extension tables: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}
