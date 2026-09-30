package testkit

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/AIShie-Education/AIShie-Core/internal/blob"
)

// ObjectStore behaves like S3 in the two ways that matter to the tool layer
// and that the filesystem store does not share:
//
//   - an upload URL is good for as many PUTs as its holder cares to make
//     until it expires, each replacing the last;
//   - attaching moves the object to a final key, as blob.S3Store does.
//
// It is the filesystem store underneath, so that the tests which need it run
// without an object store to talk to.
type ObjectStore struct{ *blob.FSStore }

const attached = "attached/"

func (s ObjectStore) FinalKey(stagingKey string) string { return attached + stagingKey }

func (s ObjectStore) Finalize(ctx context.Context, stagingKey string) (blob.Info, error) {
	rc, info, err := s.Open(ctx, stagingKey)
	if err != nil {
		return blob.Info{}, err
	}
	defer func() { _ = rc.Close() }()
	if _, err := s.Put(ctx, s.FinalKey(stagingKey), info.ContentType, rc, 1<<40); err != nil {
		return blob.Info{}, err
	}
	_ = s.Delete(ctx, stagingKey)
	return s.Stat(ctx, s.FinalKey(stagingKey))
}

// Overwrite is a PUT to an upload URL that has been used before.
func (s ObjectStore) Overwrite(ctx context.Context, key, contentType string, r io.Reader) error {
	_ = s.Delete(ctx, key)
	_, err := s.Put(ctx, key, contentType, r, 1<<40)
	return err
}

// MovingStore is this server's disk until Move, and then a bucket the disk's
// files were copied to, as an operator moves them from BLOB_STORE=fs to
// BLOB_STORE=s3: under exactly the keys the disk kept them under. What was
// attached on the disk is in the bucket under its upload's own key, which is
// what its version or message records, and what is attached from then on is
// moved to a final key, as ObjectStore moves it.
type MovingStore struct {
	*blob.FSStore
	moved *atomic.Bool
}

func NewMovingStore(fs *blob.FSStore) MovingStore {
	return MovingStore{FSStore: fs, moved: new(atomic.Bool)}
}

// Move makes the store the bucket the disk's files were copied to.
func (s MovingStore) Move() { s.moved.Store(true) }

func (s MovingStore) store() blob.Store {
	if s.moved.Load() {
		return ObjectStore{FSStore: s.FSStore}
	}
	return s.FSStore
}

func (s MovingStore) FinalKey(stagingKey string) string { return s.store().FinalKey(stagingKey) }

func (s MovingStore) Finalize(ctx context.Context, stagingKey string) (blob.Info, error) {
	return s.store().Finalize(ctx, stagingKey)
}
