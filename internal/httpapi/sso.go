package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
)

// Single sign-on, from the browser's side:
//
//	GET /v1/auth/sso/start?return_to=…   → 302 to the identity provider
//	GET /v1/auth/sso/callback?code&state → session cookie, 302 to return_to
//
// Between the two the browser holds a short-lived signed cookie carrying the
// state (so that the answer is to a question this browser asked: without it
// an attacker could finish their own sign-in in the victim's browser), the
// nonce (so that the token answers this sign-in and no other) and where to go
// afterwards.

const (
	ssoCookie     = "ais_sso"
	ssoPurpose    = "sso.state"
	ssoWindow     = 10 * time.Minute
	ssoStartPath  = "/v1/auth/sso/start"
	ssoReturnPath = "/v1/auth/sso/callback"
)

// SSOCallbackPath is what to register with the identity provider, after the
// server's public URL.
const SSOCallbackPath = ssoReturnPath

type ssoState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	ReturnTo string `json:"r"`
	Expires  int64  `json:"e"`
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *server) ssoStart(w http.ResponseWriter, r *http.Request) {
	st := ssoState{State: random(), Nonce: random(), ReturnTo: s.safeReturn(r.URL.Query().Get("return_to")),
		Expires: time.Now().Add(ssoWindow).Unix()}
	// Lax, not Strict: the provider sends the browser back here by a
	// top-level navigation from another site, and the cookie must come too.
	http.SetCookie(w, s.cookie(ssoCookie, s.Signer.Sign(ssoPurpose, st), ssoReturnPath, time.Now().Add(ssoWindow)))
	http.Redirect(w, r, s.SSO.AuthCodeURL(st.State, st.Nonce), http.StatusFound)
}

func (s *server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	// Whatever happens, the state is used once. This is a header, so it is
	// set now, before any of the answers below is written.
	http.SetCookie(w, s.cookie(ssoCookie, "", ssoReturnPath, time.Time{}))

	c, err := r.Cookie(ssoCookie)
	var st ssoState
	if err != nil || s.Signer.Open(ssoPurpose, c.Value, &st) != nil || time.Now().Unix() > st.Expires {
		s.writeError(w, r, apperr.Invalid("this sign-in was not started from this browser, or took too long; start again"))
		return
	}
	q := r.URL.Query()
	if got := q.Get("state"); got == "" || got != st.State {
		s.writeError(w, r, apperr.Invalid("this sign-in was not started from this browser; start again"))
		return
	}
	if e := q.Get("error"); e != "" {
		s.writeError(w, r, apperr.New(apperr.Unauthenticated, "the identity provider refused the sign-in (%s)", e))
		return
	}
	// No sign-in limit here: the signed, single-use state cookie and the
	// provider's single-use code already bind this to one sign-in that this
	// browser started, and the exchange is one call to the provider, not an
	// argon2 hash. A limit would only turn a lecture hall returning from the
	// provider at once into a queue of burnt sign-ins.
	id, err := s.SSO.Exchange(r.Context(), q.Get("code"), st.Nonce)
	if err != nil {
		s.Log.Warn("sso exchange failed", "err", err)
		s.writeError(w, r, apperr.New(apperr.Unauthenticated, "the identity provider's answer could not be verified"))
		return
	}
	sess, err := s.Auth.SignInWithIdentity(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	noteActor(r, sess.ActorID.String())
	http.SetCookie(w, s.sessionCookie(sess.Token, sess.ExpiresAt))
	http.Redirect(w, r, st.ReturnTo, http.StatusFound)
}

// maxReturn bounds return_to as it is written into the state cookie, escaped
// for JSON, where each '<', '>' or '&' takes six bytes; the cookie then goes
// out in base64. A browser keeps a cookie of about four kilobytes at most: a
// longer one would never come back to finish the sign-in, and would only
// have made the answer to anyone who asks, before they are signed in,
// several times the size of the question.
const maxReturn = 2 << 10

// safeReturn decides where the browser goes after signing in. Only two kinds
// of place are allowed: a path on this server, or a URL on one of the front
// end's own origins. Anything else — and an open redirect is a phishing tool
// with this site's name on it — becomes the default, as does a place longer
// than maxReturn.
func (s *server) safeReturn(want string) string {
	def := "/"
	if len(s.TrustedOrigins) > 0 {
		def = s.TrustedOrigins[0] + "/"
	}
	if want == "" || len(want) > maxReturn {
		return def
	}
	if escaped, _ := json.Marshal(want); len(escaped)-len(`""`) > maxReturn {
		return def
	}
	u, err := url.Parse(want)
	if err != nil {
		return def
	}
	if u.Scheme == "" && u.Host == "" {
		// "/path" is ours. "//host/path" is not a path at all, and nor is
		// "/\host", which a browser reads as "//host". A backslash is refused
		// wherever it is: http.Redirect cleans the path before it sends it,
		// so "/./\host" or "/x/../\host" would go out as "/\host". With no
		// backslash, and no control character (url.Parse refuses those),
		// nothing the cleaning does can make two slashes of the start.
		if strings.HasPrefix(want, "/") && !strings.HasPrefix(want, "//") && !strings.Contains(want, `\`) {
			return want
		}
		return def
	}
	for _, o := range s.TrustedOrigins {
		if u.Scheme+"://"+u.Host == o {
			return want
		}
	}
	return def
}
