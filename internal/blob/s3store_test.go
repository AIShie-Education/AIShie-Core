package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// TestS3Store runs against a real S3-compatible store, which CI provides as a
// MinIO container. Without S3_TEST_ENDPOINT it is skipped.
//
//	S3_TEST_ENDPOINT=localhost:9000 S3_TEST_ACCESS_KEY=minioadmin S3_TEST_SECRET_KEY=minioadmin go test ./internal/blob/
func TestS3Store(t *testing.T) {
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT is not set")
	}
	ctx := context.Background()
	s, err := NewS3Store(S3Config{Endpoint: endpoint, Bucket: "aishiteru-test", Region: "us-east-1",
		AccessKey: os.Getenv("S3_TEST_ACCESS_KEY"), SecretKey: os.Getenv("S3_TEST_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	key := "courses/test/" + uuid.NewString()
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat before upload: %v", err)
	}

	putURL, headers, err := s.PresignPut(ctx, key, "application/pdf", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.7 the essay")
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("PUT to the presigned URL: %v %v", res, err)
	}
	res.Body.Close()

	// When it was written is what its age as an upload is told by.
	info, err := s.Stat(ctx, key)
	if err != nil || info.Size != int64(len(body)) || info.ContentType != "application/pdf" || info.Checksum == "" ||
		time.Since(info.Modified).Abs() > time.Minute {
		t.Fatalf("stat: %+v %v", info, err)
	}
	// Listing under a prefix finds what is there and nothing else the bucket
	// holds.
	other := "backups/" + uuid.NewString()
	if _, err := s.client.PutObject(ctx, s.bucket, other, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Delete(ctx, other) }()
	list := func(after string) []string {
		t.Helper()
		var listed []string
		if err := s.List(ctx, "courses/test/", after, func(k string, _ time.Time) error {
			listed = append(listed, k)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return listed
	}
	if listed := list(""); !slices.Contains(listed, key) || slices.ContainsFunc(listed, func(k string) bool { return !strings.HasPrefix(k, "courses/test/") }) {
		t.Fatalf("listed under courses/test/: %q", listed)
	}
	// And one taken up again after a key goes on past it, to what comes
	// after it.
	later := key + "-later"
	if _, err := s.client.PutObject(ctx, s.bucket, later, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Delete(ctx, later) }()
	if listed := list(key); !slices.Contains(listed, later) || slices.ContainsFunc(listed, func(k string) bool { return k <= key }) {
		t.Fatalf("listed after %s: %q", key, listed)
	}
	// Attaching moves the object somewhere the upload URL cannot reach. The
	// URL is still valid, and whoever holds it PUTs again — and changes
	// nothing that was attached.
	final := s.FinalKey(key)
	attached, err := s.Finalize(ctx, key)
	if err != nil || attached.Size != int64(len(body)) {
		t.Fatalf("finalize: %+v %v", attached, err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the staged object is still there: %v", err)
	}
	again, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader([]byte("swapped after handing in")))
	for k, v := range headers {
		again.Header.Set(k, v)
	}
	if res, err := http.DefaultClient.Do(again); err == nil {
		res.Body.Close()
	}
	if after, err := s.Stat(ctx, final); err != nil || after != attached {
		t.Fatalf("a second PUT to the upload URL changed the attached object: %+v -> %+v (%v)", attached, after, err)
	}
	_ = s.Delete(ctx, key) // the orphan the second PUT left
	key = final

	getURL, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(getURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("read back %q", got)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
}
