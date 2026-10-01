package httpapi_test

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
)

// Identity providers the site's administrators set up from the front end
// (sso.*), signed in through as the operator's is.

// startAt begins a sign-in at path and insists the browser is sent to idp;
// it returns what idp was asked.
func (a *ssoAPI) startAt(b *http.Client, path string, idp *fakeIdP) url.Values {
	a.t.Helper()
	r := a.do(b, "GET", path, "", nil)
	if r.Status != http.StatusFound {
		a.t.Fatalf("start at %s: %d %s", path, r.Status, r.Raw)
	}
	to := mustURL(a.t, r.Header.Get("Location"))
	if !strings.HasPrefix(to.String(), idp.issuer()+"/oauth2/authorize?") {
		a.t.Fatalf("start at %s sent the browser to %s, not %s", path, to, idp.issuer())
	}
	q := to.Query()
	if q.Get("client_id") != clientID || q.Get("redirect_uri") != a.srv.URL+httpapi.SSOCallbackPath {
		a.t.Fatalf("authorize request: %v", q)
	}
	return q
}

// signInThrough signs in at path, with idp vouching for upn, and returns
// the callback's answer.
func (a *ssoAPI) signInThrough(path string, idp *fakeIdP, upn string, change func(map[string]any)) response {
	a.t.Helper()
	b := browser()
	q := a.startAt(b, path, idp)
	return a.callback(b, idp.grant(upn, q.Get("nonce"), change), q.Get("state"))
}

// provider is a provider's settings for sso.create, at idp: its client is
// the fake provider's, and an account is known by its UPN.
func provider(id, name string, idp *fakeIdP) m {
	return m{"id": id, "display_name": name, "issuer": idp.issuer(), "client_id": clientID, "client_secret": clientSecret,
		"subject_claim": "upn"}
}

// write calls a write tool over REST as who, with a fresh key.
func (a *ssoAPI) write(who, path string, body m, headers ...string) response {
	a.t.Helper()
	return a.do(nil, "POST", path, who, body, append([]string{"Idempotency-Key", uuid.NewString()}, headers...)...)
}

func (a *ssoAPI) methods() response {
	a.t.Helper()
	r := a.do(nil, "GET", httpapi.MethodsPath, "", nil)
	if r.Status != http.StatusOK {
		a.t.Fatalf("methods: %d %s", r.Status, r.Raw)
	}
	return r
}

// A provider an administrator sets up over REST, and switches on, signs in
// whoever is linked at it, on the server as it runs; the sign-in page is told
// of it, and nothing says its secret.
func TestAProviderSetUpFromTheFrontEndSignsPeopleIn(t *testing.T) {
	a := newSSOServer(t, ssoSetup{keys: true})
	root := a.tokenFor(a.c.Root)

	if r := a.methods(); r.Body["sso"] != nil || len(r.Body["sso_providers"].([]any)) != 0 {
		t.Fatalf("before: %s", r.Raw)
	}
	made := a.write(root, "/v1/sso/providers", provider("campus", "Campus ID", a.idp))
	if made.Status != http.StatusOK || made.str("result", "status") != "disabled" ||
		made.str("result", "redirect_uri") != a.srv.URL+httpapi.SSOCallbackPath {
		t.Fatalf("create: %d %s", made.Status, made.Raw)
	}
	// Switched off, it is offered nowhere and starts nothing.
	if r := a.methods(); r.Body["sso"] != nil {
		t.Fatalf("a provider switched off is offered: %s", r.Raw)
	}
	if r := a.do(browser(), "GET", "/v1/auth/sso/start/campus", "", nil); r.Status != http.StatusNotFound ||
		r.str("error", "details", "reason") != "sso_provider_not_found" {
		t.Fatalf("start through a provider switched off: %d %s", r.Status, r.Raw)
	}

	// Switched on over the version read, carried in If-Match.
	on := a.write(root, "/v1/sso/providers/campus/enabled", m{"enabled": true}, "If-Match", `"1"`)
	if on.Status != http.StatusOK || on.str("result", "status") != "offered" || on.Body["result"].(m)["version"] != float64(2) {
		t.Fatalf("switch on: %d %s", on.Status, on.Raw)
	}
	stale := a.write(root, "/v1/sso/providers/campus", m{"display_name": "Stale"}, "If-Match", `"1"`)
	if stale.Status != http.StatusConflict || stale.str("error", "details", "reason") != "version_mismatch" ||
		stale.Body["error"].(m)["details"].(m)["current_version"] != float64(2) {
		t.Fatalf("a stale If-Match: %d %s", stale.Status, stale.Raw)
	}
	if r := a.write(root, "/v1/sso/providers/campus", m{"version": 3, "display_name": "Two versions"}, "If-Match", "2"); r.Status != http.StatusBadRequest {
		t.Fatalf("If-Match and a body that disagree: %d %s", r.Status, r.Raw)
	}
	if r := a.write(root, "/v1/sso/providers/campus", m{"display_name": "Weak"}, "If-Match", `W/"2"`); r.Status != http.StatusBadRequest {
		t.Fatalf("a weak If-Match: %d %s", r.Status, r.Raw)
	}

	want := m{"password": true, "password_accepts": []any{"login_id", "email"}, "sso": m{"label": "Campus ID", "start": "/v1/auth/sso/start"},
		"sso_providers": []any{m{"id": "campus", "label": "Campus ID", "start": "/v1/auth/sso/start/campus"}}}
	if r := a.methods(); !reflect.DeepEqual(r.Body, want) {
		t.Fatalf("methods: %s", r.Raw)
	}

	if out := a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": a.c.Sato, "provider": "campus", "subject": "sato@campus.edu"}, "link"); out.Error != nil {
		t.Fatalf("link: %+v", out)
	}
	// The one provider offered is where a bare start goes, as a front end
	// from before there were several starts; and its own path, and the
	// query, go there too.
	for _, path := range []string{"/v1/auth/sso/start", "/v1/auth/sso/start/campus", "/v1/auth/sso/start?provider=campus"} {
		done := a.signInThrough(path+"", a.idp, "Sato@campus.edu", nil)
		if done.Status != http.StatusFound || !hasSession(done) {
			t.Fatalf("sign in at %s: %d %s", path, done.Status, done.Raw)
		}
	}
	if n := a.c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'session' AND label = 'sso: campus'`, a.c.Sato); n != 3 {
		t.Fatalf("%d sessions through campus", n)
	}
	if r := a.do(browser(), "GET", "/v1/auth/sso/start/campus?provider=other", "", nil); r.Status != http.StatusBadRequest {
		t.Fatalf("a path and a query naming two providers: %d %s", r.Status, r.Raw)
	}

	// Whatever is read, and whoever reads it, the secret is not said.
	for _, path := range []string{"/v1/sso/providers", "/v1/sso/providers/campus", "/v1/sso/test?provider_id=campus"} {
		r := a.do(nil, "GET", path, root, nil)
		if r.Status != http.StatusOK || strings.Contains(r.Raw, clientSecret) {
			t.Fatalf("GET %s: %d %s", path, r.Status, r.Raw)
		}
	}
	if r := a.do(nil, "GET", "/v1/sso/test?provider_id=campus", root, nil); r.Body["result"].(m)["ok"] != true {
		t.Fatalf("the provider's test: %s", r.Raw)
	}
	if n := a.c.Count(`SELECT count(*) FROM action WHERE payload::text LIKE '%' || $1 || '%' OR result::text LIKE '%' || $1 || '%'`, clientSecret); n != 0 {
		t.Fatalf("%d actions record the client secret", n)
	}
	// A person with no platform role reads nothing of it.
	if r := a.do(nil, "GET", "/v1/sso/providers", a.tokenFor(a.c.Sato), nil); r.Status != http.StatusForbidden ||
		r.str("error", "details", "reason") != "platform_role_required" {
		t.Fatalf("Sato lists the providers: %d %s", r.Status, r.Raw)
	}
}

// Two providers: the operator's and the site's, each at its own identity
// provider. The sign-in page is told of both, the operator's first; a bare
// start is refused, saying to choose; each sign-in goes through the provider
// it started with, which its state names, and an identity is linked at one
// provider only.
func TestTwoProvidersEachSignInTheirOwn(t *testing.T) {
	a := newSSOServer(t, ssoSetup{operator: true, keys: true, adjust: func(d *httpapi.Deps) { named(d, "PolyU NetID") }})
	other := newFakeIdP(t)
	root := a.tokenFor(a.c.Root)
	campus := provider("campus", "Campus ID", other)
	campus["enabled"] = true
	if r := a.write(root, "/v1/sso/providers", campus); r.Status != http.StatusOK || r.str("result", "status") != "offered" {
		t.Fatalf("create: %d %s", r.Status, r.Raw)
	}
	want := m{"password": true, "password_accepts": []any{"login_id", "email"},
		"sso": m{"label": "PolyU NetID", "start": "/v1/auth/sso/start/polyu-adfs"},
		"sso_providers": []any{
			m{"id": "polyu-adfs", "label": "PolyU NetID", "start": "/v1/auth/sso/start/polyu-adfs"},
			m{"id": "campus", "label": "Campus ID", "start": "/v1/auth/sso/start/campus"},
		}}
	if r := a.methods(); !reflect.DeepEqual(r.Body, want) {
		t.Fatalf("methods: %s", r.Raw)
	}
	if r := a.do(browser(), "GET", "/v1/auth/sso/start", "", nil); r.Status != http.StatusBadRequest ||
		r.str("error", "details", "reason") != "provider_required" {
		t.Fatalf("a bare start with two providers: %d %s", r.Status, r.Raw)
	}
	if r := a.do(browser(), "GET", "/v1/auth/sso/start/nobody", "", nil); r.Status != http.StatusNotFound {
		t.Fatalf("a start through no provider: %d %s", r.Status, r.Raw)
	}

	mei := a.c.Actor("human", "Mei")
	a.link(a.c.Sato, "sato@polyu.edu.hk")
	if out := a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": mei, "provider": "campus", "subject": "mei@campus.edu"}, "link-mei"); out.Error != nil {
		t.Fatalf("link: %+v", out)
	}
	for _, tc := range []struct {
		path  string
		idp   *fakeIdP
		upn   string
		actor uuid.UUID
		label string
	}{
		{"/v1/auth/sso/start/polyu-adfs", a.idp, "sato@polyu.edu.hk", a.c.Sato, "sso: polyu-adfs"},
		{"/v1/auth/sso/start?provider=campus", other, "mei@campus.edu", mei, "sso: campus"},
	} {
		done := a.signInThrough(tc.path, tc.idp, tc.upn, nil)
		if done.Status != http.StatusFound || !hasSession(done) {
			t.Fatalf("%s: %d %s", tc.path, done.Status, done.Raw)
		}
		if n := a.c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'session' AND label = $2`, tc.actor, tc.label); n != 1 {
			t.Fatalf("%s: %d sessions labelled %s", tc.path, n, tc.label)
		}
	}
	// The same subject at the other provider is somebody else: nobody.
	if r := a.signInThrough("/v1/auth/sso/start/campus", other, "sato@polyu.edu.hk", nil); r.Status != http.StatusForbidden || hasSession(r) {
		t.Fatalf("Sato's UPN through campus: %d %s", r.Status, r.Raw)
	}
	// The state names the provider the sign-in went through: a code the
	// other provider issued is not one this one redeems.
	b := browser()
	q := a.startAt(b, "/v1/auth/sso/start/campus", other)
	if r := a.callback(b, a.idp.grant("sato@polyu.edu.hk", q.Get("nonce"), nil), q.Get("state")); r.Status != http.StatusUnauthorized || hasSession(r) {
		t.Fatalf("the operator's provider's code at campus: %d %s", r.Status, r.Raw)
	}
	// A token signed by the other provider, for this sign-in, is not
	// verified either: another issuer, another key.
	b = browser()
	q = a.startAt(b, "/v1/auth/sso/start/campus", other)
	forged := other.grant("mei@campus.edu", q.Get("nonce"), func(c map[string]any) { c["iss"] = a.idp.issuer() })
	if r := a.callback(b, forged, q.Get("state")); r.Status != http.StatusUnauthorized || hasSession(r) {
		t.Fatalf("a token naming the other issuer: %d %s", r.Status, r.Raw)
	}
}

// A provider switched off signs nobody in, even one whose sign-in was under
// way; switched on again, everyone linked signs in as before.
func TestAProviderSwitchedOffSignsNobodyIn(t *testing.T) {
	a := newSSOServer(t, ssoSetup{keys: true})
	root := a.tokenFor(a.c.Root)
	campus := provider("campus", "Campus ID", a.idp)
	campus["enabled"] = true
	a.write(root, "/v1/sso/providers", campus)
	a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": a.c.Sato, "provider": "campus", "subject": "sato@campus.edu"}, "link")

	b := browser()
	q := a.startAt(b, "/v1/auth/sso/start/campus", a.idp)
	if r := a.write(root, "/v1/sso/providers/campus/enabled", m{"enabled": false}); r.Status != http.StatusOK {
		t.Fatalf("switch off: %d %s", r.Status, r.Raw)
	}
	if r := a.callback(b, a.idp.grant("sato@campus.edu", q.Get("nonce"), nil), q.Get("state")); r.Status != http.StatusNotFound || hasSession(r) {
		t.Fatalf("a sign-in under way when it was switched off: %d %s", r.Status, r.Raw)
	}
	if r := a.methods(); r.Body["sso"] != nil {
		t.Fatalf("offered switched off: %s", r.Raw)
	}
	if r := a.do(browser(), "GET", "/v1/auth/sso/start", "", nil); r.Status != http.StatusNotFound {
		t.Fatalf("a bare start with nothing offered: %d %s", r.Status, r.Raw)
	}
	a.write(root, "/v1/sso/providers/campus/enabled", m{"enabled": true})
	if r := a.signInThrough("/v1/auth/sso/start", a.idp, "sato@campus.edu", nil); r.Status != http.StatusFound || !hasSession(r) {
		t.Fatalf("switched on again: %d %s", r.Status, r.Raw)
	}
}

// A change to a provider is in force at the next sign-in, without a
// restart: its name on the button, and its client secret, which the
// provider checks when the code is redeemed.
func TestAChangeToAProviderIsInForceAtOnce(t *testing.T) {
	a := newSSOServer(t, ssoSetup{keys: true})
	root := a.tokenFor(a.c.Root)
	campus := provider("campus", "Campus ID", a.idp)
	campus["enabled"] = true
	a.write(root, "/v1/sso/providers", campus)
	a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": a.c.Sato, "provider": "campus", "subject": "sato@campus.edu"}, "link")
	if r := a.signInThrough("/v1/auth/sso/start", a.idp, "sato@campus.edu", nil); r.Status != http.StatusFound {
		t.Fatalf("before: %d %s", r.Status, r.Raw)
	}

	if r := a.write(root, "/v1/sso/providers/campus", m{"version": 1, "display_name": "Campus NetID",
		"client_secret": "a-secret-the-provider-never-gave"}); r.Status != http.StatusOK {
		t.Fatalf("update: %d %s", r.Status, r.Raw)
	}
	if r := a.methods(); r.str("sso", "label") != "Campus NetID" {
		t.Fatalf("the button after the change: %s", r.Raw)
	}
	if r := a.signInThrough("/v1/auth/sso/start", a.idp, "sato@campus.edu", nil); r.Status != http.StatusUnauthorized || hasSession(r) {
		t.Fatalf("with a secret the provider refuses: %d %s", r.Status, r.Raw)
	}
	if r := a.write(root, "/v1/sso/providers/campus", m{"version": 2, "client_secret": clientSecret}); r.Status != http.StatusOK {
		t.Fatalf("update: %d %s", r.Status, r.Raw)
	}
	if r := a.signInThrough("/v1/auth/sso/start", a.idp, "sato@campus.edu", nil); r.Status != http.StatusFound || !hasSession(r) {
		t.Fatalf("with the secret back: %d %s", r.Status, r.Raw)
	}
	// Moved to another issuer, it signs in through that one from the next
	// sign-in on; the identities linked at it stay linked.
	other := newFakeIdP(t)
	if r := a.write(root, "/v1/sso/providers/campus", m{"version": 3, "issuer": other.issuer()}); r.Status != http.StatusOK {
		t.Fatalf("update: %d %s", r.Status, r.Raw)
	}
	if r := a.signInThrough("/v1/auth/sso/start", other, "sato@campus.edu", nil); r.Status != http.StatusFound || !hasSession(r) {
		t.Fatalf("at the new issuer: %d %s", r.Status, r.Raw)
	}
}

// The operator's provider wins over one of the site's with its id: only the
// operator's is offered, and a sign-in by that name goes through it.
func TestTheOperatorsProviderWinsOverTheSitesOfItsName(t *testing.T) {
	a := newSSOServer(t, ssoSetup{operator: true, keys: true})
	other := newFakeIdP(t)
	sealed, err := a.keys.Seal(sso.SecretBinding("polyu-adfs"), clientSecret)
	if err != nil {
		t.Fatal(err)
	}
	a.c.Exec(`INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint, subject_claim, enabled,
		created_by_actor_id, updated_by_actor_id) VALUES ('polyu-adfs', 'Imposter', $1, $2, $3, '…', 'upn', true, $4, $4)`,
		other.issuer(), clientID, sealed, a.c.Root)
	r := a.methods()
	if ps := r.Body["sso_providers"].([]any); len(ps) != 1 || strings.Contains(r.Raw, "Imposter") {
		t.Fatalf("methods: %s", r.Raw)
	}
	a.link(a.c.Sato, "sato@polyu.edu.hk")
	if r := a.signInThrough("/v1/auth/sso/start/polyu-adfs", a.idp, "sato@polyu.edu.hk", nil); r.Status != http.StatusFound || !hasSession(r) {
		t.Fatalf("through the operator's: %d %s", r.Status, r.Raw)
	}
	root := a.tokenFor(a.c.Root)
	list := a.do(nil, "GET", "/v1/sso/providers", root, nil)
	var statuses []string
	for _, p := range list.Body["result"].(m)["providers"].([]any) {
		statuses = append(statuses, p.(m)["source"].(string)+":"+p.(m)["status"].(string))
	}
	if strings.Join(statuses, " ") != "operator:offered site:id_taken" {
		t.Fatalf("list: %s", list.Raw)
	}
}

// With link_by_email, someone the provider vouches for, whose identity is
// linked to nobody, is linked at sign-in to the active person whose email
// here is the one the provider vouches for, within its domains — and nobody
// else is: not an email it does not vouch for, not one of another domain,
// not an account whose email nobody here vouches for, not one with a
// platform role, not an identity once unlinked. Off, as by default, only
// accounts already linked sign in.
func TestLinkingByTheEmailAProviderVouchesFor(t *testing.T) {
	a := newSSOServer(t, ssoSetup{keys: true})
	root := a.tokenFor(a.c.Root)
	campus := provider("campus", "Campus ID", a.idp)
	campus["enabled"] = true
	a.write(root, "/v1/sso/providers", campus)

	person := func(name, email string) uuid.UUID {
		id := a.c.Actor("human", name)
		a.c.Exec(`UPDATE actor SET email = $2 WHERE id = $1`, id, email)
		return id
	}
	mei := person("Mei", "mei@polyu.edu.hk")
	vouched := func(email string) func(map[string]any) {
		return func(c map[string]any) { c["email"] = email; c["email_verified"] = true }
	}
	refused := func(what string, r response) {
		t.Helper()
		if r.Status != http.StatusForbidden || hasSession(r) || !strings.Contains(r.Raw, "not registered") {
			t.Fatalf("%s: %d %s", what, r.Status, r.Raw)
		}
	}
	// Off, as it is by default: an account linked to nobody is nobody.
	refused("linking by email off", a.signInThrough("/v1/auth/sso/start", a.idp, "mei-1", vouched("mei@polyu.edu.hk")))

	if r := a.write(root, "/v1/sso/providers/campus", m{"version": 1, "link_by_email": true,
		"allowed_email_domains": []any{"polyu.edu.hk"}}); r.Status != http.StatusOK || r.str("result", "email_claim") != "email" {
		t.Fatalf("switch linking by email on: %d %s", r.Status, r.Raw)
	}
	done := a.signInThrough("/v1/auth/sso/start", a.idp, "mei-1", vouched("Mei@PolyU.edu.HK"))
	if done.Status != http.StatusFound || !hasSession(done) {
		t.Fatalf("Mei, vouched for: %d %s", done.Status, done.Raw)
	}
	if n := a.c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'sso' AND provider = 'campus' AND subject = 'mei-1'
		AND revoked_at IS NULL`, mei); n != 1 {
		t.Fatalf("Mei's identity is linked %d times", n)
	}
	if n := a.c.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND action_type = 'sso.link_by_email' AND status = 'executed'`, mei); n != 1 {
		t.Fatalf("the link is on record %d times", n)
	}
	// From now on, by the subject, whatever the email says.
	if r := a.signInThrough("/v1/auth/sso/start", a.idp, "mei-1", nil); r.Status != http.StatusFound || !hasSession(r) {
		t.Fatalf("Mei again: %d %s", r.Status, r.Raw)
	}

	ho := person("Ho", "ho@polyu.edu.hk")
	refused("an email the provider does not vouch for", a.signInThrough("/v1/auth/sso/start", a.idp, "ho-1",
		func(c map[string]any) { c["email"] = "ho@polyu.edu.hk"; c["email_verified"] = false }))
	refused("an email with no email_verified", a.signInThrough("/v1/auth/sso/start", a.idp, "ho-1",
		func(c map[string]any) { c["email"] = "ho@polyu.edu.hk" }))
	person("Wu", "wu@gmail.com")
	refused("an email of another domain", a.signInThrough("/v1/auth/sso/start", a.idp, "wu-1", vouched("wu@gmail.com")))
	refused("an email of a subdomain", a.signInThrough("/v1/auth/sso/start", a.idp, "ho-2", vouched("ho@connect.polyu.edu.hk")))
	refused("an email nobody has", a.signInThrough("/v1/auth/sso/start", a.idp, "zz-1", vouched("nobody@polyu.edu.hk")))
	lee := person("Lee", "lee@polyu.edu.hk")
	a.c.Exec(`UPDATE actor SET email_verified = false WHERE id = $1`, lee)
	refused("an account whose email nobody here vouches for", a.signInThrough("/v1/auth/sso/start", a.idp, "lee-1", vouched("lee@polyu.edu.hk")))
	boss := person("Boss", "boss@polyu.edu.hk")
	a.c.Exec(`UPDATE actor SET platform_role = 'admin' WHERE id = $1`, boss)
	refused("an account with a platform role", a.signInThrough("/v1/auth/sso/start", a.idp, "boss-1", vouched("boss@polyu.edu.hk")))
	sus := person("Sus", "sus@polyu.edu.hk")
	a.c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, sus)
	refused("a suspended account", a.signInThrough("/v1/auth/sso/start", a.idp, "sus-1", vouched("sus@polyu.edu.hk")))
	// Mei's account is linked at campus already: another identity with her
	// email is not linked to it too.
	refused("a second identity for one account", a.signInThrough("/v1/auth/sso/start", a.idp, "mei-2", vouched("mei@polyu.edu.hk")))
	// An identity unlinked is not linked again by its email: that was
	// someone's decision.
	var cred uuid.UUID
	if err := a.c.Pool.QueryRow(t.Context(), `SELECT id FROM credential WHERE kind = 'sso' AND actor_id = $1`, mei).Scan(&cred); err != nil {
		t.Fatal(err)
	}
	a.c.MustCall(a.c.Root, "actor.revoke_credential", m{"actor_id": mei, "credential_id": cred}, "unlink-mei")
	refused("an identity unlinked", a.signInThrough("/v1/auth/sso/start", a.idp, "mei-1", vouched("mei@polyu.edu.hk")))

	if n := a.c.Count(`SELECT count(*) FROM credential WHERE kind = 'sso' AND actor_id IN ($1, $2, $3, $4)`, ho, lee, boss, sus); n != 0 {
		t.Fatalf("%d identities linked that should not be", n)
	}
	if n := a.c.Count(`SELECT count(*) FROM action WHERE action_type = 'sso.link_by_email' AND status = 'executed'`); n != 1 {
		t.Fatalf("%d links by email", n)
	}
}

// A server held to public addresses (no SSO_ALLOW_PRIVATE_ISSUERS) reaches
// no provider of the site's on this machine, where the fake one is: set up
// there while the server allowed it, the provider is offered still, and a
// sign-in through it is told it cannot be reached, its discovery refused as
// it was dialled, and the log says why. The operator's provider, on this
// machine too, signs people in as before: it is the operator's own setting.
func TestASitesProviderOnAPrivateAddressIsNotReached(t *testing.T) {
	a := newSSOServer(t, ssoSetup{operator: true, keys: true, publicOnly: true})
	root := a.tokenFor(a.c.Root)
	campus := provider("campus", "Campus ID", a.idp)
	campus["enabled"] = true
	if r := a.write(root, "/v1/sso/providers", campus); r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Raw)
	}
	a.c.MustCall(a.c.Root, "actor.link_sso", m{"actor_id": a.c.Sato, "provider": "campus", "subject": "sato@site.example.edu"}, "link")
	if r := a.do(browser(), "GET", "/v1/auth/sso/start/campus", "", nil); r.Status != http.StatusUnprocessableEntity ||
		r.str("error", "details", "reason") != "sso_provider_unavailable" || strings.Contains(r.Raw, "127.0.0.1") {
		t.Fatalf("a sign-in through a provider on this machine: %d %s", r.Status, r.Raw)
	}
	if !strings.Contains(a.log.String(), sso.ReasonAddressNotAllowed) {
		t.Fatalf("the log does not say why: %s", a.log.String())
	}

	a.link(a.c.Sato, "sato@campus.example.edu")
	if r := a.signInThrough("/v1/auth/sso/start/"+a.op.ID, a.idp, "sato@campus.example.edu", nil); r.Status != http.StatusFound || !hasSession(r) {
		t.Fatalf("the operator's provider: %d %s", r.Status, r.Raw)
	}
}
