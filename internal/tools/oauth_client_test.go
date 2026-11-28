package tools_test

import (
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/oauth"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

func TestOAuthClientAdministrationIsOwnerOrAdminAndSecretOnce(t *testing.T) {
	b := build(t)
	agent := b.agent(t, b.yuki, "External MCP")
	args := m{"agent_id": agent, "label": "Editor integration"}
	first := b.MustCall(b.yuki, "oauth_client.create", args, "oauth-create")
	created := testkit.Result[tools.OAuthClientSecretOut](t, first)
	secret := created.ClientSecret
	if !strings.HasPrefix(secret, "aisoc_") || created.Client.AgentActorID != agent || created.Client.ClientID.String() == "" ||
		len(created.Client.Scopes) != 1 || created.Client.Scopes[0] != "mcp.read" {
		t.Fatalf("created client: %+v", created)
	}
	// A replay is the same action but cannot reveal the secret again.
	replay := b.MustCall(b.yuki, "oauth_client.create", args, "oauth-create")
	if !replay.Replayed || testkit.Result[tools.OAuthClientSecretOut](t, replay).ClientSecret != "" {
		t.Fatalf("create replay revealed the secret: %+v", replay)
	}
	var actionResult string
	if err := b.Pool.QueryRow(t.Context(), `SELECT result::text FROM action WHERE id = $1`, first.ActionID).Scan(&actionResult); err != nil {
		t.Fatal(err)
	}
	var storedHash string
	if err := b.Pool.QueryRow(t.Context(), `SELECT secret_hash FROM oauth_client WHERE id = $1`, created.Client.ClientID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(actionResult, secret) || strings.Contains(storedHash, secret) || !strings.HasPrefix(storedHash, "sha256:") {
		t.Fatal("the client secret was stored or recorded in plaintext")
	}

	listCall := b.do(t, b.yuki, "oauth_client.list", m{"agent_id": agent})
	listed := testkit.Result[tools.OAuthClientListOut](t, listCall)
	if len(listed.Clients) != 1 || listed.Clients[0].ClientID != created.Client.ClientID {
		t.Fatalf("owner's clients: %+v", listed)
	}
	if strings.Contains(string(listCall.Result), secret) {
		t.Fatal("list revealed the secret")
	}

	for name, call := range map[string]struct {
		tool string
		args m
	}{
		"create": {"oauth_client.create", m{"agent_id": agent, "label": "Not yours"}},
		"list":   {"oauth_client.list", m{"agent_id": agent}},
		"rotate": {"oauth_client.rotate", m{"agent_id": agent, "client_id": created.Client.ClientID}},
		"revoke": {"oauth_client.revoke", m{"agent_id": agent, "client_id": created.Client.ClientID}},
	} {
		t.Run("non-owner "+name, func(t *testing.T) {
			b.try(t, b.ken, call.tool, call.args, apperr.NotFound)
		})
	}

	// A platform administrator manages an unowned agent too; an Agent id is
	// required, not a human actor's.
	admin := testkit.Result[tools.OAuthClientSecretOut](t, b.do(t, b.admin, "oauth_client.create",
		m{"agent_id": b.tutor, "label": "Administrator's client", "scopes": []string{"mcp.read", "mcp.write"}}))
	if admin.Client.AgentActorID != b.tutor || len(admin.Client.Scopes) != 2 {
		t.Fatalf("administrator's client: %+v", admin)
	}
	b.try(t, b.admin, "oauth_client.create", m{"agent_id": b.yuki, "label": "Not an agent"}, apperr.NotFound)
}

func TestOAuthClientRotationAndRevocationInvalidateSecretsAndTokens(t *testing.T) {
	b := build(t)
	agent := b.agent(t, b.yuki, "External MCP")
	created := testkit.Result[tools.OAuthClientSecretOut](t, b.do(t, b.yuki, "oauth_client.create",
		m{"agent_id": agent, "label": "Editor", "scopes": []string{"mcp.read", "mcp.write"}}))
	svc := oauth.NewService(b.Pool)
	oldToken, err := svc.IssueToken(t.Context(), created.Client.ClientID, created.ClientSecret, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	rotateArgs := m{"agent_id": agent, "client_id": created.Client.ClientID}
	rotatedCall := b.MustCall(b.yuki, "oauth_client.rotate", rotateArgs, "rotate-oauth")
	rotated := testkit.Result[tools.OAuthClientSecretOut](t, rotatedCall)
	if rotated.ClientSecret == created.ClientSecret || !strings.HasPrefix(rotated.ClientSecret, "aisoc_") || rotated.Client.RotatedAt == nil {
		t.Fatalf("rotation: %+v", rotated)
	}
	if replay := b.MustCall(b.yuki, "oauth_client.rotate", rotateArgs, "rotate-oauth"); !replay.Replayed ||
		testkit.Result[tools.OAuthClientSecretOut](t, replay).ClientSecret != "" {
		t.Fatalf("rotation replay revealed the new secret: %+v", replay)
	}
	if _, err := svc.IssueToken(t.Context(), created.Client.ClientID, created.ClientSecret, nil, "", ""); err != oauth.ErrInvalidClient {
		t.Fatalf("old secret after rotation: %v", err)
	}
	if _, err := svc.Authenticate(t.Context(), oldToken.Value); err != oauth.ErrInvalidToken {
		t.Fatalf("old access token after rotation: %v", err)
	}
	newToken, err := svc.IssueToken(t.Context(), created.Client.ClientID, rotated.ClientSecret, nil, "", "")
	if err != nil {
		t.Fatalf("new secret after rotation: %v", err)
	}

	b.do(t, b.yuki, "oauth_client.revoke", rotateArgs)
	if _, err := svc.Authenticate(t.Context(), newToken.Value); err != oauth.ErrInvalidToken {
		t.Fatalf("access token after client revocation: %v", err)
	}
	if _, err := svc.IssueToken(t.Context(), created.Client.ClientID, rotated.ClientSecret, nil, "", ""); err != oauth.ErrInvalidClient {
		t.Fatalf("secret after client revocation: %v", err)
	}
	b.try(t, b.yuki, "oauth_client.rotate", rotateArgs, apperr.NotFound)
	b.try(t, b.yuki, "oauth_client.revoke", rotateArgs, apperr.NotFound)

	for _, value := range []string{created.ClientSecret, rotated.ClientSecret, oldToken.Value, newToken.Value} {
		var leaked bool
		if err := b.Pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM action WHERE result::text LIKE '%' || $1 || '%' OR payload::text LIKE '%' || $1 || '%')`, value).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked {
			t.Fatal("a secret or access token reached the action log")
		}
	}
}

func TestOAuthClientScopeValidationRecordsNothing(t *testing.T) {
	b := build(t)
	agent := b.agent(t, b.yuki, "External MCP")
	before := b.Count(`SELECT count(*) FROM action`)
	for _, scopes := range [][]string{{}, {"mcp.read", "mcp.read"}, {"course.admin"}, {"mcp.read", "course.admin"}} {
		b.try(t, b.yuki, "oauth_client.create", m{"agent_id": agent, "label": "Bad scopes", "scopes": scopes}, apperr.InvalidArgument)
	}
	if after := b.Count(`SELECT count(*) FROM action`); after != before {
		t.Fatalf("invalid scopes recorded actions: before %d, after %d", before, after)
	}
	created := testkit.Result[tools.OAuthClientSecretOut](t, b.do(t, b.yuki, "oauth_client.create",
		m{"agent_id": agent, "label": "Write only", "scopes": []string{"mcp.write"}}))
	if len(created.Client.Scopes) != 1 || created.Client.Scopes[0] != "mcp.write" {
		t.Fatalf("write-only client: %+v", created)
	}
}
