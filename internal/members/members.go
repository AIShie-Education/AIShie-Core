// Package members holds what removing a member means, because two things do
// it: the member.remove tool, and the sweep that acts on expires_at.
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
)

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
// decided is cancelled. Seating the same actor again later is a new row with
// a new id, and a fresh start.
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
	emit(events.Event{
		Type: EventRemoved, CourseID: &courseID, SubjectType: "course_member", SubjectID: &memberID,
		Payload: map[string]any{"reason": reason},
	})

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
