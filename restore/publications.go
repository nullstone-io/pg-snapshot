package restore

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// Publication is a postgres publication -- the set of tables a logical replication consumer reads.
//
// A publication selects its tables one of three ways:
//   - AllTables: FOR ALL TABLES, every table in every schema, including ones created later
//   - Schemas:   FOR TABLES IN SCHEMA <schema>, likewise for a schema (postgres 15+)
//   - Tables:    FOR TABLE <schema>.<table>, only what is listed
//
// Schemas and Tables combine; AllTables combines with neither.
//
// The shape and the SQL are ported from pg-db-admin's postgresql/publication.go, which is what
// created these publications in the first place.
type Publication struct {
	Name      string
	AllTables bool
	Schemas   []string
	Tables    []PublicationTable

	// Publish is the publish parameter, e.g. "insert, update, delete, truncate"
	Publish string

	// ViaRoot is publish_via_partition_root
	ViaRoot bool
}

// PublicationTable is one table added to a publication explicitly.
//
// Columns and RowFilter are a deliberate divergence from pg-db-admin, which models neither because
// Terraform never creates them. Carrying them matters anyway: dropping a row filter silently
// widens what gets replicated, which is the wrong direction to be wrong in.
type PublicationTable struct {
	Schema string
	Name   string

	// Columns is the published column list. Empty means every column, including ones added later.
	Columns []string

	// RowFilter is the WHERE expression restricting published rows. Empty means every row.
	RowFilter string
}

// definition renders the table as it appears after FOR TABLE.
func (t PublicationTable) definition() string {
	out := t.quoted()
	if len(t.Columns) > 0 {
		quoted := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			quoted = append(quoted, pq.QuoteIdentifier(c))
		}
		out += " (" + strings.Join(quoted, ", ") + ")"
	}
	if t.RowFilter != "" {
		out += " WHERE (" + t.RowFilter + ")"
	}
	return out
}

// DescribePublication summarises which of the three selection forms a publication uses, and
// therefore whether recreating it needs a real superuser.
func DescribePublication(p Publication) string {
	switch {
	case p.AllTables:
		return "FOR ALL TABLES -- schema-independent, requires a real superuser to create"
	case len(p.Schemas) > 0 && len(p.Tables) > 0:
		return fmt.Sprintf("schemas %v plus %d explicit table(s) -- requires a real superuser to create",
			p.Schemas, len(p.Tables))
	case len(p.Schemas) > 0:
		return fmt.Sprintf("FOR TABLES IN SCHEMA %v -- schema-independent, requires a real superuser to create",
			p.Schemas)
	case len(p.Tables) > 0:
		return fmt.Sprintf("%d explicit table(s) -- needs ownership of them, not superuser",
			len(p.Tables))
	default:
		return "empty -- publishes nothing"
	}
}

func (t PublicationTable) quoted() string {
	if t.Schema == "" {
		return pq.QuoteIdentifier(t.Name)
	}
	return fmt.Sprintf("%s.%s", pq.QuoteIdentifier(t.Schema), pq.QuoteIdentifier(t.Name))
}

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
	Log *slog.Logger
}

func (p Publications) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// Carry copies the publications defined in `from` onto `to`.
//
// Run this *after* the migration step. A publication applied before migrations is not merely
// premature, it is destructive: dropping a published table silently removes it from the
// publication, and dropping a column named in a publication's column list fails outright, so the
// migration itself breaks.
//
// Run it *before* the swap. Publications are per-database, so the copy still sitting in the target
// is not a name conflict, and a failure here leaves the target exactly as it was.
func (p Publications) Carry(ctx context.Context, from, to *pgxpool.Pool) error {
	publications, err := ReadPublications(ctx, from)
	if err != nil {
		return err
	}
	if len(publications) < 1 {
		return nil
	}

	names := make([]string, 0, len(publications))
	for _, pub := range publications {
		names = append(names, pub.Name)
	}
	p.log().Info("carrying publications", "publications", names)

	for _, pub := range publications {
		if err := p.create(ctx, to, pub); err != nil {
			return err
		}
	}
	return nil
}

// create recreates one publication, borrowing whatever privileges the statement needs.
//
// CREATE PUBLICATION checks the CREATE privilege on the database, which CREATE DATABASE grants to
// its owner and nobody else, and naming a table explicitly requires owning that table. A superuser
// needs neither, which is why the borrowing is skipped for one.
//
// FOR ALL TABLES and FOR TABLES IN SCHEMA are a different matter: they require a *real* superuser,
// and no membership substitutes for it. Cloud SQL confers that on members of cloudsqlsuperuser;
// where it is not conferred, this fails -- before the swap, with the target untouched.
func (p Publications) create(ctx context.Context, to *pgxpool.Pool, pub Publication) error {
	info, err := readConnInfo(ctx, to)
	if err != nil {
		return err
	}

	if !info.superuser {
		owners, err := publicationOwners(ctx, to, pub)
		if err != nil {
			return err
		}
		held, err := borrowRoles(ctx, to, info.user, owners, p.log())
		defer held.release(ctx)
		if err != nil {
			return fmt.Errorf("%s could not acquire the privileges needed to create publication %q: %w",
				info.user, pub.Name, err)
		}
	}

	sq := CreatePublicationSql(pub)
	if _, err := to.Exec(ctx, sq); err != nil {
		return fmt.Errorf("error creating publication %q: %w", pub.Name, err)
	}
	return nil
}

// publicationOwners lists the roles whose privileges creating this publication requires.
func publicationOwners(ctx context.Context, db pg.Querier, pub Publication) ([]string, error) {
	owners := make([]string, 0)

	dbOwner, err := readDatabaseOwner(ctx, db)
	if err != nil {
		return nil, err
	}
	owners = append(owners, dbOwner)

	schemas := map[string]bool{}
	for _, t := range pub.Tables {
		schemas[t.Schema] = true
	}
	for _, schema := range sortedKeys(schemas) {
		rows, err := db.Query(ctx, `
			SELECT DISTINCT pg_get_userbyid(c.relowner)
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')`, schema)
		if err != nil {
			return nil, fmt.Errorf("error reading relation owners in %q: %w", schema, err)
		}
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				rows.Close()
				return nil, fmt.Errorf("error reading relation owners in %q: %w", schema, err)
			}
			owners = append(owners, owner)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("error reading relation owners in %q: %w", schema, err)
		}
	}
	return owners, nil
}

// CreatePublicationSql renders the CREATE PUBLICATION statement, exactly as pg-db-admin renders it.
//
// Tables are emitted before schemas to match the syntax postgres documents.
func CreatePublicationSql(p Publication) string {
	b := bytes.NewBufferString("CREATE PUBLICATION ")
	fmt.Fprint(b, pq.QuoteIdentifier(p.Name))

	switch {
	case p.AllTables:
		fmt.Fprint(b, " FOR ALL TABLES")
	case len(p.Tables) > 0 || len(p.Schemas) > 0:
		objects := make([]string, 0, 2)
		if len(p.Tables) > 0 {
			quoted := make([]string, 0, len(p.Tables))
			for _, t := range p.Tables {
				quoted = append(quoted, t.definition())
			}
			objects = append(objects, "TABLE "+strings.Join(quoted, ", "))
		}
		if len(p.Schemas) > 0 {
			quoted := make([]string, 0, len(p.Schemas))
			for _, s := range p.Schemas {
				quoted = append(quoted, pq.QuoteIdentifier(s))
			}
			objects = append(objects, "TABLES IN SCHEMA "+strings.Join(quoted, ", "))
		}
		fmt.Fprintf(b, " FOR %s", strings.Join(objects, ", "))
	}

	// Always emitted rather than only when non-default: pg-db-admin never sets these, so a
	// publication that has them was configured by hand and losing them silently would be worse
	// than a slightly longer statement.
	opts := []string{fmt.Sprintf("publish = %s", pq.QuoteLiteral(p.Publish))}
	if p.ViaRoot {
		opts = append(opts, "publish_via_partition_root = true")
	}
	fmt.Fprintf(b, " WITH (%s)", strings.Join(opts, ", "))

	return b.String()
}

const publicationsSql = `
SELECT pubname, puballtables, pubinsert, pubupdate, pubdelete, pubtruncate, pubviaroot
FROM pg_catalog.pg_publication
ORDER BY pubname`

// ReadPublications reads every publication defined in a database.
func ReadPublications(ctx context.Context, db pg.Querier) ([]Publication, error) {
	rows, err := db.Query(ctx, publicationsSql)
	if err != nil {
		return nil, fmt.Errorf("error reading publications: %w", err)
	}

	out := make([]Publication, 0)
	for rows.Next() {
		var p Publication
		var insert, update, del, truncate bool
		if err := rows.Scan(&p.Name, &p.AllTables, &insert, &update, &del, &truncate, &p.ViaRoot); err != nil {
			rows.Close()
			return nil, fmt.Errorf("error reading publications: %w", err)
		}
		p.Publish = publishList(insert, update, del, truncate)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error reading publications: %w", err)
	}

	for i := range out {
		if out[i].Schemas, err = readPublicationSchemas(ctx, db, out[i].Name); err != nil {
			return nil, err
		}
		if out[i].Tables, err = readPublicationTables(ctx, db, out[i].Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readPublicationSchemas(ctx context.Context, db pg.Querier, name string) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT n.nspname
		FROM pg_catalog.pg_publication_namespace pn
		JOIN pg_catalog.pg_namespace n ON n.oid = pn.pnnspid
		JOIN pg_catalog.pg_publication p ON p.oid = pn.pnpubid
		WHERE p.pubname = $1
		ORDER BY n.nspname`, name)
	if err != nil {
		return nil, fmt.Errorf("error reading schemas of publication %q: %w", name, err)
	}
	defer rows.Close()

	schemas := make([]string, 0)
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return nil, fmt.Errorf("error reading schemas of publication %q: %w", name, err)
		}
		schemas = append(schemas, schema)
	}
	return schemas, rows.Err()
}

// readPublicationTables reads only the tables added to the publication explicitly.
//
// pg_publication_rel rather than the pg_publication_tables view, and the difference matters: the
// view expands FOR ALL TABLES and FOR TABLES IN SCHEMA into their current members, so a
// schema-based publication would read back as a frozen list of whatever happened to exist.
func readPublicationTables(ctx context.Context, db pg.Querier, name string) ([]PublicationTable, error) {
	// prattrs is read rather than pg_publication_tables.attnames because the view reports every
	// column when no column list was given -- indistinguishable from an explicit list of all of
	// them, which would freeze out columns added later.
	rows, err := db.Query(ctx, `
		SELECT n.nspname, c.relname,
		       COALESCE((
		         SELECT array_agg(a.attname ORDER BY u.ord)
		         FROM unnest(pr.prattrs::int2[]) WITH ORDINALITY AS u(attnum, ord)
		         JOIN pg_catalog.pg_attribute a
		           ON a.attrelid = pr.prrelid AND a.attnum = u.attnum
		       ), '{}') AS columns,
		       COALESCE(pg_catalog.pg_get_expr(pr.prqual, pr.prrelid), '') AS rowfilter
		FROM pg_catalog.pg_publication_rel pr
		JOIN pg_catalog.pg_class c ON c.oid = pr.prrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_publication p ON p.oid = pr.prpubid
		WHERE p.pubname = $1
		ORDER BY n.nspname, c.relname`, name)
	if err != nil {
		return nil, fmt.Errorf("error reading tables of publication %q: %w", name, err)
	}
	defer rows.Close()

	tables := make([]PublicationTable, 0)
	for rows.Next() {
		var t PublicationTable
		if err := rows.Scan(&t.Schema, &t.Name, &t.Columns, &t.RowFilter); err != nil {
			return nil, fmt.Errorf("error reading tables of publication %q: %w", name, err)
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

func publishList(insert, update, del, truncate bool) string {
	parts := make([]string, 0, 4)
	for _, p := range []struct {
		on   bool
		name string
	}{{insert, "insert"}, {update, "update"}, {del, "delete"}, {truncate, "truncate"}} {
		if p.on {
			parts = append(parts, p.name)
		}
	}
	return strings.Join(parts, ", ")
}

type connInfo struct {
	user      string
	superuser bool
}

func readConnInfo(ctx context.Context, db pg.Querier) (connInfo, error) {
	var info connInfo
	if err := db.QueryRow(ctx,
		`SELECT CURRENT_USER, rolsuper FROM pg_catalog.pg_roles WHERE rolname = CURRENT_USER`,
	).Scan(&info.user, &info.superuser); err != nil {
		return info, fmt.Errorf("error reading connection identity: %w", err)
	}
	return info, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
