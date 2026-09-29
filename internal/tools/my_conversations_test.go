package tools_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// me.conversations: a person's conversations with agents, as the one who
// asked, in every course at once, the newest activity first, for a chat
// panel that is no course's page.

// elsewhere is a second course of Sato's, CS102, with Yuki in it and a
// course tutor agent of Sato's that a runtime runs.
type elsewhere struct {
	course, yukiM, tutorM uuid.UUID
	tutor                 uuid.UUID // actor
}

func (c *cast) elsewhere(t *testing.T) elsewhere {
	t.Helper()
	b := c.built
	var e elsewhere
	e.course = testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create",
		m{"dept_id": b.dept, "term_id": b.term, "code": "CS102", "section": "B", "title": "Data Structures"})).CourseID
	b.do(t, b.admin, "course.activate", m{"course_id": e.course})
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": e.course, "actor_id": b.sato})
	e.yukiM = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": e.course, "actor_id": b.yuki, "preset": "student"})).MemberID
	e.tutor = b.agent(t, b.sato, "CS102 tutor")
	e.tutorM = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add_delegate",
		m{"course_id": e.course, "actor_id": e.tutor, "preset": "course_tutor"})).MemberID
	b.SiteChat(e.tutor)
	return e
}

func (b *built) mine(t *testing.T, actor uuid.UUID, args m) tools.MyConversationsOut {
	t.Helper()
	return testkit.Result[tools.MyConversationsOut](t, b.do(t, actor, "me.conversations", args))
}

func conversationIDs(list []tools.MyConversation) []uuid.UUID {
	out := make([]uuid.UUID, len(list))
	for i, c := range list {
		out[i] = c.ConversationID
	}
	return out
}

func sameIDs(got, want []uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestMyConversationsAcrossCoursesNewestFirst(t *testing.T) {
	c := newCast(t)
	b := c.built
	e := c.elsewhere(t)

	// Yuki asks CS101's tutor, then CS102's, then her own agent; CS101's
	// tutor answers, which makes its conversation the newest.
	first, q := b.open(t, b.yuki, c.courseTutor, "Where do I start?")
	second := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open",
		m{"course_id": e.course, "respondent_member_id": e.tutorM, "title": "Trees", "body": "What is a heap?"})).ConversationID
	third, _ := b.open(t, b.yuki, c.yukiBot, "Summarise my feedback")
	b.do(t, b.seatActor(t, c.courseTutor), "conversation.answer", answerArgs(b, first, q, "At the reading list."))

	all := b.mine(t, b.yuki, m{})
	if !sameIDs(conversationIDs(all.Conversations), []uuid.UUID{first, third, second}) || all.Next != nil {
		t.Fatalf("Yuki's conversations: %+v", all)
	}
	got := all.Conversations[2]
	if got.MemberID != e.yukiM || got.Course != (tools.MyConversationCourse{CourseID: e.course, Code: "CS102", Section: "B", Title: "Data Structures"}) ||
		got.Respondent != (tools.MyConversationRespondent{MemberID: e.tutorM, ActorID: e.tutor, DisplayName: "CS102 tutor", Kind: "agent"}) ||
		got.Title == nil || *got.Title != "Trees" || got.Status != "open" || got.State != tools.StateAwaitingAnswer || !got.MayAsk ||
		got.LastActivityAt.IsZero() || got.CreatedAt.IsZero() {
		t.Fatalf("the conversation in CS102: %+v", got)
	}
	if got := all.Conversations[0]; got.MemberID != b.yukiM || got.Course.CourseID != b.course || got.State != tools.StateAnswered ||
		got.Respondent.MemberID != c.courseTutor || !got.LastActivityAt.After(all.Conversations[1].LastActivityAt) {
		t.Fatalf("the conversation in CS101, answered last: %+v", got)
	}

	// One course's alone.
	if got := b.mine(t, b.yuki, m{"course_id": e.course}); !sameIDs(conversationIDs(got.Conversations), []uuid.UUID{second}) {
		t.Fatalf("Yuki's conversations in CS102: %+v", got)
	}
	if got := b.mine(t, b.yuki, m{"course_id": uuid.New()}); len(got.Conversations) != 0 {
		t.Fatalf("Yuki's conversations in a course she is not in: %+v", got)
	}
	// Nobody else's: not Ken's, whose are his own, nor the tutor's, which
	// asked nothing.
	kens, _ := b.open(t, b.ken, c.courseTutor, "Is the exam open book?")
	if got := b.mine(t, b.ken, m{}); !sameIDs(conversationIDs(got.Conversations), []uuid.UUID{kens}) {
		t.Fatalf("Ken's conversations: %+v", got)
	}
	if got := b.mine(t, b.seatActor(t, c.courseTutor), m{}); len(got.Conversations) != 0 {
		t.Fatalf("the tutor's conversations as the one who asked: %+v", got)
	}

	// Paged, a page at a time, to the end.
	var walked []uuid.UUID
	args := m{"limit": 1}
	for range 5 {
		page := b.mine(t, b.yuki, args)
		walked = append(walked, conversationIDs(page.Conversations)...)
		if page.Next == nil {
			break
		}
		args = m{"limit": 1, "after": *page.Next}
	}
	if !sameIDs(walked, []uuid.UUID{first, third, second}) {
		t.Fatalf("Yuki's conversations a page at a time: %v", walked)
	}
	if page := b.mine(t, b.yuki, m{"limit": 2}); len(page.Conversations) != 2 || page.Next == nil {
		t.Fatalf("a full page: %+v", page)
	}
	if page := b.mine(t, b.yuki, m{"limit": 1000}); len(page.Conversations) != 3 || page.Next != nil {
		t.Fatalf("a page asked for too large: %+v", page)
	}
	for _, bad := range []string{"nonsense", "bm9uc2Vuc2U", ""} {
		b.try(t, b.yuki, "me.conversations", m{"after": bad}, apperr.InvalidArgument)
	}
}

// What conversation.list would show of hers in each course is what it
// shows: nothing from a seat paused or removed, everything from a course
// archived, where she may no longer ask.
func TestMyConversationsAreThoseMySeatsMayRead(t *testing.T) {
	c := newCast(t)
	b := c.built
	e := c.elsewhere(t)
	here, _ := b.open(t, b.yuki, c.courseTutor, "Where do I start?")
	there := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open",
		m{"course_id": e.course, "respondent_member_id": e.tutorM, "body": "What is a heap?"})).ConversationID

	b.do(t, b.sato, "member.pause", m{"course_id": e.course, "member_id": e.yukiM})
	if got := b.mine(t, b.yuki, m{}); !sameIDs(conversationIDs(got.Conversations), []uuid.UUID{here}) {
		t.Fatalf("with her seat in CS102 paused: %+v", got)
	}
	b.do(t, b.sato, "member.resume", m{"course_id": e.course, "member_id": e.yukiM})

	b.do(t, b.admin, "course.archive", m{"course_id": e.course})
	got := b.mine(t, b.yuki, m{})
	if !sameIDs(conversationIDs(got.Conversations), []uuid.UUID{there, here}) || got.Conversations[0].MayAsk || !got.Conversations[1].MayAsk {
		t.Fatalf("with CS102 archived: %+v", got)
	}

	// Removed from CS101: its conversations are closed, and hers no longer.
	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM})
	if got := b.mine(t, b.yuki, m{}); !sameIDs(conversationIDs(got.Conversations), []uuid.UUID{there}) {
		t.Fatalf("removed from CS101: %+v", got)
	}
	// Nor may she ask where the course took away conversation_ask.
	b.do(t, b.admin, "course.activate", m{"course_id": e.course})
	b.do(t, b.sato, "member.update_perms", m{"course_id": e.course, "member_id": e.yukiM, "perms": m{"conversation_ask": "denied"}})
	if got := b.mine(t, b.yuki, m{}); len(got.Conversations) != 1 || got.Conversations[0].MayAsk {
		t.Fatalf("where she may no longer ask: %+v", got)
	}
}
