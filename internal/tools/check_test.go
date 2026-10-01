package tools_test

import (
	"strings"
	"testing"
	"time"

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
	// Posted, it writes Yuki's totals down, which may then be overridden.
	midtermGrade := testkit.Result[tools.GradeSubmitOut](t, reviewed).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{midtermGrade}})
	notes := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Notes", "body_md": "Read chapter 1."})).DocumentID
	conv, question := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	newcomer := b.person(t, "Mori", "")
	// What an observer reads, at the level Tanaka holds it: she gives no
	// more than that.
	waitsToRead := m{"document_read": "confirm_required", "member_read": "confirm_required"}
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
			valid:   in(m{"actor_id": newcomer, "preset": "observer", "perms": waitsToRead}),
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
			valid:   in(m{"member_id": b.kenM, "perms": m{"document_read": "confirm_required"}}),
			invalid: in(m{"member_id": b.kenM, "perms": m{}}),
			message: "perms is empty: nothing to change"},
		{tool: "member.update_perms",
			invalid: in(m{"member_id": b.kenM, "perms": m{"grade_everything": "autonomous"}}),
			message: `there is no permission named "grade_everything"`},
		{tool: "member.update_perms_bulk",
			valid:   in(m{"role": "student", "perms": m{"document_read": "confirm_required"}}),
			invalid: in(m{"role": "dean", "perms": m{"document_read": "autonomous"}}),
			message: "role must be student, instructor, ta, observer or assistant"},
		{tool: "member.add",
			invalid: in(m{"actor_id": newcomer, "preset": "observer", "student_scope": "all", "listed_students": []uuid.UUID{b.yukiM}}),
			message: "listed_students only makes sense with student_scope = listed"},
		{tool: "member.rescope",
			invalid: in(m{"member_id": b.kenM, "expires_at": time.Now().Add(48 * time.Hour), "clear_expiry": true}),
			message: "give expires_at or clear_expiry, not both"},
		{tool: "member.rescope",
			invalid: in(m{"member_id": b.graderM, "assignment_scope": "all", "listed_assignments": []uuid.UUID{b.hw3}}),
			message: "listed_assignments only makes sense with assignment_scope = listed"},
		{tool: "member.update_perms_bulk",
			invalid: in(m{"role": "student", "perms": m{}}),
			message: "perms is empty: nothing to change"},
		{tool: "conversation.close",
			invalid: in(m{"conversation_id": conv, "reason": tools.ClosedWithAPerson}),
			message: `"` + tools.ClosedWithAPerson + `" is what closing the conversations people were asked in says; give another reason`},
		{tool: "conversation.mark_read",
			invalid: in(m{"conversation_id": conv, "up_to_message_id": question, "up_to": time.Now()}),
			message: "give up_to_message_id or up_to, not both"},
		{tool: "course.update_details",
			invalid: in(m{"title": "  "}),
			message: "title cannot be empty"},
		{tool: "document.update",
			invalid: in(m{"document_id": notes}),
			message: "give title, sort_order or both"},
		{tool: "document.purge",
			invalid: in(m{"document_id": notes, "reason": " "}),
			message: "reason is 1 to 500 characters"},
		{tool: "grade.submit",
			valid:   in(m{"submission_id": kenWork, "score": 75}),
			invalid: in(m{"submission_id": kenWork, "score": -1}),
			message: "score cannot be negative"},
		{tool: "grade.submit",
			invalid: in(m{"submission_id": kenWork, "score": 5, "breakdown": []m{{"criterion": "Thesis", "points": -1, "max": 5}}}),
			message: "breakdown points cannot be negative"},
		{tool: "grade.submit",
			invalid: in(m{"submission_id": kenWork, "score": 5, "no_rubric": true, "rubric_version_id": uuid.New()}),
			message: "give rubric_version_id or no_rubric, not both"},
		{tool: "grade.submit",
			invalid: in(m{"component_id": b.midterm, "student_member_id": b.kenM, "score": 5, "for_missing": true}),
			message: "for_missing is for a grade on a submission"},
		{tool: "grade.regrade",
			valid:   in(m{"grade_id": midtermGrade, "score": 85}),
			invalid: in(m{"grade_id": midtermGrade, "score": -5}),
			message: "score cannot be negative"},
		// The platform's own tools: the same before anyone is asked whether
		// the caller may make them.
		{tool: "actor.register",
			invalid: m{"kind": "robot", "display_name": "Robo"},
			message: "kind must be human or agent"},
		{tool: "actor.register",
			invalid: m{"kind": "human", "display_name": "   "},
			message: "display_name is required"},
		{tool: "actor.register",
			invalid: m{"kind": "agent", "display_name": "Helper"},
			message: "hosting is required for an agent: runtime or mcp; it never changes"},
		{tool: "actor.register",
			invalid: m{"kind": "human", "display_name": "Hana", "owner_actor_id": b.sato},
			message: "only an agent has an owner"},
		{tool: "actor.update",
			invalid: m{"actor_id": newcomer},
			message: "give display_name, email, login_id or more than one"},
		{tool: "actor.update",
			invalid: m{"actor_id": newcomer, "display_name": " "},
			message: "display_name cannot be empty"},
		{tool: "course.create",
			invalid: m{"dept_id": b.dept, "term_id": b.term, "code": "CS102", "title": " "},
			message: "code and title are required"},
		{tool: "course.update",
			invalid: m{"course_id": b.course, "title": ""},
			message: "title cannot be empty"},
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

// What the course says of a call, or the caller's seat, is asked before it
// is proposed too (tool.Spec.Validate), and what the moment says as it is
// proposed (tool.Spec.Pin): a call that approving would refuse is recorded
// failed, with the error approving it would give, and nobody is asked to
// approve it.
func TestACallTheCourseWouldRefuseIsNotProposed(t *testing.T) {
	b := build(t)

	// Tanaka teaches the course too, every write of hers waiting for a
	// confirmation.
	tanaka := b.person(t, "Tanaka", "")
	waits := m{}
	for _, p := range domain.AllPerms {
		if p != domain.PermConversationAnswer {
			waits[string(p)] = "confirm_required"
		}
	}
	tanakaM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor", "perms": waits})).MemberID

	// Ken's HW3 is graded, and Yuki's total is written down; Ken has none.
	kenWork := b.submit(t, b.ken, "Ken's essay")
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70})
	midterm := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{midterm}})
	notes := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Notes", "body_md": "Read chapter 1."})).DocumentID
	conv, _ := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	newcomer := b.person(t, "Mori", "")
	bot := b.agent(t, b.sato, "Sato's marker")
	botM := b.delegate(t, b.sato, bot, m{"preset": "ta"})
	// What Tanaka may give of an observer's.
	reads := m{"document_read": "confirm_required", "member_read": "confirm_required"}
	past := time.Now().Add(-48 * time.Hour)
	in := func(args m) m {
		args["course_id"] = b.course
		return args
	}

	cases := []struct {
		tool    string
		args    m
		code    apperr.Code
		message string
	}{
		{"assignment.create", in(m{"title": "HW5", "points_possible": 10, "component_id": b.midterm}),
			apperr.FailedPrecondition, `"Midterm" is graded directly; assignments hang from a bucket`},
		{"assignment.create", in(m{"title": "HW5", "points_possible": 10, "instructions_document_id": notes}),
			apperr.FailedPrecondition, "that document is of kind material, not instructions"},
		{"assignment.update", in(m{"assignment_id": b.hw3, "points_possible": 50}),
			apperr.FailedPrecondition, "grades have been entered for this assignment; say what becomes of them when its points change: existing_grades rescale or keep_scores"},
		{"component.create", in(m{"parent_id": b.midterm, "name": "Part A"}),
			apperr.FailedPrecondition, `"Midterm" is graded directly and cannot have sub-components`},
		{"component.update", in(m{"component_id": b.bucket, "points_possible": 10}),
			apperr.FailedPrecondition, `"Assignments" holds assignments and cannot be graded directly`},
		{"component.update", in(m{"component_id": b.midterm, "points_possible": 50}),
			apperr.FailedPrecondition, `grades have been entered for "Midterm"; say what becomes of them when its points change: existing_grades rescale or keep_scores`},
		{"component.move", in(m{"component_id": b.bucket, "new_parent_id": b.midterm}),
			apperr.FailedPrecondition, `"Midterm" is graded directly and cannot have sub-components`},
		{"component.move", in(m{"component_id": b.total, "new_parent_id": b.bucket}),
			apperr.FailedPrecondition, "the course total is the root and stays there"},
		{"grade.override_total", in(m{"student_member_id": b.kenM, "component_id": b.total, "score": 90, "reason": "Illness"}),
			apperr.FailedPrecondition, "no total has been written down here for this student yet: one is written when a grade beneath it is posted"},
		{"grade.override_total", in(m{"student_member_id": b.yukiM, "component_id": b.midterm, "score": 90, "reason": "Illness"}),
			apperr.FailedPrecondition, `"Midterm" is graded directly: regrade its grade instead`},
		{"grade.clear_override", in(m{"student_member_id": b.kenM, "component_id": b.total}),
			apperr.FailedPrecondition, "no total has been written down here for this student yet: one is written when a grade beneath it is posted"},
		{"grade.comment_total", in(m{"student_member_id": b.kenM, "component_id": b.total, "feedback": "Well done."}),
			apperr.FailedPrecondition, "no total has been written down here for this student yet: one is written when a grade beneath it is posted"},
		{"member.add", in(m{"actor_id": newcomer, "preset": "observer"}),
			apperr.Forbidden, "you hold document_read at confirm_required and cannot grant it at autonomous"},
		{"member.add", in(m{"actor_id": newcomer, "preset": "observer", "perms": reads, "listed_students": []uuid.UUID{b.yukiM}}),
			apperr.InvalidArgument, "listed_students only makes sense with student_scope = listed"},
		{"member.add", in(m{"actor_id": newcomer, "preset": "observer", "perms": reads, "expires_at": past}),
			apperr.InvalidArgument, "expires_at is in the past"},
		// Raising a level is measured on the whole of what the seat will
		// hold: Ken reads at autonomous, which Tanaka gives nobody.
		{"member.update_perms", in(m{"member_id": b.kenM, "perms": m{"grade_post": "autonomous"}}),
			apperr.Forbidden, "you hold document_read at confirm_required and cannot grant it at autonomous"},
		{"member.update_perms_bulk", in(m{"role": "student", "perms": m{"grade_post": "autonomous"}}),
			apperr.Forbidden, "you hold document_read at confirm_required and cannot grant it at autonomous"},
		{"member.rescope", in(m{"member_id": b.graderM, "listed_students": []uuid.UUID{b.yukiM}}),
			apperr.InvalidArgument, "listed_students only makes sense with student_scope = listed"},
		{"member.rescope", in(m{"member_id": b.kenM, "expires_at": past}),
			apperr.InvalidArgument, "expires_at is in the past; to end a membership now, remove it"},
		{"member.set_role", in(m{"member_id": botM, "role": "ta"}),
			apperr.FailedPrecondition, "a delegate's seat is its principal's agent, always assistant, and on no roster"},
		{"member.pause", in(m{"member_id": tanakaM}),
			apperr.Forbidden, "not on your own membership"},
		{"member.resume", in(m{"member_id": b.kenM}),
			apperr.Conflict, "the member is active, not paused"},
		{"member.remove", in(m{"member_id": tanakaM}),
			apperr.Forbidden, "not on your own membership"},
		{"conversation.mark_read", in(m{"conversation_id": conv, "up_to_message_id": uuid.New()}),
			apperr.InvalidArgument, "up_to_message_id must name a message of this conversation's"},
	}
	for i, tc := range cases {
		out := b.MustCall(tanaka, tc.tool, tc.args, "refuse-"+uuid.NewString())
		if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != tc.code || out.Error.Message != tc.message {
			t.Errorf("case %d, %s: %+v, want failed, %s %q, before anyone is asked", i, tc.tool, out, tc.code, tc.message)
		}
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND status = 'proposed'`, tanaka); n != 0 {
		t.Fatalf("%d of Tanaka's calls wait for someone to approve them", n)
	}
}
