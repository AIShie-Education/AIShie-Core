package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	if info.Size != int64(len(body)) || info.ContentType != "application/pdf" || !strings.HasPrefix(info.Checksum, "sha256:") || len(info.Checksum) != 71 {
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
	if r, err := NewSigner(""); err != nil || len(r.secret) != 32 {
		t.Fatalf("random key: %v", err)
	}
}
