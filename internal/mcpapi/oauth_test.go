package mcpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/oauth"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

const oauthMCPIssuer = "https://lms.example.edu"

type oauthMCP struct {
	c       *testkit.CS101
	srv     *httptest.Server
	service *oauth.Service
	client  tools.OAuthClientSecretOut
}

func newOAuthMCP(t *testing.T) *oauthMCP {
	t.Helper()
	c := testkit.NewCS101(t, 1)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	service := oauth.NewService(c.Pool)
	client := testkit.Result[tools.OAuthClientSecretOut](t, c.MustCall(c.Root, "oauth_client.create",
		m{"agent_id": c.Grader, "label": "External MCP", "scopes": []string{"mcp.read", "mcp.write"}}, "mcp-oauth-create"))
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn, OAuth: service, PublicURL: oauthMCPIssuer,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn, OAuth: service, PublicURL: oauthMCPIssuer}),
	}))
	t.Cleanup(srv.Close)
	return &oauthMCP{c: c, srv: srv, service: service, client: client}
}

func (f *oauthMCP) issue(t *testing.T, scope string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {f.client.Client.ClientID.String()},
		"client_secret": {f.client.ClientSecret}, "scope": {scope}}
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !strings.HasPrefix(out.AccessToken, "aiso_") {
		t.Fatalf("issue with scope %q: %d %+v", scope, res.StatusCode, out)
	}
	return out.AccessToken
}

func (f *oauthMCP) post(t *testing.T, token, body string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+httpapi.MCPPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(raw)
}

const oauthInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"external","version":"1"}}}`

func (f *oauthMCP) gradeCall(key string) string {
	return `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"grade_submit","arguments":{"course_id":"` +
		f.c.Course.String() + `","submission_id":"` + f.c.Students[0].HW3.String() + `","score":90,"idempotency_key":"` + key + `"}}}`
}

func TestMCPOAuthEndToEndScopeIntersectionAndLegacyCompatibility(t *testing.T) {
	f := newOAuthMCP(t)
	readOnly := f.issue(t, "mcp.read")
	metadata := oauthMCPIssuer + "/.well-known/oauth-protected-resource"
	for _, token := range []string{"", "aiso_not-a-real-token"} {
		status, header, body := f.post(t, token, oauthInitialize)
		if status != 401 || !strings.Contains(header.Get("WWW-Authenticate"), `resource_metadata="`+metadata+`"`) {
			t.Fatalf("missing/invalid bearer: %d %v %s", status, header, body)
		}
	}
	if status, _, body := f.post(t, readOnly, oauthInitialize); status != 200 || !strings.Contains(body, "protocolVersion") {
		t.Fatalf("initialize: %d %s", status, body)
	}
	if status, _, body := f.post(t, readOnly, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); status != 200 ||
		!strings.Contains(body, "me_get") || !strings.Contains(body, "grade_submit") {
		t.Fatalf("tools/list: %d %.500s", status, body)
	}
	if status, _, body := f.post(t, readOnly, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"me_get","arguments":{}}}`); status != 200 ||
		!strings.Contains(body, f.c.Grader.String()) {
		t.Fatalf("read tool: %d %s", status, body)
	}
	status, header, body := f.post(t, readOnly, f.gradeCall("read-only-write"))
	if status != 403 || !strings.Contains(body, "insufficient_scope") ||
		!strings.Contains(header.Get("WWW-Authenticate"), `scope="mcp.write"`) || !strings.Contains(header.Get("WWW-Authenticate"), metadata) {
		t.Fatalf("read scope writing: %d %v %s", status, header, body)
	}

	// The legacy ais_ token has no OAuth scopes and is deliberately not
	// subjected to the new intersection: its old write behavior is unchanged.
	legacy, _, err := auth.IssueToken(t.Context(), dbq.New(f.c.Pool), f.c.Grader, nil, "legacy", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status, _, body := f.post(t, legacy.Full, f.gradeCall("legacy-write")); status != 200 ||
		!strings.Contains(body, `\"status\":\"proposed\"`) {
		t.Fatalf("legacy ais_ write: %d %s", status, body)
	}

	writeOnly := f.issue(t, "mcp.write")
	if status, header, body := f.post(t, writeOnly, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); status != 403 ||
		!strings.Contains(header.Get("WWW-Authenticate"), `scope="mcp.read"`) || !strings.Contains(body, "insufficient_scope") {
		t.Fatalf("write scope listing: %d %v %s", status, header, body)
	}
	if status, _, body := f.post(t, writeOnly, f.gradeCall("oauth-write")); status != 200 ||
		!strings.Contains(body, `\"status\":\"proposed\"`) {
		t.Fatalf("write scope with seat permission: %d %s", status, body)
	}
	// mcp.write does not manufacture document_write on the agent's seat.
	document := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"document_create","arguments":{"course_id":"` +
		f.c.Course.String() + `","kind":"material","title":"Forbidden","body_md":"No","idempotency_key":"scope-not-seat"}}}`
	if status, _, body := f.post(t, writeOnly, document); status != 200 || !strings.Contains(body, `\"status\":\"denied\"`) {
		t.Fatalf("write scope substituted for seat permission: %d %s", status, body)
	}
}

func TestMCPOAuthRotationRevocationAgentSuspensionAndExpiry(t *testing.T) {
	f := newOAuthMCP(t)
	now := time.Now()
	f.service.SetClock(func() time.Time { return now })
	access := f.issue(t, "mcp.read mcp.write")
	if status, _, body := f.post(t, access, oauthInitialize); status != 200 {
		t.Fatalf("initial token: %d %s", status, body)
	}
	rotated := testkit.Result[tools.OAuthClientSecretOut](t, f.c.MustCall(f.c.Root, "oauth_client.rotate",
		m{"agent_id": f.c.Grader, "client_id": f.client.Client.ClientID}, "mcp-oauth-rotate"))
	status, header, body := f.post(t, access, oauthInitialize)
	if status != 401 || !strings.Contains(header.Get("WWW-Authenticate"), "oauth-protected-resource") {
		t.Fatalf("rotated token: %d %v %s", status, header, body)
	}
	f.client.ClientSecret = rotated.ClientSecret
	access = f.issue(t, "mcp.read")
	f.c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, f.c.Grader)
	if status, _, body := f.post(t, access, oauthInitialize); status != 401 {
		t.Fatalf("suspended agent token: %d %s", status, body)
	}
	f.c.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, f.c.Grader)
	now = now.Add(10*time.Minute + time.Nanosecond)
	if status, _, body := f.post(t, access, oauthInitialize); status != 401 {
		t.Fatalf("expired token: %d %s", status, body)
	}

	f.client.ClientSecret = rotated.ClientSecret
	access = f.issue(t, "mcp.read")
	f.c.MustCall(f.c.Root, "oauth_client.revoke", m{"agent_id": f.c.Grader, "client_id": f.client.Client.ClientID}, "mcp-oauth-revoke")
	if status, _, body := f.post(t, access, oauthInitialize); status != 401 {
		t.Fatalf("revoked client token: %d %s", status, body)
	}
}
