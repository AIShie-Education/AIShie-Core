package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
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

// A parent's totals are written down as posted grades on the parent. If the
// parent is later emptied and turned into something graded directly, those
// totals would sit in the way for ever: an entered grade could not be posted
// over them, and students would go on being shown a stale percentage as the
// grade for it.
func TestAFormerParentWithPostedTotalsIsNotGradedDirectly(t *testing.T) {
	b := build(t)
	exams := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.total, "name": "Exams", "weight": 30})).ID
	final := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": exams, "name": "Final", "points_possible": 100})).ID
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": final, "student_member_id": b.yukiM, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{g}})
	if n := b.Count(`SELECT count(*) FROM grade WHERE component_id = $1 AND origin = 'computed' AND superseded_by IS NULL`, exams); n != 1 {
		t.Fatalf("%d live totals on Exams, want Yuki's", n)
	}

	// Sato flattens the scheme, and tries to reuse Exams as Participation.
	b.do(t, b.sato, "component.move", m{"course_id": b.course, "component_id": final, "new_parent_id": b.total})
	reuse := m{"course_id": b.course, "component_id": exams, "name": "Participation", "points_possible": 10}
	b.try(t, b.sato, "component.update", reuse, apperr.FailedPrecondition)
	if n := b.Count(`SELECT count(*) FROM grade_component WHERE id = $1 AND name = 'Exams' AND points_possible IS NULL`, exams); n != 1 {
		t.Fatal("the refused change was kept")
	}
	// A new component is the way, and works.
	part := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": b.total, "name": "Participation", "points_possible": 10})).ID
	pg := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": part, "student_member_id": b.yukiM, "score": 9})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{pg}})

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
