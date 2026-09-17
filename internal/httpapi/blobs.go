package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"

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
		info, err := local.Put(r.Context(), key, contentType, r.Body, s.MaxUploadBytes)
		switch {
		case errors.Is(err, blob.ErrTooLarge):
			s.writeError(w, r, apperr.Invalid("the file is larger than %d bytes", s.MaxUploadBytes))
		case err != nil:
			s.writeError(w, r, apperr.Conflicts("the upload could not be stored: %v", err))
		default:
			writeJSON(w, http.StatusOK, map[string]any{"byte_size": info.Size, "checksum": info.Checksum})
		}
	}
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
