package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// Resetting a student's password (docs/schema.md §2.2). A student with no
// email cannot reset their own: Core sends nothing, and there is nowhere to
// send a link. Whoever manages the course's members, without approval, gives
// a student of theirs a temporary password instead, shown once to them to
// hand on. The student signs in with it, and sets their own before anything
// else. Whoever holds the temporary password can sign in as the student, so
// it is given only for an account that reaches nothing a student of the
// course does not: a person's, seated as a student and nothing else anywhere,
// who holds no platform role, administers no department, and signs in by no
// identity provider; and only by a person, who is who a password is handed
// to.

const (
	ToolMemberResetPassword = "member.reset_password"

	EventMemberPasswordReset = "member.password_reset"
)

// Why a password is not reset, in error.details.reason: each rule its own.
const (
	// The caller.
	ResetPeopleOnly    = "people_only"
	ResetNotAutonomous = "not_autonomous"
	ResetOwnSeat       = "own_seat"
	// The seat.
	ResetNotAStudent   = "not_a_student"
	ResetSeatNotActive = "seat_not_active"
	// The person.
	ResetNotAPerson      = "not_a_person"
	ResetPlatformRole    = "platform_role"
	ResetAdministers     = "administers"
	ResetSSOLinked       = "sso_linked"
	ResetSeatedOtherwise = "seated_other_than_student"
	ResetNoSignInName    = "no_sign_in_name"
	// What the seat holds, against what the caller does.
	ResetBeyondYourSeat = "beyond_your_seat"
)

func resetForbidden(reason, format string, args ...any) *apperr.Error {
	return apperr.Forbid(format, args...).With("reason", reason)
}

type MemberResetPasswordOut struct {
	MemberID          uuid.UUID `json:"member_id"`
	ActorID           uuid.UUID `json:"actor_id"`
	DisplayName       string    `json:"display_name"`
	LoginID           *string   `json:"login_id,omitempty" jsonschema:"what they sign in with, if they have a login ID; otherwise it is their email, which they know"`
	TemporaryPassword string    `json:"temporary_password" jsonschema:"for you to hand to the student; shown once: it is not stored, and a replay of this call comes back without it"`
	SessionsEnded     int64     `json:"sessions_ended" jsonschema:"how many sessions of theirs were signed out"`
}

func memberResetPassword() tool.Tool {
	return tool.Define(tool.Spec[MemberIDIn, MemberResetPasswordOut]{
		Name: ToolMemberResetPassword,
		Description: "Give a student of the course a new, temporary password, for one who has forgotten theirs and has no " +
			"email to reset it by. It is returned once, to hand to them; only its hash is kept. Every session they have " +
			"is signed out, and the next time they sign in, with it, they must set a password of their own before " +
			"anything else. For people who manage the course's members without approval (member_manage autonomous); " +
			"never by proposal (not_by_proposal), and never by an agent (people_only), which is never handed a " +
			"password. Only for a person's active student seat in this course (not_a_person, not_a_student, " +
			"seat_not_active) whose account reaches nothing beyond it: seated as a student and nothing else in every " +
			"course (seated_other_than_student), holding no platform role (platform_role), administering no " +
			"department (administers), signing in by no identity provider (sso_linked), and with a login ID or an " +
			"email to sign in with (no_sign_in_name); and whose seat holds nothing you do not (beyond_your_seat). " +
			"Anyone else's is an administrator's to reset (actor.invite).",
		Kind: tool.Write, Gate: manageMembers,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/members/{member_id}/reset-password"},
		SecretOut: []string{"temporary_password"},
		// The student's account is within the caller's reach only if the
		// student is: a manager listed for some students resets only theirs.
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberIDIn) (tool.Target, error) {
			target, err := resolveMember(ctx, q, in.CourseID, in.MemberID)
			target.Scope = authz.Target{StudentMemberIDs: []uuid.UUID{in.MemberID}}
			return target, err
		},
		// The password is shown once, to whoever the call returns to.
		// Carried out on approval, that would be whoever approved it, and the
		// proposal would wait with the reset in it.
		Pin: func(context.Context, dbq.Querier, *domain.Member, time.Time, MemberIDIn) (MemberIDIn, error) {
			return MemberIDIn{}, apperr.Precondition("a password is not reset by proposal: the new one is shown once, to whoever "+
				"resets it, and approving it would show it to whoever approved it; ask someone who holds member_manage "+
				"without approval").With("reason", "not_by_proposal")
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberIDIn) (MemberResetPasswordOut, error) {
			// A password is handed to a person, never to an agent, whoever's
			// delegate it is and whatever it holds, as a join link seats
			// people only. This reads kind to refuse, as the ceilings read it
			// to limit (isAgent); nothing that grants reads it.
			me, err := ec.Q.GetActor(ctx, ec.Actor.ID)
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			if isAgent(me.Kind) {
				return MemberResetPasswordOut{}, resetForbidden(ResetPeopleOnly, "a password is reset by a person, who hands it on; "+
					"an agent is never given one")
			}
			// Not under review either: a reviewer's rejection would not take
			// back a password already handed out.
			if ec.Member.Perm(domain.PermMemberManage) != domain.Autonomous {
				return MemberResetPasswordOut{}, resetForbidden(ResetNotAutonomous, "you hold member_manage subject to review; "+
					"a password is reset only by someone who holds it without")
			}
			if in.MemberID == ec.Member.ID {
				return MemberResetPasswordOut{}, resetForbidden(ResetOwnSeat, "that is your own seat: set your own password "+
					"with credential.set_password")
			}
			// Whose a seat is, and its role, never change: read before
			// anything is locked, and refused before the person is. A
			// delegate's seat is an agent's.
			found, err := ec.Q.GetMemberInCourse(ctx, dbq.GetMemberInCourseParams{ID: in.MemberID, CourseID: in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return MemberResetPasswordOut{}, apperr.Missing("no such member in this course")
			}
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			if isAgent(found.ActorKind) || found.PrincipalMemberID != nil {
				return MemberResetPasswordOut{}, resetForbidden(ResetNotAPerson, "that seat is an agent's, which signs in with "+
					"a token and has no password")
			}
			if found.Role != "student" {
				return MemberResetPasswordOut{}, resetForbidden(ResetNotAStudent, "that seat is a %s's, not a student's: "+
					"an administrator resets their password (actor.invite)", found.Role)
			}
			// The person, then their seat, as every tool that changes one
			// locks it (loadOther). Seating the person anywhere reads them
			// FOR SHARE first, and then, for a seat past its expiry, locks
			// it: it waits for this, or this for it, and then counts the seat
			// it made.
			if err := ec.Q.LockActorForPasswordReset(ctx, found.ActorID); err != nil {
				return MemberResetPasswordOut{}, err
			}
			m, err := loadOther(ctx, ec, in.CourseID, in.MemberID)
			if apperr.Is(err, apperr.Conflict) || (err == nil && m.Status != domain.MemberActive) {
				// Removed, past its expiry, or paused.
				return MemberResetPasswordOut{}, apperr.Precondition("the student's seat is not active: resume it first, or "+
					"have an administrator reset their password").With("reason", ResetSeatNotActive)
			}
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			facts, err := ec.Q.PasswordResetFacts(ctx, m.ActorID)
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			const theirs = "an administrator resets their password (actor.invite)"
			switch {
			case facts.HoldsRole:
				return MemberResetPasswordOut{}, resetForbidden(ResetPlatformRole, "they hold a platform role: "+theirs)
			case facts.Administers:
				return MemberResetPasswordOut{}, resetForbidden(ResetAdministers, "they administer a department: "+theirs)
			case facts.HasSso:
				return MemberResetPasswordOut{}, resetForbidden(ResetSSOLinked, "they sign in through the identity provider, "+
					"whose password is not ours to set")
			case facts.SeatedOtherwise:
				return MemberResetPasswordOut{}, resetForbidden(ResetSeatedOtherwise, "they hold a seat other than a "+
					"student's in a course: "+theirs)
			case !facts.HasSignInName:
				return MemberResetPasswordOut{}, apperr.Precondition("they have neither a login ID nor an email to sign in "+
					"with: an administrator gives them one first (actor.update)").With("reason", ResetNoSignInName)
			}
			// Whoever holds the password holds the seat: it is handed out
			// as a resumed seat is, as a grant of the whole of it, within
			// what the caller holds (withinGranter, outlastsGranter).
			held, err := shapeOf(ctx, ec.Q, m)
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			if err := grant(ctx, ec, shape{}, held); err != nil {
				if e, ok := apperr.As(err); ok {
					return MemberResetPasswordOut{}, e.With("reason", ResetBeyondYourSeat)
				}
				return MemberResetPasswordOut{}, err
			}

			password, err := auth.NewTemporaryPassword()
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			ended, err := auth.SetTemporaryPassword(ctx, ec.Q, m.ActorID, ec.Actor.ID, password,
				"temporary password set by "+ec.Actor.DisplayName, ec.Now)
			if err != nil {
				return MemberResetPasswordOut{}, err
			}
			// Who reset whose: the action is the caller's, the subject the
			// student's seat. Never the password.
			ec.Emit(events.Event{Type: EventMemberPasswordReset, CourseID: &in.CourseID, SubjectType: "course_member",
				SubjectID: &m.ID, StudentMemberID: &m.ID,
				Payload: map[string]any{"reset_by_member_id": ec.Member.ID, "sessions_ended": ended}})
			return MemberResetPasswordOut{MemberID: m.ID, ActorID: m.ActorID, DisplayName: facts.DisplayName,
				LoginID: facts.LoginID, TemporaryPassword: password, SessionsEnded: ended}, nil
		},
	})
}
