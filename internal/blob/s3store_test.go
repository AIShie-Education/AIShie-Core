package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
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

	// What the server writes itself, an export, is streamed in, and is
	// what was written; past its limit, nothing is kept.
	export := "exports/" + uuid.NewString() + ".csv"
	written := bytes.Repeat([]byte("對話,"), 1000)
	put, err := s.Put(ctx, export, "text/csv; charset=utf-8", bytes.NewReader(written), 1<<20)
	if err != nil || put.Size != int64(len(written)) || !strings.HasPrefix(put.Checksum, "sha256:") {
		t.Fatalf("put: %+v %v", put, err)
	}
	defer func() { _ = s.Delete(ctx, export) }()
	if got, err := s.Stat(ctx, export); err != nil || got.Size != int64(len(written)) || got.ContentType != "text/csv; charset=utf-8" {
		t.Fatalf("stat of what was put: %+v %v", got, err)
	}
	tooLarge := "exports/" + uuid.NewString() + ".csv"
	if _, err := s.Put(ctx, tooLarge, "text/csv", bytes.NewReader(written), 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("put past its limit: %v", err)
	}
	if _, err := s.Stat(ctx, tooLarge); !errors.Is(err, ErrNotFound) {
		t.Fatalf("what was put past its limit is kept: %v", err)
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

// A request names the bucket as S3_BUCKET_LOOKUP says: after the endpoint,
// in its host name, or as the S3 client judges by the endpoint, which puts
// it in the host name for AWS and after the endpoint for anything else. A
// presigned URL says which, and is made without asking the store anything,
// since the region is given.
func TestS3StoreNamesTheBucketAsItIsTold(t *testing.T) {
	ctx := context.Background()
	const key = "courses/c/u"
	for _, c := range []struct {
		endpoint, lookup string
		inHost           bool
	}{
		{"objects.example.edu", "", false},
		{"objects.example.edu", "auto", false},
		{"objects.example.edu", "path", false},
		{"objects.example.edu", "dns", true},
		{"s3.eu-west-1.amazonaws.com", "auto", true},
		{"s3.eu-west-1.amazonaws.com", "dns", true},
		{"s3.eu-west-1.amazonaws.com", "path", false},
	} {
		s, err := NewS3Store(S3Config{Endpoint: c.endpoint, Bucket: "aishie", Region: "eu-west-1", AccessKey: "access", SecretKey: "secret",
			UseSSL: true, BucketLookup: c.lookup})
		if err != nil {
			t.Fatal(err)
		}
		get, err := s.PresignGet(ctx, key, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		put, _, err := s.PresignPut(ctx, key, "application/pdf", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{get, put} {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			inHost := strings.HasPrefix(u.Host, "aishie.") && u.Path == "/"+key
			inPath := !strings.HasPrefix(u.Host, "aishie.") && u.Path == "/aishie/"+key
			if u.Scheme != "https" || inHost != c.inHost || inPath == c.inHost {
				t.Errorf("%s with the bucket lookup %q: %s://%s%s", c.endpoint, c.lookup, u.Scheme, u.Host, u.Path)
			}
			if c.endpoint == "objects.example.edu" && strings.TrimPrefix(u.Host, "aishie.") != c.endpoint {
				t.Errorf("%s with the bucket lookup %q is sent to %s", c.endpoint, c.lookup, u.Host)
			}
		}
	}
	for _, bad := range []string{"virtual", "DNS"} {
		if _, err := NewS3Store(S3Config{Endpoint: "objects.example.edu", Bucket: "aishie", Region: "eu-west-1", BucketLookup: bad}); err == nil {
			t.Errorf("the bucket lookup %q was taken", bad)
		}
	}
}

// S3_REGION is the region requests are signed for, and with AWS it is where
// they are sent. The S3 client sends a region its table of regions does not
// have, one newer than its release, to us-east-1, which refuses a request
// signed for another; the store sends it to S3_ENDPOINT, if that names the
// region, or to the region's own endpoint. A region the table has, and
// another service, are sent requests where they always were.
func TestS3StoreSendsRequestsToTheRegionTheyAreSignedFor(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		endpoint, region, lookup string
		// host is where requests must go; empty, anywhere of the region's.
		host string
	}{
		{"s3.xx-future-1.amazonaws.com", "xx-future-1", "", "aishie.s3.xx-future-1.amazonaws.com"},
		{"s3.xx-future-1.amazonaws.com", "xx-future-1", "path", "s3.xx-future-1.amazonaws.com"},
		{"s3.xx-future-1.amazonaws.com:443", "xx-future-1", "", "aishie.s3.xx-future-1.amazonaws.com"},
		{"s3.dualstack.xx-future-1.amazonaws.com", "xx-future-1", "", "aishie.s3.dualstack.xx-future-1.amazonaws.com"},
		{"s3.amazonaws.com", "xx-future-1", "", "aishie.s3.xx-future-1.amazonaws.com"},
		{"s3.us-east-1.amazonaws.com", "xx-future-1", "", "aishie.s3.xx-future-1.amazonaws.com"},
		{"s3.cn-future-1.amazonaws.com.cn", "cn-future-1", "", "aishie.s3.cn-future-1.amazonaws.com.cn"},
		{"s3.eu-west-1.amazonaws.com", "eu-west-1", "", ""},
		{"s3.amazonaws.com", "eu-west-1", "path", ""},
		{"s3.amazonaws.com", "us-east-1", "", ""},
		{"objects.example.edu", "xx-future-1", "", "objects.example.edu"},
		{"objects.example.edu", "xx-future-1", "dns", "aishie.objects.example.edu"},
	} {
		name := c.endpoint + " in " + c.region
		sent := &recorder{}
		s, err := newS3Store(S3Config{Endpoint: c.endpoint, Bucket: "aishie", Region: c.region, AccessKey: "access", SecretKey: "secret",
			UseSSL: true, BucketLookup: c.lookup}, sent)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		get, err := s.PresignGet(ctx, "courses/c/u", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		put, _, err := s.PresignPut(ctx, "courses/c/u", "application/pdf", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stat(ctx, "courses/c/u"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: stat: %v", name, err)
		}
		if err := s.Delete(ctx, "courses/c/u"); err != nil {
			t.Fatalf("%s: delete: %v", name, err)
		}
		if len(sent.requests) != 2 {
			t.Fatalf("%s: %d requests were sent, want 2", name, len(sent.requests))
		}
		for _, r := range append(sent.requests, presigned(t, get), presigned(t, put)) {
			good := r.host == c.host
			if c.host == "" {
				good = strings.Contains(r.host, "."+c.region+".") && strings.HasSuffix(r.host, ".amazonaws.com")
			}
			if !good || r.region != c.region {
				t.Errorf("%s (bucket lookup %q): sent to %s, signed for %q", name, c.lookup, r.host, r.region)
			}
		}
	}

	// A bucket whose name has a dot is not sent to the endpoint of a
	// region the client does not know; the store says so rather than send
	// it to us-east-1.
	if _, err := NewS3Store(S3Config{Endpoint: "s3.xx-future-1.amazonaws.com", Bucket: "files.example.edu", Region: "xx-future-1",
		UseSSL: true}); err == nil || !strings.Contains(err.Error(), "dot") {
		t.Fatalf("a bucket with a dot in a region the client does not know: %v", err)
	}
	if _, err := NewS3Store(S3Config{Endpoint: "s3.eu-west-1.amazonaws.com", Bucket: "files.example.edu", Region: "eu-west-1",
		UseSSL: true}); err != nil {
		t.Fatalf("a bucket with a dot in a region the client knows: %v", err)
	}
}

// sentRequest is where a request went, or a presigned URL would send one,
// and the region it is signed for.
type sentRequest struct{ host, region string }

// recorder is a store with nothing in it: every request is answered 404
// there and then, and kept.
type recorder struct {
	mu       sync.Mutex
	requests []sentRequest
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	credential, _, _ := strings.Cut(strings.TrimPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential="), ",")
	r.mu.Lock()
	r.requests = append(r.requests, sentRequest{req.URL.Host, signedFor(credential)})
	r.mu.Unlock()
	status := http.StatusNotFound
	if req.Method == http.MethodDelete {
		status = http.StatusNoContent
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func presigned(t *testing.T, raw string) sentRequest {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return sentRequest{u.Host, signedFor(u.Query().Get("X-Amz-Credential"))}
}

// signedFor is the region a signature's credential scope names:
// <access key>/<date>/<region>/s3/aws4_request.
func signedFor(credential string) string {
	if parts := strings.Split(credential, "/"); len(parts) == 5 && parts[3] == "s3" {
		return parts[2]
	}
	return ""
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
