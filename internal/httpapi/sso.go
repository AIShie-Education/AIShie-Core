package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Single sign-on, from the browser's side:
//
//	GET /v1/auth/sso/start?provider=…&return_to=…  → 302 to the identity provider
//	GET /v1/auth/sso/start/{provider}?return_to=…  → the same
//	GET /v1/auth/sso/callback?code&state           → session cookie, 302 to return_to
//
// A sign-in goes through one provider (package sso): the operator's, or one
// of the site's that is switched on. Which is named by provider, in the
// query or the path; with none named, the one provider offered, if only one
// is, so that a front end from before there were several signs in as it
// did. Every provider sends the browser back to the one callback, and the
// state says which provider the sign-in went through.
//
// Between the two the browser holds a short-lived signed cookie carrying the
// state (so that the answer is to a question this browser asked: without it
// an attacker could finish their own sign-in in the victim's browser), the
// nonce (so that the token answers this sign-in and no other), the provider,
// and where to go afterwards.

const (
	ssoCookie     = "ais_sso"
	ssoPurpose    = "sso.state"
	ssoWindow     = 10 * time.Minute
	ssoStartPath  = "/v1/auth/sso/start"
	ssoReturnPath = sso.CallbackPath
)

// SSOCallbackPath is what to register with every identity provider, after
// the server's public URL.
const SSOCallbackPath = ssoReturnPath

// SSOStartPath is where a browser starts a sign-in through the provider id:
// the path the sign-in page is told (GET /v1/auth/methods), to which it adds
// return_to.
func SSOStartPath(id string) string { return ssoStartPath + "/" + url.PathEscape(id) }

type ssoState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Provider string `json:"p,omitempty"`
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

var errProviderRequired = apperr.Invalid("more than one identity provider is offered here: say which to sign in through (provider), "+
	"as GET /v1/auth/methods lists them").With("reason", "provider_required")

func (s *server) ssoStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider")
	if q := r.URL.Query().Get("provider"); q != "" {
		if id != "" && id != q {
			s.writeError(w, r, apperr.Invalid("provider in the query is not the one in the path; they must agree"))
			return
		}
		id = q
	}
	if id == "" {
		offered, err := s.SSO.Offered(r.Context())
		switch {
		case err != nil:
			s.writeError(w, r, err)
			return
		case len(offered) == 0:
			s.writeError(w, r, sso.ErrNotOffered)
			return
		case len(offered) > 1:
			s.writeError(w, r, errProviderRequired)
			return
		}
		id = offered[0].ID
	}
	p, err := s.SSO.Resolve(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	st := ssoState{State: random(), Nonce: random(), Provider: p.ID, ReturnTo: s.safeReturn(r.URL.Query().Get("return_to")),
		Expires: time.Now().Add(ssoWindow).Unix()}
	// Lax, not Strict: the provider sends the browser back here by a
	// top-level navigation from another site, and the cookie must come too.
	http.SetCookie(w, s.cookie(ssoCookie, s.Signer.Sign(ssoPurpose, st), ssoReturnPath, time.Now().Add(ssoWindow)))
	http.Redirect(w, r, p.IdP.AuthCodeURL(st.State, st.Nonce), http.StatusFound)
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
	// A sign-in started before the state named its provider went through
	// the operator's, the one there was.
	if st.Provider == "" {
		if op := s.SSO.Operator(); op != nil {
			st.Provider = op.ID
		}
	}
	// The provider as it is now: switched off meanwhile, it signs nobody in.
	p, err := s.SSO.Resolve(r.Context(), st.Provider)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// No sign-in limit here: the signed, single-use state cookie and the
	// provider's single-use code already bind this to one sign-in that this
	// browser started, and the exchange is one call to the provider, not an
	// argon2 hash. A limit would only turn a lecture hall returning from the
	// provider at once into a queue of burnt sign-ins.
	id, err := p.IdP.Exchange(r.Context(), q.Get("code"), st.Nonce)
	if err != nil {
		s.Log.Warn("sso exchange failed", "provider", p.ID, "err", err)
		s.writeError(w, r, apperr.New(apperr.Unauthenticated, "the identity provider's answer could not be verified"))
		return
	}
	sess, err := s.Auth.SignInWithIdentity(r.Context(), id)
	if errors.Is(err, auth.ErrNotRegistered) && p.LinkByEmail && s.linkByEmail(r.Context(), p, id, st.State) {
		sess, err = s.Auth.SignInWithIdentity(r.Context(), id)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	noteActor(r, sess.ActorID.String())
	http.SetCookie(w, s.sessionCookie(sess.Token, sess.ExpiresAt))
	http.Redirect(w, r, st.ReturnTo, http.StatusFound)
}

// linkByEmail links an identity linked to nobody, at a provider that links
// by email, to the person whose email the provider vouches for, when that
// email is of one of its domains: sso.link_by_email, made through the
// pipeline as theirs, so that it is held to its rules again, in its
// transaction, and recorded. Whether it was is all it says: a sign-in it
// does not link is answered as any sign-in by someone not registered, and
// the log says why.
func (s *server) linkByEmail(ctx context.Context, p sso.Resolved, id auth.Identity, state string) bool {
	if id.Email == "" || !sso.InDomains(id.Email, p.AllowedEmailDomains) {
		return false
	}
	who, err := dbq.New(s.Pool).GetSSOLinkByEmailCandidate(ctx, id.Email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.Log.Error("sso link by email: finding the person", "provider", p.ID, "err", err)
		}
		return false
	}
	if who.Kind != "human" || who.Status != "active" {
		return false
	}
	args, err := json.Marshal(tools.SSOLinkByEmailIn{ProviderID: p.ID, Subject: id.Subject, Email: id.Email})
	if err != nil {
		return false
	}
	out, err := s.Pipeline.InvokeUnlisted(ctx, pipeline.Caller{ActorID: who.ID}, tools.ToolSSOLinkByEmail, args,
		"sso.link_by_email:"+state)
	switch {
	case err != nil:
		s.Log.Warn("sso link by email", "provider", p.ID, "actor", who.ID.String(), "err", err)
		return false
	case out.Status != domain.StatusExecuted:
		why := string(out.Status)
		if out.Error != nil {
			why = out.Error.Message
		}
		s.Log.Info("sso: not linked by email", "provider", p.ID, "actor", who.ID.String(), "why", strings.TrimPrefix(why, "that identity is not linked by email here: "))
		return false
	}
	return true
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
