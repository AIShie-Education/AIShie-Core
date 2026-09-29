package testkit

import (
	"context"
	"io"

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
