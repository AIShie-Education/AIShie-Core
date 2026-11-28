package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Subjects and operations of an approval in the agent runtime's own chat
// store. Core decides only who may judge it; the runtime owns its lifecycle.
const (
	RuntimeChatSubjectQuestion = "question"
	RuntimeChatSubjectAnswer   = "answer"

	RuntimeChatOperationDecide = "decide"
	RuntimeChatOperationReview = "review"
)

type RuntimeCheckChatDecisionIn struct {
	ActorID       uuid.UUID `json:"actor_id" jsonschema:"the reviewer named by the runtime assertion: who would decide or review"`
	AgentID       uuid.UUID `json:"agent_id" jsonschema:"the Core agent actor answering the runtime-owned conversation"`
	CourseID      uuid.UUID `json:"course_id" jsonschema:"the course whose seats and scopes govern the judgment"`
	OpenerActorID uuid.UUID `json:"opener_actor_id" jsonschema:"the person who opened the runtime-owned conversation"`
	Subject       string    `json:"subject" jsonschema:"question: the opener's pending question; answer: the agent's pending answer"`
	Operation     string    `json:"operation" jsonschema:"decide: approve, reject or request changes; review: review an executed question or answer after the fact"`
}

type RuntimeCheckChatDecisionOut struct {
	Allowed           bool   `json:"allowed" jsonschema:"whether that actor may make that judgment now; false, with no reason and no other field, for every denial or unknown id"`
	ActionDecideLevel string `json:"action_decide_level,omitempty" jsonschema:"present on every allowed result: the judge's effective action_decide level, including any principal cap; confirm_required decisions need their own confirmation, pending_review decisions execute under later review, autonomous decisions execute directly"`
}

func checkRuntimeChatDecisionArgs(in RuntimeCheckChatDecisionIn) error {
	switch in.Subject {
	case RuntimeChatSubjectQuestion, RuntimeChatSubjectAnswer:
	default:
		return apperr.Invalid("subject must be %q or %q", RuntimeChatSubjectQuestion, RuntimeChatSubjectAnswer).
			With("field", "subject")
	}
	switch in.Operation {
	case RuntimeChatOperationDecide, RuntimeChatOperationReview:
		return nil
	default:
		return apperr.Invalid("operation must be %q or %q", RuntimeChatOperationDecide, RuntimeChatOperationReview).
			With("field", "operation")
	}
}

func runtimeCheckChatDecision() tool.Tool {
	return tool.Define(tool.Spec[RuntimeCheckChatDecisionIn, RuntimeCheckChatDecisionOut]{
		Name: "agent_runtime.check_chat_decision",
		Description: "For the site's agent runtime alone: whether the actor in its assertion may decide or review a question " +
			"or answer in a conversation the runtime itself stores. Core checks the live course, actors and seats, the " +
			"opener's overseeing scope, four-eyes party rules, and the answer owner's autonomous exception, exactly as " +
			"action.decide and action.review do. This opens no conversation, writes no message, action or event, and " +
			"returns no model, provider, conversation or message. Every unknown id or denial returns only allowed false. " +
			"An allowed result adds the judge's effective action_decide_level so the runtime never mistakes a " +
			"confirm_required or pending_review judgment for an autonomous one.",
		Kind:    tool.Read,
		Gate:    agentRuntime,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/services/agent_runtime/chat-decisions/check"},
		Resolve: noTarget[RuntimeCheckChatDecisionIn]("conversation"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in RuntimeCheckChatDecisionIn) (RuntimeCheckChatDecisionOut, error) {
			if err := checkRuntimeChatDecisionArgs(in); err != nil {
				return RuntimeCheckChatDecisionOut{}, err
			}
			return mayJudgeRuntimeChat(ctx, rc.Q, in, rc.Now)
		},
	})
}

// mayJudgeRuntimeChat is the identity and scope part of action.decide and
// action.review for an AR-owned chat approval. The proposal's status, any
// nested decision chain and any escalation history belong to AR's approval
// row, not to this identity-only query. Deciding and reviewing otherwise have
// the same permission and party rules (pipeline.refuseDecision/refuseReview).
//
// A question is the opener's action: its judge must oversee the opener and be
// outside the opener's party. An answer is the respondent's action: its judge
// must oversee the opener or own the respondent, and decidesAnswers applies
// action_decide, four eyes, and the one owner-at-autonomous exception. The
// owner exception is intentionally not extended to sibling agents or to the
// respondent itself. Every expected refusal, including an unknown id, is
// indistinguishably false; only an internal failure is returned as an error.
func mayJudgeRuntimeChat(ctx context.Context, q dbq.Querier, in RuntimeCheckChatDecisionIn, now time.Time) (RuntimeCheckChatDecisionOut, error) {
	refused := func(err error) (RuntimeCheckChatDecisionOut, error) {
		if errors.Is(err, pgx.ErrNoRows) {
			return RuntimeCheckChatDecisionOut{}, nil
		}
		if _, ok := apperr.As(err); ok {
			return RuntimeCheckChatDecisionOut{}, nil
		}
		return RuntimeCheckChatDecisionOut{}, err
	}

	// action.decide/review's caller checks are write checks even though this
	// service query is read-only: an archived course and a paused, removed or
	// expired judge cannot make the judgment being checked.
	actor, err := authz.LoadActor(ctx, q, in.ActorID)
	if err != nil {
		return refused(err)
	}
	if actor.PasswordChangeRequired || authz.Services(actor, "") != authz.ReasonNone {
		return RuntimeCheckChatDecisionOut{}, nil
	}
	decision, err := authz.ForActor(ctx, q, in.ActorID, in.CourseID, decidePerm.Perms, true, now)
	if err != nil {
		return refused(err)
	}
	if !decision.Level.Allowed() || decision.Member == nil {
		return RuntimeCheckChatDecisionOut{}, nil
	}
	result := func(allowed bool) RuntimeCheckChatDecisionOut {
		out := RuntimeCheckChatDecisionOut{Allowed: allowed}
		if allowed {
			out.ActionDecideLevel = decision.Level.String()
		}
		return out
	}

	// The question and answer were accepted earlier, but approval never
	// revives a participant whose actor, seat or principal no longer counts.
	opener, reason, err := authz.SeatFor(ctx, q, in.OpenerActorID, in.CourseID, true, now)
	if err != nil {
		return refused(err)
	}
	if reason != authz.ReasonNone || opener == nil {
		return RuntimeCheckChatDecisionOut{}, nil
	}
	agent, err := q.GetActor(ctx, in.AgentID)
	if err != nil {
		return refused(err)
	}
	if agent.Kind != "agent" {
		return RuntimeCheckChatDecisionOut{}, nil
	}
	respondent, reason, err := authz.SeatFor(ctx, q, in.AgentID, in.CourseID, true, now)
	if err != nil {
		return refused(err)
	}
	if reason != authz.ReasonNone || respondent == nil {
		return RuntimeCheckChatDecisionOut{}, nil
	}

	ownsRespondent := agent.OwnerActorID != nil && *agent.OwnerActorID == in.ActorID
	if in.Subject == RuntimeChatSubjectAnswer && ownsRespondent {
		// An owner may judge their agent's answer through ownAgentsGate even
		// when they do not oversee the opener. decidesAnswers still requires
		// their effective action_decide to be autonomous.
		ok, err := decidesAnswers(ctx, q, decision.Member, in.ActorID, in.AgentID)
		if err != nil {
			return refused(err)
		}
		return result(ok), nil
	}

	staff, err := oversees(ctx, q, decision.Member, opener.ID)
	if err != nil {
		return RuntimeCheckChatDecisionOut{}, err
	}
	if !staff {
		return RuntimeCheckChatDecisionOut{}, nil
	}
	if in.Subject == RuntimeChatSubjectAnswer {
		ok, err := decidesAnswers(ctx, q, decision.Member, in.ActorID, in.AgentID)
		if err != nil {
			return refused(err)
		}
		return result(ok), nil
	}

	// For a question the proposer is the opener, so four eyes is measured
	// against that actor, not against the agent being asked.
	same, err := q.SameParty(ctx, dbq.SamePartyParams{A: in.ActorID, B: in.OpenerActorID})
	if err != nil {
		return refused(err)
	}
	return result(!same), nil
}
