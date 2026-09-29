// Package members holds what removing a member means, because several things
// do it: the member.remove tool, an owner withdrawing an agent
// (agent.withdraw), the sweeps that act on expires_at and on delegates whose
// principal has gone, and seating someone again over a seat that is over.
package members

import (
	"context"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
)

// Reasons a member is removed.
const (
	ReasonRemoved = "removed"
	ReasonExpired = "expired"
	// The owner took their agent out of the course.
	ReasonWithdrawn = "withdrawn"
	// A delegate's seat, removed with its principal's.
	ReasonPrincipalRemoved = "principal_removed"
	// A seat that counts for nothing for good: a delegate's whose principal
	// has gone, or one that no longer matches its actor's owner.
	ReasonOrphaned = "orphaned"
)

// ConversationSeatRemoved is why a conversation was closed when one of its
// participants' seats was removed.
const ConversationSeatRemoved = "seat_removed"

// Event types.
const (
	EventAdded    = "member.added"
	EventUpdated  = "member.updated"
	EventPaused   = "member.paused"
	EventResumed  = "member.resumed"
	EventRemoved  = "member.removed"
	EventRescoped = "member.rescoped"
	// A seat's roster role changed: payload from and to.
	EventRoleChanged = "member.role_changed"
)

// Remove retires a membership: status becomes 'removed', the row and all its
// history stay, and whatever the member had proposed and nobody had yet
// decided is cancelled, and every conversation it takes part in that is
// still open is closed. Seating the same actor again later is a new row with
// a new id, and a fresh start. The seats of the member's delegates go with
// it, whatever their status, and so do their proposals and conversations: a
// delegate lives no longer than its principal.
//
// Whoever calls it holds the seat FOR UPDATE already, having read it to
// decide to remove it: the removal waits for the member's calls in flight,
// or they wait for it and are refused. The order is the seat, then its
// delegates, then the conversations:
//
//   - The delegates' seats are removed by the database, in the statement
//     that removes the principal's (course_member_delegates_follow), so that
//     the release before this one, which knows no delegates, removes them
//     too. They are only updated there, which a delegate's own call in
//     flight, holding its seat KEY SHARE, does not block: that call waits
//     for the principal's seat, which it takes second, and finds it removed.
//     Whoever takes a delegate's seat FOR UPDATE — to change it, withdraw it,
//     sweep it — takes its principal's KEY SHARE before it, and so waits for
//     this, or this for it, before either has the other's row.
//   - The conversations come last, all of them in one statement: a call
//     writing in one takes both its participants' seats before the
//     conversation, so it has either finished or waits for this, and then
//     finds the conversation closed.
//
// A removed member's proposals would be refused at approval anyway, since
// approval re-authorizes the proposer. Cancelling them here as well means the
// approval queue does not fill with proposals nobody can approve.
func Remove(ctx context.Context, q *dbq.Queries, emit func(events.Event), courseID, memberID uuid.UUID, reason string) (cancelled int, err error) {
	delegates, err := q.ListLiveDelegatesOf(ctx, &memberID)
	if err != nil {
		return 0, err
	}
	n, err := q.SetMemberStatus(ctx, dbq.SetMemberStatusParams{ID: memberID, Status: domain.MemberRemoved, FromStatus: domain.MemberActive})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		if n, err = q.SetMemberStatus(ctx, dbq.SetMemberStatusParams{ID: memberID, Status: domain.MemberRemoved, FromStatus: domain.MemberPaused}); err != nil {
			return 0, err
		}
	}
	if n == 0 {
		return 0, apperr.Conflicts("the member has already been removed")
	}
	gone := []retiree{{memberID, reason}}
	for _, d := range delegates {
		gone = append(gone, retiree{d, ReasonPrincipalRemoved})
	}
	return retired(ctx, q, emit, courseID, gone)
}

type retiree struct {
	id     uuid.UUID
	reason string
}

// retired tells the feed seats are removed, closes their conversations and
// cancels their proposals.
func retired(ctx context.Context, q *dbq.Queries, emit func(events.Event), courseID uuid.UUID, gone []retiree) (cancelled int, err error) {
	ids := make([]uuid.UUID, len(gone))
	for i, g := range gone {
		ids[i] = g.id
		seat := g.id
		emit(events.Event{
			Type: EventRemoved, CourseID: &courseID, SubjectType: "course_member", SubjectID: &seat,
			Payload: map[string]any{"reason": g.reason},
		})
	}

	closed, err := q.CloseConversationsOf(ctx, ids)
	if err != nil {
		return 0, err
	}
	if err := q.DeleteDrafts(ctx, closed); err != nil {
		return 0, err
	}
	for _, id := range closed {
		conversation := id
		emit(events.Event{
			Type: events.ConversationClosed, CourseID: &courseID, SubjectType: "conversation", SubjectID: &conversation,
			Payload: map[string]any{"conversation_id": conversation, "reason": ConversationSeatRemoved},
		})
	}

	for _, g := range gone {
		proposals, err := q.ListProposedActionIDsByMember(ctx, &g.id)
		if err != nil {
			return cancelled, err
		}
		_, stored := pipeline.Cancellation(pipeline.CancelMemberRemoved, map[string]any{"member_reason": g.reason})
		for _, id := range proposals {
			n, err := q.CancelProposal(ctx, dbq.CancelProposalParams{ID: id, Result: stored})
			if err != nil {
				return cancelled, err
			}
			if n == 0 {
				continue // decided in the meantime
			}
			cancelled++
			proposal := id
			emit(events.Event{
				Type: events.ActionCancelled, CourseID: &courseID, ActionID: &proposal,
				SubjectType: "action", SubjectID: &proposal,
				Payload: map[string]any{"reason": pipeline.CancelMemberRemoved},
			})
		}
	}
	return cancelled, nil
}
