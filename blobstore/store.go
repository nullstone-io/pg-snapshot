// Package blobstore is the object storage behind a snapshot.
//
// The production bucket is the only thing the two halves of this tool share, and the restore side
// holds read-only credentials to it. That asymmetry is a security property rather than a
// convenience, so the interface deliberately keeps writes and deletes separable from reads.
package blobstore

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Store is object storage, narrowed to what a snapshot needs.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// ListPrefixes returns the immediate "directories" under prefix, which is how snapshots are
	// enumerated without listing every data file inside them
	ListPrefixes(ctx context.Context, prefix string) ([]string, error)

	Delete(ctx context.Context, key string) error
	String() string
}

// Config locates a bucket.
//
// Region and Project are carried explicitly rather than inferred from the ambient environment.
// The snapshot bucket lives in the production account or project while the restore runs in a lower
// one, so the caller's own region and project are exactly the wrong defaults -- an inferred region
// sends the request to the wrong S3 endpoint, and the failure looks like a missing object.
type Config struct {
	// URI is gs://bucket/prefix or s3://bucket/prefix. The scheme selects the backend.
	URI string

	// Region is the bucket's region. Required for s3.
	Region string

	// Project is the bucket's GCP project. Required for gs.
	Project string
}

// Open resolves a bucket to a store.
func Open(ctx context.Context, cfg Config) (Store, error) {
	scheme, rest, ok := strings.Cut(cfg.URI, "://")
	if !ok {
		return nil, fmt.Errorf("bucket %q must include a scheme, e.g. gs://my-bucket or s3://my-bucket", cfg.URI)
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return nil, fmt.Errorf("bucket %q names no bucket", cfg.URI)
	}

	switch scheme {
	case "gs":
		if cfg.Project == "" {
			return nil, fmt.Errorf("bucket %q needs its GCP project", cfg.URI)
		}
		return newGCS(ctx, bucket, prefix, cfg.Project)
	case "s3":
		if cfg.Region == "" {
			return nil, fmt.Errorf("bucket %q needs its AWS region", cfg.URI)
		}
		return newS3(ctx, bucket, prefix, cfg.Region)
	default:
		return nil, fmt.Errorf("unsupported bucket scheme %q, expected gs or s3", scheme)
	}
}

// TimestampFormat orders snapshot directories lexicographically, so "newest" is the last key in a
// sorted listing and needs no metadata lookup
const TimestampFormat = "20060102T150405Z"

// Layout is where a snapshot's objects live within a bucket.
type Layout struct {
	Database  string
	Timestamp string
}

func NewLayout(database string, at time.Time) Layout {
	return Layout{Database: database, Timestamp: at.UTC().Format(TimestampFormat)}
}

func (l Layout) Dir() string {
	return fmt.Sprintf("%s/%s", l.Database, l.Timestamp)
}

func (l Layout) Manifest() string {
	return l.Dir() + "/manifest.json"
}

func (l Layout) SchemaDump() string {
	return l.Dir() + "/schema.dump"
}

// DataFile names one table's copy stream. The schema is included so that two tables with the same
// name in different schemas cannot collide.
func (l Layout) DataFile(schema, table string) string {
	return fmt.Sprintf("%s/data/%s.%s.copy.gz", l.Dir(), schema, table)
}

// ListSnapshots returns the timestamps available for a database, oldest first.
func ListSnapshots(ctx context.Context, store Store, database string) ([]string, error) {
	prefixes, err := store.ListPrefixes(ctx, database+"/")
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if ts := strings.Trim(strings.TrimPrefix(p, database+"/"), "/"); ts != "" {
			out = append(out, ts)
		}
	}
	return out, nil
}

// LatestSnapshot resolves the most recent snapshot of a database.
func LatestSnapshot(ctx context.Context, store Store, database string) (string, error) {
	snapshots, err := ListSnapshots(ctx, store, database)
	if err != nil {
		return "", err
	}
	if len(snapshots) < 1 {
		return "", fmt.Errorf("no snapshots of %q found in %s", database, store)
	}

	latest := snapshots[0]
	for _, s := range snapshots[1:] {
		if s > latest {
			latest = s
		}
	}
	return latest, nil
}
