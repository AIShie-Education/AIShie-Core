package tools_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/members"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// What a call's arguments say alone is checked as they are decoded
// (tool.Spec.Check): a call they make invalid is refused at once, with the
// error its tool has always given, and nothing is recorded, whoever makes it.
// Someone whose every write waits for a confirmation is not queued a
// proposal nobody could carry out, and hears what is wrong as someone whose
// writes run at once does.
func TestArgumentsAreCheckedBeforeAnythingIsProposed(t *testing.T) {
	b := build(t)

	// Tanaka teaches the course too, every write of hers waiting for a
	// confirmation; Sato's run at once.
	tanaka := b.person(t, "Tanaka", "")
	waits := m{}
	for _, p := range domain.AllPerms {
		if p != domain.PermConversationAnswer { // no person answers
			waits[string(p)] = "confirm_required"
		}
	}
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor", "perms": waits})

	// What the calls are about.
	kenWork := b.submit(t, b.ken, "Ken's essay")
	graded := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "grader")
	if graded.Status != domain.StatusProposed {
		t.Fatalf("the grader's grade: %+v", graded)
	}
	reviewed := b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 80})
	notes := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Notes", "body_md": "Read chapter 1."})).DocumentID
	conv, question := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	newcomer := b.person(t, "Mori", "")
	in := func(args m) m {
		args["course_id"] = b.course
		return args
	}

	cases := []struct {
		tool string
		// valid is a call Tanaka may only propose, which shows the course
		// holds what invalid is about; wantValid is what becomes of it.
		valid     m
		wantValid domain.ActionStatus
		invalid   m
		message   string
	}{
		{tool: "action.decide",
			valid:   in(m{"action_id": graded.ActionID, "decision": "reject"}),
			invalid: in(m{"action_id": graded.ActionID, "decision": "maybe"}),
			message: `decision must be "approve" or "reject"`},
		{tool: "action.review",
			valid:   in(m{"action_id": reviewed.ActionID, "outcome": "reviewed"}),
			invalid: in(m{"action_id": reviewed.ActionID, "outcome": "fine"}),
			message: `outcome must be "reviewed" or "escalated"`},
		{tool: "assignment.create",
			valid:   in(m{"title": "HW5", "points_possible": 10}),
			invalid: in(m{"title": "HW5", "points_possible": -10}),
			message: "points_possible cannot be negative"},
		{tool: "assignment.create",
			invalid: in(m{"title": "HW5"}),
			message: "title and points_possible are required"},
		{tool: "assignment.update",
			valid:   in(m{"assignment_id": b.hw3, "title": "Homework 3"}),
			invalid: in(m{"assignment_id": b.hw3, "title": "   "}),
			message: "title is required"},
		{tool: "component.create",
			valid:   in(m{"parent_id": b.total, "name": "Quizzes"}),
			invalid: in(m{"parent_id": b.total, "name": "Quizzes", "weight": -1}),
			message: "weight cannot be negative"},
		{tool: "component.update",
			valid:   in(m{"component_id": b.midterm, "name": "Mid-term"}),
			invalid: in(m{"component_id": b.midterm, "points_possible": 50, "clear_points_possible": true}),
			message: "give points_possible or clear_points_possible, not both"},
		{tool: "component.update",
			invalid: in(m{"component_id": b.midterm, "drop_lowest": -1}),
			message: "drop_lowest cannot be negative"},
		{tool: "conversation.close",
			valid:   in(m{"conversation_id": conv, "reason": "Answered."}),
			invalid: in(m{"conversation_id": conv, "reason": members.ConversationSeatRemoved}),
			message: `"seat_removed" is what closing a removed seat's conversations says; give another reason`},
		{tool: "conversation.retract",
			valid:   in(m{"message_id": question}),
			invalid: in(m{"message_id": question, "reason": strings.Repeat("x", 501)}),
			message: "the reason is longer than 500 characters"},
		{tool: "course.join_link_create",
			// A link is made by no proposal: the call is refused when it
			// would be queued, and recorded so.
			valid: in(m{}), wantValid: domain.StatusFailed,
			invalid: in(m{"max_uses": 0}),
			message: "max_uses must be from 1 to 10000"},
		{tool: "course.update_details",
			valid:   in(m{"title": "Computing, an introduction"}),
			invalid: in(m{}),
			message: "give title, description or both"},
		{tool: "document.add_version",
			valid:   in(m{"document_id": notes, "body_md": "Read chapters 1 and 2."}),
			invalid: in(m{"document_id": notes}),
			message: "a version needs content: body_md or files"},
		{tool: "document.create",
			valid:   in(m{"kind": "material", "title": "Slides"}),
			invalid: in(m{"kind": "material", "title": " "}),
			message: "title is required"},
		{tool: "document.create",
			invalid: in(m{"kind": "submission", "title": "Essay", "submission_id": kenWork}),
			message: "a submission file has one version and needs its content now: body_md or files"},
		{tool: "document.update",
			valid:   in(m{"document_id": notes, "sort_order": 2}),
			invalid: in(m{"document_id": notes, "title": ""}),
			message: "title cannot be empty"},
		{tool: "grade.override_total",
			valid:   in(m{"student_member_id": b.yukiM, "component_id": b.total, "score": 90, "reason": "Illness"}),
			invalid: in(m{"student_member_id": b.yukiM, "component_id": b.total, "score": -1, "reason": "Illness"}),
			message: "score cannot be negative"},
		{tool: "grade.override_total",
			invalid: in(m{"student_member_id": b.yukiM, "component_id": b.total, "score": 90, "reason": "  "}),
			message: "reason is 1 to 500 characters"},
		{tool: "member.add",
			valid:   in(m{"actor_id": newcomer, "preset": "observer"}),
			invalid: in(m{"actor_id": newcomer, "preset": "observer", "role": "dean"}),
			message: "role or scope is not one of the allowed values"},
		{tool: "member.add",
			invalid: in(m{"actor_id": newcomer, "preset": "observer", "perms": m{"grade_submit": "sometimes"}}),
			message: `"sometimes" is not a level; use denied, confirm_required, pending_review or autonomous`},
		{tool: "member.add",
			invalid: in(m{"actor_id": newcomer}),
			message: "give exactly one of preset and preset_id"},
		{tool: "member.rescope",
			valid:   in(m{"member_id": b.kenM, "assignment_scope": "all"}),
			invalid: in(m{"member_id": b.kenM, "assignment_scope": "some"}),
			message: "a scope is all or listed"},
		{tool: "member.set_role",
			valid:   in(m{"member_id": b.kenM, "role": "student"}),
			invalid: in(m{"member_id": b.kenM, "role": "dean"}),
			message: "role must be student, instructor, ta, observer or assistant"},
		{tool: "member.update_perms",
			valid:   in(m{"member_id": b.kenM, "perms": m{"document_read": "autonomous"}}),
			invalid: in(m{"member_id": b.kenM, "perms": m{}}),
			message: "perms is empty: nothing to change"},
		{tool: "member.update_perms",
			invalid: in(m{"member_id": b.kenM, "perms": m{"grade_everything": "autonomous"}}),
			message: `there is no permission named "grade_everything"`},
		{tool: "member.update_perms_bulk",
			valid:   in(m{"role": "student", "perms": m{"document_read": "autonomous"}}),
			invalid: in(m{"role": "dean", "perms": m{"document_read": "autonomous"}}),
			message: "role must be student, instructor, ta, observer or assistant"},
		{tool: "submission.set_lateness",
			valid:   in(m{"submission_id": kenWork, "state": "late"}),
			invalid: in(m{"submission_id": kenWork, "state": "early"}),
			message: "state must be submitted or late"},
	}

	// Every tool with a Check is here, so that one added later is too.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.tool] = true
	}
	for _, tl := range b.P.Registry().All() {
		if tl.Check != nil && !covered[tl.Name] {
			t.Errorf("%s checks its arguments, and has no case here", tl.Name)
		}
	}

	for i, tc := range cases {
		key := func(who string) string { return "check-" + who + "-" + tc.tool + "-" + uuid.NewString() }
		if tc.valid != nil {
			want := tc.wantValid
			if want == "" {
				want = domain.StatusProposed
			}
			if out := b.MustCall(tanaka, tc.tool, tc.valid, key("valid")); out.Status != want {
				t.Errorf("case %d, %s: Tanaka's valid call: %+v, want %s", i, tc.tool, out, want)
			}
		}
		for who, actor := range map[string]uuid.UUID{"tanaka": tanaka, "sato": b.sato} {
			k := key(who)
			out, err := b.Call(actor, tc.tool, tc.invalid, k)
			e, ok := apperr.As(err)
			if !ok || e.Code != apperr.InvalidArgument || e.Message != tc.message {
				t.Errorf("case %d, %s as %s: %+v %v, want %s %q before anything is attempted", i, tc.tool, who, out, err,
					apperr.InvalidArgument, tc.message)
			}
			if n := b.Count(`SELECT count(*) FROM action WHERE idempotency_key = $1`, k); n != 0 {
				t.Errorf("case %d, %s as %s: %d actions recorded", i, tc.tool, who, n)
			}
		}
	}
	// Nothing Tanaka asked for wrongly waits for anyone.
	if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND idempotency_key LIKE 'check-tanaka-%'`, tanaka); n != 0 {
		t.Fatalf("%d of Tanaka's invalid calls are recorded", n)
	}
}

// An agent's owner decides its proposal where they could have made it
// themselves without anyone's confirmation, and could not have made one
// that would be refused: its queue says yours_to_decide false once
// approving it would be refused for what it asks, and deciding it is refused
// so, while someone else may still reject it and the owner may take it back.
func TestAProposalThatWouldBeRefusedIsNotItsOwnersToDecide(t *testing.T) {
	b := build(t)
	kenWork := b.submit(t, b.ken, "Ken's essay")
	bot := b.agent(t, b.sato, "Sato's marker")
	b.delegate(t, b.sato, bot, m{"preset": "ta", "perms": m{"grade_submit": "confirm_required", "grade_post": "confirm_required"}})
	tanaka := b.person(t, "Tanaka", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor"})

	proposed := b.MustCall(bot, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 90}, "bot-grades")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent grading: %+v", proposed)
	}
	// Ito teaches it too, and decides nothing but his own agent's
	// proposals; his agent grades Yuki's HW3 by proposal.
	ito := b.person(t, "Ito", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ito, "preset": "instructor", "perms": m{"action_decide": "denied"}})
	itoBot := b.agent(t, ito, "Ito's marker")
	b.delegate(t, ito, itoBot, m{"preset": "ta", "perms": m{"grade_submit": "confirm_required"}})
	yukiWork := b.submit(t, b.yuki, "Yuki's essay")
	itoProposed := b.MustCall(itoBot, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 90}, "ito-bot-grades")
	if itoProposed.Status != domain.StatusProposed {
		t.Fatalf("Ito's agent grading: %+v", itoProposed)
	}
	if q := b.queue(t, ito, "action.list_proposed"); !q[*itoProposed.ActionID] {
		t.Fatalf("Ito's queue, a grade he could give himself: %v", q)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*proposed.ActionID] {
		t.Fatalf("Sato's queue, a grade he could give himself: %v", q)
	}

	// HW3 is worth 50 now: a grade proposed out of 100 is refused when it is
	// approved (grade.submit's Validate), and so is not Sato's to decide.
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "points_possible": 50})
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*proposed.ActionID] {
		t.Fatalf("Sato's queue, a grade out of points the work is no longer worth: %v", q)
	}
	if q := b.queue(t, tanaka, "action.list_proposed"); !q[*proposed.ActionID] {
		t.Fatalf("Tanaka's queue: %v", q)
	}
	for _, decision := range []string{"approve", "reject"} {
		out := b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": decision}, "sato-"+decision)
		refusal, _ := out.Error.Details["refusal"].(*apperr.Error)
		if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["reason"] != "owner_would_be_refused" ||
			refusal == nil || refusal.Code != apperr.FailedPrecondition {
			t.Fatalf("Sato deciding (%s) his agent's grade that would be refused: %+v", decision, out)
		}
	}
	// Ito, who decides nothing else, is refused it as anyone without
	// action_decide is.
	if q := b.queue(t, ito, "action.list_proposed"); q[*itoProposed.ActionID] {
		t.Fatalf("Ito's queue, a grade out of points the work is no longer worth: %v", q)
	}
	for _, decision := range []string{"approve", "reject"} {
		deniedOutright(t, "Ito deciding ("+decision+") his agent's grade that would be refused",
			b.MustCall(ito, "action.decide", m{"course_id": b.course, "action_id": itoProposed.ActionID, "decision": decision}, "ito-"+decision))
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'proposed'`, *proposed.ActionID); n != 1 {
		t.Fatal("the proposal is no longer waiting")
	}

	// A proposal queued before its tool checked what its arguments say
	// alone is not his either, and approving it fails as Execute would
	// have failed it. Yuki has a total to override once her midterm is
	// posted.
	midterm := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{midterm}})
	override := b.MustCall(bot, "grade.override_total", m{"course_id": b.course, "student_member_id": b.yukiM,
		"component_id": b.total, "score": 90, "reason": "Illness"}, "bot-overrides")
	if override.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent overriding a total: %+v", override)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*override.ActionID] {
		t.Fatalf("Sato's queue, an override he could make himself: %v", q)
	}
	b.Exec(`UPDATE action SET payload = jsonb_set(payload, '{score}', '-1') WHERE id = $1`, *override.ActionID)
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*override.ActionID] {
		t.Fatalf("Sato's queue, an override below zero: %v", q)
	}
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, tanaka, "action.decide", m{"course_id": b.course, "action_id": override.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusFailed || d.Error == nil || d.Error.Code != apperr.InvalidArgument || d.Error.Message != "score cannot be negative" {
		t.Fatalf("Tanaka approving an override below zero: %+v", d)
	}

	// The grades: each owner takes his back, as an owner may whatever it
	// asks.
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": proposed.ActionID})
	b.do(t, ito, "action.withdraw", m{"course_id": b.course, "action_id": itoProposed.ActionID})
}
