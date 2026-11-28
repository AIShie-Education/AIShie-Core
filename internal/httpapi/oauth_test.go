package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/oauth"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

const oauthIssuer = "https://lms.example.edu"

type oauthHTTP struct {
	*api
	service *oauth.Service
	agent   uuid.UUID
	client  tools.OAuthClientSecretOut
}

func newOAuthHTTP(t *testing.T, limit *ratelimit.Limiter, log *slog.Logger) *oauthHTTP {
	t.Helper()
	var service *oauth.Service
	a := hardenedWith(t, nil, nil, log, func(d *httpapi.Deps) {
		service = oauth.NewService(d.Pool)
		d.OAuth, d.PublicURL, d.OAuthTokenLimit = service, oauthIssuer, limit
	})
	agent := a.c.OwnedAgent(a.c.Sato, "External MCP")
	created := testkit.Result[tools.OAuthClientSecretOut](t, a.c.MustCall(a.c.Sato, "oauth_client.create",
		m{"agent_id": agent, "label": "HTTP client", "scopes": []string{"mcp.read", "mcp.write"}}, "http-oauth-create"))
	return &oauthHTTP{api: a, service: service, agent: agent, client: created}
}

func (f *oauthHTTP) token(t *testing.T, basic bool, id, secret, body string) (rawResponse, map[string]any, string) {
	t.Helper()
	var headers []string
	if basic {
		headers = []string{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(id+":"+secret))}
	}
	res, raw := f.raw("POST", f.srv.URL+"/v1/oauth/token", "application/x-www-form-urlencoded", []byte(body), headers...)
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("token response is not JSON: %s", raw)
		}
	}
	return res, decoded, string(raw)
}

func TestOAuthMetadataAndNoAnonymousRegistration(t *testing.T) {
	f := newOAuthHTTP(t, nil, nil)
	authz := f.do(nil, "GET", "/.well-known/oauth-authorization-server", "", nil)
	if authz.Status != 200 || authz.str("issuer") != oauthIssuer || authz.str("token_endpoint") != oauthIssuer+"/v1/oauth/token" ||
		strings.Contains(authz.Raw, "registration_endpoint") || strings.Contains(authz.Raw, "authorization_endpoint") ||
		!strings.Contains(authz.Raw, "client_credentials") || !strings.Contains(authz.Raw, "client_secret_basic") ||
		!strings.Contains(authz.Raw, "client_secret_post") {
		t.Fatalf("authorization-server metadata: %d %s", authz.Status, authz.Raw)
	}
	resource := f.do(nil, "GET", "/.well-known/oauth-protected-resource", "", nil)
	if resource.Status != 200 || resource.str("resource") != oauthIssuer+"/mcp" ||
		!strings.Contains(resource.Raw, `"authorization_servers":["`+oauthIssuer+`"]`) || !strings.Contains(resource.Raw, "mcp.read") ||
		!strings.Contains(resource.Raw, "mcp.write") {
		t.Fatalf("protected-resource metadata: %d %s", resource.Status, resource.Raw)
	}
	if registered := f.do(nil, "POST", "/v1/oauth/register", "", m{"client_name": "anonymous"}); registered.Status != http.StatusNotFound {
		t.Fatalf("anonymous registration exists: %d %s", registered.Status, registered.Raw)
	}
}

func TestOAuthTokenClientAuthenticationResourceAndScope(t *testing.T) {
	f := newOAuthHTTP(t, nil, nil)
	id, secret := f.client.Client.ClientID.String(), f.client.ClientSecret
	res, body, raw := f.token(t, true, id, secret, "grant_type=client_credentials&resource="+oauthIssuer+"%2Fmcp")
	access, _ := body["access_token"].(string)
	if res.StatusCode != 200 || body["token_type"] != "Bearer" || body["expires_in"] != float64(600) ||
		body["scope"] != "mcp.read mcp.write" || !strings.HasPrefix(access, "aiso_") || strings.Contains(raw, "refresh_token") {
		t.Fatalf("Basic token: %d %v %s", res.StatusCode, res.Header, raw)
	}
	var hash string
	if err := f.c.Pool.QueryRow(t.Context(), `SELECT token_hash FROM oauth_access_token WHERE token_hash <> '' ORDER BY created_at DESC LIMIT 1`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, access) {
		t.Fatal("the opaque token was stored in plaintext")
	}

	// Body authentication and a blank scope both mean the client's scopes.
	form := "grant_type=client_credentials&client_id=" + id + "&client_secret=" + secret + "&scope="
	res, body, _ = f.token(t, false, "", "", form)
	if res.StatusCode != 200 || body["scope"] != "mcp.read mcp.write" {
		t.Fatalf("body client auth with blank scope: %d %v", res.StatusCode, body)
	}
	form = "grant_type=client_credentials&client_id=" + id + "&client_secret=" + secret + "&scope=mcp.read"
	res, body, _ = f.token(t, false, "", "", form)
	if res.StatusCode != 200 || body["scope"] != "mcp.read" {
		t.Fatalf("scope subset: %d %v", res.StatusCode, body)
	}

	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"wrong grant", "grant_type=authorization_code", "unsupported_grant_type", 400},
		{"wrong resource", "grant_type=client_credentials&resource=https%3A%2F%2Felsewhere.example%2Fmcp", "invalid_grant", 400},
		{"scope escalation", "grant_type=client_credentials&scope=course.admin", "invalid_scope", 400},
		{"duplicate scope value", "grant_type=client_credentials&scope=mcp.read+mcp.read", "invalid_scope", 400},
		{"repeated scope field", "grant_type=client_credentials&scope=mcp.read&scope=mcp.write", "invalid_request", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body, raw := f.token(t, true, id, secret, tc.body)
			if res.StatusCode != tc.status || body["error"] != tc.want || strings.Contains(raw, secret) {
				t.Fatalf("%d %v %s", res.StatusCode, body, raw)
			}
		})
	}
	res, body, raw = f.token(t, true, id, "wrong-secret", "grant_type=client_credentials")
	if res.StatusCode != 401 || body["error"] != "invalid_client" || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic ") ||
		strings.Contains(raw, "wrong-secret") {
		t.Fatalf("bad secret: %d %v %s", res.StatusCode, res.Header, raw)
	}
	res, body, _ = f.token(t, false, "", "", "grant_type=client_credentials")
	if res.StatusCode != 401 || body["error"] != "invalid_client" {
		t.Fatalf("missing client: %d %v", res.StatusCode, body)
	}
	if me := f.do(nil, "GET", "/v1/me", access, nil); me.Status != 401 {
		t.Fatalf("an OAuth access token reached REST: %d %s", me.Status, me.Raw)
	}
}

func TestOAuthTokenRotationAgentOwnerRevocationExpiryAndRateLimit(t *testing.T) {
	var logs bytes.Buffer
	f := newOAuthHTTP(t, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	id, secret := f.client.Client.ClientID.String(), f.client.ClientSecret
	_, body, _ := f.token(t, true, id, secret, "grant_type=client_credentials")
	access := body["access_token"].(string)

	rotated := testkit.Result[tools.OAuthClientSecretOut](t, f.c.MustCall(f.c.Sato, "oauth_client.rotate",
		m{"agent_id": f.agent, "client_id": f.client.Client.ClientID}, "http-rotate"))
	res, body, raw := f.token(t, true, id, secret, "grant_type=client_credentials")
	if res.StatusCode != 401 || body["error"] != "invalid_client" || strings.Contains(raw, secret) {
		t.Fatalf("old secret after rotation: %d %v %s", res.StatusCode, body, raw)
	}
	if _, err := f.service.Authenticate(t.Context(), access); err != oauth.ErrInvalidToken {
		t.Fatalf("old token after rotation: %v", err)
	}
	res, body, _ = f.token(t, true, id, rotated.ClientSecret, "grant_type=client_credentials")
	if res.StatusCode != 200 {
		t.Fatalf("new secret after rotation: %d %v", res.StatusCode, body)
	}
	access = body["access_token"].(string)

	f.c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, f.c.Sato)
	if _, err := f.service.Authenticate(t.Context(), access); err != oauth.ErrInvalidToken {
		t.Fatalf("suspended owner token: %v", err)
	}
	res, body, _ = f.token(t, true, id, rotated.ClientSecret, "grant_type=client_credentials")
	if res.StatusCode != 401 || body["error"] != "invalid_client" {
		t.Fatalf("suspended owner issuing: %d %v", res.StatusCode, body)
	}
	f.c.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, f.c.Sato)

	now := time.Now()
	f.service.SetClock(func() time.Time { return now })
	res, body, _ = f.token(t, true, id, rotated.ClientSecret, "grant_type=client_credentials")
	if res.StatusCode != 200 {
		t.Fatalf("issue with test clock: %d %v", res.StatusCode, body)
	}
	access = body["access_token"].(string)
	now = now.Add(10*time.Minute + time.Nanosecond)
	if _, err := f.service.Authenticate(t.Context(), access); err != oauth.ErrInvalidToken {
		t.Fatalf("expiry: %v", err)
	}

	f.c.MustCall(f.c.Sato, "oauth_client.revoke", m{"agent_id": f.agent, "client_id": f.client.Client.ClientID}, "http-revoke")
	res, body, _ = f.token(t, true, id, rotated.ClientSecret, "grant_type=client_credentials")
	if res.StatusCode != 401 || body["error"] != "invalid_client" {
		t.Fatalf("revoked client: %d %v", res.StatusCode, body)
	}
	for _, private := range []string{secret, rotated.ClientSecret, access} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("a secret or token reached the request logs")
		}
	}

	limited := newOAuthHTTP(t, ratelimit.New(2, 2), nil)
	for i := 1; i <= 3; i++ {
		res, body, raw := limited.token(t, true, limited.client.Client.ClientID.String(), "bad-secret", "grant_type=client_credentials")
		want := 401
		if i == 3 {
			want = 429
		}
		if res.StatusCode != want || (i == 3 && (body["error"] != "invalid_request" || res.Header.Get("Retry-After") == "")) ||
			strings.Contains(raw, "bad-secret") {
			t.Fatalf("attempt %d: %d %v %s", i, res.StatusCode, body, raw)
		}
	}
}

func TestOAuthTokenRejectsWrongContentTypeAndConflictingCredentials(t *testing.T) {
	f := newOAuthHTTP(t, nil, nil)
	id, secret := f.client.Client.ClientID.String(), f.client.ClientSecret
	res, raw := f.raw("POST", f.srv.URL+"/v1/oauth/token", "application/json", []byte(`{"grant_type":"client_credentials"}`))
	if res.StatusCode != 400 || !strings.Contains(string(raw), "invalid_request") {
		t.Fatalf("JSON token request: %d %s", res.StatusCode, raw)
	}
	form := fmt.Sprintf("grant_type=client_credentials&client_id=%s&client_secret=%s", id, secret)
	res, body, _ := f.token(t, true, id, "different", form)
	if res.StatusCode != 400 || body["error"] != "invalid_request" {
		t.Fatalf("conflicting client credentials: %d %v", res.StatusCode, body)
	}
}
