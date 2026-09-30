package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
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
	s, err := NewS3Store(S3Config{Endpoint: endpoint, Bucket: "aishie-test", Region: "us-east-1",
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
	// It is served as a download, as the disk store serves it, never as a
	// page: whatever type the uploader declared, a student's essay.html must
	// not run as script on the store's origin.
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("served with Content-Disposition %q, want an attachment", cd)
	}
	page := "courses/test/" + uuid.NewString()
	pageURL, pageHeaders, err := s.PresignPut(ctx, page, "text/html", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodPut, pageURL, strings.NewReader("<script>alert(document.cookie)</script>"))
	for k, v := range pageHeaders {
		req.Header.Set(k, v)
	}
	if res, err = http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("PUT of a page to the presigned URL: %v %v", res, err)
	}
	res.Body.Close()
	defer func() { _ = s.Delete(ctx, page) }()
	if pageURL, err = s.PresignGet(ctx, page, time.Minute); err != nil {
		t.Fatal(err)
	}
	if res, err = http.Get(pageURL); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if cd := res.Header.Get("Content-Disposition"); res.StatusCode != http.StatusOK || !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("a page uploaded as %s: %d, Content-Disposition %q, want an attachment", res.Header.Get("Content-Type"), res.StatusCode, cd)
	}
	// A message's file downloads under its name, as the disk store serves it.
	namedURL, err := s.PresignDownload(ctx, key, "作業 3.pdf", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res, err = http.Get(namedURL); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if cd := res.Header.Get("Content-Disposition"); res.StatusCode != http.StatusOK || cd != Disposition("作業 3.pdf") {
		t.Fatalf("a named download: %d, Content-Disposition %q", res.StatusCode, cd)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
}

// A listing stopped part way, by the caller or by an error, leaves nothing
// running behind it. The sweep stops one every time it has found a batch of
// orphans, and whatever went on paging for a listing nobody reads any more
// would hold the page it had in hand for as long as the process lived.
func TestS3StoreListingStoppedPartWayLeavesNothingRunning(t *testing.T) {
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT is not set")
	}
	ctx := context.Background()
	s, err := NewS3Store(S3Config{Endpoint: endpoint, Bucket: "aishie-test", Region: "us-east-1",
		AccessKey: os.Getenv("S3_TEST_ACCESS_KEY"), SecretKey: os.Getenv("S3_TEST_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	prefix := "courses/" + uuid.NewString() + "/"
	for range 5 {
		key := prefix + uuid.NewString()
		if _, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Delete(ctx, key) }()
	}

	before := runtime.NumGoroutine()
	gaveUp := errors.New("the caller gave up")
	const listings = 40
	for i := range listings {
		stop, want := ErrStopList, error(nil)
		if i%2 == 1 {
			stop, want = gaveUp, gaveUp
		}
		n := 0
		if err := s.List(ctx, prefix, "", func(string, time.Time) error {
			if n++; n == 2 {
				return stop
			}
			return nil
		}); !errors.Is(err, want) || (want == nil && err != nil) {
			t.Fatalf("a listing stopped with %v: %v", stop, err)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines are running after %d listings were stopped part way, and %d were before", runtime.NumGoroutine(), listings, before)
		}
	}
}

// Nor does a listing whose context is cancelled seem to have come to the end
// of what is there: the sweep would take that for a pass through the store.
func TestS3StoreListingCancelledSaysSo(t *testing.T) {
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT is not set")
	}
	s, err := NewS3Store(S3Config{Endpoint: endpoint, Bucket: "aishie-test", Region: "us-east-1",
		AccessKey: os.Getenv("S3_TEST_ACCESS_KEY"), SecretKey: os.Getenv("S3_TEST_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.List(ctx, "courses/", "", func(string, time.Time) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a listing with its context cancelled: %v", err)
	}
}
