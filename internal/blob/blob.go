// Package blob stores the files behind document versions: a lecture PDF, a
// submitted zip, a marked-up essay. The database holds a storage key and a
// description of the file; the bytes live here.
//
// Bytes never pass through a tool call. An MCP agent cannot stream binary
// through a JSON-RPC message, and a browser should not have to go through us
// either. So a caller asks for somewhere to upload, uploads straight to that
// URL, and then hands the tool layer an upload token to attach what it
// uploaded. Reading is the same in reverse: a tool returns a short-lived URL.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound means there is no object under the key.
var ErrNotFound = errors.New("blob: no such object")

// Info describes a stored object, as recorded on document_version.
type Info struct {
	Size        int64
	ContentType string
	// Checksum is "sha256:<hex>" when the store computed it from the bytes,
	// or "etag:<value>" when all it has is the object store's own tag.
	Checksum string
}

// Store is what the tool layer needs from object storage.
type Store interface {
	// PresignPut returns a URL that accepts one PUT of the object's bytes,
	// with the headers the PUT must carry.
	PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (url string, headers map[string]string, err error)
	// PresignGet returns a URL that serves the object for a while.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Stat describes the object, or returns ErrNotFound.
	Stat(ctx context.Context, key string) (Info, error)
	Delete(ctx context.Context, key string) error
}

// Local is implemented by a store whose URLs point back at this server, which
// must then serve them. The filesystem store is one; S3 is not.
type Local interface {
	Store
	// Redeem checks a URL token issued by PresignPut or PresignGet.
	Redeem(token, method string) (key, contentType string, err error)
	Put(ctx context.Context, key, contentType string, r io.Reader, maxBytes int64) (Info, error)
	Open(ctx context.Context, key string) (io.ReadCloser, Info, error)
}

// ErrTooLarge means an upload went past the limit.
var ErrTooLarge = errors.New("blob: upload is larger than allowed")
