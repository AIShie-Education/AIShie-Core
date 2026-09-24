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
	// Modified is when the object was last written, by the store's clock:
	// the time List reports, and the orphan sweep judges an upload's age by.
	// S3 answers Stat with it only to the second, so there it may be up to a
	// second earlier than List's. It is not recorded.
	Modified time.Time `json:"-"`
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

	// FinalKey and Finalize make an uploaded object immutable before a
	// document version points at it.
	//
	// An upload URL is a capability to write one key, and with an object
	// store it stays good until it expires: whoever holds it can PUT again
	// and replace the bytes. If the document version recorded that same key,
	// a submitted essay could be swapped after it was handed in, and nothing
	// in the database would notice. So attaching moves the object to a final
	// key that no upload URL was ever issued for, and records that.
	//
	// FinalKey is where the object uploaded under stagingKey will live. It is
	// a pure function of the staging key, so that "already attached" can be
	// checked before anything is copied, and it maps a prefix to a prefix:
	// whatever is staged under p is attached under FinalKey(p).
	FinalKey(stagingKey string) string
	// Finalize moves the object to its final key and describes what is now
	// there. The description is of the final object, read after the move: it
	// is what was attached, whatever is PUT to the staging key afterwards.
	Finalize(ctx context.Context, stagingKey string) (Info, error)

	// List calls fn for every object whose key begins with prefix, with when
	// it was last written, until fn returns an error; ErrStopList ends the
	// listing without being one. It is how uploads that nothing came to
	// point at are found and removed. The bucket or directory may hold other
	// things besides this server's files, and the prefix keeps them out of it.
	//
	// Objects come in an order of the store's own, the same on every call.
	// Given a key it listed as after, List starts past it, so that a listing
	// stopped part way can be taken up again where it stopped.
	List(ctx context.Context, prefix, after string, fn func(key string, modified time.Time) error) error
}

// ErrStopList, returned from a List callback, ends the listing early.
var ErrStopList = errors.New("blob: stop listing")

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

// ErrExists means the key has been written already: a key is written once.
var ErrExists = errors.New("blob: the object already exists; a key is written once")
