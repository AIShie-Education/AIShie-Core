package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// Report is what sso.test found at an issuer: its discovery document, what
// it says, and the keys it signs with, checked as a sign-in would use them.
// It signs nobody in and sends no secret: it reads two public documents.
type Report struct {
	// OK: nothing found stops a sign-in through the provider. Problems says
	// what does, Warnings what may.
	OK       bool     `json:"ok"`
	Issuer   string   `json:"issuer"`
	Problems []string `json:"problems"`
	Warnings []string `json:"warnings"`
	// DiscoveryURL is where the document was read: the issuer and
	// /.well-known/openid-configuration.
	DiscoveryURL string `json:"discovery_url"`
	// What the document says, null or empty where it says nothing.
	AuthorizationEndpoint *string `json:"authorization_endpoint"`
	TokenEndpoint         *string `json:"token_endpoint"`
	UserinfoEndpoint      *string `json:"userinfo_endpoint"`
	EndSessionEndpoint    *string `json:"end_session_endpoint"`
	JWKSURI               *string `json:"jwks_uri"`
	// SigningKeys are the keys at jwks_uri that sign id_tokens.
	SigningKeys                  []FoundKey `json:"signing_keys"`
	SigningAlgorithms            []string   `json:"signing_algorithms"`
	ScopesSupported              []string   `json:"scopes_supported"`
	ClaimsSupported              []string   `json:"claims_supported"`
	ResponseTypesSupported       []string   `json:"response_types_supported"`
	GrantTypesSupported          []string   `json:"grant_types_supported"`
	TokenEndpointAuthMethods     []string   `json:"token_endpoint_auth_methods"`
	SubjectTypesSupported        []string   `json:"subject_types_supported"`
	CodeChallengeMethods         []string   `json:"code_challenge_methods"`
	RequestedScopesUnsupported   []string   `json:"requested_scopes_unsupported"`
	RequestedClaimsNotAdvertised []string   `json:"requested_claims_not_advertised"`
}

// FoundKey is one key of a provider's key set, as it describes itself.
type FoundKey struct {
	KeyID     *string `json:"kid"`
	Type      string  `json:"kty"`
	Algorithm *string `json:"alg"`
	Use       *string `json:"use"`
}

// Want is what a sign-in through the provider would ask of it.
type Want struct {
	Scopes       []string
	SubjectClaim string
	EmailClaim   string
}

// What a sign-in's id_token may be signed with: what the verifier takes.
var verifiable = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA"}

// Bounds of what is read from a provider, and of what of it is said back.
const (
	maxDocument = 1 << 20
	maxListed   = 100
	maxString   = 500
)

type discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	Algorithms            []string `json:"id_token_signing_alg_values_supported"`
	Scopes                []string `json:"scopes_supported"`
	Claims                []string `json:"claims_supported"`
	ResponseTypes         []string `json:"response_types_supported"`
	GrantTypes            []string `json:"grant_types_supported"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
	SubjectTypes          []string `json:"subject_types_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// Inspect reads issuer's discovery document and its key set with client,
// which follows no redirect, and says what it found and what of it would
// stop, or may trouble, a sign-in asking for want.
func Inspect(ctx context.Context, client *http.Client, issuer string, want Want) Report {
	rep := Report{Issuer: issuer, Problems: []string{}, Warnings: []string{}, SigningKeys: []FoundKey{},
		SigningAlgorithms: []string{}, ScopesSupported: []string{}, ClaimsSupported: []string{}, ResponseTypesSupported: []string{},
		GrantTypesSupported: []string{}, TokenEndpointAuthMethods: []string{}, SubjectTypesSupported: []string{},
		CodeChallengeMethods: []string{}, RequestedScopesUnsupported: []string{}, RequestedClaimsNotAdvertised: []string{}}
	finish := func() Report {
		rep.OK = len(rep.Problems) == 0
		return rep
	}
	if _, err := CheckIssuer(issuer); err != nil {
		rep.Problems = append(rep.Problems, describe(err))
		return finish()
	}
	rep.DiscoveryURL = strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	var doc discovery
	if err := fetchJSON(ctx, client, rep.DiscoveryURL, &doc); err != nil {
		rep.Problems = append(rep.Problems, "the discovery document: "+err.Error())
		return finish()
	}

	rep.AuthorizationEndpoint, rep.TokenEndpoint = shown(doc.AuthorizationEndpoint), shown(doc.TokenEndpoint)
	rep.UserinfoEndpoint, rep.EndSessionEndpoint, rep.JWKSURI = shown(doc.UserinfoEndpoint), shown(doc.EndSessionEndpoint), shown(doc.JWKSURI)
	rep.SigningAlgorithms, rep.ScopesSupported, rep.ClaimsSupported = listed(doc.Algorithms), listed(doc.Scopes), listed(doc.Claims)
	rep.ResponseTypesSupported, rep.GrantTypesSupported = listed(doc.ResponseTypes), listed(doc.GrantTypes)
	rep.TokenEndpointAuthMethods, rep.SubjectTypesSupported = listed(doc.AuthMethods), listed(doc.SubjectTypes)
	rep.CodeChallengeMethods = listed(doc.CodeChallengeMethods)

	if doc.Issuer != issuer {
		rep.Problems = append(rep.Problems, fmt.Sprintf("the document names its issuer %q, not %q: the issuer is written exactly as the provider writes it, to the last slash",
			clip(doc.Issuer), issuer))
	}
	for name, v := range map[string]string{"authorization_endpoint": doc.AuthorizationEndpoint, "token_endpoint": doc.TokenEndpoint, "jwks_uri": doc.JWKSURI} {
		if v == "" {
			rep.Problems = append(rep.Problems, "the document has no "+name)
		} else if !webURL(v) {
			rep.Problems = append(rep.Problems, fmt.Sprintf("its %s, %q, is not an http or https URL", name, clip(v)))
		}
	}
	if len(doc.ResponseTypes) > 0 && !slices.Contains(doc.ResponseTypes, "code") {
		rep.Problems = append(rep.Problems, "it takes no response_type code, which a sign-in here asks for")
	}
	if len(doc.GrantTypes) > 0 && !slices.Contains(doc.GrantTypes, "authorization_code") {
		rep.Problems = append(rep.Problems, "it grants no authorization_code, which a sign-in here exchanges")
	}
	if len(doc.Algorithms) > 0 && !slices.ContainsFunc(doc.Algorithms, func(a string) bool { return slices.Contains(verifiable, a) }) {
		rep.Problems = append(rep.Problems, "it signs id_tokens with none of "+strings.Join(verifiable, ", "))
	}
	if strings.HasPrefix(doc.TokenEndpoint, "http://") && !loopbackURL(doc.TokenEndpoint) {
		rep.Problems = append(rep.Problems, "its token_endpoint is http: the client secret would cross the network in the clear")
	}
	for _, s := range want.Scopes {
		if len(doc.Scopes) > 0 && !slices.Contains(doc.Scopes, s) {
			rep.RequestedScopesUnsupported = append(rep.RequestedScopesUnsupported, s)
		}
	}
	if len(rep.RequestedScopesUnsupported) > 0 {
		rep.Warnings = append(rep.Warnings, "scopes_supported does not list "+strings.Join(rep.RequestedScopesUnsupported, ", ")+
			"; a provider may refuse a sign-in that asks for a scope it does not support")
	}
	for _, c := range []string{want.SubjectClaim, want.EmailClaim} {
		if c != "" && len(doc.Claims) > 0 && !slices.Contains(doc.Claims, c) {
			rep.RequestedClaimsNotAdvertised = append(rep.RequestedClaimsNotAdvertised, c)
		}
	}
	if len(rep.RequestedClaimsNotAdvertised) > 0 {
		rep.Warnings = append(rep.Warnings, "claims_supported does not list "+strings.Join(rep.RequestedClaimsNotAdvertised, ", ")+
			"; an id_token without the subject claim signs nobody in, and one without the email claim links nobody by email")
	}
	if len(doc.Scopes) == 0 {
		rep.Warnings = append(rep.Warnings, "the document lists no scopes_supported")
	}
	if len(doc.Claims) == 0 {
		rep.Warnings = append(rep.Warnings, "the document lists no claims_supported: whether its id_tokens carry the subject claim is found at the first sign-in")
	}

	if doc.JWKSURI != "" && webURL(doc.JWKSURI) {
		var set struct {
			Keys []struct {
				KeyID string `json:"kid"`
				Type  string `json:"kty"`
				Alg   string `json:"alg"`
				Use   string `json:"use"`
			} `json:"keys"`
		}
		if err := fetchJSON(ctx, client, doc.JWKSURI, &set); err != nil {
			rep.Problems = append(rep.Problems, "the key set: "+err.Error())
		} else {
			for _, k := range set.Keys {
				if k.Use != "" && k.Use != "sig" {
					continue
				}
				if len(rep.SigningKeys) < maxListed {
					rep.SigningKeys = append(rep.SigningKeys, FoundKey{KeyID: shown(k.KeyID), Type: clip(k.Type), Algorithm: shown(k.Alg), Use: shown(k.Use)})
				}
			}
			if len(rep.SigningKeys) == 0 {
				rep.Problems = append(rep.Problems, "the key set at jwks_uri has no key to check a signature with")
			}
		}
	}
	return finish()
}

// fetchJSON reads the JSON document at u into v, following no redirect and
// reading no more than maxDocument bytes. Its errors say what went wrong
// with the document, and nothing of what it said.
func fetchJSON(ctx context.Context, client *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("its URL is not one to fetch")
	}
	req.Header.Set("Accept", "application/json")
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return fmt.Errorf("%s did not answer in time", clip(u))
		}
		return fmt.Errorf("%s could not be reached", clip(u))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("%s redirects (HTTP %d) to %q: give the URL it redirects to, if that is the provider's", clip(u), resp.StatusCode,
			clip(resp.Header.Get("Location")))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s answered HTTP %d", clip(u), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	switch {
	case err != nil:
		return fmt.Errorf("%s could not be read", clip(u))
	case len(body) > maxDocument:
		return fmt.Errorf("%s is larger than %d bytes", clip(u), maxDocument)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s is not the JSON object it should be", clip(u))
	}
	return nil
}

func webURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func loopbackURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && Loopback(u.Hostname())
}

// clip holds what a provider said to maxString bytes, whole characters.
func clip(s string) string {
	if len(s) <= maxString {
		return s
	}
	cut := maxString
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func shown(s string) *string {
	if s == "" {
		return nil
	}
	s = clip(s)
	return &s
}

// listed is what a provider listed, as much of it as is said back.
func listed(v []string) []string {
	out := []string{}
	for _, s := range v {
		if len(out) == maxListed {
			break
		}
		out = append(out, clip(s))
	}
	return out
}
