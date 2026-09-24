package httpapi

import (
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
)

// These two routes exist only when files are kept on this server's own disk.
// They stand in for an object store's presigned URLs, and behave like them:
// the URL is the credential. There is no Authorization header to check —
// whoever holds the URL was given it by a tool call that was authorized, and
// it names one object, one method, for a few minutes.

func (s *server) blobPut(local blob.Local) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, contentType, err := local.Redeem(r.PathValue("token"), http.MethodPut)
		if err != nil {
			s.writeError(w, r, apperr.Forbid("the upload URL is not valid, or has expired"))
			return
		}
		// What was signed is what is stored. A client that sends a different
		// type is refused rather than silently overridden: a PDF slot must
		// not end up holding HTML.
		if got := r.Header.Get("Content-Type"); got != contentType {
			s.writeError(w, r, apperr.Invalid("this URL takes Content-Type %q, not %q", contentType, got))
			return
		}
		// A file takes longer to arrive than a JSON body does. The answer
		// then has as long as any other, from when the file is in: the time
		// the request started with may have gone on the file, and a client
		// that sent it all and heard nothing back would find, on retrying,
		// that the URL has been used.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(s.TransferTimeout))
		body := &uploadBody{Reader: r.Body}
		info, err := local.Put(r.Context(), key, contentType, body, s.MaxUploadBytes)
		_ = rc.SetWriteDeadline(time.Now().Add(s.BodyTimeout))
		switch {
		case errors.Is(err, blob.ErrTooLarge):
			s.writeError(w, r, apperr.Invalid("the file is larger than %d bytes", s.MaxUploadBytes))
		case errors.Is(err, blob.ErrExists):
			s.writeError(w, r, apperr.Conflicts("this URL has been uploaded to already; a file is written once"))
		case body.err != nil:
			// Theirs, as a rule: the uploader hung up, or had not finished
			// when the transfer timeout ran out. A disk that stalls until it
			// has run out looks the same from here, so what went wrong is
			// logged, as a warning: a run of timeouts may be the disk's.
			// Nothing of the file is kept, and the same URL takes it again.
			s.Log.Warn("an upload did not arrive in full", "err", readFault(body.err))
			// The answer claims no cause: a body can be broken off by more
			// than the clock.
			s.writeError(w, r, apperr.Invalid("the file did not arrive in full; upload it again (a file has %d minutes to arrive)",
				int(math.Ceil(s.TransferTimeout.Minutes()))))
		case err != nil:
			// Ours: a full disk, a permission, a path. Logged in full, and
			// the holder of an upload URL is told nothing of it.
			s.writeError(w, r, err)
		default:
			writeJSON(w, http.StatusOK, map[string]any{"byte_size": info.Size, "checksum": info.Checksum})
		}
	}
}

// uploadBody is a request body that keeps its own read error. The store
// says only that it could not write the file; this says whether the file
// ever came.
type uploadBody struct {
	io.Reader
	err error
}

func (b *uploadBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// readFault is what went wrong reading a body, without the addresses a
// network error names: the log has no business knowing who was uploading.
func readFault(err error) error {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Err
	}
	return err
}

func (s *server) blobGet(local blob.Local) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _, err := local.Redeem(r.PathValue("token"), http.MethodGet)
		if err != nil {
			s.writeError(w, r, apperr.Forbid("the download URL is not valid, or has expired"))
			return
		}
		f, info, err := local.Open(r.Context(), key)
		if errors.Is(err, blob.ErrNotFound) {
			s.writeError(w, r, apperr.Missing("there is no such file"))
			return
		}
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		defer func() { _ = f.Close() }()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.TransferTimeout))
		h := w.Header()
		h.Set("Content-Type", info.ContentType)
		h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
		// Uploaded files are other people's bytes served from the API's own
		// origin. They are downloads, never pages: a student's "essay.html"
		// must not run as script with the viewer's session.
		h.Set("Content-Disposition", "attachment")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		h.Set("Cache-Control", "private, no-store")
		_, _ = io.Copy(w, f)
	}
}
