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
	// Administers says the actor holds at least one live appointment as a
	// department's administrator. It is read to decide whether to look for
	// the appointment a call relies on, and never grants anything by itself.
	Administers bool
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
	// AnswersCourse is set on a delegate's seat that answers the course and
	// not its principal alone (AnswersOthers).
	AnswersCourse bool
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
// can do nothing you cannot. Three exceptions. conversation_answer is capped
// by the principal's conversation_ask: your agent answering you is you
// asking, at one remove, so a principal need not be able to answer for its
// agent to. A delegate never brings agents of its own: agent_delegate is
// denied to it whatever its row says. And the agent of someone who does not
// manage the course's members does, beyond what the built-in delegate
// preset gives, nothing that is not confirmed first (DelegateCap). It may
// manage the course's members, as far as its row and its principal's both
// allow, but never its principal's seat nor its principal's other agents'
// (tools.loadOther).
func (m *Member) Perm(p Perm) Level {
	own := m.Perms[p]
	if m.PrincipalID == nil {
		return own
	}
	if m.Principal == nil {
		return Denied // a delegate whose principal was not loaded holds nothing
	}
	return MinLevel(own, DelegateCap(m.Principal, p))
}

// DelegatePresetLevels is what the built-in delegate preset gives, as
// src/seed/presets.sql seeds it, every other permission denied: a person's
// own assistant reads the material and its principal's work and grades, and
// answers its principal. A test holds the two to each other.
var DelegatePresetLevels = map[Perm]Level{
	PermDocumentRead: Autonomous, PermSubmissionRead: Autonomous, PermGradeRead: Autonomous,
	PermConversationAnswer: Autonomous,
}

// DelegateCap is the most a delegate of principal may hold of p: the
// principal's own level, but conversation_answer capped by the principal's
// conversation_ask, and agent_delegate never. For a principal who does not
// manage the course's members — a student — anything the built-in delegate
// preset gives at a lower level is confirm_required at most: their agent
// may draft their work for them, say, but every such action is a proposal,
// which someone confirms before it is carried out. member_manage and
// member_invite are left to the principal's own level: such a principal has
// no member_manage, and member_invite is made by no proposal. Perm applies it
// on every call; seating and changing a delegate's seat hold the row to it,
// so that the row says what the delegate can do.
func DelegateCap(principal *Member, p Perm) Level {
	var limit Level
	switch p {
	case PermAgentDelegate:
		return Denied
	case PermConversationAnswer:
		limit = principal.Perm(PermConversationAsk)
	default:
		limit = principal.Perm(p)
	}
	if p != PermMemberManage && p != PermMemberInvite && !principal.Perm(PermMemberManage).Allowed() {
		limit = MinLevel(limit, max(DelegatePresetLevels[p], ConfirmRequired))
	}
	return limit
}

// AnswersOthers reports whether a delegate may be addressed by anyone but
// its principal: its seat was made to answer the course, by someone who
// manages the course's members, and its principal still does. A delegate
// answers its principal alone otherwise, and a seat that is nobody's
// delegate is not a delegate to begin with.
func (m *Member) AnswersOthers() bool {
	return m.PrincipalID != nil && m.AnswersCourse && m.Principal != nil && m.Principal.Perm(PermMemberManage).Allowed()
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
