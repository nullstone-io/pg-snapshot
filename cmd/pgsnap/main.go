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
	"strings"
	"syscall"

	"github.com/nullstone-io/pg-snapshot/blobstore"
	"github.com/nullstone-io/pg-snapshot/export"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/restore"
)

// version is set at build time via -ldflags
var version = "dev"

const banner = `
                                    _
   _ __   __ _ ___ _ __   __ _ _ __| |
  | '_ \ / _` + "`" + ` / __| '_ \ / _` + "`" + ` | '_ \ |
  | |_) | (_| \__ \ | | | (_| | |_) |_|
  | .__/ \__, |___/_| |_|\__,_| .__/(_)
  |_|    |___/                |_|      %s / %s

`

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

  restore (the postgres-restore-access capabilities publish the first two as
  RESTORE_TARGET_DATABASE and RESTORE_OWNER_ROLE; either spelling is accepted,
  and the unprefixed one wins when both are set):
    TARGET_DATABASE     database to replace, e.g. core (required)
    OWNER_ROLE          role that owns the restored objects (required)
    SOURCE_DATABASE     database the snapshot was taken from, when it is not
                        named the same as the target, e.g. patterniq
    SNAPSHOT            pin a snapshot timestamp (default: newest)
    BACKUP_RETENTION    previous versions of the target to keep (default %d)
    MIGRATE_COMMAND     command that migrates the staging database
    RESTORE_REPLICATION carry the target's publications and slots (default on)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version, defaultWorkerCount, defaultBackupsToKeep)
		os.Exit(2)
	}

	log := newLogger()

	// To stderr rather than through the logger: it is decoration, and a json log stream being
	// consumed by a collector should not have ascii art parsed into it.
	if command := os.Args[1]; command == "snapshot" || command == "restore" || command == "repair" {
		fmt.Fprintf(os.Stderr, banner, version, command)
	}

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

	// The scrub rules are named rather than dumped: the config can be large, and which tables have
	// a rule is the part worth seeing before an export that cannot be taken back.
	log.Info("snapshot starting",
		"version", version,
		"bucket", store.String(),
		"workers", workers,
		"scrubbedTables", cfg.TableNames(),
		"fkMode", orDefault(string(cfg.FKMode), "validate"))

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
	target, err := requireAnyEnv(targetDatabaseEnvVar, restoreTargetDatabaseEnvVar)
	if err != nil {
		return err
	}
	owner, err := requireAnyEnv(ownerRoleEnvVar, restoreOwnerRoleEnvVar)
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

	// Defaulted to the target, so the usual same-name restore needs no setting at all.
	source := orDefault(strings.TrimSpace(os.Getenv(sourceDatabaseEnvVar)), target)
	snapshot := os.Getenv(snapshotEnvVar)
	migrate := os.Getenv(migrateCommandEnvVar)
	replication := boolEnv(replicationEnvVar, true)

	// Logged before anything runs, and without the connection url, which carries a password.
	// A restore takes about an hour; being able to tell from the first line that it is pointed at
	// the wrong database is worth more than the line costs.
	log.Info("restore starting",
		"version", version,
		"target", target,
		"source", source,
		"owner", owner,
		"bucket", store.String(),
		"snapshot", orDefault(snapshot, "newest"),
		"workers", workers,
		"backupRetention", retention,
		"migrateCommand", orDefault(migrate, "(none)"),
		"replication", replication)

	result, err := restore.Run(ctx, restore.Options{
		AdminURL:        url,
		Target:          target,
		Source:          source,
		Owner:           owner,
		Store:           store,
		Snapshot:        snapshot,
		Workers:         workers,
		BackupRetention: retention,
		MigrateCommand:  migrate,
		Replication:     replication,
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
	target, err := requireAnyEnv(targetDatabaseEnvVar, restoreTargetDatabaseEnvVar)
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
