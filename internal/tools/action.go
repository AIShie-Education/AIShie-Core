package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

func actionTools(d Deps) []tool.Tool {
	return []tool.Tool{
		actionDecide(d), actionReview(d), actionWithdraw(),
		actionListProposed(d), actionListPendingReview(d), actionListMine(), actionGet(d),
	}
}

var decidePerm = tool.Gate{Perms: []domain.Perm{domain.PermActionDecide}}

// ownAgentsGate is decidePerm for a tool about one action, which an agent's
// owner may call about their own agent's actions whatever their own
// action_decide (ownAgentsAction).
func ownAgentsGate(d Deps, judge bool) tool.Gate {
	return tool.Gate{Perms: decidePerm.Perms, OwnAgents: ownAgentsAction(d, judge)}
}

// ownAgentsAction is what an agent's owner may do about one action of their
// own agent's, as Gate.OwnAgents: decide or review it (judge) at
// autonomous, where they could have done it themselves just now without
// anyone's confirmation (pipeline.OwnerMayJudge), as their own doing of it;
// read it, whatever it is, as they read their own. Any other action is
// nothing to them here, and whoever action_decide denies stays denied.
func ownAgentsAction(d Deps, judge bool) tool.OwnAgentsFunc {
	return func(ctx context.Context, q dbq.Querier, caller domain.Actor, seat *domain.Member, target tool.Target, now time.Time) (domain.Level, error) {
		if target.ID == nil {
			return domain.Denied, nil
		}
		a, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: *target.ID, CourseID: &target.CourseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Denied, nil
		}
		if err != nil {
			return domain.Denied, err
		}
		// Whose it is comes first, and an agent's owner never changes: no
		// one else's call waits on the proposal below.
		did, err := q.GetActor(ctx, a.ActorID)
		if err != nil {
			return domain.Denied, err
		}
		if did.OwnerActorID == nil || *did.OwnerActorID != caller.ID {
			return domain.Denied, nil
		}
		if judge {
			if a.Status == string(domain.StatusProposed) {
				// Taken as a decision about it takes it, before what
				// asking whether approving it would be refused locks
				// (pipeline.LockProposal): an approval under way holds it
				// and goes on to take those.
				if a, err = pipeline.LockProposal(ctx, q, target.CourseID, a.ID); err != nil {
					return domain.Denied, err
				}
			}
			if may, err := d.Pipeline.OwnerMayJudge(ctx, q, caller, seat.ID, a, now); err != nil || !may {
				return domain.Denied, err
			}
		}
		return domain.Autonomous, nil
	}
}

// ownAgentsQueue is decidePerm for a queue, which an agent's owner who
// decides nothing else in the course may read for their own agents' actions
// alone (queueOf): if they own an agent that holds or held a seat there.
func ownAgentsQueue() tool.Gate {
	return tool.Gate{Perms: decidePerm.Perms, OwnAgents: func(ctx context.Context, q dbq.Querier, caller domain.Actor, _ *domain.Member, target tool.Target, _ time.Time) (domain.Level, error) {
		owns, err := q.OwnsAgentSeatedIn(ctx, dbq.OwnsAgentSeatedInParams{CourseID: target.CourseID, OwnerActorID: &caller.ID})
		if err != nil || !owns {
			return domain.Denied, err
		}
		return domain.Autonomous, nil
	}}
}

// ownAgentsOnly says a queue's caller holds no action_decide of their own,
// and is let read it as an agent's owner (ownAgentsQueue): they are shown
// their own agents' actions and nobody else's.
func ownAgentsOnly(rc *tool.ReadCtx) bool {
	return rc.Member == nil || !rc.Member.Perm(domain.PermActionDecide).Allowed()
}

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
		Description: "Approve or reject a proposal, or send it back for changes: an action that was blocked before execution " +
			"because its proposer needs confirmation. Approving runs it now, as the proposer, after checking that the proposer is still " +
			"allowed to do it; if not, or if the proposal is too old, it is cancelled instead. Requesting changes (request_changes) " +
			"says what to change in reason, 1 to 2000 characters: the proposal ends in changes_requested, nothing of it carried " +
			"out, and its proposer reads the note and may propose again, naming it in revises; whoever may reject a proposal may " +
			"request changes to it, under the same rules. Nobody decides their own proposal, " +
			"nor their owner's, nor another agent's of their owner, nor a decision someone else proposed about any of those, " +
			"nor approves closing an escalation they raised or approved. An agent's owner decides its proposal only where " +
			"they could do the same themselves without anyone's confirmation: their own level for it autonomous, its " +
			"target within their reach, and the tool's own checks of what it asks passing as approving it now would run them; " +
			"by_owner then says so. That needs no action_decide of their own, and is done at " +
			"once, as their own doing of it: a student confirms her own agent's drafts of her work. An owner whose own " +
			"level or reach falls short is refused (owner_not_autonomous), and one whose agent's proposal approving now " +
			"would refuse is refused with that refusal in details.refusal (owner_would_be_refused): either way, if they " +
			"hold action_decide; one who does not is refused as anyone without it is (permission_denied). A decision " +
			"that would be refused as it is made, about a proposal that no longer waits or is not the caller's to " +
			"decide, is refused at once, and never waits for anyone's confirmation.",
		Kind:  tool.Write,
		Gate:  ownAgentsGate(d, true),
		HTTP:  tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/actions/{action_id}/decide"},
		Check: pipeline.CheckDecision,
		Resolve: func(ctx context.Context, q dbq.Querier, in pipeline.DecideIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		// Whether the proposal waits for a decision, and whether it is the
		// caller's to decide: asked before a decision is proposed, so that
		// nobody is asked to approve one that approving would refuse, and
		// again under the proposal's lock as it is made.
		Validate: d.Pipeline.ValidateDecision,
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
			"did only where they could do the same themselves without anyone's confirmation, and then needs no " +
			"action_decide of their own.",
		Kind:  tool.Write,
		Gate:  ownAgentsGate(d, true),
		HTTP:  tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/actions/{action_id}/review"},
		Check: pipeline.CheckReview,
		Resolve: func(ctx context.Context, q dbq.Querier, in pipeline.ReviewIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		// As action.decide's.
		Validate: d.Pipeline.ValidateReview,
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
	Result             json.RawMessage `json:"result,omitempty" jsonschema:"what the call returned; for a rejected proposal, or one sent back for changes, the decision: decision.reason is why, or what to change"`
	CreatedAt          time.Time       `json:"created_at"`
	RevisesActionID    *uuid.UUID      `json:"revises_action_id,omitempty" jsonschema:"the proposal this one revises: its proposer's own, sent back for changes, whose result says what was asked"`
	// Redacted is set on an action about an assignment deleted for good.
	Redacted *ActionRedaction `json:"redacted,omitempty" jsonschema:"set when what the action was about was deleted for good (assignment.delete): its payload and result were emptied then, and say nothing"`
	// YoursToDecide is set in the approval and review queues.
	YoursToDecide *bool `json:"yours_to_decide,omitempty" jsonschema:"in the approval and review queues: false when the action is yours, your owner's or another agent's of your owner, and when it is your own agent's and you could not do the same yourself without anyone's confirmation (your own level for it below autonomous, or its target beyond your reach, or, for a proposal, the tool's own checks of what it asks refuse it as approving it now would run them): someone else decides and reviews those; true otherwise, though a decision about a decision may still be refused at one remove"`
}

// ActionRedaction says which deletion emptied an action, and when.
type ActionRedaction struct {
	ByActionID uuid.UUID  `json:"by_action_id" jsonschema:"the assignment.delete that emptied it"`
	At         *time.Time `json:"at,omitempty" jsonschema:"when it did"`
}

func viewAction(a dbq.Action) ActionView {
	v := ActionView{
		ID: a.ID, ActorID: a.ActorID, MemberID: a.MemberID, ActionType: a.ActionType,
		TargetType: a.TargetType, TargetID: a.TargetID, Payload: a.Payload,
		AuthzResult: string(a.AuthzResult), Status: a.Status,
		DecidedByMemberID: a.DecidedByMemberID, DecidedAt: a.DecidedAt,
		ReviewState: a.ReviewState, ReviewedByMemberID: a.ReviewedByMemberID, ReviewedAt: a.ReviewedAt,
		ExecutedAt: a.ExecutedAt, Result: a.Result, CreatedAt: a.CreatedAt, RevisesActionID: a.RevisesActionID,
	}
	if a.RedactedByActionID != nil {
		v.Redacted = &ActionRedaction{ByActionID: *a.RedactedByActionID}
	}
	return v
}

// redactedAt says, of each action in views that a deletion emptied, when it
// did: when the deletion ran.
func redactedAt(ctx context.Context, q dbq.Querier, views []ActionView) error {
	var by []uuid.UUID
	for _, v := range views {
		if v.Redacted != nil {
			by = append(by, v.Redacted.ByActionID)
		}
	}
	if len(by) == 0 {
		return nil
	}
	rows, err := q.ListActionsRedactedAt(ctx, dedupe(by))
	if err != nil {
		return err
	}
	at := make(map[uuid.UUID]*time.Time, len(rows))
	for _, r := range rows {
		at[r.ID] = r.ExecutedAt
	}
	for i := range views {
		if r := views[i].Redacted; r != nil {
			r.At = at[r.ByActionID]
		}
	}
	return nil
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
// confirmation (pipeline.OwnerMayJudgeListed), measured now as a decision
// would be.
func queuePage(ctx context.Context, rc *tool.ReadCtx, p *pipeline.Pipeline, rows []dbq.Action, limit int32) (ActionListOut, error) {
	out := actionPage(rows, limit)
	if len(rows) == 0 {
		return out, nil
	}
	if err := redactedAt(ctx, rc.Q, out.Actions); err != nil {
		return out, err
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
			if yours, err = p.OwnerMayJudgeListed(ctx, rc.Q, rc.Actor, rc.Member.ID, r, rc.Now); err != nil {
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
			"agents', and your own agents' unless you could do the same yourself without anyone's confirmation. If you " +
			"hold no action_decide here but own an agent seated here, it lists your own agents' proposals alone. A " +
			"proposal that revises one sent back for changes names it in revises_action_id; action.get on that one says what was asked.",
		Kind:    tool.Read,
		Gate:    ownAgentsQueue(),
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/proposed"},
		Resolve: courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			var rows []dbq.Action
			var err error
			if ownAgentsOnly(rc) {
				rows, err = rc.Q.ListProposedActionsOfAgentsOf(ctx, dbq.ListProposedActionsOfAgentsOfParams{
					CourseID: &in.CourseID, OwnerActorID: &rc.Actor.ID, After: in.after(), MaxRows: in.limit()})
			} else {
				rows, err = rc.Q.ListProposedActions(ctx, dbq.ListProposedActionsParams{CourseID: &in.CourseID, After: in.after(), MaxRows: in.limit()})
			}
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
			"owner's other agents', and your own agents' unless you could do the same yourself without anyone's confirmation. " +
			"If you hold no action_decide here but own an agent seated here, it lists your own agents' actions alone.",
		Kind:    tool.Read,
		Gate:    ownAgentsQueue(),
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/pending-review"},
		Resolve: courseOnly,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionListIn) (ActionListOut, error) {
			var rows []dbq.Action
			var err error
			if ownAgentsOnly(rc) {
				rows, err = rc.Q.ListPendingReviewActionsOfAgentsOf(ctx, dbq.ListPendingReviewActionsOfAgentsOfParams{
					CourseID: &in.CourseID, OwnerActorID: &rc.Actor.ID, After: in.after(), MaxRows: in.limit()})
			} else {
				rows, err = rc.Q.ListPendingReviewActions(ctx, dbq.ListPendingReviewActionsParams{CourseID: &in.CourseID, After: in.after(), MaxRows: in.limit()})
			}
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
			"A proposal rejected or sent back for changes says why, or what to change, in result.decision.reason; one " +
			"that revises another names it in revises_action_id. exclude_types leaves out whole kinds of action: a chat's messages, say.",
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
			if err != nil {
				return ActionListOut{}, err
			}
			out := actionPage(rows, in.limit())
			return out, redactedAt(ctx, rc.Q, out.Actions)
		},
	})
}

type ActionGetIn struct {
	tool.InCourse
	ActionID uuid.UUID `json:"action_id"`
}

func actionGet(d Deps) tool.Tool {
	return tool.Define(tool.Spec[ActionGetIn, ActionView]{
		Name: "action.get",
		Description: "One action in full: what was asked, how it was authorized, what became of it, who decided or reviewed it. " +
			"An agent's owner reads any action of their own agent's, whatever they hold.",
		Kind: tool.Read,
		Gate: ownAgentsGate(d, false),
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actions/{action_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActionGetIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActionGetIn) (ActionView, error) {
			a, err := rc.Q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: in.ActionID, CourseID: &in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return ActionView{}, apperr.Missing("no such action in this course")
			}
			if err != nil {
				return ActionView{}, err
			}
			v := []ActionView{viewAction(a)}
			if err := redactedAt(ctx, rc.Q, v); err != nil {
				return ActionView{}, err
			}
			return v[0], nil
		},
	})
}

type ActionWithdrawIn struct {
	tool.InCourse
	ActionID uuid.UUID `json:"action_id" jsonschema:"your proposal, or one of an agent you own, that is still waiting for a decision"`
}

// actionWithdraw lets a proposer take back what they proposed while nobody
// has decided it: a student who asked to bring an agent in and thought
// better of it, say. The owner of an agent may take back what it proposed
// as well, as it may itself: it acts only as their delegate. Nobody else
// may, the agent's siblings included, nor an agent its owner's. It is gated
// by perm_document_read, like action.list_mine, only because every seated
// member holds some permission and this is the most basic one; whose the
// proposal is, is what the tool checks. The actor is compared, not the seat:
// someone removed and seated again is still who made it, or owns it.
func actionWithdraw() tool.Tool {
	return tool.Define(tool.Spec[ActionWithdrawIn, OK]{
		Name: "action.withdraw",
		Description: "Take back a proposal of yours, or of an agent you own, that is still waiting for a decision. It is " +
			"cancelled, and nothing of it is carried out. A proposal already decided, or anyone else's, cannot be withdrawn.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/actions/{action_id}/withdraw"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActionWithdrawIn) (tool.Target, error) {
			return actionTarget(ctx, q, in.CourseID, in.ActionID)
		},
		// Whose the proposal is, and whether it still waits: asked before a
		// withdrawal is proposed, and again under the proposal's lock as it
		// is made.
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in ActionWithdrawIn) error {
			prop, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: in.ActionID, CourseID: &in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.Missing("no such action in this course")
			}
			if err != nil {
				return err
			}
			_, err = withdrawable(ctx, q, m.ActorID, prop)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActionWithdrawIn) (OK, error) {
			prop, err := ec.Q.GetActionInCourseForUpdate(ctx, dbq.GetActionInCourseForUpdateParams{ID: in.ActionID, CourseID: &in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return OK{}, apperr.Missing("no such action in this course")
			}
			if err != nil {
				return OK{}, err
			}
			byOwner, err := withdrawable(ctx, ec.Q, ec.Actor.ID, prop)
			if err != nil {
				return OK{}, err
			}
			_, stored := pipeline.Cancellation(pipeline.CancelWithdrawn, byOwner)
			n, err := ec.Q.CancelProposal(ctx, dbq.CancelProposalParams{ID: prop.ID, Result: stored})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the proposal was decided just now")
			}
			// Filed under the proposal, so that it reads in the proposer's
			// feed like any other end of a proposal, saying whether its
			// owner took it back.
			id := prop.ID
			payload := map[string]any{"action_type": prop.ActionType, "reason": pipeline.CancelWithdrawn, "by_action_id": ec.ActionID}
			if byOwner != nil {
				payload["by_owner"] = true
			}
			ec.Emit(events.Event{Type: events.ActionCancelled, CourseID: prop.CourseID, ActionID: &id,
				SubjectType: "action", SubjectID: &id, Payload: payload})
			return OK{OK: true}, nil
		},
	})
}

// withdrawable says why actor may not withdraw proposal prop, or nil when
// they may; and, when they may, what the cancellation records of an owner
// withdrawing their agent's (by_owner), nil for the proposer's own. Whose a
// proposal is never changes: the proposer, and an agent's owner.
func withdrawable(ctx context.Context, q dbq.Querier, actor uuid.UUID, prop dbq.Action) (map[string]any, error) {
	var byOwner map[string]any
	if prop.ActorID != actor {
		did, err := q.GetActor(ctx, prop.ActorID)
		if err != nil {
			return nil, err
		}
		if did.OwnerActorID == nil || *did.OwnerActorID != actor {
			return nil, apperr.Forbid("only whoever proposed it, or the owner of the agent that did, withdraws a proposal")
		}
		byOwner = map[string]any{"by_owner": true}
	}
	if prop.Status != string(domain.StatusProposed) {
		return nil, apperr.Conflicts("the action is %s, not awaiting a decision", prop.Status)
	}
	return byOwner, nil
}
