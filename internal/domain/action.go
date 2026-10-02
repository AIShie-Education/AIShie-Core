package domain

// ActionStatus is the life of one attempt. "approved" exists only inside the
// transaction that executes the action; at rest a row is one of the others.
type ActionStatus string

const (
	StatusDenied   ActionStatus = "denied"
	StatusProposed ActionStatus = "proposed"
	StatusApproved ActionStatus = "approved"
	StatusRejected ActionStatus = "rejected"
	// StatusChangesRequested is a proposal its decider sent back with a note
	// of what to change: over, as a rejection is, nothing of it carried out.
	// A new proposal of its proposer's may name it as the one it revises.
	StatusChangesRequested ActionStatus = "changes_requested"
	StatusCancelled        ActionStatus = "cancelled"
	StatusExecuted         ActionStatus = "executed"
	StatusFailed           ActionStatus = "failed"
)

// Terminal reports whether nothing further can happen to the action itself.
// Review of an executed action is tracked separately, in ReviewState.
func (s ActionStatus) Terminal() bool {
	return s != StatusProposed && s != StatusApproved
}

type ReviewState string

const (
	ReviewNone      ReviewState = "none"
	ReviewPending   ReviewState = "pending"
	ReviewReviewed  ReviewState = "reviewed"
	ReviewEscalated ReviewState = "escalated"
)
