package blobstore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// gcs is Google Cloud Storage.
//
// Credentials come from the ambient environment -- Workload Identity on GKE -- rather than from
// configuration, so the snapshot module never handles a key and the restore module's read-only
// access is enforced by IAM rather than by this code.
type gcs struct {
	client  *storage.Client
	bucket  string
	prefix  string
	project string
}

func newGCS(ctx context.Context, bucket, prefix, project string) (Store, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("error creating gcs client: %w", err)
	}
	return &gcs{
		client:  client,
		bucket:  bucket,
		prefix:  strings.Trim(prefix, "/"),
		project: project,
	}, nil
}

// handle resolves the bucket.
//
// Deliberately does *not* set UserProject. That would bill operations to the named project, and
// would require the caller's service account to hold serviceusage.services.use on it -- an extra
// cross-project grant that is invisible until every read fails. The project is required
// configuration because a bucket in another project is the normal case here and stating it makes
// the misconfiguration legible (see errorf), but it does not change how requests are made.
func (g *gcs) handle() *storage.BucketHandle {
	return g.client.Bucket(g.bucket)
}

// errorf annotates a failure with the project the bucket is expected to live in.
//
// A cross-project permission problem otherwise surfaces as a bare 403 or "object doesn't exist",
// with nothing pointing at which project's IAM needs the grant.
func (g *gcs) errorf(action, key string, err error) error {
	return fmt.Errorf("error %s gs://%s/%s (project %s): %w", action, g.bucket, key, g.project, err)
}

func (g *gcs) String() string {
	return fmt.Sprintf("gs://%s", g.bucket)
}

func (g *gcs) key(k string) string {
	if g.prefix == "" {
		return k
	}
	return g.prefix + "/" + k
}

func (g *gcs) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	w := g.handle().Object(g.key(key)).NewWriter(ctx)

	n, err := io.Copy(w, r)
	if err != nil {
		// Close reports the upload's own failure; the copy error is the one worth surfacing
		_ = w.Close()
		return 0, g.errorf("writing", g.key(key), err)
	}
	if err := w.Close(); err != nil {
		return 0, g.errorf("writing", g.key(key), err)
	}
	return n, nil
}

func (g *gcs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := g.handle().Object(g.key(key)).NewReader(ctx)
	if err != nil {
		return nil, g.errorf("reading", g.key(key), err)
	}
	return r, nil
}

func (g *gcs) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	it := g.handle().Objects(ctx, &storage.Query{
		Prefix:    g.key(prefix),
		Delimiter: "/",
	})

	out := make([]string, 0)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, g.errorf("listing", g.key(prefix), err)
		}
		// Delimited listings report directories in Prefix and leave Name empty
		if attrs.Prefix != "" {
			out = append(out, g.unkey(attrs.Prefix))
		}
	}
	return out, nil
}

func (g *gcs) Delete(ctx context.Context, key string) error {
	if err := g.handle().Object(g.key(key)).Delete(ctx); err != nil {
		return g.errorf("deleting", g.key(key), err)
	}
	return nil
}

func (g *gcs) unkey(k string) string {
	if g.prefix == "" {
		return k
	}
	return strings.TrimPrefix(k, g.prefix+"/")
}
