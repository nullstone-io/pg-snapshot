package restore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nullstone-io/pg-snapshot/blobstore"
	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// AdminDatabase is where every database-level operation runs.
//
// A session connected to a database cannot rename it, and the swap renames two.
const AdminDatabase = "postgres"

type Options struct {
	// AdminURL connects as the restore role. Its database is replaced with AdminDatabase for
	// database-level work and with the staging database for loading.
	AdminURL string

	// Target is the database applications connect to, e.g. "core"
	Target string

	// Owner is the role the restored objects belong to. pg_restore --role makes objects correctly
	// owned as they are created, so no REASSIGN OWNED pass is needed afterwards.
	Owner string

	Store blobstore.Store

	// Snapshot pins a timestamp; empty means the most recent
	Snapshot string

	Workers int

	// BackupRetention is how many previous versions of Target to keep
	BackupRetention int

	MigrateCommand string

	Log *slog.Logger
}

type Result struct {
	Snapshot string
	Staging  string
	Backup   string
	Rows     int64
}

// Run restores a snapshot and swaps it into place.
//
// Everything up to the swap happens in a database nothing is connected to, so a failure at any
// point before it leaves the target exactly as it was. The swap itself is two catalog renames.
func Run(ctx context.Context, opts Options) (*Result, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.Workers < 1 {
		opts.Workers = 4
	}
	if opts.BackupRetention < 0 {
		opts.BackupRetention = 0
	}

	adminURL, err := pg.WithDatabase(opts.AdminURL, AdminDatabase)
	if err != nil {
		return nil, err
	}
	adminPool, err := pg.Open(ctx, adminURL, 2)
	if err != nil {
		return nil, err
	}
	defer adminPool.Close()

	admin := Admin{DB: adminPool, Log: log}

	// Two restores of the same target must not interleave: both would create staging databases
	// and both would try to rename the same target.
	locked, err := admin.Lock(ctx, opts.Target)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, fmt.Errorf("another restore of %q is already running", opts.Target)
	}
	defer admin.Unlock(context.WithoutCancel(ctx), opts.Target)

	// Whatever a previous run left behind is resolved before anything new is created
	if err := admin.Recover(ctx, opts.Target); err != nil {
		return nil, err
	}

	set, err := admin.Inspect(ctx, opts.Target)
	if err != nil {
		return nil, err
	}
	if state := Classify(set); state != StateIdle {
		return nil, fmt.Errorf("target %q is not in a state a restore can start from: %s (%s)",
			opts.Target, state, set.Describe())
	}

	// Dropped now rather than at the end of the previous run: a backup is only safe to discard
	// once there is a newer database to fall back to
	for _, name := range set.ExpiredBackups(opts.BackupRetention) {
		if err := admin.DropDatabase(ctx, name); err != nil {
			return nil, err
		}
	}

	manifest, snapshot, err := resolveSnapshot(ctx, opts, adminPool, log)
	if err != nil {
		return nil, err
	}
	layout := blobstore.Layout{Database: manifest.Source.Database, Timestamp: snapshot}

	staging, err := StagingName()
	if err != nil {
		return nil, err
	}
	if err := admin.CreateDatabase(ctx, staging, opts.Owner); err != nil {
		return nil, err
	}

	// Any failure from here until the swap discards the staging database and leaves the target
	// untouched, so there is no partial state to reason about
	success := false
	defer func() {
		if !success {
			log.Warn("restore failed; discarding staging database", "database", staging)
			if err := admin.DropDatabase(context.WithoutCancel(ctx), staging); err != nil {
				log.Error("could not drop staging database", "database", staging, "error", err)
			}
		}
	}()

	stagingURL, err := pg.WithDatabase(opts.AdminURL, staging)
	if err != nil {
		return nil, err
	}

	schemaPath, cleanup, err := fetchSchema(ctx, opts.Store, layout)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Asked of the admin connection rather than the staging one: extension availability is a
	// property of the instance, and the same list governs both sections.
	listPath, err := PlanExtensions(ctx, adminPool, schemaPath, log)
	if err != nil {
		return nil, err
	}

	log.Info("restoring schema", "section", pg.SectionPreData, "database", staging)
	if err := pg.RestoreSection(ctx, stagingURL, schemaPath, pg.RestoreOptions{
		Section:  pg.SectionPreData,
		Role:     opts.Owner,
		ListPath: listPath,
	}); err != nil {
		return nil, err
	}

	stagingPool, err := pg.Open(ctx, stagingURL, opts.Workers)
	if err != nil {
		return nil, err
	}
	defer stagingPool.Close()

	loader := Loader{
		Pool:    stagingPool,
		Store:   opts.Store,
		Layout:  layout,
		Workers: opts.Workers,
		Log:     log,
	}
	if err := loader.Load(ctx, manifest.Tables); err != nil {
		return nil, err
	}
	if err := ApplySequences(ctx, stagingPool, manifest.Sequences, log); err != nil {
		return nil, err
	}

	// Foreign keys, indexes and triggers all land here, after every row is already in place
	log.Info("restoring constraints and indexes", "section", pg.SectionPostData, "jobs", opts.Workers)
	if err := pg.RestoreSection(ctx, stagingURL, schemaPath, pg.RestoreOptions{
		Section:  pg.SectionPostData,
		Jobs:     opts.Workers,
		Role:     opts.Owner,
		ListPath: listPath,
	}); err != nil {
		return nil, err
	}

	migrator := Migrator{
		Command:     opts.MigrateCommand,
		DatabaseURL: stagingURL,
		Log:         log,
	}
	if err := migrator.Run(ctx); err != nil {
		return nil, err
	}

	if err := (Carryover{DB: adminPool, Log: log}).Apply(ctx, opts.Target, staging); err != nil {
		return nil, err
	}

	// Statistics are not carried by the snapshot, and a database with none plans every query
	// badly. Cheaper here than after the swap, where it would compete with live traffic.
	log.Info("analyzing", "database", staging)
	if err := pg.Analyze(ctx, stagingURL, opts.Workers); err != nil {
		return nil, err
	}

	// The pool holds open sessions on the staging database, and an open session blocks its rename
	stagingPool.Close()

	backup, err := admin.Swap(ctx, opts.Target, staging)
	if err != nil {
		return nil, err
	}
	success = true

	log.Info("restore complete",
		"target", opts.Target, "snapshot", snapshot, "backup", backup, "rows", manifest.TotalRows())

	return &Result{
		Snapshot: snapshot,
		Staging:  staging,
		Backup:   backup,
		Rows:     manifest.TotalRows(),
	}, nil
}

// resolveSnapshot picks the snapshot to restore and checks it may be restored here.
func resolveSnapshot(ctx context.Context, opts Options, adminPool pg.Querier, log *slog.Logger) (*export.Manifest, string, error) {
	database, err := pg.DatabaseOf(opts.AdminURL)
	if err != nil {
		return nil, "", err
	}
	// Snapshots are filed under the *source* database name, which is the target's name in the
	// normal case of restoring production's `core` into another environment's `core`
	if opts.Target != "" {
		database = opts.Target
	}

	snapshot := opts.Snapshot
	if snapshot == "" {
		if snapshot, err = blobstore.LatestSnapshot(ctx, opts.Store, database); err != nil {
			return nil, "", err
		}
	}

	layout := blobstore.Layout{Database: database, Timestamp: snapshot}
	r, err := opts.Store.Get(ctx, layout.Manifest())
	if err != nil {
		return nil, "", err
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("error reading manifest: %w", err)
	}
	manifest, err := export.ParseManifest(body)
	if err != nil {
		return nil, "", err
	}

	targetMajor, err := export.Introspector{DB: adminPool}.ServerMajor(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := manifest.Validate(targetMajor); err != nil {
		return nil, "", err
	}

	log.Info("snapshot selected",
		"snapshot", snapshot,
		"createdAt", manifest.CreatedAt,
		"sourceMajor", manifest.Source.ServerMajor,
		"targetMajor", targetMajor,
		"tables", len(manifest.Tables),
		"rows", manifest.TotalRows())

	return manifest, snapshot, nil
}

// fetchSchema downloads the schema archive.
//
// pg_restore needs a real file rather than a stream: parallel post-data restore seeks within the
// archive, which is the whole reason the index builds can overlap.
func fetchSchema(ctx context.Context, store blobstore.Store, layout blobstore.Layout) (string, func(), error) {
	dir, err := os.MkdirTemp("", "pgsnap-restore-")
	if err != nil {
		return "", nil, fmt.Errorf("error creating temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	r, err := store.Get(ctx, layout.SchemaDump())
	if err != nil {
		cleanup()
		return "", nil, err
	}
	defer r.Close()

	path := filepath.Join(dir, "schema.dump")
	f, err := os.Create(path)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("error creating schema file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("error downloading schema: %w", err)
	}
	return path, cleanup, nil
}
