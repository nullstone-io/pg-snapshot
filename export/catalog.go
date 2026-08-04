// Package export reads a source database and streams a scrubbed snapshot out of it.
package export

import (
	"context"
	"fmt"

	"github.com/nullstone-io/pg-snapshot/catalog"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// MinServerMajor is the oldest postgres this tool supports on either end.
//
// Requiring 16 removes every feature-detection branch the restore would otherwise need:
// pg_read_all_data (14+), DROP DATABASE WITH FORCE (13+), the hardened public schema (15+),
// and GRANT ... WITH INHERIT (16+) are all simply present.
const MinServerMajor = 16

// systemSchemas excludes the catalogs from every introspection query. It assumes the namespace
// is aliased `n`.
const systemSchemas = `n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'`

type Introspector struct {
	DB pg.Querier
}

// ServerMajor reports the major version of the connected server.
//
// server_version_num is used rather than parsing version(): it is a number the server computes
// itself, so there is no string format to get wrong.
//
// Read through current_setting() with an explicit cast rather than SHOW, because SHOW returns its
// result as text no matter what the setting holds.
func (i Introspector) ServerMajor(ctx context.Context) (int, error) {
	var num int
	if err := i.DB.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int`).Scan(&num); err != nil {
		return 0, fmt.Errorf("error reading server version: %w", err)
	}
	return num / 10000, nil
}

const tablesSql = `
SELECT n.nspname,
       c.relname,
       c.relkind::text,
       pg_catalog.pg_get_userbyid(c.relowner),
       c.relrowsecurity,
       c.relforcerowsecurity,
       COALESCE(pn.nspname || '.' || p.relname, '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_inherits i ON i.inhrelid = c.oid AND c.relispartition
LEFT JOIN pg_catalog.pg_class p ON p.oid = i.inhparent
LEFT JOIN pg_catalog.pg_namespace pn ON pn.oid = p.relnamespace
WHERE c.relkind IN ('r', 'p') AND ` + systemSchemas + `
ORDER BY n.nspname, c.relname`

// columnsSql reads every attribute of every table in one pass, rather than a query per table.
//
// MaxLen comes from atttypmod rather than information_schema: the length modifier is what decides
// whether a 32-character hash fits, and only the character types carry one. The -4 is the varlena
// header the modifier includes.
const columnsSql = `
SELECT n.nspname,
       c.relname,
       a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       pg_catalog.format_type(a.atttypid, NULL),
       CASE WHEN t.typname IN ('varchar', 'bpchar') AND a.atttypmod > 4
            THEN a.atttypmod - 4 ELSE -1 END,
       a.attgenerated <> '',
       a.attidentity <> '',
       a.attnotnull
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
WHERE a.attnum > 0 AND NOT a.attisdropped
  AND c.relkind IN ('r', 'p') AND ` + systemSchemas + `
ORDER BY n.nspname, c.relname, a.attnum`

// Tables reads every non-system table with its columns.
//
// Partitioned parents are returned alongside ordinary tables so callers can report on them; their
// rows live in leaf partitions. See Exportable.
func (i Introspector) Tables(ctx context.Context) ([]catalog.Table, error) {
	rows, err := i.DB.Query(ctx, tablesSql)
	if err != nil {
		return nil, fmt.Errorf("error reading tables: %w", err)
	}
	defer rows.Close()

	tables := make([]catalog.Table, 0)
	index := map[string]int{}
	for rows.Next() {
		var t catalog.Table
		var kind string
		if err := rows.Scan(&t.Schema, &t.Name, &kind, &t.Owner,
			&t.RowSecurity, &t.ForceRowSecurity, &t.Parent); err != nil {
			return nil, fmt.Errorf("error reading tables: %w", err)
		}
		t.Kind = catalog.RelKind(kind)
		index[t.Qualified()] = len(tables)
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error reading tables: %w", err)
	}

	if err := i.attachColumns(ctx, tables, index); err != nil {
		return nil, err
	}
	return tables, nil
}

func (i Introspector) attachColumns(ctx context.Context, tables []catalog.Table, index map[string]int) error {
	rows, err := i.DB.Query(ctx, columnsSql)
	if err != nil {
		return fmt.Errorf("error reading columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table string
		var col catalog.Column
		if err := rows.Scan(&schema, &table, &col.Name, &col.TypeName, &col.BaseType,
			&col.MaxLen, &col.Generated, &col.Identity, &col.NotNull); err != nil {
			return fmt.Errorf("error reading columns: %w", err)
		}
		// A table created between the two queries has no entry. It also has no snapshot, so
		// dropping its columns is the right outcome rather than an error.
		if at, ok := index[fmt.Sprintf("%s.%s", schema, table)]; ok {
			tables[at].Columns = append(tables[at].Columns, col)
		}
	}
	return rows.Err()
}

// Exportable narrows a table list to the relations whose rows are copied directly.
//
// Partitioned parents are dropped: their rows are already covered by their leaf partitions, and
// copying from the parent on load would push every row through tuple routing instead of landing
// it in a leaf.
func Exportable(tables []catalog.Table) []catalog.Table {
	out := make([]catalog.Table, 0, len(tables))
	for _, t := range tables {
		if t.Kind == catalog.RelKindTable {
			out = append(out, t)
		}
	}
	return out
}
