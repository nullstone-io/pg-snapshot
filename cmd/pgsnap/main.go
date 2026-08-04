// Command pgsnap takes scrubbed snapshots of a production postgres database and restores them
// into lower environments.
//
// One binary and one image serve both halves. The snapshot side runs the image as published; the
// restore side runs a customer-extended image that adds their migration tool, because that is the
// only customer-specific part of the process and it only runs in non-production.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nullstone-io/pg-snapshot/blobstore"
	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/restore"
)

// version is set at build time via -ldflags
var version = "dev"

const usage = `pgsnap %s

  pgsnap snapshot   export a scrubbed snapshot of a production database to a bucket
  pgsnap restore    load the newest snapshot into a target database and swap it into place
  pgsnap repair     recover a target database left behind by an interrupted restore

Configuration is read from the environment, reusing Nullstone's built-in names where one exists:

  POSTGRES_URL        postgres connection url (required)
  NUM_WORKERS         concurrent table copies (default %d)
  LOG_FORMAT          json (default) or text
  LOG_LEVEL           debug, info (default), warn, error

  bucket, one pair (the region and project are required, not inferred -- the bucket
  normally lives in another account or project than the one this runs in):
    S3_BUCKET_URL       s3://bucket/prefix
    S3_BUCKET_REGION    the bucket's region
   or
    GCS_BUCKET_URL      gs://bucket/prefix
    GCS_BUCKET_PROJECT  the bucket's project

  snapshot:
    SCRUB_CONFIG        scrub configuration as yaml
    SCRUB_CONFIG_FILE   path to the same, when it is too large for an env var

  restore:
    TARGET_DATABASE     database to replace, e.g. core (required)
    OWNER_ROLE          role that owns the restored objects (required)
    SNAPSHOT            pin a snapshot timestamp (default: newest)
    BACKUP_RETENTION    previous versions of the target to keep (default %d)
    MIGRATE_COMMAND     command that migrates the staging database
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version, defaultWorkerCount, defaultBackupsToKeep)
		os.Exit(2)
	}

	log := newLogger()

	// A snapshot or restore is long-running and interruptible. Cancelling on a signal lets the
	// deferred cleanup drop a half-loaded staging database rather than leaving it for the next
	// run's recovery pass.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "snapshot":
		err = runSnapshot(ctx, log)
	case "restore":
		err = runRestore(ctx, log)
	case "repair":
		err = runRepair(ctx, log)
	case "-h", "--help", "help":
		fmt.Fprintf(os.Stdout, usage, version, defaultWorkerCount, defaultBackupsToKeep)
		return
	case "-v", "--version", "version":
		fmt.Println(version)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		fmt.Fprintf(os.Stderr, usage, version, defaultWorkerCount, defaultBackupsToKeep)
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Warn("interrupted")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func runSnapshot(ctx context.Context, log *slog.Logger) error {
	url, err := requireEnv(postgresURLEnvVar)
	if err != nil {
		return err
	}
	bucket, err := bucketConfig()
	if err != nil {
		return err
	}
	workers, err := intEnv(numWorkersEnvVar, defaultWorkerCount)
	if err != nil {
		return err
	}
	cfg, err := loadScrubConfig()
	if err != nil {
		return err
	}

	store, err := blobstore.Open(ctx, bucket)
	if err != nil {
		return err
	}

	result, err := export.Run(ctx, export.Options{
		ConnURL:     url,
		Store:       store,
		Config:      *cfg,
		Workers:     workers,
		ToolVersion: version,
		Log:         log,
	})
	if err != nil {
		return err
	}

	fmt.Println(result.Layout.Dir())
	return nil
}

func runRestore(ctx context.Context, log *slog.Logger) error {
	url, err := requireEnv(postgresURLEnvVar)
	if err != nil {
		return err
	}
	bucket, err := bucketConfig()
	if err != nil {
		return err
	}
	target, err := requireEnv(targetDatabaseEnvVar)
	if err != nil {
		return err
	}
	owner, err := requireEnv(ownerRoleEnvVar)
	if err != nil {
		return err
	}
	workers, err := intEnv(numWorkersEnvVar, defaultWorkerCount)
	if err != nil {
		return err
	}
	retention, err := intEnv(backupRetentionEnvVar, defaultBackupsToKeep)
	if err != nil {
		return err
	}

	store, err := blobstore.Open(ctx, bucket)
	if err != nil {
		return err
	}

	result, err := restore.Run(ctx, restore.Options{
		AdminURL:        url,
		Target:          target,
		Owner:           owner,
		Store:           store,
		Snapshot:        os.Getenv(snapshotEnvVar),
		Workers:         workers,
		BackupRetention: retention,
		MigrateCommand:  os.Getenv(migrateCommandEnvVar),
		Log:             log,
	})
	if err != nil {
		return err
	}

	fmt.Printf("restored %s from %s (previous database kept as %s)\n",
		target, result.Snapshot, result.Backup)
	return nil
}

// runRepair is the manual entry point for the recovery that a restore performs automatically.
//
// It exists because the situation it fixes, a target renamed away and the process killed before the second rename, is one where nobody wants to wait for the next scheduled restore.
func runRepair(ctx context.Context, log *slog.Logger) error {
	url, err := requireEnv(postgresURLEnvVar)
	if err != nil {
		return err
	}
	target, err := requireEnv(targetDatabaseEnvVar)
	if err != nil {
		return err
	}

	adminURL, err := pg.WithDatabase(url, restore.AdminDatabase)
	if err != nil {
		return err
	}
	pool, err := pg.Open(ctx, adminURL, 1)
	if err != nil {
		return err
	}
	defer pool.Close()

	admin := restore.Admin{DB: pool, Log: log}

	locked, err := admin.Lock(ctx, target)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("a restore of %q is running; repair would race it", target)
	}
	defer admin.Unlock(context.WithoutCancel(ctx), target)

	return admin.Recover(ctx, target)
}
