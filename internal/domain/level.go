// Package domain holds the vocabulary the rest of the code shares: the
// autonomy ladder, the permission catalogue, and the actor and member shapes
// authorization works on. It imports nothing from this module.
package domain

import "fmt"

// Level is one rung of the ladder that is both permission and autonomy:
// the permission check answers "may this actor act" and "how" in one value.
// The order matches the autonomy_level enum in the database.
type Level int

const (
	Denied          Level = iota // not permitted at all
	ConfirmRequired              // blocked before execution; becomes a proposal
	PendingReview                // executes, then enters a human review queue
	Autonomous                   // executes, no human in the loop
)

var levelNames = [...]string{"denied", "confirm_required", "pending_review", "autonomous"}

func (l Level) String() string {
	if l < Denied || l > Autonomous {
		return fmt.Sprintf("Level(%d)", int(l))
	}
	return levelNames[l]
}

func (l Level) Allowed() bool { return l > Denied }

func ParseLevel(s string) (Level, error) {
	for i, n := range levelNames {
		if n == s {
			return Level(i), nil
		}
	}
	return Denied, fmt.Errorf("unknown autonomy level %q", s)
}

// MinLevel is the most restrictive of the given levels; with none, Denied.
// A tool gated by two permissions runs at the lower of the two.
func MinLevel(levels ...Level) Level {
	if len(levels) == 0 {
		return Denied
	}
	m := levels[0]
	for _, l := range levels[1:] {
		if l < m {
			m = l
		}
	}
	return m
}
