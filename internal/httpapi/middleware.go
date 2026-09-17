package httpapi

import (
	"context"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
)

// tooMany answers a call that came too soon. It was never attempted: nothing
// is recorded, and the same idempotency key is good for the retry.
func (s *server) tooMany(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.writeError(w, r, apperr.New(apperr.RateLimited, "too many calls; try again in %d seconds", secs).With("retry_after_seconds", secs))
}

// clientAddr is the address a request came from, without the port. Behind a
// proxy this is the proxy: X-Forwarded-For is deliberately not trusted here,
// since anyone can send it. The per-email limit is what holds in that case.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestInfo is filled in as a request moves through the handlers, so that
// the one log line written at the end can say who it was.
type requestInfo struct{ actor string }

type requestInfoKey struct{}

func noteActor(r *http.Request, actor string) {
	if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
		info.actor = actor
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush lets a streaming handler behind the recorder keep streaming.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logged writes one line per request. It never logs a header, a body or a
// query string — that is where credentials and students' work travel — and
// it cuts the token out of a blob URL, which is a credential in a path.
func (s *server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)))

		if r.URL.Path == "/healthz" && rec.status == http.StatusOK {
			return // a probe every few seconds is not news
		}
		attrs := []any{"method", r.Method, "path", safePath(r.URL.Path), "status", rec.status,
			"ms", time.Since(start).Milliseconds(), "bytes", rec.bytes}
		if info.actor != "" {
			attrs = append(attrs, "actor", info.actor)
		}
		if key := r.Header.Get(HeaderIdempotencyKey); key != "" {
			attrs = append(attrs, "replayed", rec.Header().Get(HeaderReplayed) == "true")
		}
		switch {
		case rec.status >= 500:
			s.Log.Error("request", attrs...)
		default:
			s.Log.Info("request", attrs...)
		}
	})
}

func safePath(p string) string {
	if strings.HasPrefix(p, blob.BlobPath) {
		return blob.BlobPath + "…"
	}
	return p
}
