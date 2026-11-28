// Package oauth implements Core's Agent-actor OAuth 2.0 Client Credentials
// flow. It is machine-to-machine authentication for /mcp only: no human
// consent, authorization code, PKCE, dynamic registration or refresh token.
package oauth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
)

// AccessTokenTTL is the longest an opaque access token may live. A client
// asks for another with its secret; no refresh token is issued.
const AccessTokenTTL = 10 * time.Minute

// Service issues and verifies OAuth tokens. Its only persistent state is in
// PostgreSQL, so every Core instance applies the same rotation and revocation.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// SetClock replaces the clock, for tests of expiry.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// IssuedToken is an access token as its one successful response sees it.
type IssuedToken struct {
	Value     string
	Scopes    []string
	ExpiresAt time.Time
}

// IssueToken verifies a client secret and issues an opaque token for the
// canonical public /mcp resource. resource may be empty; when supplied it must
// equal canonicalResource exactly. The client row is locked through insertion,
// so rotation or revocation cannot race a token into validity under an old secret.
func (s *Service) IssueToken(ctx context.Context, clientID uuid.UUID, secret string, requested []string,
	resource, canonicalResource string) (IssuedToken, error) {
	if resource != "" && resource != canonicalResource {
		return IssuedToken{}, ErrInvalidGrant
	}
	var out IssuedToken
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		client, err := q.LockOAuthClientForToken(ctx, clientID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidClient
		}
		if err != nil {
			return err
		}
		if !agentUsable(client.AgentKind, client.AgentStatus, client.AgentOwnerActorID, client.AgentOwnerStatus) ||
			client.RevokedAt != nil || !MatchesClientSecret(secret, client.SecretHash) {
			return ErrInvalidClient
		}
		scopes, err := RequestedScopes(requested, client.Scopes)
		if err != nil {
			return err
		}
		token, err := NewAccessToken()
		if err != nil {
			return err
		}
		now := s.now()
		out = IssuedToken{Value: token.Value, Scopes: scopes, ExpiresAt: now.Add(AccessTokenTTL)}
		return q.InsertOAuthAccessToken(ctx, dbq.InsertOAuthAccessTokenParams{
			ID: ids.New(), ClientID: client.ID, AgentActorID: client.AgentActorID, TokenHash: token.Hash,
			Scopes: scopes, Resource: Resource, CreatedAt: now, ExpiresAt: out.ExpiresAt,
		})
	})
	if err != nil {
		return IssuedToken{}, err
	}
	return out, nil
}

// agentUsable is agent_runtime.issue_token's standing rule: an agent and, when
// it has an owner, its owner must both be active. A missing owner row fails
// closed rather than leaving an owned agent running after owner deletion.
func agentUsable(kind, status string, ownerID *uuid.UUID, ownerStatus *string) bool {
	if kind != "agent" || status != domain.ActorActive {
		return false
	}
	return ownerID == nil || (ownerStatus != nil && *ownerStatus == domain.ActorActive)
}

// TokenInfo is what an OAuth bearer token authenticates on /mcp. TokenID is
// the access-token row id, not a legacy credential id.
type TokenInfo struct {
	AgentActorID uuid.UUID
	TokenID      uuid.UUID
	ClientID     uuid.UUID
	Scopes       []string
	ExpiresAt    time.Time
}

// Authenticate checks an opaque token, its client and its agent on every MCP
// request. Expiry, rotation, client revocation and agent suspension therefore
// take effect without waiting for a sweep.
func (s *Service) Authenticate(ctx context.Context, presented string) (TokenInfo, error) {
	if !ShapedAccessToken(presented) {
		return TokenInfo{}, ErrInvalidToken
	}
	q := dbq.New(s.pool)
	row, err := q.GetOAuthAccessTokenByHash(ctx, hashSecret(presented))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenInfo{}, ErrInvalidToken
	}
	if err != nil {
		return TokenInfo{}, err
	}
	now := s.now()
	if row.Resource != Resource || row.RevokedAt != nil || row.ClientRevokedAt != nil ||
		!agentUsable(row.AgentKind, row.AgentStatus, row.AgentOwnerActorID, row.AgentOwnerStatus) || !row.ExpiresAt.After(now) {
		return TokenInfo{}, ErrInvalidToken
	}
	// Usage accounting is best effort, exactly as it is for legacy tokens.
	_ = q.TouchOAuthAccessToken(ctx, dbq.TouchOAuthAccessTokenParams{ID: row.ID, LastUsedAt: &now})
	return TokenInfo{AgentActorID: row.AgentActorID, TokenID: row.ID, ClientID: row.ClientID,
		Scopes: append([]string(nil), row.Scopes...), ExpiresAt: row.ExpiresAt}, nil
}
