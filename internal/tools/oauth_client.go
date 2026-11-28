package tools

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/oauth"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// OAuth clients are machine credentials of Agent actors. Their owner or a
// platform administrator manages them here; there is no public registration
// endpoint and no human consent flow.
func oauthClientTools() []tool.Tool {
	return []tool.Tool{oauthClientCreate(), oauthClientList(), oauthClientRotate(), oauthClientRevoke()}
}

const maxOAuthClientLabel = 200

// manageableOAuthAgent applies owner-or-platform-administrator authorization.
// It reads kind only to enforce that the client belongs to an Agent, never to
// infer a course permission or roster role.
func manageableOAuthAgent(ctx context.Context, q dbq.Querier, caller domain.Actor, agentID uuid.UUID) (dbq.Actor, error) {
	a, err := q.GetActor(ctx, agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, errNotYourAgent
	}
	if err != nil {
		return a, err
	}
	admin := caller.PlatformRole == domain.PlatformRoot || caller.PlatformRole == domain.PlatformAdmin
	if a.Kind != "agent" || (!admin && (a.OwnerActorID == nil || *a.OwnerActorID != caller.ID)) {
		return a, errNotYourAgent
	}
	return a, nil
}

func oauthClientTarget(agentID uuid.UUID) tool.Target {
	return tool.Target{Type: "actor", ID: &agentID}
}

type OAuthClientView struct {
	ClientID         uuid.UUID  `json:"client_id"`
	AgentActorID     uuid.UUID  `json:"agent_actor_id"`
	Label            string     `json:"label"`
	Scopes           []string   `json:"scopes"`
	CreatedByActorID *uuid.UUID `json:"created_by_actor_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	RotatedAt        *time.Time `json:"rotated_at,omitempty"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
}

type OAuthClientSecretOut struct {
	Client       OAuthClientView `json:"client"`
	ClientSecret string          `json:"client_secret,omitempty" jsonschema:"the generated secret, shown only by this successful create or rotation; a replay omits it"`
}

type OAuthClientCreateIn struct {
	AgentID uuid.UUID `json:"agent_id"`
	Label   string    `json:"label" jsonschema:"what this machine client is for, at most 200 characters"`
	Scopes  []string  `json:"scopes,omitempty" jsonschema:"a subset of mcp.read and mcp.write; default [mcp.read]"`
}

func checkOAuthClientCreate(in OAuthClientCreateIn) error {
	label := strings.TrimSpace(in.Label)
	if label == "" || utf8.RuneCountInString(label) > maxOAuthClientLabel {
		return apperr.Invalid("label is 1 to %d characters", maxOAuthClientLabel).With("field", "label")
	}
	if _, err := oauth.ClientScopes(in.Scopes); err != nil {
		return apperr.Invalid("scopes is a non-empty subset of mcp.read and mcp.write, without duplicates").With("field", "scopes")
	}
	return nil
}

func oauthClientCreate() tool.Tool {
	return tool.Define(tool.Spec[OAuthClientCreateIn, OAuthClientSecretOut]{
		Name: "oauth_client.create",
		Description: "Create an OAuth Client Credentials client for one of your Agent actors, or any agent as a platform " +
			"administrator. The generated client secret is returned once and only its SHA-256 hash is kept. scopes is a " +
			"subset of mcp.read and mcp.write, default mcp.read. This is not anonymous dynamic client registration and " +
			"opens no human consent flow.",
		Kind: tool.Write, Gate: self, SecretOut: []string{"client_secret"},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/agents/{agent_id}/oauth-clients"},
		Resolve: func(_ context.Context, _ dbq.Querier, in OAuthClientCreateIn) (tool.Target, error) {
			return oauthClientTarget(in.AgentID), nil
		},
		Check: checkOAuthClientCreate,
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in OAuthClientCreateIn) (OAuthClientSecretOut, error) {
			if _, err := manageableOAuthAgent(ctx, ec.Q, ec.Actor, in.AgentID); err != nil {
				return OAuthClientSecretOut{}, err
			}
			scopes, _ := oauth.ClientScopes(in.Scopes)
			secret, err := oauth.NewClientSecret()
			if err != nil {
				return OAuthClientSecretOut{}, err
			}
			id := ids.New()
			if err := ec.Q.InsertOAuthClient(ctx, dbq.InsertOAuthClientParams{ID: id, AgentActorID: in.AgentID,
				Label: strings.TrimSpace(in.Label), SecretHash: secret.Hash, Scopes: scopes, CreatedByActorID: &ec.Actor.ID,
				CreatedAt: ec.Now}); err != nil {
				return OAuthClientSecretOut{}, err
			}
			return OAuthClientSecretOut{Client: OAuthClientView{ClientID: id, AgentActorID: in.AgentID,
				Label: strings.TrimSpace(in.Label), Scopes: scopes, CreatedByActorID: &ec.Actor.ID, CreatedAt: ec.Now},
				ClientSecret: secret.Value}, nil
		},
	})
}

type OAuthClientAgentIn struct {
	AgentID uuid.UUID `json:"agent_id"`
}

type OAuthClientListOut struct {
	Clients []OAuthClientView `json:"clients"`
}

func oauthClientList() tool.Tool {
	return tool.Define(tool.Spec[OAuthClientAgentIn, OAuthClientListOut]{
		Name: "oauth_client.list",
		Description: "An Agent actor's OAuth clients, newest first, with their labels, scopes and rotation/revocation " +
			"timestamps. For its owner or a platform administrator. Neither secret nor access-token hashes are shown.",
		Kind: tool.Read, Gate: self,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/agents/{agent_id}/oauth-clients"},
		Resolve: func(_ context.Context, _ dbq.Querier, in OAuthClientAgentIn) (tool.Target, error) {
			return oauthClientTarget(in.AgentID), nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in OAuthClientAgentIn) (OAuthClientListOut, error) {
			if _, err := manageableOAuthAgent(ctx, rc.Q, rc.Actor, in.AgentID); err != nil {
				return OAuthClientListOut{}, err
			}
			rows, err := rc.Q.ListOAuthClientsForAgent(ctx, in.AgentID)
			out := OAuthClientListOut{Clients: make([]OAuthClientView, 0, len(rows))}
			for _, r := range rows {
				out.Clients = append(out.Clients, OAuthClientView{ClientID: r.ID, AgentActorID: r.AgentActorID, Label: r.Label,
					Scopes: append([]string(nil), r.Scopes...), CreatedByActorID: r.CreatedByActorID, CreatedAt: r.CreatedAt,
					RotatedAt: r.RotatedAt, RevokedAt: r.RevokedAt})
			}
			return out, err
		},
	})
}

type OAuthClientIDIn struct {
	AgentID  uuid.UUID `json:"agent_id"`
	ClientID uuid.UUID `json:"client_id"`
}

func oauthClientRotate() tool.Tool {
	return tool.Define(tool.Spec[OAuthClientIDIn, OAuthClientSecretOut]{
		Name: "oauth_client.rotate",
		Description: "Rotate an OAuth client's secret: the old secret and every outstanding access token of that client " +
			"stop working in the same transaction. The new secret is returned once and only its hash is kept. For the " +
			"agent's owner or a platform administrator.",
		Kind: tool.Write, Gate: self, SecretOut: []string{"client_secret"},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/agents/{agent_id}/oauth-clients/{client_id}/rotate"},
		Resolve: func(_ context.Context, _ dbq.Querier, in OAuthClientIDIn) (tool.Target, error) {
			return oauthClientTarget(in.AgentID), nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in OAuthClientIDIn) (OAuthClientSecretOut, error) {
			if _, err := manageableOAuthAgent(ctx, ec.Q, ec.Actor, in.AgentID); err != nil {
				return OAuthClientSecretOut{}, err
			}
			client, err := ec.Q.GetOAuthClientForAgent(ctx, dbq.GetOAuthClientForAgentParams{ID: in.ClientID, AgentActorID: in.AgentID})
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && client.RevokedAt != nil) {
				return OAuthClientSecretOut{}, apperr.Missing("no such live OAuth client of this agent")
			}
			if err != nil {
				return OAuthClientSecretOut{}, err
			}
			secret, err := oauth.NewClientSecret()
			if err != nil {
				return OAuthClientSecretOut{}, err
			}
			n, err := ec.Q.RotateOAuthClientSecret(ctx, dbq.RotateOAuthClientSecretParams{ID: in.ClientID,
				AgentActorID: in.AgentID, SecretHash: secret.Hash, RotatedAt: &ec.Now})
			if err != nil {
				return OAuthClientSecretOut{}, err
			}
			if n == 0 {
				return OAuthClientSecretOut{}, apperr.Missing("no such live OAuth client of this agent")
			}
			if err := ec.Q.RevokeOAuthAccessTokensForClient(ctx, dbq.RevokeOAuthAccessTokensForClientParams{
				ClientID: in.ClientID, RevokedAt: &ec.Now}); err != nil {
				return OAuthClientSecretOut{}, err
			}
			return OAuthClientSecretOut{Client: OAuthClientView{ClientID: client.ID, AgentActorID: client.AgentActorID,
				Label: client.Label, Scopes: client.Scopes, CreatedByActorID: client.CreatedByActorID,
				CreatedAt: client.CreatedAt, RotatedAt: &ec.Now}, ClientSecret: secret.Value}, nil
		},
	})
}

func oauthClientRevoke() tool.Tool {
	return tool.Define(tool.Spec[OAuthClientIDIn, OK]{
		Name: "oauth_client.revoke",
		Description: "Revoke an OAuth client: its secret and every outstanding access token stop working in the same " +
			"transaction. For the agent's owner or a platform administrator.",
		Kind: tool.Write, Gate: self,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/agents/{agent_id}/oauth-clients/{client_id}/revoke"},
		Resolve: func(_ context.Context, _ dbq.Querier, in OAuthClientIDIn) (tool.Target, error) {
			return oauthClientTarget(in.AgentID), nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in OAuthClientIDIn) (OK, error) {
			if _, err := manageableOAuthAgent(ctx, ec.Q, ec.Actor, in.AgentID); err != nil {
				return OK{}, err
			}
			n, err := ec.Q.RevokeOAuthClient(ctx, dbq.RevokeOAuthClientParams{ID: in.ClientID,
				AgentActorID: in.AgentID, RevokedAt: &ec.Now})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Missing("no such live OAuth client of this agent")
			}
			if err := ec.Q.RevokeOAuthAccessTokensForClient(ctx, dbq.RevokeOAuthAccessTokensForClientParams{
				ClientID: in.ClientID, RevokedAt: &ec.Now}); err != nil {
				return OK{}, err
			}
			return OK{OK: true}, nil
		},
	})
}
