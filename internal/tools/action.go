package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func actionTools(d Deps) []tool.Tool {
	return []tool.Tool{
		actionDecide(d), actionReview(d), actionWithdraw(),
		actionListProposed(d), actionListPendingReview(d), actionListMine(), actionGet(),
	}
}

var decidePerm = tool.Gate{Perms: []domain.Perm{domain.PermActionDecide}}

// actionTarget resolves "an action in this course". perm_action_decide is
// not scoped (docs/schema.md §7), so no student or assignment is named.
func actionTarget(ctx context.Context, q dbq.Querier, courseID, actionID uuid.UUID) (tool.Target, error) {
	_, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: actionID, CourseID: &courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such action in this course")
	}
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: courseID, Type: "action", ID: &actionID}, nil
}

func actionDecide(d Deps) tool.Tool {
	return tool.Define(tool.Spec[pipeline.DecideIn, pipeline.DecideOut]{
		Name: pipeline.ToolActionDecide,
		Description: "Approve or reject a proposal: an action that was blocked before execution because its proposer " +
			"needs confirmation. Approving runs it now, as the proposer, after checking that the proposer is still " +
			"allowed to do it; if not, or if the proposal is too old, it is cancelled instead. Nobody decides their own proposal, " +
			"nor their owner's, nor another agent's of their owner, nor a decision someone else proposed about any of those, " +
			"nor approves closing an escalation they raised or approved. An agent's owner decides its proposal only where " +
			"they could do the same themselves without anyone's confirmation: their own level for it autonomous, and its " +
			"target within their reach; by_owner then says so.",
		Kind: tool.Write,
		Gate: decidePerm,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/actions/{action_id}/decide"},
		Resolve: func(ctx context.Context, q dbq.Querier, in pipeline.DecideIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in pipeline.DecideIn) (pipeline.DecideOut, error) {
			return d.Pipeline.Decide(ctx, ec, in)
		},
	})
}

func actionReview(d Deps) tool.Tool {
	return tool.Define(tool.Spec[pipeline.ReviewIn, pipeline.ReviewOut]{
		Name: pipeline.ToolActionReview,
		Description: "Record that an action which executed pending review has been looked at: reviewed, or escalated " +
			"for someone else to look at. Reviewing undoes nothing; putting something right is a separate action. Nobody " +
			"reviews their own action, their owner's or another agent's of their owner; an agent's owner reviews what it " +
			"did only where they could do the same themselves without anyone's confirmation.",
		Kind: tool.Write,
		Gate: decidePerm,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/actions/{action_id}/review"},
		Resolve: func(ctx context.Context, q dbq.Querier, in pipeline.ReviewIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in pipeline.ReviewIn) (pipeline.ReviewOut, error) {
			return d.Pipeline.Review(ctx, ec, in)
		},
	})
}

// ActionView is an action as the read tools show it.
type ActionView struct {
	ID                 uuid.UUID       `json:"id"`
	ActorID            uuid.UUID       `json:"actor_id"`
	MemberID           *uuid.UUID      `json:"member_id,omitempty"`
	ActionType         string          `json:"action_type"`
	TargetType         string          `json:"target_type"`
	TargetID           *uuid.UUID      `json:"target_id,omitempty"`
	Payload            json.RawMessage `json:"payload"`
	AuthzResult        string          `json:"authz_result"`
	Status             string          `json:"status"`
	DecidedByMemberID  *uuid.UUID      `json:"decided_by_member_id,omitempty"`
	DecidedAt          *time.Time      `json:"decided_at,omitempty"`
	ReviewState        string          `json:"review_state"`
	ReviewedByMemberID *uuid.UUID      `json:"reviewed_by_member_id,omitempty"`
	ReviewedAt         *time.Time      `json:"reviewed_at,omitempty"`
	ExecutedAt         *time.Time      `json:"executed_at,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	// YoursToDecide is set in the approval and review queues.
	YoursToDecide *bool `json:"yours_to_decide,omitempty" jsonschema:"in the approval and review queues: false when the action is yours, your owner's or another agent's of your owner, and when it is your own agent's and you could not do the same yourself without anyone's confirmation (your own level for it below autonomous, or its target beyond your reach): someone else decides and reviews those; true otherwise, though a decision about a decision may still be refused at one remove"`
}

func viewAction(a dbq.Action) ActionView {
	return ActionView{
		ID: a.ID, ActorID: a.ActorID, MemberID: a.MemberID, ActionType: a.ActionType,
		TargetType: a.TargetType, TargetID: a.TargetID, Payload: a.Payload,
		AuthzResult: string(a.AuthzResult), Status: a.Status,
		DecidedByMemberID: a.DecidedByMemberID, DecidedAt: a.DecidedAt,
		ReviewState: a.ReviewState, ReviewedByMemberID: a.ReviewedByMemberID, ReviewedAt: a.ReviewedAt,
		ExecutedAt: a.ExecutedAt, Result: a.Result, CreatedAt: a.CreatedAt,
	}
}

type ActionListIn struct {
	tool.InCourse
	Page
}

type ActionListOut struct {
	Actions []ActionView `json:"actions"`
	// Next is the cursor for the following page; absent on the last one.
	Next *uuid.UUID `json:"next,omitempty"`
}

func actionPage(rows []dbq.Action, limit int32) ActionListOut {
	out := ActionListOut{Actions: make([]ActionView, 0, len(rows))}
	for _, r := range rows {
		out.Actions = append(out.Actions, viewAction(r))
	}
	if len(rows) > 0 && len(rows) == int(limit) {
		out.Next = &rows[len(rows)-1].ID
	}
	return out
}

// queuePage is a page of a queue, each action saying whether the caller may
// decide or review it: not if it is the caller's own party's (pipeline.Decide,
// pipeline.Review), which the queue lists all the same, since it is the
// course's queue and someone else's to clear; but for the caller's own
// agent's, where the caller could have done it themselves without anyone's
// confirmation (pipeline.OwnerMayJudge), measured now as a decision would be.
func queuePage(ctx context.Context, rc *tool.ReadCtx, p *pipeline.Pipeline, rows []dbq.Action, limit int32) (ActionListOut, error) {
	out := actionPage(rows, limit)
	if len(rows) == 0 {
		return out, nil
	}
	actors := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		actors = append(actors, r.ActorID)
	}
	ours, err := rc.Q.SamePartyAmong(ctx, dbq.SamePartyAmongParams{ActorID: rc.Actor.ID, Ids: actors})
	if err != nil {
		return out, err
	}
	for i, r := range rows {
		yours := !slices.Contains(ours, r.ActorID)
		if !yours && rc.Member != nil {
			if yours, err = p.OwnerMayJudge(ctx, rc.Q, rc.Actor, rc.Member.ID, r, rc.Now); err != nil {
				return out, err
			}
		}
		out.Actions[i].YoursToDecide = &yours
	}
	return out, nil
}

func courseOnly(_ context.Context, _ dbq.Querier, in ActionListIn) (tool.Target, error) {
	return tool.Target{CourseID: in.CourseID, Type: "action"}, nil
}

func actionListProposed(d Deps) tool.Tool {
	return tool.Define(tool.Spec[ActionListIn, ActionListOut]{
		Name: "action.list_proposed",
		Description: "The approval queue: proposals in this course waiting for a decision, oldest first. yours_to_decide is " +
			"false on those of your own party, which someone else decides — yours, your owner's, your owner's other " +
			"agents', and your own agents' unless you could do the same yourself without anyone's confirmation.",
		Kind:    tool.Read,
		Gate:    decidePerm,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/proposed"},
		Resolve: courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			rows, err := rc.Q.ListProposedActions(ctx, dbq.ListProposedActionsParams{CourseID: &in.CourseID, After: in.after(), MaxRows: in.limit()})
			if err != nil {
				return ActionListOut{}, err
			}
			return queuePage(ctx, rc, d.Pipeline, rows, in.limit())
		},
	})
}

func actionListPendingReview(d Deps) tool.Tool {
	return tool.Define(tool.Spec[ActionListIn, ActionListOut]{
		Name: "action.list_pending_review",
		Description: "The review queue: actions that executed pending review and have not been reviewed, or were escalated. " +
			"yours_to_decide is false on those of your own party, which someone else reviews — yours, your owner's, your " +
			"owner's other agents', and your own agents' unless you could do the same yourself without anyone's confirmation.",
		Kind:    tool.Read,
		Gate:    decidePerm,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/pending-review"},
		Resolve: courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			rows, err := rc.Q.ListPendingReviewActions(ctx, dbq.ListPendingReviewActionsParams{CourseID: &in.CourseID, After: in.after(), MaxRows: in.limit()})
			if err != nil {
				return ActionListOut{}, err
			}
			return queuePage(ctx, rc, d.Pipeline, rows, in.limit())
		},
	})
}

// ActionListMineIn is a page of the caller's own actions.
type ActionListMineIn struct {
	tool.InCourse
	ExcludeTypes []string `json:"exclude_types,omitempty" jsonschema:"action types to leave out, such as conversation.ask and conversation.answer"`
	Page
}

// actionListMine lets any member follow its own attempts. For an agent it is
// the way to find out what became of a proposal, alongside the event feed.
// It is gated by perm_document_read only because every seated member has
// some permission and this is the most basic one; what it returns is limited
// to the caller's own rows by the query, not by the gate.
func actionListMine() tool.Tool {
	return tool.Define(tool.Spec[ActionListMineIn, ActionListOut]{
		Name: "action.list_mine",
		Description: "The caller's own actions in this course — proposals and their outcomes included — oldest first. " +
			"exclude_types leaves out whole kinds of action: a chat's messages, say.",
		Kind: tool.Read,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/mine"},
		Resolve: func(_ context.Context, _ dbq.Querier, in ActionListMineIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "action"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListMineIn) (ActionListOut, error) {
			exclude := in.ExcludeTypes
			if exclude == nil {
				exclude = []string{}
			}
			rows, err := rc.Q.ListActionsByMember(ctx, dbq.ListActionsByMemberParams{
				CourseID: &in.CourseID, MemberID: &rc.Member.ID, After: in.after(), MaxRows: in.limit(), ExcludeTypes: exclude,
			})
			return actionPage(rows, in.limit()), err
		},
	})
}

type ActionGetIn struct {
	tool.InCourse
	ActionID uuid.UUID `json:"action_id"`
}

func actionGet() tool.Tool {
	return tool.Define(tool.Spec[ActionGetIn, ActionView]{
		Name:        "action.get",
		Description: "One action in full: what was asked, how it was authorized, what became of it, who decided or reviewed it.",
		Kind:        tool.Read,
		Gate:        decidePerm,
		HTTP:        tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/{action_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActionGetIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionGetIn) (ActionView, error) {
			a, err := rc.Q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: in.ActionID, CourseID: &in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return ActionView{}, apperr.Missing("no such action in this course")
			}
			return viewAction(a), err
		},
	})
}

type ActionWithdrawIn struct {
	tool.InCourse
	ActionID uuid.UUID `json:"action_id" jsonschema:"your proposal that is still waiting for a decision"`
}

// actionWithdraw lets a proposer take back what they proposed while nobody
// has decided it: a student who asked to bring an agent in and thought
// better of it, say. It is gated by perm_document_read, like action.list_mine,
// only because every seated member holds some permission and this is the most
// basic one; that the proposal is the caller's own is what the tool checks.
// The actor is compared, not the seat: someone removed and seated again is
// still who made it.
func actionWithdraw() tool.Tool {
	return tool.Define(tool.Spec[ActionWithdrawIn, OK]{
		Name: "action.withdraw",
		Description: "Take back a proposal of yours that is still waiting for a decision. It is cancelled, and nothing of " +
			"it is carried out. A proposal already decided, or someone else's, cannot be withdrawn.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/actions/{action_id}/withdraw"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActionWithdrawIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActionWithdrawIn) (OK, error) {
			prop, err := ec.Q.GetActionInCourseForUpdate(ctx, dbq.GetActionInCourseForUpdateParams{ID: in.ActionID, CourseID: &in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return OK{}, apperr.Missing("no such action in this course")
			}
			if err != nil {
				return OK{}, err
			}
			if prop.ActorID != ec.Actor.ID {
				return OK{}, apperr.Forbid("only whoever proposed it withdraws a proposal")
			}
			if prop.Status != string(domain.StatusProposed) {
				return OK{}, apperr.Conflicts("the action is %s, not awaiting a decision", prop.Status)
			}
			_, stored := pipeline.Cancellation(pipeline.CancelWithdrawn, nil)
			n, err := ec.Q.CancelProposal(ctx, dbq.CancelProposalParams{ID: prop.ID, Result: stored})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the proposal was decided just now")
			}
			// Filed under the proposal, so that it reads in the proposer's
			// feed like any other end of a proposal.
			id := prop.ID
			ec.Emit(events.Event{Type: events.ActionCancelled, CourseID: prop.CourseID, ActionID: &id,
				SubjectType: "action", SubjectID: &id,
				Payload: map[string]any{"action_type": prop.ActionType, "reason": pipeline.CancelWithdrawn, "by_action_id": ec.ActionID}})
			return OK{OK: true}, nil
		},
	})
}
