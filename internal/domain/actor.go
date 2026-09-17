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
type Member struct {
	ID              uuid.UUID
	CourseID        uuid.UUID
	ActorID         uuid.UUID
	Status          string // active | paused | removed
	ExpiresAt       *time.Time
	StudentScope    string // all | listed
	AssignmentScope string // all | listed
	Perms           map[Perm]Level
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
func (m *Member) Perm(p Perm) Level { return m.Perms[p] }

// Live reports whether the membership counts at the given moment. expires_at
// is an automatic remove: it applies from the instant it passes, whether or
// not the sweep has run yet.
func (m *Member) Live(now time.Time) bool {
	return m.Status == MemberActive && (m.ExpiresAt == nil || m.ExpiresAt.After(now))
}

const (
	CourseDraft    = "draft"
	CourseActive   = "active"
	CourseArchived = "archived"
)
