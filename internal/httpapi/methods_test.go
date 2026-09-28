package httpapi_test

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ratelimit"
)

// The sign-in page asks how a person may sign in here, and is told whether
// to offer single sign-on and what to call it, and nothing else about the
// provider.
func TestTheSignInPageIsToldHowToSignIn(t *testing.T) {
	start := "/v1/auth/sso/start"
	for _, tc := range []struct {
		name  string
		sso   bool
		label string
		want  m
	}{
		{"password alone", false, "", m{"password": true, "sso": nil}},
		{"single sign-on, in the front end's own words", true, "", m{"password": true, "sso": m{"label": nil, "start": start}}},
		{"single sign-on, named", true, "PolyU NetID", m{"password": true, "sso": m{"label": "PolyU NetID", "start": start}}},
		{"single sign-on, named in any script and with what HTML makes much of", true, `理大 <NetID> & "SSO"`,
			m{"password": true, "sso": m{"label": `理大 <NetID> & "SSO"`, "start": start}}},
		{"a name, with single sign-on off", false, "PolyU NetID", m{"password": true, "sso": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label := func(d *httpapi.Deps) { d.SSOLabel = tc.label; d.TrustedOrigins = []string{frontEnd} }
			var a *api
			var private []string
			if tc.sso {
				s := newSSOWith(t, nil, label)
				a, private = s.api, []string{s.idp.issuer(), s.idp.srv.URL, clientID, clientSecret, "openid", "polyu-adfs", "upn"}
			} else {
				a = hardenedWith(t, nil, nil, nil, label)
			}
			r := a.do(nil, "GET", httpapi.MethodsPath, "", nil)
			if r.Status != http.StatusOK || !reflect.DeepEqual(r.Body, tc.want) {
				t.Fatalf("%d %s, want %v", r.Status, r.Raw, tc.want)
			}
			if r.Header.Get("Cache-Control") != "public, max-age=60" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
				!slices.Equal(r.Header.Values("Vary"), []string{"Origin"}) {
				t.Fatalf("headers: %v", r.Header)
			}
			for _, p := range private {
				if strings.Contains(r.Raw, p) {
					t.Fatalf("the answer says %q of the provider: %s", p, r.Raw)
				}
			}
			// Where it says to start is where a sign-in starts.
			if tc.sso {
				s := a.do(browser(), "GET", r.str("sso", "start")+"?return_to=/courses", "", nil)
				if s.Status != http.StatusFound || !strings.Contains(s.Header.Get("Location"), "/oauth2/authorize?") {
					t.Fatalf("start: %d %v", s.Status, s.Header)
				}
			}
		})
	}
}

// Anyone may ask, signed in or not, and from any page; the front end's own
// origin may read the answer from another origin, cookies and all. The
// answer is the same whoever asks, and says it varies with the origin even
// when no origin was sent, so that a cache that kept it for one never gives
// it to the other.
func TestAnyoneMayAskHowToSignIn(t *testing.T) {
	s := newSSOWith(t, nil, func(d *httpapi.Deps) { d.SSOLabel = "PolyU NetID" })
	want := m{"password": true, "sso": m{"label": "PolyU NetID", "start": "/v1/auth/sso/start"}}
	for _, tc := range []struct {
		name    string
		token   string
		headers []string
		cors    bool
	}{
		{"with no credential", "", nil, false},
		{"with a token that is no good", "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43), nil, false},
		{"with a session cookie that is no good", "", []string{"Cookie", httpapi.SessionCookie + "=ais_stale"}, false},
		{"from the front end, on another origin", "", []string{"Origin", frontEnd, "Sec-Fetch-Site", "cross-site"}, true},
		{"from a page on another site", "", []string{"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s.do(nil, "GET", httpapi.MethodsPath, tc.token, nil, tc.headers...)
			if r.Status != http.StatusOK || !reflect.DeepEqual(r.Body, want) || !slices.Equal(r.Header.Values("Vary"), []string{"Origin"}) {
				t.Fatalf("%d %v %s", r.Status, r.Header, r.Raw)
			}
			allowed := r.Header.Get("Access-Control-Allow-Origin") == frontEnd && r.Header.Get("Access-Control-Allow-Credentials") == "true"
			if allowed != tc.cors || (!tc.cors && r.Header.Get("Access-Control-Allow-Origin") != "") {
				t.Fatalf("CORS headers: %v", r.Header)
			}
		})
	}
	if r := s.do(nil, "POST", httpapi.MethodsPath, "", nil); r.Status != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d %s", r.Status, r.Raw)
	}
}

// A lecture hall behind one address opens the sign-in page at once. Asking
// how to sign in costs nothing, so it is held to no limit, and spends none
// of the sign-in attempts the address is allowed.
func TestAskingHowToSignInSpendsNoSignInAttempts(t *testing.T) {
	a := hardened(t, ratelimit.New(1, 1), ratelimit.New(1, 3), nil) // one a minute: nothing refills while the test runs
	for i := range 40 {
		if r := a.do(nil, "GET", httpapi.MethodsPath, "", nil); r.Status != http.StatusOK {
			t.Fatalf("student %d of forty: %d %s", i+1, r.Status, r.Raw)
		}
	}
	for i := range 3 {
		if r := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "nobody@example.edu", "password": "not the password!"}); r.Status != http.StatusUnauthorized {
			t.Fatalf("sign-in %d of the three the address is allowed: %d %s", i+1, r.Status, r.Raw)
		}
	}
}
