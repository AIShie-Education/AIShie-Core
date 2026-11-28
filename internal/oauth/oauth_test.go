package oauth_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/oauth"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
)

func TestSecretsAreOpaqueRandomAndHashed(t *testing.T) {
	client, err := oauth.NewClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	access, err := oauth.NewAccessToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(client.Value, "aisoc_") || !strings.HasPrefix(access.Value, "aiso_") ||
		client.Value == access.Value || strings.Contains(client.Hash, client.Value) || strings.Contains(access.Hash, access.Value) ||
		!strings.HasPrefix(client.Hash, "sha256:") || !oauth.ShapedAccessToken(access.Value) || oauth.ShapedAccessToken(client.Value) {
		t.Fatalf("secret shapes: client=%+v access=%+v", client, access)
	}
	if !oauth.MatchesClientSecret(client.Value, client.Hash) || oauth.MatchesClientSecret(client.Value+"x", client.Hash) {
		t.Fatal("constant-time secret verification accepted the wrong secret")
	}
}

func TestScopesAreDefaultsCanonicalAndSubsetOnly(t *testing.T) {
	if got, err := oauth.ClientScopes(nil); err != nil || !reflect.DeepEqual(got, []string{oauth.ScopeRead}) {
		t.Fatalf("default scopes: %v %v", got, err)
	}
	for _, invalid := range [][]string{{}, {oauth.ScopeRead, oauth.ScopeRead}, {"course.admin"}, {oauth.ScopeRead, "course.admin"}} {
		if _, err := oauth.ClientScopes(invalid); !errors.Is(err, oauth.ErrInvalidScope) {
			t.Errorf("client scopes %v: %v", invalid, err)
		}
	}
	client := []string{oauth.ScopeRead, oauth.ScopeWrite}
	for _, tc := range []struct {
		requested []string
		want      []string
	}{
		{nil, client},
		{oauth.ParseScope(""), client},
		{oauth.ParseScope("mcp.write"), []string{oauth.ScopeWrite}},
		{oauth.ParseScope("mcp.write mcp.read"), client},
	} {
		got, err := oauth.RequestedScopes(tc.requested, client)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("requested %v: %v %v, want %v", tc.requested, got, err, tc.want)
		}
	}
	for _, invalid := range [][]string{{}, {oauth.ScopeRead, oauth.ScopeRead}, {"course.admin"}} {
		if _, err := oauth.RequestedScopes(invalid, client); !errors.Is(err, oauth.ErrInvalidScope) {
			t.Errorf("requested scopes %v: %v", invalid, err)
		}
	}
	if _, err := oauth.RequestedScopes([]string{oauth.ScopeWrite}, []string{oauth.ScopeRead}); !errors.Is(err, oauth.ErrInvalidScope) {
		t.Fatalf("write escaped a read-only client: %v", err)
	}
}

type oauthWorld struct {
	*testkit.World
	service *oauth.Service
	owner   uuid.UUID
	agent   uuid.UUID
	client  uuid.UUID
	secret  oauth.Secret
	now     time.Time
}

func newOAuthWorld(t *testing.T) *oauthWorld {
	t.Helper()
	w := testkit.NewWorld(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := w.Actor("human", "Owner")
	agent := w.OwnedAgent(owner, "MCP agent")
	secret, err := oauth.NewClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	client := ids.New()
	if err := w.Q.InsertOAuthClient(context.Background(), dbq.InsertOAuthClientParams{ID: client, AgentActorID: agent,
		Label: "Test client", SecretHash: secret.Hash, Scopes: []string{oauth.ScopeRead, oauth.ScopeWrite}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	svc := oauth.NewService(w.Pool)
	out := &oauthWorld{World: w, service: svc, owner: owner, agent: agent, client: client, secret: secret, now: now}
	svc.SetClock(func() time.Time { return out.now })
	return out
}

func TestTokenLifecycleChecksAgentOwnerRevocationAndExpiry(t *testing.T) {
	w := newOAuthWorld(t)
	ctx := context.Background()
	issued, err := w.service.IssueToken(ctx, w.client, w.secret.Value, nil, "https://lms.example.edu/mcp", "https://lms.example.edu/mcp")
	if err != nil || !oauth.ShapedAccessToken(issued.Value) || !issued.ExpiresAt.Equal(w.now.Add(10*time.Minute)) {
		t.Fatalf("issue: %+v %v", issued, err)
	}
	info, err := w.service.Authenticate(ctx, issued.Value)
	if err != nil || info.AgentActorID != w.agent || info.ClientID != w.client || info.TokenID == uuid.Nil ||
		!reflect.DeepEqual(info.Scopes, []string{oauth.ScopeRead, oauth.ScopeWrite}) {
		t.Fatalf("authenticate: %+v %v", info, err)
	}
	var stored string
	if err := w.Pool.QueryRow(ctx, `SELECT token_hash FROM oauth_access_token WHERE id = $1`, info.TokenID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, issued.Value) || !strings.HasPrefix(stored, "sha256:") {
		t.Fatalf("the token itself was stored: %q", stored)
	}
	if _, err := w.service.IssueToken(ctx, w.client, w.secret.Value, nil, "https://elsewhere.example/mcp", "https://lms.example.edu/mcp"); !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Fatalf("wrong resource: %v", err)
	}
	if _, err := w.service.IssueToken(ctx, w.client, "wrong", nil, "", ""); !errors.Is(err, oauth.ErrInvalidClient) {
		t.Fatalf("wrong secret: %v", err)
	}

	// owner_suspended matches agent_runtime.issue_token, both for a new token
	// and for one already issued.
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, w.owner)
	if _, err := w.service.IssueToken(ctx, w.client, w.secret.Value, nil, "", ""); !errors.Is(err, oauth.ErrInvalidClient) {
		t.Fatalf("suspended owner issuing: %v", err)
	}
	if _, err := w.service.Authenticate(ctx, issued.Value); !errors.Is(err, oauth.ErrInvalidToken) {
		t.Fatalf("suspended owner authenticating: %v", err)
	}
	w.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, w.owner)
	if _, err := w.service.Authenticate(ctx, issued.Value); err != nil {
		t.Fatalf("owner reactivation did not restore the token: %v", err)
	}
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, w.agent)
	if _, err := w.service.IssueToken(ctx, w.client, w.secret.Value, nil, "", ""); !errors.Is(err, oauth.ErrInvalidClient) {
		t.Fatalf("suspended agent issuing: %v", err)
	}
	if _, err := w.service.Authenticate(ctx, issued.Value); !errors.Is(err, oauth.ErrInvalidToken) {
		t.Fatalf("suspended agent authenticating: %v", err)
	}
	w.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, w.agent)

	w.now = w.now.Add(10*time.Minute + time.Nanosecond)
	if _, err := w.service.Authenticate(ctx, issued.Value); !errors.Is(err, oauth.ErrInvalidToken) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestRotationAndIssueSerializeWithoutLeavingAnOldTokenValid(t *testing.T) {
	w := newOAuthWorld(t)
	ctx := context.Background()
	for i := range 12 {
		rotated, err := oauth.NewClientSecret()
		if err != nil {
			t.Fatal(err)
		}
		type issueResult struct {
			token oauth.IssuedToken
			err   error
		}
		issued := make(chan issueResult, 1)
		rotatedDone := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			token, err := w.service.IssueToken(ctx, w.client, w.secret.Value, nil, "", "")
			issued <- issueResult{token, err}
		})
		wg.Go(func() {
			rotatedDone <- db.InTx(ctx, w.Pool, func(tx pgx.Tx) error {
				q := dbq.New(tx)
				if _, err := q.RotateOAuthClientSecret(ctx, dbq.RotateOAuthClientSecretParams{ID: w.client,
					AgentActorID: w.agent, SecretHash: rotated.Hash, RotatedAt: &w.now}); err != nil {
					return err
				}
				return q.RevokeOAuthAccessTokensForClient(ctx, dbq.RevokeOAuthAccessTokensForClientParams{
					ClientID: w.client, RevokedAt: &w.now})
			})
		})
		wg.Wait()
		result, rotateErr := <-issued, <-rotatedDone
		if rotateErr != nil {
			t.Fatalf("rotation %d: %v", i, rotateErr)
		}
		if result.err != nil && !errors.Is(result.err, oauth.ErrInvalidClient) {
			t.Fatalf("issue %d: %v", i, result.err)
		}
		if result.err == nil {
			if _, err := w.service.Authenticate(ctx, result.token.Value); !errors.Is(err, oauth.ErrInvalidToken) {
				t.Fatalf("issue %d survived rotation: %v", i, err)
			}
		}
		w.secret = rotated
		// The new secret works after either ordering, proving the test made
		// progress rather than merely refusing every issue.
		next, err := w.service.IssueToken(ctx, w.client, w.secret.Value, nil, "", "")
		if err != nil {
			t.Fatalf("new secret %d: %v", i, err)
		}
		if _, err := w.service.Authenticate(ctx, next.Value); err != nil {
			t.Fatalf("new token %d: %v", i, err)
		}
	}
}
