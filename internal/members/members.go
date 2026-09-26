// Package members holds what removing a member means, because several things
// do it: the member.remove tool, an owner withdrawing an agent
// (agent.withdraw), the sweeps that act on expires_at and on delegates whose
// principal has gone, and seating someone again over a seat that is over.
package members

import (
	"context"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
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
// or they wait for it and are refused. The delegates' seats are then only
// updated (RemoveDelegatesOf), after the principal's: a seat before its
// principal is the order a delegate's own calls take the two in, and they
// take the principal's KEY SHARE, which waits for this. The conversations
// come last: a call writing in one takes both its participants' seats before
// the conversation, so it has either finished or waits for this, and then
// finds the conversation closed.
//
// A removed member's proposals would be refused at approval anyway, since
// approval re-authorizes the proposer. Cancelling them here as well means the
// approval queue does not fill with proposals nobody can approve.
func Remove(ctx context.Context, q *dbq.Queries, emit func(events.Event), courseID, memberID uuid.UUID, reason string) (cancelled int, err error) {
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
	if cancelled, err = retired(ctx, q, emit, courseID, memberID, reason); err != nil {
		return cancelled, err
	}
	delegates, err := q.RemoveDelegatesOf(ctx, &memberID)
	if err != nil {
		return cancelled, err
	}
	for _, d := range delegates {
		n, err := retired(ctx, q, emit, courseID, d, ReasonPrincipalRemoved)
		cancelled += n
		if err != nil {
			return cancelled, err
		}
	}
	return cancelled, nil
}

// retired tells the feed a seat is removed, cancels its proposals and closes
// its conversations.
func retired(ctx context.Context, q *dbq.Queries, emit func(events.Event), courseID, memberID uuid.UUID, reason string) (cancelled int, err error) {
	emit(events.Event{
		Type: EventRemoved, CourseID: &courseID, SubjectType: "course_member", SubjectID: &memberID,
		Payload: map[string]any{"reason": reason},
	})

	closed, err := q.CloseConversationsOf(ctx, memberID)
	if err != nil {
		return 0, err
	}
	for _, id := range closed {
		conversation := id
		emit(events.Event{
			Type: events.ConversationClosed, CourseID: &courseID, SubjectType: "conversation", SubjectID: &conversation,
			Payload: map[string]any{"conversation_id": conversation, "reason": ConversationSeatRemoved},
		})
	}

	proposals, err := q.ListProposedActionIDsByMember(ctx, &memberID)
	if err != nil {
		return 0, err
	}
	_, stored := pipeline.Cancellation(pipeline.CancelMemberRemoved, map[string]any{"member_reason": reason})
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
	return cancelled, nil
}
