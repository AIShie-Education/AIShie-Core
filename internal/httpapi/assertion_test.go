package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ratelimit"
)

// A person signed in to Core is vouched for to a service that hosts agents
// (a runtime), which checks the assertion against the key Core publishes.

const (
	runtimeAudience = "https://lms.example.edu/runtime"
	publicURL       = "https://lms.example.edu"
	signingKey      = "an installation's signing key, 32+ characters long"
)

// asserting is the API as serve builds it, rate limit and log included,
// making assertions for audiences with the key derived from signingKey.
func asserting(t *testing.T, calls *ratelimit.Limiter, log *slog.Logger, audiences ...string) *api {
	t.Helper()
	return hardenedWith(t, calls, nil, log, func(d *httpapi.Deps) {
		key, err := auth.AssertionKey(nil, signingKey)
		if err != nil {
			t.Fatal(err)
		}
		d.Assertions, err = auth.NewAsserter(d.Pool, auth.AssertionConfig{Issuer: publicURL, Audiences: audiences, TTL: 5 * time.Minute, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		d.TrustedOrigins, d.InsecureCookies = []string{frontEnd}, true
	})
}

// signIn signs Sato in with a password, in a browser of its own, and
// returns the browser and the session's credential id.
func (a *api) signIn() (*http.Client, uuid.UUID) {
	a.t.Helper()
	c := a.c
	c.Exec(`UPDATE actor SET email = 'sato@example.edu' WHERE id = $1`, c.Sato)
	if err := auth.SetPassword(context.Background(), dbq.New(c.Pool), c.Sato, "a long enough password", time.Now()); err != nil {
		a.t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	if r := a.do(browser, "POST", "/v1/auth/login", "", m{"email": "sato@example.edu", "password": "a long enough password"}); r.Status != 200 {
		a.failed(r, "sign in")
	}
	var sid uuid.UUID
	if err := c.Pool.QueryRow(context.Background(), `SELECT id FROM credential WHERE actor_id = $1 AND kind = 'session' ORDER BY created_at DESC LIMIT 1`,
		c.Sato).Scan(&sid); err != nil {
		a.t.Fatal(err)
	}
	return browser, sid
}

// failed fails the test with what a response said.
func (a *api) failed(r response, what string) {
	a.t.Helper()
	a.t.Fatalf("%s: %d %s", what, r.Status, r.Raw)
}

// credentialOf is the id of the credential a token is.
func (a *api) credentialOf(token string) uuid.UUID {
	a.t.Helper()
	p, err := auth.NewAuthenticator(a.c.Pool, time.Hour).Authenticate(context.Background(), token)
	if err != nil {
		a.t.Fatal(err)
	}
	return p.CredentialID
}

// runtimeClaims are what a runtime reads from an assertion beyond what
// go-oidc checks itself.
type runtimeClaims struct {
	Kind         string  `json:"kind"`
	Name         string  `json:"name"`
	Email        *string `json:"email"`
	PlatformRole *string `json:"platform_role"`
	SessionID    string  `json:"sid"`
	IssuedAt     int64   `json:"iat"`
	Expires      int64   `json:"exp"`
	NotBefore    int64   `json:"nbf"`
	ID           string  `json:"jti"`
}

// check verifies an assertion as a runtime would: with go-oidc, which
// fetches the key set from this server's /v1/auth/keys and checks the
// signature, the issuer, the audience and the times, EdDSA and nothing else.
func (a *api) check(assertion string) (*oidc.IDToken, runtimeClaims) {
	a.t.Helper()
	ctx := context.Background()
	keys := oidc.NewRemoteKeySet(ctx, a.srv.URL+httpapi.KeysPath)
	v := oidc.NewVerifier(publicURL, keys, &oidc.Config{ClientID: runtimeAudience, SupportedSigningAlgs: []string{oidc.EdDSA}})
	tok, err := v.Verify(ctx, assertion)
	if err != nil {
		a.t.Fatalf("a runtime does not accept the assertion: %v", err)
	}
	var c runtimeClaims
	if err := tok.Claims(&c); err != nil {
		a.t.Fatal(err)
	}
	return tok, c
}

func TestAPersonIsVouchedForToARuntime(t *testing.T) {
	a := asserting(t, nil, nil, "http://localhost:9091/runtime", runtimeAudience)
	c := a.c

	// The key set is public, and may be kept for a few minutes.
	keys := a.do(nil, "GET", httpapi.KeysPath, "", nil)
	if keys.Status != 200 || keys.Header.Get("Cache-Control") != "public, max-age=300" || !strings.HasPrefix(keys.Header.Get("Content-Type"), "application/json") {
		a.failed(keys, "the key set")
	}
	list, _ := keys.Body["keys"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["kty"] != "OKP" || list[0].(map[string]any)["crv"] != "Ed25519" ||
		list[0].(map[string]any)["alg"] != "EdDSA" || list[0].(map[string]any)["use"] != "sig" || list[0].(map[string]any)["kid"] == "" {
		a.failed(keys, "the key set")
	}

	// In the browser, with the session cookie, from the front end.
	browser, sid := a.signIn()
	before := time.Now()
	r := a.do(browser, "POST", httpapi.AssertionPath, "", m{"audience": runtimeAudience}, "Origin", frontEnd, "Sec-Fetch-Site", "same-site")
	if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Access-Control-Allow-Origin") != frontEnd {
		a.failed(r, "an assertion for the signed-in person")
	}
	tok, claims := a.check(r.str("assertion"))
	if tok.Subject != c.Sato.String() || tok.Issuer != publicURL || len(tok.Audience) != 1 || tok.Audience[0] != runtimeAudience ||
		claims.Kind != "human" || claims.Name != "Sato" || claims.Email == nil || *claims.Email != "sato@example.edu" ||
		claims.PlatformRole != nil || claims.SessionID != sid.String() || claims.ID == "" ||
		claims.NotBefore != claims.IssuedAt || claims.Expires-claims.IssuedAt != 300 || claims.IssuedAt < before.Unix() {
		t.Fatalf("claims: %+v %+v", tok, claims)
	}
	if exp, err := time.Parse(time.RFC3339, r.str("expires_at")); err != nil || exp.Unix() != claims.Expires {
		t.Fatalf("expires_at %q, exp %d", r.str("expires_at"), claims.Expires)
	}
	// The one audience asked for, and no other.
	other := a.do(browser, "POST", httpapi.AssertionPath, "", m{"audience": "http://localhost:9091/runtime"}, "Origin", frontEnd)
	if other.Status != 200 {
		a.failed(other, "the other audience")
	}
	if _, err := oidc.NewVerifier(publicURL, oidc.NewRemoteKeySet(context.Background(), a.srv.URL+httpapi.KeysPath),
		&oidc.Config{ClientID: runtimeAudience, SupportedSigningAlgs: []string{oidc.EdDSA}}).Verify(context.Background(), other.str("assertion")); err == nil {
		t.Fatal("an assertion for another runtime is accepted by this one")
	}

	// It ends with the session: one that has ninety seconds left gives an
	// assertion that lasts ninety seconds, not five minutes.
	c.Exec(`UPDATE credential SET expires_at = now() + interval '90.5 seconds' WHERE id = $1`, sid)
	var sessionEnds time.Time
	if err := c.Pool.QueryRow(context.Background(), `SELECT expires_at FROM credential WHERE id = $1`, sid).Scan(&sessionEnds); err != nil {
		t.Fatal(err)
	}
	short := a.do(browser, "POST", httpapi.AssertionPath, "", m{"audience": runtimeAudience})
	if short.Status != 200 {
		a.failed(short, "near the session's end")
	}
	_, claims = a.check(short.str("assertion"))
	if claims.Expires != sessionEnds.Unix() || time.Unix(claims.Expires, 0).After(sessionEnds) {
		t.Fatalf("exp %d with the session ending at %s (%d)", claims.Expires, sessionEnds, sessionEnds.Unix())
	}

	// With a bearer token as well: a person's own, pasted into the front end.
	token := a.tokenFor(c.Sato)
	bearer := a.do(nil, "POST", httpapi.AssertionPath, token, m{"audience": runtimeAudience})
	if bearer.Status != 200 || bearer.Header.Get("Cache-Control") != "no-store" {
		a.failed(bearer, "an assertion by bearer token")
	}
	if tok, claims := a.check(bearer.str("assertion")); tok.Subject != c.Sato.String() || claims.SessionID != a.credentialOf(token).String() ||
		claims.Expires-claims.IssuedAt != 300 {
		t.Fatalf("by bearer token: %+v %+v", tok, claims)
	}

	// Neither route is a tool: the catalogue, and so MCP, knows nothing of them.
	for _, tl := range a.do(nil, "GET", "/v1/tools", "", nil).Body["tools"].([]any) {
		if p, _ := tl.(map[string]any)["path"].(string); strings.HasPrefix(p, "/v1/auth") {
			t.Errorf("the catalogue lists %s", p)
		}
	}
}

// Every way an assertion is refused, and that each refusal makes none.
func TestAnAssertionIsRefused(t *testing.T) {
	a := asserting(t, nil, nil, runtimeAudience)
	c := a.c
	browser, _ := a.signIn()
	sato, agent := a.tokenFor(c.Sato), a.tokenFor(c.Grader)
	gone := c.Actor("human", "Gone")
	goneToken := a.tokenFor(gone)
	c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, gone)
	ok := a.do(nil, "POST", httpapi.AssertionPath, sato, m{"audience": runtimeAudience})
	if ok.Status != 200 {
		a.failed(ok, "an assertion")
	}
	assertion := ok.str("assertion")

	for _, tc := range []struct {
		name    string
		client  *http.Client
		method  string
		token   string
		body    any
		headers []string
		status  int
		code    string
	}{
		{"no credential", nil, "POST", "", m{"audience": runtimeAudience}, nil, 401, "unauthenticated"},
		{"a credential that is none", nil, "POST", "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43), m{"audience": runtimeAudience}, nil, 401, "unauthenticated"},
		{"an assertion to ask for another", nil, "POST", assertion, m{"audience": runtimeAudience}, nil, 401, "unauthenticated"},
		{"an audience not listed", nil, "POST", sato, m{"audience": "https://evil.example/runtime"}, nil, 400, "invalid_argument"},
		{"a listed audience with a slash", nil, "POST", sato, m{"audience": runtimeAudience + "/"}, nil, 400, "invalid_argument"},
		{"no audience", nil, "POST", sato, m{}, nil, 400, "invalid_argument"},
		{"no body", nil, "POST", sato, nil, nil, 400, "invalid_argument"},
		{"a body that is not an object", nil, "POST", sato, []string{runtimeAudience}, nil, 400, "invalid_argument"},
		{"a claim of its own", nil, "POST", sato, m{"audience": runtimeAudience, "sub": uuid.NewString()}, nil, 400, "invalid_argument"},
		{"a suspended person", nil, "POST", goneToken, m{"audience": runtimeAudience}, nil, 403, "forbidden"},
		{"an agent's token", nil, "POST", agent, m{"audience": runtimeAudience}, nil, 403, "forbidden"},
		{"a hostile page riding the cookie", browser, "POST", "", m{"audience": runtimeAudience},
			[]string{"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"}, 403, "forbidden"},
		{"a hostile page on another site with no Origin", browser, "POST", "", m{"audience": runtimeAudience},
			[]string{"Sec-Fetch-Site", "cross-site"}, 403, "forbidden"},
		{"a GET", browser, "GET", "", nil, nil, 405, "method_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := a.do(tc.client, tc.method, httpapi.AssertionPath, tc.token, tc.body, tc.headers...)
			if _, made := r.Body["assertion"]; r.Status != tc.status || r.str("error", "code") != tc.code || made || strings.Contains(r.Raw, "eyJ") {
				t.Fatalf("%d %s, want %d %s", r.Status, r.Raw, tc.status, tc.code)
			}
		})
	}
	// Suspended since signing in: the session still authenticates, and is
	// given nothing.
	c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, c.Sato)
	if r := a.do(browser, "POST", httpapi.AssertionPath, "", m{"audience": runtimeAudience}, "Origin", frontEnd); r.Status != 403 || r.str("error", "code") != "forbidden" {
		a.failed(r, "suspended after signing in")
	}
}

// The route is behind the per-actor limit, like every call: a front end in
// a loop is stopped, and nobody else is.
func TestAssertionsAreHeldToTheRateLimit(t *testing.T) {
	now := time.Now()
	calls := ratelimit.New(60, 2)
	calls.SetClock(func() time.Time { return now })
	a := asserting(t, calls, nil, runtimeAudience)
	sato, yuki := a.tokenFor(a.c.Sato), a.tokenFor(a.c.Actor("human", "Yuki"))
	for i := range 2 {
		if r := a.do(nil, "POST", httpapi.AssertionPath, sato, m{"audience": runtimeAudience}); r.Status != 200 {
			a.failed(r, "assertion "+string(rune('1'+i)))
		}
	}
	r := a.do(nil, "POST", httpapi.AssertionPath, sato, m{"audience": runtimeAudience})
	if r.Status != http.StatusTooManyRequests || r.str("error", "code") != "rate_limited" || r.Header.Get("Retry-After") == "" || r.str("assertion") != "" {
		a.failed(r, "the third at once")
	}
	if r := a.do(nil, "POST", httpapi.AssertionPath, yuki, m{"audience": runtimeAudience}); r.Status != 200 {
		a.failed(r, "someone else")
	}
	now = now.Add(time.Second)
	if r := a.do(nil, "POST", httpapi.AssertionPath, sato, m{"audience": runtimeAudience}); r.Status != 200 {
		a.failed(r, "after waiting")
	}
}

// With no audience configured no assertion is made, and the key is still
// published; with no key at all, neither route answers.
func TestNoAudienceNoAssertion(t *testing.T) {
	a := asserting(t, nil, nil)
	sato := a.tokenFor(a.c.Sato)
	for _, token := range []string{"", sato} {
		if r := a.do(nil, "POST", httpapi.AssertionPath, token, m{"audience": runtimeAudience}); r.Status != 404 || r.str("error", "code") != "not_found" {
			a.failed(r, "no audience configured")
		}
	}
	if r := a.do(nil, "GET", httpapi.KeysPath, "", nil); r.Status != 200 || len(r.Body["keys"].([]any)) != 1 {
		a.failed(r, "the key set with no audience")
	}

	none := newAPI(t, 0) // no Assertions at all
	sato = none.tokenFor(none.c.Sato)
	if r := none.do(nil, "POST", httpapi.AssertionPath, sato, m{"audience": runtimeAudience}); r.Status != 404 || r.str("error", "code") != "not_found" {
		none.failed(r, "no key")
	}
	if r := none.do(nil, "GET", httpapi.KeysPath, "", nil); r.Status != 404 || r.str("error", "code") != "not_found" {
		none.failed(r, "the key set with no key")
	}
}

// An assertion is for a runtime. Core takes it nowhere: not as a bearer
// token, not as the session cookie, not over MCP.
func TestAnAssertionIsNoCredentialHere(t *testing.T) {
	a := asserting(t, nil, nil, runtimeAudience)
	r := a.do(nil, "POST", httpapi.AssertionPath, a.tokenFor(a.c.Sato), m{"audience": runtimeAudience})
	if r.Status != 200 {
		a.failed(r, "an assertion")
	}
	assertion := r.str("assertion")
	if me := a.do(nil, "GET", "/v1/me", assertion, nil); me.Status != 401 {
		a.failed(me, "the assertion as a bearer token")
	}
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(mustURL(t, a.srv.URL), []*http.Cookie{{Name: httpapi.SessionCookie, Value: assertion}})
	if me := a.do(&http.Client{Jar: jar}, "GET", "/v1/me", "", nil); me.Status != 401 {
		a.failed(me, "the assertion as the session cookie")
	}
	res, body := a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		"Authorization", "Bearer "+assertion, "Accept", "application/json, text/event-stream")
	if res.StatusCode != 401 {
		t.Fatalf("the assertion over MCP: %d %s", res.StatusCode, body)
	}
}

// The log says who asked and how it went, and never carries the assertion,
// any part of it, or the credential it was asked with.
func TestTheLogCarriesNoAssertion(t *testing.T) {
	var buf bytes.Buffer
	a := asserting(t, nil, slog.New(slog.NewJSONHandler(&buf, nil)), runtimeAudience)
	browser, _ := a.signIn()
	var session string
	for _, ck := range browser.Jar.Cookies(mustURL(t, a.srv.URL)) {
		if ck.Name == httpapi.SessionCookie {
			session = ck.Value
		}
	}
	token := a.tokenFor(a.c.Sato)
	var secrets []string
	for _, r := range []response{
		a.do(browser, "POST", httpapi.AssertionPath, "", m{"audience": runtimeAudience}, "Origin", frontEnd),
		a.do(nil, "POST", httpapi.AssertionPath, token, m{"audience": runtimeAudience}),
	} {
		if r.Status != 200 {
			a.failed(r, "an assertion")
		}
		secrets = append(secrets, r.str("assertion"))
		secrets = append(secrets, strings.Split(r.str("assertion"), ".")[1:]...)
	}
	a.do(nil, "POST", httpapi.AssertionPath, token, m{"audience": "https://evil.example/runtime"})
	a.do(nil, "GET", httpapi.KeysPath, "", nil)

	log := buf.String()
	for _, secret := range append(secrets, session, session[17:], token, token[17:]) {
		if secret == "" || strings.Contains(log, secret) {
			t.Fatalf("the log carries a secret, or the test has none to look for: %q\n%s", secret, log)
		}
	}
	for _, want := range []string{`"path":"` + httpapi.AssertionPath + `"`, `"actor":"` + a.c.Sato.String() + `"`, `"status":400`,
		`"path":"` + httpapi.KeysPath + `"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %s\n%s", want, log)
		}
	}
}
