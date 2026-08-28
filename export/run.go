package export

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nullstone-io/pg-snapshot/blobstore"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/scrub"
)

type Options struct {
	ConnURL string
	Store   blobstore.Store
	Config  scrub.Config

	// Workers copy tables concurrently under one exported snapshot
	Workers int

	// ToolVersion is stamped into the manifest
	ToolVersion string

	Log *slog.Logger
}

type Result struct {
	Manifest *Manifest
	Layout   blobstore.Layout
}

// Run performs a snapshot.
//
// Everything that reads user data happens inside a single REPEATABLE READ READ ONLY transaction:
// READ ONLY so the snapshot role structurally cannot write to production even if it has been
// granted membership in a table owner, and REPEATABLE READ so tables copied minutes apart still
// agree with each other.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Workers < 1 {
		opts.Workers = 4
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	salt, err := newSalt()
	if err != nil {
		return nil, err
	}

	pool, err := pg.Open(ctx, opts.ConnURL, opts.Workers)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	database, err := pg.DatabaseOf(opts.ConnURL)
	if err != nil {
		return nil, err
	}

	preflight := Preflight{
		Introspector: Introspector{DB: pool},
		Config:       opts.Config,
		Salt:         salt,
	}
	// preflight, upload schema, read sequences, copy tables -- a snapshot skips nothing
	phases := &pg.Phases{Log: log, Total: 4}
	done := phases.Start("preflight", "database", database)

	report, err := preflight.Run(ctx)

	// Logged before the error is returned, and on every outcome: what a failed snapshot connected
	// to is the first question asked of it, and an error that reports only what it could not find
	// leaves the operator guessing at where it looked.
	if report != nil {
		log.Info("preflight findings",
			"database", report.Database,
			"schemas", report.Schemas(),
			"tables", len(report.Plan),
			"role", report.CurrentUser,
			"server_major", report.ServerMajor(),
			"findings", len(report.Findings))
	}

	if err != nil {
		done(err)
		return nil, err
	}
	if err := report.Err(); err != nil {
		done(err)
		return nil, err
	}
	done(nil)
	serverMajor := report.ServerMajor()

	// The master connection holds the exported snapshot open. Every worker and pg_dump joins it,
	// so the schema, the sequence positions and every table's rows are read at one instant.
	master, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("error acquiring master connection: %w", err)
	}
	defer master.Release()

	tx, err := master.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("error starting export transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
		return nil, fmt.Errorf("error configuring export transaction: %w", err)
	}
	// Turns row-level security from a silent filter into a hard error. Preflight has already
	// established that no table needs it, so this is the guard for a policy added since.
	if _, err := tx.Exec(ctx, `SET row_security = off`); err != nil {
		return nil, fmt.Errorf("error disabling row security: %w", err)
	}

	var snapshotID string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_export_snapshot()`).Scan(&snapshotID); err != nil {
		return nil, fmt.Errorf("error exporting snapshot: %w", err)
	}
	log.Info("export snapshot open", "snapshot", snapshotID, "workers", opts.Workers)

	layout := blobstore.NewLayout(database, time.Now())

	if err := phases.Run(ctx, "upload schema", func() error {
		return uploadSchema(ctx, opts, layout, snapshotID, log)
	}, "path", layout.SchemaDump()); err != nil {
		return nil, err
	}

	var sequences []Sequence
	if err := phases.Run(ctx, "read sequences", func() error {
		sequences, err = Introspector{DB: tx}.Sequences(ctx)
		if err != nil {
			return err
		}
		logSequenceSources(log, sequences)
		return nil
	}); err != nil {
		return nil, err
	}

	var entries []TableEntry
	if err := phases.Run(ctx, "copy tables", func() error {
		entries, err = copyTables(ctx, pool, opts, layout, snapshotID, report.Plan, log)
		return err
	}, "tables", len(report.Plan), "workers", opts.Workers); err != nil {
		return nil, err
	}

	manifest := &Manifest{
		ArtifactVersion:   ArtifactVersion,
		Tool:              opts.ToolVersion,
		CreatedAt:         time.Now().UTC(),
		Source:            Source{ServerMajor: serverMajor, Database: database},
		Scrubbed:          true,
		ScrubConfigSHA256: configHash(opts.Config),
		FKMode:            string(opts.Config.FKMode),
		Tables:            entries,
		Sequences:         sequences,
	}

	if previous, err := previousManifest(ctx, opts.Store, database, layout.Timestamp); err != nil {
		log.Warn("could not read previous manifest for drift comparison", "error", err)
	} else if added := manifest.ColumnsAdded(previous); len(added) > 0 {
		// Reported, never blocked: the user decides what is sensitive, and a new column exports
		// as-is. This is the line that gives them the chance to notice.
		log.Warn("columns new since last snapshot", "count", len(added), "columns", added)
	}

	body, err := manifest.Marshal()
	if err != nil {
		return nil, err
	}
	if _, err := opts.Store.Put(ctx, layout.Manifest(), bytes.NewReader(body)); err != nil {
		return nil, err
	}

	if err := tx.Rollback(ctx); err != nil {
		log.Debug("export transaction rollback", "error", err)
	}

	log.Info("snapshot complete",
		"path", layout.Dir(), "tables", len(entries), "rows", manifest.TotalRows())
	return &Result{Manifest: manifest, Layout: layout}, nil
}

// uploadSchema dumps the structure to a local file and uploads it.
//
// pg_dump needs a real file, and the restore needs one too for parallel post-data, so the
// artifact is a file at both ends and only travels as a stream in between.
func uploadSchema(ctx context.Context, opts Options, layout blobstore.Layout, snapshotID string, log *slog.Logger) error {
	dir, err := os.MkdirTemp("", "pgsnap-schema-")
	if err != nil {
		return fmt.Errorf("error creating temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "schema.dump")
	if err := pg.DumpSchema(ctx, opts.ConnURL, path, snapshotID); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("error reading schema dump: %w", err)
	}
	defer f.Close()

	n, err := opts.Store.Put(ctx, layout.SchemaDump(), f)
	if err != nil {
		return err
	}
	log.Info("schema uploaded", "bytes", n)
	return nil
}

type copyResult struct {
	entry TableEntry
	err   error
}

// copyTables streams every table concurrently, each worker joined to the master's snapshot.
func copyTables(ctx context.Context, pool *pgxpool.Pool, opts Options, layout blobstore.Layout,
	snapshotID string, plan []scrub.Projection, log *slog.Logger) ([]TableEntry, error) {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan scrub.Projection)
	results := make(chan copyResult)

	var wg sync.WaitGroup
	for range opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				entry, err := copyOne(ctx, pool, opts.Store, layout, snapshotID, p, log)
				select {
				case results <- copyResult{entry: entry, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, p := range plan {
			select {
			case jobs <- p:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	entries := make([]TableEntry, 0, len(plan))
	var firstErr error
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				cancel()
			}
			continue
		}
		entries = append(entries, r.entry)
	}
	if firstErr != nil {
		return nil, firstErr
	}

	sortEntries(entries)
	return entries, nil
}

// copyOne streams a single table out of the source and into the bucket.
//
// The data never touches local disk: COPY writes into a gzip writer, which writes into a pipe the
// uploader reads from, and the checksum is taken over the compressed bytes that actually land.
func copyOne(ctx context.Context, pool *pgxpool.Pool, store blobstore.Store, layout blobstore.Layout,
	snapshotID string, p scrub.Projection, log *slog.Logger) (TableEntry, error) {

	entry := TableEntry{
		Schema:     p.Table.Schema,
		Name:       p.Table.Name,
		Skipped:    p.Skipped,
		Columns:    p.Columns,
		Transforms: p.Transforms,
	}
	if p.Skipped {
		log.Info("table skipped", "table", p.Table.Qualified())
		return entry, nil
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return entry, fmt.Errorf("error acquiring connection for %s: %w", p.Table.Qualified(), err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return entry, fmt.Errorf("error starting transaction for %s: %w", p.Table.Qualified(), err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
		return entry, fmt.Errorf("error configuring worker transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET TRANSACTION SNAPSHOT '%s'`, snapshotID)); err != nil {
		return entry, fmt.Errorf("error joining export snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET row_security = off`); err != nil {
		return entry, fmt.Errorf("error disabling row security: %w", err)
	}

	// The tail window is planned on this same transaction, after it joined the export snapshot,
	// so the probe counts exactly the rows the COPY will see and the window agrees with every
	// other table in the run
	copySQL := p.CopyOut()
	if p.TailRows > 0 {
		window, err := PlanTailWindow(ctx, tx, p)
		if err != nil {
			return entry, err
		}
		if window.Full() {
			log.Warn("tail_rows window not applied; exporting the table in full",
				"table", p.Table.Qualified(), "requestedRows", p.TailRows, "reason", window.Reason)
		} else {
			copySQL = p.CopyOutTail(window.StartPage)
			log.Info("tail window planned", "table", p.Table.Qualified(),
				"requestedRows", p.TailRows,
				"pagesRead", window.PagesRead(), "totalPages", window.TotalPages)
		}

		entry.Tail = &TailReport{
			RequestedRows: p.TailRows,
			TotalPages:    window.TotalPages,
			PagesRead:     window.PagesRead(),
		}
		if p.TailReportColumn != "" {
			lo, hi, err := ReadTailRange(ctx, tx, p, window)
			if err != nil {
				return entry, err
			}
			entry.Tail.ReportColumn = p.TailReportColumn
			entry.Tail.Min, entry.Tail.Max = lo, hi
		}
	}

	pr, pw := io.Pipe()
	hasher := sha256.New()
	copied := make(chan int64, 1)

	go func() {
		defer close(copied)

		gz := gzip.NewWriter(io.MultiWriter(pw, hasher))
		tag, err := tx.Conn().PgConn().CopyTo(ctx, gz, copySQL)
		if err == nil {
			copied <- tag.RowsAffected()
			err = gz.Close()
		} else {
			_ = gz.Close()
		}
		pw.CloseWithError(err)
	}()

	key := layout.DataFile(p.Table.Schema, p.Table.Name)
	n, err := store.Put(ctx, key, pr)
	if err != nil {
		return entry, fmt.Errorf("error uploading %s: %w", p.Table.Qualified(), err)
	}

	entry.RowCount = <-copied
	entry.File = key
	entry.Bytes = n
	entry.SHA256 = hex.EncodeToString(hasher.Sum(nil))

	if msg, short := tailShortfall(entry); short {
		log.Warn(msg, "table", p.Table.Qualified(),
			"requestedRows", p.TailRows, "rows", entry.RowCount)
	}

	log.Info("table exported", "table", p.Table.Qualified(), "rows", entry.RowCount, "bytes", n)
	return entry, nil
}

// previousManifest reads the most recent snapshot older than the one being written, for the
// drift report. A missing or unreadable previous snapshot is not an error -- the first snapshot
// has nothing to compare against.
func previousManifest(ctx context.Context, store blobstore.Store, database, current string) (*Manifest, error) {
	snapshots, err := blobstore.ListSnapshots(ctx, store, database)
	if err != nil {
		return nil, err
	}

	previous := ""
	for _, s := range snapshots {
		if s < current && s > previous {
			previous = s
		}
	}
	if previous == "" {
		return nil, nil
	}

	r, err := store.Get(ctx, blobstore.Layout{Database: database, Timestamp: previous}.Manifest())
	if err != nil {
		return nil, err
	}
	defer r.Close()

	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseManifest(b)
}

func logSequenceSources(log *slog.Logger, sequences []Sequence) {
	var derived, unavailable int
	for _, s := range sequences {
		switch s.Source {
		case SourceMaxColumn:
			derived++
		case SourceUnavailable:
			unavailable++
			log.Warn("sequence position could not be captured; it will restore at its start value",
				"sequence", s.Qualified())
		}
	}
	if derived > 0 {
		log.Info("sequence positions derived from owned columns", "count", derived,
			"reason", "the snapshot role cannot read the sequences directly")
	}
}

// newSalt produces the per-run salt for deterministic transforms.
//
// Regenerated every run and never persisted, so hashes are consistent within a snapshot -- which
// is what keeps joins over scrubbed columns intact -- but not comparable across snapshots.
func newSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("error generating salt: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// configHash identifies the configuration without reproducing it. JSON rather than fmt because
// encoding/json sorts map keys, which keeps the hash stable across runs.
func configHash(cfg scrub.Config) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

func sortEntries(entries []TableEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Qualified() < entries[j].Qualified()
	})
}
