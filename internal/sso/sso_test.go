package sso

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/config"
)

func TestSettingsAreHeldToTheirRules(t *testing.T) {
	for _, id := range []string{"a", "school-adfs", "university-sso", "g2", strings.Repeat("a", 64), config.DefaultOIDCProviderName} {
		if err := CheckID(id); err != nil {
			t.Errorf("id %q: %v", id, err)
		}
	}
	for _, id := range []string{"", "-a", "a-", "A", "a_b", "a.b", "a/b", "a b", strings.Repeat("a", 65), "雲"} {
		if CheckID(id) == nil {
			t.Errorf("id %q taken", id)
		}
	}
	for in, want := range map[string]string{
		"https://adfs.example.edu/adfs":     "https://adfs.example.edu/adfs",
		" https://accounts.google.com ":     "https://accounts.google.com",
		"https://login.example.edu/tenant/": "https://login.example.edu/tenant/",
		"http://localhost:5556/dex":         "http://localhost:5556/dex",
		"http://127.0.0.1:18942/adfs":       "http://127.0.0.1:18942/adfs",
		"http://[::1]:8080/realms/school":   "http://[::1]:8080/realms/school",
		"https://示範大學.example/adfs":         "https://示範大學.example/adfs",
	} {
		if got, err := CheckIssuer(in, true); err != nil || got != want {
			t.Errorf("issuer %q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "adfs.example.edu", "ftp://adfs.example.edu", "http://adfs.example.edu", "http://10.0.0.1/adfs",
		"https://user:pw@adfs.example.edu", "https://adfs.example.edu/?a=b", "https://adfs.example.edu/#x", "https:///adfs",
		"https://adfs.example.edu/a b", "https://" + strings.Repeat("a", 500) + ".edu"} {
		if _, err := CheckIssuer(in, true); err == nil {
			t.Errorf("issuer %q taken", in)
		} else if e, ok := apperr.As(err); !ok || e.Details["field"] != "issuer" {
			t.Errorf("issuer %q: %v", in, err)
		}
	}
	if got, err := CheckScopes(nil); err != nil || strings.Join(got, " ") != "openid profile email" {
		t.Errorf("no scopes: %v %v", got, err)
	}
	if got, err := CheckScopes([]string{"openid", " groups ", "openid", "urn:school:roles"}); err != nil ||
		strings.Join(got, " ") != "openid groups urn:school:roles" {
		t.Errorf("scopes: %v %v", got, err)
	}
	for _, bad := range [][]string{{"profile"}, {"openid", ""}, {"openid", "a b"}, {"openid", `a\b`}, {"openid", "雲"}} {
		if _, err := CheckScopes(bad); err == nil {
			t.Errorf("scopes %q taken", bad)
		}
	}
	if got, err := CheckDomains([]string{" @Campus.Example.edu", "campus.example.edu", "students.example.edu"}); err != nil ||
		strings.Join(got, " ") != "campus.example.edu students.example.edu" {
		t.Errorf("domains: %v %v", got, err)
	}
	for _, bad := range []string{"campus", "-a.edu", "a..edu", "a.edu.", "a_b.edu", "campus.example.edu/x"} {
		if _, err := CheckDomains([]string{bad}); err == nil {
			t.Errorf("domain %q taken", bad)
		}
	}
	for email, in := range map[string]bool{"a@campus.example.edu": true, "A@CAMPUS.EXAMPLE.EDU": true, "a@x.campus.example.edu": false,
		"a@campus.example.edu.evil.example": false, "@campus.example.edu": false, "campus.example.edu": false, "a@": false} {
		if InDomains(email, []string{"campus.example.edu"}) != in {
			t.Errorf("InDomains(%q) != %v", email, in)
		}
	}
	for _, s := range []string{"s3cret", "with spaces inside", "~!@#$%^&*()"} {
		if _, err := CheckClientSecret(s); err != nil {
			t.Errorf("secret %q: %v", s, err)
		}
	}
	for _, s := range []string{"", "   ", "tab\tinside", "秘密", strings.Repeat("s", 501)} {
		_, err := CheckClientSecret(s)
		if err == nil || (s != "" && strings.TrimSpace(s) != "" && strings.Contains(err.Error(), s)) {
			t.Errorf("secret %q: %v", s, err)
		}
	}
	if _, err := CheckDisplayName("Sch\u200bool"); err == nil {
		t.Error("a zero-width space taken")
	}
	if got, err := CheckDisplayName("  示範大學 NetID "); err != nil || got != "示範大學 NetID" {
		t.Errorf("display name: %q %v", got, err)
	}
}

// The operator's provider, alone: offered, found by its id, and nothing else
// is.
func TestTheOperatorsProviderAlone(t *testing.T) {
	var none *Registry
	if none.Operator() != nil || none.Keys() != nil || none.RedirectURL() != "http://localhost/v1/auth/sso/callback" {
		t.Fatal("a nil registry")
	}
	if offered, err := none.Offered(t.Context()); err != nil || len(offered) != 0 {
		t.Fatalf("a nil registry offers %v %v", offered, err)
	}
	r := New(Config{Operator: &Operator{ID: "school-adfs", DisplayName: "School NetID"}, PublicURL: "https://lms.example.edu/"})
	if r.RedirectURL() != "https://lms.example.edu/v1/auth/sso/callback" {
		t.Fatalf("redirect: %s", r.RedirectURL())
	}
	offered, err := r.Offered(t.Context())
	if err != nil || len(offered) != 1 || offered[0].ID != "school-adfs" || *offered[0].Label != "School NetID" || offered[0].Source != SourceOperator {
		t.Fatalf("offered: %+v %v", offered, err)
	}
	if p, err := r.Resolve(t.Context(), "school-adfs"); err != nil || p.Source != SourceOperator || p.LinkByEmail {
		t.Fatalf("resolve: %+v %v", p, err)
	}
	if _, err := r.Resolve(t.Context(), "google"); !errors.Is(err, ErrNotOffered) {
		t.Fatalf("resolve another: %v", err)
	}
	if got := r.SiteStatus("school-adfs", true, "v1.x"); got != StatusIDTaken {
		t.Fatalf("a site's provider with its id: %s", got)
	}
	if got := r.SiteStatus("google", true, "v1.x"); got != StatusSecretUnavailable {
		t.Fatalf("a site's provider with no key to open it: %s", got)
	}
	if got := r.SiteStatus("google", false, "v1.x"); got != StatusDisabled {
		t.Fatalf("a site's provider switched off: %s", got)
	}
}

type stubIdP struct{ n int64 }

func (stubIdP) Name() string                      { return "stub" }
func (stubIdP) AuthCodeURL(string, string) string { return "" }
func (stubIdP) Exchange(context.Context, string, string) (auth.Identity, error) {
	return auth.Identity{}, nil
}

// What was read of a provider over the network is kept while its settings
// are the same and not for long; a lecture hall starting to sign in at once
// reads it once; a change, or an hour, reads it again; a failure is not kept.
func TestAProviderIsDiscoveredOnceForItsSettings(t *testing.T) {
	var calls atomic.Int64
	var fail atomic.Bool
	release := make(chan struct{})
	r := New(Config{Rediscover: time.Hour, Discover: func(ctx context.Context, cfg auth.OIDCConfig) (auth.IdentityProvider, error) {
		n := calls.Add(1)
		<-release
		if fail.Load() {
			return nil, errors.New("unreachable")
		}
		return stubIdP{n: n}, nil
	}})
	cfg := auth.OIDCConfig{Name: "campus", Issuer: "https://idp.example.edu", ClientID: "c", ClientSecret: "s", Scopes: []string{"openid"}}

	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			if _, err := r.discover(t.Context(), cfg); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("forty sign-ins discovered it %d times", n)
	}
	changed := cfg
	changed.ClientSecret = "another"
	if _, err := r.discover(t.Context(), changed); err != nil || calls.Load() != 2 {
		t.Fatalf("a new secret: %d %v", calls.Load(), err)
	}
	if _, err := r.discover(t.Context(), changed); err != nil || calls.Load() != 2 {
		t.Fatalf("the same again: %d %v", calls.Load(), err)
	}
	r.mu.Lock()
	r.cache["campus"].at = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()
	fail.Store(true)
	if _, err := r.discover(t.Context(), changed); err == nil || calls.Load() != 3 {
		t.Fatalf("an hour on, unreachable: %d %v", calls.Load(), err)
	}
	fail.Store(false)
	if _, err := r.discover(t.Context(), changed); err != nil || calls.Load() != 4 {
		t.Fatalf("a failure is not kept: %d %v", calls.Load(), err)
	}
}
