package tools_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// try calls a tool expecting it to be recorded as failed with the given code.
func (b *built) try(t *testing.T, actor uuid.UUID, name string, args m, want apperr.Code) {
	t.Helper()
	b.key++
	out, err := b.Call(actor, name, args, "try-"+uuid.NewString())
	switch {
	case err != nil && apperr.Is(err, want):
		return // refused before it was an attempt
	case err != nil:
		t.Fatalf("%s: %v, want %s", name, err, want)
	case out.Status == domain.StatusExecuted || out.Status == domain.StatusProposed:
		t.Fatalf("%s went through (%s), want %s", name, out.Status, want)
	case out.Error == nil || out.Error.Code != want:
		t.Fatalf("%s: %+v, want %s", name, out.Error, want)
	}
}

// ---------------------------------------------------------------------------
// Nobody hands out more than they hold
// ---------------------------------------------------------------------------

func TestMemberManagementCannotEscalate(t *testing.T) {
	b := build(t)
	// A TA who may manage members, but who grades only with confirmation,
	// cannot post, and is limited to Yuki.
	helper := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Helper"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "ta",
		"perms":         m{"member_manage": "autonomous", "grade_submit": "confirm_required"},
		"student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	puppet := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "Puppet"})).ActorID

	add := func(args m) m {
		args["course_id"], args["actor_id"] = b.course, puppet
		return args
	}
	// The obvious attack: seat a second account as instructor.
	b.try(t, helper, "member.add", add(m{"preset": "instructor"}), apperr.Forbidden)
	// Subtler: a harmless preset, with one permission raised past your own.
	b.try(t, helper, "member.add", add(m{"preset": "observer", "perms": m{"grade_post": "confirm_required"}, "student_scope": "listed"}), apperr.Forbidden)
	b.try(t, helper, "member.add", add(m{"preset": "observer", "perms": m{"grade_submit": "autonomous"}, "student_scope": "listed"}), apperr.Forbidden)
	// Scope is held to the same rule: not the whole class, not Ken.
	b.try(t, helper, "member.add", add(m{"preset": "observer"}), apperr.Forbidden) // observer's scope is 'all'
	b.try(t, helper, "member.add", add(m{"preset": "observer", "student_scope": "listed", "listed_students": []uuid.UUID{b.kenM}}), apperr.Forbidden)

	// Within what they hold, it works.
	seated := testkit.Result[tools.MemberIDOut](t, b.do(t, helper, "member.add", add(m{"preset": "observer",
		"perms": m{"grade_submit": "confirm_required"}, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}}))).MemberID

	// And raising it afterwards is held to the same rule.
	b.try(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_submit": "autonomous"}}, apperr.Forbidden)
	b.try(t, helper, "member.rescope", m{"course_id": b.course, "member_id": seated, "student_scope": "all"}, apperr.Forbidden)
	b.try(t, helper, "member.rescope", m{"course_id": b.course, "member_id": seated, "listed_students": []uuid.UUID{b.yukiM, b.kenM}}, apperr.Forbidden)
	// Lowering is always allowed, even of something the manager does not hold.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_post": "autonomous"}})
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_post": "denied"}})

	// Nobody manages their own seat.
	var helperM uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM course_member WHERE actor_id = $1`, helper).Scan(&helperM); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"member.pause", "member.remove"} {
		b.try(t, helper, name, m{"course_id": b.course, "member_id": helperM}, apperr.Forbidden)
	}
	b.try(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": helperM, "perms": m{"grade_post": "autonomous"}}, apperr.Forbidden)

	// Typos are refused, not ignored.
	b.try(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_sumbit": "denied"}}, apperr.InvalidArgument)
	b.try(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_submit": "sometimes"}}, apperr.InvalidArgument)
}

// ---------------------------------------------------------------------------
// docs/schema.md §4, "enforced by the application", one by one
// ---------------------------------------------------------------------------

func TestSameCourseRules(t *testing.T) {
	b := build(t)
	// A second course, with a student and an assignment of its own.
	if n := len(testkit.Result[tools.TermListOut](t, b.do(t, b.sato, "term.list", m{})).Terms); n == 0 {
		t.Fatal("any signed-in actor may list terms")
	}
	term, dept := b.term, b.dept
	cs205 := testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create", m{"dept_id": dept, "term_id": term, "code": "CS205", "title": "Algorithms"}))
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": cs205.CourseID, "actor_id": b.sato})
	foreignStudent := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": cs205.CourseID, "actor_id": b.ken, "preset": "student"})).MemberID
	foreignHW := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": cs205.CourseID, "title": "PS1", "points_possible": 10})).ID

	t.Run("scope rows name members and assignments of the same course", func(t *testing.T) {
		someone := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "x"})).ActorID
		b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": someone, "preset": "tutor", "listed_students": []uuid.UUID{foreignStudent}}, apperr.FailedPrecondition)
		b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": someone, "preset": "grader", "listed_assignments": []uuid.UUID{foreignHW}}, apperr.FailedPrecondition)
		// A listed "student" must be a student: listing the instructor is refused.
		b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": someone, "preset": "tutor", "listed_students": []uuid.UUID{b.satoM}}, apperr.FailedPrecondition)
		if n := b.Count(`SELECT count(*) FROM course_member WHERE actor_id = $1`, someone); n != 0 {
			t.Fatal("a refused member.add left a member behind")
		}
	})

	t.Run("an assignment's documents are documents of its course, of the right kind", func(t *testing.T) {
		foreignDoc, material := uuid.New(), uuid.New()
		b.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'rubric', 'theirs')`, foreignDoc, cs205.CourseID)
		b.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'material', 'a lecture')`, material, b.course)
		b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": foreignDoc}, apperr.FailedPrecondition)
		b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": material}, apperr.FailedPrecondition)
		b.try(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "x", "points_possible": 1, "component_id": cs205.RootComponentID}, apperr.FailedPrecondition)
		b.try(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "x", "points_possible": 1, "component_id": b.midterm}, apperr.FailedPrecondition)
	})

	t.Run("the submitting member is a student", func(t *testing.T) {
		b.try(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3}, apperr.FailedPrecondition)                                 // the instructor, for himself
		b.try(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.graderM}, apperr.FailedPrecondition) // for the agent
		b.try(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": foreignStudent}, apperr.NotFound)
		// A student may not hand in work as another student.
		if out := b.MustCall(b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.kenM}, "as-ken"); out.Status != domain.StatusDenied {
			t.Fatalf("Yuki submitting as Ken: %+v", out)
		}
	})
}

func TestComponentTreeStaysATree(t *testing.T) {
	b := build(t)
	mk := func(parent uuid.UUID, name string) uuid.UUID {
		return testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": parent, "name": name})).ID
	}
	a := mk(b.total, "A")
	bb := mk(a, "B")
	c := mk(bb, "C")

	move := func(id, under uuid.UUID) m {
		return m{"course_id": b.course, "component_id": id, "new_parent_id": under}
	}
	// The database blocks only A under A. The longer cycles are ours.
	b.try(t, b.sato, "component.move", move(a, a), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(a, bb), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(a, c), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(b.total, a), apperr.FailedPrecondition) // the root stays the root
	b.do(t, b.sato, "component.move", move(c, a))                                   // a legal move still works

	// A parent is rolled up: it has no points and holds no assignments.
	b.try(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.midterm, "name": "Part 1"}, apperr.FailedPrecondition)
	b.try(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.bucket, "name": "Essays"}, apperr.FailedPrecondition)
	b.try(t, b.sato, "component.update", m{"course_id": b.course, "component_id": a, "points_possible": 50}, apperr.FailedPrecondition)
	b.try(t, b.sato, "component.update", m{"course_id": b.course, "component_id": b.total, "points_possible": 100}, apperr.FailedPrecondition)

	tree := testkit.Result[tools.ComponentTreeOut](t, b.do(t, b.yuki, "component.tree", m{"course_id": b.course}))
	if len(tree.Components) != 6 || tree.Components[0].ID != b.total {
		t.Fatalf("tree: %+v", tree.Components)
	}
	seen := map[uuid.UUID]bool{}
	for _, comp := range tree.Components { // parents come before their children
		if comp.ParentID != nil && !seen[*comp.ParentID] {
			t.Fatalf("%s is listed before its parent", comp.Name)
		}
		seen[comp.ID] = true
	}
}

func TestRemovingAMemberCancelsTheirProposals(t *testing.T) {
	b := build(t)
	yukiWork, kenWork := b.submit(t, b.yuki, "essay"), b.submit(t, b.ken, "essay")
	p1 := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 85}, "p1")
	p2 := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "p2")

	// Pausing leaves the queue alone: the same membership will carry on.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.graderM})
	if n := b.Count(`SELECT count(*) FROM action WHERE status = 'proposed'`); n != 2 {
		t.Fatalf("%d proposals after a pause, want 2 still queued", n)
	}
	if out := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 1}, "while-paused"); out.Status != domain.StatusDenied {
		t.Fatalf("a paused member calling: %+v", out)
	}
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": b.graderM})

	removed := testkit.Result[tools.MemberRemoveOut](t, b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.graderM}))
	if removed.CancelledProposals != 2 {
		t.Fatalf("cancelled %d proposals, want 2", removed.CancelledProposals)
	}
	for _, p := range []pipeline.Outcome{p1, p2} {
		if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled'
			AND result->'error'->'details'->>'reason' = 'member_removed'`, *p.ActionID); n != 1 {
			t.Fatal("a proposal was not cancelled with its reason")
		}
	}
	if queue := testkit.Result[tools.ActionListOut](t, b.do(t, b.sato, "action.list_proposed", m{"course_id": b.course})); len(queue.Actions) != 0 {
		t.Fatalf("the approval queue still holds %d proposals nobody could approve", len(queue.Actions))
	}
	// History is kept; the seat is gone; seating again is a fresh start.
	if n := b.Count(`SELECT count(*) FROM action WHERE member_id = $1`, b.graderM); n < 2 {
		t.Fatal("the removed member's history was lost")
	}
	again := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.grader, "preset": "grader", "listed_assignments": []uuid.UUID{b.hw3}})).MemberID
	if again == b.graderM {
		t.Fatal("re-adding reused the old membership id")
	}
	b.try(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.graderM}, apperr.Conflict)
	b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.grader, "preset": "grader"}, apperr.Conflict) // one live seat per actor
}

// ---------------------------------------------------------------------------
// Platform
// ---------------------------------------------------------------------------

func TestPlatformRules(t *testing.T) {
	b := build(t)
	admin := "admin"

	// Platform tools check platform_role and nothing else. An instructor is
	// nobody here, and the refusal is on record with no course attached.
	out := b.MustCall(b.sato, "actor.register", m{"kind": "agent", "display_name": "mine"}, "sato-registers")
	if out.Status != domain.StatusDenied {
		t.Fatalf("an instructor registering an actor: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND course_id IS NULL AND member_id IS NULL AND status = 'denied'`, *out.ActionID); n != 1 {
		t.Fatal("the platform denial is not recorded as a platform action")
	}
	// And the other way round: an admin is nobody inside a course.
	if out := b.MustCall(b.admin, "member.list", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("an admin with no seat listing members: %+v", out)
	}

	b.try(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Another admin", "platform_role": admin}, apperr.Forbidden) // only root
	b.try(t, b.Root, "actor.register", m{"kind": "human", "display_name": "x", "platform_role": "root"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.register", m{"kind": "system", "display_name": "x"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.suspend", m{"actor_id": b.admin}, apperr.Forbidden) // not yourself
	b.try(t, b.admin, "actor.suspend", m{"actor_id": b.Root}, apperr.Forbidden)  // not root, if you are not root

	email := "taken@example.edu"
	b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "First", "email": email})
	b.try(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Second", "email": "TAKEN@example.edu"}, apperr.Conflict)

	// Suspension is everywhere at once, and lifts cleanly.
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
	if out := b.MustCall(b.sato, "member.list", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "actor_not_active" {
		t.Fatalf("a suspended instructor: %+v", out)
	}
	if out := b.MustCall(b.sato, "me.get", m{}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a suspended actor reading its own account: %+v", out)
	}
	b.try(t, b.admin, "actor.suspend", m{"actor_id": b.sato}, apperr.Conflict)
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": b.sato})
	b.do(t, b.sato, "member.list", m{"course_id": b.course})

	// An agent's first token comes from an admin, once, and works.
	issued := b.do(t, b.admin, "actor.issue_token", m{"actor_id": b.grader, "label": "grader-v2 production"})
	if tok := testkit.Result[tools.IssueTokenOut](t, issued); len(tok.Token) < 40 {
		t.Fatalf("token: %+v", tok)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND (result ? 'token' OR result::text LIKE '%ais_%')`, *issued.ActionID); n != 0 {
		t.Fatal("the issued token is in the action log")
	}

	// Courses.
	term, dept := b.term, b.dept
	b.try(t, b.admin, "course.create", m{"dept_id": dept, "term_id": term, "code": "CS101", "section": "A", "title": "Again"}, apperr.Conflict)
	b.try(t, b.admin, "course.create", m{"dept_id": uuid.New(), "term_id": term, "code": "X", "title": "X"}, apperr.NotFound)
	b.try(t, b.admin, "term.create", m{"name": "Backwards", "starts_on": "2026-12-01", "ends_on": "2026-01-01"}, apperr.InvalidArgument)

	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if out := b.MustCall(b.sato, "assignment.create", m{"course_id": b.course, "title": "late idea", "points_possible": 1}, "archived"); out.Error.Details["reason"] != "course_archived" {
		t.Fatalf("writing to an archived course: %+v", out)
	}
	b.do(t, b.sato, "assignment.list", m{"course_id": b.course}) // still readable
	b.try(t, b.admin, "course.update", m{"course_id": b.course, "title": "New title"}, apperr.FailedPrecondition)
	b.do(t, b.admin, "course.activate", m{"course_id": b.course})

	// Department presets sit beside the built-ins, and win by name.
	body := m{"dept_id": dept, "name": "grader", "role": "assistant", "student_scope": "all", "assignment_scope": "listed",
		"perms": m{"document_read": "autonomous", "submission_read": "autonomous", "grade_submit": "pending_review"}}
	presetID := testkit.Result[tools.IDOut](t, b.do(t, b.admin, "preset.create", body)).ID
	b.try(t, b.admin, "preset.create", body, apperr.Conflict)
	agent := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "grader-v3"})).ActorID
	seated := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": agent, "preset": "grader", "listed_assignments": []uuid.UUID{b.hw3}})).MemberID
	got := testkit.Result[tools.MemberView](t, b.do(t, b.sato, "member.get", m{"course_id": b.course, "member_id": seated}))
	if got.Perms["grade_submit"] != "pending_review" || got.PresetID == nil || *got.PresetID != presetID || len(got.ListedAssignments) != 1 {
		t.Fatalf("the department's preset was not the one applied: %+v", got)
	}
	// Editing the preset changes nobody already seated.
	body["perms"] = m{"document_read": "autonomous"}
	delete(body, "dept_id")
	delete(body, "name")
	body["preset_id"] = presetID
	b.do(t, b.admin, "preset.update", body)
	after := testkit.Result[tools.MemberView](t, b.do(t, b.sato, "member.get", m{"course_id": b.course, "member_id": seated}))
	if after.Perms["grade_submit"] != "pending_review" {
		t.Fatal("editing a preset changed a member who was already seated")
	}
	var builtin uuid.UUID
	for _, p := range testkit.Result[tools.PresetListOut](t, b.do(t, b.sato, "preset.list", m{})).Presets {
		if p.Name == "instructor" {
			builtin = p.ID
		}
	}
	body["preset_id"] = builtin
	b.try(t, b.admin, "preset.update", body, apperr.FailedPrecondition)
}

// ---------------------------------------------------------------------------
// Submissions
// ---------------------------------------------------------------------------

func TestSubmissionLifecycle(t *testing.T) {
	b := build(t)
	now := time.Now()
	due := now.Add(24 * time.Hour)
	// Instructions, with a published version to pin.
	doc, v1, v2 := uuid.New(), uuid.New(), uuid.New()
	b.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'instructions', 'HW3')`, doc, b.course)
	b.Exec(`INSERT INTO document_version (id, document_id, seq, body_md, author_member_id) VALUES ($1, $2, 1, '1000 words', $3)`, v1, doc, b.satoM)
	b.Exec(`UPDATE document SET published_version_id = $1 WHERE id = $2`, v1, doc)
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": doc, "due_at": due})

	created := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3}))
	if created.Attempt != 1 {
		t.Fatalf("first attempt is %d", created.Attempt)
	}
	sub := m{"course_id": b.course, "submission_id": created.SubmissionID}
	b.try(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3}, apperr.Conflict) // one open draft
	b.try(t, b.yuki, "submission.submit", sub, apperr.FailedPrecondition)                                    // nothing to hand in
	b.do(t, b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": created.SubmissionID, "body": "draft one"})
	b.do(t, b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": created.SubmissionID, "body": "my essay"})

	if got := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.yuki, "submission.submit", sub)); got.State != "submitted" {
		t.Fatalf("on time: %+v", got)
	}
	// Frozen. By us, and by the database underneath us.
	b.try(t, b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": created.SubmissionID, "body": "second thoughts"}, apperr.Conflict)
	b.try(t, b.yuki, "submission.submit", sub, apperr.Conflict)
	if _, err := b.Pool.Exec(t.Context(), `UPDATE submission SET body = 'tampered' WHERE id = $1`, created.SubmissionID); err == nil {
		t.Fatal("the database let a submitted row change")
	}
	// What the student was told is pinned, and stays pinned when the
	// instructions move on.
	b.Exec(`INSERT INTO document_version (id, document_id, seq, body_md, author_member_id) VALUES ($1, $2, 2, '2000 words', $3)`, v2, doc, b.satoM)
	b.Exec(`UPDATE document SET published_version_id = $1 WHERE id = $2`, v2, doc)
	got := testkit.Result[tools.SubmissionView](t, b.do(t, b.yuki, "submission.get", sub))
	if got.InstructionsVersionID == nil || *got.InstructionsVersionID != v1 || got.Body == nil || *got.Body != "my essay" {
		t.Fatalf("submitted row: %+v", got)
	}

	// Lateness is for graders to correct, not for the student.
	late := m{"course_id": b.course, "submission_id": created.SubmissionID, "state": "late"}
	if out := b.MustCall(b.yuki, "submission.set_lateness", late, "self-serve"); out.Status != domain.StatusDenied {
		t.Fatalf("a student changing their own lateness: %+v", out)
	}
	b.do(t, b.sato, "submission.set_lateness", late)
	b.try(t, b.sato, "submission.set_lateness", late, apperr.Conflict) // already late
	late["state"] = "draft"
	b.try(t, b.sato, "submission.set_lateness", late, apperr.InvalidArgument)

	// Resubmitting is a new attempt; after the due date it is late, and it
	// pins the instructions as they stand now.
	b.P.SetClock(func() time.Time { return due.Add(time.Hour) })
	second := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "revised"}))
	if second.Attempt != 2 {
		t.Fatalf("second attempt is %d", second.Attempt)
	}
	if got := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": second.SubmissionID})); got.State != "late" {
		t.Fatalf("after the due date: %+v", got)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE id = $1 AND instructions_version_id = $2`, second.SubmissionID, v2); n != 1 {
		t.Fatal("the second attempt did not pin the instructions in force when it was submitted")
	}

	// A 'missing' placeholder is taken over by late work, not sat beside.
	placeholder := uuid.New()
	b.Exec(`INSERT INTO submission (id, assignment_id, course_id, student_member_id, state) VALUES ($1, $2, $3, $4, 'missing')`,
		placeholder, b.hw3, b.course, b.kenM)
	reused := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "sorry"}))
	if reused.SubmissionID != placeholder || reused.Attempt != 1 {
		t.Fatalf("late work did not take over the missing row: %+v", reused)
	}
}

func TestAStudentWithAnEmptiedScopeReachesNobody(t *testing.T) {
	b := build(t)
	b.Exec(`DELETE FROM member_student_scope WHERE member_id = $1`, b.yukiM)
	b.try(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "x"}, apperr.Forbidden)
	if out := b.MustCall(b.yuki, "gradebook.get", m{"course_id": b.course, "student_member_id": b.yukiM}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("scope did not fail closed: %+v", out)
	}
}
