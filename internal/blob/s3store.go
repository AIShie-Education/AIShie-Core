package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

// S3Config reaches any S3-compatible store: AWS, MinIO, Ceph, R2.
type S3Config struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
	// BucketLookup is how a request names the bucket (S3_BUCKET_LOOKUP):
	// "path" after the endpoint (endpoint/bucket/key), "dns" in the host
	// name (bucket.endpoint/key, virtual-hosted style), or "auto", as
	// empty is, which is dns for AWS, Google Cloud Storage and Alibaba
	// Cloud OSS and path for anything else. A service that takes only
	// virtual-hosted requests needs dns.
	BucketLookup string
}

// bucketLookups are the values S3Config.BucketLookup may take.
var bucketLookups = map[string]minio.BucketLookupType{
	"": minio.BucketLookupAuto, "auto": minio.BucketLookupAuto, "path": minio.BucketLookupPath, "dns": minio.BucketLookupDNS,
}

// S3Store hands out presigned URLs straight to the object store. The bytes
// never touch this server.
type S3Store struct {
	client *minio.Client
	bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error) { return newS3Store(cfg, nil) }

// newS3Store is NewS3Store with the transport requests go through: nil for
// the default one, and in tests one that answers them itself.
func newS3Store(cfg S3Config, transport http.RoundTripper) (*S3Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("blob: S3 needs an endpoint and a bucket")
	}
	lookup, ok := bucketLookups[cfg.BucketLookup]
	if !ok {
		return nil, fmt.Errorf("blob: the bucket lookup %q is not auto, path or dns", cfg.BucketLookup)
	}
	// Requests are signed for the region given (S3_REGION); given one,
	// minio-go never asks the store where the bucket is.
	opts := minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    transport,
	}
	c, err := minio.New(cfg.Endpoint, &opts)
	if err != nil {
		return nil, err
	}
	if err := sendToRegion(c, cfg, opts); err != nil {
		return nil, err
	}
	return &S3Store{client: c, bucket: cfg.Bucket}, nil
}

// usEast1 are where minio-go sends a request for AWS in a region its table
// of regions does not have: us-east-1's endpoint, or its dual-stack one.
var usEast1 = []string{"s3.us-east-1.amazonaws.com", "s3.dualstack.us-east-1.amazonaws.com"}

// sendToRegion has requests for AWS sent to the region they are signed for.
//
// minio-go signs a request for the region it is given, but sends one for AWS
// to the host its own table of regions gives that region, and one for a
// region the table does not have, as a region newer than the release does
// not, to us-east-1, which refuses a request signed for another. Where it
// would, the store sends them to S3_ENDPOINT, if that names the region, or
// else to the region's own endpoint. Whether it would is asked of minio-go
// itself, by a URL presigned without asking the store anything, so that a
// region the table has, or comes to have, is sent where minio-go sends it.
// (It is presigned by a client of its own, with credentials of its own, as
// minio-go presigns nothing without them.) The host is named by the one
// means minio-go has of sending AWS requests to a host of the caller's
// choosing, the one transfer acceleration uses; nothing else about a
// request changes.
func sendToRegion(c *minio.Client, cfg S3Config, opts minio.Options) error {
	if cfg.Region == "" || cfg.Region == "us-east-1" {
		return nil
	}
	opts.Creds = credentials.NewStaticV4("region-probe", "region-probe", "")
	prober, err := minio.New(cfg.Endpoint, &opts)
	if err != nil {
		return err
	}
	probe, err := prober.PresignedGetObject(context.Background(), cfg.Bucket, "aishie-region-probe", time.Minute, nil)
	if err != nil {
		return err
	}
	if !slices.Contains(usEast1, strings.TrimPrefix(probe.Hostname(), cfg.Bucket+".")) {
		return nil
	}
	host := "s3." + cfg.Region + ".amazonaws.com"
	if strings.HasPrefix(cfg.Region, "cn-") {
		host += ".cn"
	}
	if endpoint, err := url.Parse("//" + cfg.Endpoint); err == nil && s3utils.GetRegionFromURL(*endpoint) == cfg.Region {
		host = endpoint.Host
	}
	// minio-go sends no bucket whose name has a dot to a host so named:
	// the name would not match the host's certificate.
	if strings.Contains(cfg.Bucket, ".") {
		return fmt.Errorf("blob: requests for S3_REGION %s cannot be sent to %s for a bucket whose name has a dot; use a bucket without one",
			cfg.Region, host)
	}
	c.SetS3TransferAccelerate(host)
	return nil
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
	return s.PresignDownload(ctx, key, "", ttl)
}

func (s *S3Store) PresignDownload(ctx context.Context, key, filename string, ttl time.Duration) (string, error) {
	// The object keeps the type its uploader declared, which may be
	// text/html. The URL itself asks for it as a download, so that no page
	// runs from the bucket, and that holds for what was attached before too.
	attachment := url.Values{"response-content-disposition": {Disposition(filename)}}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, attachment)
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
	return Info{Size: o.Size, ContentType: o.ContentType, Checksum: "etag:" + o.ETag, Modified: o.LastModified}, nil
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

// List goes in key order, byte by byte, which is how S3 lists and what its
// StartAfter means.
func (s *S3Store) List(ctx context.Context, prefix, after string, fn func(key string, modified time.Time) error) error {
	// The iterator pages as it is ranged over, and leaving the loop stops
	// it. ListObjects would page in a goroutine of its own, which stays
	// blocked, holding its page, unless whoever stops reading drains it.
	for obj := range s.client.ListObjectsIter(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, StartAfter: after, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := fn(obj.Key, obj.LastModified); errors.Is(err, ErrStopList) {
			return nil
		} else if err != nil {
			return err
		}
	}
	// A cancelled context ends the iterator between pages without a word,
	// which is not the end of the listing.
	return ctx.Err()
}

// putPartSize is how much of an object Put holds in memory at a time: one
// part of a multipart upload. Of an object whose size is not known before
// it is written, minio-go would otherwise hold a part large enough for the
// largest object S3 takes, over 500 MiB; at 16 MiB, one Put holds 16 MiB,
// and writes an object of up to 156 GiB in S3's 10,000 parts.
const putPartSize = 16 << 20

// Put streams the object to the store, a part at a time, as it is read. An
// object past maxBytes is not completed: the upload is abandoned, and
// nothing is kept under key.
func (s *S3Store) Put(ctx context.Context, key, contentType string, r io.Reader, maxBytes int64) (Info, error) {
	h := sha256.New()
	limited := &capped{r: io.TeeReader(r, h), left: maxBytes}
	up, err := s.client.PutObject(ctx, s.bucket, key, limited, -1,
		minio.PutObjectOptions{ContentType: contentType, PartSize: putPartSize})
	if errors.Is(err, ErrTooLarge) || limited.over {
		return Info{}, ErrTooLarge
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: up.Size, ContentType: contentType, Checksum: "sha256:" + hex.EncodeToString(h.Sum(nil)),
		Modified: up.LastModified}, nil
}

// capped reads from r until more than left bytes have come, and then fails
// with ErrTooLarge, which abandons the upload reading it.
type capped struct {
	r    io.Reader
	left int64
	over bool
}

func (c *capped) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.left -= int64(n); c.left < 0 {
		c.over = true
		return 0, ErrTooLarge
	}
	return n, err
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
