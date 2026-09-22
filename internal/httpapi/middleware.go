package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"runtime/debug"
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

// clientAddr is the address a request came from, as far as it can be known.
//
// Reached directly, that is the peer. Reached through a proxy the operator
// has named in TrustedProxies, it is the last hop of X-Forwarded-For that is
// not itself a trusted proxy — the one the proxy appended, which the client
// could not forge. From anywhere else X-Forwarded-For is ignored, since
// anyone can send it. When a trusted proxy names no client, the address is
// unknown, and the caller must not key a limit on it: behind a proxy that
// would be one bucket for the whole installation.
func (s *server) clientAddr(r *http.Request) (addr string, known bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.trustedProxy(host) {
		return host, host != ""
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" || s.trustedProxy(hop) {
			continue
		}
		if ip := net.ParseIP(hop); ip != nil {
			return ip.String(), true
		}
		return "", false // not an address: something is forging headers
	}
	return "", false
}

// parseCIDR is net.ParseCIDR, here so that a test in this package can build
// a server's proxies without going through NewHandler.
var parseCIDR = net.ParseCIDR

func (s *server) trustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.proxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// recovered turns a panic in a handler into a logged 500 with a JSON body,
// rather than a dropped connection with nothing in our log. The server's own
// recovery would log it, but the client would get no answer at all.
func (s *server) recovered(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v) // the handler meant to abort; let the server see it
				}
				s.Log.Error("panic in handler", "method", r.Method, "path", safePath(r.URL.Path), "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				s.writeError(w, r, fmt.Errorf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
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
		// The rest of the request, and the response, have until then; a
		// handler moving a file asks for longer. Set on every request, so
		// nothing carries over to the next one on the connection.
		rc := http.NewResponseController(w)

		_ = rc.SetReadDeadline(start.Add(s.BodyTimeout))
		_ = rc.SetWriteDeadline(start.Add(s.BodyTimeout))
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
