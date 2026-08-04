package restore

import (
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nullstone-io/pg-snapshot/blobstore"
	"github.com/nullstone-io/pg-snapshot/export"
)

// Loader streams a snapshot's data files into the staging database.
//
// Safe to run fully concurrently and in any order: at this point the schema has only its pre-data
// section, so no foreign key, index, or trigger exists to serialise on. Everything that would
// have needed session_replication_role is simply not there yet.
type Loader struct {
	Pool    *pgxpool.Pool
	Store   blobstore.Store
	Layout  blobstore.Layout
	Workers int
	Log     *slog.Logger
}

func (l Loader) Load(ctx context.Context, entries []export.TableEntry) error {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	workers := l.Workers
	if workers < 1 {
		workers = 4
	}

	// Largest first, so the long pole starts immediately rather than last. With equal-sized work
	// the ordering is irrelevant; with one table dominating it is the difference between the
	// restore taking as long as the biggest table and taking that plus everything else.
	pending := make([]export.TableEntry, 0, len(entries))
	for _, e := range entries {
		if !e.Skipped && e.File != "" {
			pending = append(pending, e)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Bytes > pending[j].Bytes })

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan export.TableEntry)
	errs := make(chan error, len(pending))

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				if err := l.loadOne(ctx, e, log); err != nil {
					errs <- err
					cancel()
					return
				}
			}
		}()
	}

	for _, e := range pending {
		select {
		case jobs <- e:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (l Loader) loadOne(ctx context.Context, e export.TableEntry, log *slog.Logger) error {
	conn, err := l.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("error acquiring connection for %s: %w", e.Qualified(), err)
	}
	defer conn.Release()

	// Safe here specifically because a failed restore discards the whole staging database. There
	// is no half-loaded state worth preserving, so there is nothing to lose by not flushing WAL.
	if _, err := conn.Exec(ctx, `SET synchronous_commit = off`); err != nil {
		return fmt.Errorf("error configuring load session: %w", err)
	}

	object, err := l.Store.Get(ctx, e.File)
	if err != nil {
		return err
	}
	defer object.Close()

	gz, err := gzip.NewReader(object)
	if err != nil {
		return fmt.Errorf("error reading %s: %w", e.File, err)
	}
	defer gz.Close()

	tag, err := conn.Conn().PgConn().CopyFrom(ctx, gz, e.CopyIn())
	if err != nil {
		return fmt.Errorf("error loading %s: %w", e.Qualified(), err)
	}

	if tag.RowsAffected() != e.RowCount {
		return fmt.Errorf("loaded %d rows into %s but the snapshot recorded %d; "+
			"the data file may be truncated", tag.RowsAffected(), e.Qualified(), e.RowCount)
	}

	log.Info("table loaded", "table", e.Qualified(), "rows", tag.RowsAffected())
	return nil
}

// ApplySequences replays every captured sequence position.
//
// Runs after the data and before post-data. Without it every sequence restarts at 1 and the
// first insert in the restored environment collides with a row that is already there.
func ApplySequences(ctx context.Context, pool *pgxpool.Pool, sequences []export.Sequence, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	var applied, skipped int
	for _, s := range sequences {
		if s.Source == export.SourceUnavailable {
			skipped++
			continue
		}
		if _, err := pool.Exec(ctx, s.SetvalSql()); err != nil {
			return fmt.Errorf("error restoring sequence %s: %w", s.Qualified(), err)
		}
		applied++
	}

	log.Info("sequences restored", "applied", applied, "skipped", skipped)
	if skipped > 0 {
		log.Warn("some sequences restart at their start value because the snapshot could not read them",
			"count", skipped)
	}
	return nil
}
