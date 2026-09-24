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
	"slices"
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
	// The root may be a symlink to where the files are. A walk from the
	// link stops at it, so the store keeps the directory it leads to, or the
	// path as given when that cannot be worked out.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &FSStore{root: abs, baseURL: strings.TrimRight(publicURL, "/"), signer: signer, now: time.Now}, nil
}

// BlobPath is where the server mounts the store's URLs.
const BlobPath = "/v1/blobs/"

// Root is the directory the store keeps its files under.
func (s *FSStore) Root() string { return s.root }

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
		return Info{}, ErrExists
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
	written, err := os.Stat(p)
	if err != nil {
		_ = os.Remove(p)
		return Info{}, err
	}
	info := Info{Size: n, ContentType: contentType, Checksum: "sha256:" + hex.EncodeToString(h.Sum(nil)), Modified: written.ModTime()}
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
	if err := json.Unmarshal(meta, &info); err != nil {
		return Info{}, err
	}
	// When it was written is the file's own, as List reports it. The .meta
	// is written after the bytes and removed before them (Delete), so bytes
	// missing under a .meta were lost, not half written or half deleted:
	// that is a fault, and is not reported as nothing there.
	written, err := os.Stat(p)
	if err != nil {
		return Info{}, err
	}
	info.Modified = written.ModTime()
	return info, nil
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

// FinalKey is the staging key itself: a file here is created with O_EXCL, so
// its upload URL was never good for a second write.
func (s *FSStore) FinalKey(stagingKey string) string { return stagingKey }

func (s *FSStore) Finalize(ctx context.Context, stagingKey string) (Info, error) {
	return s.Stat(ctx, stagingKey)
}

// List goes in the order the directory tree is walked in, name by name at
// each level (see walkOrder), and skips whatever comes before after.
func (s *FSStore) List(ctx context.Context, prefix, after string, fn func(key string, modified time.Time) error) error {
	// Only the directory the prefix names is walked, not the whole root:
	// whatever else is kept there is not ours to go through.
	start := s.root
	if dir := prefix[:strings.LastIndex(prefix, "/")+1]; dir != "" {
		var err error
		if start, err = s.path(dir); err != nil {
			return err
		}
	}
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if p == start && errors.Is(err, fs.ErrNotExist) {
			return nil // nothing has been written under the prefix yet
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if d.IsDir() {
			// A directory whose every key comes before after is not gone
			// into, so that taking a listing up again does not walk again
			// through all it has passed.
			if after != "" && p != s.root && walkOrder(key, after) < 0 && !strings.HasPrefix(after, key+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(key, ".meta") || !strings.HasPrefix(key, prefix) {
			return nil
		}
		if after != "" && walkOrder(key, after) <= 0 {
			return nil // listed already
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted while we were walking
		}
		if err != nil {
			return err
		}
		return fn(key, info.ModTime())
	})
	if errors.Is(err, ErrStopList) {
		return nil
	}
	return err
}

// walkOrder compares keys in the order filepath.WalkDir comes to them: path
// element by path element, each by name, a directory before what is in it.
// It is not the order of the keys as strings: courses-old/x comes after
// courses/y here, because courses comes before courses-old.
func walkOrder(a, b string) int {
	return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
}

func (s *FSStore) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	// The .meta goes first, as it is written last: it is what says the file
	// is whole. Stopped half way, a delete leaves bytes without one, which
	// List still finds and a sweep deletes again.
	for _, f := range []string{p + ".meta", p} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
