package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
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

	info, err := s.Stat(ctx, key)
	if err != nil || info.Size != int64(len(body)) || info.ContentType != "application/pdf" || info.Checksum == "" {
		t.Fatalf("stat: %+v %v", info, err)
	}
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
