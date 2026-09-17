package pipeline

import (
	"testing"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

func TestStatusTable(t *testing.T) {
	all := []domain.ActionStatus{
		domain.StatusDenied, domain.StatusProposed, domain.StatusApproved, domain.StatusRejected,
		domain.StatusCancelled, domain.StatusExecuted, domain.StatusFailed,
	}
	type edge struct {
		level    domain.Level
		from, to domain.ActionStatus
	}
	legal := map[edge]bool{
		{domain.ConfirmRequired, domain.StatusProposed, domain.StatusExecuted}:  true,
		{domain.ConfirmRequired, domain.StatusProposed, domain.StatusFailed}:    true,
		{domain.ConfirmRequired, domain.StatusProposed, domain.StatusRejected}:  true,
		{domain.ConfirmRequired, domain.StatusProposed, domain.StatusCancelled}: true,
		{domain.PendingReview, domain.StatusApproved, domain.StatusExecuted}:    true,
		{domain.PendingReview, domain.StatusApproved, domain.StatusFailed}:      true,
		{domain.Autonomous, domain.StatusApproved, domain.StatusExecuted}:       true,
		{domain.Autonomous, domain.StatusApproved, domain.StatusFailed}:         true,
	}
	for _, level := range []domain.Level{domain.Denied, domain.ConfirmRequired, domain.PendingReview, domain.Autonomous} {
		for _, from := range all {
			for _, to := range all {
				if got, want := canFollow(level, from, to), legal[edge{level, from, to}]; got != want {
					t.Errorf("%s: %s → %s: canFollow = %v, want %v", level, from, to, got, want)
				}
			}
		}
	}

	first := map[domain.Level]domain.ActionStatus{
		domain.Denied: domain.StatusDenied, domain.ConfirmRequired: domain.StatusProposed,
		domain.PendingReview: domain.StatusApproved, domain.Autonomous: domain.StatusApproved,
	}
	for level, want := range first {
		if got := initialStatus(level); got != want {
			t.Errorf("initialStatus(%s) = %s, want %s", level, got, want)
		}
	}
	// Every status but the two in-flight ones is final.
	for _, s := range all {
		inFlight := s == domain.StatusProposed || s == domain.StatusApproved
		if s.Terminal() == inFlight {
			t.Errorf("%s: Terminal() = %v", s, s.Terminal())
		}
	}
}

func TestReviewTable(t *testing.T) {
	states := []domain.ReviewState{domain.ReviewNone, domain.ReviewPending, domain.ReviewReviewed, domain.ReviewEscalated}
	legal := map[[2]domain.ReviewState]bool{
		{domain.ReviewPending, domain.ReviewReviewed}:   true,
		{domain.ReviewPending, domain.ReviewEscalated}:  true,
		{domain.ReviewEscalated, domain.ReviewReviewed}: true,
	}
	for _, from := range states {
		for _, to := range states {
			if got, want := canReview(from, to), legal[[2]domain.ReviewState{from, to}]; got != want {
				t.Errorf("%s → %s: canReview = %v, want %v", from, to, got, want)
			}
		}
	}
}
