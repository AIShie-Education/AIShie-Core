package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/canon"
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

type assertionOut struct {
	Assertion string    `json:"assertion"`
	ExpiresAt time.Time `json:"expires_at"`
}

// errNoAssertions answers both routes on a server that makes no assertions.
var errNoAssertions = apperr.Missing("this server makes no assertions for a service that hosts agents")

// errAssertionBody is the one answer to a body that is not exactly
// {"audience": "…"}.
var errAssertionBody = apperr.Invalid(`the body must be one JSON object, {"audience": "…"}, with the audience a string and nothing else in it`)

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
	audience, err := readAudience(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p := r.Context().Value(callerKey{}).(auth.Principal)
	a, err := s.Assertions.Assert(r.Context(), p, audience)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, assertionOut{Assertion: a.Token, ExpiresAt: a.ExpiresAt})
}

// readAudience reads the body of a request for an assertion, which must be
// exactly one JSON object with one member, "audience", a string. It is read
// as strictly as a tool's arguments are, and more: a key in another case, a
// key given twice, another member, or anything after the object is refused
// rather than read one way here and another by whoever looks at it next.
func readAudience(w http.ResponseWriter, r *http.Request) (string, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			return "", err // writeError says how large a body may be
		}
		return "", apperr.Invalid("the body could not be read")
	}
	if err := canon.Check(body); err != nil {
		return "", errAssertionBody
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&fields); err != nil || len(fields) != 1 {
		return "", errAssertionBody
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errAssertionBody
	}
	var audience *string
	if err := json.Unmarshal(fields["audience"], &audience); err != nil || audience == nil {
		return "", errAssertionBody
	}
	return *audience, nil
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
