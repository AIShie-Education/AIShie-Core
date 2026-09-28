package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
)

// A person signed in here asks for an assertion of who they are, for a
// service that hosts agents (a runtime), and the runtime checks it against
// the key published here:
//
//	POST /v1/auth/assertion {"audience": …} → {"assertion": …, "expires_at": …}
//	GET  /v1/auth/keys                      → the JSON Web Key Set that checks it
//
// Neither is a tool: an assertion is no action of anyone's in a course, and
// an agent has no use for one, so neither MCP nor the catalogue offers them.
// The first is a POST behind the authenticated wrapper, so it takes the
// session cookie or a bearer token, is held to the caller's rate limit, and
// comes from another origin only from a trusted one.

const (
	AssertionPath = "/v1/auth/assertion"
	KeysPath      = "/v1/auth/keys"

	// keysMaxAge is how long a runtime may keep the key set before it asks
	// again; one that meets a key id it does not know asks at once.
	keysMaxAge = 5 * time.Minute
)

type assertionIn struct {
	Audience string `json:"audience"`
}

type assertionOut struct {
	Assertion string    `json:"assertion"`
	ExpiresAt time.Time `json:"expires_at"`
}

// errNoAssertions answers both routes on a server that makes no assertions.
var errNoAssertions = apperr.Missing("this server makes no assertions for a service that hosts agents")

// assertions refuses a request for an assertion on a server that makes
// none, before it asks who is calling.
func (s *server) assertions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Assertions == nil || !s.Assertions.Issues() {
			s.writeError(w, r, errNoAssertions)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// assert makes an assertion of the caller for the audience the body names.
// It is written to the response and nowhere else: not to the log, which
// takes no bodies, and not to the database.
func (s *server) assert(w http.ResponseWriter, r *http.Request) {
	var in assertionIn
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.writeError(w, r, apperr.Invalid("the body must be JSON with the audience, and nothing else"))
		return
	}
	p := r.Context().Value(callerKey{}).(auth.Principal)
	a, err := s.Assertions.Assert(r.Context(), p, in.Audience)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, assertionOut{Assertion: a.Token, ExpiresAt: a.ExpiresAt})
}

// keys publishes the key set. Anyone may read it: it holds a public key
// alone.
func (s *server) keys(w http.ResponseWriter, r *http.Request) {
	if s.Assertions == nil {
		s.writeError(w, r, errNoAssertions)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(keysMaxAge.Seconds())))
	writeJSON(w, http.StatusOK, s.Assertions.KeySet())
}
