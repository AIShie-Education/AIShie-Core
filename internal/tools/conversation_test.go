package tools_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Conversations: nobody gains through one more than they hold. A member
// addresses only someone who can see and do nothing they cannot, or their
// own agent; only the two participants write; staff who decide actions for
// the opener look on; news of it goes to the participants alone.

// open starts a conversation as actor with respondent and asks body in it.
func (b *built) open(t *testing.T, actor, respondent uuid.UUID, body string) (conversation, message uuid.UUID) {
	t.Helper()
	out := testkit.Result[tools.ConversationOpenOut](t, b.do(t, actor, "conversation.open",
		m{"course_id": b.course, "respondent_member_id": respondent, "body": body}))
	if out.MessageID == nil {
		t.Fatal("no message for the question the conversation was opened with")
	}
	return out.ConversationID, *out.MessageID
}

func (b *built) ask(t *testing.T, actor, conversation uuid.UUID, body string) uuid.UUID {
	t.Helper()
	return testkit.Result[tools.MessageIDOut](t, b.do(t, actor, "conversation.ask",
		m{"course_id": b.course, "conversation_id": conversation, "body": body})).MessageID
}

func answerArgs(b *built, conversation, question uuid.UUID, body string) m {
	return m{"course_id": b.course, "conversation_id": conversation, "in_reply_to_message_id": question, "body": body}
}

func (b *built) respondents(t *testing.T, actor uuid.UUID) map[uuid.UUID]tools.RespondentView {
	t.Helper()
	out := map[uuid.UUID]tools.RespondentView{}
	for _, r := range testkit.Result[tools.RespondentsOut](t, b.do(t, actor, "conversation.respondents", m{"course_id": b.course})).Respondents {
		out[r.MemberID] = r
	}
	return out
}

func (b *built) conversation(t *testing.T, actor, conversation uuid.UUID) tools.ConversationGetOut {
	t.Helper()
	return testkit.Result[tools.ConversationGetOut](t, b.do(t, actor, "conversation.get", m{"course_id": b.course, "conversation_id": conversation}))
}

func (b *built) messages(t *testing.T, actor, conversation uuid.UUID) []tools.MessageView {
	t.Helper()
	return testkit.Result[tools.ConversationMessagesOut](t, b.do(t, actor, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conversation})).Messages
}

func (b *built) inbox(t *testing.T, actor uuid.UUID) []tools.ConversationView {
	t.Helper()
	return testkit.Result[tools.ConversationInboxOut](t, b.do(t, actor, "conversation.inbox", m{"course_id": b.course})).Conversations
}

func (b *built) listConversations(t *testing.T, actor uuid.UUID, args m) []tools.ConversationView {
	t.Helper()
	args["course_id"] = b.course
	return testkit.Result[tools.ConversationListOut](t, b.do(t, actor, "conversation.list", args)).Conversations
}

// cast is a course with every kind of respondent in it: Yuki's own agent,
// the course's tutor agent (Sato's delegate), the tutor listed for Yuki that
// build seats, and a TA. A runtime runs each agent, and says so.
type cast struct {
	*built
	bot                  uuid.UUID // Yuki's agent
	yukiBot, courseTutor uuid.UUID // seats
	ta                   uuid.UUID // actor
	taM                  uuid.UUID
}

func newCast(t *testing.T) *cast {
	t.Helper()
	b := build(t)
	c := &cast{built: b}
	c.bot = b.agent(t, b.yuki, "Yuki's helper")
	c.yukiBot = b.delegate(t, b.yuki, c.bot, m{})
	tutor := b.agent(t, b.sato, "Course tutor")
	c.courseTutor = b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	b.SiteChat(c.bot)
	b.SiteChat(tutor)
	c.ta = testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Mori"})).ActorID
	c.taM = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": c.ta, "preset": "ta"})).MemberID
	return c
}

// Whom each member may address, from both ends: the respondents they are
// offered, and what opening a conversation with each comes to.
func TestWhoMayAddressWhom(t *testing.T) {
	c := newCast(t)
	b := c.built
	cases := []struct {
		name       string
		asker      uuid.UUID
		respondent uuid.UUID
		ok         bool
	}{
		{"a student, her own agent", b.yuki, c.yukiBot, true},
		{"a student, someone else's agent", b.ken, c.yukiBot, false},
		{"a student, the course's tutor", b.yuki, c.courseTutor, true},
		{"another student, the course's tutor", b.ken, c.courseTutor, true},
		{"a student, the instructor", b.yuki, b.satoM, false},
		{"a student, a TA", b.yuki, c.taM, false},
		{"a student, the tutor listed for her", b.yuki, b.tutorM, true},
		{"another student, the tutor listed for her", b.ken, b.tutorM, false},
		{"the instructor, a student's agent", b.sato, c.yukiBot, false},
		{"the instructor, his own course tutor", b.sato, c.courseTutor, true},
		{"the instructor, the tutor listed for a student", b.sato, b.tutorM, true},
		{"a student, another student", b.yuki, b.kenM, false},
		{"a student, herself", b.yuki, b.yukiM, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, offered := b.respondents(t, tc.asker)[tc.respondent]
			if offered != tc.ok {
				t.Fatalf("offered as a respondent: %v, want %v", offered, tc.ok)
			}
			args := m{"course_id": b.course, "respondent_member_id": tc.respondent, "body": "Where do I start?"}
			if !tc.ok {
				b.try(t, tc.asker, "conversation.open", args, apperr.Forbidden)
				return
			}
			b.do(t, tc.asker, "conversation.open", args)
		})
	}
	// What a respondent says about itself.
	mine := b.respondents(t, b.yuki)
	if r := mine[c.yukiBot]; !r.IsMyDelegate || r.Kind != "agent" || r.OwnerName == nil || *r.OwnerName != "Yuki" || r.AnswerLevel != "autonomous" {
		t.Fatalf("Yuki's own agent, as offered to her: %+v", r)
	}
	if r := mine[c.courseTutor]; r.IsMyDelegate || r.OwnerName == nil || *r.OwnerName != "Sato" || r.Role != "assistant" {
		t.Fatalf("the course tutor, as offered to Yuki: %+v", r)
	}
	// A seat that answers nothing, or has stopped counting, is nobody's to ask.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": c.courseTutor, "perms": m{"conversation_answer": "denied"}})
	if _, ok := b.respondents(t, b.ken)[c.courseTutor]; ok {
		t.Fatal("a tutor that answers nothing is offered")
	}
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.yukiM})
	if out := b.MustCall(c.bot, "conversation.inbox", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("the agent of a paused student reads its inbox: %+v", out)
	}
	// And asking needs conversation_ask, answering conversation_answer.
	if out := b.MustCall(b.grader, "conversation.respondents", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a grader, who asks nothing, lists respondents: %+v", out)
	}
	if out := b.MustCall(b.ken, "conversation.inbox", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student, who answers nothing, reads an inbox: %+v", out)
	}
}

// Within means every level as well as every reach: a respondent listed for
// Yuki alone that may do one thing she may not is not hers to ask. Nor is a
// respondent whose actor is suspended.
func TestARespondentWithinReachButNotWithinLevelsIsNotAddressable(t *testing.T) {
	c := newCast(t)
	b := c.built
	helper := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "display_name": "Essay helper"})).ActorID
	seat := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "tutor",
		"listed_students": []uuid.UUID{b.yukiM}, "perms": m{"grade_submit": "autonomous"}})).MemberID
	b.SiteChat(helper)
	ask := m{"course_id": b.course, "respondent_member_id": seat, "body": "Grade me an A?"}
	if _, ok := b.respondents(t, b.yuki)[seat]; ok {
		t.Fatal("a respondent that grades is offered to a student who does not")
	}
	b.try(t, b.yuki, "conversation.open", ask, apperr.Forbidden)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"grade_submit": "denied"}})
	if _, ok := b.respondents(t, b.yuki)[seat]; !ok {
		t.Fatal("a respondent within Yuki's seat is not offered to her")
	}
	b.do(t, b.yuki, "conversation.open", ask)

	// The course tutor suspended by its owner answers nobody.
	b.do(t, b.sato, "agent.suspend", m{"actor_id": b.seatActor(t, c.courseTutor)})
	if _, ok := b.respondents(t, b.ken)[c.courseTutor]; ok {
		t.Fatal("a suspended agent is offered")
	}
	b.try(t, b.ken, "conversation.open", m{"course_id": b.course, "respondent_member_id": c.courseTutor, "body": "Hello?"}, apperr.Forbidden)
}

// A delegate answers others than its principal only if its seat was made to
// answer the course, by someone who manages the course's members, and only
// while that person still does. An instructor's own assistant, which reads
// no more than a student may, answers the instructor alone.
func TestADelegateAnswersTheCourseOnlyIfSeatedTo(t *testing.T) {
	c := newCast(t)
	b := c.built
	// Sato's own assistant, with the delegate preset: listed for nobody,
	// since he reaches the whole class, and so within every student's seat.
	helper := b.agent(t, b.sato, "Sato's private helper")
	private := b.delegate(t, b.sato, helper, m{})
	b.SiteChat(helper)
	if v := b.memberView(t, private); v.AnswersCourse || v.Perms["conversation_answer"] != "autonomous" || len(v.ListedStudents) != 0 {
		t.Fatalf("Sato's assistant: %+v", v)
	}
	if v := b.memberView(t, c.courseTutor); !v.AnswersCourse {
		t.Fatalf("the course tutor, seated with course_tutor by an instructor: %+v", v)
	}
	for _, who := range []uuid.UUID{b.yuki, b.ken} {
		if _, ok := b.respondents(t, who)[private]; ok {
			t.Fatal("a student is offered the instructor's own assistant")
		}
		b.try(t, who, "conversation.open", m{"course_id": b.course, "respondent_member_id": private, "body": "What did Sato tell you?"}, apperr.Forbidden)
		if r, ok := b.respondents(t, who)[c.courseTutor]; !ok || !r.AnswersCourse {
			t.Fatalf("the course tutor, as offered to a student: %+v", r)
		}
	}
	if r := b.respondents(t, b.sato)[private]; !r.IsMyDelegate || r.AnswersCourse {
		t.Fatalf("Sato's assistant, as offered to him: %+v", r)
	}
	// The candidates are narrowed in SQL already: another member's own agent
	// is not one of Ken's, so that however many students bring agents, the
	// course's agents are not crowded out.
	now := time.Now()
	rows, err := b.Q.ListRespondentCandidates(t.Context(), dbq.ListRespondentCandidatesParams{CourseID: b.course, CallerMemberID: b.kenM,
		Now: &now, MaxRows: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == c.yukiBot || r.ID == private {
			t.Fatalf("someone else's own agent is a candidate for Ken: %+v", r)
		}
	}

	// Chosen when the seat is made: a course_tutor answering Sato alone, and
	// a plain delegate answering the course, as he says.
	quietAgent, loudAgent := b.agent(t, b.sato, "Quiet tutor"), b.agent(t, b.sato, "Open helper")
	b.SiteChat(quietAgent)
	b.SiteChat(loudAgent)
	quiet := b.delegate(t, b.sato, quietAgent, m{"preset": "course_tutor", "answers_course": false})
	loud := b.delegate(t, b.sato, loudAgent, m{"answers_course": true})
	if _, ok := b.respondents(t, b.yuki)[quiet]; ok {
		t.Fatal("a course_tutor seated to answer its principal alone is offered to a student")
	}
	if _, ok := b.respondents(t, b.yuki)[loud]; !ok {
		t.Fatal("a delegate seated to answer the course is not offered to a student it is within")
	}
	if d := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.sato, "member.delegate_defaults", m{"course_id": b.course, "preset": "course_tutor"})); !d.AnswersCourse {
		t.Fatalf("what a course_tutor of Sato's would get: %+v", d)
	}
	if d := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.sato, "member.delegate_defaults", m{"course_id": b.course})); d.AnswersCourse {
		t.Fatalf("what a delegate of Sato's would get: %+v", d)
	}
	// Only someone who manages the members may say so: a student's agent,
	// even a course_tutor, answers the student alone.
	b.try(t, b.ken, "member.add_delegate", m{"course_id": b.course, "actor_id": b.agent(t, b.ken, "Ken's helper"), "answers_course": true}, apperr.Forbidden)
	if d := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.ken, "member.delegate_defaults", m{"course_id": b.course, "preset": "course_tutor"})); d.AnswersCourse {
		t.Fatalf("what a course_tutor of Ken's would get: %+v", d)
	}
	if got := b.membership(t, b.seatActor(t, c.courseTutor)); !got.AnswersCourse {
		t.Fatalf("the course tutor's own membership: %+v", got)
	}
	if got := b.membership(t, c.bot); got.AnswersCourse {
		t.Fatalf("Yuki's agent's own membership: %+v", got)
	}

	// Sato no longer manages the members: his course tutor answers him alone.
	kato := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Kato"})).ActorID
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": b.course, "actor_id": kato})
	b.do(t, kato, "member.update_perms", m{"course_id": b.course, "member_id": b.satoM, "perms": m{"member_manage": "denied"}})
	if _, ok := b.respondents(t, b.ken)[c.courseTutor]; ok {
		t.Fatal("a course tutor whose principal no longer manages the members is offered to a student")
	}
	b.try(t, b.ken, "conversation.open", m{"course_id": b.course, "respondent_member_id": c.courseTutor, "body": "Hello?"}, apperr.Forbidden)
	if got := b.membership(t, b.seatActor(t, c.courseTutor)); got.AnswersCourse {
		t.Fatalf("the course tutor's own membership, its principal no longer managing: %+v", got)
	}
}

// An answer answers the opener's latest message; one made for an older
// question is refused, and says what the latest is.
func TestAnAnswerAnswersTheLatestQuestion(t *testing.T) {
	b := build(t)
	conv, first := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	second := b.ask(t, b.yuki, conv, "And how long should mine be?")

	if in := b.inbox(t, b.tutor); len(in) != 1 || in[0].ID != conv || in[0].LatestOpenerMessageID == nil || *in[0].LatestOpenerMessageID != second ||
		in[0].State != tools.StateAwaitingAnswer {
		t.Fatalf("the tutor's inbox: %+v", in)
	}
	stale := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, first, "A claim you argue for."), "stale")
	if stale.Status != domain.StatusFailed || stale.Error.Code != apperr.Conflict || fmt.Sprint(stale.Error.Details["latest_opener_message_id"]) != second.String() {
		t.Fatalf("answering an older question: %+v", stale)
	}
	answered := testkit.Result[tools.MessageIDOut](t, b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, second, "A sentence or two."))).MessageID

	// What may be answered, by whom.
	b.try(t, b.tutor, "conversation.answer", answerArgs(b, conv, answered, "Answering myself."), apperr.InvalidArgument)
	b.try(t, b.tutor, "conversation.answer", answerArgs(b, conv, uuid.New(), "Answering nothing."), apperr.InvalidArgument)
	b.try(t, b.tutor, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "May I ask?"}, apperr.Forbidden)
	b.try(t, b.tutor, "conversation.answer", answerArgs(b, conv, second, "   "), apperr.InvalidArgument)
	if out := b.MustCall(b.yuki, "conversation.answer", answerArgs(b, conv, second, "I answer myself."), "self"); out.Status != domain.StatusDenied {
		t.Fatalf("a student answering: %+v", out)
	}
	b.try(t, b.sato, "conversation.close", m{"course_id": b.course, "conversation_id": conv}, apperr.Forbidden)

	msgs := b.messages(t, b.yuki, conv)
	if len(msgs) != 3 || msgs[2].ID != answered || msgs[2].InReplyToMessageID == nil || *msgs[2].InReplyToMessageID != second ||
		msgs[0].Seq != 1 || msgs[2].Seq != 3 || msgs[2].AuthorMemberID != b.tutorM || msgs[2].Body == nil || *msgs[2].Body != "A sentence or two." {
		t.Fatalf("the conversation: %+v", msgs)
	}
	if got := b.conversation(t, b.yuki, conv); got.State != tools.StateAnswered || len(got.VisibleTo) != 4 ||
		got.VisibleTo[3] != tools.VisibleToRespondentsOthers {
		t.Fatalf("the conversation, answered: %+v", got)
	}
	if in := b.inbox(t, b.tutor); len(in) != 0 {
		t.Fatalf("an answered conversation is still in the inbox: %+v", in)
	}
	// Paged: after a seq, oldest first; the newest before one, oldest first.
	page := testkit.Result[tools.ConversationMessagesOut](t, b.do(t, b.yuki, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conv, "after_seq": 1, "limit": 1}))
	if len(page.Messages) != 1 || page.Messages[0].ID != second || !page.More {
		t.Fatalf("after seq 1: %+v", page)
	}
	page = testkit.Result[tools.ConversationMessagesOut](t, b.do(t, b.yuki, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conv, "limit": 2}))
	if len(page.Messages) != 2 || page.Messages[0].ID != second || page.Messages[1].ID != answered || page.Conversation.ID != conv {
		t.Fatalf("the newest two: %+v", page)
	}
	page = testkit.Result[tools.ConversationMessagesOut](t, b.do(t, b.yuki, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conv, "before_seq": 2}))
	if len(page.Messages) != 1 || page.Messages[0].ID != first || page.More {
		t.Fatalf("before seq 2: %+v", page)
	}
}

// An answer that waits for approval is a proposal: the conversation is out
// of the inbox meanwhile, and on approval the same checks run again.
func TestAnAnswerThatWaitsForApproval(t *testing.T) {
	b := build(t)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	if r := b.respondents(t, b.yuki)[b.tutorM]; r.AnswerLevel != "confirm_required" {
		t.Fatalf("the tutor, as offered: %+v", r)
	}
	conv, q := b.open(t, b.yuki, b.tutorM, "Is HW3 due on Friday?")

	proposed := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q, "Yes, at noon."), "answer:"+conv.String()+":"+q.String())
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the tutor's answer: %+v", proposed)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("an answer waiting for approval was posted")
	}
	if in := b.inbox(t, b.tutor); len(in) != 0 {
		t.Fatalf("a conversation whose answer waits is still in the inbox: %+v", in)
	}
	if got := b.conversation(t, b.yuki, conv); got.State != tools.StateReplyPendingApproval || got.PendingReplyActionID == nil ||
		*got.PendingReplyActionID != *proposed.ActionID || got.Respondent.AnswerLevel != "confirm_required" {
		t.Fatalf("the conversation, its answer waiting: %+v", got)
	}
	if got := b.listConversations(t, b.yuki, m{"state": tools.StateReplyPendingApproval}); len(got) != 1 {
		t.Fatalf("listed by state: %+v", got)
	}
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusExecuted {
		t.Fatalf("approving the answer: %+v", d)
	}
	msgs := b.messages(t, b.yuki, conv)
	if len(msgs) != 2 || msgs[1].AuthorMemberID != b.tutorM || msgs[1].InReplyToMessageID == nil || *msgs[1].InReplyToMessageID != q {
		t.Fatalf("after approval: %+v", msgs)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE id = $1 AND created_by_action_id = $2`, msgs[1].ID, proposed.ActionID); n != 1 {
		t.Fatal("the answer does not name the proposal as the action that wrote it")
	}

	// Asked again while an answer waits: the approval finds the conversation
	// moved on, and the conversation is back in the inbox.
	q2 := b.ask(t, b.yuki, conv, "And HW4?")
	late := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "Also Friday."), "late")
	b.ask(t, b.yuki, conv, "Sorry, I meant HW5.")
	d = testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": late.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusFailed || d.Error == nil || d.Error.Code != apperr.Conflict {
		t.Fatalf("approving an answer to a question overtaken: %+v", d)
	}
	if in := b.inbox(t, b.tutor); len(in) != 1 || in[0].ID != conv {
		t.Fatalf("the inbox once the stale answer failed: %+v", in)
	}
	// A proposal is refused before it is queued if it could never run.
	stale := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "Also Friday."), "stale")
	if stale.Status != domain.StatusFailed || stale.Error.Code != apperr.Conflict {
		t.Fatalf("proposing an answer to an old question: %+v", stale)
	}
}

// A closed conversation takes no more messages, from either participant,
// and one closed while a message is on its way is not written in either:
// the message waits for the close and then finds it closed.
func TestAClosedConversationTakesNoMessages(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv, "reason": "Found it in the notes."})
	b.try(t, b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "One more thing"}, apperr.Conflict)
	b.try(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "A claim."), apperr.Conflict)
	b.try(t, b.tutor, "conversation.close", m{"course_id": b.course, "conversation_id": conv}, apperr.Conflict)
	if got := b.conversation(t, b.yuki, conv); got.Status != "closed" || got.State != tools.StateClosed ||
		got.ClosedReason == nil || *got.ClosedReason != "Found it in the notes." {
		t.Fatalf("the closed conversation: %+v", got)
	}
	if got := b.messages(t, b.tutor, conv); len(got) != 1 {
		t.Fatalf("a closed conversation is still readable: %+v", got)
	}

	// The race: a close under way holds the conversation; a question made
	// meanwhile waits for it, and is refused.
	conv, _ = b.open(t, b.yuki, b.tutorM, "Second try")
	release := b.hold(t, `UPDATE conversation SET status = 'closed', closed_reason = 'closing' WHERE id = $1`, conv)
	done := make(chan pipeline.Outcome, 1)
	b.start(t, done, b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "Are you there?"})
	b.blocked(t, 1, done)
	release()
	if out := <-done; out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict {
		t.Fatalf("a question racing a close: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("a message was written in a conversation closed under it")
	}
}

// A message is withdrawn by its author, or by staff who decide actions for
// the opener, and then read without its text. The action that wrote it keeps
// it: the log is the log.
func TestRetractingAMessage(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "My password is hunter2")
	answer := testkit.Result[tools.MessageIDOut](t, b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Please do not share that."))).MessageID

	b.try(t, b.tutor, "conversation.retract", m{"course_id": b.course, "message_id": q}, apperr.Forbidden)
	b.try(t, b.ken, "conversation.retract", m{"course_id": b.course, "message_id": q}, apperr.Forbidden)
	b.try(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": uuid.New()}, apperr.NotFound)
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q, "reason": "Oops"})
	b.try(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q}, apperr.Conflict)
	b.do(t, b.sato, "conversation.retract", m{"course_id": b.course, "message_id": answer})

	msgs := b.messages(t, b.tutor, conv)
	if len(msgs) != 2 || msgs[0].Body != nil || msgs[0].Retracted == nil || msgs[0].Retracted.ByMemberID == nil ||
		*msgs[0].Retracted.ByMemberID != b.yukiM || msgs[0].Retracted.Reason == nil || *msgs[0].Retracted.Reason != "Oops" ||
		msgs[1].Body != nil || msgs[1].Retracted == nil || *msgs[1].Retracted.ByMemberID != b.satoM {
		t.Fatalf("the retracted messages: %+v", msgs)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.open' AND payload->>'body' = 'My password is hunter2'`); n != 1 {
		t.Fatal("the action that wrote the message no longer says what it wrote")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE payload::text LIKE '%hunter2%' OR payload::text LIKE '%Oops%'`); n != 0 {
		t.Fatal("what was written reached the feed")
	}
}

// stateOf is the state of Yuki's conversation conv as conversation.get says
// it to her, insisting that conversation.messages and me.conversations say
// the same, and that conversation.list, filtered by each state, lists it
// under that one alone.
func (b *built) stateOf(t *testing.T, conv uuid.UUID) string {
	t.Helper()
	got := b.conversation(t, b.yuki, conv).State
	read := testkit.Result[tools.ConversationMessagesOut](t, b.do(t, b.yuki, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conv})).Conversation
	if read.State != got {
		t.Fatalf("conversation.get says %s, conversation.messages %s", got, read.State)
	}
	mine := false
	for _, c := range b.mine(t, b.yuki, m{}).Conversations {
		if c.ConversationID == conv {
			mine = true
			if c.State != got {
				t.Fatalf("conversation.get says %s, me.conversations %s", got, c.State)
			}
		}
	}
	if !mine {
		t.Fatal("me.conversations leaves the conversation out")
	}
	for _, state := range []string{tools.StateAwaitingAnswer, tools.StateReplyPendingApproval, tools.StateAnswered, tools.StateClosed} {
		listed := false
		for _, v := range b.listConversations(t, b.yuki, m{"state": state}) {
			listed = listed || v.ID == conv
		}
		if listed != (state == got) {
			t.Fatalf("conversation.get says %s; listed with state %s: %v", got, state, listed)
		}
	}
	return got
}

// withdrawn insists that e refuses an answer because its question was
// withdrawn: a conflict, moved_on, naming no message to answer instead.
func withdrawn(t *testing.T, e *apperr.Error, what string) {
	t.Helper()
	if e == nil || e.Code != apperr.Conflict || e.Details["reason"] != "moved_on" {
		t.Fatalf("%s: %+v", what, e)
	}
	if id, named := e.Details["latest_opener_message_id"]; named {
		t.Fatalf("%s names %v to answer: %+v", what, id, e)
	}
}

// A question its opener withdraws — "stop", conversation.retract of their
// latest message — waits for no answer: an answer to it is refused as
// moved_on, naming nothing else to answer; the conversation is answered in
// every view and filter, out of the inbox, and takes no draft, the one
// under way gone with the question. Withdrawing an older message changes
// none of that, whoever withdraws the latest does the same, and asking
// again makes the conversation wait for an answer again.
func TestAWithdrawnQuestionWaitsForNoAnswer(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "Is HW3 due on Friday?")
	b.draft(t, b.tutor, conv, "a1", 1, m{"text": "Yes"})
	if s := b.stateOf(t, conv); s != tools.StateAwaitingAnswer {
		t.Fatalf("the conversation asked: %s", s)
	}
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q, "reason": "stop"})

	out := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q, "Yes, at noon."), "answer:"+conv.String()+":"+q.String()+":1")
	if out.Status != domain.StatusFailed {
		t.Fatalf("an answer to a withdrawn question: %+v", out)
	}
	withdrawn(t, out.Error, "an answer to a withdrawn question")
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("an answer to a withdrawn question was posted")
	}
	if s := b.stateOf(t, conv); s != tools.StateAnswered {
		t.Fatalf("the conversation, its question withdrawn: %s", s)
	}
	if got := b.conversation(t, b.tutor, conv); got.State != tools.StateAnswered || got.LastRetractedAt == nil {
		t.Fatalf("the conversation as its respondent reads it: %+v", got)
	}
	if in := b.inbox(t, b.tutor); len(in) != 0 {
		t.Fatalf("a withdrawn question is in the inbox: %+v", in)
	}
	if n := b.drafts(t, conv); n != 0 || b.seen(t, b.yuki, conv) != nil {
		t.Fatal("the draft outlived the question it answered")
	}
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 2, m{"text": "Yes, at"})), apperr.Conflict, "conversation_not_awaiting")

	// Asked again, it waits again, and the new question is answered.
	q2 := b.ask(t, b.yuki, conv, "Sorry: is HW4 due on Friday?")
	if s := b.stateOf(t, conv); s != tools.StateAwaitingAnswer {
		t.Fatalf("the conversation asked again: %s", s)
	}
	q3 := b.ask(t, b.yuki, conv, "I mean this Friday.")
	b.draft(t, b.tutor, conv, "a2", 1, m{"text": "No"})

	// An older message withdrawn: the latest still waits, with its draft,
	// and an answer to the older one is told which to answer.
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q2})
	if s := b.stateOf(t, conv); s != tools.StateAwaitingAnswer {
		t.Fatalf("the conversation, an older message withdrawn: %s", s)
	}
	if in := b.inbox(t, b.tutor); len(in) != 1 || in[0].ID != conv || *in[0].LatestOpenerMessageID != q3 {
		t.Fatalf("the inbox, an older message withdrawn: %+v", in)
	}
	if d := b.seen(t, b.yuki, conv); d == nil || d.Attempt != "a2" {
		t.Fatalf("the draft, an older message withdrawn: %+v", d)
	}
	stale := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "No."), "answer:"+conv.String()+":"+q2.String()+":1")
	if stale.Status != domain.StatusFailed || reason(stale) != "moved_on" || fmt.Sprint(stale.Error.Details["latest_opener_message_id"]) != q3.String() {
		t.Fatalf("an answer to an older message, withdrawn: %+v", stale)
	}
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q3, "No, on Monday."))
	if s := b.stateOf(t, conv); s != tools.StateAnswered {
		t.Fatalf("the conversation answered: %s", s)
	}

	// Staff who oversee the opener withdraw her question as she would.
	q4 := b.ask(t, b.yuki, conv, "My password is hunter2, can you log in for me?")
	b.do(t, b.sato, "conversation.retract", m{"course_id": b.course, "message_id": q4})
	out = b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q4, "Please do not share that."), "answer:"+conv.String()+":"+q4.String()+":1")
	if out.Status != domain.StatusFailed {
		t.Fatalf("an answer to a question staff withdrew: %+v", out)
	}
	withdrawn(t, out.Error, "an answer to a question staff withdrew")
	if s := b.stateOf(t, conv); s != tools.StateAnswered {
		t.Fatalf("the conversation, staff having withdrawn its question: %s", s)
	}

	// Closed wins.
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
	if s := b.stateOf(t, conv); s != tools.StateClosed {
		t.Fatalf("the conversation closed: %s", s)
	}
}

// An answer waiting for approval to a question since withdrawn waits for
// nothing: the conversation is answered, shows no answer waiting, and
// approving the answer can only fail, moved_on. Proposed after the
// withdrawal, it is refused before it is queued.
func TestAnAnswerProposedToAWithdrawnQuestionIsNeverPosted(t *testing.T) {
	b := build(t)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	conv, q := b.open(t, b.yuki, b.tutorM, "Is HW3 due on Friday?")
	proposed := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q, "Yes, at noon."), "answer:"+conv.String()+":"+q.String()+":1")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the answer: %+v", proposed)
	}
	if s := b.stateOf(t, conv); s != tools.StateReplyPendingApproval {
		t.Fatalf("the conversation, its answer waiting: %s", s)
	}

	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q})
	if got := b.conversation(t, b.yuki, conv); got.State != tools.StateAnswered || got.PendingReplyActionID != nil {
		t.Fatalf("the conversation, the question its answer waits for withdrawn: %+v", got)
	}
	if s := b.stateOf(t, conv); s != tools.StateAnswered {
		t.Fatalf("the conversation, the question its answer waits for withdrawn: %s", s)
	}
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusFailed {
		t.Fatalf("approving an answer to a withdrawn question: %+v", d)
	}
	withdrawn(t, d.Error, "approving an answer to a withdrawn question")
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("an answer to a withdrawn question was posted on approval")
	}

	late := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q, "Yes."), "answer:"+conv.String()+":"+q.String()+":2")
	if late.Status != domain.StatusFailed {
		t.Fatalf("proposing an answer to a withdrawn question: %+v", late)
	}
	withdrawn(t, late.Error, "proposing an answer to a withdrawn question")
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.answer' AND status = 'proposed'`); n != 0 {
		t.Fatalf("%d answers to a withdrawn question wait for approval", n)
	}
	if s := b.stateOf(t, conv); s != tools.StateAnswered {
		t.Fatalf("the conversation after it all: %s", s)
	}
}

// A question withdrawn and an answer to it are made one after the other,
// never across each other: the opener's retraction takes the conversation as
// writing a message does. So a retraction waits for an answer being posted,
// and withdraws a question answered already; and an answer, posted or
// proposed, that comes while the question is being withdrawn waits for the
// retraction and is refused. Once "stop" is done, no answer to it follows.
func TestAWithdrawalAndAnAnswerNeverPassEachOther(t *testing.T) {
	b := build(t)
	ctx := context.Background()

	// An answer being posted holds the conversation (TouchConversation).
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	release := b.hold(t, `UPDATE conversation SET last_message_at = now(), last_author_member_id = respondent_member_id WHERE id = $1`, conv)
	done := make(chan pipeline.Outcome, 1)
	b.start(t, done, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q})
	b.blocked(t, 1, done)
	if len(done) != 0 {
		t.Fatalf("the retraction did not wait for the answer being posted: %+v", <-done)
	}
	release()
	select {
	case out := <-done:
		if out.Status != domain.StatusExecuted {
			t.Fatalf("a retraction that waited for an answer: %+v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retraction never came back")
	}

	// A retraction under way holds the conversation as LockConversationForAnswer
	// does, and has written its row: an answer made meanwhile waits for it.
	during := func(t *testing.T, conv, q uuid.UUID, answer func(done chan pipeline.Outcome)) pipeline.Outcome {
		t.Helper()
		var action uuid.UUID
		if err := b.Pool.QueryRow(ctx, `SELECT created_by_action_id FROM conversation_message WHERE id = $1`, q).Scan(&action); err != nil {
			t.Fatal(err)
		}
		tx, err := b.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM conversation WHERE id = $1 FOR NO KEY UPDATE`, conv); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id)
			VALUES ($1, $2, $3, $4)`, q, b.course, b.yukiM, action); err != nil {
			t.Fatal(err)
		}
		done := make(chan pipeline.Outcome, 1)
		answer(done)
		b.blocked(t, 1, done)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case out := <-done:
			return out
		case <-time.After(10 * time.Second):
			t.Fatal("the answer never came back")
		}
		return pipeline.Outcome{}
	}
	t.Run("posted", func(t *testing.T) {
		conv, q := b.open(t, b.yuki, b.tutorM, "Where do I start?")
		out := during(t, conv, q, func(done chan pipeline.Outcome) {
			b.start(t, done, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Chapter one."))
		})
		if out.Status != domain.StatusFailed {
			t.Fatalf("an answer posted while its question was withdrawn: %+v", out)
		}
		withdrawn(t, out.Error, "an answer posted while its question was withdrawn")
		if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
			t.Fatal("an answer was posted to a question withdrawn under it")
		}
	})
	t.Run("proposed", func(t *testing.T) {
		b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
		defer b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "autonomous"}})
		conv, q := b.open(t, b.yuki, b.tutorM, "And then?")
		out := during(t, conv, q, func(done chan pipeline.Outcome) {
			b.start(t, done, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Chapter two."))
		})
		if out.Status != domain.StatusFailed {
			t.Fatalf("an answer proposed while its question was withdrawn: %+v", out)
		}
		withdrawn(t, out.Error, "an answer proposed while its question was withdrawn")
	})
}

// A respondent reads a conversation only while its opener may still address
// it: narrowing or widening seats since it was asked counts. The opener
// always reads it, and so does whoever oversees the opener.
func TestTheRespondentReadsOnlyWhileStillAddressable(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "Can you look at my essay?")
	b.conversation(t, b.tutor, conv)

	// The tutor now reaches Ken too, which Yuki does not: it has become
	// someone who can see what she cannot.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.tutorM, "listed_students": []uuid.UUID{b.yukiM, b.kenM}})
	b.try(t, b.tutor, "conversation.get", m{"course_id": b.course, "conversation_id": conv}, apperr.NotFound)
	b.try(t, b.tutor, "conversation.messages", m{"course_id": b.course, "conversation_id": conv}, apperr.NotFound)
	b.try(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Sure."), apperr.Forbidden)
	b.try(t, b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "Hello?"}, apperr.Forbidden)
	if in := b.inbox(t, b.tutor); len(in) != 0 {
		t.Fatalf("the inbox of a tutor that may no longer be asked: %+v", in)
	}
	b.conversation(t, b.yuki, conv)
	b.conversation(t, b.sato, conv)
	b.try(t, b.ken, "conversation.get", m{"course_id": b.course, "conversation_id": conv}, apperr.NotFound)
	b.try(t, b.ken, "conversation.get", m{"course_id": b.course, "conversation_id": uuid.New()}, apperr.NotFound)

	// Narrowed back, it may answer again.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.tutorM, "listed_students": []uuid.UUID{b.yukiM}})
	b.conversation(t, b.tutor, conv)

	// The opener narrowed instead: Yuki reaches nobody now, not even herself,
	// and a tutor that reads her work is more than she holds.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.yukiM, "listed_students": []uuid.UUID{}})
	b.try(t, b.tutor, "conversation.get", m{"course_id": b.course, "conversation_id": conv}, apperr.NotFound)
	b.try(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Sure."), apperr.Forbidden)
	b.conversation(t, b.yuki, conv)
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.yukiM, "listed_students": []uuid.UUID{b.yukiM}})
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Sure."))
}

// Staff who decide actions see the conversations opened by the students
// within their scope, and no others; everyone else sees their own.
func TestOverseeingConversations(t *testing.T) {
	c := newCast(t)
	b := c.built
	// Mori decides actions, for Ken only.
	mori := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Mori"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta",
		"perms": m{"action_decide": "autonomous"}, "student_scope": "listed", "listed_students": []uuid.UUID{b.kenM}})
	yukis, yq := b.open(t, b.yuki, b.tutorM, "Yuki's question")
	kens, _ := b.open(t, b.ken, c.courseTutor, "Ken's question")

	ids := func(views []tools.ConversationView) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		for _, v := range views {
			out[v.ID] = true
		}
		return out
	}
	if got := ids(b.listConversations(t, b.sato, m{"as": "overseer"})); len(got) != 2 {
		t.Fatalf("what Sato oversees: %v", got)
	}
	if got := ids(b.listConversations(t, mori, m{})); len(got) != 1 || !got[kens] {
		t.Fatalf("what Mori sees: %v", got)
	}
	if got := ids(b.listConversations(t, b.yuki, m{})); len(got) != 1 || !got[yukis] {
		t.Fatalf("what Yuki sees: %v", got)
	}
	if got := ids(b.listConversations(t, b.tutor, m{"as": "respondent"})); len(got) != 1 || !got[yukis] {
		t.Fatalf("what the tutor is asked: %v", got)
	}
	if got := ids(b.listConversations(t, b.tutor, m{"as": "opener"})); len(got) != 0 {
		t.Fatalf("what the tutor asked: %v", got)
	}
	if got := b.listConversations(t, b.sato, m{"as": "overseer", "state": tools.StateAwaitingAnswer}); len(got) != 2 {
		t.Fatalf("awaiting an answer: %+v", got)
	}
	if got := b.listConversations(t, b.sato, m{"state": "closed"}); len(got) != 0 {
		t.Fatalf("closed: %+v", got)
	}
	b.try(t, b.ken, "conversation.list", m{"course_id": b.course, "as": "overseer"}, apperr.Forbidden)
	b.try(t, b.ken, "conversation.list", m{"course_id": b.course, "state": "sleeping"}, apperr.InvalidArgument)

	b.conversation(t, mori, kens)
	b.try(t, mori, "conversation.get", m{"course_id": b.course, "conversation_id": yukis}, apperr.NotFound)
	b.try(t, mori, "conversation.retract", m{"course_id": b.course, "message_id": yq}, apperr.Forbidden)
	if got := b.conversation(t, b.sato, yukis); got.Opener.MemberID != b.yukiM || got.Respondent.MemberID != b.tutorM ||
		got.Respondent.IsDelegateOfOpener || got.Respondent.SeatStatus != "active" || got.Respondent.Kind != "agent" {
		t.Fatalf("the conversation, as Sato oversees it: %+v", got)
	}
	// One's own agent is one's own.
	own, oq := b.open(t, b.yuki, c.yukiBot, "Summarise my feedback")
	if got := b.conversation(t, b.yuki, own); !got.Respondent.IsDelegateOfOpener || got.Respondent.OwnerName == nil ||
		len(got.VisibleTo) != 3 || got.VisibleTo[0] != tools.VisibleToParticipants {
		t.Fatalf("Yuki's conversation with her agent: %+v", got)
	}
	if in := b.inbox(t, c.bot); len(in) != 1 || in[0].ID != own {
		t.Fatalf("the agent's inbox: %+v", in)
	}
	b.do(t, c.bot, "conversation.answer", answerArgs(b, own, oq, "You lost marks on evidence."))
}

// News of a conversation goes to its two participants, and to nobody else,
// whatever they hold.
func TestConversationNewsIsForItsParticipants(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "A claim."))
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q})
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})

	want := map[string]int{"conversation.opened": 1, "conversation.message_posted": 2, "conversation.message_retracted": 1, "conversation.closed": 1}
	for _, who := range []uuid.UUID{b.yuki, b.tutor} {
		got := types(feed(t, b, who))
		for typ, n := range want {
			if got[typ] != n {
				t.Fatalf("a participant's feed: %v, want %v", got, want)
			}
		}
	}
	for _, who := range []uuid.UUID{b.ken, b.sato, b.grader} {
		for typ := range types(feed(t, b, who)) {
			if want[typ] != 0 {
				t.Fatalf("someone who does not take part is told %s", typ)
			}
		}
	}
}

// Removing a seat closes the conversations it takes part in, and a
// delegate's go with its principal's.
func TestRemovingASeatClosesItsConversations(t *testing.T) {
	c := newCast(t)
	b := c.built
	withBot, _ := b.open(t, b.yuki, c.yukiBot, "Hello, helper")
	withTutor, _ := b.open(t, b.yuki, c.courseTutor, "Hello, tutor")
	kens, _ := b.open(t, b.ken, c.courseTutor, "Hello from Ken")

	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM})
	for _, conv := range []uuid.UUID{withBot, withTutor} {
		if n := b.Count(`SELECT count(*) FROM conversation WHERE id = $1 AND status = 'closed' AND closed_reason = 'seat_removed'`, conv); n != 1 {
			t.Fatalf("conversation %s was not closed with the seat", conv)
		}
		if n := b.Count(`SELECT count(*) FROM event WHERE type = 'conversation.closed' AND subject_id = $1 AND payload->>'reason' = 'seat_removed'`, conv); n != 1 {
			t.Fatal("no event says the conversation closed with the seat")
		}
	}
	if got := b.conversation(t, b.ken, kens); got.Status != "open" {
		t.Fatalf("someone else's conversation with the same tutor: %+v", got)
	}
	// The manager who removed her sees none of it in the feed.
	for typ := range types(feed(t, b, b.sato)) {
		if typ == "conversation.closed" {
			t.Fatal("the manager is told of conversations he took no part in")
		}
	}
	// Seated again, Yuki starts afresh, with a new seat and no conversation.
	yuki := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.yuki, "preset": "student"})).MemberID
	if got := b.listConversations(t, b.yuki, m{}); len(got) != 0 || yuki == b.yukiM {
		t.Fatalf("Yuki's conversations, seated again: %+v", got)
	}
	// The course tutor withdrawn by its owner: Ken's conversation closes too.
	b.do(t, b.sato, "agent.withdraw", m{"actor_id": b.seatActor(t, c.courseTutor), "course_id": b.course})
	if got := b.conversation(t, b.ken, kens); got.Status != "closed" || got.Respondent.SeatStatus != "removed" || got.Respondent.AnswerLevel != "denied" {
		t.Fatalf("Ken's conversation with a tutor withdrawn: %+v", got)
	}
}

func (b *built) seatActor(t *testing.T, seat uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT actor_id FROM course_member WHERE id = $1`, seat).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A member's own actions can leave the chat out.
func TestMyActionsLeaveOutTheChat(t *testing.T) {
	b := build(t)
	b.open(t, b.yuki, b.tutorM, "Hello")
	b.submit(t, b.yuki, "Yuki's essay")
	all := testkit.Result[tools.ActionListOut](t, b.do(t, b.yuki, "action.list_mine", m{"course_id": b.course})).Actions
	some := testkit.Result[tools.ActionListOut](t, b.do(t, b.yuki, "action.list_mine",
		m{"course_id": b.course, "exclude_types": []string{"conversation.open", "conversation.ask"}})).Actions
	if len(all) != 3 || len(some) != 2 {
		t.Fatalf("Yuki's actions: %d in all, %d without the chat", len(all), len(some))
	}
	for _, a := range some {
		if a.ActionType == "conversation.open" {
			t.Fatal("an excluded type was listed")
		}
	}
}

func (b *built) inboxOf(t *testing.T, actor uuid.UUID, limit int) []tools.ConversationView {
	t.Helper()
	return testkit.Result[tools.ConversationInboxOut](t, b.do(t, actor, "conversation.inbox", m{"course_id": b.course, "limit": limit})).Conversations
}

// The inbox lists what can be answered. Conversations whose openers may no
// longer address the one asked, however many and however old, do not stand
// in front of one that can; nor does one whose latest question is
// retracted, nor an answer waiting for approval to a question overtaken.
func TestTheInboxShowsWhatCanBeAnswered(t *testing.T) {
	c := newCast(t)
	b := c.built
	// Ken asks the course tutor five times, then may no longer ask it: it
	// reads the material, which he no longer may.
	for i := range 5 {
		b.open(t, b.ken, c.courseTutor, fmt.Sprintf("Question %d", i))
	}
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"document_read": "denied"}})
	yukis, _ := b.open(t, b.yuki, c.courseTutor, "A question that can be answered")
	tutor := b.seatActor(t, c.courseTutor)
	if in := b.inboxOf(t, tutor, 1); len(in) != 1 || in[0].ID != yukis {
		t.Fatalf("the inbox behind five conversations it cannot answer: %+v", in)
	}
	// An opener suspended everywhere is left out before anything is looked at.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"document_read": "autonomous"}})
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.ken})
	if in := b.inboxOf(t, tutor, 1); len(in) != 1 || in[0].ID != yukis {
		t.Fatalf("the inbox behind a suspended opener's conversations: %+v", in)
	}
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": b.ken})
	if in := b.inboxOf(t, tutor, 20); len(in) != 6 {
		t.Fatalf("the inbox with Ken back: %d conversations", len(in))
	}

	// A question its writer took back waits for nothing.
	conv, q := b.open(t, b.yuki, b.tutorM, "oops, wrong window: my password is x")
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q})
	for _, v := range b.inbox(t, b.tutor) {
		if v.ID == conv {
			t.Fatal("a conversation whose only question is retracted waits for an answer")
		}
	}
	if got := b.conversation(t, b.yuki, conv); got.LastRetractedAt == nil {
		t.Fatalf("the conversation does not say a message in it was retracted: %+v", got)
	}
	q2 := b.ask(t, b.yuki, conv, "What is a thesis?")
	if in := b.inbox(t, b.tutor); len(in) != 1 || in[0].ID != conv || *in[0].LatestOpenerMessageID != q2 {
		t.Fatalf("the inbox once she asks again: %+v", in)
	}

	// An answer waiting for approval to a question since overtaken hides
	// nothing: approving it can only fail.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	waiting := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "A claim."), "answer:"+conv.String()+":"+q2.String()+":1")
	if waiting.Status != domain.StatusProposed || len(b.inbox(t, b.tutor)) != 0 {
		t.Fatalf("an answer waiting: %+v", waiting)
	}
	q3 := b.ask(t, b.yuki, conv, "And an argument?")
	if in := b.inbox(t, b.tutor); len(in) != 1 || in[0].State != tools.StateAwaitingAnswer || in[0].PendingReplyActionID != nil {
		t.Fatalf("the inbox with an answer waiting to an older question: %+v", in)
	}
	if got := b.conversation(t, b.yuki, conv); got.State != tools.StateAwaitingAnswer {
		t.Fatalf("the conversation, an answer to an older question waiting: %+v", got)
	}
	if got := b.listConversations(t, b.yuki, m{"state": tools.StateReplyPendingApproval}); len(got) != 0 {
		t.Fatalf("listed as waiting for approval: %+v", got)
	}
	if n := len(b.listConversations(t, b.yuki, m{"state": tools.StateAwaitingAnswer})); n != 2 {
		t.Fatalf("%d listed as awaiting an answer", n)
	}
	if next := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q3, "Reasons for the claim."), "answer:"+conv.String()+":"+q3.String()+":1"); next.Status != domain.StatusProposed {
		t.Fatalf("an answer to the newest question: %+v", next)
	}
}

// A question is answered once. A second answer to it — a retry under a new
// key, a second proposal — is refused, however it comes; and an answer
// rejected is written again under a new key.
func TestAQuestionIsAnsweredOnce(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "Is HW3 due on Friday?")
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Yes."))
	again := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q, "Yes, at noon."), "answer:"+conv.String()+":"+q.String()+":2")
	if again.Status != domain.StatusFailed || again.Error.Code != apperr.Conflict || reason(again) != "already_answered" {
		t.Fatalf("a second answer to one question: %+v", again)
	}

	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	q2 := b.ask(t, b.yuki, conv, "And HW4?")
	key := func(attempt int) string { return fmt.Sprintf("answer:%s:%s:%d", conv, q2, attempt) }
	first := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "Yes"), key(1))
	if first.Status != domain.StatusProposed {
		t.Fatalf("the first answer: %+v", first)
	}
	second := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "No, Monday"), key(2))
	if second.Status != domain.StatusFailed || reason(second) != "answer_pending" {
		t.Fatalf("a second answer to a question whose first waits: %+v", second)
	}
	// Rejected, the conversation is back in the inbox, and the answer is
	// written again under the next attempt.
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": first.ActionID, "decision": "reject", "reason": "HW4 is due Monday"})
	if in := b.inbox(t, b.tutor); len(in) != 1 || in[0].ID != conv {
		t.Fatalf("the inbox after a rejection: %+v", in)
	}
	if replay := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "Yes"), key(1)); replay.Status != domain.StatusRejected {
		t.Fatalf("the rejected attempt, retried: %+v", replay)
	}
	third := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q2, "No, Monday"), key(3))
	if third.Status != domain.StatusProposed {
		t.Fatalf("the answer written again: %+v", third)
	}
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": third.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusExecuted {
		t.Fatalf("approving it: %+v", d)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE in_reply_to_message_id = $1`, q2); n != 1 {
		t.Fatalf("%d answers to one question", n)
	}

	// Nobody passes a close off as a seat removal.
	b.try(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv, "reason": "seat_removed"}, apperr.InvalidArgument)
	b.try(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv, "reason": " Seat_Removed "}, apperr.InvalidArgument)
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv, "reason": "Thanks"})
}

// The answers of a course tutor its instructor owns are that instructor's
// party's: the queues list them, and say whose they are to decide. No person
// answers a conversation, so the instructor is measured for them by what
// judging an answer is: they decide them where they decide actions without
// anyone's confirmation; where their own decisions wait for one, someone
// else decides the tutor's.
func TestAnInstructorsOwnTutorIsDecidedByThemOnlyWhereTheyDecideFreely(t *testing.T) {
	c := newCast(t)
	b := c.built
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": c.courseTutor, "perms": m{"conversation_answer": "confirm_required"}})
	conv, q := b.open(t, b.ken, c.courseTutor, "Is HW3 due on Friday?")
	answer := b.MustCall(b.seatActor(t, c.courseTutor), "conversation.answer", answerArgs(b, conv, q, "Yes."), "answer:1")
	if answer.Status != domain.StatusProposed {
		t.Fatalf("the tutor's answer: %+v", answer)
	}
	request := b.MustCall(b.ken, "member.add_delegate", m{"course_id": b.course, "actor_id": b.agent(t, b.ken, "Ken's helper")}, "request")
	queue := func() map[uuid.UUID]*bool {
		got := map[uuid.UUID]*bool{}
		for _, a := range testkit.Result[tools.ActionListOut](t, b.do(t, b.sato, "action.list_proposed", m{"course_id": b.course})).Actions {
			got[a.ID] = a.YoursToDecide
		}
		return got
	}
	if v := queue()[*request.ActionID]; v == nil || !*v {
		t.Fatalf("Ken's request, as Sato's queue lists it: %v", v)
	}
	// Sato answers nothing himself, whatever his row is told, and it makes
	// no difference to this.
	b.Exec(`UPDATE course_member SET perm_conversation_answer = 'autonomous' WHERE id = $1`, b.satoM)
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND perm_conversation_answer = 'denied'`, b.satoM); n != 1 {
		t.Fatal("a person's seat was written answering conversations")
	}
	if v := queue()[*answer.ActionID]; v == nil || !*v {
		t.Fatalf("Sato's own tutor's answer, as his queue lists it while he decides freely: %v", v)
	}

	// Sato's own decisions wait for a confirmation: his tutor's answers are
	// not his, and a decision of his about one is itself a proposal.
	b.Exec(`UPDATE course_member SET perm_action_decide = 'confirm_required' WHERE id = $1`, b.satoM)
	if v := queue()[*answer.ActionID]; v == nil || *v {
		t.Fatalf("Sato's own tutor's answer, as his queue lists it while his own decisions wait: %v", v)
	}
	if d := b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": answer.ActionID, "decision": "approve"}, "decide"); d.Status != domain.StatusProposed {
		t.Fatalf("Sato deciding his own tutor's answer while his own decisions wait: %+v", d)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("the answer was posted on a decision that waits for a confirmation")
	}

	// He decides without anyone again: the tutor's answer is his to decide,
	// and approved, it is posted, the record saying its owner approved it.
	b.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = $1`, b.satoM)
	if v := queue()[*answer.ActionID]; v == nil || !*v {
		t.Fatalf("Sato's own tutor's answer, as his queue lists it: %v", v)
	}
	if v := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide",
		m{"course_id": b.course, "action_id": answer.ActionID, "decision": "approve"})); v.Outcome != domain.StatusExecuted || !v.ByOwner {
		t.Fatalf("Sato approving his own tutor's answer: %+v", v)
	}
	if got := testkit.Result[tools.ConversationMessagesOut](t, b.do(t, b.ken, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conv})).Messages; len(got) != 2 || got[1].Body == nil || *got[1].Body != "Yes." {
		t.Fatalf("Ken's conversation once the answer was approved: %+v", got)
	}
}

// A call writing in a conversation takes its caller's seat, then the other
// participant's and that one's principal's, and the conversation last: a
// change to either seat under way is waited for, and what it did is seen.
// And an answer is checked against the newest question under the
// conversation's lock, so a question written while the answer waits for the
// lock is not passed over.
func TestAConversationWriteWaitsForTheSeatsItDependsOn(t *testing.T) {
	c := newCast(t)
	b := c.built
	pause := `WITH held AS (SELECT id FROM course_member WHERE id = $1 FOR UPDATE)
		UPDATE course_member SET status = 'paused' WHERE id IN (SELECT id FROM held)`
	waits := func(what string, release func(), done chan pipeline.Outcome, want apperr.Code) {
		t.Helper()
		b.blocked(t, 1, done)
		select {
		case out := <-done:
			t.Fatalf("%s did not wait: %+v", what, out)
		default:
		}
		release()
		if out := <-done; out.Status != domain.StatusFailed || out.Error.Code != want {
			t.Fatalf("%s, once it could go on: %+v", what, out)
		}
	}
	resume := func(seat uuid.UUID) { b.Exec(`UPDATE course_member SET status = 'active' WHERE id = $1`, seat) }

	// The respondent paused meanwhile.
	conv, _ := b.open(t, b.yuki, b.tutorM, "What is a thesis?")
	done := make(chan pipeline.Outcome, 1)
	release := b.hold(t, pause, b.tutorM)
	b.start(t, done, b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "Hello?"})
	waits("asking while the respondent is paused", release, done, apperr.Forbidden)
	resume(b.tutorM)

	// The respondent's principal paused meanwhile: the course tutor goes
	// with Sato.
	kens, _ := b.open(t, b.ken, c.courseTutor, "What is a thesis?")
	release = b.hold(t, pause, b.satoM)
	b.start(t, done, b.ken, "conversation.ask", m{"course_id": b.course, "conversation_id": kens, "body": "Hello?"})
	waits("asking while the respondent's principal is paused", release, done, apperr.Forbidden)
	resume(b.satoM)

	// The opener paused meanwhile, from the respondent's side.
	conv, q := b.open(t, b.yuki, b.tutorM, "And an argument?")
	release = b.hold(t, pause, b.yukiM)
	b.start(t, done, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Reasons for a claim."))
	waits("answering while the opener is paused", release, done, apperr.Forbidden)
	resume(b.yukiM)

	// A newer question written while the answer waits for the conversation.
	var action uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT created_by_action_id FROM conversation_message WHERE id = $1`, q).Scan(&action); err != nil {
		t.Fatal(err)
	}
	release = b.hold(t, `WITH touched AS (
			UPDATE conversation SET last_message_at = now(), last_author_member_id = $2 WHERE id = $1 RETURNING id)
		INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
		SELECT gen_random_uuid(), id, $3, 2, $2, 'Sorry, I meant a thesis.', $4 FROM touched`, conv, b.yukiM, b.course, action)
	b.start(t, done, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Reasons for a claim."))
	waits("answering while a newer question is written", release, done, apperr.Conflict)
}

// An agent's page: those who oversee conversations list one agent's, with
// the members they decide actions for, and nobody else's; a member who
// oversees nothing lists their own with it, and no more.
func TestAnAgentsConversationsForThoseWhoOverseeThem(t *testing.T) {
	c := newCast(t)
	b := c.built
	// Mori decides actions, for Ken only.
	mori := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Mori"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta",
		"perms": m{"action_decide": "autonomous"}, "student_scope": "listed", "listed_students": []uuid.UUID{b.kenM}})
	yukiTutor, _ := b.open(t, b.yuki, c.courseTutor, "Where do I start?")
	kenTutor, _ := b.open(t, b.ken, c.courseTutor, "Is the exam open book?")
	yukiListed, _ := b.open(t, b.yuki, b.tutorM, "Can you look at my essay?")
	yukiOwn, _ := b.open(t, b.yuki, c.yukiBot, "Summarise my feedback")
	b.do(t, b.ken, "conversation.close", m{"course_id": b.course, "conversation_id": kenTutor})

	ids := func(views []tools.ConversationView) []uuid.UUID {
		out := make([]uuid.UUID, len(views))
		for i, v := range views {
			out[i] = v.ID
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		who   uuid.UUID
		args  m
		wants []uuid.UUID
	}{
		{"Sato, the course tutor's", b.sato, m{"as": "overseer", "respondent_member_id": c.courseTutor}, []uuid.UUID{yukiTutor, kenTutor}},
		{"Sato, the course tutor's still open", b.sato, m{"as": "overseer", "respondent_member_id": c.courseTutor, "state": "open"}, []uuid.UUID{yukiTutor}},
		{"Sato, Yuki's tutor's", b.sato, m{"as": "overseer", "respondent_member_id": b.tutorM}, []uuid.UUID{yukiListed}},
		{"Sato, Yuki's own agent's", b.sato, m{"as": "overseer", "respondent_member_id": c.yukiBot}, []uuid.UUID{yukiOwn}},
		{"Sato, a seat nobody asked", b.sato, m{"as": "overseer", "respondent_member_id": b.graderM}, []uuid.UUID{}},
		{"Mori, the course tutor's with Ken", mori, m{"as": "overseer", "respondent_member_id": c.courseTutor}, []uuid.UUID{kenTutor}},
		{"Mori, Yuki's tutor's", mori, m{"as": "overseer", "respondent_member_id": b.tutorM}, []uuid.UUID{}},
		{"Yuki, her own with the course tutor", b.yuki, m{"respondent_member_id": c.courseTutor}, []uuid.UUID{yukiTutor}},
		{"the course tutor, those addressed to it", b.seatActor(t, c.courseTutor), m{"respondent_member_id": c.courseTutor}, []uuid.UUID{yukiTutor, kenTutor}},
	} {
		if got := ids(b.listConversations(t, tc.who, tc.args)); !sameIDs(got, tc.wants) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.wants)
		}
	}
	// Paged by id, as ever.
	page := testkit.Result[tools.ConversationListOut](t, b.do(t, b.sato, "conversation.list",
		m{"course_id": b.course, "as": "overseer", "respondent_member_id": c.courseTutor, "limit": 1}))
	if len(page.Conversations) != 1 || page.Conversations[0].ID != yukiTutor || page.Next == nil {
		t.Fatalf("the first page of the course tutor's: %+v", page)
	}
	page = testkit.Result[tools.ConversationListOut](t, b.do(t, b.sato, "conversation.list",
		m{"course_id": b.course, "as": "overseer", "respondent_member_id": c.courseTutor, "limit": 1, "after": page.Next}))
	if len(page.Conversations) != 1 || page.Conversations[0].ID != kenTutor {
		t.Fatalf("the second page of the course tutor's: %+v", page)
	}
	b.try(t, b.ken, "conversation.list", m{"course_id": b.course, "as": "overseer", "respondent_member_id": c.courseTutor}, apperr.Forbidden)
}
