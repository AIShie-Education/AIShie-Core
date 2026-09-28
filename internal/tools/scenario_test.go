package tools_test

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// built is a course made entirely through the tools, from an empty platform:
// not one row of it was inserted by a fixture. It is docs/schema.md §5 again,
// and also the delegation chain from §2.1 — root makes an admin, the admin
// makes the course and seats its instructor, the instructor adds everyone else.
type built struct {
	*testkit.Platform
	admin, sato, grader, tutor uuid.UUID
	yuki, ken                  uuid.UUID
	course, term, dept         uuid.UUID
	total, bucket, midterm     uuid.UUID
	hw3                        uuid.UUID
	satoM, graderM, tutorM     uuid.UUID
	yukiM, kenM                uuid.UUID
	key                        int
}

// do calls a tool and insists it executed.
func (b *built) do(t *testing.T, actor uuid.UUID, name string, args m) pipeline.Outcome {
	t.Helper()
	b.key++
	out := b.MustCall(actor, name, args, "k"+string(rune('a'+b.key%26))+uuid.NewString())
	if out.Status != domain.StatusExecuted {
		t.Fatalf("%s: %+v", name, out)
	}
	return out
}

func build(t *testing.T) *built {
	t.Helper()
	return buildOn(t, testkit.NewPlatform(t))
}

// buildOn is build on a platform the test has set up itself.
func buildOn(t *testing.T, p *testkit.Platform) *built {
	t.Helper()
	b := &built{Platform: p}
	root := b.Root

	// Root makes an admin; the admin does the rest of the platform's work.
	admin := "admin"
	b.admin = testkit.Result[tools.ActorOut](t, b.do(t, root, "actor.register",
		m{"kind": "human", "display_name": "Admin", "platform_role": admin})).ActorID
	register := func(kind, name string) uuid.UUID {
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": kind, "display_name": name})).ActorID
	}
	b.sato, b.yuki, b.ken = register("human", "Sato"), register("human", "Yuki"), register("human", "Ken")
	b.grader, b.tutor = register("agent", "grader-v2"), register("agent", "tutor")

	b.term = testkit.Result[tools.IDOut](t, b.do(t, b.admin, "term.create", m{"name": "2026 Autumn", "starts_on": "2026-09-01", "ends_on": "2026-12-20"})).ID
	b.dept = testkit.Result[tools.IDOut](t, b.do(t, b.admin, "department.create", m{"name": "Computer Science"})).ID
	made := testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create",
		m{"dept_id": b.dept, "term_id": b.term, "code": "CS101", "section": "A", "title": "Introduction to Computing"}))
	b.course, b.total = made.CourseID, made.RootComponentID
	b.do(t, b.admin, "course.activate", m{"course_id": b.course})
	b.satoM = testkit.Result[tools.MemberIDOut](t, b.do(t, b.admin, "course.seat_instructor", m{"course_id": b.course, "actor_id": b.sato})).MemberID

	// From here on it is Sato's course.
	b.bucket = testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": b.total, "name": "Assignments", "weight": 40})).ID
	b.midterm = testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": b.total, "name": "Midterm", "weight": 30, "points_possible": 100})).ID
	b.hw3 = testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create",
		m{"course_id": b.course, "title": "HW3", "points_possible": 100, "component_id": b.bucket})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": b.hw3})

	add := func(actor uuid.UUID, args m) uuid.UUID {
		args["course_id"], args["actor_id"] = b.course, actor
		return testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", args)).MemberID
	}
	b.yukiM = add(b.yuki, m{"preset": "student"})
	b.kenM = add(b.ken, m{"preset": "student"})
	b.graderM = add(b.grader, m{"preset": "grader", "listed_assignments": []uuid.UUID{b.hw3}})
	b.tutorM = add(b.tutor, m{"preset": "tutor", "listed_students": []uuid.UUID{b.yukiM}})
	return b
}

// submit hands in HW3 for a student, as that student.
func (b *built) submit(t *testing.T, student uuid.UUID, body string) uuid.UUID {
	t.Helper()
	id := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, student, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": body})).SubmissionID
	b.do(t, student, "submission.submit", m{"course_id": b.course, "submission_id": id})
	return id
}

func TestWorkedExampleBuiltThroughToolsAlone(t *testing.T) {
	b := build(t)
	yukiWork, kenWork := b.submit(t, b.yuki, "Yuki's essay"), b.submit(t, b.ken, "Ken's essay")

	// The agent reads what it is about to grade, then proposes.
	read := b.do(t, b.grader, "submission.get", m{"course_id": b.course, "submission_id": yukiWork})
	if got := testkit.Result[tools.SubmissionView](t, read); got.Body == nil || *got.Body != "Yuki's essay" {
		t.Fatalf("the grader read %+v", got)
	}
	proposed := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 85}, "p1")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("grader's grade: %+v", proposed)
	}
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70})
	posted := testkit.Result[tools.GradePostOut](t, b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3}))
	if len(posted.Posted) != 2 || posted.Snapshots != 4 {
		t.Fatalf("posted %+v", posted)
	}

	// Yuki sees her grade and her total; and only hers.
	mine := testkit.Result[tools.GradeListOut](t, b.do(t, b.yuki, "grade.list", m{"course_id": b.course}))
	if len(mine.Grades) != 3 { // HW3, and the two totals written down at posting
		t.Fatalf("Yuki sees %d grades, want her HW3 grade and two totals: %+v", len(mine.Grades), mine.Grades)
	}
	for _, g := range mine.Grades {
		if g.StudentMemberID != b.yukiM || g.State != "posted" {
			t.Fatalf("Yuki sees a grade that is not hers, or not posted: %+v", g)
		}
	}

	// Yuki asks the tutor listed for her about her work, and it answers; she
	// withdraws what she asked and closes the conversation. News of it is the
	// two participants' alone.
	conv, question := b.open(t, b.yuki, b.tutorM, "Why was my essay marked down?")
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, question, "Section 2 asserts more than it shows."))
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": question})
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
	for who, want := range map[uuid.UUID]int{b.yuki: 5, b.tutor: 5, b.ken: 0, b.sato: 0} {
		n := 0
		for _, e := range feed(t, b, who) {
			if e.SubjectType == "conversation" {
				n++
			}
		}
		if n != want {
			t.Fatalf("%d events of the conversation in the feed of %s, want %d", n, who, want)
		}
	}

	// Every action in the course traces back to someone seated by someone:
	// the delegation chain has no gaps.
	if n := b.Count(`SELECT count(*) FROM actor WHERE created_by_actor_id IS NULL`); n != 1 {
		t.Fatalf("%d actors were created by nobody; only root may be", n)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE status NOT IN ('executed', 'proposed')`); n != 0 {
		t.Fatalf("%d actions in a clean run did not execute", n)
	}
	// Every event type the tools emitted has a visibility rule. A type with
	// none would be visible to nobody but its author, which is safe, but it
	// should be a decision and not an oversight.
	rows, err := b.Pool.Query(context.Background(), `SELECT DISTINCT type FROM event ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	known := map[string]bool{}
	for _, k := range tools.KnownEventTypes() {
		known[k] = true
	}
	var emitted []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		emitted = append(emitted, typ)
		if !known[typ] {
			t.Errorf("event type %q is emitted but has no visibility rule", typ)
		}
	}
	sort.Strings(emitted)
	for _, typ := range []string{"conversation.opened", "conversation.message_posted", "conversation.message_retracted", "conversation.closed"} {
		if !slices.Contains(emitted, typ) {
			t.Errorf("a full run emitted no %s", typ)
		}
	}
	if len(emitted) < 10 {
		t.Fatalf("only %d event types in a full run: %v", len(emitted), emitted)
	}
}
