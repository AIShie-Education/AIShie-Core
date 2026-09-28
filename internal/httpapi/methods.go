package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"time"
)

// The front end's sign-in page asks this server how a person may sign in,
// before it offers anything:
//
//	GET /v1/auth/methods → {"password": true, "sso": null}
//	                     → {"password": true, "sso": {"label": "PolyU NetID", "start": "/v1/auth/sso/start"}}
//
// One web image serves every installation, so whether it shows a single
// sign-on button, and what the button says, is this server's to say, not the
// image's. Single sign-on is on exactly when OIDC_ISSUER is set. "label" is
// OIDC_DISPLAY_NAME, or null when it is not set, and the front end then uses
// words of its own. "start" is the path on this server where a browser begins
// a sign-in; the front end adds return_to, and this server's origin when it
// is on another one. "password" is always true, since password sign-in cannot
// be turned off; it is there so that the answer can say otherwise one day.
//
// Nothing else about the provider is said: not its issuer, its client id,
// its scopes or its secret. The answer is the same for everyone and changes
// only when the server restarts with other settings, so anyone may ask, with
// no credential, and a browser or a cache may keep the answer for a minute.
// It has no rate limit, as /v1/auth/keys and /v1/auth/sso/start have none:
// it costs no hash and no query, and a lecture hall opening the sign-in page
// at once must not spend the sign-in attempts its address is allowed.

const (
	MethodsPath = "/v1/auth/methods"

	// methodsMaxAge is how long a browser or a cache may keep the answer: a
	// change of settings reaches the sign-in page within it.
	methodsMaxAge = time.Minute
)

type methodsOut struct {
	Password bool       `json:"password"`
	SSO      *ssoMethod `json:"sso"`
}

type ssoMethod struct {
	Label *string `json:"label"`
	Start string  `json:"start"`
}

// methods says how a person may sign in here.
//
// The answer says Vary: Origin whatever the request's origin, not only when
// cors adds it for a trusted one. A cache may keep it, and one kept for a
// request from nowhere in particular, without CORS headers, would otherwise
// be given to the front end, and its browser would keep it from the page.
func (s *server) methods(w http.ResponseWriter, _ *http.Request) {
	out := methodsOut{Password: true}
	if s.SSO != nil {
		out.SSO = &ssoMethod{Start: ssoStartPath}
		if s.SSOLabel != "" {
			out.SSO.Label = &s.SSOLabel
		}
	}
	h := w.Header()
	if !slices.Contains(h.Values("Vary"), "Origin") {
		h.Add("Vary", "Origin")
	}
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(methodsMaxAge.Seconds())))
	writeJSON(w, http.StatusOK, out)
}
