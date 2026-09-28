package tools_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// liveGrade is the one live grade entered on a submission, or on a component
// for a student: its id, its score, and whether it is posted.
func (b *built) liveGrade(t *testing.T, submission, component *uuid.UUID, student uuid.UUID) (uuid.UUID, decimal.Decimal, bool) {
	t.Helper()
	var id uuid.UUID
	var score decimal.Decimal
	var posted bool
	if err := b.Pool.QueryRow(t.Context(), `SELECT id, score, posted_at IS NOT NULL FROM grade
		WHERE origin = 'entered' AND superseded_by IS NULL AND student_member_id = $3
		  AND (submission_id = $1 OR component_id = $2)`, submission, component, student).Scan(&id, &score, &posted); err != nil {
		t.Fatalf("live grade: %v", err)
	}
	return id, score, posted
}

// totalOf is the student's live posted total on a component, and whether it
// says it has nothing to go on.
func (b *built) totalOf(t *testing.T, component, student uuid.UUID) (decimal.Decimal, bool) {
	t.Helper()
	var score decimal.Decimal
	var none bool
	if err := b.Pool.QueryRow(t.Context(), `SELECT score, breakdown->'fraction' = 'null'::jsonb FROM grade
		WHERE origin = 'computed' AND component_id = $1 AND student_member_id = $2
		  AND posted_at IS NOT NULL AND superseded_by IS NULL`, component, student).Scan(&score, &none); err != nil {
		t.Fatalf("live total: %v", err)
	}
	return score, none
}

func (b *built) gradeHW3(t *testing.T, student uuid.UUID, score int, post bool) (work, grade uuid.UUID) {
	t.Helper()
	work = b.submit(t, student, "essay")
	grade = testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "submission_id": work, "score": score, "feedback": "Clear thesis."})).GradeID
	if post {
		b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{grade}})
	}
	return work, grade
}

// What an assignment is worth may change after grades are entered for it,
// and the change says what becomes of them: rescaled, each a new grade in
// proportion with the old one kept, or kept as they are, out of the new
// points. Either way the totals it changes are written again at once.
func TestAChangeOfPointsSaysWhatBecomesOfItsGrades(t *testing.T) {
	b := build(t)
	yukiWork, yukiGrade := b.gradeHW3(t, b.yuki, 90, true)
	kenWork, kenDraft := b.gradeHW3(t, b.ken, 60, false)
	// A feedback file travels with Yuki's grade.
	note := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": "feedback",
		"title": "Comments", "grade_id": yukiGrade, "body_md": "Section 2 needs evidence."})).DocumentID
	if total, _ := b.totalOf(t, b.total, b.yukiM); !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Yuki's total before: %s", total)
	}
	points := func(to int, how string) m {
		args := m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": to}
		if how != "" {
			args["existing_grades"] = how
		}
		return args
	}

	// Nobody says what becomes of them: refused, and nothing changes.
	out := b.MustCall(b.sato, "assignment.update", points(50, ""), "silent")
	if out.Status != domain.StatusFailed || reason(out) != "existing_grades_required" {
		t.Fatalf("a change of points with grades entered and nothing said: %+v", out)
	}
	if _, err := b.Call(b.sato, "assignment.update", points(50, "halve"), "typo"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("existing_grades that is neither: %v", err)
	}

	// Rescaled: 90 of 100 is 45 of 50, the draft 60 is 30, each a new row.
	out = b.do(t, b.sato, "assignment.update", points(50, "rescale"))
	res := testkit.Result[tools.SchemeChangeOut](t, out)
	if res.Rescaled != 2 || res.Snapshots == 0 {
		t.Fatalf("rescale: %+v", res)
	}
	now, score, posted := b.liveGrade(t, &yukiWork, nil, b.yukiM)
	if !score.Equal(decimal.NewFromInt(45)) || !posted || now == yukiGrade {
		t.Fatalf("Yuki's grade after rescaling: %s posted=%v", score, posted)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND superseded_by = $2 AND score = 90`, yukiGrade, now); n != 1 {
		t.Fatal("the grade rescaled is not kept, superseded by the new one")
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE id = $1 AND created_by_action_id = $2 AND feedback = 'Clear thesis.'
		AND rubric_version_id IS NOT DISTINCT FROM (SELECT rubric_version_id FROM grade WHERE id = $3)`, now, *out.ActionID, yukiGrade); n != 1 {
		t.Fatal("the rescaled grade does not name the change, or lost what the grader wrote")
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND grade_id = $2`, note, now); n != 1 {
		t.Fatal("the feedback file did not travel with the rescaled grade")
	}
	kenNow, kenScore, kenPosted := b.liveGrade(t, &kenWork, nil, b.kenM)
	if !kenScore.Equal(decimal.NewFromInt(30)) || kenPosted || kenNow == kenDraft {
		t.Fatalf("Ken's draft after rescaling: %s posted=%v", kenScore, kenPosted)
	}
	if total, _ := b.totalOf(t, b.bucket, b.yukiM); !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Yuki's bucket after rescaling: %s, want 90 still", total)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'grade.regraded' AND subject_id = $1 AND payload->>'rescaled' = 'true'
		AND payload->>'replaces' = $2`, now, yukiGrade.String()); n != 1 {
		t.Fatal("no grade.regraded for the rescaled grade")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'grade.created' AND subject_id = $1 AND payload->>'rescaled' = 'true'`, kenNow); n != 1 {
		t.Fatal("no grade.created for the rescaled draft")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'assignment.updated' AND action_id = $1
		AND payload->>'existing_grades' = 'rescale' AND payload->>'points_changed' = 'true'`, *out.ActionID); n != 1 {
		t.Fatal("the change's own event does not say what it did")
	}
	// Yuki is told her grade changed, and reads it and its file.
	mine := testkit.Result[tools.GradeView](t, b.do(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": now}))
	if len(mine.FeedbackFiles) != 1 || mine.FeedbackFiles[0].DocumentID != note {
		t.Fatalf("Yuki's rescaled grade: %+v", mine)
	}

	// Kept: 45 stays 45, now out of 100, and the totals follow at once.
	res = testkit.Result[tools.SchemeChangeOut](t, b.do(t, b.sato, "assignment.update", points(100, "keep_scores")))
	if res.Rescaled != 0 || res.Snapshots == 0 {
		t.Fatalf("keep_scores: %+v", res)
	}
	if again, score, _ := b.liveGrade(t, &yukiWork, nil, b.yukiM); again != now || !score.Equal(decimal.NewFromInt(45)) {
		t.Fatalf("keep_scores wrote the grade again: %s", score)
	}
	if total, _ := b.totalOf(t, b.total, b.yukiM); !total.Equal(decimal.NewFromInt(45)) {
		t.Fatalf("Yuki's total after keeping 45 out of 100: %s", total)
	}
	// A score that fitted and would not fit any more is not kept.
	b.try(t, b.sato, "assignment.update", points(40, "keep_scores"), apperr.FailedPrecondition)
	if out := b.MustCall(b.sato, "assignment.update", points(40, "keep_scores"), "over"); reason(out) != "score_above_points" {
		t.Fatalf("keeping 45 on work worth 40: %+v", out)
	}

	// Saying what becomes of grades is changing them: it takes grade_submit
	// and grade_post as well, as a regrade does.
	designer := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Designer"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": designer, "preset": "ta",
		"perms": m{"assignment_write": "autonomous", "grade_post": "denied"}})
	if out := b.MustCall(designer, "assignment.update", points(50, "rescale"), "designer"); out.Status != domain.StatusDenied {
		t.Fatalf("rescaling without grade_post: %+v", out)
	}
	// It rewrites the totals of the whole class, so it reaches them all.
	listed := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Listed"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": listed, "preset": "instructor",
		"student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	if out := b.MustCall(listed, "assignment.update", points(50, "rescale"), "listed"); reason(out) != "student_out_of_scope" {
		t.Fatalf("rescaling a class beyond one's scope: %+v", out)
	}
	if _, score, _ := b.liveGrade(t, &yukiWork, nil, b.yukiM); !score.Equal(decimal.NewFromInt(45)) {
		t.Fatal("a refused change was kept")
	}
	// What is not about the grade still changes as it did, and needs none of it.
	b.do(t, designer, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "title": "HW3 (revised brief)"})
}

// A directly graded component is held to the same rule as an assignment.
func TestAChangeOfAnExamsPointsSaysWhatBecomesOfItsGrades(t *testing.T) {
	b := build(t)
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{g}})
	change := func(to int, how string) m {
		args := m{"course_id": b.course, "component_id": b.midterm, "points_possible": to}
		if how != "" {
			args["existing_grades"] = how
		}
		return args
	}
	if out := b.MustCall(b.sato, "component.update", change(50, ""), "silent"); reason(out) != "existing_grades_required" {
		t.Fatalf("a change of an exam's points with grades and nothing said: %+v", out)
	}
	res := testkit.Result[tools.SchemeChangeOut](t, b.do(t, b.sato, "component.update", change(50, "rescale")))
	if res.Rescaled != 1 {
		t.Fatalf("rescale: %+v", res)
	}
	if _, score, posted := b.liveGrade(t, nil, &b.midterm, b.yukiM); !score.Equal(decimal.NewFromInt(40)) || !posted {
		t.Fatalf("Yuki's midterm after rescaling: %s", score)
	}
	b.do(t, b.sato, "component.update", change(80, "keep_scores"))
	// 40 of 80 is half: the total follows at once.
	if total, _ := b.totalOf(t, b.total, b.yukiM); !total.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("Yuki's total: %s, want 50", total)
	}
	// It stays graded directly: its grades would otherwise sit on a bucket.
	if out := b.MustCall(b.sato, "component.update", m{"course_id": b.course, "component_id": b.midterm, "clear_points_possible": true}, "clear"); reason(out) != "graded_directly" {
		t.Fatalf("an exam with grades made a bucket: %+v", out)
	}
	// Nothing can be rescaled from nothing.
	quiz := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": b.total, "name": "Quiz", "points_possible": 0})).ID
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": quiz, "student_member_id": b.yukiM, "score": 0})
	if out := b.MustCall(b.sato, "component.update", m{"course_id": b.course, "component_id": quiz, "points_possible": 10, "existing_grades": "rescale"}, "zero"); reason(out) != "nothing_to_rescale" {
		t.Fatalf("rescaling from nothing: %+v", out)
	}
	b.do(t, b.sato, "component.update", m{"course_id": b.course, "component_id": quiz, "points_possible": 10, "existing_grades": "keep_scores"})
}

// Graded work may move in the scheme, an assignment to another bucket or out
// of the grade, a component under another parent. The totals where it was and
// where it goes are written again at once, with history; a total left with
// nothing beneath it says it has none, rather than go on showing what it did.
func TestGradedWorkMovesAndItsTotalsFollow(t *testing.T) {
	b := build(t)
	b.gradeHW3(t, b.yuki, 90, true)
	projects := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": b.total, "name": "Projects", "weight": 30})).ID
	move := func(args m) tools.SchemeChangeOut {
		args["course_id"], args["assignment_id"] = b.course, b.hw3
		return testkit.Result[tools.SchemeChangeOut](t, b.do(t, b.sato, "assignment.update", args))
	}

	if res := move(m{"component_id": projects}); res.Snapshots < 2 {
		t.Fatalf("moving HW3: %+v", res)
	}
	if total, none := b.totalOf(t, projects, b.yukiM); none || !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Projects after HW3 came: %s none=%v", total, none)
	}
	if total, none := b.totalOf(t, b.bucket, b.yukiM); !none || !total.IsZero() {
		t.Fatalf("Assignments after HW3 left: %s none=%v, want no total", total, none)
	}
	grades := testkit.Result[tools.GradeListOut](t, b.do(t, b.yuki, "grade.list", m{"course_id": b.course}))
	var told bool
	for _, g := range grades.Grades {
		if g.ComponentID != nil && *g.ComponentID == b.bucket {
			told = g.NoTotal
		}
	}
	if !told {
		t.Fatal("Yuki is not told her Assignments total has nothing to go on")
	}
	// Out of the grade altogether, and back.
	move(m{"clear_component": true})
	if _, none := b.totalOf(t, b.total, b.yukiM); !none {
		t.Fatal("the course total still counts work that no longer counts")
	}
	move(m{"component_id": b.bucket})
	if total, none := b.totalOf(t, b.bucket, b.yukiM); none || !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Assignments after HW3 came back: %s none=%v", total, none)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE origin = 'computed' AND component_id = $1 AND student_member_id = $2`, b.bucket, b.yukiM); n != 3 {
		t.Fatalf("%d totals on Assignments, want each kept: 90, none, 90", n)
	}

	// A component moves with what is graded beneath it.
	coursework := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
		m{"course_id": b.course, "parent_id": b.total, "name": "Coursework", "weight": 50})).ID
	res := testkit.Result[tools.SchemeChangeOut](t, b.do(t, b.sato, "component.move",
		m{"course_id": b.course, "component_id": b.bucket, "new_parent_id": coursework}))
	if res.Snapshots == 0 {
		t.Fatalf("moving a graded component: %+v", res)
	}
	if total, none := b.totalOf(t, coursework, b.yukiM); none || !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Coursework after Assignments came under it: %s none=%v", total, none)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'component.moved' AND subject_id = $1 AND payload->>'to_parent_id' = $2`, b.bucket, coursework.String()); n != 1 {
		t.Fatal("no component.moved event naming where it went")
	}
	// Beyond one's reach it is refused, as it rewrites everyone's totals.
	listed := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Listed"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": listed, "preset": "instructor",
		"student_scope": "listed", "listed_students": []uuid.UUID{b.kenM}})
	if out := b.MustCall(listed, "component.move", m{"course_id": b.course, "component_id": b.bucket, "new_parent_id": b.total}, "listed"); reason(out) != "student_out_of_scope" {
		t.Fatalf("moving graded work beyond one's scope: %+v", out)
	}
	if out := b.MustCall(listed, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "component_id": projects}, "listed-hw3"); reason(out) != "student_out_of_scope" {
		t.Fatalf("moving a graded assignment beyond one's scope: %+v", out)
	}
	// Work nobody has graded moves as it always did.
	b.do(t, listed, "component.move", m{"course_id": b.course, "component_id": projects, "new_parent_id": coursework})
}

// A regrade and a change of points at once: the regrade holds what the work
// is worth before it takes its grade, as grade.submit does, so the change
// waits for it, finds the new grade and rescales it, and nothing is left out
// of the old points.
func TestARegradeAndARescaleTakeTurns(t *testing.T) {
	b := build(t)
	work, g := b.gradeHW3(t, b.yuki, 90, true)
	// The regrade is on its way, its grade held and its score checked
	// against 100, waiting to write the new row (its key to Yuki's seat).
	release := heldBy(t, b, `SELECT 1 FROM course_member WHERE id = $1 FOR UPDATE`, b.yukiM)
	regrade := b.inFlight(b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": g, "score": 80}, "regrade")
	b.waitingFor(t, 1, regrade)
	rescale := b.inFlight(b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 50,
		"existing_grades": "rescale"}, "rescale")
	b.waitingFor(t, 2, regrade, rescale)
	release()
	if out := settled(t, regrade); out.Status != domain.StatusExecuted {
		t.Fatalf("regrade: %+v", out)
	}
	if out := settled(t, rescale); out.Status != domain.StatusExecuted {
		t.Fatalf("rescale: %+v", out)
	}
	if _, score, _ := b.liveGrade(t, &work, nil, b.yukiM); !score.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("Yuki's grade: %s, want the regrade's 80 rescaled to 40", score)
	}
}
