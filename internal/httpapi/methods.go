package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/sso"
)

// The front end's sign-in page asks this server how a person may sign in,
// before it offers anything:
//
//	GET /v1/auth/methods → {"password": true, "password_accepts": ["login_id", "email"],
//	                        "sso": null, "sso_providers": []}
//	                     → {"password": true, "password_accepts": ["login_id", "email"],
//	                        "sso": {"label": "School NetID", "start": "/v1/auth/sso/start"},
//	                        "sso_providers": [{"id": "school-adfs", "label": "School NetID",
//	                                           "start": "/v1/auth/sso/start/school-adfs"}]}
//
// One web image serves every installation, so whether it shows single
// sign-on buttons, and what each says, is this server's to say, not the
// image's. "sso_providers" are the identity providers a sign-in may go
// through now (package sso), in the order the page shows them: the one the
// server's operator sets (OIDC_ISSUER), then those the site's
// administrators set up and switched on, by position. Each has its id, its
// label — the operator's OIDC_DISPLAY_NAME, or null for the front end's own
// words; a site's provider's display name — and "start", the path on this
// server where a browser begins a sign-in through it, to which the front
// end adds return_to, and this server's origin when it is on another one.
//
// "sso" is the first of them, for a front end from before there were
// several, which shows one button: null when there is none; its "start" is
// /v1/auth/sso/start when it is the only one, as it always was, and its own
// path otherwise. "password" is always true, since password sign-in cannot
// be turned off; it is there so that the answer can say otherwise one day.
// "password_accepts" is what password sign-in takes for whose account it is,
// in the order the sign-in field's label should name them: a login ID — a
// student or staff number — and an email; POST /v1/auth/login takes either
// in "login", and tells them apart by the @ an email has.
//
// Nothing else about a provider is said: not its issuer, its client id, its
// scopes or its secret. The answer is the same for everyone, so anyone may
// ask, with no credential, and a browser or a cache may keep it for a
// minute: a provider switched on or off reaches the sign-in page within it.
// It has no rate limit, as /v1/auth/keys and /v1/auth/sso/start have none:
// it costs no hash, one small query, and a lecture hall opening the sign-in
// page at once must not spend the sign-in attempts its address is allowed.

const (
	MethodsPath = "/v1/auth/methods"

	// methodsMaxAge is how long a browser or a cache may keep the answer: a
	// change of settings reaches the sign-in page within it.
	methodsMaxAge = time.Minute
)

type methodsOut struct {
	Password        bool          `json:"password"`
	PasswordAccepts []string      `json:"password_accepts"`
	SSO             *ssoMethod    `json:"sso"`
	SSOProviders    []ssoProvider `json:"sso_providers"`
}

// passwordAccepts is what a password sign-in takes as the account's name.
var passwordAccepts = []string{"login_id", "email"}

type ssoMethod struct {
	Label *string `json:"label"`
	Start string  `json:"start"`
}

type ssoProvider struct {
	ID    string  `json:"id"`
	Label *string `json:"label"`
	Start string  `json:"start"`
}

// methods says how a person may sign in here.
//
// The answer says Vary: Origin whatever the request's origin, not only when
// cors adds it for a trusted one. A cache may keep it, and one kept for a
// request from nowhere in particular, without CORS headers, would otherwise
// be given to the front end, and its browser would keep it from the page.
func (s *server) methods(w http.ResponseWriter, r *http.Request) {
	out := methodsOut{Password: true, PasswordAccepts: passwordAccepts, SSOProviders: []ssoProvider{}}
	if s.SSO != nil {
		offered, err := s.SSO.Offered(r.Context())
		if err != nil {
			// The site's providers could not be read: the page is told of
			// the operator's, which needs no database, rather than of none.
			s.Log.Error("sign-in methods: the identity providers could not be read", "err", err)
			offered = nil
			if op := s.SSO.Operator(); op != nil {
				o := sso.Offer{ID: op.ID}
				if op.DisplayName != "" {
					o.Label = &op.DisplayName
				}
				offered = []sso.Offer{o}
			}
		}
		for _, o := range offered {
			out.SSOProviders = append(out.SSOProviders, ssoProvider{ID: o.ID, Label: o.Label, Start: SSOStartPath(o.ID)})
		}
		if len(offered) > 0 {
			out.SSO = &ssoMethod{Label: offered[0].Label, Start: SSOStartPath(offered[0].ID)}
			if len(offered) == 1 {
				out.SSO.Start = ssoStartPath
			}
		}
	}
	h := w.Header()
	if !slices.Contains(h.Values("Vary"), "Origin") {
		h.Add("Vary", "Origin")
	}
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(methodsMaxAge.Seconds())))
	writeJSON(w, http.StatusOK, out)
}
