package export

import (
	"context"
	"fmt"

	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/scrub"
)

// Tail sampling exports approximately the newest rows of a table by reading only the end of its
// heap, for the table nothing else can afford: large, append-only, no index on its timestamp.
// A `where` on such a table seq-scans all of it inside the export's long-lived transaction, and
// an ORDER BY ... LIMIT adds a full sort on top. A ctid window reads only the pages it names.
//
// The window is sized from two measurements taken inside the export's own snapshot:
//
//   - the heap's size in pages, from pg_relation_size -- deliberately not pg_class.relpages,
//     which is only refreshed by VACUUM and ANALYZE and silently shifts the window back in time
//     as it goes stale
//   - the live-row density of the tail pages themselves, because tail pages run fuller than the
//     table-wide average and an average-based window undershoots
//
// The result deliberately lands somewhat over the requested count rather than under it.
const (
	// tailProbePages is how many trailing pages the density probe counts rows in
	tailProbePages = 2000

	// tailMarginPct oversizes the window against density drift between the probed pages and the
	// rest of the window. Percent, so the sizing stays in integer math.
	tailMarginPct = 110
)

// TailWindow is the physical slice of a table's heap that a tail_rows export reads.
type TailWindow struct {
	// TotalPages is the heap's size in pages at the time of the probe
	TotalPages int64

	// StartPage is the first heap page the export reads. 0 means the whole table: the window
	// swallowed it, or no window could be sized.
	StartPage int64

	// Reason explains a full-table window, for the log line reporting that no window was applied
	Reason string
}

// Full reports whether the export reads the whole table rather than a tail window
func (w TailWindow) Full() bool { return w.StartPage <= 0 }

// PagesRead is how much of the heap the export reads, for the manifest
func (w TailWindow) PagesRead() int64 { return w.TotalPages - w.StartPage }

// PlanTailWindow sizes the ctid window for a tail_rows table.
//
// It must run on the transaction that will run the COPY: the density it measures has to be the
// density the export will see, or the window disagrees with the rows it is sized for.
func PlanTailWindow(ctx context.Context, db pg.Querier, p scrub.Projection) (TailWindow, error) {
	qualified := fmt.Sprintf("%s.%s",
		pq.QuoteIdentifier(p.Table.Schema), pq.QuoteIdentifier(p.Table.Name))

	var totalPages int64
	if err := db.QueryRow(ctx,
		`SELECT pg_catalog.pg_relation_size($1::regclass) / current_setting('block_size')::bigint`,
		qualified).Scan(&totalPages); err != nil {
		return TailWindow{}, fmt.Errorf("error reading heap size of %s: %w", p.Table.Qualified(), err)
	}
	if totalPages == 0 {
		return planTail(0, 0, p.TailRows), nil
	}

	probeStart := max(totalPages-tailProbePages, 0)
	var probeRows int64
	if err := db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`,
		qualified, scrub.TailPredicate(probeStart))).Scan(&probeRows); err != nil {
		return TailWindow{}, fmt.Errorf("error probing tail density of %s: %w", p.Table.Qualified(), err)
	}

	return planTail(totalPages, probeRows, p.TailRows), nil
}

// planTail turns the probe's two measurements into a window. Pure, so the sizing arithmetic is
// unit-testable; PlanTailWindow is the half that talks to the database.
func planTail(totalPages, probedRows, requestedRows int64) TailWindow {
	w := TailWindow{TotalPages: totalPages}

	switch {
	case totalPages == 0:
		w.Reason = "table is empty"

	case probedRows == 0:
		// No live rows in the probed tail, so density is unknowable. Falling back to the full
		// table is the safe direction: it can only export more than asked, never a silent zero.
		w.Reason = "no live rows in the probed tail pages"

	default:
		if pages := tailWindowPages(requestedRows, min(totalPages, tailProbePages), probedRows); pages < totalPages {
			w.StartPage = totalPages - pages
		} else {
			w.Reason = "requested tail spans the whole table"
		}
	}
	return w
}

// tailWindowPages is ceil(requested / density * margin), carried out in integer math with
// density expressed as probedRows / probedPages.
func tailWindowPages(requestedRows, probedPages, probedRows int64) int64 {
	num := requestedRows * probedPages * tailMarginPct
	den := probedRows * 100
	return (num + den - 1) / den
}

// tailShortfall renders the warning for a windowed export that came back short of the request.
//
// The margin makes this rare, but not impossible: a window wider than the probed pages extends
// into heap whose density the probe never measured, and in the worst case the probed live rows
// all sit below the window's start and the COPY returns nothing. Either way a rule that
// under-applied has to be reported, not left for someone to infer from the manifest later.
//
// A full-table window is exempt -- that fallback was already reported when it was planned, and
// a full export cannot be short of anything.
func tailShortfall(entry TableEntry) (string, bool) {
	t := entry.Tail
	if t == nil || t.PagesRead >= t.TotalPages {
		return "", false
	}
	switch {
	case entry.RowCount == 0:
		return "tail window exported zero rows; the table's live rows sit below the computed window", true
	case entry.RowCount < t.RequestedRows:
		return "tail window exported fewer rows than requested", true
	}
	return "", false
}

// ReadTailRange reads the exported window's min/max of the projection's report column, on the
// same transaction the window was planned on.
//
// This is the drift instrument: the tail-is-newest property degrades silently when the table
// starts taking heavy UPDATEs or is VACUUM FULL'd -- reclaimed low pages absorb new rows -- and
// a reported time window that stops looking recent is how an operator notices.
func ReadTailRange(ctx context.Context, db pg.Querier, p scrub.Projection, w TailWindow) (minVal, maxVal string, err error) {
	sql := tailRangeSQL(p)
	if !w.Full() {
		sql += " WHERE " + scrub.TailPredicate(w.StartPage)
	}

	var lo, hi *string
	if err := db.QueryRow(ctx, sql).Scan(&lo, &hi); err != nil {
		return "", "", fmt.Errorf("error reading tail range of %s: %w", p.Table.Qualified(), err)
	}
	if lo != nil {
		minVal = *lo
	}
	if hi != nil {
		maxVal = *hi
	}
	return minVal, maxVal, nil
}

// checkTailReportColumn proves the report column can be aggregated, before any data moves.
//
// WHERE false reads no rows, so the server type-checks min/max for free. A column whose type has
// no ordering -- json, point -- would otherwise fail mid-run, after the schema upload and however
// many tables happened to copy first.
func checkTailReportColumn(ctx context.Context, db pg.Querier, p scrub.Projection) error {
	var lo, hi *string
	if err := db.QueryRow(ctx, tailRangeSQL(p)+" WHERE false").Scan(&lo, &hi); err != nil {
		return fmt.Errorf("table %q: tail_report_column %q cannot be aggregated with min/max: %w",
			p.Table.Qualified(), p.TailReportColumn, err)
	}
	return nil
}

func tailRangeSQL(p scrub.Projection) string {
	col := pq.QuoteIdentifier(p.TailReportColumn)
	return fmt.Sprintf(`SELECT min(%s)::text, max(%s)::text FROM %s.%s`, col, col,
		pq.QuoteIdentifier(p.Table.Schema), pq.QuoteIdentifier(p.Table.Name))
}
