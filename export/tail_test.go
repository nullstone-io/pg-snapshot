package export

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTailWindowPages(t *testing.T) {
	// The production case this feature was built from: a 2000-page probe holding 25k live rows
	// (12.5 rows/page), asked for 2000 rows. 2000 / 12.5 = 160 pages, times the 110% margin.
	t.Run("applies the margin", func(t *testing.T) {
		assert.Equal(t, int64(176), tailWindowPages(2000, 2000, 25000))
	})

	// ceil, not truncation: a window that rounds down is a window that undershoots
	t.Run("rounds up", func(t *testing.T) {
		// 10 / (999/2000) * 1.1 = 22.02... -> 23
		assert.Equal(t, int64(23), tailWindowPages(10, 2000, 999))
	})

	t.Run("never returns zero pages", func(t *testing.T) {
		// One requested row of a dense probe still reads a page
		assert.Equal(t, int64(1), tailWindowPages(1, 2000, 200000))
	})
}

func TestPlanTail(t *testing.T) {
	t.Run("sizes a window against tail density", func(t *testing.T) {
		w := planTail(10000, 25000, 2000)

		assert.False(t, w.Full())
		assert.Equal(t, int64(10000), w.TotalPages)
		assert.Equal(t, int64(10000-176), w.StartPage)
		assert.Equal(t, int64(176), w.PagesRead())
	})

	// A table smaller than the probe measures density over however many pages it has
	t.Run("probe smaller than tailProbePages", func(t *testing.T) {
		// 400 pages, all probed, 50k rows -> 125 rows/page; 2000 rows -> ceil(16 * 1.1) = 18
		w := planTail(400, 50000, 2000)

		assert.False(t, w.Full())
		assert.Equal(t, int64(400-18), w.StartPage)
	})

	t.Run("caps at the whole table", func(t *testing.T) {
		// Asking for more rows than the table plausibly holds degrades to a full export
		w := planTail(400, 50000, 60000)

		assert.True(t, w.Full())
		assert.Equal(t, int64(400), w.PagesRead(), "a full window reads every page")
		assert.Contains(t, w.Reason, "spans the whole table")
	})

	// The margin alone can push a window past the end: 45k of 50k rows needs 360 of 400 pages
	// before the margin and 396 after, which is fine -- but 46k needs 405 and must cap
	t.Run("margin overshoot caps rather than producing a negative start", func(t *testing.T) {
		w := planTail(400, 50000, 46000)
		assert.True(t, w.Full())
	})

	t.Run("empty table", func(t *testing.T) {
		w := planTail(0, 0, 2000)

		assert.True(t, w.Full())
		assert.Equal(t, "table is empty", w.Reason)
	})

	// A tail of only dead tuples leaves density unknowable; the safe direction is exporting
	// more than asked, never a silent zero
	t.Run("dead tail falls back to the whole table", func(t *testing.T) {
		w := planTail(10000, 0, 2000)

		assert.True(t, w.Full())
		assert.Contains(t, w.Reason, "no live rows")
	})
}

func TestTailShortfall(t *testing.T) {
	windowed := func(rows int64) TableEntry {
		return TableEntry{
			RowCount: rows,
			Tail:     &TailReport{RequestedRows: 2000, TotalPages: 10000, PagesRead: 176},
		}
	}

	t.Run("met or exceeded is silent", func(t *testing.T) {
		for _, rows := range []int64{2000, 2200} {
			_, short := tailShortfall(windowed(rows))
			assert.False(t, short, "%d rows meets a request for 2000", rows)
		}
	})

	t.Run("undershoot is reported", func(t *testing.T) {
		msg, short := tailShortfall(windowed(1400))
		assert.True(t, short)
		assert.Contains(t, msg, "fewer rows than requested")
	})

	// The one outcome worse than a short window: a window that missed every live row
	t.Run("zero rows gets its own warning", func(t *testing.T) {
		msg, short := tailShortfall(windowed(0))
		assert.True(t, short)
		assert.Contains(t, msg, "zero rows")
	})

	// A full-table fallback was already reported at plan time, and cannot be short of anything
	t.Run("full-table window is exempt", func(t *testing.T) {
		entry := windowed(300)
		entry.Tail.PagesRead = entry.Tail.TotalPages
		_, short := tailShortfall(entry)
		assert.False(t, short)
	})

	t.Run("tables without tail sampling are exempt", func(t *testing.T) {
		_, short := tailShortfall(TableEntry{RowCount: 0})
		assert.False(t, short)
	})
}
