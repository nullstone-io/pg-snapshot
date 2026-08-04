package blobstore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3store is AWS S3.
//
// Credentials come from the ambient environment -- the task or pod role -- rather than from
// configuration. Cross-account access is granted out of band, and on AWS that means the KMS key
// policy as well as the bucket policy: a bucket-only grant produces an AccessDenied on Get that
// looks like a missing object.
type s3store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	prefix   string
}

// newS3 builds a client pinned to the bucket's own region.
//
// The region is not inferred from the environment on purpose: the snapshot bucket usually lives in
// the production account while the restore runs in a lower one, and the two are not necessarily in
// the same region. A client pointed at the wrong regional endpoint fails in a way that reads like
// a missing object rather than a misconfiguration.
func newS3(ctx context.Context, bucket, prefix, region string) (Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("error loading aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg)

	return &s3store{
		client:   client,
		uploader: manager.NewUploader(client),
		bucket:   bucket,
		prefix:   strings.Trim(prefix, "/"),
	}, nil
}

func (s *s3store) String() string {
	return fmt.Sprintf("s3://%s", s.bucket)
}

func (s *s3store) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

// Put streams through the SDK's uploader, which buffers into multipart chunks rather than
// requiring the whole object in memory -- a data file for a large table does not fit.
func (s *s3store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	counter := &countingReader{r: r}

	if _, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    ptr(s.key(key)),
		Body:   counter,
	}); err != nil {
		return 0, fmt.Errorf("error writing s3://%s/%s: %w", s.bucket, s.key(key), err)
	}
	return counter.n, nil
}

func (s *s3store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    ptr(s.key(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("error reading s3://%s/%s: %w", s.bucket, s.key(key), err)
	}
	return out.Body, nil
}

func (s *s3store) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    &s.bucket,
		Prefix:    ptr(s.key(prefix)),
		Delimiter: ptr("/"),
	})

	out := make([]string, 0)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("error listing s3://%s/%s: %w", s.bucket, s.key(prefix), err)
		}
		for _, cp := range page.CommonPrefixes {
			if cp.Prefix != nil {
				out = append(out, s.unkey(*cp.Prefix))
			}
		}
	}
	return out, nil
}

func (s *s3store) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    ptr(s.key(key)),
	}); err != nil {
		return fmt.Errorf("error deleting s3://%s/%s: %w", s.bucket, s.key(key), err)
	}
	return nil
}

func (s *s3store) unkey(k string) string {
	if s.prefix == "" {
		return k
	}
	return strings.TrimPrefix(k, s.prefix+"/")
}

// countingReader records how many bytes were uploaded. The SDK reports no size for a streaming
// body, and the manifest records one per file.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func ptr[T any](v T) *T { return &v }
