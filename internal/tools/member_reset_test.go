package tools_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// An instructor gives a student who has forgotten their password, and has
// no email to reset it by, a temporary one: shown once, kept nowhere but as
// a hash, and good for one thing only, until the student has set their own.

var temporaryShape = regexp.MustCompile(`^[a-hjkmnp-z2-9]{4}(-[a-hjkmnp-z2-9]{4}){3}$`)

// reset is member.reset_password as caller, on seat, under a key of its own.
func (b *built) reset(t *testing.T, caller, seat uuid.UUID) pipeline.Outcome {
	t.Helper()
	return b.MustCall(caller, tools.ToolMemberResetPassword, m{"course_id": b.course, "member_id": seat}, "reset-"+uuid.NewString())
}

// resetRefused insists a reset was attempted and refused, as status, with
// code, for why.
func resetRefused(t *testing.T, out pipeline.Outcome, status domain.ActionStatus, code apperr.Code, why string) {
	t.Helper()
	if out.Status != status || out.Error == nil || out.Error.Code != code || reason(out) != why {
		t.Fatalf("want %s %s (%s), got %+v", status, code, why, out)
	}
}

func TestAnInstructorResetsAStudentsPassword(t *testing.T) {
	b := build(t)
	ctx := t.Context()
	authn := auth.NewAuthenticator(b.Pool, 0)
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "login_id": "20230007"})
	if err := auth.SetPassword(ctx, b.Q, b.yuki, "yukis forgotten password", b.P.Clock()); err != nil {
		t.Fatal(err)
	}
	before, err := authn.Login(ctx, "20230007", "yukis forgotten password")
	if err != nil || before.PasswordChangeRequired {
		t.Fatalf("Yuki signs in, before: %+v %v", before, err)
	}
	token := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.yuki, "credential.issue_token", m{"label": "her own script"})).Token

	b.key++
	out := b.MustCall(b.sato, tools.ToolMemberResetPassword, m{"course_id": b.course, "member_id": b.yukiM}, "reset-once")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("the reset: %+v", out)
	}
	got := testkit.Result[tools.MemberResetPasswordOut](t, out)
	if got.MemberID != b.yukiM || got.ActorID != b.yuki || got.DisplayName != "Yuki" || got.LoginID == nil ||
		*got.LoginID != "20230007" || !temporaryShape.MatchString(got.TemporaryPassword) || got.SessionsEnded != 1 {
		t.Fatalf("the reset's answer: %+v", got)
	}
	temporary := got.TemporaryPassword

	// Kept nowhere: not in the action's payload or result, nor in any event,
	// nor as anything but a hash, which is the credential's.
	for _, where := range []string{
		`SELECT count(*) FROM action WHERE payload::text LIKE '%' || $1 || '%' OR result::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM event WHERE payload::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM credential WHERE secret_hash LIKE '%' || $1 || '%' OR coalesce(label, '') LIKE '%' || $1 || '%'`,
	} {
		if n := b.Count(where, temporary); n != 0 {
			t.Fatalf("the temporary password is kept: %s", where)
		}
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND result ? 'temporary_password'`, *out.ActionID); n != 0 {
		t.Fatal("the action's result has the temporary password")
	}
	// A retry is answered as the reset was, without it, and resets nothing.
	replayed := b.MustCall(b.sato, tools.ToolMemberResetPassword, m{"course_id": b.course, "member_id": b.yukiM}, "reset-once")
	if !replayed.Replayed || replayed.Status != domain.StatusExecuted || strings.Contains(string(replayed.Result), "temporary_password") ||
		testkit.Result[tools.MemberResetPasswordOut](t, replayed).MemberID != b.yukiM {
		t.Fatalf("the replay: %+v %s", replayed, replayed.Result)
	}

	// The event says who reset whose, to whoever manages the members, and
	// not to the student.
	var sawReset bool
	for _, e := range testkit.Result[tools.EventListOut](t, b.do(t, b.sato, "event.list", m{"course_id": b.course})).Events {
		if e.Type == tools.EventMemberPasswordReset {
			sawReset = true
			if e.SubjectID == nil || *e.SubjectID != b.yukiM || e.StudentMemberID == nil || *e.StudentMemberID != b.yukiM ||
				!strings.Contains(string(e.Payload), b.satoM.String()) {
				t.Fatalf("the event: %+v %s", e, e.Payload)
			}
		}
	}
	if !sawReset {
		t.Fatal("no member.password_reset for Sato")
	}

	// Her session is over, and her old password with it; her own token does
	// nothing until she has set her own.
	if _, err := authn.Authenticate(ctx, before.Token); err == nil {
		t.Fatal("her session outlived the reset")
	}
	if _, err := authn.Login(ctx, "20230007", "yukis forgotten password"); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("her old password: %v", err)
	}
	p, err := authn.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("her token: %v", err)
	}
	if out, _ := b.CallWith(pipeline.Caller{ActorID: b.yuki, CredentialID: p.CredentialID}, "me.get", m{}, ""); out.Status != domain.StatusDenied ||
		reason(out) != "password_change_required" {
		t.Fatalf("her token, before she sets her own: %+v", out)
	}

	// She signs in with the temporary one, and is told to change it.
	sess, err := authn.Login(ctx, "20230007", temporary)
	if err != nil || !sess.PasswordChangeRequired {
		t.Fatalf("signing in with the temporary password: %+v %v", sess, err)
	}
	// Until she does, every other call is refused, reads and writes alike,
	// and a write is on record as refused.
	for _, call := range []struct {
		name string
		args m
	}{
		{"me.get", m{}}, {"me.memberships", m{}}, {"credential.list", m{}},
		{"document.list", m{"course_id": b.course}},
		{"submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "draft"}},
		{"credential.issue_token", m{"label": "another"}},
	} {
		out := b.MustCall(b.yuki, call.name, call.args, "pending-"+call.name)
		if out.Status != domain.StatusDenied || out.Error.Code != apperr.Forbidden || reason(out) != "password_change_required" {
			t.Fatalf("%s before she sets her own: %+v", call.name, out)
		}
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND status = 'denied' AND result->'error'->'details'->>'reason' = 'password_change_required'`,
		b.yuki); n != 2 {
		t.Fatalf("%d refused writes on record, want 2", n)
	}
	// Joining through a link is refused too.
	link := b.joinLink(t, b.sato, m{})
	if out := b.join(t, b.yuki, link.Token); out.Status != domain.StatusDenied || reason(out) != "password_change_required" {
		t.Fatalf("joining before she sets her own: %+v", out)
	}

	// Setting her own is the one call she may make, and not to the one she
	// was given.
	same := b.MustCall(b.yuki, "credential.set_password", m{"password": temporary}, "same")
	if same.Status != domain.StatusFailed || same.Error.Code != apperr.InvalidArgument || reason(same) != "password_unchanged" {
		t.Fatalf("the temporary password as her own: %+v", same)
	}
	b.do(t, b.yuki, "credential.set_password", m{"password": "yukis new own password"})

	// From then on she works as before.
	if me := testkit.Result[tools.MeOut](t, b.do(t, b.yuki, "me.get", m{})); me.ID != b.yuki {
		t.Fatalf("me.get after: %+v", me)
	}
	if _, err := authn.Authenticate(ctx, sess.Token); err != nil {
		t.Fatalf("the session she changed it in: %v", err)
	}
	for _, e := range testkit.Result[tools.EventListOut](t, b.do(t, b.yuki, "event.list", m{"course_id": b.course})).Events {
		if e.Type == tools.EventMemberPasswordReset {
			t.Fatalf("Yuki sees %+v", e)
		}
	}
	if s, err := authn.Login(ctx, "20230007", "yukis new own password"); err != nil || s.PasswordChangeRequired {
		t.Fatalf("signing in with her own: %+v %v", s, err)
	}
	if _, err := authn.Login(ctx, "20230007", temporary); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("the temporary password after: %v", err)
	}
	if out, _ := b.CallWith(pipeline.Caller{ActorID: b.yuki, CredentialID: p.CredentialID}, "me.get", m{}, ""); out.Status != domain.StatusExecuted {
		t.Fatalf("her token after: %+v", out)
	}

	// Her credentials say who set the temporary one, and that it was.
	creds := testkit.Result[tools.CredentialListOut](t, b.do(t, b.yuki, "credential.list", m{}))
	var marked int
	for _, c := range creds.Credentials {
		if c.MustChange {
			marked++
			if c.Kind != "password" || c.RevokedAt == nil || c.IssuedByID == nil || *c.IssuedByID != b.sato {
				t.Fatalf("a temporary password: %+v", c)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("%d temporary passwords listed, want the one Sato set", marked)
	}
}

func TestAPasswordIsResetOnlyForAStudentWhoseAccountReachesNothingMore(t *testing.T) {
	b := build(t)
	give := func(actor uuid.UUID, login string) {
		b.do(t, b.admin, "actor.update", m{"actor_id": actor, "login_id": login})
	}

	// Ken has neither a login ID nor an email: there is nothing to sign in
	// with.
	resetRefused(t, b.reset(t, b.sato, b.kenM), domain.StatusFailed, apperr.FailedPrecondition, tools.ResetNoSignInName)
	give(b.ken, "20230002")
	give(b.yuki, "20230007")

	// Not one's own seat; not an agent's; not a TA's.
	resetRefused(t, b.reset(t, b.sato, b.satoM), domain.StatusFailed, apperr.Forbidden, tools.ResetOwnSeat)
	resetRefused(t, b.reset(t, b.sato, b.graderM), domain.StatusFailed, apperr.Forbidden, tools.ResetNotAPerson)
	tanaka := b.person(t, "Tanaka", "tanaka@example.edu")
	tanakaM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": tanaka, "preset": "ta"})).MemberID
	resetRefused(t, b.reset(t, b.sato, tanakaM), domain.StatusFailed, apperr.Forbidden, tools.ResetNotAStudent)
	// An agent seated as a student is still an agent.
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "Study bot"})).ActorID
	botM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "student"})).MemberID
	resetRefused(t, b.reset(t, b.sato, botM), domain.StatusFailed, apperr.Forbidden, tools.ResetNotAPerson)
	// A seat of another course is none of this one's.
	if _, err := b.Call(b.sato, tools.ToolMemberResetPassword, m{"course_id": b.course, "member_id": uuid.New()}, "nobody"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("no such seat: %v", err)
	}

	// A paused seat, until it is resumed.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.kenM})
	resetRefused(t, b.reset(t, b.sato, b.kenM), domain.StatusFailed, apperr.FailedPrecondition, tools.ResetSeatNotActive)
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": b.kenM})
	if out := b.reset(t, b.sato, b.kenM); out.Status != domain.StatusExecuted {
		t.Fatalf("Ken, resumed: %+v", out)
	}

	// A student who is a TA in another course is an administrator's.
	other := testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create",
		m{"dept_id": b.dept, "term_id": b.term, "code": "CS102", "title": "More Computing"})).CourseID
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": other, "actor_id": b.sato})
	b.do(t, b.sato, "member.add", m{"course_id": other, "actor_id": b.ken, "preset": "ta"})
	resetRefused(t, b.reset(t, b.sato, b.kenM), domain.StatusFailed, apperr.Forbidden, tools.ResetSeatedOtherwise)

	// So is one who signs in through the identity provider, holds a platform
	// role, or administers a department.
	b.do(t, b.admin, "actor.link_sso", m{"actor_id": b.yuki, "provider": "hainanu-cas", "subject": "20230007"})
	resetRefused(t, b.reset(t, b.sato, b.yukiM), domain.StatusFailed, apperr.Forbidden, tools.ResetSSOLinked)
	mei := testkit.Result[tools.ActorOut](t, b.do(t, b.Root, "actor.register",
		m{"kind": "human", "display_name": "Mei", "login_id": "20230009", "platform_role": "admin"})).ActorID
	meiM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mei, "preset": "student"})).MemberID
	resetRefused(t, b.reset(t, b.sato, meiM), domain.StatusFailed, apperr.Forbidden, tools.ResetPlatformRole)
	jun := b.person(t, "Jun", "jun@example.edu")
	junM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": jun, "preset": "student"})).MemberID
	if out := b.reset(t, b.sato, junM); out.Status != domain.StatusExecuted {
		t.Fatalf("Jun, with an email alone: %+v", out)
	}
	b.do(t, b.admin, "department.add_admin", m{"dept_id": b.dept, "actor_id": jun})
	resetRefused(t, b.reset(t, b.sato, junM), domain.StatusFailed, apperr.Forbidden, tools.ResetAdministers)
}

func TestOnlyAPersonWhoManagesMembersWithoutApprovalResetsAPassword(t *testing.T) {
	b := build(t)
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "login_id": "20230007"})
	b.do(t, b.admin, "actor.update", m{"actor_id": b.ken, "login_id": "20230002"})
	manage := func(seat uuid.UUID, level string) {
		b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"member_manage": level}})
	}

	// A student manages nobody, nor does her own agent.
	if out := b.reset(t, b.yuki, b.kenM); out.Status != domain.StatusDenied || reason(out) != "permission_denied" {
		t.Fatalf("a student: %+v", out)
	}
	yukis := b.agent(t, b.yuki, "Yuki's helper")
	b.delegate(t, b.yuki, yukis, m{})
	if out := b.reset(t, yukis, b.kenM); out.Status != domain.StatusDenied || reason(out) != "permission_denied" {
		t.Fatalf("a student's delegate: %+v", out)
	}

	// Tanaka, a TA, manages members at each level in turn.
	tanaka := b.person(t, "Tanaka", "tanaka@example.edu")
	tanakaM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": tanaka, "preset": "ta"})).MemberID
	// With approval, not by proposal: nothing is queued, since the password
	// would be shown to whoever approved it.
	manage(tanakaM, "confirm_required")
	out := b.reset(t, tanaka, b.kenM)
	resetRefused(t, out, domain.StatusFailed, apperr.FailedPrecondition, "not_by_proposal")
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = $1 AND status = 'proposed'`, tools.ToolMemberResetPassword); n != 0 {
		t.Fatal("a reset was queued")
	}
	// Under review, not either: a rejection would take back nothing.
	manage(tanakaM, "pending_review")
	resetRefused(t, b.reset(t, tanaka, b.kenM), domain.StatusFailed, apperr.Forbidden, tools.ResetNotAutonomous)
	// Without approval, only for a seat within what he holds: a student's
	// seat hands in work, which a TA's does not.
	manage(tanakaM, "autonomous")
	out = b.reset(t, tanaka, b.kenM)
	resetRefused(t, out, domain.StatusFailed, apperr.Forbidden, tools.ResetBeyondYourSeat)
	if out.Error.Details["permission"] != "submission_write" {
		t.Fatalf("beyond his seat, but not for handing in work: %+v", out.Error)
	}
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": tanakaM, "perms": m{"submission_write": "autonomous"}})
	// And for a student he reaches: listed for Yuki, Ken is out of reach.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": tanakaM, "student_scope": "listed",
		"listed_students": []uuid.UUID{b.yukiM}})
	if out := b.reset(t, tanaka, b.kenM); out.Status != domain.StatusDenied || reason(out) != "student_out_of_scope" {
		t.Fatalf("a student out of his reach: %+v", out)
	}
	if out := b.reset(t, tanaka, b.yukiM); out.Status != domain.StatusExecuted {
		t.Fatalf("a student within it: %+v", out)
	}

	// An agent is never handed a password, whoever's it is and whatever it
	// holds: neither an agent nobody owns given member_manage, nor Sato's
	// own delegate given it up to his level.
	manage(b.graderM, "autonomous")
	resetRefused(t, b.reset(t, b.grader, b.kenM), domain.StatusFailed, apperr.Forbidden, tools.ResetPeopleOnly)
	helper := b.agent(t, b.sato, "Enrolment helper")
	b.delegate(t, b.sato, helper, m{"preset": "instructor", "perms": m{"member_manage": "autonomous"}})
	if got := b.membership(t, helper).Perms["member_manage"]; got != "autonomous" {
		t.Fatalf("the delegate's member_manage: %s", got)
	}
	resetRefused(t, b.reset(t, helper, b.kenM), domain.StatusFailed, apperr.Forbidden, tools.ResetPeopleOnly)
}
