package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

func submitArgs(c *testkit.CS101, s testkit.Student, score any) m {
	return m{"course_id": c.Course, "submission_id": s.HW3, "score": score}
}

func reason(out pipeline.Outcome) any {
	if out.Error == nil {
		return nil
	}
	return out.Error.Details["reason"]
}

func scoreOf(t *testing.T, c *testkit.CS101, gradeID uuid.UUID) decimal.Decimal {
	t.Helper()
	var s decimal.Decimal
	if err := c.Pool.QueryRow(context.Background(), `SELECT score FROM grade WHERE id = $1`, gradeID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func TestRetryReplaysTheStoredOutcome(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]

	first := c.MustCall(c.Sato, "grade.submit", submitArgs(c, yuki, 85), "k")
	if first.Status != domain.StatusExecuted || first.Replayed {
		t.Fatalf("first call: %+v", first)
	}
	// The same call again, written differently: key order, whitespace and
	// number form are not content.
	raw := []byte(`{ "score": 8.5e1, "submission_id": "` + yuki.HW3.String() + `",  "course_id": "` + c.Course.String() + `" }`)
	again, err := c.P.Invoke(context.Background(), pipeline.Caller{ActorID: c.Sato}, "grade.submit", raw, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Status != domain.StatusExecuted || *again.ActionID != *first.ActionID {
		t.Fatalf("retry: %+v", again)
	}
	if a, b := testkit.Result[tools.GradeSubmitOut](t, first), testkit.Result[tools.GradeSubmitOut](t, again); a != b {
		t.Fatalf("retry returned %v, first call returned %v", b, a)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 1 {
		t.Fatalf("%d grades after a retry, want 1", n)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 1 {
		t.Fatalf("%d actions after a retry, want 1", n)
	}
}

// The case the payload hash exists for: an agent reuses a key for new content.
func TestSameKeyDifferentContentIsRefused(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]

	first := c.MustCall(c.Sato, "grade.submit", submitArgs(c, yuki, 85), "k")
	gradeID := testkit.Result[tools.GradeSubmitOut](t, first).GradeID

	_, err := c.Call(c.Sato, "grade.submit", submitArgs(c, yuki, 60), "k")
	if !apperr.Is(err, apperr.IdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
	if got := scoreOf(t, c, gradeID); !got.Equal(decimal.NewFromInt(85)) {
		t.Fatalf("score is now %s", got)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 1 {
		t.Fatalf("%d grades, want 1", n)
	}
	// The key belongs to the actor: someone else may use the same string.
	other := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 60), "k")
	if other.Replayed || other.Status != domain.StatusProposed {
		t.Fatalf("another actor with the same key: %+v", other)
	}
}

func TestConcurrentCallsWithOneKeyActOnce(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	args := submitArgs(c, c.Students[0], 85)

	const callers = 8
	outs := make([]pipeline.Outcome, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outs[i], errs[i] = c.Call(c.Sato, "grade.submit", args, "same-key")
		}()
	}
	close(start)
	wg.Wait()

	fresh := 0
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if outs[i].Status != domain.StatusExecuted || *outs[i].ActionID != *outs[0].ActionID {
			t.Fatalf("caller %d: %+v", i, outs[i])
		}
		if !outs[i].Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d callers executed, want exactly 1", fresh)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 1 {
		t.Fatalf("%d grades, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// What is recorded, and what is not
// ---------------------------------------------------------------------------

func TestDenialsAreRecorded(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	yuki, ken := c.Students[0], c.Students[1]

	out := c.MustCall(yuki.Actor, "grade.submit", submitArgs(c, ken, 100), "cheeky")
	if out.Status != domain.StatusDenied || reason(out) != "permission_denied" {
		t.Fatalf("student grading: %+v", out)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'denied' AND authz_result = 'denied'
		AND member_id = $2 AND action_type = 'grade.submit' AND payload->>'score' = '100'`, *out.ActionID, yuki.Member); n != 1 {
		t.Fatal("the denied attempt is not in the log with what was attempted")
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("a denied call wrote a grade")
	}
	if replay := c.MustCall(yuki.Actor, "grade.submit", submitArgs(c, ken, 100), "cheeky"); !replay.Replayed || replay.Status != domain.StatusDenied {
		t.Fatalf("replayed denial: %+v", replay)
	}
}

// A caller who fails steps 1–3 learns nothing about which ids exist.
func TestDenialComesBeforeLookup(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	stranger := c.Actor("human", "Stranger")

	real := c.MustCall(stranger, "grade.submit", submitArgs(c, c.Students[0], 1), "a")
	fake := c.MustCall(stranger, "grade.submit", m{"course_id": c.Course, "submission_id": uuid.New(), "score": 1}, "b")
	if real.Status != domain.StatusDenied || fake.Status != domain.StatusDenied || reason(real) != reason(fake) {
		t.Fatalf("a real id and a made-up id are told apart: %+v vs %+v", real, fake)
	}
	// Someone who is allowed does get not_found, and nothing is recorded.
	before := c.Count(`SELECT count(*) FROM action`)
	_, err := c.Call(c.Sato, "grade.submit", m{"course_id": c.Course, "submission_id": uuid.New(), "score": 1}, "c")
	if !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("err = %v, want not_found", err)
	}
	if after := c.Count(`SELECT count(*) FROM action`); after != before {
		t.Fatal("a call with nothing to act on was recorded")
	}
}

func TestMalformedCallsAreNotRecorded(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	cases := []struct {
		name, tool, key string
		args            m
		code            apperr.Code
	}{
		{"unknown tool", "grade.obliterate", "k", m{}, apperr.NotFound},
		{"no idempotency key", "grade.submit", "", submitArgs(c, yuki, 1), apperr.InvalidArgument},
		{"missing required field", "grade.submit", "k", m{"course_id": c.Course, "submission_id": yuki.HW3}, apperr.InvalidArgument},
		{"unknown field", "grade.submit", "k", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 1, "sudo": true}, apperr.InvalidArgument},
		{"wrong type", "grade.submit", "k", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": true}, apperr.InvalidArgument},
		{"not a uuid", "grade.submit", "k", m{"course_id": "cs101", "submission_id": yuki.HW3, "score": 1}, apperr.InvalidArgument},
		{"neither target", "grade.submit", "k", m{"course_id": c.Course, "score": 1}, apperr.InvalidArgument},
		{"unknown course", "grade.submit", "k", m{"course_id": uuid.New(), "submission_id": yuki.HW3, "score": 1}, apperr.NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Call(c.Sato, tc.tool, tc.args, tc.key); !apperr.Is(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatalf("%d actions recorded for calls that were never attempts", n)
	}
}

func TestRuleBreakingCallsAreRecordedAsFailed(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]

	over := c.MustCall(c.Sato, "grade.submit", submitArgs(c, yuki, 150), "over")
	if over.Status != domain.StatusFailed || over.Error.Code != apperr.FailedPrecondition {
		t.Fatalf("150 out of 100: %+v", over)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'failed' AND executed_at IS NULL
		AND result->'error'->>'code' = 'failed_precondition'`, *over.ActionID); n != 1 {
		t.Fatal("the failure is not recorded with its reason")
	}
	extra := submitArgs(c, yuki, 105)
	extra["allow_extra"] = true
	if out := c.MustCall(c.Sato, "grade.submit", extra, "extra"); out.Status != domain.StatusExecuted {
		t.Fatalf("105 with allow_extra: %+v", out)
	}
	// A proposal that could never run is failed at once, not queued.
	if out := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 150), "agent-over"); out.Status != domain.StatusFailed {
		t.Fatalf("agent proposing 150 out of 100: %+v, want failed rather than proposed", out)
	}
}

type probeIn struct {
	Mode string `json:"mode"`
}
type probeOut struct {
	OK bool `json:"ok"`
}

// probe is a self-gated tool that writes a row and then succeeds, fails as
// the caller's fault, or fails as ours.
func probe() tool.Tool {
	return tool.Define(tool.Spec[probeIn, probeOut]{
		Name: "probe.run", Kind: tool.Write, Gate: tool.Gate{Self: true},
		Resolve: func(context.Context, dbq.Querier, probeIn) (tool.Target, error) { return tool.Target{}, nil },
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in probeIn) (probeOut, error) {
			if _, err := ec.Tx.Exec(ctx, `INSERT INTO department (name) VALUES ('written by probe')`); err != nil {
				return probeOut{}, err
			}
			switch in.Mode {
			case "caller-fault":
				return probeOut{}, apperr.Precondition("no")
			case "constraint":
				_, err := ec.Tx.Exec(ctx, `INSERT INTO term (name, starts_on, ends_on) VALUES ('backwards', '2026-12-01', '2026-01-01')`)
				return probeOut{}, err
			case "our-fault":
				return probeOut{}, errors.New("disk on fire")
			}
			return probeOut{OK: true}, nil
		},
	})
}

func TestExecuteFailures(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	c.P.Registry().Register(probe())
	written := func() int { return c.Count(`SELECT count(*) FROM department WHERE name = 'written by probe'`) }

	if out := c.MustCall(c.Sato, "probe.run", m{"mode": "ok"}, "1"); out.Status != domain.StatusExecuted || written() != 1 {
		t.Fatalf("ok: %+v, %d rows", out, written())
	}
	// The caller's fault: the work is undone, the attempt is kept.
	out := c.MustCall(c.Sato, "probe.run", m{"mode": "caller-fault"}, "2")
	if out.Status != domain.StatusFailed || written() != 1 {
		t.Fatalf("caller-fault: %+v, %d rows", out, written())
	}
	// A constraint violation escaping a tool is a conflict, and the
	// transaction survives it to record the failure.
	out = c.MustCall(c.Sato, "probe.run", m{"mode": "constraint"}, "3")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict || written() != 1 {
		t.Fatalf("constraint: %+v, %d rows", out, written())
	}
	// Our fault: an error, nothing recorded, and the same key runs clean
	// when the caller retries.
	before := c.Count(`SELECT count(*) FROM action`)
	if _, err := c.Call(c.Sato, "probe.run", m{"mode": "our-fault"}, "4"); err == nil || apperr.Is(err, apperr.FailedPrecondition) {
		t.Fatalf("our-fault: err = %v, want an internal error", err)
	}
	if c.Count(`SELECT count(*) FROM action`) != before || written() != 1 {
		t.Fatal("an internal fault left something behind")
	}
}

type secretIn struct {
	Label    string `json:"label"`
	Password string `json:"password"`
}
type secretOut struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

func TestSecretsAreNeverStored(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	c.P.Registry().Register(tool.Define(tool.Spec[secretIn, secretOut]{
		Name: "probe.secret", Kind: tool.Write, Gate: tool.Gate{Self: true},
		SecretIn: []string{"password"}, SecretOut: []string{"token"},
		Resolve: func(context.Context, dbq.Querier, secretIn) (tool.Target, error) { return tool.Target{}, nil },
		Execute: func(context.Context, *tool.ExecCtx, secretIn) (secretOut, error) {
			return secretOut{ID: "cred-1", Token: "ais_live_TOPSECRET"}, nil
		},
	}))

	first := c.MustCall(c.Sato, "probe.secret", m{"label": "laptop", "password": "hunter2"}, "k")
	if got := testkit.Result[secretOut](t, first); got.Token != "ais_live_TOPSECRET" {
		t.Fatalf("the caller did not get its token: %+v", got)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE payload::text LIKE '%hunter2%' OR result::text LIKE '%TOPSECRET%'
		OR payload ? 'password' OR result ? 'token'`); n != 0 {
		t.Fatal("a secret reached the action log")
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE payload->>'label' = 'laptop' AND result->>'id' = 'cred-1'`); n != 1 {
		t.Fatal("the non-secret parts were not stored")
	}
	// A replay cannot show the secret a second time, because it was not kept.
	again := c.MustCall(c.Sato, "probe.secret", m{"label": "laptop", "password": "hunter2"}, "k")
	if got := testkit.Result[secretOut](t, again); !again.Replayed || got.Token != "" || got.ID != "cred-1" {
		t.Fatalf("replay: %+v %+v", again, got)
	}
}

func TestReadsLeaveNoTrace(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	yuki, ken := c.Students[0], c.Students[1]

	own := c.MustCall(yuki.Actor, "gradebook.get", m{"course_id": c.Course, "student_member_id": yuki.Member}, "")
	if own.Status != domain.StatusExecuted || own.ActionID != nil {
		t.Fatalf("own gradebook: %+v", own)
	}
	other := c.MustCall(yuki.Actor, "gradebook.get", m{"course_id": c.Course, "student_member_id": ken.Member}, "")
	if other.Status != domain.StatusDenied || reason(other) != "student_out_of_scope" || other.Result != nil {
		t.Fatalf("another student's gradebook: %+v", other)
	}
	// Listed for HW3 only: a gradebook spans every assignment.
	c.Exec(`UPDATE course_member SET perm_grade_read = 'autonomous' WHERE id = $1`, c.GraderM)
	if out := c.MustCall(c.Grader, "gradebook.get", m{"course_id": c.Course, "student_member_id": yuki.Member}, ""); reason(out) != "assignment_out_of_scope" {
		t.Fatalf("HW3-only grader reading a whole gradebook: %+v", out)
	}
	if n := c.Count(`SELECT count(*) FROM action`) + c.Count(`SELECT count(*) FROM event`); n != 0 {
		t.Fatalf("reads left %d rows behind", n)
	}
}

// ---------------------------------------------------------------------------
// Scope, archived courses
// ---------------------------------------------------------------------------

func TestScopeIsCheckedOnWrites(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	yuki, ken := c.Students[0], c.Students[1]
	hw4 := c.Submission(c.Course, c.HW4, yuki.Member)

	out := c.MustCall(c.Grader, "grade.submit", m{"course_id": c.Course, "submission_id": hw4, "score": 50}, "hw4")
	if out.Status != domain.StatusDenied || reason(out) != "assignment_out_of_scope" {
		t.Fatalf("grader on an unlisted assignment: %+v", out)
	}
	// A batch is in scope only if all of it is.
	ta := c.Actor("human", "TA for Yuki")
	c.Member(c.Course, ta, "ta", testkit.ListedStudents(yuki.Member), testkit.WithPerm(domain.PermGradePost, domain.Autonomous))
	g1 := testkit.Result[tools.GradeSubmitOut](t, c.MustCall(c.Sato, "grade.submit", submitArgs(c, yuki, 80), "g1")).GradeID
	g2 := testkit.Result[tools.GradeSubmitOut](t, c.MustCall(c.Sato, "grade.submit", submitArgs(c, ken, 70), "g2")).GradeID

	batch := c.MustCall(ta, "grade.post", m{"course_id": c.Course, "grade_ids": []uuid.UUID{g1, g2}}, "post-both")
	if batch.Status != domain.StatusDenied || reason(batch) != "student_out_of_scope" {
		t.Fatalf("batch reaching outside scope: %+v", batch)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE posted_at IS NOT NULL`); n != 0 {
		t.Fatal("part of an out-of-scope batch was posted")
	}
	if one := c.MustCall(ta, "grade.post", m{"course_id": c.Course, "grade_ids": []uuid.UUID{g1}}, "post-yuki"); one.Status != domain.StatusExecuted {
		t.Fatalf("posting the in-scope grade alone: %+v", one)
	}
}

func TestArchivedCourseRefusesEveryWrite(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	c.Exec(`UPDATE course SET status = 'archived' WHERE id = $1`, c.Course)

	out := c.MustCall(c.Sato, "grade.submit", submitArgs(c, c.Students[0], 85), "k")
	if out.Status != domain.StatusDenied || reason(out) != "course_archived" {
		t.Fatalf("instructor writing to an archived course: %+v", out)
	}
	if read := c.MustCall(c.Sato, "gradebook.get", m{"course_id": c.Course, "student_member_id": c.Students[0].Member}, ""); read.Status != domain.StatusExecuted {
		t.Fatalf("reading an archived course: %+v", read)
	}
}

// ---------------------------------------------------------------------------
// pending_review
// ---------------------------------------------------------------------------

func TestPendingReview(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	ta := c.Actor("human", "TA")
	taM := c.Member(c.Course, ta, "ta", testkit.WithPerm(domain.PermGradeSubmit, domain.PendingReview))

	// Executes immediately, then waits in the review queue.
	out := c.MustCall(ta, "grade.submit", submitArgs(c, c.Students[0], 77), "ta-1")
	if out.Status != domain.StatusExecuted || out.ReviewState != domain.ReviewPending {
		t.Fatalf("pending_review call: %+v", out)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE grader_member_id = $1`, taM); n != 1 {
		t.Fatal("a pending_review action must execute at once")
	}
	queue := testkit.Result[tools.ActionListOut](t, c.MustCall(c.Sato, "action.list_pending_review", m{"course_id": c.Course}, ""))
	if len(queue.Actions) != 1 || queue.Actions[0].ID != *out.ActionID {
		t.Fatalf("review queue: %+v", queue.Actions)
	}

	review := func(actor uuid.UUID, outcome, key string) pipeline.Outcome {
		return c.MustCall(actor, "action.review", m{"course_id": c.Course, "action_id": out.ActionID, "outcome": outcome}, key)
	}
	// Nobody reviews their own action, even with the permission to review.
	c.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = $1`, taM)
	if self := review(ta, "reviewed", "self"); self.Status != domain.StatusFailed || self.Error.Code != apperr.Forbidden {
		t.Fatalf("self-review: %+v", self)
	}
	// Nor from a seat taken since: removed and seated again, the TA is still
	// who entered the grade.
	c.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, taM)
	c.Member(c.Course, ta, "instructor")
	if self := review(ta, "reviewed", "self-again"); self.Status != domain.StatusFailed || self.Error.Code != apperr.Forbidden {
		t.Fatalf("self-review from a new seat: %+v", self)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND review_state = 'pending'`, *out.ActionID); n != 1 {
		t.Fatal("the TA's grade was marked reviewed from the TA's new seat")
	}
	if esc := review(c.Sato, "escalated", "esc"); esc.Status != domain.StatusExecuted {
		t.Fatalf("escalate: %+v", esc)
	}
	if twice := review(c.Sato, "escalated", "esc2"); twice.Status != domain.StatusFailed {
		t.Fatalf("escalating twice: %+v", twice)
	}
	// An escalation is for someone else to look at: Sato does not close his
	// own, from his seat or from one he has taken since.
	if own := review(c.Sato, "reviewed", "own"); own.Status != domain.StatusFailed || own.Error.Code != apperr.Forbidden {
		t.Fatalf("closing his own escalation: %+v", own)
	}
	c.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, c.SatoM)
	c.Member(c.Course, c.Sato, "instructor")
	if own := review(c.Sato, "reviewed", "own-again"); own.Status != domain.StatusFailed || own.Error.Code != apperr.Forbidden {
		t.Fatalf("closing his own escalation from a new seat: %+v", own)
	}
	second := c.Actor("human", "Second reviewer")
	secondM := c.Member(c.Course, second, "instructor")
	if done := review(second, "reviewed", "done"); done.Status != domain.StatusExecuted {
		t.Fatalf("review after escalation: %+v", done)
	}
	if again := review(second, "reviewed", "again"); again.Status != domain.StatusFailed {
		t.Fatalf("reviewing twice: %+v", again)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND review_state = 'reviewed' AND reviewed_by_member_id = $2`, *out.ActionID, secondM); n != 1 {
		t.Fatal("the review is not recorded against the second reviewer")
	}
	// Reviewing undoes nothing.
	if n := c.Count(`SELECT count(*) FROM grade WHERE grader_member_id = $1 AND superseded_by IS NULL`, taM); n != 1 {
		t.Fatal("review changed the grade")
	}
	// An autonomous action is not reviewable.
	auto := c.MustCall(c.Sato, "grade.submit", submitArgs(c, c.Students[1], 60), "auto")
	if out := c.MustCall(ta, "action.review", m{"course_id": c.Course, "action_id": auto.ActionID, "outcome": "reviewed"}, "r-auto"); out.Status != domain.StatusFailed {
		t.Fatalf("reviewing an autonomous action: %+v", out)
	}
}

// A decision can wait for a decision of its own: a triage agent whose
// approvals a human confirms. Confirming it is deciding what it decides, so
// the proposer of the action underneath is not the one who may confirm it —
// or the four eyes on the TA's grade would be the TA's own two and an
// agent's. The same goes for a review, and for a chain of any length.
func TestNobodyDecidesTheirOwnActionAtOneRemove(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	ta := c.Actor("human", "TA")
	taM := c.Member(c.Course, ta, "ta",
		testkit.WithPerm(domain.PermGradeSubmit, domain.ConfirmRequired),
		testkit.WithPerm(domain.PermActionDecide, domain.Autonomous))
	triage := c.Actor("agent", "triage")
	triageM := c.Member(c.Course, triage, "ta", testkit.WithPerm(domain.PermActionDecide, domain.ConfirmRequired))
	second := c.Actor("agent", "second opinion")
	c.Member(c.Course, second, "ta", testkit.WithPerm(domain.PermActionDecide, domain.ConfirmRequired))

	decide := func(actor uuid.UUID, action *uuid.UUID, decision, key string) pipeline.Outcome {
		t.Helper()
		return c.MustCall(actor, "action.decide", m{"course_id": c.Course, "action_id": action, "decision": decision}, key)
	}
	refused := func(what string, out pipeline.Outcome) {
		t.Helper()
		if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.Forbidden {
			t.Fatalf("%s: %+v", what, out)
		}
	}

	proposed := c.MustCall(ta, "grade.submit", submitArgs(c, c.Students[0], 99), "p")
	refused("the TA deciding her own proposal", decide(ta, proposed.ActionID, "approve", "self"))
	nested := decide(triage, proposed.ActionID, "approve", "t")
	if nested.Status != domain.StatusProposed {
		t.Fatalf("the agent's approval: %+v", nested)
	}
	refused("the TA confirming the approval of her own proposal", decide(ta, nested.ActionID, "approve", "ta-approve"))
	refused("the TA rejecting the approval of her own proposal", decide(ta, nested.ActionID, "reject", "ta-reject"))
	// Two removes are one remove twice.
	deeper := decide(second, nested.ActionID, "approve", "s")
	if deeper.Status != domain.StatusProposed {
		t.Fatalf("the second agent's approval: %+v", deeper)
	}
	refused("the TA at two removes", decide(ta, deeper.ActionID, "approve", "ta-deeper"))
	// Every link counts, not only the last: the agent, trusted to decide on
	// its own since, confirming the approval of its own approval.
	c.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = $1`, triageM)
	refused("the agent confirming the approval of its own decision", decide(triage, deeper.ActionID, "approve", "t-deeper"))
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("the TA's own proposal was carried out on her own say-so")
	}
	// Someone else confirms, and the chain says who.
	if v := testkit.Result[pipeline.DecideOut](t, decide(c.Sato, nested.ActionID, "approve", "sato")); v.Outcome != domain.StatusExecuted {
		t.Fatalf("Sato confirming the approval: %+v", v)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'executed' AND decided_by_member_id = $2`, *proposed.ActionID, triageM); n != 1 {
		t.Fatal("the TA's proposal is not recorded as decided by the agent")
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND decided_by_member_id = $2`, *nested.ActionID, c.SatoM); n != 1 {
		t.Fatal("the agent's approval is not recorded as confirmed by Sato")
	}

	// A review is the same: the triage agent marks the TA's grade reviewed,
	// which waits for a human, and that human is not the TA.
	c.Exec(`UPDATE course_member SET perm_grade_submit = 'pending_review' WHERE id = $1`, taM)
	c.Exec(`UPDATE course_member SET perm_action_decide = 'confirm_required' WHERE id = $1`, triageM)
	done := c.MustCall(ta, "grade.submit", submitArgs(c, c.Students[1], 70), "p2")
	if done.ReviewState != domain.ReviewPending {
		t.Fatalf("the TA's grade: %+v", done)
	}
	review := c.MustCall(triage, "action.review", m{"course_id": c.Course, "action_id": done.ActionID, "outcome": "reviewed"}, "t2")
	if review.Status != domain.StatusProposed {
		t.Fatalf("the agent's review: %+v", review)
	}
	refused("the TA confirming the review of her own grade", decide(ta, review.ActionID, "approve", "ta-review"))
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND review_state = 'pending'`, *done.ActionID); n != 1 {
		t.Fatal("the TA's grade was marked reviewed on her own say-so")
	}
	// And a decision that is itself under review is not reviewed by whoever
	// it decided for.
	c.Exec(`UPDATE course_member SET perm_grade_submit = 'confirm_required' WHERE id = $1`, taM)
	c.Exec(`UPDATE course_member SET perm_action_decide = 'pending_review' WHERE id = $1`, triageM)
	again := c.MustCall(ta, "grade.submit", submitArgs(c, c.Students[1], 72), "p3")
	approval := decide(triage, again.ActionID, "approve", "t3")
	if approval.Status != domain.StatusExecuted || approval.ReviewState != domain.ReviewPending {
		t.Fatalf("the agent's approval under review: %+v", approval)
	}
	refused("the TA reviewing the approval of her own proposal",
		c.MustCall(ta, "action.review", m{"course_id": c.Course, "action_id": approval.ActionID, "outcome": "reviewed"}, "ta-r"))

	// A seat is not who someone is: removed and seated again, with a seat
	// that may decide anything, the TA is still whose actions these are.
	last := c.MustCall(ta, "grade.submit", submitArgs(c, c.Students[0], 50), "p4")
	if last.Status != domain.StatusProposed {
		t.Fatalf("the TA's last proposal: %+v", last)
	}
	c.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, taM)
	c.Member(c.Course, ta, "instructor")
	refused("the TA, seated again, deciding her own proposal", decide(ta, last.ActionID, "approve", "ta-again"))
	refused("the TA, seated again, confirming the review of her own grade", decide(ta, review.ActionID, "approve", "ta-review-again"))
	refused("the TA, seated again, reviewing the approval of her own proposal",
		c.MustCall(ta, "action.review", m{"course_id": c.Course, "action_id": approval.ActionID, "outcome": "reviewed"}, "ta-r-again"))
	if n := c.Count(`SELECT count(*) FROM action WHERE id = ANY($1) AND (status <> 'executed' OR review_state <> 'pending')`,
		[]uuid.UUID{*done.ActionID, *approval.ActionID}); n != 0 {
		t.Fatal("an action of the TA's was reviewed from her new seat")
	}
}

// ---------------------------------------------------------------------------
// Proposals: re-authorization and expiry
// ---------------------------------------------------------------------------

func TestApprovalReauthorizesTheProposer(t *testing.T) {
	cases := []struct {
		name    string
		change  func(c *testkit.CS101)
		outcome domain.ActionStatus
		reason  string
	}{
		{"nothing changed", func(*testkit.CS101) {}, domain.StatusExecuted, ""},
		{"proposer paused", func(c *testkit.CS101) {
			c.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, c.GraderM)
		}, domain.StatusCancelled, "membership_not_active"},
		{"proposer removed and seated again", func(c *testkit.CS101) {
			c.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, c.GraderM)
			c.Member(c.Course, c.Grader, "grader", testkit.ListedAssignments(c.HW3))
		}, domain.StatusCancelled, "membership_not_active"},
		{"proposer expired", func(c *testkit.CS101) {
			c.Exec(`UPDATE course_member SET expires_at = now() - interval '1 minute' WHERE id = $1`, c.GraderM)
		}, domain.StatusCancelled, "membership_not_active"},
		{"proposer lost the permission", func(c *testkit.CS101) {
			c.Exec(`UPDATE course_member SET perm_grade_submit = 'denied' WHERE id = $1`, c.GraderM)
		}, domain.StatusCancelled, "permission_denied"},
		{"assignment no longer listed", func(c *testkit.CS101) {
			c.Exec(`DELETE FROM member_assignment_scope WHERE member_id = $1`, c.GraderM)
		}, domain.StatusCancelled, "assignment_out_of_scope"},
		{"proposer's actor suspended", func(c *testkit.CS101) {
			c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, c.Grader)
		}, domain.StatusCancelled, "actor_not_active"},
		{"proposer since trusted more", func(c *testkit.CS101) {
			c.Exec(`UPDATE course_member SET perm_grade_submit = 'autonomous' WHERE id = $1`, c.GraderM)
		}, domain.StatusExecuted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testkit.NewCS101(t, 1)
			proposed := c.MustCall(c.Grader, "grade.submit", submitArgs(c, c.Students[0], 85), "p")
			tc.change(c)

			decided := c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve"}, "d")
			if decided.Status != domain.StatusExecuted {
				t.Fatalf("the decision itself: %+v", decided)
			}
			v := testkit.Result[pipeline.DecideOut](t, decided)
			if v.Outcome != tc.outcome {
				t.Fatalf("proposal outcome = %s, want %s (%+v)", v.Outcome, tc.outcome, v.Error)
			}
			wantGrades := 0
			if tc.outcome == domain.StatusExecuted {
				wantGrades = 1
			} else {
				if v.Error.Details["reason"] != pipeline.CancelReauthorization || v.Error.Details["authz_reason"] != tc.reason {
					t.Fatalf("cancelled because %v (%v), want %s (%s)",
						v.Error.Details["reason"], v.Error.Details["authz_reason"], pipeline.CancelReauthorization, tc.reason)
				}
				if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled' AND decided_by_member_id IS NULL`, *proposed.ActionID); n != 1 {
					t.Fatal("the proposal is not recorded as cancelled")
				}
				if n := c.Count(`SELECT count(*) FROM event WHERE type = 'action.cancelled' AND action_id = $1`, *proposed.ActionID); n != 1 {
					t.Fatal("no action.cancelled event for the proposer to find")
				}
			}
			if n := c.Count(`SELECT count(*) FROM grade`); n != wantGrades {
				t.Fatalf("%d grades, want %d", n, wantGrades)
			}
		})
	}
}

// A draft is as old as the call that made it. A proposal replaces the drafts
// that were there when it was made, and not one entered after it: on
// Wednesday the approver sees the proposal, not Tuesday's draft, and must not
// wipe out a judgement nobody put in front of them.
func TestAnApprovedProposalReplacesOnlyWhatCameBefore(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	yuki, ken := c.Students[0], c.Students[1]
	approve := func(action *uuid.UUID, key string) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": action, "decision": "approve"}, key))
	}

	// Before: Sato's draft, then the agent's proposal, then the approval.
	satos := testkit.Result[tools.GradeSubmitOut](t, c.MustCall(c.Sato, "grade.submit", submitArgs(c, yuki, 70), "sato")).GradeID
	proposed := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 85), "p")
	v := approve(proposed.ActionID, "d")
	if v.Outcome != domain.StatusExecuted {
		t.Fatalf("outcome: %+v", v)
	}
	agents := testkit.Result[tools.GradeSubmitOut](t, pipeline.Outcome{Result: v.Result}).GradeID
	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND superseded_by = $2`, satos, agents); n != 1 {
		t.Fatal("the earlier draft was not superseded by the approved one")
	}

	// After: the agent's proposal, then Sato's draft, then the approval.
	proposed = c.MustCall(c.Grader, "grade.submit", submitArgs(c, ken, 85), "p2")
	c.P.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	kens := testkit.Result[tools.GradeSubmitOut](t, c.MustCall(c.Sato, "grade.submit", submitArgs(c, ken, 60), "sato2")).GradeID
	c.P.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if v := approve(proposed.ActionID, "d2"); v.Outcome != domain.StatusFailed || v.Error == nil || !strings.Contains(v.Error.Message, "newer draft") {
		t.Fatalf("approving over a newer draft: %+v", v)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND superseded_by IS NULL`, kens); n != 1 {
		t.Fatal("the newer draft was replaced by an older judgement")
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'failed'`, *proposed.ActionID); n != 1 {
		t.Fatal("the proposal is not recorded as failed")
	}
	// The proposer can tell from the feed that the approval came to nothing.
	if n := c.Count(`SELECT count(*) FROM event WHERE type = 'action.approved' AND action_id = $1 AND payload->>'outcome' = 'failed'`, *proposed.ActionID); n != 1 {
		t.Fatal("the action.approved event does not say the proposal failed")
	}
	if n := c.Count(`SELECT count(*) FROM event WHERE type = 'action.approved' AND payload->>'outcome' = 'executed'`); n != 1 {
		t.Fatal("the action.approved event for the one that ran does not say so")
	}
}

// The draft an approved proposal writes is as old as the proposal, not the
// approval. Two proposals for the same work, approved in the order the queue
// lists them, oldest first, leave the later one standing: it was made after
// the first, so it replaces it, and which of them survives does not depend
// on the order the approver happens to work in.
func TestAnApprovedDraftIsAsOldAsItsProposal(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	first := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 70), "p1")
	c.P.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	second := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 85), "p2")
	for i, key := range []string{"d1", "d2"} {
		c.P.SetClock(func() time.Time { return time.Now().Add(time.Duration(i+1) * time.Hour) })
		proposal := []pipeline.Outcome{first, second}[i]
		v := testkit.Result[pipeline.DecideOut](t, c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": proposal.ActionID, "decision": "approve"}, key))
		if v.Outcome != domain.StatusExecuted {
			t.Fatalf("approving the proposals oldest first, number %d: %+v", i+1, v)
		}
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE submission_id = $1 AND score = 85 AND posted_at IS NULL AND superseded_by IS NULL`, yuki.HW3); n != 1 {
		t.Fatal("the later proposal's draft is not the live one")
	}
	if n := c.Count(`SELECT count(*) FROM grade g JOIN action a ON a.id = g.created_by_action_id
		WHERE g.submission_id = $1 AND g.created_at <> a.created_at`, yuki.HW3); n != 0 {
		t.Fatal("a draft is dated when it was approved, not when the call that made it was made")
	}
}

// The same, when the instances of a server do not share a clock. The agent
// proposes on one whose clock runs ahead, Sato enters a draft there after
// it, and the proposal is approved on one whose clock is behind both. It is
// still an approval, and still must not replace the draft entered after it.
func TestAnApprovalOnASlowerClockReplacesOnlyWhatCameBefore(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	yuki := c.Students[0]
	base := time.Now()
	clock := func(d time.Duration) { c.P.SetClock(func() time.Time { return base.Add(d) }) }
	clock(3 * time.Minute)
	proposed := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 85), "p")
	clock(4 * time.Minute)
	satos := testkit.Result[tools.GradeSubmitOut](t, c.MustCall(c.Sato, "grade.submit", submitArgs(c, yuki, 60), "sato")).GradeID
	clock(time.Minute)
	v := testkit.Result[pipeline.DecideOut](t, c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve"}, "d"))
	if v.Outcome != domain.StatusFailed || v.Error == nil || !strings.Contains(v.Error.Message, "newer draft") {
		t.Fatalf("approving over a newer draft on a clock behind the proposer's: %+v", v)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE id = $1 AND superseded_by IS NULL`, satos); n != 1 {
		t.Fatal("the newer draft was replaced by an older judgement")
	}
}

func TestProposalsExpire(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	proposed := c.MustCall(c.Grader, "grade.submit", submitArgs(c, c.Students[0], 85), "p")

	c.P.SetClock(func() time.Time { return time.Now().Add(pipeline.DefaultProposalTTL + time.Hour) })
	decided := c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve"}, "late")
	v := testkit.Result[pipeline.DecideOut](t, decided)
	if v.Outcome != domain.StatusCancelled || v.Error.Details["reason"] != pipeline.CancelExpired {
		t.Fatalf("approving a stale proposal: %+v", v)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("a stale proposal executed")
	}
}

func TestDecideRaces(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	other := c.Actor("human", "Co-instructor")
	c.Member(c.Course, other, "instructor")
	proposed := c.MustCall(c.Grader, "grade.submit", submitArgs(c, c.Students[0], 85), "p")

	var wg sync.WaitGroup
	outs := make([]pipeline.Outcome, 2)
	for i, actor := range []uuid.UUID{c.Sato, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i] = c.MustCall(actor, "action.decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve"}, "race")
		}()
	}
	wg.Wait()
	executed := 0
	for _, o := range outs {
		if o.Status == domain.StatusExecuted {
			executed++
		} else if o.Status != domain.StatusFailed {
			t.Fatalf("loser of the race: %+v", o)
		}
	}
	if executed != 1 {
		t.Fatalf("%d approvals went through, want exactly 1", executed)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 1 {
		t.Fatalf("%d grades, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// The feed cursor
// ---------------------------------------------------------------------------

// A reader that remembers the highest seq it has seen must never miss an
// event. Sequences number rows in the order they are asked, not the order
// transactions commit, so without the per-course lock in events.Flush a
// slower writer can commit a lower seq after the reader has moved past it.
func TestFeedCursorNeverSkips(t *testing.T) {
	c := testkit.NewCS101(t, 40)
	ctx := context.Background()

	done := make(chan struct{})
	var seen []int64
	var readerErr error
	var rg sync.WaitGroup
	rg.Add(1)
	go func() {
		defer rg.Done()
		var cursor int64
		poll := func() {
			rows, err := c.Pool.Query(ctx, `SELECT seq FROM event WHERE course_id = $1 AND seq > $2 ORDER BY seq`, c.Course, cursor)
			if err != nil {
				readerErr = err
				return
			}
			defer rows.Close()
			for rows.Next() {
				var s int64
				if readerErr = rows.Scan(&s); readerErr != nil {
					return
				}
				seen = append(seen, s)
				cursor = s
			}
		}
		for {
			select {
			case <-done:
				poll() // one last look after the writers have finished
				return
			default:
				poll()
			}
		}
	}()

	var wg sync.WaitGroup
	work := make(chan testkit.Student)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				c.MustCall(c.Sato, "grade.submit", submitArgs(c, s, 80), "g-"+s.HW3.String())
			}
		}()
	}
	for _, s := range c.Students {
		work <- s
	}
	close(work)
	wg.Wait()
	close(done)
	rg.Wait()

	if readerErr != nil {
		t.Fatal(readerErr)
	}
	if total := c.Count(`SELECT count(*) FROM event WHERE course_id = $1`, c.Course); len(seen) != total || total != 40 {
		t.Fatalf("the reader saw %d of %d events", len(seen), total)
	}
}

// What a replay returns is fixed by the row's status, not by the shape of
// what it stored: an executed decision whose proposal was cancelled stores a
// result with an "error" field of its own, and a retry of that decision must
// get the result back as it was, not be told the decision errored.
func TestAReplayedDecisionKeepsItsResult(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	proposed := c.MustCall(c.Grader, "grade.submit", submitArgs(c, c.Students[0], 85), "p")
	c.Exec(`UPDATE course_member SET perm_grade_submit = 'denied' WHERE id = $1`, c.GraderM)

	decide := m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve"}
	first := c.MustCall(c.Sato, "action.decide", decide, "d")
	again := c.MustCall(c.Sato, "action.decide", decide, "d")
	if first.Status != domain.StatusExecuted || first.Error != nil || len(first.Result) == 0 {
		t.Fatalf("first: %+v", first)
	}
	if !again.Replayed || again.Status != first.Status || again.Error != nil {
		t.Fatalf("replay: %+v\nfirst:  %+v", again, first)
	}
	was, now := testkit.Result[pipeline.DecideOut](t, first), testkit.Result[pipeline.DecideOut](t, again)
	if now.Outcome != domain.StatusCancelled || now.ActionID != *proposed.ActionID || now.Error == nil ||
		now.Outcome != was.Outcome || now.ActionID != was.ActionID || now.Error.Details["reason"] != was.Error.Details["reason"] {
		t.Fatalf("replayed decision: %+v, want %+v", now, was)
	}
}

// A key reused with a different secret is caught like any other reuse. The
// secret is not stored, so the hash commits to it through a keyed digest
// instead — which also means the hash gives nothing away about the password.
func TestAKeyReusedWithADifferentSecretIsRefused(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	set := func(password string) (pipeline.Outcome, error) {
		return c.Call(c.Sato, "credential.set_password", m{"password": password}, "pw")
	}
	if out, err := set("correct horse battery staple"); err != nil || out.Status != domain.StatusExecuted {
		t.Fatalf("first: %+v %v", out, err)
	}
	if out, err := set("correct horse battery staple"); err != nil || !out.Replayed {
		t.Fatalf("the same password again: %+v %v", out, err)
	}
	if _, err := set("a different password entirely"); !apperr.Is(err, apperr.IdempotencyConflict) {
		t.Fatalf("a different password under the same key: %v", err)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE action_type = 'credential.set_password' AND (payload::text LIKE '%horse%' OR payload::text LIKE '%different%')`); n != 0 {
		t.Fatal("a password reached the action log")
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE action_type = 'credential.set_password' AND payload_hash = encode(sha256(('credential.set_password' || chr(10) || '{}')::bytea), 'hex')`); n != 0 {
		t.Fatal("the hash does not commit to the password at all")
	}
}

// A NUL character cannot be stored by the database, so a call carrying one
// is refused as malformed before anything is attempted, rather than failing
// on our side and inviting a retry that fails the same way.
func TestANulCharacterIsRefusedAsInput(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	args := submitArgs(c, c.Students[0], 85)
	args["feedback"] = "well\x00argued"
	if _, err := c.Call(c.Sato, "grade.submit", args, "nul"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("a NUL in an argument: %v", err)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatal("something was recorded")
	}
}

// An idempotency key the database cannot hold is the caller's to fix, said
// before anything is attempted, not a fault of ours to retry for ever.
func TestAnUnstorableKeyIsRefusedAsInput(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	for _, key := range []string{"a\x00b", "a\xffb"} {
		if _, err := c.Call(c.Sato, "grade.submit", submitArgs(c, c.Students[0], 85), key); !apperr.Is(err, apperr.InvalidArgument) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatal("something was recorded")
	}
}

// A key is held to its length in characters, as MCP clients are told,
// however many bytes they take; and one that is not UTF-8 is refused as
// that, however long it is.
func TestAKeyIsCountedInCharacters(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	long := strings.Repeat("あ", pipeline.MaxIdempotencyKeyLen) // three bytes each
	if out, err := c.Call(c.Sato, "grade.submit", submitArgs(c, c.Students[0], 85), long); err != nil || out.Status != domain.StatusExecuted {
		t.Fatalf("%d characters: %+v %v", pipeline.MaxIdempotencyKeyLen, out, err)
	}
	if _, err := c.Call(c.Sato, "grade.submit", submitArgs(c, c.Students[0], 85), long+"あ"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("%d characters: %v", pipeline.MaxIdempotencyKeyLen+1, err)
	}
	if _, err := c.Call(c.Sato, "grade.submit", submitArgs(c, c.Students[0], 85), strings.Repeat("\xff", 3*pipeline.MaxIdempotencyKeyLen)); !apperr.Is(err, apperr.InvalidArgument) || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("a long key that is not UTF-8: %v", err)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 1 {
		t.Fatalf("%d actions recorded, want the one", n)
	}
}

type flakyIn struct {
	tool.InCourse
}

// A deadlock lost while an approval re-checks its proposal says nothing about
// the proposal. The decision is made again; lost again, it is recorded as
// failed, "try again", but the proposal still waits, and deciding again
// works. The proposal is not failed for good.
func TestAnApprovalThatLosesADeadlockLeavesTheProposalWaiting(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	flaky := false
	c.P.Registry().Register(tool.Define(tool.Spec[flakyIn, probeOut]{
		Name: "probe.flaky", Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		Resolve: func(_ context.Context, _ dbq.Querier, in flakyIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course"}, nil
		},
		Validate: func(context.Context, dbq.Querier, *domain.Member, flakyIn) error {
			if flaky {
				return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
			}
			return nil
		},
		Execute: func(context.Context, *tool.ExecCtx, flakyIn) (probeOut, error) { return probeOut{OK: true}, nil },
	}))
	prop := c.MustCall(c.Grader, "probe.flaky", m{"course_id": c.Course}, "p")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("proposal: %+v", prop)
	}
	flaky = true
	decide := func(key string) pipeline.Outcome {
		return c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": prop.ActionID, "decision": "approve"}, key)
	}
	if out := decide("d1"); out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.Conflict {
		t.Fatalf("a decision that lost a deadlock: %+v", out)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'proposed'`, prop.ActionID); n != 1 {
		t.Fatal("the proposal no longer waits")
	}
	flaky = false
	if out := decide("d2"); out.Status != domain.StatusExecuted || !strings.Contains(string(out.Result), `"outcome":"executed"`) {
		t.Fatalf("deciding again: %+v", out)
	}
}

type onceIn struct {
	tool.InCourse
	LoseIn string `json:"lose_in"`
}

// A call that loses a deadlock is made again, once, in a fresh transaction:
// nothing was wrong with it, and its key is not spent on a failure saying
// "try again". Here a tool loses one the first time through its Validate,
// its Pin or its Execute, and the call goes through all the same, once.
func TestACallThatLosesADeadlockIsMadeAgain(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	lost := map[string]int{}
	lose := func(where string, in onceIn) error {
		if in.LoseIn != where || lost[where] > 0 {
			return nil
		}
		lost[where]++
		return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	}
	c.P.Registry().Register(tool.Define(tool.Spec[onceIn, probeOut]{
		Name: "probe.once", Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		Resolve: func(_ context.Context, _ dbq.Querier, in onceIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course"}, nil
		},
		Validate: func(_ context.Context, _ dbq.Querier, _ *domain.Member, in onceIn) error {
			return lose("validate", in)
		},
		Pin: func(_ context.Context, _ dbq.Querier, _ time.Time, in onceIn) (onceIn, error) {
			return in, lose("pin", in)
		},
		Execute: func(_ context.Context, _ *tool.ExecCtx, in onceIn) (probeOut, error) {
			return probeOut{OK: true}, lose("execute", in)
		},
	}))
	for _, tc := range []struct {
		where  string
		caller uuid.UUID
		want   domain.ActionStatus
	}{
		{"validate", c.Sato, domain.StatusExecuted},
		{"execute", c.Sato, domain.StatusExecuted},
		{"pin", c.Grader, domain.StatusProposed}, // only a proposal is pinned
	} {
		out, err := c.Call(tc.caller, "probe.once", m{"course_id": c.Course, "lose_in": tc.where}, tc.where)
		if err != nil || out.Status != tc.want || out.Replayed || lost[tc.where] != 1 {
			t.Fatalf("losing a deadlock in %s: %+v %v (lost %d)", tc.where, out, err, lost[tc.where])
		}
		if n := c.Count(`SELECT count(*) FROM action WHERE idempotency_key = $1 AND status = $2`, tc.where, string(tc.want)); n != 1 {
			t.Fatalf("losing a deadlock in %s: %d actions recorded as %s, want the one", tc.where, n, tc.want)
		}
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 3 {
		t.Fatalf("%d actions recorded, want one a call", n)
	}
}
