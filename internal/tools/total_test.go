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

// liveTotalRow is the student's live total on a component: its id, the score
// worked out, and the override beside it, if any.
func (b *built) liveTotalRow(t *testing.T, component, student uuid.UUID) (uuid.UUID, decimal.Decimal, decimal.NullDecimal, *string) {
	t.Helper()
	var id uuid.UUID
	var score decimal.Decimal
	var override decimal.NullDecimal
	var feedback *string
	if err := b.Pool.QueryRow(t.Context(), `SELECT id, score, override_score, feedback FROM grade
		WHERE origin = 'computed' AND component_id = $1 AND student_member_id = $2
		  AND posted_at IS NOT NULL AND superseded_by IS NULL`, component, student).Scan(&id, &score, &override, &feedback); err != nil {
		t.Fatalf("live total: %v", err)
	}
	return id, score, override, feedback
}

func want(t *testing.T, what string, got decimal.Decimal, w string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(w)) {
		t.Fatalf("%s = %s, want %s", what, got, w)
	}
}

// A total a person overrides keeps the number worked out beside the
// override; above it the override counts. Writing the total again, as a
// grade beneath is regraded, carries the override on, and only clearing it
// takes it away.
func TestAnOverrideStandsBesideTheTotalAndCountsAboveIt(t *testing.T) {
	b := build(t)
	_, hw := b.gradeHW3(t, b.yuki, 80, true)
	mid := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 60})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{mid}})
	_, total, _, _ := b.liveTotalRow(t, b.total, b.yukiM)
	want(t, "total before", total, "71.43") // (40×0.8 + 30×0.6) / 70
	override := func(component uuid.UUID, score any, reason string) m {
		return m{"course_id": b.course, "student_member_id": b.yukiM, "component_id": component, "score": score, "reason": reason}
	}

	out := b.do(t, b.sato, "grade.override_total", override(b.bucket, 90, "The late penalty on HW3 is waived."))
	res := testkit.Result[tools.TotalOut](t, out)
	if !res.Changed || res.Snapshots != 1 {
		t.Fatalf("override: %+v, want it changed and the course total written again", res)
	}
	id, score, over, _ := b.liveTotalRow(t, b.bucket, b.yukiM)
	want(t, "bucket worked out", score, "80")
	if !over.Valid || !over.Decimal.Equal(decimal.NewFromInt(90)) || id != res.GradeID {
		t.Fatalf("the override is not beside the total: %v", over)
	}
	_, total, _, _ = b.liveTotalRow(t, b.total, b.yukiM)
	want(t, "total with the bucket overridden", total, "77.14") // (40×0.9 + 30×0.6) / 70
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND action_type = 'grade.override_total' AND target_type = 'grade_component'
		AND target_id = $2 AND payload->>'reason' = 'The late penalty on HW3 is waived.'`, *out.ActionID, b.bucket); n != 1 {
		t.Fatal("the override is not on record with its reason")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'grade.total_overridden' AND subject_id = $1 AND student_member_id = $2`, id, b.yukiM); n != 1 {
		t.Fatal("no grade.total_overridden event")
	}
	// The gradebook shows both, and says which line above is an override.
	book := gradebookOf(t, b, b.yuki, b.yukiM)
	if l := book[b.bucket]; l.Percent == nil || !l.Percent.Equal(decimal.NewFromInt(80)) || l.OverridePercent == nil || !l.OverridePercent.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("the bucket's line: %+v", l)
	}
	if l := book[b.total]; l.Percent == nil || !l.Percent.Equal(decimal.RequireFromString("77.14")) || !l.Items[0].Overridden {
		t.Fatalf("the total's line: %+v", l)
	}
	// Yuki sees the override; who made it and why are for those who grade.
	seen := testkit.Result[tools.GradeView](t, b.do(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": id}))
	if seen.Override == nil || !seen.Override.Score.Equal(decimal.NewFromInt(90)) || seen.Override.Reason != nil || seen.Override.ByMemberID != nil {
		t.Fatalf("Yuki reads the override as %+v", seen.Override)
	}
	seen = testkit.Result[tools.GradeView](t, b.do(t, b.sato, "grade.get", m{"course_id": b.course, "grade_id": id}))
	if seen.Override == nil || seen.Override.Reason == nil || *seen.Override.ByMemberID != b.satoM || !seen.Score.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("Sato reads the override as %+v", seen.Override)
	}
	// The same again changes nothing.
	if testkit.Result[tools.TotalOut](t, b.do(t, b.sato, "grade.override_total", override(b.bucket, 90, "The late penalty on HW3 is waived."))).Changed {
		t.Fatal("the same override again was a change")
	}

	// A regrade beneath writes the bucket again, worked out anew: the
	// override is carried on, and still counts above.
	b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": hw, "score": 70})
	_, score, over, _ = b.liveTotalRow(t, b.bucket, b.yukiM)
	want(t, "bucket worked out after the regrade", score, "70")
	if !over.Valid || !over.Decimal.Equal(decimal.NewFromInt(90)) {
		t.Fatal("writing the total again took the override away")
	}
	_, total, _, _ = b.liveTotalRow(t, b.total, b.yukiM)
	want(t, "total after the regrade", total, "77.14")

	// Cleared, the total worked out counts again; the override stays in
	// the history.
	res = testkit.Result[tools.TotalOut](t, b.do(t, b.sato, "grade.clear_override",
		m{"course_id": b.course, "student_member_id": b.yukiM, "component_id": b.bucket}))
	if !res.Changed {
		t.Fatalf("clear: %+v", res)
	}
	if _, _, over, _ = b.liveTotalRow(t, b.bucket, b.yukiM); over.Valid {
		t.Fatal("the override is still on the live total")
	}
	_, total, _, _ = b.liveTotalRow(t, b.total, b.yukiM)
	want(t, "total cleared", total, "65.71") // (40×0.7 + 30×0.6) / 70
	if n := b.Count(`SELECT count(*) FROM grade WHERE component_id = $1 AND student_member_id = $2 AND override_score = 90
		AND superseded_by IS NOT NULL`, b.bucket, b.yukiM); n < 2 {
		t.Fatalf("%d superseded totals carry the override; its history is lost", n)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'grade.total_override_cleared' AND student_member_id = $1`, b.yukiM); n != 1 {
		t.Fatal("no grade.total_override_cleared event")
	}
	if testkit.Result[tools.TotalOut](t, b.do(t, b.sato, "grade.clear_override",
		m{"course_id": b.course, "student_member_id": b.yukiM, "component_id": b.bucket})).Changed {
		t.Fatal("clearing what is not there was a change")
	}
	// The course total itself is overridden the same way.
	b.do(t, b.sato, "grade.override_total", override(b.total, 70, "Borderline: the lab work was strong."))
	if book := gradebookOf(t, b, b.sato, b.yukiM); book[b.total].OverridePercent == nil || !book[b.total].OverridePercent.Equal(decimal.NewFromInt(70)) {
		t.Fatalf("the course total's override: %+v", book[b.total])
	}
}

// A comment on a total is feedback for the student, carried on as the total
// is written again, and taken away with an empty one.
func TestATotalTakesAComment(t *testing.T) {
	b := build(t)
	_, hw := b.gradeHW3(t, b.yuki, 80, true)
	comment := func(text string) tools.TotalOut {
		return testkit.Result[tools.TotalOut](t, b.do(t, b.sato, "grade.comment_total",
			m{"course_id": b.course, "student_member_id": b.yukiM, "component_id": b.total, "feedback": text}))
	}
	if !comment("A strong first half of term.").Changed {
		t.Fatal("the comment changed nothing")
	}
	id, _, _, _ := b.liveTotalRow(t, b.total, b.yukiM)
	if got := testkit.Result[tools.GradeView](t, b.do(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": id})); got.Feedback == nil || *got.Feedback != "A strong first half of term." {
		t.Fatalf("Yuki reads %+v", got.Feedback)
	}
	if comment("A strong first half of term.").Changed {
		t.Fatal("the same comment again was a change")
	}
	b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": hw, "score": 85})
	if _, score, _, fb := b.liveTotalRow(t, b.total, b.yukiM); !score.Equal(decimal.NewFromInt(85)) || fb == nil {
		t.Fatalf("the total written again: %s, comment %v", score, fb)
	}
	comment("")
	if _, _, _, fb := b.liveTotalRow(t, b.total, b.yukiM); fb != nil {
		t.Fatal("an empty comment did not take the comment away")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'grade.total_commented' AND student_member_id = $1`, b.yukiM); n != 2 {
		t.Fatalf("%d grade.total_commented events, want two", n)
	}
}

// Who may override a total, and what may be overridden.
func TestWhatTotalsMayBeOverriddenAndByWhom(t *testing.T) {
	b := build(t)
	b.gradeHW3(t, b.yuki, 80, true)
	args := func(student, component uuid.UUID, score any, reason string) m {
		return m{"course_id": b.course, "student_member_id": student, "component_id": component, "score": score, "reason": reason}
	}
	ok := args(b.yukiM, b.bucket, 90, "waived")
	// It writes a grade and posts it: grade_submit and grade_post, as a
	// regrade takes them, and a total spans assignments.
	ta := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "TA"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ta, "preset": "ta"}) // grade_post denied
	for name, actor := range map[string]uuid.UUID{"a student": b.yuki, "a TA who cannot post": ta, "a grader listed for HW3": b.grader} {
		if out := b.MustCall(actor, "grade.override_total", ok, "x-"+name); out.Status != domain.StatusDenied {
			t.Fatalf("%s: %+v", name, out)
		}
	}
	listed := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Listed"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": listed, "preset": "instructor", "assignment_scope": "listed",
		"listed_assignments": []uuid.UUID{b.hw3}})
	if out := b.MustCall(listed, "grade.override_total", ok, "listed"); reason(out) != "assignment_out_of_scope" {
		t.Fatalf("overriding a total with a listed assignment scope: %+v", out)
	}
	// A component graded directly is regraded, not overridden; a total not
	// written down yet has nothing to override.
	if out := b.MustCall(b.sato, "grade.override_total", args(b.yukiM, b.midterm, 90, "x"), "direct"); reason(out) != "graded_directly" {
		t.Fatalf("overriding an exam: %+v", out)
	}
	if out := b.MustCall(b.sato, "grade.override_total", args(b.kenM, b.bucket, 90, "x"), "none"); reason(out) != "no_total" {
		t.Fatalf("overriding a total never written: %+v", out)
	}
	b.try(t, b.sato, "grade.override_total", args(b.yukiM, b.bucket, -1, "x"), apperr.InvalidArgument)
	b.try(t, b.sato, "grade.override_total", args(b.yukiM, b.bucket, 90, "  "), apperr.InvalidArgument)
	if _, err := b.Call(b.sato, "grade.override_total", args(uuid.New(), b.bucket, 90, "x"), "nobody"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student who is not there: %v", err)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE override_score IS NOT NULL`); n != 0 {
		t.Fatal("a refused override was kept")
	}
	// Totals computed or overridden are still not posted or regraded as
	// entered grades are.
	id, _, _, _ := b.liveTotalRow(t, b.bucket, b.yukiM)
	b.try(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": id, "score": 50}, apperr.FailedPrecondition)
}

func gradebookOf(t *testing.T, b *built, actor, student uuid.UUID) map[uuid.UUID]tools.GradebookLine {
	t.Helper()
	lines := map[uuid.UUID]tools.GradebookLine{}
	for _, l := range testkit.Result[tools.GradebookGetOut](t, b.do(t, actor, "gradebook.get", m{"course_id": b.course, "student_member_id": student})).Components {
		lines[l.ComponentID] = l
	}
	return lines
}
