package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func actionTools(d Deps) []tool.Tool {
	return []tool.Tool{
		actionDecide(d), actionReview(d),
		actionListProposed(), actionListPendingReview(), actionListMine(), actionGet(),
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
			"nor a decision someone else proposed about it, nor approves closing an escalation they raised or approved.",
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
			"for someone else to look at. Reviewing undoes nothing; putting something right is a separate action.",
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

func courseOnly(_ context.Context, _ dbq.Querier, in ActionListIn) (tool.Target, error) {
	return tool.Target{CourseID: in.CourseID, Type: "action"}, nil
}

func actionListProposed() tool.Tool {
	return tool.Define(tool.Spec[ActionListIn, ActionListOut]{
		Name:        "action.list_proposed",
		Description: "The approval queue: proposals in this course waiting for a decision, oldest first.",
		Kind:        tool.Read,
		Gate:        decidePerm,
		HTTP:        tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/proposed"},
		Resolve:     courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			rows, err := rc.Q.ListProposedActions(ctx, dbq.ListProposedActionsParams{CourseID: &in.CourseID, After: in.after(), MaxRows: in.limit()})
			return actionPage(rows, in.limit()), err
		},
	})
}

func actionListPendingReview() tool.Tool {
	return tool.Define(tool.Spec[ActionListIn, ActionListOut]{
		Name:        "action.list_pending_review",
		Description: "The review queue: actions that executed pending review and have not been reviewed, or were escalated.",
		Kind:        tool.Read,
		Gate:        decidePerm,
		HTTP:        tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/pending-review"},
		Resolve:     courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			rows, err := rc.Q.ListPendingReviewActions(ctx, dbq.ListPendingReviewActionsParams{CourseID: &in.CourseID, After: in.after(), MaxRows: in.limit()})
			return actionPage(rows, in.limit()), err
		},
	})
}

// action.list_mine lets any member follow its own attempts. For an agent it is
// the way to find out what became of a proposal, alongside the event feed.
// It is gated by perm_document_read only because every seated member has
// some permission and this is the most basic one; what it returns is limited
// to the caller's own rows by the query, not by the gate.
func actionListMine() tool.Tool {
	return tool.Define(tool.Spec[ActionListIn, ActionListOut]{
		Name:        "action.list_mine",
		Description: "The caller's own actions in this course — proposals and their outcomes included — oldest first.",
		Kind:        tool.Read,
		Gate:        tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP:        tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/mine"},
		Resolve:     courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			rows, err := rc.Q.ListActionsByMember(ctx, dbq.ListActionsByMemberParams{
				CourseID: &in.CourseID, MemberID: &rc.Member.ID, After: in.after(), MaxRows: in.limit(),
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
