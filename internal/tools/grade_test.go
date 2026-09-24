package tools_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

type m = map[string]any

func grade(t *testing.T, c *testkit.CS101, args m, key string) uuid.UUID {
	t.Helper()
	out := c.MustCall(c.Sato, "grade.submit", args, key)
	if out.Status != domain.StatusExecuted {
		t.Fatalf("grade.submit: %+v", out)
	}
	return testkit.Result[tools.GradeSubmitOut](t, out).GradeID
}

func post(t *testing.T, c *testkit.CS101, key string, ids ...uuid.UUID) tools.GradePostOut {
	t.Helper()
	out := c.MustCall(c.Sato, "grade.post", m{"course_id": c.Course, "grade_ids": ids}, key)
	if out.Status != domain.StatusExecuted {
		t.Fatalf("grade.post: %+v", out)
	}
	return testkit.Result[tools.GradePostOut](t, out)
}

// liveSnapshot is the student's current computed score for a component.
func liveSnapshot(t *testing.T, c *testkit.CS101, component, student uuid.UUID) decimal.Decimal {
	t.Helper()
	var s decimal.Decimal
	err := c.Pool.QueryRow(context.Background(), `SELECT score FROM grade WHERE origin = 'computed'
		AND component_id = $1 AND student_member_id = $2 AND posted_at IS NOT NULL AND superseded_by IS NULL`,
		component, student).Scan(&s)
	if err != nil {
		t.Fatalf("live snapshot: %v", err)
	}
	return s
}

func wantScore(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

func gradebook(t *testing.T, c *testkit.CS101, actor, student uuid.UUID) map[uuid.UUID]tools.GradebookLine {
	t.Helper()
	out := c.MustCall(actor, "gradebook.get", m{"course_id": c.Course, "student_member_id": student}, "")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("gradebook.get: %+v", out)
	}
	lines := map[uuid.UUID]tools.GradebookLine{}
	for _, l := range testkit.Result[tools.GradebookGetOut](t, out).Components {
		lines[l.ComponentID] = l
	}
	return lines
}

func TestANewDraftReplacesTheOldOne(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	first := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 70}, "1")
	second := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 75}, "2")

	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND superseded_by = $2`, first, second); n != 1 {
		t.Fatal("the first draft was not superseded by the second")
	}
	// Only the live draft is posted, and the replaced one cannot be.
	if out := c.MustCall(c.Sato, "grade.post", m{"course_id": c.Course, "grade_ids": []uuid.UUID{first}}, "p1"); out.Status != domain.StatusFailed {
		t.Fatalf("posting a replaced draft: %+v", out)
	}
	if got := post(t, c, "p2", second); len(got.Posted) != 1 {
		t.Fatalf("posted %v", got.Posted)
	}
}

func TestAGradeIsPostedOnce(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	g := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 70}, "1")
	post(t, c, "p1", g)

	again := c.MustCall(c.Sato, "grade.post", m{"course_id": c.Course, "grade_ids": []uuid.UUID{g}}, "p2")
	if again.Status != domain.StatusFailed || again.Error.Code != apperr.Conflict {
		t.Fatalf("posting twice: %+v", again)
	}
	// A second draft for work that already has a posted grade cannot be
	// posted over it; that is what regrade is for.
	g2 := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 90}, "2")
	over := c.MustCall(c.Sato, "grade.post", m{"course_id": c.Course, "grade_ids": []uuid.UUID{g2}}, "p3")
	if over.Status != domain.StatusFailed || over.Error.Code != apperr.Conflict {
		t.Fatalf("posting over a posted grade: %+v", over)
	}
	if empty := c.MustCall(c.Sato, "grade.post", m{"course_id": c.Course, "assignment_id": c.HW4}, "p4"); empty.Status != domain.StatusFailed {
		t.Fatalf("posting an assignment with no drafts: %+v", empty)
	}
	if _, err := c.Call(c.Sato, "grade.post", m{"course_id": c.Course}, "p5"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("neither grade_ids nor assignment_id: %v", err)
	}
	if _, err := c.Call(c.Sato, "grade.post", m{"course_id": c.Course, "grade_ids": []uuid.UUID{uuid.New()}}, "p6"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("unknown grade id: %v", err)
	}
}

// "Regrading appends. The number a student was shown must not drift when a
// lower grade changes later; updating it is a regrade, with history."
func TestRegradeKeepsHistory(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	old := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 70}, "1")
	post(t, c, "p", old)
	wantScore(t, "total before", liveSnapshot(t, c, c.Total, yuki.Member), "70")

	out := c.MustCall(c.Sato, "grade.regrade", m{"course_id": c.Course, "grade_id": old, "score": 90, "feedback": "Section 3 was marked too harshly."}, "r")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("regrade: %+v", out)
	}
	re := testkit.Result[tools.GradeRegradeOut](t, out)
	if re.Replaces != old || re.Snapshots != 2 {
		t.Fatalf("regrade result: %+v", re)
	}
	// The old grade is kept, superseded; the new one is posted at once and
	// names the regrade as the action that made it.
	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND superseded_by = $2 AND score = 70 AND posted_at IS NOT NULL`, old, re.GradeID); n != 1 {
		t.Fatal("the old grade was not kept as superseded")
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND score = 90 AND posted_at IS NOT NULL AND superseded_by IS NULL
		AND created_by_action_id = $2 AND rubric_version_id = $3`, re.GradeID, *out.ActionID, c.RubricVersion); n != 1 {
		t.Fatal("the new grade is not live, posted and attributed to the regrade")
	}
	// Snapshots move the same way: a new row, the old one superseded.
	wantScore(t, "bucket after", liveSnapshot(t, c, c.Assignments, yuki.Member), "90")
	wantScore(t, "total after", liveSnapshot(t, c, c.Total, yuki.Member), "90")
	if n := c.Count(`SELECT count(*) FROM grade WHERE origin = 'computed' AND student_member_id = $1`, yuki.Member); n != 4 {
		t.Fatalf("%d snapshot rows, want 4: two components, each with its history", n)
	}
	if n := c.Count(`SELECT count(*) FROM event WHERE type = 'grade.regraded' AND subject_id = $1 AND payload->>'replaces' = $2`, re.GradeID, old.String()); n != 1 {
		t.Fatal("no grade.regraded event")
	}
	// Regrading to the same total writes no new snapshot.
	same := c.MustCall(c.Sato, "grade.regrade", m{"course_id": c.Course, "grade_id": re.GradeID, "score": 90}, "r2")
	if got := testkit.Result[tools.GradeRegradeOut](t, same); got.Snapshots != 0 {
		t.Fatalf("an unchanged total was written down again: %+v", got)
	}
	// What cannot be regraded.
	stale := c.MustCall(c.Sato, "grade.regrade", m{"course_id": c.Course, "grade_id": old, "score": 50}, "r3")
	if stale.Status != domain.StatusFailed || stale.Error.Code != apperr.Conflict {
		t.Fatalf("regrading a superseded grade: %+v", stale)
	}
	var snap uuid.UUID
	if err := c.Pool.QueryRow(context.Background(), `SELECT id FROM grade WHERE origin = 'computed' LIMIT 1`).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	if out := c.MustCall(c.Sato, "grade.regrade", m{"course_id": c.Course, "grade_id": snap, "score": 50}, "r4"); out.Status != domain.StatusFailed {
		t.Fatalf("regrading a computed total: %+v", out)
	}
}

// Regrade takes both permissions and runs at the lower of their levels.
func TestRegradeRunsAtTheLowerLevel(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	g := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 70}, "1")
	post(t, c, "p", g)

	ta := c.Actor("human", "TA")
	c.Member(c.Course, ta, "ta") // grade_submit autonomous, grade_post denied
	if out := c.MustCall(ta, "grade.regrade", m{"course_id": c.Course, "grade_id": g, "score": 80}, "ta1"); out.Status != domain.StatusDenied {
		t.Fatalf("TA who cannot post, regrading: %+v", out)
	}
	c.Exec(`UPDATE course_member SET perm_grade_post = 'confirm_required' WHERE actor_id = $1`, ta)
	out := c.MustCall(ta, "grade.regrade", m{"course_id": c.Course, "grade_id": g, "score": 80}, "ta2")
	if out.Status != domain.StatusProposed {
		t.Fatalf("autonomous submit + confirm_required post: %+v, want proposed", out)
	}
	decided := c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": out.ActionID, "decision": "approve"}, "d")
	if v := testkit.Result[pipeline.DecideOut](t, decided); v.Outcome != domain.StatusExecuted {
		t.Fatalf("approved regrade: %+v", v)
	}
	wantScore(t, "total", liveSnapshot(t, c, c.Total, yuki.Member), "80")
}

func TestComponentGradesAndTotals(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	yuki, ken := c.Students[0], c.Students[1]

	hw3 := grade(t, c, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 85}, "hw3")
	mid := grade(t, c, m{"course_id": c.Course, "component_id": c.Midterm, "student_member_id": yuki.Member, "score": 70}, "mid")
	got := post(t, c, "p", hw3, mid)
	// Assignments bucket, and the total: the midterm is entered, not computed.
	if got.Snapshots != 2 {
		t.Fatalf("snapshots = %d, want 2", got.Snapshots)
	}
	wantScore(t, "bucket", liveSnapshot(t, c, c.Assignments, yuki.Member), "85")
	// (40 × 0.85 + 30 × 0.70) / 70
	wantScore(t, "total", liveSnapshot(t, c, c.Total, yuki.Member), "78.57")

	// The read side agrees, computes live, and writes nothing.
	before := c.Count(`SELECT count(*) FROM grade`)
	book := gradebook(t, c, yuki.Actor, yuki.Member)
	wantScore(t, "gradebook total", *book[c.Total].Percent, "78.57")
	wantScore(t, "gradebook midterm", *book[c.Midterm].Percent, "70")
	if book[c.Total].Complete || !book[c.Midterm].Complete {
		t.Fatalf("completeness: total %v (HW4 is ungraded), midterm %v", book[c.Total].Complete, book[c.Midterm].Complete)
	}
	if ken := gradebook(t, c, c.Sato, ken.Member); ken[c.Total].Percent != nil {
		t.Fatalf("a student with no posted grades has a total: %v", ken[c.Total].Percent)
	}
	if after := c.Count(`SELECT count(*) FROM grade`); after != before {
		t.Fatal("reading the gradebook wrote something")
	}
	// Drafts do not count.
	grade(t, c, m{"course_id": c.Course, "submission_id": ken.HW3, "score": 100}, "ken-draft")
	if ken := gradebook(t, c, c.Sato, ken.Member); ken[c.Total].Percent != nil {
		t.Fatal("an unposted draft shows in the gradebook")
	}
	// treat_ungraded_as_zero: HW4 becomes a zero. Bucket (85+0)/200, total (40×0.425 + 30×0.7)/70.
	out := c.MustCall(c.Sato, "gradebook.get", m{"course_id": c.Course, "student_member_id": yuki.Member, "treat_ungraded_as_zero": true}, "")
	for _, l := range testkit.Result[tools.GradebookGetOut](t, out).Components {
		if l.ComponentID == c.Total {
			wantScore(t, "final total", *l.Percent, "54.29")
		}
	}
}

func TestWhatMayBeGraded(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	cases := []struct {
		name string
		args m
		code apperr.Code
	}{
		{"a rolled-up component", m{"component_id": c.Assignments, "student_member_id": yuki.Member, "score": 1}, apperr.FailedPrecondition},
		{"the course total", m{"component_id": c.Total, "student_member_id": yuki.Member, "score": 1}, apperr.FailedPrecondition},
		{"someone who is not a student", m{"component_id": c.Midterm, "student_member_id": c.SatoM, "score": 1}, apperr.FailedPrecondition},
		{"a negative score", m{"submission_id": yuki.HW3, "score": -1}, apperr.InvalidArgument},
		{"a rubric version from nowhere", m{"submission_id": yuki.HW3, "score": 1, "rubric_version_id": uuid.New()}, apperr.FailedPrecondition},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["course_id"] = c.Course
			out := c.MustCall(c.Sato, "grade.submit", tc.args, string(rune('a'+i)))
			if out.Status != domain.StatusFailed || out.Error.Code != tc.code {
				t.Fatalf("%+v, want failed with %s", out, tc.code)
			}
		})
	}
	if _, err := c.Call(c.Sato, "grade.submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "student_member_id": yuki.Member, "score": 1}, "z"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("student_member_id with submission_id: %v", err)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatalf("%d grades written by calls that should all have failed", n)
	}

	// A draft submission is not graded.
	c.Exec(`INSERT INTO submission (id, assignment_id, course_id, student_member_id, state) VALUES ($1, $2, $3, $4, 'draft')`,
		uuid.New(), c.HW4, c.Course, yuki.Member)
	var draft uuid.UUID
	if err := c.Pool.QueryRow(context.Background(), `SELECT id FROM submission WHERE state = 'draft'`).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	if out := c.MustCall(c.Sato, "grade.submit", m{"course_id": c.Course, "submission_id": draft, "score": 1}, "draft"); out.Status != domain.StatusFailed {
		t.Fatalf("grading a draft: %+v", out)
	}
}

// A grader listed for HW3 may not touch a component grade: it belongs to no
// single assignment, and "no assignment" must not mean "no check".
func TestListedGraderCannotGradeAComponent(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	out := c.MustCall(c.Grader, "grade.submit",
		m{"course_id": c.Course, "component_id": c.Midterm, "student_member_id": c.Students[0].Member, "score": 100}, "k")
	if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "assignment_out_of_scope" {
		t.Fatalf("%+v", out)
	}
}

// A submission from another course is not found here, whatever the caller's
// standing in either course.
func TestIdsFromAnotherCourseAreNotFound(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	other := c.World.Course("CS205")
	c.Member(other, c.Sato, "instructor")
	_, err := c.Call(c.Sato, "grade.submit", m{"course_id": other, "submission_id": c.Students[0].HW3, "score": 1}, "k")
	if !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("err = %v, want not_found", err)
	}
}

// A score is a score in the scheme it was given under. Once a grade has been
// entered beneath a component, its place in the scheme is fixed: moving it
// would change what every one of those grades counts toward. And a parent's
// totals, once written down, keep it from ever being graded directly.
func TestAGradedComponentKeepsItsPlace(t *testing.T) {
	b := build(t)
	exams := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.total, "name": "Exams", "weight": 30})).ID
	final := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": exams, "name": "Final", "points_possible": 100})).ID
	quiz := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": exams, "name": "Quiz", "points_possible": 10})).ID
	move := func(id, under uuid.UUID) m {
		return m{"course_id": b.course, "component_id": id, "new_parent_id": under}
	}
	// Nothing entered yet: the scheme is still being arranged.
	b.do(t, b.sato, "component.move", move(quiz, b.total))
	b.do(t, b.sato, "component.move", move(quiz, exams))

	// A draft is enough.
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": final, "student_member_id": b.yukiM, "score": 80})).GradeID
	b.try(t, b.sato, "component.move", move(final, b.total), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(exams, b.bucket), apperr.FailedPrecondition) // graded beneath it
	b.do(t, b.sato, "component.move", move(quiz, b.total))                               // nothing entered for the quiz
	// The same for a bucket once one of its assignments is graded.
	work := b.submit(t, b.yuki, "essay")
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 70})
	b.try(t, b.sato, "component.move", move(b.bucket, exams), apperr.FailedPrecondition)

	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{g}})
	if n := b.Count(`SELECT count(*) FROM grade WHERE component_id = $1 AND origin = 'computed' AND superseded_by IS NULL`, exams); n != 1 {
		t.Fatalf("%d live totals on Exams, want Yuki's", n)
	}
	// A parent whose totals were written down can never become something
	// graded directly, where those totals would sit in the way for ever.
	// The tools no longer let a parent be emptied once graded; a scheme from
	// before that rule still might be.
	legacy := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.total, "name": "Legacy"})).ID
	b.Exec(`INSERT INTO grade (id, student_member_id, component_id, origin, score, grader_member_id, created_by_action_id, posted_at, posted_by_member_id)
		SELECT $1, student_member_id, $2, 'computed', score, grader_member_id, created_by_action_id, posted_at, posted_by_member_id
		FROM grade WHERE component_id = $3 AND origin = 'computed' AND superseded_by IS NULL`, uuid.New(), legacy, exams)
	b.try(t, b.sato, "component.update", m{"course_id": b.course, "component_id": legacy, "name": "Participation", "points_possible": 10}, apperr.FailedPrecondition)
	if n := b.Count(`SELECT count(*) FROM grade_component WHERE id = $1 AND name = 'Legacy' AND points_possible IS NULL`, legacy); n != 1 {
		t.Fatal("the refused change was kept")
	}
	// One that never had totals written down can still be turned.
	spare := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.total, "name": "Spare"})).ID
	b.do(t, b.sato, "component.update", m{"course_id": b.course, "component_id": spare, "points_possible": 5})
}

// "An unpublished assignment is visible only to holders of
// perm_assignment_write." Nobody can submit to one, so it can never carry a
// grade; the gradebook and the totals written down at posting leave it out,
// rather than naming it to every student and counting a zero nobody could
// have avoided.
func TestAnUnpublishedAssignmentStaysOutOfTheGradebook(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	hidden := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create",
		m{"course_id": b.course, "title": "SECRET final project", "points_possible": 300, "component_id": b.bucket})).ID

	book := func() tools.GradebookGetOut {
		return testkit.Result[tools.GradebookGetOut](t, b.do(t, b.yuki, "gradebook.get", m{"course_id": b.course, "student_member_id": b.yukiM}))
	}
	names := func(out tools.GradebookGetOut) string {
		raw, _ := json.Marshal(out)
		return string(raw)
	}
	if strings.Contains(names(book()), hidden.String()) {
		t.Fatal("Yuki's gradebook names an assignment she cannot see")
	}
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{g}, "treat_ungraded_as_zero": true})
	for _, line := range book().Components {
		if line.ComponentID == b.bucket && (line.Percent == nil || !line.Percent.Equal(decimal.NewFromInt(80))) {
			t.Fatalf("Assignments = %v, want 80: the unpublished project was counted", line.Percent)
		}
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE origin = 'computed' AND student_member_id = $1 AND superseded_by IS NULL AND breakdown::text LIKE '%' || $2 || '%'`, b.yukiM, hidden.String()); n != 0 {
		t.Fatal("a posted total's breakdown names the unpublished assignment")
	}
	// Published, it counts from then on.
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hidden})
	if !strings.Contains(names(book()), hidden.String()) {
		t.Fatal("the published project is not in the gradebook")
	}
}

// A score is a score out of the points possible when it was given. Once any
// grade has been entered — a draft waiting to be posted as much as a posted
// one — what the work is worth no longer changes under it: a 95 entered out
// of 100 must not be posted out of 50 with nobody having said so.
func TestPointsAreFixedOnceAGradeIsEntered(t *testing.T) {
	b := build(t)
	// Before anything is entered, an assignment can be rescaled.
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 100})
	work := b.submit(t, b.yuki, "essay")
	draft := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 95})).GradeID
	b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 50}, apperr.FailedPrecondition)
	b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 200}, apperr.FailedPrecondition)
	b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "component_id": b.midterm}, apperr.FailedPrecondition)
	// What is not about the grade still changes.
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "title": "HW3 (revised brief)"})
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{draft}})
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND posted_at IS NOT NULL AND score = 95`, draft); n != 1 {
		t.Fatal("the draft was not posted as entered")
	}
	// A directly graded component is held to the same rule.
	b.do(t, b.sato, "component.update", m{"course_id": b.course, "component_id": b.midterm, "points_possible": 50})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 45})
	b.try(t, b.sato, "component.update", m{"course_id": b.course, "component_id": b.midterm, "points_possible": 100}, apperr.FailedPrecondition)
	b.do(t, b.sato, "component.update", m{"course_id": b.course, "component_id": b.midterm, "name": "Midterm exam"})
}

// The same when the grade and the rescaling come at once. Each checks before
// it writes — the score against the points possible, the points against the
// grades entered — and neither may pass on what the other has not written
// yet, or a 95 stands on an assignment worth 50.
func TestPointsStayFixedWhileAGradeIsEntered(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	done := make(chan pipeline.Outcome, 2)
	// The grade is on its way, its score checked against 100 and waiting to
	// be written (its foreign key to the student's seat, held here), when
	// the assignment is rescaled.
	release := b.hold(t, `SELECT 1 FROM course_member WHERE id = $1 FOR UPDATE`, b.yukiM)
	b.start(t, done, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 95})
	b.blocked(t, 1, done)
	b.start(t, done, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 50})
	b.blocked(t, 2, done)
	release()
	executed := 0
	for range 2 {
		if out := <-done; out.Status == domain.StatusExecuted {
			executed++
		}
	}
	if n := b.Count(`SELECT count(*) FROM grade g JOIN submission s ON s.id = g.submission_id JOIN assignment a ON a.id = s.assignment_id
		WHERE g.superseded_by IS NULL AND g.score > a.points_possible`); n != 0 {
		t.Fatal("a 95 was entered on an assignment worth 50")
	}
	if executed != 1 {
		t.Fatalf("%d of the two went through; whichever came second should have been refused", executed)
	}
}

// Two graders entering a draft for the same work at the same time must leave
// one live draft, not two: nothing could post two.
func TestOneLiveDraftUnderConcurrency(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.graderM, "perms": m{"grade_submit": "autonomous"}})
	for round := range 8 {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, actor := range []uuid.UUID{b.sato, b.grader, b.sato, b.grader} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				args := m{"course_id": b.course, "submission_id": work, "score": 50 + i}
				if out, err := b.Call(actor, "grade.submit", args, "race-"+strconv.Itoa(round)+"-"+strconv.Itoa(i)); err != nil || out.Status != domain.StatusExecuted {
					t.Errorf("grade.submit: %v %+v", err, out)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1 AND posted_at IS NULL AND superseded_by IS NULL`, work); n != 1 {
			t.Fatalf("round %d: %d live drafts", round, n)
		}
	}
	// And posting the assignment then works.
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3})
}

// Two posts touching one student at once — an assignment posted while an
// exam is regraded — both write the student's totals. They take turns rather
// than colliding on the one live total.
func TestTotalsAreWrittenByOneAtATime(t *testing.T) {
	b := build(t)
	mid := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 70})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{mid}})
	for round := range 8 {
		// A fresh attempt each round, so that each round posts a new grade.
		work := b.submit(t, b.yuki, "essay "+strconv.Itoa(round))
		hw := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 60 + round})).GradeID
		start := make(chan struct{})
		var wg sync.WaitGroup
		var regraded uuid.UUID
		for i, c := range []struct {
			name string
			args m
		}{
			{"grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{hw}}},
			{"grade.regrade", m{"course_id": b.course, "grade_id": mid, "score": 71 + round}},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out, err := b.Call(b.sato, c.name, c.args, "turns-"+strconv.Itoa(round)+"-"+strconv.Itoa(i))
				if err != nil || out.Status != domain.StatusExecuted {
					t.Errorf("round %d %s: %v %+v", round, c.name, err, out)
					return
				}
				if c.name == "grade.regrade" {
					regraded = testkit.Result[tools.GradeRegradeOut](t, out).GradeID
				}
			}()
		}
		close(start)
		wg.Wait()
		if t.Failed() {
			t.FailNow()
		}
		mid = regraded
		if n := b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND origin = 'computed' AND component_id = $2 AND superseded_by IS NULL`, b.yukiM, b.total); n != 1 {
			t.Fatalf("round %d: %d live totals", round, n)
		}
	}
	// And the total that stands is the one both changes lead to.
	book := testkit.Result[tools.GradebookGetOut](t, b.do(t, b.sato, "gradebook.get", m{"course_id": b.course, "student_member_id": b.yukiM}))
	for _, l := range book.Components {
		if l.ComponentID == b.total {
			if n := b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND component_id = $2 AND origin = 'computed' AND superseded_by IS NULL AND score = $3`, b.yukiM, b.total, *l.Percent); n != 1 {
				t.Fatalf("the live total is not %s", *l.Percent)
			}
		}
	}
}

// Final is final. Once a student's totals have been written with ungraded
// work counted as zero, a later post or regrade beneath them — made without
// saying so — keeps counting it as zero, rather than quietly turning the
// final grade back into a grade so far.
func TestFinalTotalsStayFinal(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	hw := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80})).GradeID
	book := func(zero bool) *decimal.Decimal {
		out := testkit.Result[tools.GradebookGetOut](t, b.do(t, b.sato, "gradebook.get", m{"course_id": b.course, "student_member_id": b.yukiM, "treat_ungraded_as_zero": zero}))
		for _, l := range out.Components {
			if l.ComponentID == b.total {
				return l.Percent
			}
		}
		return nil
	}
	total := func() (decimal.Decimal, bool) {
		var score decimal.Decimal
		var breakdown []byte
		if err := b.Pool.QueryRow(t.Context(), `SELECT score, breakdown FROM grade WHERE student_member_id = $1 AND component_id = $2 AND origin = 'computed' AND superseded_by IS NULL`, b.yukiM, b.total).Scan(&score, &breakdown); err != nil {
			t.Fatal(err)
		}
		var working struct {
			UngradedAsZero bool `json:"ungraded_as_zero"`
		}
		if err := json.Unmarshal(breakdown, &working); err != nil {
			t.Fatal(err)
		}
		return score, working.UngradedAsZero
	}

	// Posted as final: the midterm, never sat, counts as zero.
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{hw}, "treat_ungraded_as_zero": true})
	if got, final := total(); !final || !got.Equal(*book(true)) {
		t.Fatalf("posted as final: %s (final: %v), want %s", got, final, *book(true))
	}
	// A regrade made without the flag keeps it final.
	b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": hw, "score": 90})
	if got, final := total(); !final || !got.Equal(*book(true)) || got.Equal(*book(false)) {
		t.Fatalf("after a regrade: %s (final: %v), want %s and not the grade so far %s", got, final, *book(true), *book(false))
	}
	// So does a later post beneath it.
	mid := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 50})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{mid}})
	if got, final := total(); !final {
		t.Fatalf("after a post: %s is no longer final", got)
	}
	// Another student, never finalised, still gets a grade so far.
	kens := b.submit(t, b.ken, "essay")
	kg := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{kg}})
	if n := b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND component_id = $2 AND origin = 'computed' AND superseded_by IS NULL AND breakdown::text LIKE '%ungraded_as_zero%'`, b.kenM, b.total); n != 0 {
		t.Fatal("Ken's totals were written as final; nobody said so")
	}
}

// treat_ungraded_as_zero is a decision about the course total: every other
// assignment's ungraded work becomes a zero, and stays one. A course total is
// within scope only for assignment_scope = 'all', so a grader listed for HW3
// may post and regrade HW3, but not make Yuki's totals final by doing so.
func TestFinalisingTotalsTakesTheWholeCourse(t *testing.T) {
	b := build(t)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.graderM, "perms": m{"grade_submit": "autonomous", "grade_post": "autonomous"}})
	work := b.submit(t, b.yuki, "essay")
	hw := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80})).GradeID
	denied := func(what string, out pipeline.Outcome) {
		t.Helper()
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "assignment_out_of_scope" {
			t.Fatalf("%s: %+v", what, out)
		}
	}
	final := func() int {
		return b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND origin = 'computed' AND breakdown::text LIKE '%ungraded_as_zero%'`, b.yukiM)
	}

	denied("posting HW3 as final", b.MustCall(b.grader, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{hw}, "treat_ungraded_as_zero": true}, "final"))
	denied("posting HW3 as final by assignment", b.MustCall(b.grader, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3, "treat_ungraded_as_zero": true}, "final-hw3"))
	if out := b.MustCall(b.grader, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{hw}}, "post"); out.Status != domain.StatusExecuted {
		t.Fatalf("posting HW3: %+v", out)
	}
	denied("regrading HW3 as final", b.MustCall(b.grader, "grade.regrade", m{"course_id": b.course, "grade_id": hw, "score": 90, "treat_ungraded_as_zero": true}, "regrade-final"))
	if out := b.MustCall(b.grader, "grade.regrade", m{"course_id": b.course, "grade_id": hw, "score": 90}, "regrade"); out.Status != domain.StatusExecuted {
		t.Fatalf("regrading HW3: %+v", out)
	}
	if final() != 0 {
		t.Fatal("a grader listed for HW3 made Yuki's totals final")
	}
}

// The rubric version a grade is against is the one the grader was shown. For
// a proposal that is the rubric as published when the proposal was made, not
// when it was approved: it is pinned into the proposal.
func TestAProposalPinsTheRubricItWasMadeAgainst(t *testing.T) {
	b := build(t)
	rubric := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "rubric", "title": "HW3 rubric", "body_md": "v1"}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": rubric.DocumentID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": rubric.DocumentID})
	work := b.submit(t, b.yuki, "essay")

	proposed := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 85}, "p")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("%+v", proposed)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND payload->>'rubric_version_id' = $2`, *proposed.ActionID, rubric.VersionID.String()); n != 1 {
		t.Fatal("the proposal does not carry the rubric version it was made against")
	}
	// The rubric moves on before anyone decides.
	b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": rubric.DocumentID, "body_md": "v2", "publish": true})
	v := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if v.Outcome != domain.StatusExecuted {
		t.Fatalf("%+v", v)
	}
	gradeID := testkit.Result[tools.GradeSubmitOut](t, pipeline.Outcome{Result: v.Result}).GradeID
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND rubric_version_id = $2`, gradeID, *rubric.VersionID); n != 1 {
		t.Fatal("the approved grade is pinned to a rubric version the grader never saw")
	}
	// A direct call is against the rubric as it stands.
	kens := b.submit(t, b.ken, "essay")
	direct := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 70})).GradeID
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND rubric_version_id = $2`, direct, *rubric.VersionID); n != 0 {
		t.Fatal("a direct grade was pinned to the old rubric")
	}
}

// A proposal to post an assignment's drafts is about the drafts that were
// waiting when it was made. One a TA enters while it waits — the TA grades
// but does not post — has been in front of nobody who could release it, and
// an old approval must not release it with the rest.
func TestAPostProposalPostsWhatWasWaiting(t *testing.T) {
	b := build(t)
	register := func(kind, name string) uuid.UUID {
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": kind, "display_name": name})).ActorID
	}
	bot, ta := register("agent", "release-bot"), register("human", "TA")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "ta", "perms": m{"grade_post": "confirm_required"}})
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ta, "preset": "ta"})
	yukis, kens := b.submit(t, b.yuki, "essay"), b.submit(t, b.ken, "essay")
	reviewed := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": yukis, "score": 80})).GradeID
	approve := func(action *uuid.UUID) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": action, "decision": "approve"}))
	}
	posted := func(grade uuid.UUID) bool {
		return b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND posted_at IS NOT NULL`, grade) == 1
	}

	proposed := b.MustCall(bot, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3}, "post")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the bot's proposal: %+v", proposed)
	}
	later := testkit.Result[tools.GradeSubmitOut](t, b.do(t, ta, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 3})).GradeID
	if v := approve(proposed.ActionID); v.Outcome != domain.StatusExecuted {
		t.Fatalf("approval: %+v", v)
	}
	if !posted(reviewed) || posted(later) {
		t.Fatalf("posted: Yuki's %v (want true), Ken's, entered after the proposal, %v (want false)", posted(reviewed), posted(later))
	}

	// A draft replaced while the proposal waits is not what was proposed
	// either, and neither is its replacement: the approval fails plainly.
	proposed = b.MustCall(bot, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3}, "post-again")
	replaced := testkit.Result[tools.GradeSubmitOut](t, b.do(t, ta, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 5})).GradeID
	if v := approve(proposed.ActionID); v.Outcome != domain.StatusFailed || strings.Contains(v.Error.Message, "regrade") {
		t.Fatalf("approving over a replaced draft: %+v", v)
	}
	if posted(later) || posted(replaced) {
		t.Fatal("a draft nobody proposed to post was posted")
	}
}

// A proposal to post an assignment names the drafts that were waiting when
// it was made. One of them posted by hand while it waits is out already, as
// what was proposed, so approving passes over it and posts the rest. One
// posted and then regraded has been replaced: the approval fails, and does
// not send the approver to regrade it. Only an approval passes over
// anything. A call naming a posted grade is told it is posted, and one
// giving drafts beside the assignment, as a proposal records them, is
// refused.
func TestAnApprovedPostPassesOverDraftsPostedMeanwhile(t *testing.T) {
	b := build(t)
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "release-bot"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "ta", "perms": m{"grade_post": "confirm_required"}})
	handIn := func(student, assignment uuid.UUID) uuid.UUID {
		t.Helper()
		work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, student, "submission.create",
			m{"course_id": b.course, "assignment_id": assignment, "body": "essay"})).SubmissionID
		b.do(t, student, "submission.submit", m{"course_id": b.course, "submission_id": work})
		return work
	}
	draft := func(work uuid.UUID, score int) uuid.UUID {
		t.Helper()
		return testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": score})).GradeID
	}
	propose := func(assignment uuid.UUID, key string) *uuid.UUID {
		t.Helper()
		out := b.MustCall(bot, "grade.post", m{"course_id": b.course, "assignment_id": assignment}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("the bot's proposal: %+v", out)
		}
		return out.ActionID
	}
	approve := func(action *uuid.UUID) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": action, "decision": "approve"}))
	}
	posted := func(grade uuid.UUID) bool {
		return b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND posted_at IS NOT NULL`, grade) == 1
	}

	yukis, kens := draft(handIn(b.yuki, b.hw3), 80), draft(handIn(b.ken, b.hw3), 70)
	proposal := propose(b.hw3, "post-hw3")
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukis}})
	v := approve(proposal)
	if v.Outcome != domain.StatusExecuted {
		t.Fatalf("approving after Yuki's draft was posted by hand: %+v", v)
	}
	if got := testkit.Result[tools.GradePostOut](t, pipeline.Outcome{Result: v.Result}).Posted; len(got) != 1 || got[0] != kens || !posted(kens) {
		t.Fatalf("the approval posted %v, want Ken's %s alone", got, kens)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND posted_by_member_id = $2`, yukis, b.satoM); n != 1 {
		t.Fatal("Yuki's grade is no longer the one Sato posted")
	}

	if out := b.MustCall(b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukis}}, "again"); out.Status != domain.StatusFailed ||
		!strings.Contains(out.Error.Message, "already posted; use grade.regrade") {
		t.Fatalf("a call posting Yuki's posted grade: %+v", out)
	}
	mid := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 50})).GradeID
	both := m{"course_id": b.course, "assignment_id": b.hw3, "grade_ids": []uuid.UUID{yukis, mid}}
	for _, by := range []struct {
		what  string
		actor uuid.UUID
	}{{"a call", b.sato}, {"a proposal", bot}} {
		if out := b.MustCall(by.actor, "grade.post", both, "both"); out.Status != domain.StatusFailed ||
			out.Error.Code != apperr.InvalidArgument || !strings.Contains(out.Error.Message, "exactly one") {
			t.Fatalf("%s giving drafts beside the assignment: %+v", by.what, out)
		}
	}
	if posted(mid) {
		t.Fatal("the midterm draft was posted by a call giving drafts beside the assignment")
	}

	// On HW4, Yuki's draft is posted by hand and then regraded.
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10, "component_id": b.bucket})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw4})
	yukis, kens = draft(handIn(b.yuki, hw4), 8), draft(handIn(b.ken, hw4), 7)
	proposal = propose(hw4, "post-hw4")
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukis}})
	b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": yukis, "score": 9})
	if v := approve(proposal); v.Outcome != domain.StatusFailed || strings.Contains(v.Error.Message, "regrade") {
		t.Fatalf("approving after Yuki's draft was posted and regraded: %+v", v.Error)
	}
	if posted(kens) {
		t.Fatal("Ken's draft was posted by an approval that failed")
	}
}

// A proposal that names its drafts by id is approved as one that has them
// pinned for an assignment: a draft posted by hand while it waits is out
// already, as proposed, and the approval posts the rest; one replaced
// meanwhile fails it, without sending the approver to regrade. A call naming
// a posted grade is still told it is posted, and a proposal naming one is
// refused as it is made.
func TestAnApprovedPostOfNamedDraftsPassesOverOnesPostedMeanwhile(t *testing.T) {
	b := build(t)
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "release-bot"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "ta", "perms": m{"grade_post": "confirm_required"}})
	draft := func(work uuid.UUID, score int) uuid.UUID {
		t.Helper()
		return testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": score})).GradeID
	}
	propose := func(key string, grades ...uuid.UUID) *uuid.UUID {
		t.Helper()
		out := b.MustCall(bot, "grade.post", m{"course_id": b.course, "grade_ids": grades}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("the bot's proposal: %+v", out)
		}
		return out.ActionID
	}
	approve := func(action *uuid.UUID) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": action, "decision": "approve"}))
	}
	posted := func(grade uuid.UUID) bool {
		return b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND posted_at IS NOT NULL`, grade) == 1
	}

	yukis, kens := draft(b.submit(t, b.yuki, "essay"), 80), draft(b.submit(t, b.ken, "essay"), 70)
	proposal := propose("post", yukis, kens)
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukis}})
	v := approve(proposal)
	if v.Outcome != domain.StatusExecuted {
		t.Fatalf("approving after Yuki's draft was posted by hand: %+v", v.Error)
	}
	if got := testkit.Result[tools.GradePostOut](t, pipeline.Outcome{Result: v.Result}).Posted; len(got) != 1 || got[0] != kens || !posted(kens) {
		t.Fatalf("the approval posted %v, want Ken's %s alone", got, kens)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND posted_by_member_id = $2`, yukis, b.satoM); n != 1 {
		t.Fatal("Yuki's grade is no longer the one Sato posted")
	}

	if out := b.MustCall(b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukis}}, "again"); out.Status != domain.StatusFailed ||
		!strings.Contains(out.Error.Message, "already posted; use grade.regrade") {
		t.Fatalf("a call posting Yuki's posted grade: %+v", out)
	}
	if out := b.MustCall(bot, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukis}}, "propose-posted"); out.Status != domain.StatusFailed ||
		!strings.Contains(out.Error.Message, "already posted; use grade.regrade") {
		t.Fatalf("a proposal to post Yuki's posted grade: %+v", out)
	}

	// Second attempts, and Ken's draft is replaced while the proposal waits.
	kensWork := b.submit(t, b.ken, "revised")
	yukis, kens = draft(b.submit(t, b.yuki, "revised"), 85), draft(kensWork, 75)
	proposal = propose("post-again", yukis, kens)
	replacement := draft(kensWork, 78)
	if v := approve(proposal); v.Outcome != domain.StatusFailed || !strings.Contains(v.Error.Message, "replaced") || strings.Contains(v.Error.Message, "regrade") {
		t.Fatalf("approving after Ken's draft was replaced: %+v", v.Error)
	}
	if posted(yukis) || posted(kens) || posted(replacement) {
		t.Fatal("a draft was posted by an approval that failed")
	}
}

// Validate and Pin each look for the drafts waiting, and a post by hand can
// come in between. A proposal made then would name no draft, and nobody
// could ever approve it, so Pin refuses it as Validate would have. The race
// is played here by calling Pin once the only draft has been posted.
func TestAPostProposalAboutNothingIsRefused(t *testing.T) {
	b := build(t)
	yukis := b.submit(t, b.yuki, "essay")
	draft := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": yukis, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{draft}})

	post, _ := b.P.Registry().Get("grade.post")
	pinned, err := post.Pin(context.Background(), b.Q, time.Now(), tools.GradePostIn{InCourse: tool.InCourse{CourseID: b.course}, AssignmentID: &b.hw3})
	if !apperr.Is(err, apperr.FailedPrecondition) {
		t.Fatalf("pinning a proposal to post HW3 with nothing waiting: %+v, %v", pinned, err)
	}
}

// Posting by assignment is about the assignment: one outside the caller's
// scope is out of scope whether or not anything is waiting on it, so the
// refusal tells them nothing about what is. And the batch is checked again
// against what is actually about to be posted, which may be more than what
// was there when the call was authorized.
func TestPostingByAssignmentIsScopedToTheAssignment(t *testing.T) {
	b := build(t)
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10, "component_id": b.bucket})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw4})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.graderM, "perms": m{"grade_submit": "autonomous", "grade_post": "autonomous"}})
	// The grader is listed for HW3. HW4, with nothing waiting: denied, and
	// recorded against the assignment.
	out := b.MustCall(b.grader, "grade.post", m{"course_id": b.course, "assignment_id": hw4}, "hw4")
	if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "assignment_out_of_scope" {
		t.Fatalf("posting an assignment outside the grader's scope: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND target_type = 'assignment' AND target_id = $2`, *out.ActionID, hw4); n != 1 {
		t.Fatal("the denial is not recorded against the assignment")
	}

	// A grader listed for Yuki alone posts HW3 by assignment while Sato
	// keeps entering drafts for Ken. Whatever the timing, none of Ken's
	// grades is ever posted by the grader.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	for round := range 12 {
		yukis, kens := b.submit(t, b.yuki, "essay"), b.submit(t, b.ken, "essay")
		b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": yukis, "score": 80})
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := b.Call(b.grader, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3}, "post-"+strconv.Itoa(round)); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := b.Call(b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 60}, "ken-"+strconv.Itoa(round)); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		if n := b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND posted_by_member_id = $2`, b.kenM, b.graderM); n != 0 {
			t.Fatalf("round %d: the grader posted Ken's grade, outside its scope", round)
		}
		// Whatever is still a draft, Sato posts, so the next round starts clean.
		if _, err := b.Call(b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3}, "clean-"+strconv.Itoa(round)); err != nil {
			t.Fatal(err)
		}
	}
}

// A score is a score out of the points possible when it was given, and for a
// proposal that is when it was made. While it waits there is no grade row,
// so nothing stops the work being rescaled; a 95 proposed out of 100 must not
// then be carried out as 95 out of 200, with nobody having said so.
func TestAProposalPinsThePointsItWasGivenOutOf(t *testing.T) {
	b := build(t)
	approve := func(action *uuid.UUID) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": action, "decision": "approve"}))
	}
	propose := func(args m, key string) *uuid.UUID {
		t.Helper()
		args["course_id"] = b.course
		out := b.MustCall(b.grader, "grade.submit", args, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("%+v", out)
		}
		return out.ActionID
	}
	work := b.submit(t, b.yuki, "essay")

	proposed := propose(m{"submission_id": work, "score": 95}, "p")
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND (payload->>'out_of')::numeric = 100`, *proposed); n != 1 {
		t.Fatal("the proposal does not carry the points possible its score is out of")
	}
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 200})
	if v := approve(proposed); v.Outcome != domain.StatusFailed || v.Error == nil || v.Error.Code != apperr.FailedPrecondition {
		t.Fatalf("approving 95 out of 100 on work now worth 200: %+v", v)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1`, work); n != 0 {
		t.Fatal("the score was carried out against points it was not given out of")
	}
	// Proposed again, out of what the work is worth now, it goes through.
	if v := approve(propose(m{"submission_id": work, "score": 190}, "p2")); v.Outcome != domain.StatusExecuted {
		t.Fatalf("%+v", v)
	}

	// A directly graded component is held to the same.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "assignment_scope": "all"})
	proposed = propose(m{"component_id": b.midterm, "student_member_id": b.kenM, "score": 80}, "p3")
	b.do(t, b.sato, "component.update", m{"course_id": b.course, "component_id": b.midterm, "points_possible": 120})
	if v := approve(proposed); v.Outcome != domain.StatusFailed {
		t.Fatalf("approving 80 out of 100 on an exam now worth 120: %+v", v)
	}
	// And a direct call that says what its score is out of is held to it.
	kens := b.submit(t, b.ken, "essay")
	b.try(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 90, "out_of": 100}, apperr.FailedPrecondition)
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 180, "out_of": 200})
}
