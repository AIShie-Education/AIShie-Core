package pipeline_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

type m = map[string]any

// docs/schema.md §5, step for step: an agent grades an essay.
//
//	grader-v2 is an assistant in CS101 with perm_grade_submit = confirm_required,
//	assignment_scope = 'listed' → HW3.
func TestWorkedExample_AnAgentGradesAnEssay(t *testing.T) {
	c := testkit.NewCS101(t, 40)
	yuki := c.Students[0]

	// 1. The agent calls grade.submit for Yuki's HW3 with a score, feedback
	//    and breakdown.
	submit := m{
		"course_id": c.Course, "submission_id": yuki.HW3, "score": 85,
		"feedback": "Clear thesis; the second section needs evidence.",
		"breakdown": []m{
			{"criterion": "Thesis", "points": 36, "max": 40},
			{"criterion": "Evidence", "points": 31, "max": 40, "comment": "Section 2 asserts more than it shows."},
			{"criterion": "Style", "points": 18, "max": 20},
		},
	}
	proposed := c.MustCall(c.Grader, "grade.submit", submit, "yuki-hw3")

	// 2. authorize() finds the membership, reads confirm_required, checks HW3
	//    is listed. An action row is written with status = 'proposed' and the
	//    whole proposal in payload. No grade row exists.
	if proposed.Status != domain.StatusProposed || proposed.ActionID == nil {
		t.Fatalf("step 2: outcome = %+v, want proposed", proposed)
	}
	proposal := *proposed.ActionID
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatalf("step 2: %d grade rows exist before anyone approved", n)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'proposed' AND authz_result = 'confirm_required'
		AND member_id = $2 AND target_type = 'submission' AND target_id = $3
		AND payload->>'score' = '85' AND jsonb_array_length(payload->'breakdown') = 3`,
		proposal, c.GraderM, yuki.HW3); n != 1 {
		t.Fatal("step 2: the action row does not hold the proposal as described")
	}

	// 3. Sato sees it in the approval queue...
	queue := testkit.Result[tools.ActionListOut](t, c.MustCall(c.Sato, "action.list_proposed", m{"course_id": c.Course}, ""))
	if len(queue.Actions) != 1 || queue.Actions[0].ID != proposal {
		t.Fatalf("step 3: approval queue = %+v", queue.Actions)
	}
	//    ...the agent cannot approve itself (it may not decide at all, and the
	//    database would refuse a self-decision regardless)...
	self := c.MustCall(c.Grader, "action.decide", m{"course_id": c.Course, "action_id": proposal, "decision": "approve"}, "self-approve")
	if self.Status != domain.StatusDenied {
		t.Fatalf("step 3: the agent deciding its own proposal: %+v, want denied", self)
	}
	//    ...and Sato approves: decided_by_member_id = Sato.
	decided := c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": proposal, "decision": "approve"}, "approve-yuki")
	if decided.Status != domain.StatusExecuted {
		t.Fatalf("step 3: decide outcome = %+v", decided)
	}
	verdict := testkit.Result[pipeline.DecideOut](t, decided)
	if verdict.Outcome != domain.StatusExecuted {
		t.Fatalf("step 3: proposal outcome = %+v", verdict)
	}
	gradeID := testkit.Result[tools.GradeSubmitOut](t, pipeline.Outcome{Result: verdict.Result}).GradeID

	// 4. Execution writes the grade (origin = 'entered', created_by_action_id =
	//    this action, rubric_version_id = the rubric version the agent was
	//    shown) and an event grade.created. The action becomes executed.
	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND origin = 'entered' AND score = 85
		AND created_by_action_id = $2 AND grader_member_id = $3 AND rubric_version_id = $4
		AND student_member_id = $5 AND submission_id = $6 AND posted_at IS NULL`,
		gradeID, proposal, c.GraderM, c.RubricVersion, yuki.Member, yuki.HW3); n != 1 {
		t.Fatal("step 4: the grade row is not as described")
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'executed' AND decided_by_member_id = $2
		AND executed_at IS NOT NULL AND review_state = 'none' AND result->>'grade_id' = $3`,
		proposal, c.SatoM, gradeID.String()); n != 1 {
		t.Fatal("step 4: the proposal is not executed and decided by Sato")
	}
	if n := c.Count(`SELECT count(*) FROM event WHERE type = 'grade.created' AND action_id = $1 AND subject_id = $2
		AND student_member_id = $3 AND assignment_id = $4`, proposal, gradeID, yuki.Member, c.HW3); n != 1 {
		t.Fatal("step 4: no grade.created event under the proposal, scoped to Yuki and HW3")
	}
	// The dispute query: which agent, which membership, who approved.
	var agent, approver string
	err := c.Pool.QueryRow(context.Background(), `
		SELECT pa.display_name, da.display_name
		FROM grade g
		JOIN action a        ON a.id = g.created_by_action_id
		JOIN actor pa        ON pa.id = a.actor_id
		JOIN course_member d ON d.id = a.decided_by_member_id
		JOIN actor da        ON da.id = d.actor_id
		WHERE g.id = $1`, gradeID).Scan(&agent, &approver)
	if err != nil || agent != "grader-v2" || approver != "Sato" {
		t.Fatalf("step 4: created_by_action_id does not answer the dispute: %q approved by %q (%v)", agent, approver, err)
	}

	// The other 39 are graded the same way. Sato is autonomous, so grading
	// them directly executes at once; that is not what is under test here.
	for i, s := range c.Students[1:] {
		out := c.MustCall(c.Sato, "grade.submit", m{"course_id": c.Course, "submission_id": s.HW3, "score": 60 + i%40}, "sato-"+s.HW3.String())
		if out.Status != domain.StatusExecuted {
			t.Fatalf("grading student %d: %+v", i+1, out)
		}
	}

	// 5. Sato posts the batch: one grade.post action, forty grade.posted
	//    events, and for each student a computed snapshot of the assignments
	//    bucket and the course total.
	posted := c.MustCall(c.Sato, "grade.post", m{"course_id": c.Course, "assignment_id": c.HW3}, "post-hw3")
	if posted.Status != domain.StatusExecuted {
		t.Fatalf("step 5: %+v", posted)
	}
	post := testkit.Result[tools.GradePostOut](t, posted)
	if len(post.Posted) != 40 || post.Snapshots != 80 {
		t.Fatalf("step 5: posted %d grades and wrote %d snapshots, want 40 and 80", len(post.Posted), post.Snapshots)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE action_type = 'grade.post'`); n != 1 {
		t.Fatalf("step 5: %d grade.post actions, want one for the whole batch", n)
	}
	if n := c.Count(`SELECT count(*) FROM event WHERE type = 'grade.posted' AND action_id = $1`, *posted.ActionID); n != 40 {
		t.Fatalf("step 5: %d grade.posted events, want 40", n)
	}
	if n := c.Count(`SELECT count(DISTINCT student_member_id) FROM event WHERE type = 'grade.posted'`); n != 40 {
		t.Fatalf("step 5: grade.posted events cover %d students, want one each for 40", n)
	}
	for _, component := range []uuid.UUID{c.Assignments, c.Total} {
		if n := c.Count(`SELECT count(*) FROM grade WHERE origin = 'computed' AND component_id = $1
			AND posted_at IS NOT NULL AND superseded_by IS NULL AND created_by_action_id = $2`, component, *posted.ActionID); n != 40 {
			t.Fatalf("step 5: %d live snapshots for component %s, want 40", n, component)
		}
	}
	// Yuki: HW3 85/100 is all there is. HW4 and the midterm are ungraded and
	// left out, so both the bucket and the total stand at 85.
	var bucket, total decimal.Decimal
	if err := c.Pool.QueryRow(context.Background(), `
		SELECT (SELECT score FROM grade WHERE origin = 'computed' AND component_id = $1 AND student_member_id = $3),
		       (SELECT score FROM grade WHERE origin = 'computed' AND component_id = $2 AND student_member_id = $3)`,
		c.Assignments, c.Total, yuki.Member).Scan(&bucket, &total); err != nil {
		t.Fatal(err)
	}
	if !bucket.Equal(decimal.NewFromInt(85)) || !total.Equal(decimal.NewFromInt(85)) {
		t.Fatalf("step 5: Yuki's snapshots are %s and %s, want 85 and 85", bucket, total)
	}
	// Nothing about a score leaks into the feed: payloads hold ids and small
	// facts, so no score or feedback key, and no number anywhere.
	if n := c.Count(`SELECT count(*) FROM event
		WHERE payload ?| array['score', 'feedback', 'breakdown', 'percent']
		   OR EXISTS (SELECT 1 FROM jsonb_each(payload) kv WHERE jsonb_typeof(kv.value) = 'number')`); n != 0 {
		t.Fatalf("%d events carry a score, feedback or a number in their payload", n)
	}
}

// "If Sato rejects, the action becomes rejected and the grade tables are never
// touched — no garbage in the grades, full record in the log."
func TestWorkedExample_SatoRejects(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]

	proposed := c.MustCall(c.Grader, "grade.submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 12}, "k1")
	decided := c.MustCall(c.Sato, "action.decide",
		m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "reject", "reason": "Far too harsh; read section 3 again."}, "k2")

	if v := testkit.Result[pipeline.DecideOut](t, decided); v.Outcome != domain.StatusRejected {
		t.Fatalf("outcome = %+v", v)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatalf("%d grade rows after a rejection", n)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'rejected' AND decided_by_member_id = $2
		AND executed_at IS NULL AND result->'decision'->>'reason' LIKE 'Far too harsh%'`, *proposed.ActionID, c.SatoM); n != 1 {
		t.Fatal("the log does not record the rejection, who made it and why")
	}
	// The agent finds out from its own feed.
	if n := c.Count(`SELECT count(*) FROM event e JOIN action a ON a.id = e.action_id
		WHERE e.type = 'action.rejected' AND a.member_id = $1`, c.GraderM); n != 1 {
		t.Fatal("no action.rejected event filed under the agent's proposal")
	}
	// A rejected proposal cannot be decided again.
	again := c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve"}, "k3")
	if again.Status != domain.StatusFailed {
		t.Fatalf("deciding twice: %+v, want failed", again)
	}
}
