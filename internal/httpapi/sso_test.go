package httpapi_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
	"github.com/AIShie-Education/AIShie-Core/internal/signing"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// fakeIdP is an OpenID Connect provider, as much of one as a relying party
// can tell: discovery, keys, and a token endpoint that redeems each code once.
// What goes into the id_token is up to the test, which is the point — a real
// provider cannot be asked to misbehave.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]map[string]any // code → claims
	signer *rsa.PrivateKey           // what tokens are actually signed with
}

const (
	clientID     = "aishie"
	clientSecret = "s3cret-for-the-token-endpoint"
)

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{t: t, key: key, signer: key, codes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	b64 := base64.RawURLEncoding.EncodeToString
	mux.HandleFunc("GET /adfs/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.issuer(), "authorization_endpoint": p.issuer() + "/oauth2/authorize",
			"token_endpoint": p.issuer() + "/oauth2/token", "jwks_uri": p.issuer() + "/discovery/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /adfs/discovery/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /adfs/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, basic := r.BasicAuth()
		if !basic {
			id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
		}
		p.mu.Lock()
		claims, ok := p.codes[r.PostFormValue("code")]
		delete(p.codes, r.PostFormValue("code")) // a code is good once
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if id != clientID || secret != clientSecret || !ok || r.PostFormValue("grant_type") != "authorization_code" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque", "token_type": "Bearer", "expires_in": 3600, "id_token": p.sign(claims)})
	})
	return p
}

func (p *fakeIdP) issuer() string { return p.srv.URL + "/adfs" }

func (p *fakeIdP) sign(claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(claims)
	signed := b64(header) + "." + b64(body)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.signer, crypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

// grant is the user signing in at the provider: it returns the code the
// browser is sent back with. change edits the claims first.
func (p *fakeIdP) grant(upn, nonce string, change func(map[string]any)) string {
	claims := map[string]any{"iss": p.issuer(), "aud": clientID, "sub": "opaque-" + uuid.NewString(), "upn": upn, "nonce": nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	if change != nil {
		change(claims)
	}
	code := uuid.NewString()
	p.mu.Lock()
	p.codes[code] = claims
	p.mu.Unlock()
	return code
}

type ssoAPI struct {
	*api
	idp *fakeIdP
	// op is the operator's provider, school-adfs, at idp; nil in a server
	// with none (newSite).
	op *sso.Operator
	// keys seal the site's providers' client secrets; nil in a server with
	// no SECRETS_KEY.
	keys *secrets.Keyring
	// registry is the server's, and log what it says.
	registry *sso.Registry
	log      syncBuffer
}

func newSSO(t *testing.T) *ssoAPI {
	t.Helper()
	return newSSOWith(t, ratelimit.New(0, 0), nil)
}

// newSSOWith is newSSO with a sign-in limit, and the Deps adjusted first.
func newSSOWith(t *testing.T, signIns *ratelimit.Limiter, adjust func(*httpapi.Deps)) *ssoAPI {
	t.Helper()
	return newSSOServer(t, ssoSetup{operator: true, keys: true, signIns: signIns, adjust: adjust})
}

// ssoSetup is a server's single sign-on: the operator's provider or none,
// a secrets key or none.
type ssoSetup struct {
	operator, keys bool
	// publicOnly holds the server's sign-ins through the site's providers
	// to public addresses, as a server is held without
	// SSO_ALLOW_PRIVATE_ISSUERS; its tools still set up a provider on this
	// machine, where every fake one is, as such a server's were before.
	publicOnly bool
	signIns    *ratelimit.Limiter
	adjust     func(*httpapi.Deps)
}

func testKeys(t *testing.T) *secrets.Keyring {
	t.Helper()
	k := make([]byte, secrets.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func newSSOServer(t *testing.T, set ssoSetup) *ssoAPI {
	t.Helper()
	idp := newFakeIdP(t)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signing.New("")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := &ssoAPI{idp: idp}
	if set.keys {
		a.keys = testKeys(t)
	}
	if set.operator {
		provider, err := auth.NewOIDC(context.Background(), auth.OIDCConfig{Name: "school-adfs", Issuer: idp.issuer(), ClientID: clientID,
			ClientSecret: clientSecret, SubjectClaim: "upn", RedirectURL: srv.URL + httpapi.SSOCallbackPath})
		if err != nil {
			t.Fatalf("discovery: %v", err)
		}
		a.op = &sso.Operator{ID: "school-adfs", Issuer: idp.issuer(), ClientID: clientID, SecretHint: secrets.Hint(clientSecret),
			Scopes: sso.DefaultScopes, SubjectClaim: "upn", IdP: provider}
	}
	// The tools and the server share the operator's provider and the keys,
	// as serve's one registry has them.
	c := testkit.NewCS101WithDeps(t, 0, func(d *tools.Deps) {
		d.SSO = sso.New(sso.Config{Operator: a.op, Keys: a.keys, PublicURL: srv.URL, PrivateIssuers: true})
	})
	a.registry = sso.New(sso.Config{Pool: c.Pool, Operator: a.op, Keys: a.keys, PublicURL: srv.URL, PrivateIssuers: !set.publicOnly,
		Log: slog.New(slog.NewTextHandler(&a.log, nil))})
	deps := httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: auth.NewAuthenticator(c.Pool, time.Hour),
		TrustedOrigins: []string{frontEnd}, InsecureCookies: true, SSO: a.registry, Signer: signer,
		SignIns: set.signIns,
	}
	if set.adjust != nil {
		set.adjust(&deps)
	}
	mux.Handle("/", httpapi.NewHandler(deps))
	a.api = &api{t: t, c: c, srv: srv}
	return a
}

func (a *ssoAPI) link(actor uuid.UUID, upn string) {
	a.t.Helper()
	if out := a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": actor, "provider": "school-adfs", "subject": upn}, "link-"+upn); out.Error != nil {
		a.t.Fatalf("link %s: %+v", upn, out)
	}
}

// browser is a client that remembers cookies and shows its redirects rather
// than following them.
func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// start begins a sign-in and returns what the provider was asked.
func (a *ssoAPI) start(b *http.Client, returnTo string) url.Values {
	a.t.Helper()
	path := "/v1/auth/sso/start"
	if returnTo != "" {
		path += "?return_to=" + url.QueryEscape(returnTo)
	}
	r := a.do(b, "GET", path, "", nil)
	if r.Status != http.StatusFound {
		a.t.Fatalf("start: %d %s", r.Status, r.Raw)
	}
	to := mustURL(a.t, r.Header.Get("Location"))
	if !strings.HasPrefix(to.String(), a.idp.issuer()+"/oauth2/authorize?") {
		a.t.Fatalf("start sent the browser to %s", to)
	}
	q := to.Query()
	if q.Get("client_id") != clientID || q.Get("redirect_uri") != a.srv.URL+httpapi.SSOCallbackPath || q.Get("response_type") != "code" ||
		len(q.Get("state")) < 40 || len(q.Get("nonce")) < 40 || q.Get("state") == q.Get("nonce") {
		a.t.Fatalf("authorize request: %v", q)
	}
	return q
}

func (a *ssoAPI) callback(b *http.Client, code, state string) response {
	a.t.Helper()
	return a.do(b, "GET", httpapi.SSOCallbackPath+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), "", nil)
}

func hasSession(r response) bool {
	for _, c := range r.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, httpapi.SessionCookie+"=") && !strings.HasPrefix(c, httpapi.SessionCookie+"=;") {
			return true
		}
	}
	return false
}

func TestSingleSignOn(t *testing.T) {
	a := newSSO(t)
	// Linked as an administrator would type it; the provider capitalises it
	// its own way.
	a.link(a.c.Sato, "sato@campus.example.edu")

	b := browser()
	q := a.start(b, "/courses?tab=mine")
	done := a.callback(b, a.idp.grant("Sato@Campus.example.edu", q.Get("nonce"), nil), q.Get("state"))
	if done.Status != http.StatusFound || done.Header.Get("Location") != "/courses?tab=mine" || !hasSession(done) {
		t.Fatalf("callback: %d %v %s", done.Status, done.Header, done.Raw)
	}
	for _, c := range done.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, httpapi.SessionCookie+"=") && (!strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Lax")) {
			t.Fatalf("session cookie: %q", c)
		}
	}
	if me := a.do(b, "GET", "/v1/me", "", nil); me.Status != 200 || me.str("result", "display_name") != "Sato" {
		t.Fatalf("me, after signing in: %d %s", me.Status, me.Raw)
	}
	// The session is a credential like any other: listed, and revocable.
	if n := a.c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'session' AND label = 'sso: school-adfs' AND revoked_at IS NULL`, a.c.Sato); n != 1 {
		t.Fatalf("%d sessions recorded", n)
	}

	// The state is good once. Replaying the provider's answer, even in the
	// same browser, starts nothing: the cookie went with the first answer.
	if again := a.callback(b, a.idp.grant("sato@campus.example.edu", q.Get("nonce"), nil), q.Get("state")); again.Status != http.StatusBadRequest || hasSession(again) {
		t.Fatalf("a replayed callback: %d %s", again.Status, again.Raw)
	}
}

// Nobody is created by signing in. The provider vouching for someone says who
// they are, not that they belong here.
func TestSingleSignOnCreatesNobody(t *testing.T) {
	a := newSSO(t)
	actors := a.c.Count(`SELECT count(*) FROM actor`)
	signIn := func(upn string) response {
		b := browser()
		q := a.start(b, "")
		return a.callback(b, a.idp.grant(upn, q.Get("nonce"), nil), q.Get("state"))
	}
	refused := func(what string, r response) {
		t.Helper()
		if r.Status != http.StatusForbidden || hasSession(r) || !strings.Contains(r.Raw, "not registered") {
			t.Fatalf("%s: %d %s", what, r.Status, r.Raw)
		}
	}
	refused("a stranger", signIn("stranger@campus.example.edu"))

	a.link(a.c.Sato, "sato@campus.example.edu")
	if r := signIn("sato@campus.example.edu"); r.Status != http.StatusFound {
		t.Fatalf("Sato: %d %s", r.Status, r.Raw)
	}
	// Suspended, the answer is the same as for a stranger.
	a.c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, a.c.Sato)
	refused("a suspended actor", signIn("sato@campus.example.edu"))
	a.c.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, a.c.Sato)

	// Unlinked — Sato revokes the credential — likewise.
	var cred uuid.UUID
	if err := a.c.Pool.QueryRow(t.Context(), `SELECT id FROM credential WHERE kind = 'sso' AND actor_id = $1`, a.c.Sato).Scan(&cred); err != nil {
		t.Fatal(err)
	}
	a.c.MustCall(a.c.Sato, "credential.revoke", m{"credential_id": cred}, "unlink")
	refused("an unlinked identity", signIn("sato@campus.example.edu"))

	// An identity that once opened one account never comes to open another.
	if out := a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": a.c.Grader, "provider": "school-adfs", "subject": "SATO@campus.example.edu"}, "steal"); out.Error == nil {
		t.Fatalf("Sato's old identity was linked to someone else: %+v", out)
	}
	// Linking it to Sato again brings it back.
	a.link(a.c.Sato, "Sato@campus.example.edu")
	if r := signIn("sato@campus.example.edu"); r.Status != http.StatusFound {
		t.Fatalf("Sato, linked again: %d %s", r.Status, r.Raw)
	}
	if out := a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": a.c.Sato, "provider": "school-adfs", "subject": "sato@campus.example.edu"}, "twice"); out.Error == nil {
		t.Fatal("linked twice")
	}
	if n := a.c.Count(`SELECT count(*) FROM actor`); n != actors {
		t.Fatalf("signing in made %d actors", n-actors)
	}
}

// Everything the protocol says to check, checked: each of these is an answer
// a real provider would never give and an attacker would.
func TestSingleSignOnRejectsWhatItCannotVerify(t *testing.T) {
	a := newSSO(t)
	a.link(a.c.Sato, "sato@campus.example.edu")
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		claims func(map[string]any)
		forge  bool
	}{
		"a token answering some other sign-in": {claims: func(c map[string]any) { c["nonce"] = "somebody-else's" }},
		"a token with no nonce":                {claims: func(c map[string]any) { delete(c, "nonce") }},
		"a token for another client":           {claims: func(c map[string]any) { c["aud"] = "some-other-app" }},
		"a token from another issuer":          {claims: func(c map[string]any) { c["iss"] = "https://evil.example/adfs" }},
		"an expired token":                     {claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		"a token with no upn":                  {claims: func(c map[string]any) { delete(c, "upn") }},
		"a token signed with someone's key":    {forge: true},
	} {
		b := browser()
		q := a.start(b, "")
		a.idp.signer = a.idp.key
		if tc.forge {
			a.idp.signer = other
		}
		r := a.callback(b, a.idp.grant("sato@campus.example.edu", q.Get("nonce"), tc.claims), q.Get("state"))
		if r.Status != http.StatusUnauthorized || hasSession(r) {
			t.Fatalf("%s: %d %s", name, r.Status, r.Raw)
		}
		// Why is for the log, not for whoever is on the other end.
		if strings.Contains(r.Raw, "nonce") || strings.Contains(r.Raw, "signature") || strings.Contains(r.Raw, "audience") {
			t.Fatalf("%s: the answer explains itself: %s", name, r.Raw)
		}
	}
	a.idp.signer = a.idp.key

	// The answer must be to a question this browser asked. Otherwise an
	// attacker finishes their own sign-in in the victim's browser, and the
	// victim works — and uploads — as the attacker.
	attacker, victim := browser(), browser()
	q := a.start(attacker, "")
	code := a.idp.grant("sato@campus.example.edu", q.Get("nonce"), nil)
	if r := a.callback(victim, code, q.Get("state")); r.Status != http.StatusBadRequest || hasSession(r) {
		t.Fatalf("a callback in a browser that never started: %d %s", r.Status, r.Raw)
	}
	vq := a.start(victim, "")
	if r := a.callback(victim, code, q.Get("state")); r.Status != http.StatusBadRequest || hasSession(r) {
		t.Fatalf("a callback carrying another browser's state: %d %s", r.Status, r.Raw)
	}
	_ = vq
	// A code the provider never issued.
	b := browser()
	q = a.start(b, "")
	if r := a.callback(b, "made-up", q.Get("state")); r.Status != http.StatusUnauthorized || hasSession(r) {
		t.Fatalf("a made-up code: %d %s", r.Status, r.Raw)
	}
	// The provider saying no.
	b = browser()
	q = a.start(b, "")
	if r := a.do(b, "GET", httpapi.SSOCallbackPath+"?error=access_denied&state="+q.Get("state"), "", nil); r.Status != http.StatusUnauthorized || hasSession(r) {
		t.Fatalf("access_denied: %d %s", r.Status, r.Raw)
	}
}

// return_to is followed after signing in, with this site's name on the
// redirect. It goes to this server or to the front end, and nowhere else;
// and it is no longer than a cookie can carry.
func TestSingleSignOnDoesNotRedirectElsewhere(t *testing.T) {
	a := newSSO(t)
	a.link(a.c.Sato, "sato@campus.example.edu")
	long := "/courses/1?q=" + strings.Repeat("a", 2<<10-len("/courses/1?q="))
	for want, tries := range map[string][]string{
		frontEnd + "/": {"", "https://evil.example/", "//evil.example/x", `/\evil.example`, `\\evil.example`, "https://lms.example.edu.evil.example/",
			"https://lms.example.edu@evil.example/", "http://lms.example.edu/", "javascript:alert(1)", "evil.example", "/\t/evil.example", "https:evil.example",
			long + "a",
			// Short, but six times as long escaped in the cookie.
			"/courses/1?q=" + strings.Repeat("<", 400),
			// Paths that http.Redirect cleans into /\evil.example.
			`/./\evil.example`, `/x/../\evil.example`, `/x#/../\evil.example`},
		"/courses/1?tab=grades":            {"/courses/1?tab=grades"},
		frontEnd + "/courses/1#submission": {frontEnd + "/courses/1#submission"},
		long:                               {long},
	} {
		for _, returnTo := range tries {
			b := browser()
			q := a.start(b, returnTo)
			r := a.callback(b, a.idp.grant("sato@campus.example.edu", q.Get("nonce"), nil), q.Get("state"))
			if r.Status != http.StatusFound || r.Header.Get("Location") != want {
				t.Fatalf("return_to %q: %d → %q, want %q", returnTo, r.Status, r.Header.Get("Location"), want)
			}
		}
	}
}

// Without single sign-on configured, its routes are not there.
func TestSingleSignOnIsOffByDefault(t *testing.T) {
	a := newAPI(t, 0)
	if r := a.do(browser(), "GET", "/v1/auth/sso/start", "", nil); r.Status != http.StatusNotFound {
		t.Fatalf("start with no provider configured: %d %s", r.Status, r.Raw)
	}
}

// Forty students in a lecture hall click "sign in" within the same minute,
// and all forty come back from the provider through the one reverse proxy.
// The callback is bound by the state cookie and the provider's single-use
// code, not by a per-address limit that would turn the hall into a queue of
// burnt sign-ins.
func TestALectureHallSignsInAtOnce(t *testing.T) {
	a := newSSOWith(t, ratelimit.New(10, 10), nil) // as serve builds it
	a.link(a.c.Sato, "sato@campus.example.edu")
	for i := range 40 {
		b := browser()
		q := a.start(b, "")
		if r := a.callback(b, a.idp.grant("sato@campus.example.edu", q.Get("nonce"), nil), q.Get("state")); r.Status != http.StatusFound || !hasSession(r) {
			t.Fatalf("student %d: %d %s", i+1, r.Status, r.Raw)
		}
	}
}
