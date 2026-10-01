package tools_test

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// ssoSite is a platform whose single sign-on has the operator's provider,
// polyu-adfs, and a secrets key, unless the test says otherwise; Admin is a
// platform administrator and Dan a person with no role.
type ssoSite struct {
	*testkit.Platform
	t          *testing.T
	admin, dan uuid.UUID
	keys       *secrets.Keyring
	// op is the operator's provider, nil in a site with none.
	op *sso.Operator
}

const googleSecret = "GOCSPX-a-google-client-secret-1234"

func newSSOSite(t *testing.T, operator, keys bool) *ssoSite {
	t.Helper()
	return newSSOSiteWith(t, ssoOptions{operator: operator, keys: keys})
}

// ssoOptions are what newSSOSiteWith makes a site with.
type ssoOptions struct {
	operator, keys bool
	// private lets the site's providers be on private addresses, as
	// SSO_ALLOW_PRIVATE_ISSUERS does: a fake provider is on this machine.
	private bool
	// operatorIssuer, when set, is the operator's provider's issuer in
	// place of the one it has otherwise.
	operatorIssuer string
}

func newSSOSiteWith(t *testing.T, o ssoOptions) *ssoSite {
	t.Helper()
	s := &ssoSite{t: t}
	operator, keys := o.operator, o.keys
	if keys {
		k := make([]byte, secrets.KeySize)
		_, _ = rand.Read(k)
		s.keys, _ = secrets.NewKeyring(k)
	}
	var op *sso.Operator
	if operator {
		op = &sso.Operator{ID: "polyu-adfs", DisplayName: "PolyU NetID", Issuer: "https://adfs.polyu.example/adfs", ClientID: "aishie",
			SecretHint: secrets.Hint("the-operator's-client-secret"), Scopes: sso.DefaultScopes, SubjectClaim: "upn"}
		if o.operatorIssuer != "" {
			op.Issuer = o.operatorIssuer
		}
	}
	s.op = op
	s.Platform = testkit.NewPlatformWithDeps(t, func(d *tools.Deps) {
		d.SSO = sso.New(sso.Config{Operator: op, Keys: s.keys, PublicURL: "https://lms.example.edu", PrivateIssuers: o.private})
	})
	s.admin = s.Actor("human", "Admin")
	s.Exec(`UPDATE actor SET platform_role = 'admin' WHERE id = $1`, s.admin)
	s.dan = s.Actor("human", "Dan")
	return s
}

func (s *ssoSite) call(actor uuid.UUID, name string, args m) pipeline.Outcome {
	s.t.Helper()
	return s.MustCall(actor, name, args, "k-"+uuid.NewString())
}

func (s *ssoSite) do(actor uuid.UUID, name string, args m) pipeline.Outcome {
	s.t.Helper()
	out := s.call(actor, name, args)
	if out.Status != domain.StatusExecuted {
		s.t.Fatalf("%s: %+v", name, out)
	}
	return out
}

func (s *ssoSite) fails(actor uuid.UUID, name string, args m, code apperr.Code, reason string) *apperr.Error {
	s.t.Helper()
	out := s.call(actor, name, args)
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != code ||
		reason != "" && out.Error.Details["reason"] != reason {
		s.t.Fatalf("%s: %+v, want failed %s (%s)", name, out, code, reason)
	}
	return out.Error
}

func (s *ssoSite) view(name string, args m) tools.SSOProviderView {
	s.t.Helper()
	return testkit.Result[tools.SSOProviderView](s.t, s.do(s.admin, name, args))
}

func (s *ssoSite) list() tools.SSOListOut {
	s.t.Helper()
	return testkit.Result[tools.SSOListOut](s.t, s.do(s.admin, "sso.list", m{}))
}

var google = m{"id": "google", "display_name": "Google", "issuer": "https://accounts.google.com", "client_id": "aishie.apps.example",
	"client_secret": googleSecret, "email_claim": "email"}

// A platform administrator sets up a provider, reads it, changes it over the
// version read, switches it on and removes it; each is an action, and the
// client secret is sealed and never said back, recorded or kept in the
// clear.
func TestAnAdministratorSetsUpAnIdentityProvider(t *testing.T) {
	s := newSSOSite(t, true, true)

	l := s.list()
	if !l.CanAdd || l.CannotAddReason != nil || l.SecretsKeyID == nil || *l.SecretsKeyID != s.keys.KeyID() ||
		l.RedirectURI != "https://lms.example.edu/v1/auth/sso/callback" || len(l.Providers) != 1 {
		t.Fatalf("list: %+v", l)
	}
	op := l.Providers[0]
	if op.ID != "polyu-adfs" || op.Source != "operator" || !op.ReadOnly || op.Status != "offered" || !op.Enabled || op.Version != nil ||
		op.DisplayName == nil || *op.DisplayName != "PolyU NetID" || op.ClientSecretHint != "…cret" || op.SubjectClaim != "upn" ||
		op.RedirectURI != l.RedirectURI {
		t.Fatalf("the operator's provider: %+v", op)
	}

	created := s.view("sso.create", google)
	if created.ID != "google" || created.Source != "site" || created.ReadOnly || created.Status != "disabled" || created.Enabled ||
		created.Version == nil || *created.Version != 1 || created.ClientSecretHint != "…1234" || created.SubjectClaim != "sub" ||
		strings.Join(created.Scopes, " ") != "openid profile email" || created.EmailClaim == nil || *created.EmailClaim != "email" ||
		created.LinkByEmail || created.Position != 1 || created.CreatedBy == nil || created.CreatedBy.ActorID != s.admin ||
		created.UpdatedBy.DisplayName != "Admin" || created.ClientSecretKeyID == nil || *created.ClientSecretKeyID != s.keys.KeyID() ||
		created.RedirectURI != "https://lms.example.edu/v1/auth/sso/callback" || created.LinkedAccounts != 0 {
		t.Fatalf("created: %+v", created)
	}
	if got := s.view("sso.get", m{"provider_id": "google"}); got.Issuer != "https://accounts.google.com" || *got.Version != 1 {
		t.Fatalf("get: %+v", got)
	}
	if l := s.list(); len(l.Providers) != 2 || l.Providers[0].ID != "polyu-adfs" || l.Providers[1].ID != "google" {
		t.Fatalf("list: %+v", l.Providers)
	}

	// Changed over the version read: what is given changes, and the rest,
	// the secret among it, is kept.
	var sealed string
	sealedNow := func() string {
		t.Helper()
		var v string
		if err := s.Pool.QueryRow(t.Context(), `SELECT client_secret_sealed FROM sso_provider WHERE id = 'google'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	sealed = sealedNow()
	updated := s.view("sso.update", m{"provider_id": "google", "version": 1, "display_name": "Google account", "position": 5})
	if *updated.DisplayName != "Google account" || *updated.Version != 2 || updated.Position != 5 || updated.ClientSecretHint != "…1234" ||
		updated.Issuer != created.Issuer || sealedNow() != sealed {
		t.Fatalf("updated: %+v", updated)
	}
	// Made over the version before: refused, saying which version it is now.
	e := s.fails(s.admin, "sso.update", m{"provider_id": "google", "version": 1, "display_name": "Stale"}, apperr.Conflict, "version_mismatch")
	if e.Details["current_version"] != int32(2) {
		t.Fatalf("the refusal: %+v", e)
	}
	// A new secret is sealed afresh; given nothing else, nothing else changes.
	const newSecret = "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-9876"
	updated = s.view("sso.update", m{"provider_id": "google", "version": 2, "client_secret": newSecret})
	if updated.ClientSecretHint != "…9876" || *updated.Version != 3 || sealedNow() == sealed || *updated.DisplayName != "Google account" {
		t.Fatalf("a new secret: %+v", updated)
	}
	if got, err := s.keys.Open(sso.SecretBinding("google"), sealedNow()); err != nil || got != newSecret {
		t.Fatalf("the sealed secret opens to %q, %v", got, err)
	}

	on := s.view("sso.set_enabled", m{"provider_id": "google", "enabled": true, "version": 3})
	if !on.Enabled || on.Status != "offered" || *on.Version != 4 {
		t.Fatalf("switched on: %+v", on)
	}
	if again := s.view("sso.set_enabled", m{"provider_id": "google", "enabled": true}); *again.Version != 4 {
		t.Fatalf("switched on twice: %+v", again)
	}

	// Neither secret is anywhere a reader could find it: not in any answer,
	// not in the action log, not in the row.
	for _, secret := range []string{googleSecret, newSecret} {
		if n := s.Count(`SELECT count(*) FROM action WHERE payload::text LIKE '%' || $1 || '%' OR result::text LIKE '%' || $1 || '%'`,
			secret); n != 0 {
			t.Fatalf("the action log holds a client secret in %d actions", n)
		}
		if n := s.Count(`SELECT count(*) FROM sso_provider WHERE row_to_json(sso_provider)::text LIKE '%' || $1 || '%'`, secret); n != 0 {
			t.Fatal("the provider's row holds its secret in the clear")
		}
	}
	for name, args := range map[string]m{"sso.list": {}, "sso.get": {"provider_id": "google"}} {
		raw, _ := json.Marshal(s.do(s.admin, name, args))
		if strings.Contains(string(raw), "GOCSPX-") || strings.Contains(string(raw), "the-operator's-client-secret") {
			t.Fatalf("%s says a secret: %s", name, raw)
		}
	}
	if n := s.Count(`SELECT count(*) FROM action WHERE action_type LIKE 'sso.%' AND status = 'executed' AND actor_id = $1`, s.admin); n != 5 {
		t.Fatalf("%d of the provider's changes are on record, want 5", n)
	}

	// Removed while accounts are linked at it: refused, saying how many,
	// unless forced, which unlinks them for the record.
	yuki, ken := s.Actor("human", "Yuki"), s.Actor("human", "Ken")
	s.do(s.admin, "actor.link_sso", m{"actor_id": yuki, "provider": "google", "subject": "1001"})
	s.do(s.admin, "actor.link_sso", m{"actor_id": ken, "provider": "google", "subject": "1002"})
	if got := s.view("sso.get", m{"provider_id": "google"}); got.LinkedAccounts != 2 {
		t.Fatalf("linked accounts: %d", got.LinkedAccounts)
	}
	e = s.fails(s.admin, "sso.delete", m{"provider_id": "google"}, apperr.Conflict, "provider_in_use")
	if e.Details["linked_accounts"] != int32(2) || !strings.Contains(e.Message, "2 accounts") {
		t.Fatalf("the refusal: %+v", e)
	}
	s.fails(s.admin, "sso.delete", m{"provider_id": "google", "force": true, "version": 3}, apperr.Conflict, "version_mismatch")
	gone := testkit.Result[tools.SSODeleteOut](t, s.do(s.admin, "sso.delete", m{"provider_id": "google", "force": true, "version": 4}))
	if !gone.Deleted || gone.UnlinkedAccounts != 2 || gone.ID != "google" {
		t.Fatalf("deleted: %+v", gone)
	}
	if n := s.Count(`SELECT count(*) FROM credential WHERE provider = 'google' AND revoked_at IS NOT NULL`); n != 2 {
		t.Fatalf("%d identities unlinked for the record, want 2", n)
	}
	if _, err := s.Call(s.admin, "sso.get", m{"provider_id": "google"}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	s.fails(s.admin, "sso.delete", m{"provider_id": "google"}, apperr.NotFound, "sso_provider_not_found")
	// Set up again under the same id, it finds nobody linked: the unlinked
	// stay unlinked until an administrator links them again.
	again := s.view("sso.create", google)
	if again.LinkedAccounts != 0 || *again.Version != 1 {
		t.Fatalf("set up again: %+v", again)
	}
}

// A retry of a write with its key is the same write, done once.
func TestSettingUpAProviderTwiceWithOneKeyDoesItOnce(t *testing.T) {
	s := newSSOSite(t, false, true)
	first := s.MustCall(s.admin, "sso.create", google, "set-up-google")
	again := s.MustCall(s.admin, "sso.create", google, "set-up-google")
	if first.Status != domain.StatusExecuted || !again.Replayed || *again.ActionID != *first.ActionID ||
		testkit.Result[tools.SSOProviderView](t, again).ClientSecretHint != "…1234" {
		t.Fatalf("first %+v, again %+v", first, again)
	}
	other := m{}
	for k, v := range google {
		other[k] = v
	}
	other["client_secret"] = "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ame-key"
	if out, err := s.Call(s.admin, "sso.create", other, "set-up-google"); !apperr.Is(err, apperr.IdempotencyConflict) {
		t.Fatalf("the key with another secret: %+v %v", out, err)
	}
	if n := s.Count(`SELECT count(*) FROM sso_provider`); n != 1 {
		t.Fatalf("%d providers", n)
	}
}

// The operator's provider is read-only here: nothing an administrator does
// changes it, and its id is taken.
func TestTheOperatorsProviderIsReadOnly(t *testing.T) {
	s := newSSOSite(t, true, true)
	for name, args := range map[string]m{
		"sso.update":      {"provider_id": "polyu-adfs", "version": 1, "display_name": "Mine now"},
		"sso.set_enabled": {"provider_id": "polyu-adfs", "enabled": false},
		"sso.delete":      {"provider_id": "polyu-adfs", "force": true},
	} {
		s.fails(s.admin, name, args, apperr.FailedPrecondition, "set_by_operator")
	}
	taken := m{}
	for k, v := range google {
		taken[k] = v
	}
	taken["id"] = "polyu-adfs"
	s.fails(s.admin, "sso.create", taken, apperr.Conflict, "id_taken")
	s.do(s.admin, "sso.create", google)
	s.fails(s.admin, "sso.create", google, apperr.Conflict, "id_taken")

	// A site's provider with its id, from before the operator set theirs
	// (here, by hand), is listed as such, offered nowhere, and its
	// identities are the operator's.
	sealed, err := s.keys.Seal(sso.SecretBinding("polyu-adfs"), "an-old-secret-of-the-sites-own")
	if err != nil {
		t.Fatal(err)
	}
	s.Exec(`INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint, enabled,
		created_by_actor_id, updated_by_actor_id) VALUES ('polyu-adfs', 'Old ADFS', 'https://old.example/adfs', 'old', $1, '…', true, $2, $2)`,
		sealed, s.admin)
	yuki := s.Actor("human", "Yuki")
	s.do(s.admin, "actor.link_sso", m{"actor_id": yuki, "provider": "polyu-adfs", "subject": "yuki@polyu.edu.hk"})
	l := s.list()
	if len(l.Providers) != 3 || l.Providers[0].Source != "operator" || l.Providers[0].LinkedAccounts != 1 {
		t.Fatalf("list: %+v", l.Providers)
	}
	var shadowed *tools.SSOProviderView
	for i, p := range l.Providers {
		if p.Source == "site" && p.ID == "polyu-adfs" {
			shadowed = &l.Providers[i]
		}
	}
	if shadowed == nil || shadowed.Status != "id_taken" || shadowed.LinkedAccounts != 0 {
		t.Fatalf("the site's provider under the operator's id: %+v", shadowed)
	}
}

// Without a secrets key, no provider is added, and the list says why; the
// operator's provider is there all the same.
func TestNoProviderIsAddedWithoutASecretsKey(t *testing.T) {
	s := newSSOSite(t, true, false)
	l := s.list()
	if l.CanAdd || l.CannotAddReason == nil || *l.CannotAddReason != "secrets_key_missing" || l.SecretsKeyID != nil ||
		len(l.Providers) != 1 || l.Providers[0].Status != "offered" {
		t.Fatalf("list: %+v", l)
	}
	e := s.fails(s.admin, "sso.create", google, apperr.FailedPrecondition, "secrets_key_missing")
	if !strings.Contains(e.Message, "SECRETS_KEY") || strings.Contains(e.Message, googleSecret) {
		t.Fatalf("the refusal: %s", e.Message)
	}
	if n := s.Count(`SELECT count(*) FROM sso_provider`); n != 0 {
		t.Fatalf("%d providers", n)
	}
}

// What a provider is set up with is held to what a sign-in can use, and a
// refusal names the field without repeating a secret.
func TestAProvidersSettingsAreHeldToTheirRules(t *testing.T) {
	s := newSSOSite(t, false, true)
	with := func(k string, v any) m {
		out := m{}
		for key, val := range google {
			out[key] = val
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	for name, tc := range map[string]struct {
		args  m
		field string
	}{
		"an id with upper case":            {with("id", "Google"), "id"},
		"an id with a slash":               {with("id", "a/b"), "id"},
		"an id ending in a hyphen":         {with("id", "google-"), "id"},
		"a name that is white space":       {with("display_name", "   "), "display_name"},
		"a name of two lines":              {with("display_name", "Google\nAccount"), "display_name"},
		"a name of 65 characters":          {with("display_name", strings.Repeat("g", 65)), "display_name"},
		"an issuer over http":              {with("issuer", "http://accounts.google.com"), "issuer"},
		"an issuer with a query":           {with("issuer", "https://accounts.google.com?x=1"), "issuer"},
		"an issuer that is no URL":         {with("issuer", "accounts.google.com"), "issuer"},
		"no client id":                     {with("client_id", " "), "client_id"},
		"a client secret that is no ASCII": {with("client_secret", "GOCSPX-秘密-secret-secret"), "client_secret"},
		"no client secret":                 {with("client_secret", ""), "client_secret"},
		"scopes without openid":            {with("scopes", []any{"profile", "email"}), "scopes"},
		"a scope with a quote":             {with("scopes", []any{"openid", `pro"file`}), "scopes"},
		"a claim with a space":             {with("subject_claim", "user name"), "subject_claim"},
		"a domain that is no domain":       {with("allowed_email_domains", []any{"polyu"}), "allowed_email_domains"},
		"linking by email with no domains": {with("link_by_email", true), "allowed_email_domains"},
		"a position below the page":        {with("position", -1), "position"},
	} {
		t.Run(name, func(t *testing.T) {
			e := s.fails(s.admin, "sso.create", tc.args, apperr.InvalidArgument, "")
			if e.Details["field"] != tc.field || strings.Contains(e.Message, "秘密") || strings.Contains(e.Message, googleSecret) {
				t.Fatalf("%+v", e)
			}
		})
	}
	// Without SSO_ALLOW_PRIVATE_ISSUERS, an issuer plainly not at a public
	// address is refused as it is set up, for that; a name is checked when
	// it is fetched (TestAnIssuerOnAPrivateAddressIsNotFetched).
	for name, issuer := range map[string]string{
		"an issuer on this machine over http":   "http://127.0.0.1:5556/dex",
		"an issuer at localhost":                "https://localhost/adfs",
		"an issuer at a subdomain of localhost": "https://idp.localhost/adfs",
		"an issuer on a private network":        "https://10.20.30.40/adfs",
		"an issuer at the cloud's metadata":     "https://169.254.169.254/latest",
		"an issuer on loopback in IPv6":         "https://[::ffff:127.0.0.1]/adfs",
		"an issuer on a unique local IPv6":      "https://[fd00:ec2::254]/adfs",
	} {
		t.Run(name, func(t *testing.T) {
			e := s.fails(s.admin, "sso.create", with("issuer", issuer), apperr.InvalidArgument, sso.ReasonAddressNotAllowed)
			if e.Details["field"] != "issuer" || strings.Contains(e.Message, googleSecret) {
				t.Fatalf("%+v", e)
			}
		})
	}
	// A public address is taken, as a name is.
	s.do(s.admin, "sso.create", with("issuer", "https://8.8.8.8/adfs"))

	// With SSO_ALLOW_PRIVATE_ISSUERS, http is for this machine only; a
	// domain is kept in lower case, once; scopes are kept in order, once;
	// linking by email reads email unless told otherwise.
	s = newSSOSiteWith(t, ssoOptions{keys: true, private: true})
	local := s.view("sso.create", m{"id": "dev", "display_name": "Dev", "issuer": "http://127.0.0.1:5556/dex", "client_id": "aishie",
		"client_secret": "x", "scopes": []any{"openid", "email", "openid"}, "link_by_email": true,
		"allowed_email_domains": []any{"@PolyU.edu.hk", "polyu.edu.hk", "connect.polyu.hk"}})
	if local.Issuer != "http://127.0.0.1:5556/dex" || strings.Join(local.Scopes, " ") != "openid email" || local.EmailClaim == nil ||
		*local.EmailClaim != "email" || strings.Join(local.AllowedEmailDomains, " ") != "polyu.edu.hk connect.polyu.hk" ||
		!local.LinkByEmail || local.ClientSecretHint != "…" {
		t.Fatalf("%+v", local)
	}
	// Changed, the settings are held to the same rules together.
	s.fails(s.admin, "sso.update", m{"provider_id": "dev", "version": 1, "allowed_email_domains": []any{}}, apperr.InvalidArgument, "")
	s.fails(s.admin, "sso.update", m{"provider_id": "dev", "version": 1, "issuer": "http://idp.example.edu"}, apperr.InvalidArgument, "")
	off := s.view("sso.update", m{"provider_id": "dev", "version": 1, "link_by_email": false, "email_claim": "", "allowed_email_domains": []any{}})
	if off.LinkByEmail || off.EmailClaim != nil || len(off.AllowedEmailDomains) != 0 {
		t.Fatalf("%+v", off)
	}
}

// Only root and the platform's administrators set up providers: a person
// with no role is refused, on the record.
func TestOnlyPlatformAdministratorsSetUpProviders(t *testing.T) {
	s := newSSOSite(t, true, true)
	s.do(s.admin, "sso.create", google)
	for name, args := range map[string]m{
		"sso.list":        {},
		"sso.get":         {"provider_id": "google"},
		"sso.test":        {"provider_id": "google"},
		"sso.create":      merge(google, m{"id": "google2"}),
		"sso.update":      {"provider_id": "google", "version": 1, "display_name": "Mine"},
		"sso.set_enabled": {"provider_id": "google", "enabled": true},
		"sso.delete":      {"provider_id": "google", "force": true},
	} {
		out := s.call(s.dan, name, args)
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "platform_role_required" {
			t.Fatalf("%s: %+v", name, out)
		}
	}
	if got := s.view("sso.get", m{"provider_id": "google"}); *got.Version != 1 || got.Enabled {
		t.Fatalf("changed by someone refused: %+v", got)
	}
	// Root may, as any platform administrator.
	s.do(s.Root, "sso.set_enabled", m{"provider_id": "google", "enabled": true})
}

// sso.test reads an issuer's discovery document and keys, says what it
// found, and catches what would stop a sign-in; it signs nobody in.
func TestAProvidersIssuerIsTested(t *testing.T) {
	s := newSSOSiteWith(t, ssoOptions{keys: true, private: true})
	var issuer string
	doc := func(change func(map[string]any)) map[string]any {
		d := map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"jwks_uri": issuer + "/keys", "userinfo_endpoint": issuer + "/userinfo", "id_token_signing_alg_values_supported": []string{"RS256"},
			"scopes_supported": []string{"openid", "profile", "email"}, "claims_supported": []string{"sub", "email", "upn"},
			"response_types_supported": []string{"code", "id_token"}}
		if change != nil {
			change(d)
		}
		return d
	}
	var discovery map[string]any
	keys := `{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig","n":"AQAB","e":"AQAB"},{"kty":"RSA","kid":"enc","use":"enc"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /idp/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(discovery)
	})
	mux.HandleFunc("GET /idp/keys", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(keys)) })
	mux.HandleFunc("GET /moved/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/idp/.well-known/openid-configuration", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL + "/idp"
	test := func(args m) sso.Report {
		t.Helper()
		return testkit.Result[sso.Report](t, s.do(s.admin, "sso.test", args))
	}

	discovery = doc(nil)
	r := test(m{"issuer": issuer})
	if !r.OK || len(r.Problems) != 0 || r.DiscoveryURL != issuer+"/.well-known/openid-configuration" ||
		r.AuthorizationEndpoint == nil || *r.AuthorizationEndpoint != issuer+"/authorize" || r.TokenEndpoint == nil ||
		r.UserinfoEndpoint == nil || r.EndSessionEndpoint != nil || len(r.SigningKeys) != 1 || *r.SigningKeys[0].KeyID != "k1" ||
		strings.Join(r.ScopesSupported, " ") != "openid profile email" || strings.Join(r.ClaimsSupported, " ") != "sub email upn" ||
		strings.Join(r.SigningAlgorithms, " ") != "RS256" {
		t.Fatalf("a good issuer: %+v", r)
	}
	// A provider set up is tested with its own scopes and claims.
	s.do(s.admin, "sso.create", m{"id": "idp", "display_name": "IdP", "issuer": issuer, "client_id": "c", "client_secret": "s",
		"subject_claim": "employee_id", "scopes": []any{"openid", "groups"}})
	r = test(m{"provider_id": "idp"})
	if !r.OK || strings.Join(r.RequestedScopesUnsupported, " ") != "groups" || strings.Join(r.RequestedClaimsNotAdvertised, " ") != "employee_id" ||
		len(r.Warnings) != 2 {
		t.Fatalf("a provider asking for what is not advertised: %+v", r)
	}

	for name, tc := range map[string]struct {
		issuer string
		doc    func(map[string]any)
		keys   string
		want   string
	}{
		"an issuer written otherwise than the provider writes it": {issuer: issuer + "/", want: "names its issuer"},
		"a document naming another issuer": {doc: func(d map[string]any) { d["issuer"] = "https://evil.example/idp" },
			want: "names its issuer"},
		"no token endpoint":     {doc: func(d map[string]any) { delete(d, "token_endpoint") }, want: "no token_endpoint"},
		"no key set":            {doc: func(d map[string]any) { delete(d, "jwks_uri") }, want: "no jwks_uri"},
		"no key to sign with":   {keys: `{"keys":[{"kty":"RSA","kid":"enc","use":"enc"}]}`, want: "no key to check a signature with"},
		"a key set that is not": {keys: `<html>`, want: "not the JSON object"},
		"no code flow": {doc: func(d map[string]any) { d["response_types_supported"] = []string{"id_token"} },
			want: "no response_type code"},
		"signatures nobody checks": {doc: func(d map[string]any) { d["id_token_signing_alg_values_supported"] = []string{"none", "HS256"} },
			want: "signs id_tokens with none of"},
		"a path with no provider":    {issuer: srv.URL + "/nothing", want: "answered HTTP 404"},
		"a redirect":                 {issuer: srv.URL + "/moved", want: "redirects (HTTP 302)"},
		"a provider nobody answers":  {issuer: "http://127.0.0.1:1/adfs", want: "could not be reached"},
		"an issuer over http":        {issuer: "http://idp.example.edu/adfs", want: "is http"},
		"an issuer that is no URL":   {issuer: "idp.example.edu", want: "absolute URL"},
		"a token endpoint over http": {doc: func(d map[string]any) { d["token_endpoint"] = "http://idp.example.edu/token" }, want: "in the clear"},
	} {
		t.Run(name, func(t *testing.T) {
			discovery, keys = doc(tc.doc), `{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig"}]}`
			if tc.keys != "" {
				keys = tc.keys
			}
			at := issuer
			if tc.issuer != "" {
				at = tc.issuer
			}
			r := test(m{"issuer": at})
			if r.OK || !strings.Contains(strings.Join(r.Problems, "; "), tc.want) {
				t.Fatalf("%+v", r)
			}
		})
	}
	if _, err := s.Call(s.admin, "sso.test", m{}, ""); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("neither provider_id nor issuer: %v", err)
	}
	if _, err := s.Call(s.admin, "sso.test", m{"issuer": issuer, "provider_id": "idp"}, ""); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("both: %v", err)
	}
	if _, err := s.Call(s.admin, "sso.test", m{"provider_id": "nobody"}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a provider that is not there: %v", err)
	}
	// Nothing was recorded: it is a read.
	if n := s.Count(`SELECT count(*) FROM action WHERE action_type = 'sso.test'`); n != 0 {
		t.Fatalf("%d tests recorded", n)
	}
}

// Without SSO_ALLOW_PRIVATE_ISSUERS, sso.test fetches nothing of a provider
// of the site's on this machine, where the fake one is: an issuer plainly
// there is a problem before anything is fetched, and so is the issuer of
// one set up there while the server allowed it. A name that resolves there
// is refused as it is dialled (package sso's tests). The
// operator's provider, on this machine as well, is tested as before: it is
// the operator's own setting.
func TestAnIssuerOnAPrivateAddressIsNotFetched(t *testing.T) {
	var fetched atomic.Int64
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /idp/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		fetched.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"scopes_supported": []string{"openid", "profile", "email"}, "claims_supported": []string{"sub", "upn", "email"}})
	})
	mux.HandleFunc("GET /idp/keys", func(w http.ResponseWriter, _ *http.Request) {
		fetched.Add(1)
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL + "/idp"
	s := newSSOSiteWith(t, ssoOptions{operator: true, keys: true, operatorIssuer: issuer})
	test := func(args m) sso.Report {
		t.Helper()
		return testkit.Result[sso.Report](t, s.do(s.admin, "sso.test", args))
	}
	refused := func(r sso.Report) bool {
		return !r.OK && len(r.Problems) == 1 && strings.Contains(r.Problems[0], sso.ReasonAddressNotAllowed) &&
			strings.Contains(r.Problems[0], "SSO_ALLOW_PRIVATE_ISSUERS")
	}

	for _, at := range []string{issuer, "http://localhost:1/idp", "https://169.254.169.254/latest", "https://[::1]/idp"} {
		if r := test(m{"issuer": at}); !refused(r) || r.DiscoveryURL != "" {
			t.Fatalf("%s: %+v", at, r)
		}
	}
	if n := fetched.Load(); n != 0 {
		t.Fatalf("fetched %d times", n)
	}

	s.do(s.admin, "sso.create", m{"id": "campus", "display_name": "Campus", "issuer": "https://idp.campus.example/adfs", "client_id": "c",
		"client_secret": "s"})
	s.Exec(`UPDATE sso_provider SET issuer = $1 WHERE id = 'campus'`, issuer)
	r := test(m{"provider_id": "campus"})
	if !refused(r) || !strings.HasPrefix(r.Problems[0], "issuer: ") || r.DiscoveryURL != "" {
		t.Fatalf("a provider set up on this machine: %+v", r)
	}
	if n := fetched.Load(); n != 0 {
		t.Fatalf("fetched %d times", n)
	}
	s.fails(s.admin, "sso.update", m{"provider_id": "campus", "version": 1, "issuer": issuer}, apperr.InvalidArgument,
		sso.ReasonAddressNotAllowed)

	if r := test(m{"provider_id": s.op.ID}); !r.OK || len(r.SigningKeys) != 1 || fetched.Load() != 2 {
		t.Fatalf("the operator's provider: %+v", r)
	}
}
