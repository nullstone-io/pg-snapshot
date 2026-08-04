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

	byName := make(map[string]catalog.Column, len(t.Columns))
	for _, col := range t.Columns {
		byName[col.Name] = col
	}

	errs := make([]error, 0)

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
