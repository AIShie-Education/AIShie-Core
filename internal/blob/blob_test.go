package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newFS(t *testing.T) (*FSStore, *Signer) {
	t.Helper()
	signer, err := NewSigner(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewFSStore(filepath.Join(t.TempDir(), "blobs"), "http://lms.test/", signer)
	if err != nil {
		t.Fatal(err)
	}
	return s, signer
}

func TestFSStoreRoundTrip(t *testing.T) {
	s, _ := newFS(t)
	ctx := context.Background()
	key := "courses/c1/essay.pdf"

	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat before upload: %v", err)
	}
	putURL, headers, err := s.PresignPut(ctx, key, "application/pdf", time.Minute)
	if err != nil || !strings.HasPrefix(putURL, "http://lms.test/v1/blobs/") || headers["Content-Type"] != "application/pdf" {
		t.Fatalf("presign put: %q %v %v", putURL, headers, err)
	}
	token := strings.TrimPrefix(putURL, "http://lms.test"+BlobPath)
	gotKey, ct, err := s.Redeem(token, "PUT")
	if err != nil || gotKey != key || ct != "application/pdf" {
		t.Fatalf("redeem: %q %q %v", gotKey, ct, err)
	}
	// A PUT URL is not a GET URL.
	if _, _, err := s.Redeem(token, "GET"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a PUT token used for GET: %v", err)
	}

	body := []byte("%PDF-1.7 the essay")
	info, err := s.Put(ctx, key, ct, bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// echo -n '%PDF-1.7 the essay' | shasum -a 256
	if info.Size != int64(len(body)) || info.ContentType != "application/pdf" || !strings.HasPrefix(info.Checksum, "sha256:") || len(info.Checksum) != 71 ||
		time.Since(info.Modified).Abs() > time.Minute {
		t.Fatalf("info: %+v", info)
	}
	if stat, err := s.Stat(ctx, key); err != nil || stat != info {
		t.Fatalf("stat: %+v %v", stat, err)
	}
	rc, _, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("read back %q", got)
	}
	// A key is written once: what a document version points at cannot be
	// swapped out from under it.
	if _, err := s.Put(ctx, key, ct, strings.NewReader("other bytes"), 1<<20); err == nil {
		t.Fatal("a second PUT replaced the object")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
}

// A file's .meta is written after its bytes and removed before them, so it
// says the file is whole. Bytes lost from under one are a fault, not a file
// that is not there; and a delete stopped half way has taken the .meta
// already, leaving bytes that a listing still finds.
// A file with a name of its own, a message's attachment, downloads under it,
// in whatever script it is written; one without, as a document's file does.
// Either way it is a download, never a page.
func TestAFileDownloadsUnderItsName(t *testing.T) {
	for name, want := range map[string]string{
		"":             "attachment",
		"essay.pdf":    "attachment; filename=essay.pdf",
		"essay 1.pdf":  `attachment; filename="essay 1.pdf"`,
		`say "hi".txt`: `attachment; filename="say \"hi\".txt"`,
		"作業 3.docx":    "attachment; filename*=utf-8''%E4%BD%9C%E6%A5%AD%203.docx",
	} {
		if got := Disposition(name); got != want {
			t.Errorf("Disposition(%q) = %q, want %q", name, got, want)
		}
	}

	s, _ := newFS(t)
	ctx := context.Background()
	key := "conversations/c1/u1"
	named, err := s.PresignDownload(ctx, key, "作業 3.docx", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gotKey, filename, err := s.RedeemDownload(strings.TrimPrefix(named, "http://lms.test"+BlobPath))
	if err != nil || gotKey != key || filename != "作業 3.docx" {
		t.Fatalf("a named download redeems as %q %q %v", gotKey, filename, err)
	}
	plain, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, filename, err := s.RedeemDownload(strings.TrimPrefix(plain, "http://lms.test"+BlobPath)); err != nil || filename != "" {
		t.Fatalf("a download with no name redeems as %q %v", filename, err)
	}
	// An upload URL downloads nothing, named or not.
	put, _, err := s.PresignPut(ctx, key, "text/plain", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RedeemDownload(strings.TrimPrefix(put, "http://lms.test"+BlobPath)); !errors.Is(err, ErrBadToken) {
		t.Fatalf("an upload URL redeemed as a download: %v", err)
	}
}

// An object store is asked, in the URL it signs, to serve the file as a
// download under its name: nothing is sent to the store to sign it.
func TestS3StoreSignsANamedDownload(t *testing.T) {
	s, err := NewS3Store(S3Config{Endpoint: "s3.invalid:9000", Bucket: "aishie", Region: "us-east-1", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"": "attachment", "作業 3.docx": Disposition("作業 3.docx")} {
		raw, err := s.PresignDownload(context.Background(), "attached/conversations/c1/u1", name, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := u.Query().Get("response-content-disposition"); got != want {
			t.Fatalf("a download named %q is signed with %q, want %q", name, got, want)
		}
	}
}

func TestFSStoreTellsLostBytesFromNone(t *testing.T) {
	s, _ := newFS(t)
	ctx := context.Background()
	for _, key := range []string{"lost", "stuck"} {
		if _, err := s.Put(ctx, key, "text/plain", strings.NewReader("hello"), 5); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.path("lost")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "lost"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("stat of a file whose bytes were lost: %v", err)
	}
	if _, _, err := s.Open(ctx, "lost"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("open of a file whose bytes were lost: %v", err)
	}

	// Bytes that cannot be removed, standing in for a delete cut short.
	p, _ = s.path("stuck")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, "in-the-way"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "stuck"); err == nil {
		t.Fatal("a delete that could not remove the bytes said nothing")
	}
	if _, err := os.Stat(p + ".meta"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a delete cut short left the .meta: %v", err)
	}
}

func TestFSStoreLimitsAndPaths(t *testing.T) {
	s, _ := newFS(t)
	ctx := context.Background()

	if _, err := s.Put(ctx, "big", "text/plain", strings.NewReader(strings.Repeat("x", 101)), 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := s.Stat(ctx, "big"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an oversized upload was kept")
	}
	if _, err := s.Put(ctx, "exact", "text/plain", strings.NewReader(strings.Repeat("x", 100)), 100); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	for _, key := range []string{"", "../outside", "a/../../outside", "/etc/passwd", "a\\b", "a\x00b"} {
		if _, err := s.Put(ctx, key, "text/plain", strings.NewReader("x"), 100); err == nil {
			t.Errorf("key %q was accepted", key)
		}
		if _, _, err := s.PresignPut(ctx, key, "text/plain", time.Minute); err == nil {
			t.Errorf("key %q was presigned", key)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(s.root))
	if len(entries) != 1 {
		t.Fatalf("something was written outside the root: %v", entries)
	}
}

// Listing under a prefix goes through what is under it and nothing else: the
// root may hold things that are not the server's.
func TestFSStoreListsUnderAPrefix(t *testing.T) {
	s, _ := newFS(t)
	ctx := context.Background()
	for _, key := range []string{"courses/c1/a", "courses/c2/b", "courses-old/c", "backups/nightly.sql.gz", "README"} {
		if _, err := s.Put(ctx, key, "text/plain", strings.NewReader("x"), 100); err != nil {
			t.Fatal(err)
		}
	}
	list := func(prefix string) []string {
		t.Helper()
		var keys []string
		if err := s.List(ctx, prefix, "", func(key string, _ time.Time) error {
			keys = append(keys, key)
			return nil
		}); err != nil {
			t.Fatalf("list %q: %v", prefix, err)
		}
		return keys
	}
	for prefix, want := range map[string]string{
		"courses/":          "courses/c1/a courses/c2/b",
		"courses/c2/":       "courses/c2/b",
		"courses":           "courses-old/c courses/c1/a courses/c2/b",
		"attached/courses/": "",
		"":                  "README backups/nightly.sql.gz courses-old/c courses/c1/a courses/c2/b",
	} {
		got := list(prefix)
		slices.Sort(got)
		if got := strings.Join(got, " "); got != want {
			t.Errorf("under %q: %q, want %q", prefix, got, want)
		}
	}
}

// BLOB_FS_ROOT may be a symlink to where the files really are. The store
// goes through it as through any directory: a listing from the root itself
// finds the files under it, where a walk that stopped at the link would
// report the link, as ".", for a file.
func TestFSStoreUnderASymlinkedRoot(t *testing.T) {
	signer, err := NewSigner(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(dir, "blobs")); err != nil {
		t.Fatal(err)
	}
	s, err := NewFSStore(filepath.Join(dir, "blobs"), "http://lms.test/", signer)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Put(ctx, "courses/c1/a", "text/plain", strings.NewReader("x"), 100); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"courses/", ""} {
		var keys []string
		if err := s.List(ctx, prefix, "", func(key string, _ time.Time) error {
			keys = append(keys, key)
			return nil
		}); err != nil || !slices.Equal(keys, []string{"courses/c1/a"}) {
			t.Errorf("under %q: listed %q, %v", prefix, keys, err)
		}
	}
}

// The root may hold a directory the server cannot read: lost+found, at the
// top of a volume mounted for the files. Listing the server's own prefix
// walks only the prefix's directory and never comes to it. A walk of the
// whole root would, and would fail there, on every sweep.
func TestFSStoreListsPastADirectoryItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	s, _ := newFS(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "courses/c1/a", "text/plain", strings.NewReader("x"), 100); err != nil {
		t.Fatal(err)
	}
	lost := filepath.Join(s.Root(), "lost+found")
	if err := os.Mkdir(lost, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lost, 0o700) })
	var keys []string
	if err := s.List(ctx, "courses/", "", func(key string, _ time.Time) error {
		keys = append(keys, key)
		return nil
	}); err != nil || !slices.Equal(keys, []string{"courses/c1/a"}) {
		t.Fatalf("listed %q: %v", keys, err)
	}
}

// A listing stopped part way is taken up again after the last key it gave,
// and goes on with exactly what it had not given yet — also where the walk's
// order is not the order of the keys as strings.
func TestFSStoreListingIsTakenUpWhereItStopped(t *testing.T) {
	s, _ := newFS(t)
	ctx := context.Background()
	for _, key := range []string{"courses/b/w", "courses/a-b/y", "courses/a/z", "courses/a/sub/q", "courses/a/x", "courses/b/w2", "other/v"} {
		if _, err := s.Put(ctx, key, "text/plain", strings.NewReader("x"), 100); err != nil {
			t.Fatal(err)
		}
	}
	list := func(after string, stopAt int) []string {
		t.Helper()
		var keys []string
		if err := s.List(ctx, "courses/", after, func(key string, _ time.Time) error {
			if keys = append(keys, key); len(keys) == stopAt {
				return ErrStopList
			}
			return nil
		}); err != nil {
			t.Fatalf("list after %q: %v", after, err)
		}
		return keys
	}
	all := list("", 0)
	if len(all) != 6 {
		t.Fatalf("listed %q", all)
	}
	for n := 1; n <= len(all); n++ {
		var got []string
		for after := ""; ; {
			batch := list(after, n)
			if got = append(got, batch...); len(batch) < n {
				break
			}
			after = batch[len(batch)-1]
		}
		if !slices.Equal(got, all) {
			t.Errorf("in batches of %d: %q, want %q", n, got, all)
		}
	}
}

func TestURLTokensExpire(t *testing.T) {
	s, _ := newFS(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	url, _, _ := s.PresignPut(context.Background(), "k", "text/plain", time.Minute)
	token := strings.TrimPrefix(url, "http://lms.test"+BlobPath)

	s.now = func() time.Time { return now.Add(59 * time.Second) }
	if _, _, err := s.Redeem(token, "PUT"); err != nil {
		t.Fatalf("within the window: %v", err)
	}
	s.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, _, err := s.Redeem(token, "PUT"); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("after the window: %v", err)
	}
}

func TestUploadTokens(t *testing.T) {
	signer, _ := NewSigner(strings.Repeat("k", 32))
	other, _ := NewSigner(strings.Repeat("z", 32))
	claim := UploadClaim{Key: "courses/c/1", CourseID: uuid.New(), MemberID: uuid.New(), Purpose: "submission",
		ContentType: "text/plain", Expires: time.Now().Add(-time.Hour).Unix()}
	token := signer.SignUpload(claim)

	got, err := signer.VerifyUpload(token)
	if err != nil || got != claim {
		// Expired on purpose: attaching does not check expiry. See UploadClaim.
		t.Fatalf("verify: %+v %v", got, err)
	}
	if _, err := other.VerifyUpload(token); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a token signed with another key: %v", err)
	}
	body, mac, _ := strings.Cut(token, ".")
	forged := signer.SignUpload(UploadClaim{Key: "courses/c/someone-elses", CourseID: claim.CourseID, MemberID: claim.MemberID})
	forgedBody, _, _ := strings.Cut(forged, ".")
	for name, bad := range map[string]string{
		"another claim under this signature": forgedBody + "." + mac,
		"truncated":                          body,
		"empty":                              "",
		"garbage":                            "not.a-token",
	} {
		if _, err := signer.VerifyUpload(bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewSigner("too short"); err == nil {
		t.Fatal("a short signing key was accepted")
	}
	if _, err := NewSigner(""); err != nil {
		t.Fatalf("random key: %v", err)
	}
}

// An upload token and a store URL are signed with the same key. Neither may
// ever be taken for the other.
func TestUploadTokensAndURLsAreNotInterchangeable(t *testing.T) {
	s, signer := newFS(t)
	upload := signer.SignUpload(UploadClaim{Key: "courses/c/1", CourseID: uuid.New(), MemberID: uuid.New(), Purpose: "material", Expires: time.Now().Add(time.Hour).Unix()})
	for _, method := range []string{"PUT", "GET"} {
		if _, _, err := s.Redeem(upload, method); !errors.Is(err, ErrBadToken) {
			t.Errorf("an upload token redeemed as a %s URL: %v", method, err)
		}
	}
	url, _, _ := s.PresignPut(context.Background(), "courses/c/1", "text/plain", time.Minute)
	if _, err := signer.VerifyUpload(strings.TrimPrefix(url, "http://lms.test"+BlobPath)); !errors.Is(err, ErrBadToken) {
		t.Errorf("a URL token verified as an upload token: %v", err)
	}
}
