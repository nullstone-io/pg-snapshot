package acc

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/scrub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tail window is a claim about physical heap layout -- that a ctid predicate over the last
// pages captures the newest rows -- so it can only be tested against a real heap.
func TestTailRowsExport(t *testing.T) {
	pool, ctx := Connect(t)

	schema := "pgsnap_acc_tail"
	_, err := pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	})

	// The motivating shape: append-only, random-UUID PK, no index on the timestamp. seq records
	// insertion order so the test can verify which rows the window captured.
	const totalRows = 50_000
	const requested = 2_000
	require.NoError(t, execAll(ctx, pool,
		fmt.Sprintf(`CREATE TABLE %s.events (
			id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			seq        bigint NOT NULL,
			created_at timestamptz NOT NULL
		)`, schema),
		fmt.Sprintf(`INSERT INTO %s.events (seq, created_at)
			SELECT g, timestamptz '2026-01-01' + g * interval '1 second'
			FROM generate_series(1, %d) g`, schema, totalRows),
		// event_tags reference only the oldest events, all of which the tail window will exclude
		fmt.Sprintf(`CREATE TABLE %s.event_tags (
			tag_seq  bigint PRIMARY KEY,
			event_id uuid NOT NULL
		)`, schema),
		fmt.Sprintf(`INSERT INTO %s.event_tags (tag_seq, event_id)
			SELECT seq, id FROM %s.events WHERE seq <= 100`, schema, schema),
	))

	tables, err := export.Introspector{DB: pool}.Tables(ctx)
	require.NoError(t, err)

	n := int64(requested)
	cfg := scrub.Config{Version: 1, FKMode: scrub.FKModeNotValid, Tables: map[string]scrub.TableConfig{
		schema + ".events": {TailRows: &n, TailReportColumn: "created_at"},
	}}

	var projection *scrub.Projection
	for _, tbl := range export.Exportable(tables) {
		if tbl.Schema == schema && tbl.Name == "events" {
			projection, err = scrub.BuildProjection(tbl, cfg, "acc-salt")
			require.NoError(t, err)
		}
	}
	require.NotNil(t, projection, "events table was not discovered by introspection")
	require.Equal(t, int64(requested), projection.TailRows)

	// The same shape copyOne uses: plan the window and run the COPY on one transaction
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	window, err := export.PlanTailWindow(ctx, tx, *projection)
	require.NoError(t, err)
	require.False(t, window.Full(), "a %d-row table must not degrade to a full export", totalRows)
	assert.Less(t, window.PagesRead(), window.TotalPages)

	lo, hi, err := export.ReadTailRange(ctx, tx, *projection, window)
	require.NoError(t, err)

	var out bytes.Buffer
	_, err = tx.Conn().PgConn().CopyTo(ctx, &out, projection.CopyOutTail(window.StartPage))
	require.NoError(t, err, "postgres rejected the tail projection")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")

	// At least what was asked for, and bounded overshoot: the 110% margin plus one partial page
	// of slack, nowhere near the full table
	require.GreaterOrEqual(t, len(lines), requested)
	assert.Less(t, len(lines), requested*3/2, "overshoot beyond the margin suggests the window is mis-sized")

	// Only rows from the tail pages: an append-only heap keeps ctid order equal to insertion
	// order, so the export must be exactly the newest len(lines) rows, contiguous through the end
	seqAt := 1 // column order: id, seq, created_at
	minSeq, maxSeq := int64(1<<62), int64(0)
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		require.Len(t, fields, 3)
		seq, err := strconv.ParseInt(fields[seqAt], 10, 64)
		require.NoError(t, err)
		minSeq = min(minSeq, seq)
		maxSeq = max(maxSeq, seq)
	}
	assert.Equal(t, int64(totalRows), maxSeq, "the newest row must be included")
	assert.Equal(t, int64(totalRows-len(lines)+1), minSeq, "the window must be a contiguous suffix of the heap")

	// The manifest's drift instrument: the reported range is the exported window's
	assert.Contains(t, lo, "2026-01-01", "min of created_at should render as a timestamp")
	expectedMax := fmt.Sprintf("timestamptz '2026-01-01' + %d seconds", totalRows)
	var wantHi string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT (timestamptz '2026-01-01' + $1 * interval '1 second')::text`, totalRows).Scan(&wantHi))
	assert.Equal(t, wantHi, hi, "max of created_at must be the newest row's timestamp (%s)", expectedMax)

	// min(json) does not exist in postgres, and the preflight must say so before any data moves
	// rather than mid-run after other tables have streamed
	t.Run("preflight rejects a report column that cannot be aggregated", func(t *testing.T) {
		_, err := pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE %s.event_payloads (seq bigint PRIMARY KEY, payload json)`, schema))
		require.NoError(t, err)

		bad := scrub.Config{Version: 1, FKMode: scrub.FKModeNotValid, Tables: map[string]scrub.TableConfig{
			schema + ".event_payloads": {TailRows: &n, TailReportColumn: "payload"},
		}}
		_, err = export.Preflight{
			Introspector: export.Introspector{DB: pool},
			Config:       bad,
			Salt:         "acc-salt",
		}.Run(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `tail_report_column "payload" cannot be aggregated`)
	})

	// A tail window orphans child rows exactly as `where` does: event_tags reference events the
	// window excluded, so plain FK creation fails on restore and fk_mode: not_valid survives.
	// This is the property the README documents; here it is demonstrated against a real load.
	t.Run("fk creation on the restored data behaves per fk_mode", func(t *testing.T) {
		restored := "pgsnap_acc_tail_restored"
		require.NoError(t, execAll(ctx, pool,
			fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, restored),
			fmt.Sprintf(`CREATE SCHEMA %s`, restored),
			fmt.Sprintf(`CREATE TABLE %s.events (id uuid PRIMARY KEY, seq bigint NOT NULL, created_at timestamptz NOT NULL)`, restored),
			fmt.Sprintf(`CREATE TABLE %s.event_tags (tag_seq bigint PRIMARY KEY, event_id uuid NOT NULL)`, restored),
			// The tail-sampled parent and the full child, as a restore would load them
			fmt.Sprintf(`INSERT INTO %s.events SELECT * FROM %s.events WHERE seq > %d`,
				restored, schema, totalRows-len(lines)),
			fmt.Sprintf(`INSERT INTO %s.event_tags SELECT tag_seq, event_id FROM %s.event_tags`, restored, schema),
		))
		t.Cleanup(func() {
			pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, restored))
		})

		fk := fmt.Sprintf(`ALTER TABLE %s.event_tags ADD CONSTRAINT event_tags_event_fk
			FOREIGN KEY (event_id) REFERENCES %s.events (id)`, restored, restored)

		_, err := pool.Exec(ctx, fk)
		require.Error(t, err, "validating FK creation must fail against orphaned event_tags")
		assert.Contains(t, err.Error(), "violates foreign key constraint")

		_, err = pool.Exec(ctx, fk+` NOT VALID`)
		require.NoError(t, err, "NOT VALID is what lets a tail-filtered restore keep its FKs")
	})
}
