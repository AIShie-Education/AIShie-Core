package domain

import (
	"time"

	"github.com/google/uuid"
)

// Actor is who is calling: a human, an agent or the system. Which of the
// three is not a field here. Nothing in authorization may branch on it, and
// the simplest way to guarantee that is for the type not to carry it.
type Actor struct {
	ID           uuid.UUID
	DisplayName  string
	Status       string // active | suspended
	PlatformRole string // "", root or admin; read only for operations outside any course
}

func (a Actor) Active() bool { return a.Status == ActorActive }

const (
	ActorActive    = "active"
	ActorSuspended = "suspended"

	PlatformRoot  = "root"
	PlatformAdmin = "admin"
)

// Member is one actor's seat in one course: permissions and scope. The
// roster role is left out for the same reason Actor leaves out its type.
//
// A seat may be a delegate's: the seat of an agent someone owns, which acts
// only as its owner's delegate. Its principal is the owner's seat in the
// same course, and the delegate never holds more than the principal does
// (Perm, PrincipalLive; authz.CheckScope for reach).
type Member struct {
	ID              uuid.UUID
	CourseID        uuid.UUID
	ActorID         uuid.UUID
	Status          string // active | paused | removed
	ExpiresAt       *time.Time
	StudentScope    string // all | listed
	AssignmentScope string // all | listed
	Perms           map[Perm]Level

	// PrincipalID is set on a delegate's seat, and Principal is that seat
	// as it stands: loaded with it, never itself a delegate.
	PrincipalID *uuid.UUID
	Principal   *Member
	// SeatValid says the seat is what its actor's ownership says it must
	// be: a seat with no principal for an actor nobody owns, or the owner's
	// own seat as principal, held by an active owner. An owner can change
	// after a seat was taken, and the seat then stops counting. The zero
	// value is false, so a seat loaded without this check counts for
	// nothing.
	SeatValid bool
}

const (
	MemberActive  = "active"
	MemberPaused  = "paused"
	MemberRemoved = "removed"

	ScopeAll    = "all"
	ScopeListed = "listed"
)

// Perm is the member's level for p; a permission the row does not carry is
// denied.
//
// A delegate's level is the lower of its own and its principal's: your agent
// can do nothing you cannot. Two exceptions. conversation_answer is capped
// by the principal's conversation_ask: your agent answering you is you
// asking, at one remove, so a principal need not be able to answer for its
// agent to. And a delegate never manages the course or brings agents of its
// own: member_manage and agent_delegate are denied to it whatever its row
// says.
func (m *Member) Perm(p Perm) Level {
	own := m.Perms[p]
	if m.PrincipalID == nil {
		return own
	}
	if m.Principal == nil {
		return Denied // a delegate whose principal was not loaded holds nothing
	}
	switch p {
	case PermMemberManage, PermAgentDelegate:
		return Denied
	case PermConversationAnswer:
		return MinLevel(own, m.Principal.Perm(PermConversationAsk))
	}
	return MinLevel(own, m.Principal.Perm(p))
}

// Live reports whether the membership counts at the given moment. expires_at
// is an automatic remove: it applies from the instant it passes, whether or
// not the sweep has run yet. It is about the seat itself; a delegate's
// principal is PrincipalLive's.
func (m *Member) Live(now time.Time) bool {
	return m.Status == MemberActive && (m.ExpiresAt == nil || m.ExpiresAt.After(now))
}

// PrincipalLive reports whether whatever the seat depends on counts at the
// given moment: for a delegate, that its seat matches its actor's owner and
// that its principal is live, since a delegate lives no longer than its
// principal and is paused while it is; for any other seat, that it is not
// an owned agent's seat left without a principal.
func (m *Member) PrincipalLive(now time.Time) bool {
	if !m.SeatValid {
		return false
	}
	return m.PrincipalID == nil || (m.Principal != nil && m.Principal.Live(now))
}

const (
	CourseDraft    = "draft"
	CourseActive   = "active"
	CourseArchived = "archived"
)
