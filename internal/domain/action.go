package domain

// ActionStatus is the life of one attempt. "approved" exists only inside the
// transaction that executes the action; at rest a row is one of the others.
type ActionStatus string

const (
	StatusDenied    ActionStatus = "denied"
	StatusProposed  ActionStatus = "proposed"
	StatusApproved  ActionStatus = "approved"
	StatusRejected  ActionStatus = "rejected"
	StatusCancelled ActionStatus = "cancelled"
	StatusExecuted  ActionStatus = "executed"
	StatusFailed    ActionStatus = "failed"
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
