package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FSStore keeps objects as files under a directory. It is for development,
// tests and single-machine installations. Its URLs point back at this server
// (/v1/blobs/<token>), so a client uploads and downloads exactly as it would
// with S3: one PUT, one GET, no credentials beyond the URL itself.
type FSStore struct {
	root    string
	baseURL string // the server's public URL, without a trailing slash
	signer  *Signer
	now     func() time.Time
}

func NewFSStore(root, publicURL string, signer *Signer) (*FSStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &FSStore{root: abs, baseURL: strings.TrimRight(publicURL, "/"), signer: signer, now: time.Now}, nil
}

// BlobPath is where the server mounts the store's URLs.
const BlobPath = "/v1/blobs/"

func (s *FSStore) PresignPut(_ context.Context, key, contentType string, ttl time.Duration) (string, map[string]string, error) {
	if _, err := s.path(key); err != nil {
		return "", nil, err
	}
	url := s.baseURL + BlobPath + s.signer.signURL(key, "PUT", contentType, ttl, s.now())
	return url, map[string]string{"Content-Type": contentType}, nil
}

func (s *FSStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	if _, err := s.path(key); err != nil {
		return "", err
	}
	return s.baseURL + BlobPath + s.signer.signURL(key, "GET", "", ttl, s.now()), nil
}

func (s *FSStore) Redeem(token, method string) (string, string, error) {
	c, err := s.signer.verifyURL(token, method, s.now())
	return c.Key, c.ContentType, err
}

// path maps a key to a file, and refuses any key that would leave the root.
// Keys are made by us, but the check costs nothing and a traversal would cost
// everything.
func (s *FSStore) path(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00") {
		return "", errors.New("blob: malformed key")
	}
	p := filepath.Join(s.root, filepath.FromSlash(key))
	if !strings.HasPrefix(p, s.root+string(filepath.Separator)) {
		return "", errors.New("blob: malformed key")
	}
	return p, nil
}

func (s *FSStore) Put(_ context.Context, key, contentType string, r io.Reader, maxBytes int64) (Info, error) {
	p, err := s.path(key)
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return Info{}, err
	}
	// O_EXCL: a key is written once. A second PUT to the same URL cannot
	// change what a document version already points at.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // p is confined to root by path()
	if errors.Is(err, fs.ErrExist) {
		return Info{}, errors.New("blob: the object already exists; a key is written once")
	}
	if err != nil {
		return Info{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
	closeErr := f.Close()
	switch {
	case err != nil, closeErr != nil:
		_ = os.Remove(p)
		return Info{}, errors.Join(err, closeErr)
	case n > maxBytes:
		_ = os.Remove(p)
		return Info{}, ErrTooLarge
	}
	info := Info{Size: n, ContentType: contentType, Checksum: "sha256:" + hex.EncodeToString(h.Sum(nil))}
	meta, _ := json.Marshal(info)
	if err := os.WriteFile(p+".meta", meta, 0o640); err != nil { //nolint:gosec // as above
		_ = os.Remove(p)
		return Info{}, err
	}
	return info, nil
}

func (s *FSStore) Stat(_ context.Context, key string) (Info, error) {
	p, err := s.path(key)
	if err != nil {
		return Info{}, err
	}
	meta, err := os.ReadFile(p + ".meta") //nolint:gosec // as above
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	var info Info
	return info, json.Unmarshal(meta, &info)
}

func (s *FSStore) Open(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, Info{}, err
	}
	p, _ := s.path(key)
	f, err := os.Open(p) //nolint:gosec // as above
	return f, info, err
}

func (s *FSStore) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	for _, f := range []string{p, p + ".meta"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
