package tools_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
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
	helper := b.agent(t, tanaka, "Tanaka's helper")
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
		// Sato's grade is not under review: Tanaka's review of it, which
		// says nothing wrong, is refused as approving it would refuse it
		// (Validate), recorded, where the invalid one is not.
		{tool: "action.review",
			valid: in(m{"action_id": reviewed.ActionID, "outcome": "reviewed"}), wantValid: domain.StatusFailed,
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
		// Tanaka takes no part in Yuki's conversation: her call that says
		// nothing wrong is refused as approving it would refuse it
		// (Validate), recorded, where the invalid one is not.
		{tool: "conversation.close",
			valid: in(m{"conversation_id": conv, "reason": "Answered."}), wantValid: domain.StatusFailed,
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
		{tool: "member.add_delegate",
			valid:   in(m{"actor_id": helper}),
			invalid: in(m{"actor_id": helper, "perms": m{"grade_everything": "autonomous"}}),
			message: `there is no permission named "grade_everything"`},
		{tool: "member.add_delegate",
			invalid: in(m{"actor_id": helper, "perms": m{"document_read": "sometimes"}}),
			message: `"sometimes" is not a level; use denied, confirm_required, pending_review or autonomous`},
		{tool: "member.add_delegate",
			invalid: in(m{"actor_id": helper, "preset": "delegate", "preset_id": uuid.New()}),
			message: "give exactly one of preset and preset_id"},
		{tool: "member.add_delegate",
			invalid: in(m{"actor_id": helper, "student_scope": "some"}),
			message: "a scope is all or listed"},
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
		// A conversation's messages: what they say, and the files they
		// carry, by how many and what they are called. Tanaka may address
		// no agent here, which the course tells (Validate).
		{tool: "conversation.open",
			valid: in(m{"respondent_member_id": b.tutorM, "body": "What is a thesis?"}), wantValid: domain.StatusFailed,
			invalid: in(m{"respondent_member_id": b.tutorM, "body": "  "}),
			message: "the message is empty"},
		{tool: "conversation.open",
			invalid: in(m{"respondent_member_id": b.tutorM, "title": strings.Repeat("t", 201)}),
			message: "the title is longer than 200 characters"},
		{tool: "conversation.open",
			invalid: in(m{"respondent_member_id": b.tutorM, "attachments": []m{{"upload_token": "t", "filename": "a.txt"}}}),
			message: "files come with a message: give body as well as attachments"},
		{tool: "conversation.open",
			invalid: in(m{"respondent_member_id": b.tutorM, "body": "Two?", "attachments": []m{
				{"upload_token": "t", "filename": "a.txt"}, {"upload_token": "t", "filename": "b.txt"}}}),
			message: "the same upload is named twice"},
		{tool: "conversation.ask",
			invalid: in(m{"conversation_id": conv, "body": ""}),
			message: "the message is empty"},
		{tool: "conversation.ask",
			invalid: in(m{"conversation_id": conv, "body": "Look", "attachments": []m{{"upload_token": "t", "filename": "../a.txt"}}}),
			message: "a file's name is 1 to 255 characters on one line, a name and not a path"},
		{tool: "conversation.answer",
			invalid: in(m{"conversation_id": conv, "in_reply_to_message_id": question, "body": strings.Repeat("a", 20001)}),
			message: "the message is 20001 characters long; the most is 20000"},
		// A grade's feedback files, and a post's drafts: a call gives the
		// drafts or the assignment, and a proposal to post the assignment
		// stores both (CheckCall).
		{tool: "grade.submit",
			invalid: in(m{"submission_id": kenWork, "score": 5, "feedback_files": []m{{"title": " ", "upload_token": "t"}}}),
			message: "every feedback file needs a title"},
		{tool: "grade.regrade",
			invalid: in(m{"grade_id": midtermGrade, "score": 85, "feedback_files": []m{
				{"title": "Notes", "upload_token": "t"}, {"title": "More notes", "upload_token": "t"}}}),
			message: "the same upload is listed twice"},
		{tool: "grade.post",
			invalid: in(m{"assignment_id": b.hw3, "grade_ids": []uuid.UUID{midtermGrade}}),
			message: "give exactly one of grade_ids and assignment_id"},
		{tool: "grade.post",
			invalid: in(m{}),
			message: "give exactly one of grade_ids and assignment_id"},
		// A version's files, and a text version's text.
		{tool: "document.create",
			invalid: in(m{"kind": "material", "title": "Slides", "files": []m{
				{"upload_token": "t", "filename": "a.pdf"}, {"upload_token": "t", "filename": "b.pdf"}}}),
			message: "the same upload is named twice"},
		{tool: "document.add_version",
			invalid: in(m{"document_id": notes, "upload_token": "t", "files": []m{{"upload_token": "u", "filename": "a.pdf"}}}),
			message: "give files, or upload_token for one file, not both"},
		{tool: "document.text_update",
			invalid: in(m{"document_id": notes, "version_id": uuid.New(), "body": " \n "}),
			message: "the text is empty"},
		// The site's services: what they write back.
		{tool: "agent_runtime.rendition_complete",
			invalid: m{"rendition_id": uuid.New(), "lease_id": uuid.New(), "status": "done", "page_count": 3},
			message: "done needs upload_token"},
		{tool: "document_text.complete",
			invalid: m{"version_id": uuid.New(), "lease_id": uuid.New(), "status": "failed"},
			message: "reason is 1 to 500 characters"},
	}

	// Every tool with a Check is here, so that one added later is too.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.tool] = true
	}
	for _, tl := range b.P.Registry().All() {
		if (tl.Check != nil || tl.CheckCall != nil) && !covered[tl.Name] {
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
	b.delegate(t, b.sato, bot, m{"preset": "ta", "perms": m{"grade_submit": "confirm_required", "grade_post": "confirm_required",
		"document_write": "confirm_required"}})
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

	// What the course must be for a proposal to be carried out is asked
	// the same way: the agent asks for the notes to be published, and Sato
	// publishes them himself meanwhile, which approving it would refuse.
	notes := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Notes", "body_md": "Read chapter 1."}))
	publishing := b.MustCall(bot, "document.publish", m{"course_id": b.course, "document_id": notes.DocumentID}, "bot-publishes")
	if publishing.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent publishing the notes: %+v", publishing)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*publishing.ActionID] {
		t.Fatalf("Sato's queue, notes he could publish himself: %v", q)
	}
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": notes.DocumentID, "version_id": notes.VersionID})
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*publishing.ActionID] {
		t.Fatalf("Sato's queue, notes published already: %v", q)
	}
	out := b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": publishing.ActionID, "decision": "approve"}, "sato-publishes")
	if refusal, _ := out.Error.Details["refusal"].(*apperr.Error); out.Status != domain.StatusFailed || out.Error.Details["reason"] != "owner_would_be_refused" ||
		refusal == nil || refusal.Code != apperr.Conflict || refusal.Message != "that version is already the published one" {
		t.Fatalf("Sato approving his agent's publishing of notes published already: %+v", out)
	}
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": publishing.ActionID})

	// The grades: each owner takes his back, as an owner may whatever it
	// asks.
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": proposed.ActionID})
	b.do(t, ito, "action.withdraw", m{"course_id": b.course, "action_id": itoProposed.ActionID})
}

// What the moment refuses, and what changes while a proposal waits, is
// asked of it again for its owner as approving it now would ask it
// (tool.Spec.Validate, given the moment, and tool.Spec.Since, given when it
// was proposed): an agent's change of a seat's expiry that has passed
// meanwhile, a change to a seat that has ended since, a grade with a newer
// draft entered since, a post of a draft replaced or posted since, and an agent's
// answer to a question its opener has asked again since, are not their
// owner's to decide. Someone else may still reject them, and the owner take
// them back.
func TestWhatApprovingWouldRefuseNowIsNotItsOwnersToDecide(t *testing.T) {
	b := build(t)
	tanaka := b.person(t, "Tanaka", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor"})
	refusedAs := func(what string, out pipeline.Outcome, code apperr.Code, check func(*apperr.Error) bool) {
		t.Helper()
		refusal, _ := out.Error.Details["refusal"].(*apperr.Error)
		if out.Status != domain.StatusFailed || out.Error.Details["reason"] != "owner_would_be_refused" ||
			refusal == nil || refusal.Code != code || !check(refusal) {
			t.Fatalf("%s: %+v", what, out)
		}
	}

	// Sato's agent manages the course's members for him, by proposal: it
	// asks for Ken's seat to end in two days.
	bot := b.agent(t, b.sato, "Sato's registrar")
	b.delegate(t, b.sato, bot, m{"preset": "ta", "perms": m{"member_manage": "confirm_required",
		"grade_submit": "confirm_required", "grade_post": "confirm_required"}})
	rescope := b.MustCall(bot, "member.rescope", m{"course_id": b.course, "member_id": b.kenM,
		"expires_at": time.Now().Add(48 * time.Hour)}, "bot-rescopes")
	if rescope.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent ending Ken's seat in two days: %+v", rescope)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*rescope.ActionID] {
		t.Fatalf("Sato's queue, an expiry he could set himself: %v", q)
	}
	// The two days pass while it waits.
	b.Exec(`UPDATE action SET payload = jsonb_set(payload, '{expires_at}', to_jsonb($2::text)) WHERE id = $1`,
		*rescope.ActionID, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*rescope.ActionID] {
		t.Fatalf("Sato's queue, an expiry passed while it waited: %v", q)
	}
	if q := b.queue(t, tanaka, "action.list_proposed"); !q[*rescope.ActionID] {
		t.Fatalf("Tanaka's queue: %v", q)
	}
	refusedAs("Sato approving an expiry that has passed",
		b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": rescope.ActionID, "decision": "approve"}, "sato-rescope"),
		apperr.InvalidArgument, func(e *apperr.Error) bool {
			return e.Message == "expires_at is in the past; to end a membership now, remove it"
		})
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": rescope.ActionID})

	// It asks for Ono's seat to read no more, and Ono's seat ends while it
	// waits: the sweep has not been by, and the seat is as good as removed.
	onoM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": b.person(t, "Ono", ""), "preset": "observer"})).MemberID
	narrowing := b.MustCall(bot, "member.update_perms", m{"course_id": b.course, "member_id": onoM,
		"perms": m{"document_read": "denied"}}, "bot-narrows")
	if narrowing.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent narrowing Ono's seat: %+v", narrowing)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*narrowing.ActionID] {
		t.Fatalf("Sato's queue, a seat he could narrow himself: %v", q)
	}
	b.Exec(`UPDATE course_member SET expires_at = now() - interval '1 minute' WHERE id = $1`, onoM)
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*narrowing.ActionID] {
		t.Fatalf("Sato's queue, a seat that ended while it waited: %v", q)
	}
	refusedAs("Sato approving a change to a seat that has ended",
		b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": narrowing.ActionID, "decision": "approve"}, "sato-narrows"),
		apperr.Conflict, func(e *apperr.Error) bool {
			return e.Message == "the member has been removed; seat the actor again for a fresh start"
		})
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": narrowing.ActionID})

	// It grades Ken's essay by proposal, and Sato enters a draft of his own
	// for it meanwhile, which approving the proposal would replace unseen.
	kenWork := b.submit(t, b.ken, "Ken's essay")
	grading := b.MustCall(bot, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "bot-grades")
	if grading.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent grading Ken's essay: %+v", grading)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*grading.ActionID] {
		t.Fatalf("Sato's queue, a grade he could give himself: %v", q)
	}
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 75})
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*grading.ActionID] {
		t.Fatalf("Sato's queue, a grade with a newer draft entered since: %v", q)
	}
	refusedAs("Sato approving a grade with a newer draft entered since",
		b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": grading.ActionID, "decision": "approve"}, "sato-grades"),
		apperr.FailedPrecondition, func(e *apperr.Error) bool { return strings.Contains(e.Message, "a newer draft was entered") })
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": grading.ActionID})

	// It proposes posting Yuki's draft, which Sato replaces meanwhile.
	yukiWork := b.submit(t, b.yuki, "Yuki's essay")
	yukiDraft := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "submission_id": yukiWork, "score": 80})).GradeID
	posting := b.MustCall(bot, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukiDraft}}, "bot-posts")
	if posting.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent posting Yuki's draft: %+v", posting)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*posting.ActionID] {
		t.Fatalf("Sato's queue, a draft he could post himself: %v", q)
	}
	yukiDraft = testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "submission_id": yukiWork, "score": 85})).GradeID
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*posting.ActionID] {
		t.Fatalf("Sato's queue, posting a draft replaced since: %v", q)
	}
	refusedAs("Sato approving the posting of a draft replaced since",
		b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": posting.ActionID, "decision": "approve"}, "sato-posts"),
		apperr.Conflict, func(e *apperr.Error) bool {
			return strings.HasSuffix(e.Message, "has been replaced since it was proposed; propose posting again")
		})
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": posting.ActionID})

	// It proposes posting the new draft, and Sato posts it by hand
	// meanwhile: there is nothing left for the proposal to post.
	posting = b.MustCall(bot, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukiDraft}}, "bot-posts-again")
	if posting.Status != domain.StatusProposed {
		t.Fatalf("Sato's agent posting Yuki's new draft: %+v", posting)
	}
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukiDraft}})
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*posting.ActionID] {
		t.Fatalf("Sato's queue, posting a draft posted since: %v", q)
	}
	refusedAs("Sato approving the posting of a draft posted since",
		b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": posting.ActionID, "decision": "approve"}, "sato-posts-again"),
		apperr.FailedPrecondition, func(e *apperr.Error) bool {
			return strings.Contains(e.Message, "there is nothing left for it to post")
		})
	b.do(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": posting.ActionID})

	// Sato's course tutor answers Ken by proposal; Ken asks again before
	// anyone decides it, and the answer is to a question no longer the
	// latest.
	tutor := b.runtimeAgent(t, b.sato, "Course tutor")
	tutorM := b.delegate(t, b.sato, tutor, m{"preset": "course_tutor", "perms": m{"conversation_answer": "confirm_required"}})
	b.Host(tutor)
	conv, question := b.open(t, b.ken, tutorM, "Is HW3 due on Friday?")
	answer := b.MustCall(tutor, "conversation.answer", answerArgs(b, conv, question, "Yes."), "tutor-answers")
	if answer.Status != domain.StatusProposed {
		t.Fatalf("the tutor's answer: %+v", answer)
	}
	if q := b.queue(t, b.sato, "action.list_proposed"); !q[*answer.ActionID] {
		t.Fatalf("Sato's queue, his tutor's answer: %v", q)
	}
	b.ask(t, b.ken, conv, "Or is it Monday?")
	if q := b.queue(t, b.sato, "action.list_proposed"); q[*answer.ActionID] {
		t.Fatalf("Sato's queue, an answer to a question asked again since: %v", q)
	}
	refusedAs("Sato approving his tutor's answer to a question asked again since",
		b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": answer.ActionID, "decision": "approve"}, "sato-answer"),
		apperr.Conflict, func(e *apperr.Error) bool { return e.Details["reason"] == "moved_on" })
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, tanaka, "action.decide",
		m{"course_id": b.course, "action_id": answer.ActionID, "decision": "reject"}))
	if d.Outcome != domain.StatusRejected {
		t.Fatalf("Tanaka rejecting it: %+v", d)
	}
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
	kenGrading := b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70})
	kenGraded, kenGrade := kenGrading.ActionID, testkit.Result[tools.GradeSubmitOut](t, kenGrading).GradeID
	midterm := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 80})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{midterm}})
	notesDoc := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Notes", "body_md": "Read chapter 1."}))
	notes, notesVersion := notesDoc.DocumentID, *notesDoc.VersionID
	conv, _ := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	newcomer := b.person(t, "Mori", "")
	bot := b.agent(t, b.sato, "Sato's marker")
	botM := b.delegate(t, b.sato, bot, m{"preset": "ta"})
	// What Tanaka may give of an observer's.
	reads := m{"document_read": "confirm_required", "member_read": "confirm_required"}
	past := time.Now().Add(-48 * time.Hour)

	// HW4 is not published, and could not be: its brief is a draft.
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW4", "body_md": "tbd"})).DocumentID
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create",
		m{"course_id": b.course, "title": "HW4", "points_possible": 10, "instructions_document_id": brief})).ID
	// The syllabus is published; last year's notes are archived.
	syllabus := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Syllabus", "body_md": "Week 1."}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": syllabus.DocumentID})
	lastYear := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Last year", "body_md": "Old."}))
	old := lastYear.DocumentID
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": old})
	// Yuki has handed in an essay as a file; Ken has started a second
	// attempt and written nothing in it yet.
	yukiDraft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	essay := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission",
		"title": "essay.txt", "submission_id": yukiDraft, "upload_token": b.upload(t, b.yuki, "submission", "text/plain", []byte("my essay"))})).DocumentID
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": yukiDraft})
	kenDraft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	// Yuki took back a question of hers; Sato revoked a join link.
	_, withdrawn := b.open(t, b.yuki, b.tutorM, "Never mind")
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": withdrawn})
	link := testkit.Result[tools.JoinLinkCreateOut](t, b.do(t, b.sato, "course.join_link_create", m{"course_id": b.course})).LinkID
	b.do(t, b.sato, "course.join_link_revoke", m{"course_id": b.course, "link_id": link})
	// The grader proposes a grade for Yuki's essay, which waits.
	graderProposal := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiDraft, "score": 60}, "grader-yuki")
	if graderProposal.Status != domain.StatusProposed {
		t.Fatalf("the grader's grade: %+v", graderProposal)
	}
	// Sato's handout: its Word file converted, its PDF never.
	handout := b.handout(t, false)
	b.convert(t, b.claimRenditions(t, m{})[0], pdfOf("the handout"), 1)
	// Yuki's question carries a text file, which has no rendition.
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": "My notes?", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, "text/plain", []byte("notes")), "filename": "notes.txt"}}}))
	notesFile := b.messages(t, b.yuki, opened.ConversationID)[0].Attachments[0].ID
	tanakaBot := b.agent(t, tanaka, "Tanaka's helper")
	// Ono's seat ended an hour ago, and the sweep has not been by: it is
	// as good as removed.
	onoM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": b.person(t, "Ono", ""), "preset": "observer"})).MemberID
	b.Exec(`UPDATE course_member SET expires_at = now() - interval '1 hour' WHERE id = $1`, onoM)
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
		{"conversation.mark_read", in(m{"conversation_id": conv}),
			apperr.Forbidden, "only the two who take part in a conversation mark it read; overseeing it keeps no place in it"},
		// A seat past its expiry, which the sweep has yet to remove.
		{"member.update_perms", in(m{"member_id": onoM, "perms": m{"document_read": "denied"}}),
			apperr.Conflict, "the member has been removed; seat the actor again for a fresh start"},
		{"member.rescope", in(m{"member_id": onoM, "assignment_scope": "all"}),
			apperr.Conflict, "the member has been removed; seat the actor again for a fresh start"},
		{"member.set_role", in(m{"member_id": onoM, "role": "ta"}),
			apperr.Conflict, "the member has been removed; seat the actor again for a fresh start"},
		{"member.pause", in(m{"member_id": onoM}),
			apperr.Conflict, "the member has been removed; seat the actor again for a fresh start"},
		{"member.resume", in(m{"member_id": onoM}),
			apperr.Conflict, "the member has been removed; seat the actor again for a fresh start"},
		{"member.remove", in(m{"member_id": onoM}),
			apperr.Conflict, "the member has been removed; seat the actor again for a fresh start"},
		{"conversation.close", in(m{"conversation_id": conv}),
			apperr.Forbidden, "only the two who take part in a conversation close it"},
		{"conversation.retract", in(m{"message_id": withdrawn}),
			apperr.Conflict, "the message is retracted already"},
		{"member.add", in(m{"actor_id": b.ken, "preset": "observer", "perms": reads}),
			apperr.Conflict, "the actor already has a seat in this course; change it, or remove it and add again for a fresh start"},
		{"assignment.publish", in(m{"assignment_id": b.hw3}),
			apperr.Conflict, "the assignment is already published"},
		{"assignment.publish", in(m{"assignment_id": hw4}),
			apperr.FailedPrecondition, "the instructions have no published version yet; students would see an assignment with nothing to read"},
		{"assignment.unpublish", in(m{"assignment_id": hw4}),
			apperr.Conflict, "the assignment is not published"},
		{"assignment.unpublish", in(m{"assignment_id": b.hw3}),
			apperr.FailedPrecondition, "it already has submissions — a draft, a hand-in, or the 'missing' rows recorded by hand or when its due date passed — so it can no longer be unpublished"},
		{"document.publish", in(m{"document_id": notes, "version_id": uuid.New()}),
			apperr.FailedPrecondition, "there is no such version of this document to publish"},
		{"document.publish", in(m{"document_id": syllabus.DocumentID, "version_id": syllabus.VersionID}),
			apperr.Conflict, "that version is already the published one"},
		{"document.publish", in(m{"document_id": old}),
			apperr.Conflict, "the document is archived"},
		{"document.publish", in(m{"document_id": essay}),
			apperr.FailedPrecondition, "a submission file is not published; it is visible to whoever may see its owner"},
		{"document.archive", in(m{"document_id": old}),
			apperr.Conflict, "the document is already archived"},
		{"document.archive", in(m{"document_id": essay}),
			apperr.Conflict, "the submission has been handed in; its files no longer change"},
		{"document.unarchive", in(m{"document_id": notes}),
			apperr.Conflict, "the document is not archived"},
		{"document.update", in(m{"document_id": essay, "title": "essay-final.txt"}),
			apperr.Conflict, "the submission has been handed in; its files no longer change"},
		{"submission.create", in(m{"assignment_id": b.hw3, "student_member_id": b.kenM}),
			apperr.Conflict, "there is already an open draft; edit or submit that one"},
		{"submission.create", in(m{"assignment_id": b.hw3, "student_member_id": b.graderM}),
			apperr.FailedPrecondition, "work is submitted by, or for, a current student of the course"},
		{"submission.update_draft", in(m{"submission_id": kenWork, "body": "More."}),
			apperr.Conflict, "the submission is no longer a draft; start a new attempt to submit again"},
		{"submission.submit", in(m{"submission_id": kenDraft}),
			apperr.FailedPrecondition, "there is nothing to hand in: the draft has no text and no files"},
		{"submission.set_lateness", in(m{"submission_id": kenDraft, "state": "late"}),
			apperr.Conflict, "the submission is draft; only a submitted or late attempt can be switched, and only to the other"},
		{"submission.record_missing", in(m{"assignment_id": b.hw3, "student_member_id": b.kenM}),
			apperr.Conflict, "the student already has a submission (draft) for this assignment"},
		{"submission.record_missing", in(m{"assignment_id": hw4, "student_member_id": b.kenM}),
			apperr.FailedPrecondition, "the assignment is not published; nobody can have missed it"},
		{"course.join_link_revoke", in(m{"link_id": link}),
			apperr.Conflict, "the join link is revoked already"},
		{"grade.undo_ungraded_as_zero", in(m{"student_member_id": b.kenM}),
			apperr.Conflict, "the student's totals do not count ungraded work as zero"},
		{"grade.undo_ungraded_as_zero", in(m{"all_students": true}),
			apperr.FailedPrecondition, "no student's totals count ungraded work as zero"},
		// The actions themselves: a decision, a review or a withdrawal of
		// one that is not the caller's to make, or not waiting for it.
		{"action.decide", in(m{"action_id": kenGraded, "decision": "approve"}),
			apperr.Conflict, "the action is executed, not awaiting a decision"},
		{"action.review", in(m{"action_id": kenGraded, "outcome": "reviewed"}),
			apperr.Conflict, "the action is not awaiting review"},
		{"action.withdraw", in(m{"action_id": graderProposal.ActionID}),
			apperr.Forbidden, "only whoever proposed it, or the owner of the agent that did, withdraws a proposal"},
		{"action.withdraw", in(m{"action_id": kenGraded}),
			apperr.Forbidden, "only whoever proposed it, or the owner of the agent that did, withdraws a proposal"},
		// A file's rendition: none for a PDF, and done for the Word file.
		{"document.rendition_retry", in(m{"document_id": handout.DocumentID, "file_id": handout.FileIDs[1]}),
			apperr.NotFound, "that file has no PDF rendition: only an Office or OpenDocument file has one"},
		{"document.rendition_retry", in(m{"document_id": handout.DocumentID, "file_id": handout.FileIDs[0]}),
			apperr.FailedPrecondition, "the PDF is there already; it is made once"},
		{"conversation.rendition_retry", in(m{"attachment_id": notesFile}),
			apperr.NotFound, "that file has no PDF rendition: only an Office or OpenDocument file has one"},
		// A conversation, which Tanaka did not open.
		{"conversation.ask", in(m{"conversation_id": conv, "body": "And?"}),
			apperr.Forbidden, "only whoever opened a conversation asks in it; the member it is addressed to answers, with conversation.answer"},
		// What the work's grades would become: Ken's 70 kept out of 50,
		// Yuki's 80 on the midterm too.
		{"assignment.update", in(m{"assignment_id": b.hw3, "points_possible": 50, "existing_grades": "keep_scores"}),
			apperr.FailedPrecondition, fmt.Sprintf("grade %s is 70, which was within the 100 points it was given out of and is above the 50 it would be out of; rescale, or regrade it first", kenGrade)},
		{"component.update", in(m{"component_id": b.midterm, "points_possible": 50, "existing_grades": "keep_scores"}),
			apperr.FailedPrecondition, fmt.Sprintf("grade %s is 80, which was within the 100 points it was given out of and is above the 50 it would be out of; rescale, or regrade it first", midterm)},
		// A document's versions and files, and their texts.
		{"document.add_version", in(m{"document_id": old, "body_md": "New."}),
			apperr.Conflict, "the document is archived"},
		{"document.add_version", in(m{"document_id": essay, "body_md": "New."}),
			apperr.FailedPrecondition, "a submission file has exactly one version; replace it by archiving it and adding another"},
		{"document.create", in(m{"kind": "submission", "title": "more.txt", "submission_id": yukiDraft, "body_md": "More."}),
			apperr.Conflict, "the submission is submitted; files are added to a draft, and a new attempt is a new draft"},
		{"document.create", in(m{"kind": "material", "title": "Slides", "upload_token": "not a token"}),
			apperr.InvalidArgument, "upload_token is not valid"},
		{"document.text_update", in(m{"document_id": old, "version_id": lastYear.VersionID, "body": "Old, corrected."}),
			apperr.Conflict, "the document is archived"},
		{"document.text_update", in(m{"document_id": notes, "version_id": notesVersion, "body": "Chapter 1."}),
			apperr.NotFound, "this version has no text version: only a file of a version of material, instructions or a rubric has one"},
		{"document.text_retranscribe", in(m{"document_id": handout.DocumentID, "version_id": *handout.VersionID,
			"file_id": handout.FileIDs[0], "base_revision": 99}),
			apperr.Conflict, "the text has changed since revision 99; it is at revision 1"},
		// An agent of Tanaka's, to be seated already gone.
		{"member.add_delegate", in(m{"actor_id": tanakaBot, "expires_at": past}),
			apperr.InvalidArgument, "expires_at is in the past"},
		// ... to reach someone who is no student, or an assignment the
		// course does not have.
		{"member.add_delegate", in(m{"actor_id": tanakaBot, "student_scope": "listed", "listed_students": []uuid.UUID{b.graderM}}),
			apperr.FailedPrecondition, "listed_students must all be current students of this course"},
		{"member.add_delegate", in(m{"actor_id": tanakaBot, "assignment_scope": "listed", "listed_assignments": []uuid.UUID{uuid.New()}}),
			apperr.FailedPrecondition, "listed_assignments must all be assignments of this course"},
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

	// Tanaka takes part in no conversation; the tutor, reading by proposal
	// now, takes part in Yuki's, and marks read up to a message not in it.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"document_read": "confirm_required"}})
	out := b.MustCall(b.tutor, "conversation.mark_read", in(m{"conversation_id": conv, "up_to_message_id": uuid.New()}), "tutor-reads")
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.InvalidArgument ||
		out.Error.Message != "up_to_message_id must name a message of this conversation's" {
		t.Fatalf("the tutor marking read up to a message not in the conversation: %+v", out)
	}
}

// downStore is a file store whose Stat stops answering while down is set.
type downStore struct {
	*blob.FSStore
	down *atomic.Bool
}

func (s downStore) Stat(ctx context.Context, key string) (blob.Info, error) {
	if s.down.Load() {
		return blob.Info{}, errors.New("the file store does not answer")
	}
	return s.FSStore.Stat(ctx, key)
}

// Whether approving an agent's grade would be refused is asked of its
// feedback files, which are in the file store. While the store does not
// answer, the queue still answers, and says the grade is not its owner's
// to decide; and an owner's decision fails with nothing recorded, whether
// they hold action_decide or decide nothing but their agents' proposals,
// so that the same call, made again once the store answers, is carried out.
func TestAnOwnersDecisionFailsUnrecordedWhileTheFileStoreDoesNotAnswer(t *testing.T) {
	var down atomic.Bool
	b := buildOn(t, testkit.NewPlatformWithStore(t, func(fs *blob.FSStore) blob.Store {
		return downStore{FSStore: fs, down: &down}
	}))
	ito := b.person(t, "Ito", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ito, "preset": "instructor", "perms": m{"action_decide": "denied"}})
	proposals := map[string]uuid.UUID{}
	for owner, actor := range map[string]uuid.UUID{"sato": b.sato, "ito": ito} {
		bot := b.agent(t, actor, owner+"'s marker")
		b.delegate(t, actor, bot, m{"preset": "ta", "perms": m{"grade_submit": "confirm_required"}})
		work := b.submit(t, map[string]uuid.UUID{"sato": b.ken, "ito": b.yuki}[owner], "an essay")
		notes := b.upload(t, bot, "feedback", "text/plain", []byte("well argued"))
		out := b.MustCall(bot, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80,
			"feedback_files": []m{{"title": "notes.txt", "upload_token": notes}}}, owner+"-bot-grades")
		if out.Status != domain.StatusProposed {
			t.Fatalf("%s's agent grading with feedback: %+v", owner, out)
		}
		proposals[owner] = *out.ActionID
	}
	owners := map[string]uuid.UUID{"sato": b.sato, "ito": ito}

	down.Store(true)
	for owner, actor := range owners {
		if q := b.queue(t, actor, "action.list_proposed"); q[proposals[owner]] {
			t.Fatalf("%s's queue while the file store does not answer: %v", owner, q)
		}
		key := owner + "-approves"
		if out, err := b.Call(actor, "action.decide", m{"course_id": b.course, "action_id": proposals[owner], "decision": "approve"}, key); err == nil {
			t.Fatalf("%s approving while the file store does not answer: %+v", owner, out)
		}
		if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND idempotency_key = $2`, actor, key); n != 0 {
			t.Fatalf("%s's failed approval is recorded %d times", owner, n)
		}
	}

	down.Store(false)
	for owner, actor := range owners {
		if q := b.queue(t, actor, "action.list_proposed"); !q[proposals[owner]] {
			t.Fatalf("%s's queue once the file store answers: %v", owner, q)
		}
		out := b.MustCall(actor, "action.decide", m{"course_id": b.course, "action_id": proposals[owner], "decision": "approve"}, owner+"-approves")
		if d := testkit.Result[pipeline.DecideOut](t, out); out.Replayed || d.Outcome != domain.StatusExecuted {
			t.Fatalf("%s approving again under the same key once the file store answers: %+v %+v", owner, out, d)
		}
	}
}
