package pipeline

import "github.com/AIShie-Education/AIShie-Core/internal/domain"

// The life of an action, by how it was authorized:
//
//	authz_result       first status      then                          review_state
//	denied             denied            —                             none
//	confirm_required   proposed          executed | failed  (approved) none
//	                                     rejected
//	                                     changes_requested
//	                                     cancelled  (proposer no longer
//	                                       allowed, target gone, too old,
//	                                       member removed)
//	pending_review     approved ─┬─ executed                           pending → reviewed | escalated
//	                             └─ failed                              none      escalated → reviewed
//	autonomous         approved ─┬─ executed                           none
//	                             └─ failed
//
// "approved" is never seen at rest: a row is inserted as approved and moved
// to executed or failed before the same transaction commits. Any allowed
// call that breaks a rule of the domain before it starts is recorded as
// failed from the outset.
//
// The database holds the part of this a CHECK can express
// (action_status_matches_authz); the order of events is held here.

// initialStatus is the status an action row is first written with.
func initialStatus(level domain.Level) domain.ActionStatus {
	switch level {
	case domain.Denied:
		return domain.StatusDenied
	case domain.ConfirmRequired:
		return domain.StatusProposed
	default:
		return domain.StatusApproved
	}
}

// canFollow reports whether an action authorized at level may move from one
// status to another.
func canFollow(level domain.Level, from, to domain.ActionStatus) bool {
	switch from {
	case domain.StatusProposed:
		if level != domain.ConfirmRequired {
			return false
		}
		switch to {
		case domain.StatusExecuted, domain.StatusFailed, domain.StatusRejected, domain.StatusChangesRequested,
			domain.StatusCancelled:
			return true
		}
	case domain.StatusApproved:
		if level != domain.PendingReview && level != domain.Autonomous {
			return false
		}
		return to == domain.StatusExecuted || to == domain.StatusFailed
	}
	return false // denied, executed, failed, rejected, changes_requested and cancelled are final
}

// canReview reports whether a review may move from one state to another.
func canReview(from, to domain.ReviewState) bool {
	switch from {
	case domain.ReviewPending:
		return to == domain.ReviewReviewed || to == domain.ReviewEscalated
	case domain.ReviewEscalated:
		return to == domain.ReviewReviewed
	}
	return false
}
