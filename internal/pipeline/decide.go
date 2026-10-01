package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Deciding and reviewing are themselves tools (action.decide, action.review),
// gated by perm_action_decide, so each decision is an action in the log with
// its own row. The logic lives here, beside the rest of the status table; the
// tools in package tools are thin wrappers.

// The names of the two tools. A decision can be about a decision, so the
// pipeline has to tell them from the rest (judgesOwn).
const (
	ToolActionDecide = "action.decide"
	ToolActionReview = "action.review"
)

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
	// ByOwner says the decider was the owner of the agent that proposed it,
	// deciding what they could have done themselves (ownerJudges).
	ByOwner bool `json:"by_owner,omitempty" jsonschema:"true when you decided as the owner of the agent that proposed it"`
}

// Cancellation reasons recorded on a proposal.
const (
	CancelExpired          = "proposal_expired"
	CancelReauthorization  = "reauthorization_failed"
	CancelTargetGone       = "target_gone"
	CancelMemberRemoved    = "member_removed"
	CancelToolNoLongerHere = "tool_removed"
	// The proposer took it back (action.withdraw).
	CancelWithdrawn = "withdrawn"
)

// CheckDecision is action.decide's Check: a decision is to approve or to
// reject.
func CheckDecision(in DecideIn) error {
	if in.Decision != DecisionApprove && in.Decision != DecisionReject {
		return apperr.Invalid("decision must be %q or %q", DecisionApprove, DecisionReject)
	}
	return nil
}

// errNoAction is what an action id that is not one of the course's answers.
var errNoAction = apperr.Missing("no such action in this course")

// refuseDecision says why actor, from seat m, may not decide proposal prop
// as decision says now, or nil when they may; and, when they may, whether
// they decide it as the owner of the agent that proposed it (byOwner). It
// writes nothing. Decide asks it of the proposal under its lock, and
// action.decide's Validate (ValidateDecision) before a decision is proposed,
// carried out or approved, so that nobody is asked to approve a decision
// that approving would refuse. What it asks can change while a proposed
// decision waits — the proposal decided by someone else, withdrawn, its
// agent's owner's level for it — and is asked again when it is approved.
func (p *Pipeline) refuseDecision(ctx context.Context, q dbq.Querier, actor domain.Actor, m *domain.Member, prop dbq.Action, decision string, now time.Time) (byOwner bool, err error) {
	if prop.Status != string(domain.StatusProposed) {
		return false, apperr.Conflicts("the action is %s, not awaiting a decision", prop.Status)
	}
	if m == nil || prop.MemberID == nil {
		return false, apperr.Forbid("only a course member decides a member's proposal")
	}
	if same, err := sameParty(ctx, q, prop.ActorID, actor.ID); err != nil {
		return false, err
	} else if *prop.MemberID == m.ID || same {
		// The database refuses the same seat too; saying so here is kinder.
		// The actor is compared here as well: someone removed and seated
		// again has a new seat, and is still who made the proposal. So is
		// the party: an agent decides nothing its owner proposed, nor
		// another of the owner's agents anything it did. Its owner decides
		// what it proposed only where they could have done it themselves
		// without anyone's confirmation (ownerJudges).
		owner, may, refused, err := p.ownerJudges(ctx, q, actor, m.ID, prop, now)
		switch {
		case err != nil:
			return false, err
		case owner && refused != nil:
			return false, errOwnerWouldBeRefused(refused)
		case owner && !may:
			return false, errOwnerNotAutonomous("decide")
		case !may:
			return false, apperr.Forbid("nobody decides their own proposal, nor their owner's, nor another agent's of their owner")
		}
		byOwner = true
	}
	if own, err := judgesOwn(ctx, q, prop, m.ID, actor.ID); err != nil {
		return false, err
	} else if own {
		return false, apperr.Forbid("nobody decides their own proposal, even at one remove: this one decides or reviews an action of yours")
	}
	if decision == DecisionApprove {
		if own, err := closesOwnEscalation(ctx, q, prop, actor.ID); err != nil {
			return false, err
		} else if own {
			return false, apperr.Forbid("an escalation is for someone else to look at")
		}
	}
	return byOwner, nil
}

// ValidateDecision is action.decide's Validate: what Decide would refuse the
// decision in, made from seat m at now, before anything of it is decided
// (refuseDecision), asked of the proposal under the lock Decide takes it
// with (lockProposal), which the call keeps.
func (p *Pipeline) ValidateDecision(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in DecideIn) error {
	prop, err := lockProposal(ctx, q, in.CourseID, in.ActionID)
	if err != nil {
		return err
	}
	actor, err := seatActor(ctx, q, m)
	if err != nil {
		return err
	}
	_, err = p.refuseDecision(ctx, q, actor, m, prop, in.Decision, now)
	return err
}

// seatActor is the actor of seat m, which a decision or a review is made
// from; the zero actor for no seat, which both refuse.
func seatActor(ctx context.Context, q dbq.Querier, m *domain.Member) (domain.Actor, error) {
	if m == nil {
		return domain.Actor{}, nil
	}
	return authz.LoadActor(ctx, q, m.ActorID)
}

// lockProposal takes proposal id of course for a decision about it: the
// proposer's seat first, then the proposal, the order a removal of that seat
// takes them in (the seat, then its proposals) and the order every write
// takes its caller's seat in. A pause, narrowing or removal of the proposer
// then waits for the decision, or the decision waits for it and sees what
// it did. For a delegate's proposal its principal's seat comes second, as a
// delegate's own calls take the two: whatever the principal loses then
// applies to the approval too. Whose proposal it is, and whose delegate that
// seat is, never change, so they are read before anything is locked.
//
// Decide takes it so, and ValidateDecision before it, ahead of whatever the
// proposed tool's Validate locks as it asks whether approving would be
// refused (ownerJudges): an approval takes the proposal and then those, and
// an owner's decision must not take them the other way round.
func lockProposal(ctx context.Context, q dbq.Querier, course, id uuid.UUID) (dbq.Action, error) {
	if ahead, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: id, CourseID: &course}); err == nil && ahead.MemberID != nil {
		if err := q.ShareSeats(ctx, []uuid.UUID{*ahead.MemberID}); err != nil {
			return dbq.Action{}, err
		}
		principal, err := q.GetSeatPrincipal(ctx, *ahead.MemberID)
		if err != nil {
			return dbq.Action{}, err
		}
		if principal != nil {
			if err := q.ShareSeats(ctx, []uuid.UUID{*principal}); err != nil {
				return dbq.Action{}, err
			}
		}
	}
	prop, err := q.GetActionInCourseForUpdate(ctx, dbq.GetActionInCourseForUpdateParams{ID: id, CourseID: &course})
	if errors.Is(err, pgx.ErrNoRows) {
		return prop, errNoAction
	}
	return prop, err
}

// Decide approves or rejects a proposal. It runs as the Execute of
// action.decide, inside that action's savepoint, on a decision CheckDecision
// has taken.
func (p *Pipeline) Decide(ctx context.Context, ec *tool.ExecCtx, in DecideIn) (DecideOut, error) {
	prop, err := lockProposal(ctx, ec.Q, in.CourseID, in.ActionID)
	if err != nil {
		return DecideOut{}, err
	}
	byOwner, err := p.refuseDecision(ctx, ec.Q, ec.Actor, ec.Member, prop, in.Decision, ec.Now)
	if err != nil {
		return DecideOut{}, err
	}
	out := DecideOut{ActionID: prop.ID, ByOwner: byOwner}
	// What the event says of the decision, and the record of a rejection:
	// that the proposer's owner made it, when they did.
	said := func(m map[string]any) map[string]any {
		if byOwner {
			if m == nil {
				m = map[string]any{}
			}
			m["by_owner"] = true
		}
		return m
	}
	cancel := func(code string, details map[string]any) (DecideOut, error) {
		o, err := p.cancel(ctx, ec, prop, code, details)
		o.ByOwner = byOwner
		return o, err
	}

	if p.cfg.ProposalTTL > 0 && prop.CreatedAt.Add(p.cfg.ProposalTTL).Before(ec.Now) {
		return cancel(CancelExpired, nil)
	}

	if in.Decision == DecisionReject {
		res, _ := json.Marshal(map[string]any{"decision": said(map[string]any{
			"decision": DecisionReject, "reason": in.Reason, "by_action_id": ec.ActionID,
		})})
		if err := finish(ctx, ec.Q, prop.ID, domain.StatusRejected, &ec.Member.ID, ec, false, res); err != nil {
			return DecideOut{}, err
		}
		ec.Emit(proposalEvent(events.ActionRejected, prop, ec.ActionID, said(nil)))
		out.Outcome = domain.StatusRejected
		return out, nil
	}

	// Approve. The world may have moved since the proposal was made, so the
	// proposer is authorized again, now, against the membership row the
	// proposal was made under, and the target is looked up again. An owner
	// who approves has passed their own authorization for it just now; the
	// proposer's is what the approval carries out, and it is checked all
	// the same.
	t, ok := p.reg.Get(prop.ActionType)
	if !ok {
		return cancel(CancelToolNoLongerHere, nil)
	}
	// Read back by the schema alone: arguments it no longer takes are a tool
	// that is no longer here, and arguments the tool's Check refuses fail
	// below, as Execute would have failed them.
	args, err := t.Parse(prop.Payload)
	if err != nil {
		return cancel(CancelToolNoLongerHere, map[string]any{"detail": err.Error()})
	}
	proposer, err := authz.LoadActor(ctx, ec.Q, prop.ActorID)
	if err != nil {
		return DecideOut{}, err
	}
	a, err := p.authorize(ctx, ec.Q, t, args, proposer, uuid.Nil, prop.MemberID, ec.Now)
	switch {
	case apperr.Is(err, apperr.NotFound):
		return cancel(CancelTargetGone, nil)
	case err != nil:
		return DecideOut{}, err
	case !a.decision.Level.Allowed():
		return cancel(CancelReauthorization, map[string]any{"authz_reason": string(a.decision.Reason)})
	}

	// Approved says what the approver did; outcome says what came of it. A
	// proposer reading the feed must be able to tell an approval that ran
	// from one that was refused by the domain, or it would take the second
	// for the first.
	fail := func(e *apperr.Error) (DecideOut, error) {
		if err := finish(ctx, ec.Q, prop.ID, domain.StatusFailed, &ec.Member.ID, ec, false, errorResult(e)); err != nil {
			return DecideOut{}, err
		}
		ec.Emit(proposalEvent(events.ActionApproved, prop, ec.ActionID, said(map[string]any{"outcome": domain.StatusFailed, "error": e.Code})))
		out.Outcome, out.Error = domain.StatusFailed, e
		return out, nil
	}
	// A proposal queued before Check refused what it says: nothing that
	// Check refuses is queued now.
	if t.Check != nil {
		if err := t.Check(args); err != nil {
			e, ok := apperr.As(err)
			if !ok {
				return DecideOut{}, err
			}
			return fail(e)
		}
	}
	if t.Validate != nil {
		if err := validate(ctx, ec.Tx, t, a.decision.Member, ec.Now, args); err != nil {
			if transient(err) {
				// Lost a deadlock: nothing is wrong with the proposal. The
				// decision is undone and made again, once, in a fresh
				// transaction (see inTx). If it loses again it is recorded
				// as failed, the proposal still waits, and deciding again
				// can work.
				return DecideOut{}, err
			}
			e, ok := isCallerFault(err)
			if !ok {
				return DecideOut{}, err
			}
			return fail(e)
		}
	}
	// What changed since it was proposed, which no call is refused for.
	if t.Since != nil {
		if err := t.Since(ctx, ec.Q, prop.CreatedAt, args); err != nil {
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
			ActionID: prop.ID, Now: ec.Now, ActionCreatedAt: prop.CreatedAt, Approved: true, Emit: stamp(child, prop.ID),
		}, args)
	})
	if err != nil {
		if transient(err) {
			return DecideOut{}, fmt.Errorf("%s: %w", t.Name, err)
		}
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
	ec.Emit(proposalEvent(events.ActionApproved, prop, ec.ActionID, said(map[string]any{"outcome": domain.StatusExecuted})))
	child.Drain(ec.Emit)
	out.Outcome, out.Result = domain.StatusExecuted, full
	return out, nil
}

// sameParty reports whether two actors are one party for four eyes: the same
// actor, one of them the other's owner, or two agents of one owner. An agent
// someone owns acts only as their delegate, so it is them, at one remove.
// One party judges nothing of its own, but for its owner's judging an
// agent's action they could have done themselves (ownerJudges).
func sameParty(ctx context.Context, q dbq.Querier, a, b uuid.UUID) (bool, error) {
	if a == b {
		return true, nil
	}
	return q.SameParty(ctx, dbq.SamePartyParams{A: a, B: b})
}

// ownerJudges reports whether actor, deciding or reviewing a from its seat,
// is the owner of the agent that did a (owner), and, if so, whether they may
// (may): whether they could have done a themselves just now without anyone's
// confirmation. That is their own authorization for the very same call, as
// authorize() would give it them from that seat if they made it now: every
// permission that gates it held at autonomous, and its target within their
// reach; for a tool whose permission no person holds, what the tool says
// they are measured by instead (tool.Spec.OwnerJudgedBy: for an answer,
// action_decide). Where their own level is lower, the course has someone
// check them too, and so their agent: someone outside the party decides it. It is the
// one case in which a party judges its own, and only at no remove: nobody
// else of the party — the agent itself, its sibling — and nobody judging an
// action of the party through a decision about it (judgesOwn) is let by it.
// It is the same rule for approving and rejecting, and for reviewing.
//
// A proposal they could not have made themselves without its being refused
// is not theirs to decide either: one whose arguments the tool's Check
// refuses, or that its Validate refuses as approving it now would run it,
// as the proposer's (refusal). That refusal is returned as refused, for
// Decide to say why; an owner who wants such a proposal gone takes it back
// (action.withdraw), and someone else may reject it. What has been carried
// out already, reviewed afterwards, is not asked again.
func (p *Pipeline) ownerJudges(ctx context.Context, q dbq.Querier, actor domain.Actor, seat uuid.UUID, a dbq.Action, now time.Time) (owner, may bool, refused *apperr.Error, err error) {
	if a.MemberID == nil || *a.MemberID == seat || a.ActorID == actor.ID {
		return false, false, nil, nil
	}
	did, err := q.GetActor(ctx, a.ActorID)
	if err != nil {
		return false, false, nil, err
	}
	if did.OwnerActorID == nil || *did.OwnerActorID != actor.ID {
		return false, false, nil, nil
	}
	t, ok := p.reg.Get(a.ActionType)
	if !ok {
		return true, false, nil, nil
	}
	gated := t
	if len(t.OwnerJudgedBy) > 0 {
		// A permission no person holds, conversation_answer: the owner is
		// measured by what judging it is instead (tool.Spec.OwnerJudgedBy).
		gated.Gate.Perms, gated.Gate.Any, gated.Gate.OwnAgents = t.OwnerJudgedBy, false, nil
	}
	args, err := t.Parse(a.Payload)
	if err != nil {
		return true, false, nil, nil
	}
	got, err := p.authorize(ctx, q, gated, args, actor, uuid.Nil, &seat, now)
	if err != nil {
		// A target gone, or not found in the course: not something they
		// could do now. Anything else is a fault, and says so.
		if _, ok := apperr.As(err); ok {
			return true, false, nil, nil
		}
		return true, false, nil, err
	}
	if got.decision.Level != domain.Autonomous {
		return true, false, nil, nil
	}
	if a.Status == string(domain.StatusProposed) {
		if refused, err = refusal(ctx, q, t, a, args, now); err != nil {
			return true, false, nil, unsure{err}
		}
		if refused != nil {
			return true, false, refused, nil
		}
	}
	return true, true, nil, nil
}

// unsure is a fault met while asking whether approving a proposal would be
// refused (refusal): a decision fails on it, as approving would, while a
// queue does not say the proposal is the owner's to decide
// (OwnerMayJudgeListed).
type unsure struct{ err error }

func (u unsure) Error() string { return u.err.Error() }
func (u unsure) Unwrap() error { return u.err }

// refusal is what approving proposal a, of tool t with arguments args, would
// be refused with at now for what it asks, before anything is carried out:
// the tool's Check, its Validate, run as approving it runs it, against the
// proposer's seat, and its Since, of what changed since a was proposed. nil
// when none refuses it. It writes nothing; Validate may take its locks,
// which q's transaction, if it has one, keeps.
func refusal(ctx context.Context, q dbq.Querier, t tool.Tool, a dbq.Action, args any, now time.Time) (*apperr.Error, error) {
	refused := func(err error) (*apperr.Error, error) {
		if e, ok := apperr.As(err); ok {
			return e, nil
		}
		return nil, err
	}
	if t.Check != nil {
		if err := t.Check(args); err != nil {
			return refused(err)
		}
	}
	if t.Validate != nil && a.MemberID != nil {
		proposer, err := authz.LoadMember(ctx, q, *a.MemberID)
		if err != nil {
			return refused(err)
		}
		if err := t.Validate(ctx, q, proposer, now, args); err != nil {
			return refused(err)
		}
	}
	if t.Since != nil {
		if err := t.Since(ctx, q, a.CreatedAt, args); err != nil {
			return refused(err)
		}
	}
	return nil, nil
}

// OwnerMayJudge is ownerJudges for the gate of a decision or a review about
// one action (action.decide, action.review): true when the caller, from
// seat, is the owner of the agent that did a and could have done it
// themselves just now without anyone's confirmation, and, for a proposal,
// approving it now would not be refused for what it asks. A fault met on
// the way is returned, whatever it is, so that the call fails with nothing
// recorded, as Decide's own asking fails it: a denial recorded for a file
// store that did not answer would be replayed to a retry under the same key
// long after it answers again.
func (p *Pipeline) OwnerMayJudge(ctx context.Context, q dbq.Querier, actor domain.Actor, seat uuid.UUID, a dbq.Action, now time.Time) (bool, error) {
	_, may, _, err := p.ownerJudges(ctx, q, actor, seat, a, now)
	return may, err
}

// OwnerMayJudgeListed is OwnerMayJudge for the approval and review queues,
// which say of each action whether it is the caller's to decide
// (yours_to_decide) and record nothing. A fault outside the database met
// while asking whether approving a proposal would be refused — a file store
// that does not answer while a grade's feedback files are looked at, say —
// leaves it not theirs, rather than failing the whole queue: deciding it
// would fail on the same fault. One of the database's is returned, as any
// other is: the transaction it was met in may be over.
func (p *Pipeline) OwnerMayJudgeListed(ctx context.Context, q dbq.Querier, actor domain.Actor, seat uuid.UUID, a dbq.Action, now time.Time) (bool, error) {
	may, err := p.OwnerMayJudge(ctx, q, actor, seat, a, now)
	var u unsure
	var pgErr *pgconn.PgError
	if errors.As(err, &u) && !errors.As(u.err, &pgErr) {
		return false, nil
	}
	return may, err
}

// errOwnerNotAutonomous refuses an agent's owner who could not have done
// what their agent did without someone's confirmation.
func errOwnerNotAutonomous(what string) *apperr.Error {
	return apperr.Forbid("you %s what your agent did only where you would do it yourself without anyone's confirmation; "+
		"here your own level for it is lower, or it is beyond your reach, so someone else %ss it", what, what).
		With("reason", "owner_not_autonomous")
}

// errOwnerWouldBeRefused refuses an agent's owner a decision about its
// proposal that, approved now, would be refused as refused says.
// Its reason is its own, owner_would_be_refused: the owner may well hold the
// action at autonomous, and what stands in the way is refusal, which says
// why.
func errOwnerWouldBeRefused(refused *apperr.Error) *apperr.Error {
	return apperr.Forbid("you decide what your agent proposed only where you would do it yourself without anyone's confirmation, "+
		"and approved now it would be refused (%s); take it back with action.withdraw, or someone else rejects it", refused.Message).
		With("reason", "owner_would_be_refused").With("refusal", refused)
}

// judgesOwn reports whether a is a decision or a review about an action of
// member's, or of actor's party (sameParty) in any seat, at any remove. The CHECKs on action
// compare a row with its own decider only, and a decision can itself wait
// for a decision, or be under review: a triage agent whose approvals a human
// confirms. Confirming that approval is what carries out the proposal
// underneath, so if the proposer could confirm it, the four eyes on their
// proposal would be their own two and an agent's. The chain runs back in
// time — an action can only be about one that was there before it — so it
// ends.
func judgesOwn(ctx context.Context, q dbq.Querier, a dbq.Action, member, actor uuid.UUID) (bool, error) {
	for (a.ActionType == ToolActionDecide || a.ActionType == ToolActionReview) && a.TargetID != nil {
		about, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: *a.TargetID, CourseID: a.CourseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if about.MemberID != nil && *about.MemberID == member {
			return true, nil
		}
		if same, err := sameParty(ctx, q, about.ActorID, actor); err != nil || same {
			return same, err
		}
		a = about
	}
	return false, nil
}

// closesOwnEscalation reports whether carrying out a would close an
// escalation that actor, or its party, had a hand in: a is a review of an action actor
// escalated, or the approval of one, at any remove. Once escalated, the
// action can only be marked reviewed, so carrying such a review out closes
// the escalation or fails. The review is carried out as its proposer, and
// Review checks only them; whoever approves it is checked here. Saying no
// closes nothing, so a rejection anywhere on the way down is not this.
func closesOwnEscalation(ctx context.Context, q dbq.Querier, a dbq.Action, actor uuid.UUID) (bool, error) {
	for a.ActionType == ToolActionDecide && a.TargetID != nil {
		var d DecideIn
		if json.Unmarshal(a.Payload, &d) != nil || d.Decision != DecisionApprove {
			return false, nil
		}
		about, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: *a.TargetID, CourseID: a.CourseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		a = about
	}
	if a.ActionType != ToolActionReview || a.TargetID == nil {
		return false, nil
	}
	return q.EscalatedBy(ctx, dbq.EscalatedByParams{ActionID: *a.TargetID, ActorID: actor})
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
	// ByOwner says the reviewer was the owner of the agent that did it.
	ByOwner bool `json:"by_owner,omitempty" jsonschema:"true when you reviewed it as the owner of the agent that did it"`
}

// CheckReview is action.review's Check: a review finds an action reviewed,
// or escalates it.
func CheckReview(in ReviewIn) error {
	if in.Outcome != ReviewReviewed && in.Outcome != ReviewEscalated {
		return apperr.Invalid("outcome must be %q or %q", ReviewReviewed, ReviewEscalated)
	}
	return nil
}

// refuseReview says why actor, from seat m, may not review action row as
// outcome says now, or nil when they may; and, when they may, whether they
// review it as the owner of the agent that did it (byOwner). It writes
// nothing. Review asks it of the action under its lock, and action.review's
// Validate (ValidateReview) before a review is proposed, carried out or
// approved. What it asks can change while a proposed review waits — the
// action reviewed by someone else, or escalated — and is asked again when
// it is approved.
func (p *Pipeline) refuseReview(ctx context.Context, q dbq.Querier, actor domain.Actor, m *domain.Member, row dbq.Action, outcome string, now time.Time) (byOwner bool, err error) {
	from := domain.ReviewState(row.ReviewState)
	if !canReview(from, domain.ReviewState(outcome)) {
		if from == domain.ReviewEscalated {
			return false, apperr.Conflicts("the action is already escalated")
		}
		return false, apperr.Conflicts("the action is not awaiting review")
	}
	if m == nil {
		return false, apperr.Forbid("only a course member reviews")
	}
	if same, err := sameParty(ctx, q, row.ActorID, actor.ID); err != nil {
		return false, err
	} else if (row.MemberID != nil && *row.MemberID == m.ID) || same {
		// The database refuses only the same seat; the actor is compared
		// here, so a seat taken since is refused too, and so is its party:
		// an agent does not review its owner's action, nor a sibling's. Its
		// owner reviews what it did only where they could have done it
		// themselves without anyone's confirmation (ownerJudges).
		owner, may, _, err := p.ownerJudges(ctx, q, actor, m.ID, row, now)
		switch {
		case err != nil:
			return false, err
		case owner && !may:
			return false, errOwnerNotAutonomous("review")
		case !may:
			return false, apperr.Forbid("nobody reviews their own action, nor their owner's, nor another agent's of their owner")
		}
		byOwner = true
	}
	if own, err := judgesOwn(ctx, q, row, m.ID, actor.ID); err != nil {
		return false, err
	} else if own {
		return false, apperr.Forbid("nobody reviews their own action, even at one remove: this one decides or reviews an action of yours")
	}
	if from == domain.ReviewEscalated {
		// An escalation asks for a second reviewer, so whoever raised it does
		// not close it: not from the seat they raised it from, nor from one
		// they have taken since. Approving someone else's escalation is
		// raising it too. Approving someone else's review that closes it is
		// refused in Decide (closesOwnEscalation).
		mine, err := q.EscalatedBy(ctx, dbq.EscalatedByParams{ActionID: row.ID, ActorID: actor.ID})
		if err != nil {
			return false, err
		}
		if mine {
			return false, apperr.Forbid("an escalation is for someone else to look at")
		}
	}
	return byOwner, nil
}

// ValidateReview is action.review's Validate: what Review would refuse the
// review in, made from seat m at now, before anything of it is recorded
// (refuseReview), asked of the action as it stands, without its lock.
func (p *Pipeline) ValidateReview(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in ReviewIn) error {
	row, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: in.ActionID, CourseID: &in.CourseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoAction
	}
	if err != nil {
		return err
	}
	actor, err := seatActor(ctx, q, m)
	if err != nil {
		return err
	}
	_, err = p.refuseReview(ctx, q, actor, m, row, in.Outcome, now)
	return err
}

// Review records that a human has looked at a pending_review action after the
// fact. It undoes nothing. Putting right what the action did is another
// action (a regrade), with its own row. It runs on an outcome CheckReview
// has taken.
func (p *Pipeline) Review(ctx context.Context, ec *tool.ExecCtx, in ReviewIn) (ReviewOut, error) {
	row, err := ec.Q.GetActionInCourseForUpdate(ctx, dbq.GetActionInCourseForUpdateParams{ID: in.ActionID, CourseID: &in.CourseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewOut{}, errNoAction
	}
	if err != nil {
		return ReviewOut{}, err
	}
	byOwner, err := p.refuseReview(ctx, ec.Q, ec.Actor, ec.Member, row, in.Outcome, ec.Now)
	if err != nil {
		return ReviewOut{}, err
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
	var extra map[string]any
	if byOwner {
		extra = map[string]any{"by_owner": true}
	}
	ec.Emit(proposalEvent(typ, row, ec.ActionID, extra))
	return ReviewOut{ActionID: row.ID, ReviewState: domain.ReviewState(in.Outcome), ByOwner: byOwner}, nil
}
