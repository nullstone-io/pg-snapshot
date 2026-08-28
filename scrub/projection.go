package scrub

import (
	"fmt"
	"strings"

	"github.com/go-multierror/multierror"
	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/catalog"
)

// Projection is the export plan for a single table.
type Projection struct {
	Table catalog.Table

	// Skipped reports mode: skip -- the table's structure is snapshotted but none of its rows
	Skipped bool

	// Columns is the exported column list in SELECT order. The restore replays it verbatim as
	// the column list of its COPY ... FROM STDIN, so the two stay aligned even when the target
	// table's attribute order differs.
	Columns []string

	// Transforms records the *configured* transform per column, for the manifest.
	//
	// Deliberately not the generated SQL: that embeds the run's salt, and the manifest is
	// written to the bucket alongside the data.
	Transforms map[string]string

	// TailRows is the requested heap-tail sample size; 0 exports the table in full.
	//
	// The projection only carries the request: the window itself depends on the table's live
	// heap, so the export sizes it at copy time, inside the transaction that runs the COPY.
	TailRows int64

	// TailReportColumn names the column whose min/max over the exported window goes into the
	// manifest, or "" for none
	TailReportColumn string

	// SelectSQL is the statement the export wraps in COPY (...) TO STDOUT
	SelectSQL string
}

// BuildProjection renders the export plan for one table.
//
// This doubles as configuration validation against the live schema: every rule is resolved
// against a real column here, so a rule naming a column that was dropped in production fails
// loudly rather than silently not applying. Running it over every table is what the export
// preflight does.
func BuildProjection(t catalog.Table, cfg Config, salt string) (*Projection, error) {
	if t.Kind == catalog.RelKindPartitioned {
		return nil, fmt.Errorf("table %q is a partitioned parent and is never exported directly; "+
			"export its leaf partitions instead", t.Qualified())
	}

	tc, _ := cfg.TableConfigFor(t.Schema, t.Name)

	p := &Projection{
		Table:      t,
		Skipped:    tc.Mode == TableModeSkip,
		Columns:    make([]string, 0, len(t.Columns)),
		Transforms: map[string]string{},
	}
	if tc.TailRows != nil {
		p.TailRows = *tc.TailRows
		p.TailReportColumn = tc.TailReportColumn
	}

	byName := make(map[string]catalog.Column, len(t.Columns))
	for _, col := range t.Columns {
		byName[col.Name] = col
	}

	errs := make([]error, 0)

	if tc.TailReportColumn != "" {
		_, exists := byName[tc.TailReportColumn]
		switch {
		case !exists:
			errs = append(errs, fmt.Errorf("table %q: no column %q for tail_report_column",
				t.Qualified(), tc.TailReportColumn))
		case strings.TrimSpace(tc.Columns[tc.TailReportColumn]) != "":
			// The manifest ships to the bucket next to the data; recording a scrubbed column's
			// real min/max there would leak the very range the rule exists to hide
			errs = append(errs, fmt.Errorf("table %q: tail_report_column %q also has a column rule; "+
				"the manifest would record the unscrubbed range", t.Qualified(), tc.TailReportColumn))
		}
	}

	// Rules are resolved against the schema first, so a stale rule is reported even when the
	// table is skipped and nothing would have used it
	for _, name := range sortedKeys(tc.Columns) {
		col, ok := byName[name]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("table %q: no column %q", t.Qualified(), name))
		case col.Generated:
			errs = append(errs, fmt.Errorf("table %q column %q: generated columns are not exported "+
				"and are recomputed on restore; remove the rule", t.Qualified(), name))
		}
	}

	if p.Skipped {
		if len(errs) > 0 {
			return nil, multierror.New(errs)
		}
		return p, nil
	}

	selects := make([]string, 0, len(t.Columns))
	for _, col := range t.Columns {
		// COPY rejects generated columns outright. Leaving them out is not a loss: postgres
		// recomputes them on load, so one derived from a scrubbed column is scrubbed too.
		if col.Generated {
			continue
		}

		p.Columns = append(p.Columns, col.Name)

		transform, configured := tc.Columns[col.Name]
		if !configured {
			selects = append(selects, pq.QuoteIdentifier(col.Name))
			continue
		}

		expr, err := renderTransform(col, transform, salt)
		if err != nil {
			errs = append(errs, fmt.Errorf("table %q column %q: %w", t.Qualified(), col.Name, err))
			continue
		}
		p.Transforms[col.Name] = transform
		selects = append(selects, fmt.Sprintf("%s AS %s", expr, pq.QuoteIdentifier(col.Name)))
	}

	if len(errs) > 0 {
		return nil, multierror.New(errs)
	}
	if len(p.Columns) < 1 {
		return nil, fmt.Errorf("table %q has no exportable columns", t.Qualified())
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s FROM %s.%s",
		strings.Join(selects, ", "), pq.QuoteIdentifier(t.Schema), pq.QuoteIdentifier(t.Name))
	if tc.Where != "" {
		fmt.Fprintf(&b, " WHERE %s", tc.Where)
	}
	p.SelectSQL = b.String()

	return p, nil
}

// CopyOut renders the statement that streams this projection out of the source database
func (p Projection) CopyOut() string {
	return fmt.Sprintf("COPY (%s) TO STDOUT", p.SelectSQL)
}

// CopyOutTail renders the copy statement for the heap-tail window beginning at startPage.
//
// Appending WHERE directly is safe because tail_rows and `where` are mutually exclusive, so
// SelectSQL never carries a filter of its own. No ORDER BY and no LIMIT, deliberately: a sort
// materialises the rows and reintroduces the temp-disk blowup the ctid window exists to avoid.
func (p Projection) CopyOutTail(startPage int64) string {
	return fmt.Sprintf("COPY (%s WHERE %s) TO STDOUT", p.SelectSQL, TailPredicate(startPage))
}

// TailPredicate selects every row at or after a heap page. On the postgres versions this tool
// supports it plans as a Tid Range Scan, which reads only the pages it names.
func TailPredicate(startPage int64) string {
	return fmt.Sprintf("ctid >= '(%d,0)'::tid", startPage)
}

// CopyIn renders the statement that loads this projection into the target database.
//
// The column list is explicit so that a target table whose attribute order differs -- because a
// migration added a column in between -- still loads correctly.
func (p Projection) CopyIn() string {
	quoted := make([]string, 0, len(p.Columns))
	for _, c := range p.Columns {
		quoted = append(quoted, pq.QuoteIdentifier(c))
	}
	return fmt.Sprintf("COPY %s.%s (%s) FROM STDIN",
		pq.QuoteIdentifier(p.Table.Schema), pq.QuoteIdentifier(p.Table.Name), strings.Join(quoted, ", "))
}

// renderTransform resolves a configured transform to SQL, preferring a builtin and falling back
// to treating the value as a raw expression.
func renderTransform(col catalog.Column, transform, salt string) (string, error) {
	if b, ok := LookupBuiltin(transform); ok {
		return b.Expr(col, salt)
	}
	// Raw SQL. Wrapped so that a bare expression like `a || b` cannot rebind against the
	// surrounding select list, but otherwise passed through untouched -- no cast, because the
	// user chose the expression and may be producing a non-text type deliberately.
	return fmt.Sprintf("(%s)", transform), nil
}
