package blob

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config reaches any S3-compatible store: AWS, MinIO, Ceph, R2.
type S3Config struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

// S3Store hands out presigned URLs straight to the object store. The bytes
// never touch this server.
type S3Store struct {
	client *minio.Client
	bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("blob: S3 needs an endpoint and a bucket")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &S3Store{client: c, bucket: cfg.Bucket}, nil
}

// EnsureBucket creates the bucket if it is missing. For development and
// tests; in production the bucket and its policy are made by whoever owns it.
func (s *S3Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil || ok {
		return err
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

func (s *S3Store) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, map[string]string, error) {
	// The content type is part of what is signed, so the PUT must carry it
	// and the object is stored with it.
	h := http.Header{"Content-Type": []string{contentType}}
	u, err := s.client.PresignHeader(ctx, http.MethodPut, s.bucket, key, ttl, url.Values{}, h)
	if err != nil {
		return "", nil, err
	}
	return u.String(), map[string]string{"Content-Type": contentType}, nil
}

func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *S3Store) Stat(ctx context.Context, key string) (Info, error) {
	o, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
			return Info{}, ErrNotFound
		}
		return Info{}, err
	}
	// A presigned PUT gives the store no chance to demand a SHA-256, so the
	// object's ETag is what there is. It still changes when the bytes do.
	return Info{Size: o.Size, ContentType: o.ContentType, Checksum: "etag:" + o.ETag}, nil
}

// attachedPrefix holds objects that document versions point at. No presigned
// PUT is ever issued under it.
const attachedPrefix = "attached/"

func (s *S3Store) FinalKey(stagingKey string) string { return attachedPrefix + stagingKey }

// Finalize copies the staged object, server side, to its final key, removes
// the staged one, and describes the copy. A presigned PUT for the staging key
// may still be valid afterwards; what it writes is an orphan that no document
// points at.
func (s *S3Store) Finalize(ctx context.Context, stagingKey string) (Info, error) {
	final := s.FinalKey(stagingKey)
	if _, err := s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: final},
		minio.CopySrcOptions{Bucket: s.bucket, Object: stagingKey}); err != nil {
		if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
			return Info{}, ErrNotFound
		}
		return Info{}, err
	}
	// Best effort: a staged object left behind is swept as an orphan.
	_ = s.client.RemoveObject(ctx, s.bucket, stagingKey, minio.RemoveObjectOptions{})
	return s.Stat(ctx, final)
}

func (s *S3Store) List(ctx context.Context, prefix string, fn func(key string, modified time.Time) error) error {
	// Cancelling is how the client is told to stop paging.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := fn(obj.Key, obj.LastModified); errors.Is(err, ErrStopList) {
			return nil
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
