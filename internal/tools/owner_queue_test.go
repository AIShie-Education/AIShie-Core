package tools_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// An agent's owner decides, reviews and reads their own agent's actions
// whatever action_decide they hold, deciding and reviewing where they could
// have done the same themselves without anyone's confirmation: a student
// confirms her own agent's drafts of her work. Holding no action_decide,
// they reach their own agents' actions and nothing else, and are denied the
// rest as anyone without it is.

// deniedOutright insists a call was denied at the gate, as a call without
// the permission is: status denied, reason permission_denied.
func deniedOutright(t *testing.T, what string, out pipeline.Outcome) {
	t.Helper()
	if out.Status != domain.StatusDenied || out.Error == nil || out.Error.Details["reason"] != "permission_denied" {
		t.Fatalf("%s: %+v, want denied (permission_denied)", what, out)
	}
}

// queue is a queue as actor reads it, by action id, with yours_to_decide.
func (b *built) queue(t *testing.T, actor uuid.UUID, name string) map[uuid.UUID]bool {
	t.Helper()
	out := map[uuid.UUID]bool{}
	for _, a := range testkit.Result[tools.ActionListOut](t, b.do(t, actor, name, m{"course_id": b.course})).Actions {
		out[a.ID] = a.YoursToDecide != nil && *a.YoursToDecide
	}
	return out
}

func TestAStudentConfirmsHerOwnAgentsWork(t *testing.T) {
	b := build(t)
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw4})
	kenWork := b.submit(t, b.ken, "Ken's essay")
	graded := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "grader")

	// Ken has no agent here: the queues are no more his than before.
	deniedOutright(t, "Ken, with no agent, reading the approval queue", b.MustCall(b.ken, "action.list_proposed", m{"course_id": b.course}, ""))
	deniedOutright(t, "Ken reading the grader's proposal", b.MustCall(b.ken, "action.get", m{"course_id": b.course, "action_id": graded.ActionID}, ""))

	// Yuki's agent, seated with the student preset, drafts her HW3 only by
	// proposal; Ken's drafts his HW4 so.
	yukiBot := b.agent(t, b.yuki, "Yuki's helper")
	b.delegate(t, b.yuki, yukiBot, m{"preset": "student"})
	kenBot := b.agent(t, b.ken, "Ken's helper")
	b.delegate(t, b.ken, kenBot, m{"preset": "student"})
	draft := func(bot, student, assignment uuid.UUID, key string) *uuid.UUID {
		t.Helper()
		out := b.MustCall(bot, "submission.create", m{"course_id": b.course, "assignment_id": assignment, "student_member_id": student, "body": "A draft."}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("%s: %+v", key, out)
		}
		return out.ActionID
	}
	yukiDraft := draft(yukiBot, b.yukiM, b.hw3, "yuki-draft")
	kenDraft := draft(kenBot, b.kenM, hw4, "ken-draft")

	// Her queue is her agent's proposal and nothing else, hers to decide;
	// Sato's is the whole course's.
	if q := b.queue(t, b.yuki, "action.list_proposed"); len(q) != 1 || !q[*yukiDraft] {
		t.Fatalf("Yuki's approval queue: %v", q)
	}
	if q := b.queue(t, b.ken, "action.list_proposed"); len(q) != 1 || !q[*kenDraft] {
		t.Fatalf("Ken's approval queue: %v", q)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); len(q) != 3 {
		t.Fatalf("Sato's approval queue: %v", q)
	}
	b.do(t, b.yuki, "action.get", m{"course_id": b.course, "action_id": yukiDraft})
	for what, id := range map[string]*uuid.UUID{"Ken's agent's": kenDraft, "the grader's": graded.ActionID, "no": ptr(uuid.New())} {
		deniedOutright(t, "Yuki reading "+what+" proposal", b.MustCall(b.yuki, "action.get", m{"course_id": b.course, "action_id": id}, ""))
		deniedOutright(t, "Yuki deciding "+what+" proposal", b.MustCall(b.yuki, "action.decide",
			m{"course_id": b.course, "action_id": id, "decision": "approve"}, "yuki-decides-"+id.String()))
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND action_type = 'action.decide' AND status = 'denied'`, b.yuki); n != 3 {
		t.Fatalf("%d of Yuki's denied decisions are on record, want 3", n)
	}

	// She approves her agent's draft: it is carried out at once, as her
	// own doing, and the record says its owner approved it.
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.yuki, "action.decide", m{"course_id": b.course, "action_id": yukiDraft, "decision": "approve"}))
	if d.Outcome != domain.StatusExecuted || !d.ByOwner {
		t.Fatalf("Yuki approving her agent's draft: %+v", d)
	}
	var sub tools.SubmissionCreateOut
	if err := json.Unmarshal(d.Result, &sub); err != nil || b.Count(`SELECT count(*) FROM submission WHERE id = $1 AND student_member_id = $2`, sub.SubmissionID, b.yukiM) != 1 {
		t.Fatalf("the draft was not written: %s %v", d.Result, err)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND action_type = 'action.decide' AND status = 'executed' AND authz_result = 'autonomous'`, b.yuki); n != 1 {
		t.Fatal("Yuki's approval is not on record as her own, at autonomous")
	}

	// She rejects its hand-in likewise.
	handIn := b.MustCall(yukiBot, "submission.submit", m{"course_id": b.course, "submission_id": sub.SubmissionID}, "yuki-submit")
	if handIn.Status != domain.StatusProposed {
		t.Fatalf("the agent handing in Yuki's work: %+v", handIn)
	}
	if d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.yuki, "action.decide", m{"course_id": b.course, "action_id": handIn.ActionID,
		"decision": "reject", "reason": "not yet"})); d.Outcome != domain.StatusRejected || !d.ByOwner {
		t.Fatalf("Yuki rejecting her agent's hand-in: %+v", d)
	}

	// Where she could not do it herself without a confirmation, it is not
	// hers to decide: her queue says so, and deciding it is denied.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"submission_write": "confirm_required"}})
	again := b.MustCall(yukiBot, "submission.submit", m{"course_id": b.course, "submission_id": sub.SubmissionID}, "yuki-submit-again")
	if again.Status != domain.StatusProposed {
		t.Fatalf("the agent handing in again: %+v", again)
	}
	if q := b.queue(t, b.yuki, "action.list_proposed"); len(q) != 1 || q[*again.ActionID] {
		t.Fatalf("Yuki's queue, her own writes waiting for a confirmation: %v", q)
	}
	deniedOutright(t, "Yuki approving what she could not do on her own", b.MustCall(b.yuki, "action.decide",
		m{"course_id": b.course, "action_id": again.ActionID, "decision": "approve"}, "yuki-again"))
	deniedOutright(t, "Yuki rejecting it", b.MustCall(b.yuki, "action.decide",
		m{"course_id": b.course, "action_id": again.ActionID, "decision": "reject"}, "yuki-again-reject"))
	b.do(t, b.yuki, "action.get", m{"course_id": b.course, "action_id": again.ActionID})
}

// Reviewing is the same: an owner who manages the members, and so whose
// agent may act under review, reviews what it did without action_decide of
// their own, and nothing else.
func TestAnOwnerWithoutActionDecideReviewsTheirAgentsWork(t *testing.T) {
	b := build(t)
	hana := b.person(t, "Hana", "")
	hanaM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": hana, "preset": "ta",
		"perms": m{"member_manage": "autonomous", "agent_delegate": "autonomous"}})).MemberID
	if v := b.memberView(t, hanaM); v.Perms["action_decide"] != "denied" {
		t.Fatalf("Hana's seat: %+v", v.Perms)
	}
	bot := b.agent(t, hana, "Hana's grader")
	b.delegate(t, hana, bot, m{"preset": "ta", "perms": m{"grade_submit": "pending_review"}})
	kenWork := b.submit(t, b.ken, "Ken's essay")
	graded := b.MustCall(bot, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 75}, "bot-grades")
	if graded.Status != domain.StatusExecuted || graded.ReviewState != domain.ReviewPending {
		t.Fatalf("Hana's agent grading: %+v", graded)
	}
	proposed := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 71}, "grader")

	if q := b.queue(t, hana, "action.list_pending_review"); len(q) != 1 || !q[*graded.ActionID] {
		t.Fatalf("Hana's review queue: %v", q)
	}
	if q := b.queue(t, hana, "action.list_proposed"); len(q) != 0 {
		t.Fatalf("Hana's approval queue: %v", q)
	}
	deniedOutright(t, "Hana reviewing the grader's proposal", b.MustCall(hana, "action.review",
		m{"course_id": b.course, "action_id": proposed.ActionID, "outcome": "reviewed"}, "hana-other"))
	deniedOutright(t, "Hana deciding the grader's proposal", b.MustCall(hana, "action.decide",
		m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}, "hana-other-decide"))
	out := b.do(t, hana, "action.review", m{"course_id": b.course, "action_id": graded.ActionID, "outcome": "reviewed"})
	if r := testkit.Result[pipeline.ReviewOut](t, out); r.ReviewState != domain.ReviewReviewed || !r.ByOwner {
		t.Fatalf("Hana reviewing her agent's grade: %+v", r)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'action.reviewed' AND action_id = $1 AND payload->>'by_owner' = 'true'`, *graded.ActionID); n != 1 {
		t.Fatal("the review does not say the owner made it")
	}
}

func ptr[T any](v T) *T { return &v }
