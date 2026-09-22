package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// Deciding and reviewing are themselves tools (action.decide, action.review),
// gated by perm_action_decide, so each decision is an action in the log with
// its own row. The logic lives here, beside the rest of the status table; the
// tools in package tools are thin wrappers.

const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

type DecideIn struct {
	tool.InCourse
	ActionID uuid.UUID `json:"action_id" jsonschema:"the proposal being decided"`
	Decision string    `json:"decision" jsonschema:"approve or reject"`
	Reason   *string   `json:"reason,omitempty" jsonschema:"why; shown to the proposer"`
}

// DecideOut reports what became of the proposal, which is not the same as
// what became of the decision: approving a proposal whose proposer has since
// lost the permission succeeds as a decision and cancels the proposal.
type DecideOut struct {
	ActionID uuid.UUID `json:"action_id"`
	// Outcome is the proposal's new status: executed, failed, rejected or
	// cancelled.
	Outcome domain.ActionStatus `json:"outcome"`
	Result  json.RawMessage     `json:"result,omitempty"`
	Error   *apperr.Error       `json:"error,omitempty"`
}

// Cancellation reasons recorded on a proposal.
const (
	CancelExpired          = "proposal_expired"
	CancelReauthorization  = "reauthorization_failed"
	CancelTargetGone       = "target_gone"
	CancelMemberRemoved    = "member_removed"
	CancelToolNoLongerHere = "tool_removed"
)

// Decide approves or rejects a proposal. It runs as the Execute of
// action.decide, inside that action's savepoint.
func (p *Pipeline) Decide(ctx context.Context, ec *tool.ExecCtx, in DecideIn) (DecideOut, error) {
	if in.Decision != DecisionApprove && in.Decision != DecisionReject {
		return DecideOut{}, apperr.Invalid("decision must be %q or %q", DecisionApprove, DecisionReject)
	}
	prop, err := ec.Q.GetActionInCourseForUpdate(ctx, dbq.GetActionInCourseForUpdateParams{ID: in.ActionID, CourseID: &in.CourseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DecideOut{}, apperr.Missing("no such action in this course")
	}
	if err != nil {
		return DecideOut{}, err
	}
	if prop.Status != string(domain.StatusProposed) {
		return DecideOut{}, apperr.Conflicts("the action is %s, not awaiting a decision", prop.Status)
	}
	if ec.Member == nil || prop.MemberID == nil {
		return DecideOut{}, apperr.Forbid("only a course member decides a member's proposal")
	}
	if *prop.MemberID == ec.Member.ID {
		// The database refuses this too; saying so here is kinder.
		return DecideOut{}, apperr.Forbid("nobody decides their own proposal")
	}
	out := DecideOut{ActionID: prop.ID}

	if p.cfg.ProposalTTL > 0 && prop.CreatedAt.Add(p.cfg.ProposalTTL).Before(ec.Now) {
		return p.cancel(ctx, ec, prop, CancelExpired, nil)
	}

	if in.Decision == DecisionReject {
		res, _ := json.Marshal(map[string]any{"decision": map[string]any{
			"decision": DecisionReject, "reason": in.Reason, "by_action_id": ec.ActionID,
		}})
		if err := finish(ctx, ec.Q, prop.ID, domain.StatusRejected, &ec.Member.ID, ec, false, res); err != nil {
			return DecideOut{}, err
		}
		ec.Emit(proposalEvent(events.ActionRejected, prop, ec.ActionID, nil))
		out.Outcome = domain.StatusRejected
		return out, nil
	}

	// Approve. The world may have moved since the proposal was made, so the
	// proposer is authorized again, now, against the membership row the
	// proposal was made under, and the target is looked up again.
	t, ok := p.reg.Get(prop.ActionType)
	if !ok {
		return p.cancel(ctx, ec, prop, CancelToolNoLongerHere, nil)
	}
	args, err := t.Decode(prop.Payload)
	if err != nil {
		return p.cancel(ctx, ec, prop, CancelToolNoLongerHere, map[string]any{"detail": err.Error()})
	}
	proposer, err := authz.LoadActor(ctx, ec.Q, prop.ActorID)
	if err != nil {
		return DecideOut{}, err
	}
	a, err := p.authorize(ctx, ec.Q, t, args, proposer, prop.MemberID, ec.Now)
	switch {
	case apperr.Is(err, apperr.NotFound):
		return p.cancel(ctx, ec, prop, CancelTargetGone, nil)
	case err != nil:
		return DecideOut{}, err
	case !a.decision.Level.Allowed():
		return p.cancel(ctx, ec, prop, CancelReauthorization, map[string]any{"authz_reason": string(a.decision.Reason)})
	}

	// Approved says what the approver did; outcome says what came of it. A
	// proposer reading the feed must be able to tell an approval that ran
	// from one that was refused by the domain, or it would take the second
	// for the first.
	fail := func(e *apperr.Error) (DecideOut, error) {
		if err := finish(ctx, ec.Q, prop.ID, domain.StatusFailed, &ec.Member.ID, ec, false, errorResult(e)); err != nil {
			return DecideOut{}, err
		}
		ec.Emit(proposalEvent(events.ActionApproved, prop, ec.ActionID, map[string]any{"outcome": domain.StatusFailed, "error": e.Code}))
		out.Outcome, out.Error = domain.StatusFailed, e
		return out, nil
	}
	if t.Validate != nil {
		if err := t.Validate(ctx, ec.Q, a.decision.Member, args); err != nil {
			e, ok := isCallerFault(err)
			if !ok {
				return DecideOut{}, err
			}
			return fail(e)
		}
	}

	// Execute as the proposer, under the proposal's id: the grade it creates
	// names the proposal as the action that made it, and the proposal names
	// who approved it. That chain is the whole answer to "who marked this?".
	child := &events.Buffer{}
	res, err := savepoint(ctx, ec.Tx, func(sp pgx.Tx) (any, error) {
		return t.Execute(ctx, &tool.ExecCtx{
			Tx: sp, Q: dbq.New(sp), Actor: proposer, Member: a.decision.Member,
			ActionID: prop.ID, Now: ec.Now, ActionCreatedAt: prop.CreatedAt, Emit: stamp(child, prop.ID),
		}, args)
	})
	if err != nil {
		e, ok := isCallerFault(err)
		if !ok {
			return DecideOut{}, fmt.Errorf("%s: %w", t.Name, err)
		}
		return fail(e)
	}
	full, err := json.Marshal(res)
	if err != nil {
		return DecideOut{}, fmt.Errorf("%s: result: %w", t.Name, err)
	}
	if err := finish(ctx, ec.Q, prop.ID, domain.StatusExecuted, &ec.Member.ID, ec, true, stripTopLevel(full, t.SecretOut)); err != nil {
		return DecideOut{}, err
	}
	ec.Emit(proposalEvent(events.ActionApproved, prop, ec.ActionID, map[string]any{"outcome": domain.StatusExecuted}))
	child.Drain(ec.Emit)
	out.Outcome, out.Result = domain.StatusExecuted, full
	return out, nil
}

// cancel ends a proposal without executing it and without blaming anyone: it
// was fine when it was made and is not any more.
func (p *Pipeline) cancel(ctx context.Context, ec *tool.ExecCtx, prop dbq.Action, code string, details map[string]any) (DecideOut, error) {
	e, stored := Cancellation(code, details)
	if err := finish(ctx, ec.Q, prop.ID, domain.StatusCancelled, nil, ec, false, stored); err != nil {
		return DecideOut{}, err
	}
	ec.Emit(proposalEvent(events.ActionCancelled, prop, ec.ActionID, map[string]any{"reason": code}))
	return DecideOut{ActionID: prop.ID, Outcome: domain.StatusCancelled, Error: e}, nil
}

// Cancellation builds the error a cancelled proposal carries, and the form of
// it stored in action.result. Whoever cancels a proposal — a decision that
// found it stale, the removal of its proposer, the expiry sweep — records it
// the same way, so a replay reads the same whichever it was.
func Cancellation(code string, details map[string]any) (*apperr.Error, []byte) {
	e := apperr.Precondition("the proposal can no longer be carried out").With("reason", code)
	for k, v := range details {
		e = e.With(k, v)
	}
	return e, errorResult(e)
}

// finish moves a proposal to its end state.
func finish(ctx context.Context, q *dbq.Queries, id uuid.UUID, status domain.ActionStatus, decidedBy *uuid.UUID, ec *tool.ExecCtx, executed bool, result []byte) error {
	if !canFollow(domain.ConfirmRequired, domain.StatusProposed, status) {
		return fmt.Errorf("a proposal cannot become %s", status)
	}
	arg := dbq.FinishProposalParams{ID: id, Status: string(status), DecidedByMemberID: decidedBy, Result: result}
	if decidedBy != nil {
		arg.DecidedAt = &ec.Now
	}
	if executed {
		arg.ExecutedAt = &ec.Now
	}
	n, err := q.FinishProposal(ctx, arg)
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.Conflicts("the proposal was decided by someone else just now")
	}
	return nil
}

// proposalEvent is filed under the proposal's id, not the decision's, so that
// the proposer — who may see nothing else in the course's action log — finds
// it in its own feed. For a pull-based agent this is how it learns the answer.
func proposalEvent(typ string, prop dbq.Action, byAction uuid.UUID, extra map[string]any) events.Event {
	id := prop.ID
	payload := map[string]any{"action_type": prop.ActionType, "by_action_id": byAction}
	for k, v := range extra {
		payload[k] = v
	}
	return events.Event{
		Type: typ, CourseID: prop.CourseID, ActionID: &id,
		SubjectType: "action", SubjectID: &id, Payload: payload,
	}
}

const (
	ReviewReviewed  = "reviewed"
	ReviewEscalated = "escalated"
)

type ReviewIn struct {
	tool.InCourse
	ActionID uuid.UUID `json:"action_id" jsonschema:"the executed action under review"`
	Outcome  string    `json:"outcome" jsonschema:"reviewed or escalated"`
	Note     *string   `json:"note,omitempty"`
}

type ReviewOut struct {
	ActionID    uuid.UUID          `json:"action_id"`
	ReviewState domain.ReviewState `json:"review_state"`
}

// Review records that a human has looked at a pending_review action after the
// fact. It undoes nothing. Putting right what the action did is another
// action (a regrade), with its own row.
func (p *Pipeline) Review(ctx context.Context, ec *tool.ExecCtx, in ReviewIn) (ReviewOut, error) {
	if in.Outcome != ReviewReviewed && in.Outcome != ReviewEscalated {
		return ReviewOut{}, apperr.Invalid("outcome must be %q or %q", ReviewReviewed, ReviewEscalated)
	}
	row, err := ec.Q.GetActionInCourseForUpdate(ctx, dbq.GetActionInCourseForUpdateParams{ID: in.ActionID, CourseID: &in.CourseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewOut{}, apperr.Missing("no such action in this course")
	}
	if err != nil {
		return ReviewOut{}, err
	}
	if from := domain.ReviewState(row.ReviewState); !canReview(from, domain.ReviewState(in.Outcome)) {
		if from == domain.ReviewEscalated {
			return ReviewOut{}, apperr.Conflicts("the action is already escalated")
		}
		return ReviewOut{}, apperr.Conflicts("the action is not awaiting review")
	}
	if ec.Member == nil {
		return ReviewOut{}, apperr.Forbid("only a course member reviews")
	}
	if row.MemberID != nil && *row.MemberID == ec.Member.ID {
		return ReviewOut{}, apperr.Forbid("nobody reviews their own action")
	}
	n, err := ec.Q.SetActionReview(ctx, dbq.SetActionReviewParams{
		ID: row.ID, ReviewState: in.Outcome, ReviewedByMemberID: &ec.Member.ID, ReviewedAt: &ec.Now,
	})
	if err != nil {
		return ReviewOut{}, err
	}
	if n == 0 {
		return ReviewOut{}, apperr.Conflicts("the action was reviewed by someone else just now")
	}
	typ := events.ActionReviewed
	if in.Outcome == ReviewEscalated {
		typ = events.ActionEscalated
	}
	ec.Emit(proposalEvent(typ, row, ec.ActionID, nil))
	return ReviewOut{ActionID: row.ID, ReviewState: domain.ReviewState(in.Outcome)}, nil
}
