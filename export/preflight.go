package export

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-multierror/multierror"
	"github.com/nullstone-io/pg-snapshot/catalog"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/scrub"
)

// Preflight decides whether a snapshot can run, before any data moves.
//
// Everything it checks is cheap and catalog-only. A snapshot that fails an hour in, after
// streaming most of a database, is a waste; one that fails before it starts is a bug report with
// a fix attached.
type Preflight struct {
	Introspector
	Config scrub.Config
	Salt   string
}

type FindingKind string

const (
	// FindingNoSelect means the role cannot read some of the table's columns at all
	FindingNoSelect FindingKind = "no_select"

	// FindingRowSecurity means row-level security would silently filter the export.
	//
	// Resolvable: the table's owner bypasses its own policies, so membership in the owner role
	// makes the table readable in full.
	FindingRowSecurity FindingKind = "row_security"

	// FindingForcedRowSecurity means the table sets FORCE ROW LEVEL SECURITY, which subjects
	// even its owner to its policies.
	//
	// Not resolvable by any grant this tool can obtain: only BYPASSRLS lifts it, and neither
	// rds_superuser nor cloudsqlsuperuser is a true superuser. The table has to be excluded.
	FindingForcedRowSecurity FindingKind = "forced_row_security"
)

type Finding struct {
	Table   string
	Owner   string
	Kind    FindingKind
	Columns []string
}

type Report struct {
	// Plan is the export plan, one entry per exportable table, in a stable order
	Plan []scrub.Projection

	// Findings are the reasons the snapshot cannot proceed. Empty means it can.
	Findings []Finding

	// CurrentUser is the role the snapshot would run as, quoted into remediation SQL
	CurrentUser string

	// Database is the database actually connected to, as the server reports it
	Database string

	// ServerVersionNum is the source server's server_version_num
	ServerVersionNum int
}

// ServerMajor is the source server's major version.
func (r Report) ServerMajor() int {
	return r.ServerVersionNum / 10000
}

// ExcludedTables lists the tables mode: skip leaves out of the snapshot entirely, in plan order.
func (r Report) ExcludedTables() []string {
	out := make([]string, 0)
	for _, p := range r.Plan {
		if p.Excluded {
			out = append(out, p.Table.Qualified())
		}
	}
	return out
}

// ExcludeTablePatterns renders the excluded tables as the pg_dump patterns the schema dump takes.
func (r Report) ExcludeTablePatterns() []string {
	out := make([]string, 0)
	for _, p := range r.Plan {
		if p.Excluded {
			out = append(out, pg.ExcludeTablePattern(p.Table.Schema, p.Table.Name))
		}
	}
	return out
}

// Schemas lists the distinct schemas the plan covers, in order.
func (r Report) Schemas() []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, p := range r.Plan {
		if !seen[p.Table.Schema] {
			seen[p.Table.Schema] = true
			out = append(out, p.Table.Schema)
		}
	}
	sort.Strings(out)
	return out
}

// Run returns a report whenever one can be built, including alongside an error. Which database
// the connection actually landed in is the first thing worth knowing about a failed snapshot, so
// identity is read before anything that can reject the server and the report carries it out even
// when the run is refused.
func (p Preflight) Run(ctx context.Context) (*Report, error) {
	report := &Report{}
	if err := p.DB.QueryRow(ctx,
		`SELECT CURRENT_USER, current_database(), current_setting('server_version_num')::int`,
	).Scan(&report.CurrentUser, &report.Database, &report.ServerVersionNum); err != nil {
		return nil, fmt.Errorf("error reading connection identity: %w", err)
	}

	if major := report.ServerMajor(); major < MinServerMajor {
		return report, fmt.Errorf("postgres %d is not supported; pg-snapshot requires %d or newer",
			major, MinServerMajor)
	}

	tables, err := p.Tables(ctx)
	if err != nil {
		return report, err
	}

	unreadable, err := p.unreadableColumns(ctx)
	if err != nil {
		return nil, err
	}
	exempt, err := p.rlsExemptions(ctx)
	if err != nil {
		return nil, err
	}

	// Projection errors are configuration problems -- a rule naming a column that production
	// dropped. They are collected separately from findings because they are the user's config
	// being wrong, not the database withholding access.
	planErrs := make([]error, 0)

	for _, t := range Exportable(tables) {
		projection, err := scrub.BuildProjection(t, p.Config, p.Salt)
		if err != nil {
			planErrs = append(planErrs, err)
			continue
		}
		report.Plan = append(report.Plan, *projection)

		// A table left out of the snapshot entirely is neither dumped nor copied, so no access to
		// it is needed. mode: skip-data is not that: its structure still comes from pg_dump, which
		// locks every table it dumps, and that lock needs SELECT. Reading the two modes as one is
		// what let a table the role could not read reach pg_dump behind a clean preflight.
		if projection.Excluded {
			continue
		}
		if cols := unreadable[t.Qualified()]; len(cols) > 0 {
			report.Findings = append(report.Findings, Finding{
				Table: t.Qualified(), Owner: t.Owner, Kind: FindingNoSelect, Columns: cols,
			})
			continue
		}
		// Row security filters rows, and a skip-data table exports none, so there is nothing left
		// for a policy to filter
		if projection.Skipped {
			continue
		}
		if t.RowSecurity && !exempt[t.Qualified()] {
			kind := FindingRowSecurity
			if t.ForceRowSecurity {
				kind = FindingForcedRowSecurity
			}
			report.Findings = append(report.Findings, Finding{
				Table: t.Qualified(), Owner: t.Owner, Kind: kind,
			})
		}
	}

	// A table mode: skip removes from the schema dump takes nothing else with it -- pg_dump still
	// emits every view, function and foreign key that referenced it, and pg_restore is where they
	// fail on a relation that is not there. That is an hour of export and an artifact in the
	// bucket away from here, so the dependents are read now, while the fix is still a config edit.
	if excluded := report.ExcludedTables(); len(excluded) > 0 {
		dependents, err := p.excludedDependents(ctx, excluded)
		if err != nil {
			return report, err
		}
		for _, name := range excluded {
			if deps := dependents[name]; len(deps) > 0 {
				planErrs = append(planErrs, fmt.Errorf("table %q is left out of the snapshot by "+
					"mode: skip, but %s still depends on it and the restore would fail creating it; "+
					"exclude the dependent as well, or use mode: skip-data to keep the table's "+
					"structure (which needs SELECT on the table)", name, strings.Join(deps, ", ")))
			}
		}
	}

	// The report column of every tail-sampled table is type-checked here, over no rows, rather
	// than discovered to be un-aggregatable mid-run after other tables have already streamed
	for _, prj := range report.Plan {
		if prj.TailRows > 0 && prj.TailReportColumn != "" {
			if err := checkTailReportColumn(ctx, p.DB, prj); err != nil {
				planErrs = append(planErrs, err)
			}
		}
	}

	// The planning loop is driven by what the database contains, so a rule naming a table that is
	// not there is never visited and never validated. That is the same class of problem as a rule
	// naming a dropped column, and it is the one that catches a connection pointed at the wrong
	// database -- every rule misses at once.
	known := make(map[string]catalog.Table, len(tables))
	for _, t := range tables {
		known[t.Qualified()] = t
	}
	for _, name := range p.Config.TableNames() {
		t, ok := known[name]
		switch {
		case !ok:
			planErrs = append(planErrs, fmt.Errorf("no table %q in database %q", name, report.Database))
		case t.Kind == catalog.RelKindPartitioned:
			planErrs = append(planErrs, fmt.Errorf("table %q is a partitioned parent, so its rules "+
				"would never apply; configure its leaf partitions instead", name))
		}
	}

	// An empty plan is never a legitimate snapshot, and left alone it is silent: the export
	// succeeds, writes a manifest with no tables, and a restore loads that over a target
	// environment. Failing here costs nothing and the alternative is losing a database.
	if len(report.Plan) < 1 && len(planErrs) < 1 {
		planErrs = append(planErrs, fmt.Errorf("database %q contains no tables to export; "+
			"check that POSTGRES_URL names the database you mean to snapshot", report.Database))
	}

	if len(planErrs) > 0 {
		return report, multierror.New(planErrs)
	}
	return report, nil
}

// Err renders the findings as the error the operator sees, or nil when the snapshot can run.
func (r Report) Err() error {
	if len(r.Findings) < 1 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "snapshot aborted: %d %s cannot be exported in full by role %q.\n",
		len(r.Findings), plural(len(r.Findings), "table", "tables"), r.CurrentUser)

	for _, f := range r.Findings {
		b.WriteString("\n")
		switch f.Kind {
		case FindingNoSelect:
			fmt.Fprintf(&b, "  %-24s owner=%s\n", f.Table, f.Owner)
			fmt.Fprintf(&b, "      → no SELECT on: %s\n", strings.Join(f.Columns, ", "))
			// Not skip-data: without SELECT the role cannot lock the table, and pg_dump locks
			// every table whose structure it dumps. Only full exclusion keeps it out of reach.
			fmt.Fprintf(&b, "        grant it, or leave the table out of the snapshot entirely:\n")
			fmt.Fprintf(&b, "          tables:\n            %s: { mode: skip }\n", f.Table)

		case FindingRowSecurity:
			fmt.Fprintf(&b, "  %-24s owner=%s   RLS=on   FORCE=no\n", f.Table, f.Owner)
			fmt.Fprintf(&b, "      → grant membership in the owner role:\n")
			fmt.Fprintf(&b, "          GRANT %q TO %q;\n", f.Owner, r.CurrentUser)
			fmt.Fprintf(&b, "        or add it to the module's table_owner_roles:\n")
			fmt.Fprintf(&b, "          table_owner_roles = [%q]\n", f.Owner)

		case FindingForcedRowSecurity:
			fmt.Fprintf(&b, "  %-24s owner=%s   RLS=on   FORCE=yes\n", f.Table, f.Owner)
			fmt.Fprintf(&b, "      → owner membership will NOT help; FORCE ROW LEVEL SECURITY is set.\n")
			fmt.Fprintf(&b, "        Grant BYPASSRLS (requires a true superuser — unavailable on\n")
			fmt.Fprintf(&b, "        both Cloud SQL and RDS), or export the table empty:\n")
			// What a policy withholds here is rows, and the role can still read the structure --
			// so skip-data is the narrower fix. mode: skip drops the table from the artifact too.
			fmt.Fprintf(&b, "          tables:\n            %s: { mode: skip-data }\n", f.Table)
		}
	}

	b.WriteString("\nNo data was exported.")
	return fmt.Errorf("%s", b.String())
}

// excludedDependentsSql finds the objects that would outlive a table mode: skip removes.
//
// Three kinds reach a restore and fail there: a foreign key declared on a table that is still
// exported, a view or materialized view built over the excluded table, and a function whose body
// was parsed at creation (a SQL-standard body, the only kind postgres records a dependency for).
// Each is matched through the catalog rather than by reading definitions, so a rename or a
// schema-qualified reference cannot hide one.
//
// Dependents that are themselves excluded are not reported: they leave with the table.
const excludedDependentsSql = `
WITH excluded AS (
    SELECT c.oid, n.nspname || '.' || c.relname AS qualified
    FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname || '.' || c.relname = ANY($1)
)
SELECT DISTINCT * FROM (
    SELECT e.qualified,
           'foreign key ' || con.conname || ' on ' || cn.nspname || '.' || cl.relname AS dependent
    FROM pg_catalog.pg_constraint con
    JOIN excluded e ON e.oid = con.confrelid
    JOIN pg_catalog.pg_class cl ON cl.oid = con.conrelid
    JOIN pg_catalog.pg_namespace cn ON cn.oid = cl.relnamespace
    WHERE con.contype = 'f'
      AND con.conrelid NOT IN (SELECT oid FROM excluded)

    UNION ALL

    SELECT e.qualified,
           CASE v.relkind WHEN 'm' THEN 'materialized view ' ELSE 'view ' END
               || vn.nspname || '.' || v.relname
    FROM pg_catalog.pg_depend d
    JOIN excluded e ON e.oid = d.refobjid
    JOIN pg_catalog.pg_rewrite rw ON rw.oid = d.objid
    JOIN pg_catalog.pg_class v ON v.oid = rw.ev_class
    JOIN pg_catalog.pg_namespace vn ON vn.oid = v.relnamespace
    WHERE d.classid = 'pg_rewrite'::regclass
      AND d.refclassid = 'pg_class'::regclass
      AND v.oid NOT IN (SELECT oid FROM excluded)

    UNION ALL

    SELECT e.qualified,
           'function ' || pn.nspname || '.' || p.proname
    FROM pg_catalog.pg_depend d
    JOIN excluded e ON e.oid = d.refobjid
    JOIN pg_catalog.pg_proc p ON p.oid = d.objid
    JOIN pg_catalog.pg_namespace pn ON pn.oid = p.pronamespace
    WHERE d.classid = 'pg_proc'::regclass
      AND d.refclassid = 'pg_class'::regclass
) deps
ORDER BY 1, 2`

// excludedDependents maps each excluded table to the retained objects that still depend on it.
func (p Preflight) excludedDependents(ctx context.Context, excluded []string) (map[string][]string, error) {
	rows, err := p.DB.Query(ctx, excludedDependentsSql, excluded)
	if err != nil {
		return nil, fmt.Errorf("error checking dependencies on excluded tables: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var table, dependent string
		if err := rows.Scan(&table, &dependent); err != nil {
			return nil, fmt.Errorf("error checking dependencies on excluded tables: %w", err)
		}
		out[table] = append(out[table], dependent)
	}
	return out, rows.Err()
}

// unreadableColumnsSql finds columns the role cannot SELECT.
//
// Generated columns are ignored because they are never exported -- postgres recomputes them on
// load -- so lacking access to one does not block anything.
const unreadableColumnsSql = `
SELECT n.nspname, c.relname, a.attname
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
  AND c.relkind = 'r' AND ` + systemSchemas + `
  AND NOT pg_catalog.has_column_privilege(c.oid, a.attnum, 'SELECT')
ORDER BY n.nspname, c.relname, a.attnum`

func (p Preflight) unreadableColumns(ctx context.Context) (map[string][]string, error) {
	rows, err := p.DB.Query(ctx, unreadableColumnsSql)
	if err != nil {
		return nil, fmt.Errorf("error checking column privileges: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var schema, table, column string
		if err := rows.Scan(&schema, &table, &column); err != nil {
			return nil, fmt.Errorf("error checking column privileges: %w", err)
		}
		key := fmt.Sprintf("%s.%s", schema, table)
		out[key] = append(out[key], column)
	}
	return out, rows.Err()
}

// rlsExemptionsSql reports, per table, whether the current role escapes its row security.
//
// Two ways out: owning the table (or holding the owner's privileges through membership), or the
// BYPASSRLS attribute. FORCE ROW LEVEL SECURITY closes the first, which is why the table's flag
// is read separately rather than folded in here.
const rlsExemptionsSql = `
SELECT n.nspname,
       c.relname,
       pg_catalog.pg_has_role(c.relowner, 'USAGE') OR r.rolbypassrls
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN pg_catalog.pg_roles r
WHERE r.rolname = CURRENT_USER
  AND c.relkind = 'r' AND c.relrowsecurity AND ` + systemSchemas

func (p Preflight) rlsExemptions(ctx context.Context) (map[string]bool, error) {
	rows, err := p.DB.Query(ctx, rlsExemptionsSql)
	if err != nil {
		return nil, fmt.Errorf("error checking row security: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var schema, table string
		var exempt bool
		if err := rows.Scan(&schema, &table, &exempt); err != nil {
			return nil, fmt.Errorf("error checking row security: %w", err)
		}
		out[fmt.Sprintf("%s.%s", schema, table)] = exempt
	}
	return out, rows.Err()
}

// OwnerRoles lists the distinct owners of tables blocked only by resolvable row security.
//
// This is what auto_grant_table_owner grants membership in. It is worth seeing the list before
// enabling it: one owner is unremarkable, six is a wider escalation than the flag suggests.
func (r Report) OwnerRoles() []string {
	seen := map[string]bool{}
	for _, f := range r.Findings {
		if f.Kind == FindingRowSecurity {
			seen[f.Owner] = true
		}
	}
	owners := make([]string, 0, len(seen))
	for o := range seen {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	return owners
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
