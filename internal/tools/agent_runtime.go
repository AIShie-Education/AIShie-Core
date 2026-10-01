package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// The site's agent runtime's tools (docs/schema.md §2.1, Agents' hosting).
// The runtime is a site service with a credential of its own
// (service.issue_credential with scope agent_runtime, or the operator's
// `aishie-core service issue agent_runtime` at setup), and it hosts the
// runtime agents, and nothing else does. It hosts one by its id: a person
// signed in to it, by Core's assertion, asks it to host an agent of theirs;
// it asks here whether that person owns that agent and whether it may host
// it (agent_runtime.check_owner), and is then issued the agent's one token
// (agent_runtime.issue_token), which it runs the agent with, over MCP, as
// the agent. Nobody pastes a token, and nobody declares anything: people in
// the site ask a runtime agent while the token the runtime holds for it
// lives. Ending the hosting revokes it (agent_runtime.revoke_token).
//
// Nothing else is the runtime's here: no course, no seat, no person's work,
// and no mcp agent, whose tokens are its owner's.

var agentRuntime = tool.Gate{Service: domain.ServiceAgentRuntime}

const (
	EventRuntimeTokenIssued  = "agent_runtime.token_issued"
	EventRuntimeTokenRevoked = "agent_runtime.token_revoked"

	// DefaultRuntimeTokenLabel names a runtime token issued without a label.
	DefaultRuntimeTokenLabel = "agent runtime"
	maxRuntimeTokenLabel     = 200
)

func agentRuntimeTools() []tool.Tool {
	return []tool.Tool{runtimeAgent(), runtimeCheckOwner(), runtimeIssueToken(), runtimeRevokeToken()}
}

// Why the runtime may not host an agent, each as issue_token refuses it and
// check_owner and agent say it (hostable, reason).
var (
	errRuntimeNotRuntimeHosted = apperr.Precondition("that agent is an mcp agent: its owner's own tools reach it over MCP, with "+
		"tokens of the owner's, and the site's agent runtime does not host it").With("reason", "not_runtime_hosted")
	errRuntimeAgentSuspended = apperr.Precondition("that agent is suspended: it is hosted again once it is reactivated").
					With("reason", "agent_suspended")
	errRuntimeOwnerSuspended = apperr.Precondition("that agent's owner is suspended: it is hosted again once they are reactivated").
					With("reason", "owner_suspended")
	errNoSuchAgent = apperr.Missing("no such agent")
)

// hostRefusal is why the runtime may not host the agent now, or nil.
func hostRefusal(a dbq.GetAgentForRuntimeRow) *apperr.Error {
	switch {
	case domain.Hosting(a.Hosting) != domain.HostingRuntime:
		return errRuntimeNotRuntimeHosted
	case a.Status != domain.ActorActive:
		return errRuntimeAgentSuspended
	case a.OwnerStatus != nil && *a.OwnerStatus != domain.ActorActive:
		return errRuntimeOwnerSuspended
	}
	return nil
}

type RuntimeAgentIn struct {
	AgentID uuid.UUID `json:"agent_id"`
}

// RuntimeToken is the token the runtime holds for an agent, as it may see
// it: never the secret.
type RuntimeToken struct {
	CredentialID uuid.UUID  `json:"credential_id"`
	TokenPrefix  *string    `json:"token_prefix,omitempty" jsonschema:"the public part of the token, enough to tell which one it is"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty" jsonschema:"when the agent last called with it, to the minute"`
}

// RuntimeAgentView is an agent as the site's runtime needs it to host it.
type RuntimeAgentView struct {
	AgentID      uuid.UUID     `json:"agent_id"`
	DisplayName  string        `json:"display_name"`
	Hosting      string        `json:"hosting" jsonschema:"runtime: the runtime hosts it, and alone holds its token; mcp: its owner's own tools reach it, and the runtime does not host it"`
	Status       string        `json:"status" jsonschema:"active or suspended"`
	OwnerActorID *uuid.UUID    `json:"owner_actor_id,omitempty" jsonschema:"the person who owns it, for good; absent for an agent nobody owns"`
	OwnerName    *string       `json:"owner_name,omitempty"`
	OwnerStatus  *string       `json:"owner_status,omitempty" jsonschema:"active or suspended: an agent is paused while its owner is"`
	LiveSeats    int64         `json:"live_seats" jsonschema:"courses where it is seated and its seat counts now"`
	Hostable     bool          `json:"hostable" jsonschema:"whether agent_runtime.issue_token would issue it a token now"`
	Reason       *string       `json:"reason,omitempty" jsonschema:"why not, when it is not hostable: not_runtime_hosted, an mcp agent; agent_suspended; owner_suspended"`
	RuntimeToken *RuntimeToken `json:"runtime_token,omitempty" jsonschema:"the live token the runtime holds for it, if any: one at most"`
	SiteChat     bool          `json:"site_chat" jsonschema:"whether people in the site may ask it now: it is hostable and its runtime token lives"`
}

func viewRuntimeAgent(a dbq.GetAgentForRuntimeRow) RuntimeAgentView {
	v := RuntimeAgentView{AgentID: a.ID, DisplayName: a.DisplayName, Hosting: a.Hosting, Status: a.Status, OwnerActorID: a.OwnerActorID,
		OwnerName: a.OwnerName, OwnerStatus: a.OwnerStatus, LiveSeats: a.LiveSeats, SiteChat: a.SiteChat, Hostable: true}
	if why := hostRefusal(a); why != nil {
		reason, _ := why.Details["reason"].(string)
		v.Hostable, v.Reason = false, &reason
	}
	if a.RuntimeCredentialID != nil {
		v.RuntimeToken = &RuntimeToken{CredentialID: *a.RuntimeCredentialID, TokenPrefix: a.RuntimeTokenPrefix,
			CreatedAt: *a.RuntimeTokenCreatedAt, LastUsedAt: a.RuntimeTokenLastUsedAt}
	}
	return v
}

// agentForRuntime reads an agent as the runtime sees it, errNoSuchAgent for
// anyone who is not an agent, or nobody.
func agentForRuntime(ctx context.Context, q dbq.Querier, id uuid.UUID, now time.Time) (dbq.GetAgentForRuntimeRow, error) {
	a, err := q.GetAgentForRuntime(ctx, dbq.GetAgentForRuntimeParams{ID: id, Now: &now})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, errNoSuchAgent
	}
	return a, err
}

// resolveRuntimeAgent finds the agent a call of the runtime's is about: an
// id that is no agent's ends the call, with no action.
func resolveRuntimeAgent(ctx context.Context, q dbq.Querier, id uuid.UUID) (tool.Target, error) {
	a, err := q.GetActor(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.Kind != "agent") {
		return tool.Target{}, errNoSuchAgent
	}
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{Type: "actor", ID: &id}, nil
}

func runtimeAgent() tool.Tool {
	return tool.Define(tool.Spec[RuntimeAgentIn, RuntimeAgentView]{
		Name: "agent_runtime.agent",
		Description: "For the site's agent runtime alone: an agent as the runtime needs it to host it by its id: how it is " +
			"hosted (runtime or mcp), its standing and its owner's, how many seats of its count now, whether the runtime " +
			"may host it now (hostable, with the reason if not), the live token the runtime holds for it, if any, and " +
			"whether people in the site may ask it now. not_found for an id that is no agent's.",
		Kind: tool.Read, Gate: agentRuntime,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/services/agent_runtime/agents/{agent_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RuntimeAgentIn) (tool.Target, error) {
			return resolveRuntimeAgent(ctx, q, in.AgentID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in RuntimeAgentIn) (RuntimeAgentView, error) {
			a, err := agentForRuntime(ctx, rc.Q, in.AgentID, rc.Now)
			if err != nil {
				return RuntimeAgentView{}, err
			}
			return viewRuntimeAgent(a), nil
		},
	})
}

type RuntimeCheckOwnerIn struct {
	ActorID uuid.UUID `json:"actor_id" jsonschema:"the person signed in to the runtime: the sub of the assertion Core made for them"`
	AgentID uuid.UUID `json:"agent_id" jsonschema:"the agent they ask the runtime to host, or to change or stop hosting"`
}

type RuntimeCheckOwnerOut struct {
	Owns  bool              `json:"owns" jsonschema:"whether that person owns that agent; false, and nothing more, for an agent of someone else's or nobody's, or an id that is no agent's"`
	Agent *RuntimeAgentView `json:"agent,omitempty" jsonschema:"when they own it, the agent as agent_runtime.agent says it: whether it may be hosted (hostable, reason) among it"`
}

func runtimeCheckOwner() tool.Tool {
	return tool.Define(tool.Spec[RuntimeCheckOwnerIn, RuntimeCheckOwnerOut]{
		Name: "agent_runtime.check_owner",
		Description: "For the site's agent runtime alone: whether a person owns an agent, before the runtime does anything " +
			"about it for them — hosting it, changing its settings, stopping it. The runtime knows the person by Core's " +
			"assertion (its sub); this says whether they own the agent, and, if they do, the agent as agent_runtime.agent " +
			"says it, with whether the runtime may host it (hostable: an mcp agent is not_runtime_hosted). Of anyone " +
			"else's agent, or an id that is no agent's, it says owns false and nothing else.",
		Kind: tool.Read, Gate: agentRuntime,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/services/agent_runtime/owners/{actor_id}/agents/{agent_id}"},
		Resolve: noTarget[RuntimeCheckOwnerIn]("actor"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in RuntimeCheckOwnerIn) (RuntimeCheckOwnerOut, error) {
			a, err := agentForRuntime(ctx, rc.Q, in.AgentID, rc.Now)
			switch {
			case errors.Is(err, errNoSuchAgent):
				return RuntimeCheckOwnerOut{}, nil
			case err != nil:
				return RuntimeCheckOwnerOut{}, err
			case a.OwnerActorID == nil || *a.OwnerActorID != in.ActorID:
				return RuntimeCheckOwnerOut{}, nil
			}
			v := viewRuntimeAgent(a)
			return RuntimeCheckOwnerOut{Owns: true, Agent: &v}, nil
		},
	})
}

type RuntimeIssueTokenIn struct {
	AgentID uuid.UUID `json:"agent_id"`
	Label   *string   `json:"label,omitempty" jsonschema:"what the token is for, as its owner sees it listed (agent.list_credentials); agent runtime if omitted"`
}

type RuntimeIssueTokenOut struct {
	AgentID      uuid.UUID   `json:"agent_id"`
	CredentialID uuid.UUID   `json:"credential_id"`
	Token        string      `json:"token" jsonschema:"the agent's API token, which the runtime runs it with over MCP, as a bearer token; shown once: it is not stored, and a replay of this call comes back without it"`
	TokenPrefix  string      `json:"token_prefix"`
	Replaced     []uuid.UUID `json:"replaced,omitempty" jsonschema:"the token the runtime held for the agent before, revoked by this call"`
}

func runtimeIssueToken() tool.Tool {
	return tool.Define(tool.Spec[RuntimeIssueTokenIn, RuntimeIssueTokenOut]{
		Name: "agent_runtime.issue_token",
		Description: "For the site's agent runtime alone: issue the API token it runs a runtime agent with, by the agent's " +
			"id, for it to seal and keep. It is the agent's one token: the one issued before is revoked in the same call. " +
			"It never expires; people in the site ask the agent while it lives. Refused for an mcp agent " +
			"(not_runtime_hosted), a suspended agent (agent_suspended), and an agent whose owner is suspended " +
			"(owner_suspended); not_found for an id that is no agent's. Ask agent_runtime.check_owner first that the person " +
			"asking owns it. The token is returned once and only its hash is kept: a replay of this call, under the same " +
			"idempotency key, comes back without it, so a call that timed out is made again under a new key, which " +
			"revokes the token never received.",
		Kind: tool.Write, Gate: agentRuntime,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/services/agent_runtime/agents/{agent_id}/token"},
		SecretOut: []string{"token"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RuntimeIssueTokenIn) (tool.Target, error) {
			return resolveRuntimeAgent(ctx, q, in.AgentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in RuntimeIssueTokenIn) (RuntimeIssueTokenOut, error) {
			label := DefaultRuntimeTokenLabel
			if in.Label != nil {
				label = strings.TrimSpace(*in.Label)
				if label == "" || len(label) > maxRuntimeTokenLabel {
					return RuntimeIssueTokenOut{}, apperr.Invalid("label is 1 to %d characters", maxRuntimeTokenLabel).With("field", "label")
				}
			}
			// One issue or revocation at a time for an agent, and its
			// standing read under the lock: a suspension made meanwhile
			// waits for this call, or this call sees it.
			if err := ec.Q.LockAgentForHosting(ctx, in.AgentID); err != nil {
				return RuntimeIssueTokenOut{}, err
			}
			a, err := agentForRuntime(ctx, ec.Q, in.AgentID, ec.Now)
			if err != nil {
				return RuntimeIssueTokenOut{}, err
			}
			if why := hostRefusal(a); why != nil {
				return RuntimeIssueTokenOut{}, why
			}
			tok, id, replaced, err := auth.IssueRuntimeToken(ctx, ec.Q, in.AgentID, ec.Actor.ID, label, ec.Now)
			if err != nil {
				return RuntimeIssueTokenOut{}, err
			}
			// For the release before, which reads who is asked in the site
			// there; this one reads it nowhere.
			if err := ec.Q.SetSiteChatCredential(ctx, dbq.SetSiteChatCredentialParams{ID: in.AgentID, CredentialID: &id}); err != nil {
				return RuntimeIssueTokenOut{}, err
			}
			ec.Emit(events.Event{Type: EventRuntimeTokenIssued, SubjectType: "actor", SubjectID: &in.AgentID,
				Payload: map[string]any{"credential_id": id, "replaced": len(replaced)}})
			return RuntimeIssueTokenOut{AgentID: in.AgentID, CredentialID: id, Token: tok.Full, TokenPrefix: tok.Prefix, Replaced: replaced}, nil
		},
	})
}

type RuntimeRevokeTokenOut struct {
	AgentID uuid.UUID   `json:"agent_id"`
	Revoked []uuid.UUID `json:"revoked" jsonschema:"the token the runtime held for the agent, now revoked; empty when it held none, which is no error"`
}

func runtimeRevokeToken() tool.Tool {
	return tool.Define(tool.Spec[RuntimeAgentIn, RuntimeRevokeTokenOut]{
		Name: "agent_runtime.revoke_token",
		Description: "For the site's agent runtime alone: revoke the token it holds for an agent, by the agent's id, when it " +
			"stops hosting it: its owner removed it from the runtime, or paused it there. People in the site no longer ask " +
			"it, until the runtime is issued another (agent_runtime.issue_token). Revoking none, for an agent it holds no " +
			"token for, is no error; not_found for an id that is no agent's.",
		Kind: tool.Write, Gate: agentRuntime,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/services/agent_runtime/agents/{agent_id}/token/revoke"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RuntimeAgentIn) (tool.Target, error) {
			return resolveRuntimeAgent(ctx, q, in.AgentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in RuntimeAgentIn) (RuntimeRevokeTokenOut, error) {
			if err := ec.Q.LockAgentForHosting(ctx, in.AgentID); err != nil {
				return RuntimeRevokeTokenOut{}, err
			}
			revoked, err := ec.Q.RevokeRuntimeTokens(ctx, dbq.RevokeRuntimeTokensParams{ActorID: in.AgentID, Now: &ec.Now})
			if err != nil {
				return RuntimeRevokeTokenOut{}, err
			}
			out := RuntimeRevokeTokenOut{AgentID: in.AgentID, Revoked: revoked}
			if out.Revoked == nil {
				out.Revoked = []uuid.UUID{}
				return out, nil
			}
			if err := ec.Q.SetSiteChatCredential(ctx, dbq.SetSiteChatCredentialParams{ID: in.AgentID}); err != nil {
				return RuntimeRevokeTokenOut{}, err
			}
			ec.Emit(events.Event{Type: EventRuntimeTokenRevoked, SubjectType: "actor", SubjectID: &in.AgentID,
				Payload: map[string]any{"credentials": revoked}})
			return out, nil
		},
	})
}
