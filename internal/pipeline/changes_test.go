package pipeline_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// A reviewer sends an agent's grade back for changes, saying what to
// change; nothing of it is carried out, and the agent learns of it as it
// learns of a rejection, reads the note, and proposes again naming the one
// it revises, which the reviewer approves.
func TestChangesAreRequestedAndTheRevisionApproved(t *testing.T) {
	c := testkit.NewCS101(t, 2)
	yuki := c.Students[0]
	decide := func(actor uuid.UUID, action uuid.UUID, decision string, reason any, key string) pipeline.Outcome {
		t.Helper()
		args := m{"course_id": c.Course, "action_id": action, "decision": decision}
		if reason != nil {
			args["reason"] = reason
		}
		return c.MustCall(actor, "action.decide", args, key)
	}

	first := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 85), "yuki-hw3")
	if first.Status != domain.StatusProposed {
		t.Fatalf("the agent's grade: %+v", first)
	}
	proposal := *first.ActionID

	// Asked with no note, or one of nothing but blanks, nothing is recorded.
	for _, reason := range []any{nil, " \n\t "} {
		args := m{"course_id": c.Course, "action_id": proposal, "decision": "request_changes"}
		if reason != nil {
			args["reason"] = reason
		}
		_, err := c.Call(c.Sato, "action.decide", args, "sato-nothing")
		if e, ok := apperr.As(err); !ok || e.Code != apperr.InvalidArgument || e.Details["reason"] != "note_required" {
			t.Fatalf("changes requested saying nothing (%v): %v", reason, err)
		}
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE action_type = 'action.decide'`); n != 0 {
		t.Fatalf("%d decisions recorded for requests that said nothing", n)
	}
	// The agent decides nothing of its own, this no more than the rest.
	if out := decide(c.Grader, proposal, "request_changes", "Be kinder.", "self"); out.Status != domain.StatusDenied {
		t.Fatalf("the agent asking for changes to its own grade: %+v", out)
	}

	const note = "Give each criterion its points: the breakdown is missing."
	out := decide(c.Sato, proposal, "request_changes", "  "+note+"\n", "sato-changes")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("Sato asking for changes: %+v", out)
	}
	if v := testkit.Result[pipeline.DecideOut](t, out); v.Outcome != domain.StatusChangesRequested || v.ActionID != proposal || v.ByOwner {
		t.Fatalf("what became of the proposal: %+v", v)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'changes_requested' AND decided_by_member_id = $2
		AND decided_at IS NOT NULL AND executed_at IS NULL AND result->'decision'->>'decision' = 'request_changes'
		AND result->'decision'->>'reason' = $3 AND result->'decision'->>'by_action_id' = $4`,
		proposal, c.SatoM, note, out.ActionID.String()); n != 1 {
		t.Fatal("the proposal does not say who asked for what, trimmed, by which decision")
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("a grade sent back for changes was written")
	}
	// It is over: nobody decides it again, nor its proposer withdraws it.
	if again := decide(c.Sato, proposal, "approve", nil, "sato-approves-after"); again.Status != domain.StatusFailed ||
		again.Error.Code != apperr.Conflict {
		t.Fatalf("approving a proposal sent back for changes: %+v", again)
	}
	if w := c.MustCall(c.Grader, "action.withdraw", m{"course_id": c.Course, "action_id": proposal}, "withdraw"); w.Status != domain.StatusFailed {
		t.Fatalf("withdrawing a proposal sent back for changes: %+v", w)
	}

	// The agent finds it in its feed, as a rejection, under the proposal,
	// and nobody without action_decide does; the note is in its own list,
	// never in the feed.
	evs := testkit.Result[tools.EventListOut](t, c.MustCall(c.Grader, "event.list", m{"course_id": c.Course}, ""))
	var heard *tools.EventView
	for i, e := range evs.Events {
		if e.Type == "action.changes_requested" {
			heard = &evs.Events[i]
		}
	}
	if heard == nil || heard.ActionID == nil || *heard.ActionID != proposal || *heard.SubjectID != proposal {
		t.Fatalf("the agent's feed: %+v", evs.Events)
	}
	var payload map[string]any
	if err := json.Unmarshal(heard.Payload, &payload); err != nil || payload["action_type"] != "grade.submit" ||
		payload["by_action_id"] != out.ActionID.String() || payload["reason"] != nil {
		t.Fatalf("the event's payload: %s", heard.Payload)
	}
	// A decider, who proposed nothing, is told of it under the proposal too.
	seen := false
	for _, e := range testkit.Result[tools.EventListOut](t, c.MustCall(c.Sato, "event.list", m{"course_id": c.Course}, "")).Events {
		if e.Type == "action.changes_requested" && e.ActionID != nil && *e.ActionID == proposal {
			seen = true
		}
	}
	if !seen {
		t.Fatal("Sato, who holds action_decide, is not told of it")
	}
	for _, e := range testkit.Result[tools.EventListOut](t, c.MustCall(yuki.Actor, "event.list", m{"course_id": c.Course}, "")).Events {
		if e.Type == "action.changes_requested" {
			t.Fatal("Yuki, who decides nothing, is told of it")
		}
	}
	mine := testkit.Result[tools.ActionListOut](t, c.MustCall(c.Grader, "action.list_mine", m{"course_id": c.Course,
		"exclude_types": []string{"action.decide", "action.withdraw"}}, ""))
	var asked struct {
		Decision struct{ Decision, Reason string } `json:"decision"`
	}
	if len(mine.Actions) != 1 || mine.Actions[0].ID != proposal || mine.Actions[0].Status != "changes_requested" ||
		json.Unmarshal(mine.Actions[0].Result, &asked) != nil || asked.Decision.Reason != note || asked.Decision.Decision != "request_changes" {
		t.Fatalf("the agent's own actions: %+v", mine.Actions)
	}
	// Retried, the call says what became of it.
	if replay := c.MustCall(c.Grader, "grade.submit", submitArgs(c, yuki, 85), "yuki-hw3"); !replay.Replayed || replay.Status != domain.StatusChangesRequested {
		t.Fatalf("the first call, retried: %+v", replay)
	}

	// What the agent may not revise: nothing is recorded of any of it.
	notRevisable := func(what string, actor uuid.UUID, tool string, args m, revises uuid.UUID, code apperr.Code) {
		t.Helper()
		before := c.Count(`SELECT count(*) FROM action`)
		_, err := c.CallRevising(actor, tool, args, "bad-"+uuid.NewString(), revises)
		if e, ok := apperr.As(err); !ok || e.Code != code || e.Details["reason"] != "not_revisable" {
			t.Fatalf("%s: %v", what, err)
		}
		if after := c.Count(`SELECT count(*) FROM action`); after != before {
			t.Fatalf("%s: %d actions recorded", what, after-before)
		}
	}
	ken := c.Students[1]
	notRevisable("someone else's proposal", c.Sato, "grade.submit", submitArgs(c, ken, 70), proposal, apperr.InvalidArgument)
	notRevisable("an action that is no proposal of the course's", c.Grader, "grade.submit", submitArgs(c, yuki, 80), uuid.New(), apperr.InvalidArgument)
	notRevisable("a read", c.Grader, "action.list_mine", m{"course_id": c.Course}, proposal, apperr.InvalidArgument)
	rejected := c.MustCall(c.Grader, "grade.submit", submitArgs(c, ken, 10), "ken-hw3")
	decide(c.Sato, *rejected.ActionID, "reject", "No.", "sato-rejects")
	notRevisable("a proposal rejected", c.Grader, "grade.submit", submitArgs(c, ken, 60), *rejected.ActionID, apperr.FailedPrecondition)

	// The revision: proposed as any call, naming the one it revises.
	revised := m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 85, "breakdown": []m{
		{"criterion": "Thesis", "points": 40, "max": 50}, {"criterion": "Evidence", "points": 45, "max": 50}}}
	second, err := c.CallRevising(c.Grader, "grade.submit", revised, "yuki-hw3-2", proposal)
	if err != nil || second.Status != domain.StatusProposed {
		t.Fatalf("the revision: %+v, %v", second, err)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND revises_action_id = $2`, *second.ActionID, proposal); n != 1 {
		t.Fatal("the revision does not name what it revises")
	}
	if n := c.Count(`SELECT count(*) FROM event WHERE type = 'action.proposed' AND action_id = $1 AND payload->>'revises_action_id' = $2`,
		*second.ActionID, proposal.String()); n != 1 {
		t.Fatal("the news of the revision does not name what it revises")
	}
	// A proposal still waiting is not one sent back.
	notRevisable("a proposal still waiting", c.Grader, "grade.submit", submitArgs(c, yuki, 80), *second.ActionID, apperr.FailedPrecondition)
	// Its key is its call's, and what it revises is part of the call.
	if again, err := c.CallRevising(c.Grader, "grade.submit", revised, "yuki-hw3-2", proposal); err != nil || !again.Replayed ||
		*again.ActionID != *second.ActionID {
		t.Fatalf("the revision, retried: %+v, %v", again, err)
	}
	for what, call := range map[string]func() (pipeline.Outcome, error){
		"revising nothing": func() (pipeline.Outcome, error) { return c.Call(c.Grader, "grade.submit", revised, "yuki-hw3-2") },
		"revising another": func() (pipeline.Outcome, error) {
			return c.CallRevising(c.Grader, "grade.submit", revised, "yuki-hw3-2", *rejected.ActionID)
		},
		"the first call, revising": func() (pipeline.Outcome, error) {
			return c.CallRevising(c.Grader, "grade.submit", submitArgs(c, yuki, 85), "yuki-hw3", proposal)
		},
	} {
		if _, err := call(); !isCode(err, apperr.IdempotencyConflict) {
			t.Fatalf("the key, %s: %v", what, err)
		}
	}

	// Sato reads it in his queue, with what it revises, and approves it.
	queue := testkit.Result[tools.ActionListOut](t, c.MustCall(c.Sato, "action.list_proposed", m{"course_id": c.Course}, ""))
	if len(queue.Actions) != 1 || queue.Actions[0].ID != *second.ActionID || queue.Actions[0].RevisesActionID == nil ||
		*queue.Actions[0].RevisesActionID != proposal {
		t.Fatalf("the approval queue: %+v", queue.Actions)
	}
	was := testkit.Result[tools.ActionView](t, c.MustCall(c.Sato, "action.get", m{"course_id": c.Course, "action_id": proposal}, ""))
	if was.Status != "changes_requested" || was.DecidedByMemberID == nil || *was.DecidedByMemberID != c.SatoM {
		t.Fatalf("what the revision revises, as Sato reads it: %+v", was)
	}
	approved := testkit.Result[pipeline.DecideOut](t, decide(c.Sato, *second.ActionID, "approve", nil, "sato-approves"))
	if approved.Outcome != domain.StatusExecuted {
		t.Fatalf("approving the revision: %+v", approved)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE created_by_action_id = $1 AND breakdown IS NOT NULL`, *second.ActionID); n != 1 {
		t.Fatal("the revision's grade was not written as revised")
	}
	// A revision sent back in turn is revised in turn: the chain runs back.
	third := c.MustCall(c.Grader, "grade.submit", submitArgs(c, ken, 65), "ken-hw3-2")
	decide(c.Sato, *third.ActionID, "request_changes", "Ken's essay is longer than that.", "sato-ken")
	fourth, err := c.CallRevising(c.Grader, "grade.submit", submitArgs(c, ken, 75), "ken-hw3-3", *third.ActionID)
	if err != nil || fourth.Status != domain.StatusProposed {
		t.Fatalf("revising a revision's forerunner: %+v, %v", fourth, err)
	}
	decide(c.Sato, *fourth.ActionID, "request_changes", "Closer; say why.", "sato-ken-2")
	if fifth, err := c.CallRevising(c.Grader, "grade.submit", submitArgs(c, ken, 75), "ken-hw3-4", *fourth.ActionID); err != nil ||
		fifth.Status != domain.StatusProposed {
		t.Fatalf("revising a revision: %+v, %v", fifth, err)
	}
}

// The database holds what a revision may name, whatever writes it, and
// what a row revises never changes.
func TestTheDatabaseHoldsWhatARevisionNames(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	first := c.MustCall(c.Grader, "grade.submit", submitArgs(c, c.Students[0], 85), "p")
	insert := func(revises uuid.UUID) error {
		_, err := c.Pool.Exec(t.Context(), `INSERT INTO action (actor_id, course_id, member_id, action_type, target_type,
			payload_hash, idempotency_key, authz_result, status, revises_action_id)
			VALUES ($1, $2, $3, 'grade.submit', 'submission', repeat('0', 64), $4, 'confirm_required', 'proposed', $5)`,
			c.Grader, c.Course, c.GraderM, uuid.NewString(), revises)
		return err
	}
	if err := insert(*first.ActionID); !isSQLState(err, "23514") {
		t.Fatalf("revising a proposal still waiting: %v", err)
	}
	c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": first.ActionID, "decision": "request_changes",
		"reason": "Fix it."}, "d")
	if err := insert(*first.ActionID); err != nil {
		t.Fatalf("revising it once changes were asked for: %v", err)
	}
	if _, err := c.Pool.Exec(t.Context(), `UPDATE action SET revises_action_id = NULL WHERE revises_action_id IS NOT NULL`); !isSQLState(err, "23001") {
		t.Fatalf("changing what a row revises: %v", err)
	}
	if _, err := c.Pool.Exec(t.Context(), `UPDATE action SET result = '{"decision": {"reason": ""}}' WHERE id = $1`, *first.ActionID); !isSQLState(err, "23514") {
		t.Fatalf("a request for changes with an empty note: %v", err)
	}
}

func isCode(err error, code apperr.Code) bool {
	e, ok := apperr.As(err)
	return ok && e.Code == code
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
