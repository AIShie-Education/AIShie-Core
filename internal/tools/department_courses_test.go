package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Courses and people for department administrators (docs/schema.md §2.10):
// they manage the courses of their subtree from outside, as a platform
// administrator does, find people by their whole email, and invite those
// whose accounts reach nothing beyond what they administer. Inside a course
// they are what a seat there makes them.

// courses is what course.list shows the caller, by id.
func (w tree) courses(actor uuid.UUID, args m) []uuid.UUID {
	w.t.Helper()
	var got []uuid.UUID
	for _, c := range testkit.Result[tools.CourseListOut](w.t, w.do(actor, "course.list", args)).Courses {
		got = append(got, c.ID)
	}
	slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return got
}

func sorted(ids ...uuid.UUID) []uuid.UUID {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}

// §9.3 of the spec, "A parent department's administrator manages a
// grandchild's course": Ada, at Engineering, manages AI's courses two levels
// down, each call recorded as made by her appointment there.
func TestAParentDepartmentsAdministratorManagesAGrandchildsCourses(t *testing.T) {
	w := newTree(t)
	made := w.do(w.Ada, "course.create", m{"dept_id": w.D, "term_id": w.Term, "code": "AI201", "title": "Machine Learning"})
	w.wantCapacity(made, domain.AuthorityDepartment, w.F)
	ml := testkit.Result[tools.CourseCreateOut](t, made).CourseID
	if n := w.Count(`SELECT count(*) FROM course WHERE id = $1 AND dept_id = $2 AND created_by_actor_id = $3 AND status = 'draft'`,
		ml, w.D, w.Ada); n != 1 {
		t.Fatal("the course is not in AI, a draft, made by Ada")
	}
	w.wantCapacity(w.do(w.Ada, "course.update", m{"course_id": w.CD, "title": "Artificial Intelligence I"}), domain.AuthorityDepartment, w.F)
	w.wantCapacity(w.do(w.Ada, "course.archive", m{"course_id": w.CD}), domain.AuthorityDepartment, w.F)
	w.wantCapacity(w.do(w.Ada, "course.activate", m{"course_id": w.CD}), domain.AuthorityDepartment, w.F)
	w.do(w.Ada, "course.activate", m{"course_id": ml})

	found := testkit.Result[tools.ActorLookupOut](t, w.do(w.Ada, "actor.lookup_by_email", m{"email": "KEN@EXAMPLE.EDU"}))
	if found.ActorID != w.Ken || found.DisplayName != "Ken" {
		t.Fatalf("actor.lookup_by_email: %+v", found)
	}
	seated := w.do(w.Ada, "course.seat_instructor", m{"course_id": ml, "actor_id": found.ActorID})
	w.wantCapacity(seated, domain.AuthorityDepartment, w.F)
	if n := w.Count(`SELECT count(*) FROM course_member WHERE course_id = $1 AND actor_id = $2 AND role = 'instructor'
		AND added_by_actor_id = $3`, ml, w.Ken, w.Ada); n != 1 {
		t.Fatal("Ken is not seated as the instructor Ada made him")
	}

	if got, want := w.courses(w.Ada, m{}), sorted(w.CD, w.CArch, w.CS, w.CS2, ml); !slices.Equal(got, want) {
		t.Fatalf("Ada's courses: %v, want %v", got, want)
	}
	if got, want := w.courses(w.Ada, m{"within_dept_id": w.S}), sorted(w.CD, w.CArch, w.CS, ml); !slices.Equal(got, want) {
		t.Fatalf("Ada's courses within Computing: %v, want %v", got, want)
	}
	if got, want := w.courses(w.Ada, m{"dept_id": w.S}), sorted(w.CS); !slices.Equal(got, want) {
		t.Fatalf("Ada's courses directly in Computing: %v, want %v", got, want)
	}
	if got := w.courses(w.Ada, m{"within_dept_id": w.F2}); len(got) != 0 {
		t.Fatalf("Ada's courses within Humanities: %v", got)
	}

	moved := w.do(w.Ada, "course.move", m{"course_id": w.CD, "dept_id": w.S2})
	w.wantCapacity(moved, domain.AuthorityDepartment, w.F)
	if n := w.Count(`SELECT count(*) FROM course WHERE id = $1 AND dept_id = $2`, w.CD, w.S2); n != 1 {
		t.Fatal("AI101 is not in Design")
	}
	if n := w.Count(`SELECT count(*) FROM event WHERE type = 'course.moved' AND course_id = $1 AND action_id = $2
		AND payload->>'from_dept_id' = $3 AND payload->>'to_dept_id' = $4`, w.CD, *moved.ActionID, w.D.String(), w.S2.String()); n != 1 {
		t.Fatal("no course.moved in the course's feed, saying from where to where")
	}
	w.fails(w.Ada, "course.move", m{"course_id": w.CD, "dept_id": w.S2}, apperr.Conflict, "same_department")
	// The course's own people see it moved, by the same rule as its other news.
	feed := testkit.Result[tools.EventListOut](t, w.do(w.Instructor[w.CD], "event.list", m{"course_id": w.CD}))
	if !slices.ContainsFunc(feed.Events, func(e tools.EventView) bool { return e.Type == tools.EventCourseMoved }) {
		t.Fatal("the course's instructor does not see it moved")
	}

	// Administering a course gives nothing inside it. Seated like anyone
	// else, and recorded, she is what her seat makes her.
	w.denied(w.Ada, "course.get", m{"course_id": w.CD}, "not_a_member")
	w.denied(w.Ada, "member.list", m{"course_id": w.CD}, "not_a_member")
	w.denied(w.Ada, "assignment.create", m{"course_id": w.CD, "title": "HW1", "points_possible": 10}, "not_a_member")
	if n := w.Count(`SELECT count(*) FROM course_member WHERE course_id = $1 AND actor_id = $2`, w.CD, w.Ada); n != 0 {
		t.Fatal("Ada was given a seat by looking")
	}
	w.wantCapacity(w.do(w.Ada, "course.seat_instructor", m{"course_id": w.CD, "actor_id": w.Ada}), domain.AuthorityDepartment, w.F)
	w.do(w.Ada, "course.get", m{"course_id": w.CD})
	w.wantCapacity(w.do(w.Ada, "assignment.create", m{"course_id": w.CD, "title": "HW1", "points_possible": 10}), "", uuid.Nil)
	w.everyEmittedTypeHasARule()
}

// §9.3 of the spec, "Not a sibling's course": whatever is outside an
// appointment's subtree, a sibling's or at the top of the tree, is refused,
// on the record, and not listed; and nothing is moved out of reach.
func TestNotASiblingsCourseNorOneAtTheTop(t *testing.T) {
	w := newTree(t)
	var law uuid.UUID
	if err := w.Pool.QueryRow(t.Context(), `INSERT INTO department (name) VALUES ('Law') RETURNING id`).Scan(&law); err != nil {
		t.Fatal(err)
	}
	top := testkit.Result[tools.CourseCreateOut](t, w.do(w.Admin, "course.create",
		m{"dept_id": law, "term_id": w.Term, "code": "LAW101", "title": "Contract"})).CourseID
	atU := testkit.Result[tools.CourseCreateOut](t, w.do(w.Admin, "course.create",
		m{"dept_id": w.U, "term_id": w.Term, "code": "UNI101", "title": "Orientation"})).CourseID

	for _, c := range []struct {
		who, course uuid.UUID
	}{{w.Bob, w.CS2}, {w.Bob, w.CS3}, {w.Bob, top}, {w.Bob, atU}, {w.Ada, top}, {w.Ada, atU}, {w.Ada, w.CS3}, {w.Carol, w.CD}} {
		w.denied(c.who, "course.update", m{"course_id": c.course, "title": "Taken"}, "department_out_of_scope")
		w.denied(c.who, "course.archive", m{"course_id": c.course}, "department_out_of_scope")
		w.denied(c.who, "course.activate", m{"course_id": c.course}, "department_out_of_scope")
		w.denied(c.who, "course.seat_instructor", m{"course_id": c.course, "actor_id": c.who}, "department_out_of_scope")
		w.denied(c.who, "course.move", m{"course_id": c.course, "dept_id": w.S}, "department_out_of_scope")
	}
	w.denied(w.Bob, "course.create", m{"dept_id": w.S2, "term_id": w.Term, "code": "DES201", "title": "Type"}, "department_out_of_scope")
	w.denied(w.Bob, "course.create", m{"dept_id": w.F, "term_id": w.Term, "code": "ENG101", "title": "Statics"}, "department_out_of_scope")
	w.denied(w.Ada, "course.create", m{"dept_id": law, "term_id": w.Term, "code": "LAW102", "title": "Torts"}, "department_out_of_scope")
	if n := w.Count(`SELECT count(*) FROM course WHERE title IN ('Taken', 'Type', 'Statics', 'Torts')`); n != 0 {
		t.Fatal("a refused change was made")
	}
	// No seat there to read by.
	w.denied(w.Bob, "course.get", m{"course_id": w.CS2}, "not_a_member")
	if got, want := w.courses(w.Bob, m{}), sorted(w.CD, w.CArch, w.CS); !slices.Equal(got, want) {
		t.Fatalf("Bob's courses: %v, want %v", got, want)
	}
	if got := w.courses(w.Bob, m{"dept_id": w.S2}); len(got) != 0 {
		t.Fatalf("Bob's courses in Design: %v", got)
	}

	// AI101 is Bob's, and Design is not: it stays where he could reach it.
	w.fails(w.Bob, "course.move", m{"course_id": w.CD, "dept_id": w.S2}, apperr.Forbidden, "destination_out_of_scope")
	w.fails(w.Ada, "course.move", m{"course_id": w.CD, "dept_id": w.S3}, apperr.Forbidden, "destination_out_of_scope")
	w.fails(w.Ada, "course.move", m{"course_id": w.CD, "dept_id": w.U}, apperr.Forbidden, "destination_out_of_scope")
	if n := w.Count(`SELECT count(*) FROM course WHERE id = $1 AND dept_id = $2`, w.CD, w.D); n != 1 {
		t.Fatal("AI101 was moved out of reach")
	}
	w.do(w.Bob, "course.move", m{"course_id": w.CD, "dept_id": w.S})
	if _, err := w.Call(w.Bob, "course.move", m{"course_id": w.CD, "dept_id": uuid.New()}, "nowhere"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("to a department that does not exist: %v", err)
	}
	if _, err := w.Call(w.Bob, "course.update", m{"course_id": uuid.New(), "title": "X"}, "no-course"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a course that does not exist: %v", err)
	}

	// A department that moves takes its courses, and their administrators
	// with them: once Ada has put AI under Design, AI101 is Bob's no more.
	w.do(w.Bob, "course.move", m{"course_id": w.CD, "dept_id": w.D})
	w.do(w.Ada, "department.move", m{"dept_id": w.D, "parent_id": w.S2})
	w.denied(w.Bob, "course.update", m{"course_id": w.CD, "title": "Mine"}, "department_out_of_scope")
	if got, want := w.courses(w.Bob, m{}), sorted(w.CS); !slices.Equal(got, want) {
		t.Fatalf("Bob's courses after AI moved: %v, want %v", got, want)
	}

	// Nobody who administers nothing gets as far as asking which course.
	for _, who := range []uuid.UUID{w.Yuki, w.Robo, w.Instructor[w.CD]} {
		w.denied(who, "course.update", m{"course_id": w.CD, "title": "Mine"}, "platform_role_required")
		w.denied(who, "course.create", m{"dept_id": w.D, "term_id": w.Term, "code": "X1", "title": "X"}, "platform_role_required")
		if out := w.MustCall(who, "course.list", m{}, ""); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "platform_role_required" {
			t.Fatalf("course.list by someone who administers nothing: %+v", out)
		}
	}
	// An archived course is closed to its administrators as to anyone, but
	// for opening it again.
	w.denied(w.Ada, "course.update", m{"course_id": w.CArch, "title": "Revived"}, "course_archived")
	w.denied(w.Ada, "course.move", m{"course_id": w.CArch, "dept_id": w.S}, "course_archived")
	w.do(w.Ada, "course.activate", m{"course_id": w.CArch})
}

// A move is measured with the course held, where it is then: moved out of
// the caller's reach while they waited, it is not theirs to take back.
func TestACourseMovedAwayMeanwhileIsNotTakenBack(t *testing.T) {
	w := newTree(t)
	tx, commit := w.begin()
	if _, err := tx.Exec(t.Context(), `UPDATE course SET dept_id = $2 WHERE id = $1`, w.CD, w.S3); err != nil {
		t.Fatal(err)
	}
	// The gate finds AI101 in AI, which Ada covers.
	done := w.start(w.Ada, "course.move", m{"course_id": w.CD, "dept_id": w.S2})
	w.blocked(1, done)
	commit()
	out := <-done
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["reason"] != "department_out_of_scope" {
		t.Fatalf("taking back a course moved out of reach: %+v", out)
	}
	if n := w.Count(`SELECT count(*) FROM course WHERE id = $1 AND dept_id = $2`, w.CD, w.S3); n != 1 {
		t.Fatal("AI101 was taken back")
	}
}

// §9.3 of the spec, "Lookup by email": the whole address, in any case, and
// no more than who the person is, whether they can sign in, and whether the
// caller may invite them.
func TestAPersonIsFoundByTheirWholeEmailAlone(t *testing.T) {
	w := newTree(t)
	lookup := func(actor uuid.UUID, email string) tools.ActorLookupOut {
		t.Helper()
		return testkit.Result[tools.ActorLookupOut](t, w.do(actor, "actor.lookup_by_email", m{"email": email}))
	}
	ken := w.do(w.Bob, "actor.lookup_by_email", m{"email": " ken@example.edu "})
	if got := testkit.Result[tools.ActorLookupOut](t, ken); got.ActorID != w.Ken || got.Kind != "human" || got.Status != "active" ||
		got.CanSignIn || got.InviteExpiresAt != nil {
		t.Fatalf("Ken: %+v", got)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ken.Result, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"actor_id", "can_sign_in", "display_name", "invitable", "kind", "status"}; !slices.Equal(keys, want) ||
		strings.Contains(string(ken.Result), "example.edu") {
		t.Fatalf("a lookup says %s", ken.Result)
	}
	for _, near := range []string{"ken@example", "ken", "en@example.edu", "ken@example.edu.", "%@example.edu", "k_n@example.edu", "*"} {
		if _, err := w.Call(w.Bob, "actor.lookup_by_email", m{"email": near}, ""); !apperr.Is(err, apperr.NotFound) {
			t.Fatalf("%q: %v, want not found", near, err)
		}
	}
	if _, err := w.Call(w.Bob, "actor.lookup_by_email", m{"email": "  "}, ""); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("no email: %v", err)
	}
	for _, who := range []uuid.UUID{w.Yuki, w.Robo, w.Instructor[w.CD]} {
		if out := w.MustCall(who, "actor.lookup_by_email", m{"email": "ken@example.edu"}, ""); out.Status != domain.StatusDenied ||
			out.Error.Details["reason"] != "platform_role_required" {
			t.Fatalf("a lookup by someone who administers nothing: %+v", out)
		}
	}

	// Whether the caller may invite them is the caller's rule, as they stand.
	w.Exec(`UPDATE actor SET email = 'robo@example.edu' WHERE id = $1`, w.Robo)
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, w.Dan)
	for _, c := range []struct {
		email string
		want  bool
	}{
		{"ken@example.edu", true},    // seated only beneath Engineering
		{"eve@example.edu", true},    // seated nowhere
		{"chan@example.edu", false},  // administers Computing
		{"lin@example.edu", false},   // owns an agent
		{"admin@example.edu", false}, // holds a platform role
		{"robo@example.edu", false},  // not a person
		{"dan@example.edu", false},   // suspended
		{"ada@example.edu", false},   // herself
	} {
		if got := lookup(w.Ada, c.email); got.Invitable != c.want {
			t.Errorf("Ada may invite %s: %v, want %v", c.email, got.Invitable, c.want)
		}
	}
	// Ken is a student in Design as well, which is not Bob's.
	if lookup(w.Bob, "ken@example.edu").Invitable {
		t.Error("Bob may invite Ken, who is seated beyond him")
	}
	// A platform administrator's is actor.invite's: only root reaches root.
	w.Exec(`UPDATE actor SET email = 'root@example.edu' WHERE id = $1`, w.Root)
	if !lookup(w.Admin, "ken@example.edu").Invitable || lookup(w.Admin, "root@example.edu").Invitable {
		t.Error("a platform administrator's rule is not actor.invite's")
	}
	if got := lookup(w.Admin, "robo@example.edu"); got.Kind != "agent" {
		t.Errorf("Robo: %+v", got)
	}
}

// §9.3 of the spec, "Invitations": a department administrator registers
// and invites new people, and invites again only someone whose account
// reaches nothing beyond what they administer, which is asked again when the
// invitation is taken up.
func TestADepartmentAdministratorInvitesOnlyWithinReach(t *testing.T) {
	w := newTree(t)
	authn := auth.NewAuthenticator(w.Pool, time.Hour)
	takeUp := func(token string) error {
		t.Helper()
		_, err := authn.AcceptInvite(context.Background(), token, "a long enough password")
		return err
	}
	inviteNew := func(name string) tools.ActorInviteNewOut {
		t.Helper()
		out := w.do(w.Ada, "actor.invite_new", m{"display_name": name, "email": strings.ToLower(name) + "@example.edu "})
		w.wantCapacity(out, domain.AuthorityDepartment, uuid.Nil)
		if n := w.Count(`SELECT count(*) FROM action WHERE id = $1 AND result::text LIKE '%aisinv_%'`, *out.ActionID); n != 0 {
			t.Fatal("the invitation is in the action log")
		}
		return testkit.Result[tools.ActorInviteNewOut](t, out)
	}

	pat := inviteNew("Pat")
	if !strings.HasPrefix(pat.Token, "aisinv_") || pat.Email != "pat@example.edu" || pat.ExpiresAt.Before(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("actor.invite_new: %+v", pat)
	}
	if n := w.Count(`SELECT count(*) FROM actor WHERE id = $1 AND kind = 'human' AND display_name = 'Pat' AND created_by_actor_id = $2`,
		pat.ActorID, w.Ada); n != 1 {
		t.Fatal("Pat is not registered as Ada made him")
	}
	if n := w.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'invite' AND issued_by_actor_id = $2`, pat.ActorID, w.Ada); n != 1 {
		t.Fatal("the invitation does not say Ada issued it")
	}
	if n := w.Count(`SELECT count(*) FROM event WHERE subject_id = $1 AND type IN ('actor.registered', 'actor.invited') AND course_id IS NULL`,
		pat.ActorID); n != 2 {
		t.Fatal("no actor.registered and actor.invited")
	}
	taken := w.MustCall(w.Ada, "actor.invite_new", m{"display_name": "Pat again", "email": "PAT@example.edu"}, "pat-again")
	if taken.Status != domain.StatusFailed || taken.Error.Code != apperr.Conflict || taken.Error.Details["reason"] != "email_taken" ||
		fmt.Sprint(taken.Error.Details["actor_id"]) != pat.ActorID.String() {
		t.Fatalf("an email already registered: %+v", taken)
	}
	w.fails(w.Ada, "actor.invite_new", m{"display_name": "Nobody", "email": "nobody"}, apperr.InvalidArgument, "")
	w.fails(w.Ada, "actor.invite_new", m{"display_name": " ", "email": "blank@example.edu"}, apperr.InvalidArgument, "")
	w.fails(w.Ada, "actor.invite_new", m{"display_name": "Late", "email": "late@example.edu", "expires_in_days": 31}, apperr.InvalidArgument, "")

	// Seated where she administers, and invited again: still hers to invite.
	w.do(w.Ada, "course.seat_instructor", m{"course_id": w.CD, "actor_id": pat.ActorID})
	again := testkit.Result[tools.ActorInviteOut](t, w.do(w.Ada, "actor.invite", m{"actor_id": pat.ActorID}))
	if err := takeUp(pat.Token); err == nil {
		t.Fatal("the invitation replaced by a newer one was taken up")
	}
	if err := takeUp(again.Token); err != nil {
		t.Fatalf("Pat takes up Ada's invitation: %v", err)
	}
	w.fails(w.Ada, "actor.invite", m{"actor_id": pat.ActorID}, apperr.Forbidden, "invite_not_allowed")
	if out := w.MustCall(w.Ada, "actor.invite", m{"actor_id": pat.ActorID}, "pat-signed-in"); out.Error.Details["why"] != auth.InviteSignedIn {
		t.Fatalf("inviting someone who has signed in: %+v", out)
	}

	// Each clause of the rule, and nobody herself. An agent is refused
	// before any of it, as it is whoever invites it: it holds tokens, and
	// never signs in.
	w.fails(w.Ada, "actor.invite", m{"actor_id": w.Robo}, apperr.Forbidden, auth.ReasonAgentsUseTokens)
	for who, why := range map[uuid.UUID]string{
		w.Chan: auth.InviteAdministers, w.Lin: auth.InviteOwnsAgents, w.Admin: auth.InvitePlatformRole,
		w.Actor("system", "system"): auth.InviteNotAPerson,
	} {
		if out := w.MustCall(w.Ada, "actor.invite", m{"actor_id": who}, "why-"+who.String()); out.Status != domain.StatusFailed ||
			out.Error.Code != apperr.Forbidden || out.Error.Details["reason"] != "invite_not_allowed" || out.Error.Details["why"] != why {
			t.Fatalf("inviting %v: %+v, want %s", who, out, why)
		}
	}
	if out := w.MustCall(w.Bob, "actor.invite", m{"actor_id": w.Ken}, "ken-by-bob"); out.Error == nil || out.Error.Details["why"] != auth.InviteSeatedElsewhere {
		t.Fatalf("Bob inviting Ken, who is seated beyond him: %+v", out)
	}
	w.fails(w.Ada, "actor.invite", m{"actor_id": w.Ada}, apperr.Forbidden, "")
	w.wantCapacity(w.do(w.Ada, "actor.invite", m{"actor_id": w.Ken}), domain.AuthorityDepartment, uuid.Nil)
	w.denied(w.Yuki, "actor.invite", m{"actor_id": w.Ken}, "platform_role_required")
	w.denied(w.Yuki, "actor.invite_new", m{"display_name": "Sly", "email": "sly@example.edu"}, "platform_role_required")
	w.denied(w.Robo, "actor.invite_new", m{"display_name": "Sly", "email": "sly@example.edu"}, "platform_role_required")

	// Quinn is seated beyond Ada before taking her invitation up: refused,
	// as an expired one is. A platform administrator's invitation works.
	quinn := inviteNew("Quinn")
	w.do(w.Carol, "course.seat_instructor", m{"course_id": w.CS3, "actor_id": quinn.ActorID})
	if err := takeUp(quinn.Token); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("an invitation that now reaches beyond its issuer: %v", err)
	}
	if n := w.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'password'`, quinn.ActorID); n != 0 {
		t.Fatal("Quinn has a password from it")
	}
	byAdmin := testkit.Result[tools.ActorInviteOut](t, w.do(w.Admin, "actor.invite", m{"actor_id": quinn.ActorID}))
	if err := takeUp(byAdmin.Token); err != nil {
		t.Fatalf("a platform administrator's invitation: %v", err)
	}

	// Rae's is taken up after Ada's appointment has ended; Sam's after Ada
	// was suspended. Neither issuer could make it now.
	rae, sam := inviteNew("Rae"), inviteNew("Sam")
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, w.Ada)
	if err := takeUp(sam.Token); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("an invitation from an issuer since suspended: %v", err)
	}
	w.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, w.Ada)
	w.do(w.Admin, "department.remove_admin", m{"dept_id": w.F, "actor_id": w.Ada})
	if err := takeUp(rae.Token); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("an invitation from an issuer who administers nothing now: %v", err)
	}
	// One made before issuers were recorded is a platform administrator's.
	w.Exec(`UPDATE credential SET issued_by_actor_id = NULL WHERE actor_id = $1 AND kind = 'invite' AND revoked_at IS NULL`, sam.ActorID)
	if err := takeUp(sam.Token); err != nil {
		t.Fatalf("an invitation with no issuer recorded: %v", err)
	}
	w.everyEmittedTypeHasARule()
}

// Ending an appointment takes effect on the next call: a call relying on it
// that is in flight finishes first, and one that comes after is refused.
func TestAnEndedAppointmentManagesNoCourseFromTheNextCall(t *testing.T) {
	w := newTree(t)
	title := func() string {
		t.Helper()
		var s string
		if err := w.Pool.QueryRow(t.Context(), `SELECT title FROM course WHERE id = $1`, w.CD).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Ada's change is held at the course's row, holding her appointment; its
	// ending waits for her change, which goes through.
	tx, commit := w.begin()
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM course WHERE id = $1 FOR NO KEY UPDATE`, w.CD); err != nil {
		t.Fatal(err)
	}
	change := w.start(w.Ada, "course.update", m{"course_id": w.CD, "title": "Before it ended"})
	w.blocked(1, change)
	ending := w.start(w.Admin, "department.remove_admin", m{"dept_id": w.F, "actor_id": w.Ada})
	w.blocked(2, change, ending)
	commit()
	if out := <-change; out.Status != domain.StatusExecuted {
		t.Fatalf("the change in flight: %+v", out)
	}
	if out := <-ending; out.Status != domain.StatusExecuted {
		t.Fatalf("the ending: %+v", out)
	}
	if title() != "Before it ended" {
		t.Fatal("the change in flight was lost")
	}
	w.denied(w.Ada, "course.update", m{"course_id": w.CD, "title": "After it ended"}, "platform_role_required")
	w.denied(w.Ada, "course.create", m{"dept_id": w.D, "term_id": w.Term, "code": "AI301", "title": "Late"}, "platform_role_required")
	w.denied(w.Ada, "course.move", m{"course_id": w.CD, "dept_id": w.S2}, "platform_role_required")
	w.denied(w.Ada, "actor.invite_new", m{"display_name": "Late", "email": "late@example.edu"}, "platform_role_required")
	for _, read := range []struct {
		name string
		args m
	}{{"course.list", m{}}, {"actor.lookup_by_email", m{"email": "ken@example.edu"}}} {
		if out := w.MustCall(w.Ada, read.name, read.args, ""); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "platform_role_required" {
			t.Fatalf("%s after the appointment ended: %+v", read.name, out)
		}
	}

	// The other way round: the ending holds the appointment, and her change,
	// waiting for it, finds it ended. Bob's is held at the feed's lock.
	tx, commit = w.begin()
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1, 0)`, int32(0x41495345)); err != nil {
		t.Fatal(err)
	}
	ending = w.start(w.Admin, "department.remove_admin", m{"dept_id": w.S, "actor_id": w.Bob})
	w.blocked(1, ending)
	change = w.start(w.Bob, "course.update", m{"course_id": w.CD, "title": "Bob's"})
	w.blocked(2, ending, change)
	commit()
	if out := <-ending; out.Status != domain.StatusExecuted {
		t.Fatalf("the ending: %+v", out)
	}
	if out := <-change; out.Status != domain.StatusDenied || out.Error.Details["reason"] != "department_out_of_scope" {
		t.Fatalf("a change that waited for the ending: %+v", out)
	}
	if title() != "Before it ended" {
		t.Fatal("a change made after the appointment ended went through")
	}
	w.denied(w.Bob, "course.update", m{"course_id": w.CD, "title": "Bob's"}, "platform_role_required")
}
