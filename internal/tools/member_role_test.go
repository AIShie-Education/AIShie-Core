package tools_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// A seat's role is a fact of the roster, and member.set_role changes that
// fact and nothing else: not what the seat may do, which authorization reads
// from its levels and reach alone, and not what a student already has. It
// changes who is on the roster from then on.
func TestARoleChangesTheRosterAndNothingElse(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{g}})
	perms := func() string {
		var s string
		if err := b.Pool.QueryRow(t.Context(), `SELECT to_jsonb(m) - 'role' FROM course_member m WHERE id = $1`, b.yukiM).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := perms()
	onRoster := func() bool {
		out := testkit.Result[tools.SubmissionRosterOut](t, b.do(t, b.sato, "submission.roster", m{"course_id": b.course, "assignment_id": b.hw3}))
		return slices.ContainsFunc(out.Students, func(e tools.RosterEntry) bool { return e.StudentMemberID == b.yukiM })
	}
	if !onRoster() {
		t.Fatal("Yuki is not on the roster to begin with")
	}

	out := b.do(t, b.sato, "member.set_role", m{"course_id": b.course, "member_id": b.yukiM, "role": "ta"})
	if got := testkit.Result[tools.MemberSetRoleOut](t, out); got != (tools.MemberSetRoleOut{Role: "ta", Previous: "student", Changed: true}) {
		t.Fatalf("set_role: %+v", got)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND action_type = 'member.set_role' AND target_id = $2 AND status = 'executed'`,
		*out.ActionID, b.yukiM); n != 1 {
		t.Fatal("the change is not on record")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'member.role_changed' AND subject_id = $1
		AND payload->>'from' = 'student' AND payload->>'to' = 'ta'`, b.yukiM); n != 1 {
		t.Fatal("no member.role_changed event")
	}
	if b.memberView(t, b.yukiM).Role != "ta" {
		t.Fatal("member.get does not show the new role")
	}
	// Nothing the seat may do has changed: authorization never reads role.
	if after := perms(); after != before {
		t.Fatalf("the seat changed beyond its role:\n%s\n%s", before, after)
	}
	// What she was given stays hers, readable as before; work she handed in
	// may still be graded.
	if mine := testkit.Result[tools.GradeListOut](t, b.do(t, b.yuki, "grade.list", m{"course_id": b.course})); len(mine.Grades) == 0 {
		t.Fatal("Yuki no longer sees the grades she was given")
	}
	b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": g, "score": 82})
	// From now on she is off the roster: not listed, handing in nothing new,
	// given no new grade on a component.
	if onRoster() {
		t.Fatal("a TA is on the roster")
	}
	students := testkit.Result[tools.MemberListOut](t, b.do(t, b.sato, "member.list", m{"course_id": b.course, "role": "student"}))
	if slices.ContainsFunc(students.Members, func(v tools.MemberView) bool { return v.ID == b.yukiM }) {
		t.Fatal("member.list lists a TA as a student")
	}
	hw5 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW5", "points_possible": 10, "component_id": b.bucket})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw5})
	b.try(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw5, "body": "x"}, apperr.FailedPrecondition)
	b.try(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 50}, apperr.FailedPrecondition)

	// The same role again changes nothing, and says so.
	again := testkit.Result[tools.MemberSetRoleOut](t, b.do(t, b.sato, "member.set_role", m{"course_id": b.course, "member_id": b.yukiM, "role": "ta"}))
	if again.Changed || again.Previous != "ta" {
		t.Fatalf("setting the role a seat has: %+v", again)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'member.role_changed' AND subject_id = $1`, b.yukiM); n != 1 {
		t.Fatalf("%d member.role_changed events, want the one", n)
	}
	// Made a student again, she is on the roster and hands work in.
	b.do(t, b.sato, "member.set_role", m{"course_id": b.course, "member_id": b.yukiM, "role": "student"})
	if !onRoster() {
		t.Fatal("a student is not on the roster")
	}
	b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw5, "body": "x"})
	// Those who read the member list are told.
	if !slices.ContainsFunc(feed(t, b, b.sato), func(e tools.EventView) bool { return e.Type == "member.role_changed" }) {
		t.Fatal("the change is not in the instructor's feed")
	}
}

func TestWhoseRoleMayBeChanged(t *testing.T) {
	b := build(t)
	set := func(member uuid.UUID, role string) m {
		return m{"course_id": b.course, "member_id": member, "role": role}
	}
	// Not one's own seat, as no member tool acts on it.
	b.try(t, b.sato, "member.set_role", set(b.satoM, "observer"), apperr.Forbidden)
	// Only those who manage the course's members.
	if out := b.MustCall(b.yuki, "member.set_role", set(b.kenM, "ta"), "yuki"); out.Status != domain.StatusDenied {
		t.Fatalf("a student changing a role: %+v", out)
	}
	b.try(t, b.sato, "member.set_role", set(b.kenM, "dean"), apperr.InvalidArgument)
	if _, err := b.Call(b.sato, "member.set_role", set(uuid.New(), "ta"), "nobody"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a seat that is not there: %v", err)
	}
	// A delegate's seat is its principal's agent, always assistant.
	bot := b.agent(t, b.sato, "Sato's assistant")
	seat := b.delegate(t, b.sato, bot, m{})
	b.try(t, b.sato, "member.set_role", set(seat, "student"), apperr.FailedPrecondition)
	out := b.MustCall(b.sato, "member.set_role", set(seat, "ta"), "delegate")
	if reason(out) != "delegate_seat" {
		t.Fatalf("a delegate's role: %+v", out)
	}
	// A removed seat is history.
	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.kenM})
	b.try(t, b.sato, "member.set_role", set(b.kenM, "ta"), apperr.Conflict)
	if n := b.Count(`SELECT count(*) FROM course_member WHERE role <> 'student' AND id IN ($1, $2)`, b.yukiM, b.kenM); n != 0 {
		t.Fatal("a refused change was kept")
	}
}

// A delegate that manages the course's members changes their roles for its
// principal, as it changes the rest of their seats: never its principal's own
// seat's role, nor another of its principal's agents', whose seats only their
// owner answers for — even to give them the role they have.
func TestADelegateChangesNoRoleOfItsPrincipalsOrItsSiblings(t *testing.T) {
	b := build(t)
	helper, seat := b.helper(t, "Sato's enrolment helper", "autonomous")
	_, sibling := b.helper(t, "Sato's other agent", "denied")
	set := func(member uuid.UUID, role string) m {
		return m{"course_id": b.course, "member_id": member, "role": role}
	}
	notYours(t, "its principal's seat", b.MustCall(helper, "member.set_role", set(b.satoM, "observer"), "principal"))
	notYours(t, "its principal's seat, the role it has", b.MustCall(helper, "member.set_role", set(b.satoM, "instructor"), "principal-same"))
	notYours(t, "its principal's other agent", b.MustCall(helper, "member.set_role", set(sibling, "ta"), "sibling"))
	b.try(t, helper, "member.set_role", set(seat, "ta"), apperr.Forbidden) // its own
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id IN ($1, $2, $3) AND role = CASE WHEN id = $1 THEN 'instructor' ELSE 'assistant' END`,
		b.satoM, sibling, seat); n != 3 {
		t.Fatal("a refused change was kept")
	}
	// Anyone else's it changes, on record from its own seat.
	out := b.do(t, helper, "member.set_role", set(b.kenM, "ta"))
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND member_id = $2`, *out.ActionID, seat); n != 1 {
		t.Fatal("the change is not on record as the helper's")
	}
	if b.memberView(t, b.kenM).Role != "ta" {
		t.Fatal("the helper did not change Ken's role")
	}
}
