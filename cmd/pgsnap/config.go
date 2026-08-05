package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/nullstone-io/pg-snapshot/blobstore"
	"github.com/nullstone-io/pg-snapshot/scrub"
)

// Configuration arrives entirely through the environment, and reuses Nullstone's built-in names
// wherever one exists.
//
// That is what lets the restore be an ordinary app: attach `aws-postgres-access` and an S3 access
// capability to it and most of this is already populated, with no pgsnap-specific wiring.
const (
	// postgresURLEnvVar is the name Nullstone's postgres access capabilities already publish
	postgresURLEnvVar = "POSTGRES_URL"

	s3BucketURLEnvVar    = "S3_BUCKET_URL"
	s3BucketRegionEnvVar = "S3_BUCKET_REGION"

	gcsBucketURLEnvVar     = "GCS_BUCKET_URL"
	gcsBucketProjectEnvVar = "GCS_BUCKET_PROJECT"

	scrubConfigEnvVar     = "SCRUB_CONFIG"
	scrubConfigFileEnvVar = "SCRUB_CONFIG_FILE"
	numWorkersEnvVar      = "NUM_WORKERS"
	targetDatabaseEnvVar  = "TARGET_DATABASE"
	ownerRoleEnvVar       = "OWNER_ROLE"
	snapshotEnvVar        = "SNAPSHOT"
	backupRetentionEnvVar = "BACKUP_RETENTION"
	replicationEnvVar     = "RESTORE_REPLICATION"
	migrateCommandEnvVar  = "MIGRATE_COMMAND"
	logFormatEnvVar       = "LOG_FORMAT"
	logLevelEnvVar        = "LOG_LEVEL"

	defaultWorkerCount   = 4
	defaultBackupsToKeep = 1
)

func requireEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

// boolEnv reads an on/off switch.
//
// Anything other than a recognised negative is the fallback: this gates behaviour that is on by
// default, and a typo should not quietly disable it.
func boolEnv(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return fallback
	case "0", "f", "false", "no", "off":
		return false
	default:
		return true
	}
}

func intEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", name, raw)
	}
	return n, nil
}

// bucketConfig resolves where snapshots live.
//
// Both clouds are checked rather than switched on a build tag: one binary and one image serve every
// platform. Each cloud's URL comes with its locator -- region on AWS, project on GCP -- and both
// halves are required, because a bucket in another account or project is the normal case here and
// neither can be inferred from where this happens to be running.
func bucketConfig() (blobstore.Config, error) {
	s3URL := strings.TrimSpace(os.Getenv(s3BucketURLEnvVar))
	gcsURL := strings.TrimSpace(os.Getenv(gcsBucketURLEnvVar))

	switch {
	case s3URL != "" && gcsURL != "":
		return blobstore.Config{}, fmt.Errorf("both %s and %s are set; only one bucket can be used",
			s3BucketURLEnvVar, gcsBucketURLEnvVar)

	case s3URL != "":
		region := strings.TrimSpace(os.Getenv(s3BucketRegionEnvVar))
		if region == "" {
			return blobstore.Config{}, fmt.Errorf("%s is required alongside %s",
				s3BucketRegionEnvVar, s3BucketURLEnvVar)
		}
		return blobstore.Config{URI: s3URL, Region: region}, nil

	case gcsURL != "":
		project := strings.TrimSpace(os.Getenv(gcsBucketProjectEnvVar))
		if project == "" {
			return blobstore.Config{}, fmt.Errorf("%s is required alongside %s",
				gcsBucketProjectEnvVar, gcsBucketURLEnvVar)
		}
		return blobstore.Config{URI: gcsURL, Project: project}, nil

	default:
		return blobstore.Config{}, fmt.Errorf("one of %s or %s is required",
			s3BucketURLEnvVar, gcsBucketURLEnvVar)
	}
}

// loadScrubConfig reads the scrub configuration from the environment.
//
// The inline form is the normal one; the file form exists because an ECS task definition is capped
// at 64 KiB in total and a wide enough schema can approach it.
func loadScrubConfig() (*scrub.Config, error) {
	if path := strings.TrimSpace(os.Getenv(scrubConfigFileEnvVar)); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("error reading %s: %w", scrubConfigFileEnvVar, err)
		}
		return scrub.Parse(b)
	}

	if inline := os.Getenv(scrubConfigEnvVar); strings.TrimSpace(inline) != "" {
		return scrub.Parse([]byte(inline))
	}

	// An empty configuration is legitimate: it exports everything as-is. Saying so out loud is
	// better than silently treating "no config" as "nothing sensitive".
	return &scrub.Config{Version: scrub.ConfigVersion}, nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv(logLevelEnvVar)) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(os.Getenv(logFormatEnvVar), "text") {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
