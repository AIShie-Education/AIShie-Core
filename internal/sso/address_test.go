package sso

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
)

// A provider of the site's is reached at a public address only: not a
// private network's, this machine's, a link's, nor one no host has, however
// it is written.
func TestOnlyAPublicAddressIsPublic(t *testing.T) {
	for _, a := range []string{
		"8.8.8.8", "1.1.1.1", "203.0.114.1", "100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.0", "192.169.0.1",
		"2001:4860:4860::8888", "2606:4700:4700::1111", "2400:cb00::1",
		"::ffff:8.8.8.8",   // mapped
		"64:ff9b::808:808", // NAT64 of 8.8.8.8, as an IPv6-only server's DNS64 gives it
		"2002:808:808::1",  // 6to4 of 8.8.8.8
		"2001:4860:4860::8888%eth0",
	} {
		if !Public(netip.MustParseAddr(a)) {
			t.Errorf("%s is not public", a)
		}
	}
	for _, a := range []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "10.255.255.255", "100.64.0.1", "100.100.100.200", "127.0.0.1", "127.255.255.254",
		"169.254.169.254", "169.254.0.1", "172.16.0.1", "172.31.255.255", "192.0.0.192", "192.0.2.1", "192.168.1.1",
		"198.18.0.1", "198.51.100.7", "203.0.113.7", "224.0.0.251", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::127.0.0.1", "fc00::1", "fd00:ec2::254", "fe80::1", "fe80::1%en0", "fec0::1", "ff02::1", "ff05::1:3",
		"100::1", "2001:db8::1", "3fff::1", "2001::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2", "64:ff9b:1::a00:1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::c0a8:101", // NAT64 of loopback, the metadata, a private network
		"2002:7f00:1::1", "2002:a9fe:a9fe::", "2002:a00:1::1", // 6to4 of the same
	} {
		if Public(netip.MustParseAddr(a)) {
			t.Errorf("%s is public", a)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("no address is public")
	}
}

// Without SSO_ALLOW_PRIVATE_ISSUERS, an issuer plainly not at a public
// address is refused as it is set up, saying why and naming the setting;
// with it, as before, http is for this machine alone.
func TestAnIssuerIsAtAPublicAddressUnlessPrivateOnesAreAllowed(t *testing.T) {
	for _, in := range []string{"https://adfs.example.edu/adfs", "https://8.8.8.8/adfs", "https://[2001:4860:4860::8888]/adfs",
		"https://localhost.example.edu/adfs", "https://示範大學.example/adfs"} {
		if got, err := CheckIssuer(in, false); err != nil || got != in {
			t.Errorf("issuer %q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"http://localhost:5556/dex", "https://localhost/adfs", "https://LOCALHOST./adfs", "https://idp.localhost/adfs",
		"http://127.0.0.1:18942/adfs", "http://[::1]:8080/realms/school", "https://10.0.0.1/adfs", "https://172.20.1.1/adfs",
		"https://192.168.0.10/adfs", "https://169.254.169.254/latest", "https://100.100.100.200/latest", "https://0.0.0.0/adfs",
		"https://[fd12:3456::1]/adfs", "https://[fe80::1%25en0]/adfs", "https://[::ffff:127.0.0.1]/adfs", "https://[64:ff9b::a9fe:a9fe]/adfs"} {
		_, err := CheckIssuer(in, false)
		e, ok := apperr.As(err)
		if !ok || e.Code != apperr.InvalidArgument || e.Details["field"] != "issuer" || e.Details["reason"] != ReasonAddressNotAllowed ||
			!strings.Contains(e.Message, "SSO_ALLOW_PRIVATE_ISSUERS") {
			t.Errorf("issuer %q: %v", in, err)
		}
	}
	for _, in := range []string{"https://10.0.0.1/adfs", "https://169.254.169.254/latest", "http://localhost:5556/dex", "https://[fd12:3456::1]/adfs"} {
		if _, err := CheckIssuer(in, true); err != nil {
			t.Errorf("issuer %q, private addresses allowed: %v", in, err)
		}
	}
	// http is never taken but for this machine, which is said only where
	// this machine is taken.
	for _, private := range []bool{false, true} {
		_, err := CheckIssuer("http://10.0.0.1/adfs", private)
		if err == nil || strings.Contains(err.Error(), ReasonAddressNotAllowed) || strings.Contains(err.Error(), "this machine") != private {
			t.Errorf("http to a private network, private %v: %v", private, err)
		}
	}
}

// The client the site's providers are reached with connects to no address
// that is not public, checked on the address dialled once a name is
// resolved: a name for this machine is refused as an address is, and the
// server is never reached. Allowed private addresses, it reaches it.
func TestTheSitesClientConnectsToPublicAddressesOnly(t *testing.T) {
	var reached atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	for _, u := range []string{srv.URL, "http://localhost:" + port} {
		resp, err := NewClient(false).Get(u)
		if err == nil {
			_ = resp.Body.Close()
		}
		if !IsAddressNotAllowed(err) {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("reached %d times", n)
	}
	resp, err := NewClient(true).Get("http://localhost:" + port)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || reached.Load() != 1 {
		t.Fatalf("private addresses allowed: %d, reached %d times", resp.StatusCode, reached.Load())
	}
	// A registry's client is held to public addresses unless it is told
	// otherwise, and so is the one a nil registry gives.
	var none *Registry
	for name, c := range map[string]*http.Client{"a registry's": New(Config{}).Client(), "a nil registry's": none.Client()} {
		resp, err := c.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		if !IsAddressNotAllowed(err) {
			t.Fatalf("%s client: %v", name, err)
		}
	}
	if New(Config{}).PrivateIssuers() || !New(Config{PrivateIssuers: true}).PrivateIssuers() {
		t.Fatal("PrivateIssuers")
	}
	// It goes through no proxy (HTTPS_PROXY), which would connect to the
	// provider for it, wherever that is, while the check saw the proxy's
	// address alone. The field is what is checked: ProxyFromEnvironment
	// reads the environment once, before a test could set it.
	if tr, ok := NewClient(false).Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the site's client goes through a proxy")
	}
}

// A sign-in redeems its code, and fetches the key set its id_token is
// checked with, through the client it was discovered with: held to public
// addresses, neither reaches a server it may not. Here the client takes
// the discovery document's server, and then the token endpoint's too.
func TestASignInRedeemsAndChecksNothingOnAPrivateAddress(t *testing.T) {
	var tokenReached, keysReached atomic.Int64
	var issuer string
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		keysReached.Add(1)
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(keys.Close)
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenReached.Add(1)
		enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
		idToken := enc(map[string]string{"alg": "RS256", "kid": "k1"}) + "." +
			enc(map[string]any{"iss": issuer, "aud": "c", "sub": "yuki", "nonce": "n", "exp": time.Now().Add(time.Hour).Unix()}) + "." +
			base64.RawURLEncoding.EncodeToString([]byte("not checked: the keys are never fetched"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer", "id_token": idToken})
	}))
	t.Cleanup(token.Close)
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": token.URL + "/token", "jwks_uri": keys.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	t.Cleanup(docs.Close)
	issuer = docs.URL + "/idp"
	at := func(s *httptest.Server) netip.AddrPort { return netip.MustParseAddrPort(s.Listener.Addr().String()) }

	signIn := func(allowed ...netip.AddrPort) error {
		idp, err := auth.NewOIDC(t.Context(), auth.OIDCConfig{Name: "campus", Issuer: issuer, ClientID: "c", ClientSecret: "s",
			RedirectURL: "https://lms.example.edu/v1/auth/sso/callback",
			HTTPClient:  guardedClient(func(a netip.AddrPort) bool { return slices.Contains(allowed, a) })})
		if err != nil {
			t.Fatalf("discovery: %v", err)
		}
		_, err = idp.Exchange(t.Context(), "the-code", "n")
		return err
	}
	if err := signIn(at(docs)); !IsAddressNotAllowed(err) || tokenReached.Load() != 0 {
		t.Fatalf("the code's exchange: %v, the token endpoint reached %d times", err, tokenReached.Load())
	}
	// go-oidc says why the keys were not fetched, but does not wrap it.
	if err := signIn(at(docs), at(token)); err == nil || !strings.Contains(err.Error(), ReasonAddressNotAllowed) ||
		tokenReached.Load() != 1 || keysReached.Load() != 0 {
		t.Fatalf("the key set: %v, the token endpoint reached %d times, the key set %d", err, tokenReached.Load(), keysReached.Load())
	}
}

// Discovering a provider of the site's for a sign-in goes through the
// registry's client, and so reaches nothing on this machine.
func TestASignInDiscoversNoProviderOnAPrivateAddress(t *testing.T) {
	var reached atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	_, err := auth.NewOIDC(t.Context(), auth.OIDCConfig{Name: "campus", Issuer: "http://localhost:" + port + "/idp", ClientID: "c",
		ClientSecret: "s", RedirectURL: "https://lms.example.edu/v1/auth/sso/callback", HTTPClient: New(Config{}).Client()})
	if !IsAddressNotAllowed(err) || reached.Load() != 0 {
		t.Fatalf("discovery: %v, reached %d times", err, reached.Load())
	}
}

// sso.test says a document or key set it was refused for its address is
// on such an address, naming the reason and the setting, and never the
// address the name resolved to. Here the client takes the discovery
// document's server alone, and the key set is at a name for this machine.
func TestAReportSaysAnAddressWasRefusedWithoutSayingIt(t *testing.T) {
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig"}]}`))
	}))
	t.Cleanup(keys.Close)
	_, keysPort, _ := net.SplitHostPort(keys.Listener.Addr().String())
	var issuer string
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": issuer + "/token", "jwks_uri": "http://localhost:" + keysPort + "/keys"})
	}))
	t.Cleanup(docs.Close)
	issuer = docs.URL + "/idp"
	docsAt := netip.MustParseAddrPort(docs.Listener.Addr().String())
	client := guardedClient(func(a netip.AddrPort) bool { return a == docsAt })

	r := Inspect(context.Background(), client, true, issuer, Want{})
	if r.OK || len(r.Problems) != 1 || r.JWKSURI == nil || len(r.SigningKeys) != 0 {
		t.Fatalf("%+v", r)
	}
	p := r.Problems[0]
	if !strings.HasPrefix(p, "the key set: http://localhost:"+keysPort+"/keys is on this machine or a private") ||
		!strings.Contains(p, ReasonAddressNotAllowed) || !strings.Contains(p, "SSO_ALLOW_PRIVATE_ISSUERS") ||
		strings.Contains(p, "127.0.0.1") || strings.Contains(p, "::1") {
		t.Fatalf("the problem: %s", p)
	}
}

// A token endpoint not at a public address is a problem for a provider of
// the site's, held to public addresses: a sign-in would be refused as it
// redeemed its code, once the person had signed in at the provider.
func TestAReportSaysATokenEndpointIsNotAtAPublicAddress(t *testing.T) {
	const issuer = "https://idp.example.edu/adfs"
	token := "https://10.0.0.7/adfs/token"
	client := &http.Client{Transport: roundTrip(func(r *http.Request) *http.Response {
		rec := httptest.NewRecorder()
		switch r.URL.Path {
		case "/adfs/.well-known/openid-configuration":
			_ = json.NewEncoder(rec).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": token, "jwks_uri": issuer + "/keys"})
		case "/adfs/keys":
			_, _ = rec.WriteString(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig"}]}`)
		default:
			rec.WriteHeader(http.StatusNotFound)
		}
		return rec.Result()
	})}
	for private, want := range map[bool]int{false: 1, true: 0} {
		r := Inspect(context.Background(), client, private, issuer, Want{})
		if len(r.Problems) != want || want == 1 && !strings.Contains(r.Problems[0], "its token_endpoint, \""+token+"\", is on this machine") {
			t.Fatalf("private %v: %+v", private, r)
		}
	}

	// A token endpoint named is resolved, as it is not fetched, and is a
	// problem when none of its addresses is public; the addresses are not
	// said. One that does not resolve is left to the sign-in.
	t.Cleanup(func() { lookup = net.DefaultResolver.LookupNetIP })
	for _, c := range []struct {
		addrs   []string
		err     error
		problem bool
	}{
		{addrs: []string{"10.0.0.7", "fd00::7"}, problem: true},
		{addrs: []string{"198.18.6.49"}, problem: true}, // what a transparent proxy's DNS gives every name
		{addrs: []string{"10.0.0.7", "8.8.8.8"}},
		{err: &net.DNSError{Err: "no such host", Name: "token.example.edu", IsNotFound: true}},
	} {
		lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
			if network != "ip" || host != "token.example.edu" {
				t.Fatalf("looked up %s %s", network, host)
			}
			var out []netip.Addr
			for _, a := range c.addrs {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, c.err
		}
		token = "https://token.example.edu/adfs/token"
		r := Inspect(context.Background(), client, false, issuer, Want{})
		if got := len(r.Problems) == 1 && strings.Contains(r.Problems[0], "its token_endpoint, \""+token+"\", is on this machine") &&
			strings.Contains(r.Problems[0], ReasonAddressNotAllowed) && !strings.Contains(r.Problems[0], c.addrs[0]); got != c.problem ||
			!c.problem && len(r.Problems) != 0 {
			t.Fatalf("%v %v: %+v", c.addrs, c.err, r)
		}
		if r := Inspect(context.Background(), client, true, issuer, Want{}); len(r.Problems) != 0 {
			t.Fatalf("private addresses allowed, %v: %+v", c.addrs, r)
		}
	}
}

type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

// A provider of the site's, held to public addresses, whose token endpoint
// or key set is at no public address is refused as it is discovered, by
// the rule sso.test reports it by: a sign-in through it is refused as it
// starts, rather than once the person has signed in at the provider and
// comes back. The refusal names the endpoint and the setting, never the
// address its name resolved to. A name with a public address among its
// addresses is taken, as the connection goes on to it, and one that does
// not resolve is left to the sign-in.
func TestAProvidersEndpointsAreAtPublicAddresses(t *testing.T) {
	t.Cleanup(func() { lookup = net.DefaultResolver.LookupNetIP })
	answers := map[string][]string{}
	lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		a, ok := answers[host]
		if network != "ip" || !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []netip.Addr
		for _, s := range a {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
	const (
		publicToken = "https://idp.example.edu/adfs/token"
		publicKeys  = "https://idp.example.edu/adfs/keys"
	)
	answers["idp.example.edu"] = []string{"8.8.8.8"}
	answers["token.example.edu"] = []string{"10.0.0.7", "fd00::7"}
	answers["keys.example.edu"] = []string{"169.254.169.254"}
	answers["fake-ip.example.edu"] = []string{"198.18.6.49"}
	answers["mixed.example.edu"] = []string{"10.0.0.7", "8.8.8.8"}
	for _, c := range []struct {
		token, keys string
		refused     string // the endpoint refused, or none
	}{
		{token: publicToken, keys: publicKeys},
		{token: "https://mixed.example.edu/token", keys: publicKeys},
		{token: "https://nxdomain.example.edu/token", keys: publicKeys},
		{token: publicToken, keys: ""}, // none named: go-oidc says so as it checks a signature
		{token: "https://10.0.0.7/adfs/token", keys: publicKeys, refused: "token_endpoint"},
		{token: "https://token.example.edu/adfs/token", keys: publicKeys, refused: "token_endpoint"},
		{token: "https://fake-ip.example.edu/adfs/token", keys: publicKeys, refused: "token_endpoint"},
		{token: "https://[::ffff:127.0.0.1]/token", keys: publicKeys, refused: "token_endpoint"},
		{token: publicToken, keys: "https://keys.example.edu/adfs/keys", refused: "jwks_uri"},
		{token: publicToken, keys: "http://localhost:8080/keys", refused: "jwks_uri"},
		{token: "https://token.example.edu/adfs/token", keys: "https://keys.example.edu/adfs/keys", refused: "token_endpoint"},
	} {
		err := CheckEndpoints(t.Context(), c.token, c.keys)
		if c.refused == "" {
			if err != nil {
				t.Errorf("%s, %s: %v", c.token, c.keys, err)
			}
			continue
		}
		at := map[string]string{"token_endpoint": c.token, "jwks_uri": c.keys}[c.refused]
		if !IsAddressNotAllowed(err) || !strings.HasPrefix(err.Error(), "its "+c.refused+", \""+at+"\", is on this machine or a private") ||
			!strings.Contains(err.Error(), "SSO_ALLOW_PRIVATE_ISSUERS") || slices.ContainsFunc([]string{"10.0.0.7", "fd00::7", "169.254", "198.18"},
			func(a string) bool { return !strings.Contains(at, a) && strings.Contains(err.Error(), a) }) {
			t.Errorf("%s, %s: %v, want %s refused", c.token, c.keys, err, c.refused)
		}
	}

	// As a sign-in discovers the provider: refused when its token endpoint
	// or its key set resolves privately, taken when both resolve publicly,
	// and taken whatever they resolve to with no check, as for the
	// operator's provider or with private addresses allowed.
	var issuer, token, keys string
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": token, "jwks_uri": keys, "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	t.Cleanup(docs.Close)
	issuer = docs.URL + "/idp"
	discover := func(check func(context.Context, string, string) error) error {
		_, err := auth.NewOIDC(t.Context(), auth.OIDCConfig{Name: "campus", Issuer: issuer, ClientID: "c", ClientSecret: "s",
			RedirectURL: "https://lms.example.edu/v1/auth/sso/callback", HTTPClient: docs.Client(), CheckEndpoints: check})
		return err
	}
	token, keys = "https://token.example.edu/adfs/token", publicKeys
	if err := discover(CheckEndpoints); !IsAddressNotAllowed(err) || !strings.Contains(err.Error(), "its token_endpoint") {
		t.Fatalf("a token endpoint on a private network: %v", err)
	}
	if err := discover(nil); err != nil {
		t.Fatalf("not checked: %v", err)
	}
	token, keys = publicToken, "https://keys.example.edu/adfs/keys"
	if err := discover(CheckEndpoints); !IsAddressNotAllowed(err) || !strings.Contains(err.Error(), "its jwks_uri") {
		t.Fatalf("a key set on a private network: %v", err)
	}
	if err := discover(nil); err != nil {
		t.Fatalf("not checked: %v", err)
	}
	keys = publicKeys
	if err := discover(CheckEndpoints); err != nil {
		t.Fatalf("a public token endpoint and key set: %v", err)
	}

	// The registry checks them unless private addresses are allowed.
	row := dbq.ListEnabledSSOProvidersRow{ID: "campus", Issuer: "https://idp.example.edu/adfs", ClientID: "c", SubjectClaim: "sub"}
	if New(Config{}).oidcConfig(row, "s").CheckEndpoints == nil {
		t.Fatal("a registry held to public addresses does not check a provider's endpoints")
	}
	if New(Config{PrivateIssuers: true}).oidcConfig(row, "s").CheckEndpoints != nil {
		t.Fatal("a registry allowed private addresses checks a provider's endpoints")
	}
}

// A provider of the site's whose issuer is plainly not at a public address,
// set up before the server was held to public ones or while it allowed
// private ones, is issuer_address_not_allowed to administrators while it is
// held to them, and offered when it is not; its name is not resolved.
func TestAProviderAtAPrivateAddressIsNotOffered(t *testing.T) {
	k := make([]byte, secrets.KeySize)
	_, _ = rand.Read(k)
	keys, err := secrets.NewKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keys.Seal(SecretBinding("campus"), "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lookup = net.DefaultResolver.LookupNetIP })
	lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("a provider's status resolved its issuer")
		return nil, nil
	}
	held, allowed := New(Config{Keys: keys}), New(Config{Keys: keys, PrivateIssuers: true})
	for _, issuer := range []string{"https://10.0.0.7/adfs", "http://127.0.0.1:5556/dex", "https://localhost/adfs", "https://[fd00:ec2::254]/idp"} {
		if got := held.SiteStatus("campus", issuer, true, sealed); got != StatusAddressNotAllowed {
			t.Errorf("%s: %s", issuer, got)
		}
		if got := held.SiteStatus("campus", issuer, false, sealed); got != StatusDisabled {
			t.Errorf("%s, switched off: %s", issuer, got)
		}
		if got := allowed.SiteStatus("campus", issuer, true, sealed); got != StatusOffered {
			t.Errorf("%s, private addresses allowed: %s", issuer, got)
		}
	}
	if got := held.SiteStatus("campus", "https://adfs.example.edu/adfs", true, sealed); got != StatusOffered {
		t.Errorf("a name: %s", got)
	}
}
