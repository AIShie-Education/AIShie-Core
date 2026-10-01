package tools_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
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
	e.tutor = b.runtimeAgent(t, b.sato, "CS102 tutor")
	e.tutorM = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add_delegate",
		m{"course_id": e.course, "actor_id": e.tutor, "preset": "course_tutor"})).MemberID
	b.Host(e.tutor)
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

func (b *built) markRead(t *testing.T, actor, conversation uuid.UUID, args m) tools.ConversationMarkReadOut {
	t.Helper()
	args["course_id"], args["conversation_id"] = b.course, conversation
	return testkit.Result[tools.ConversationMarkReadOut](t, b.do(t, actor, "conversation.mark_read", args))
}

// Read state: what each participant has read of a conversation, which only
// goes forward; unread, whether the other has written, and not retracted,
// anything since. me.conversations, conversation.list and conversation.get
// say it to a participant; nobody else is told anything of it.
func TestUnreadUntilMarkedRead(t *testing.T) {
	c := newCast(t)
	b := c.built
	tutor := b.seatActor(t, c.courseTutor)
	conv, q1 := b.open(t, b.yuki, c.courseTutor, "Where do I start?")
	unread := func(when string, want bool) {
		t.Helper()
		mine := b.mine(t, b.yuki, m{})
		if len(mine.Conversations) != 1 || mine.Conversations[0].Unread != want {
			t.Fatalf("%s: me.conversations says %+v, want unread %v", when, mine.Conversations, want)
		}
		if got := b.conversation(t, b.yuki, conv); got.Unread == nil || *got.Unread != want {
			t.Fatalf("%s: conversation.get says unread %v, want %v", when, got.Unread, want)
		}
		if got := b.listConversations(t, b.yuki, m{}); len(got) != 1 || got[0].Unread == nil || *got[0].Unread != want {
			t.Fatalf("%s: conversation.list says %+v, want unread %v", when, got, want)
		}
	}
	unread("asked, and nothing answered", false)

	a1 := testkit.Result[tools.MessageIDOut](t, b.do(t, tutor, "conversation.answer", answerArgs(b, conv, q1, "At the reading list."))).MessageID
	unread("answered", true)
	// The agent, which read the question to answer it, has nothing unread;
	// the instructor who oversees it is told nothing either way.
	if got := b.conversation(t, tutor, conv); got.Unread == nil || !*got.Unread {
		t.Fatalf("the tutor, which marked nothing read, is not told the question is unread: %+v", got.Unread)
	}
	if got := b.conversation(t, b.sato, conv); got.Unread != nil {
		t.Fatalf("an overseer is told of unread: %+v", got)
	}
	if got := b.listConversations(t, b.sato, m{"as": "overseer"}); len(got) != 1 || got[0].Unread != nil {
		t.Fatalf("an overseer's list says unread: %+v", got)
	}

	read := b.markRead(t, b.yuki, conv, m{})
	if read.ReadUpToSeq != 2 || read.Unread {
		t.Fatalf("marked read: %+v", read)
	}
	unread("read", false)
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.mark_read' AND status = 'executed' AND target_id = $1`, conv); n != 1 {
		t.Fatal("marking read is not on record as an action")
	}
	if n := b.Count(`SELECT count(*) FROM event e JOIN action a ON a.id = e.action_id WHERE a.action_type = 'conversation.mark_read'`); n != 0 {
		t.Fatal("marking read is news to someone")
	}
	// The same again, under the same key, is what it was; under a new one,
	// it changes nothing.
	again := b.MustCall(b.yuki, "conversation.mark_read", m{"course_id": b.course, "conversation_id": conv}, "read-once")
	if replay := b.MustCall(b.yuki, "conversation.mark_read", m{"course_id": b.course, "conversation_id": conv}, "read-once"); !replay.Replayed ||
		testkit.Result[tools.ConversationMarkReadOut](t, replay) != testkit.Result[tools.ConversationMarkReadOut](t, again) {
		t.Fatalf("marking read again under the same key: %+v", replay)
	}

	// She asks again, it answers again: unread once more.
	q2 := b.ask(t, b.yuki, conv, "And after that?")
	unread("asked again", false)
	a2 := testkit.Result[tools.MessageIDOut](t, b.do(t, tutor, "conversation.answer", answerArgs(b, conv, q2, "The first exercise."))).MessageID
	unread("answered again", true)
	// Marking an earlier message read leaves what she has read where it
	// was; a time before the answer reads her own question, not the answer.
	if got := b.markRead(t, b.yuki, conv, m{"up_to_message_id": a1}); got.ReadUpToSeq != 2 || !got.Unread {
		t.Fatalf("marking an earlier answer read: %+v", got)
	}
	var before string
	if err := b.Pool.QueryRow(t.Context(), `SELECT to_char((created_at - interval '1 microsecond') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
		FROM conversation_message WHERE id = $1`, a2).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if got := b.markRead(t, b.yuki, conv, m{"up_to": before}); got.ReadUpToSeq != 3 || !got.Unread {
		t.Fatalf("marking read up to just before the answer: %+v", got)
	}
	if got := b.markRead(t, b.yuki, conv, m{"up_to_message_id": a2}); got.ReadUpToSeq != 4 || got.Unread {
		t.Fatalf("marking the answer read: %+v", got)
	}
	unread("read again", false)

	// An answer retracted before she read it is nothing to read.
	q3 := b.ask(t, b.yuki, conv, "And then?")
	a3 := testkit.Result[tools.MessageIDOut](t, b.do(t, tutor, "conversation.answer", answerArgs(b, conv, q3, "Oops, wrong course."))).MessageID
	unread("answered a third time", true)
	b.do(t, b.sato, "conversation.retract", m{"course_id": b.course, "message_id": a3})
	unread("the answer retracted", false)

	// The agent's own place: it reads the questions, and marks them read too.
	if got := b.markRead(t, tutor, conv, m{}); got.ReadUpToSeq != 6 || got.Unread {
		t.Fatalf("the tutor marking read: %+v", got)
	}
	if got := b.conversation(t, tutor, conv); got.Unread == nil || *got.Unread {
		t.Fatalf("the tutor, having read it: %+v", got.Unread)
	}

	// Closed, it is still read.
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
	b.markRead(t, b.yuki, conv, m{})

	// Only a participant has a place: an overseer is refused, anyone else
	// finds nothing, and an argument that says nothing is refused.
	if out := b.MustCall(b.sato, "conversation.mark_read", m{"course_id": b.course, "conversation_id": conv}, "sato-reads"); out.Status != "failed" ||
		out.Error.Code != apperr.Forbidden || reason(out) != "not_a_participant" {
		t.Fatalf("an overseer marking read: %+v", out)
	}
	b.try(t, b.ken, "conversation.mark_read", m{"course_id": b.course, "conversation_id": conv}, apperr.NotFound)
	b.try(t, b.yuki, "conversation.mark_read", m{"course_id": b.course, "conversation_id": conv, "up_to_message_id": a2, "up_to": before}, apperr.InvalidArgument)
	other, _ := b.open(t, b.yuki, c.yukiBot, "Hello")
	b.try(t, b.yuki, "conversation.mark_read", m{"course_id": b.course, "conversation_id": other, "up_to_message_id": a2}, apperr.InvalidArgument)

	// The database holds the same: a place is a participant's, and goes
	// forward only.
	for _, stmt := range []struct{ sql, code string }{
		{`INSERT INTO conversation_read (conversation_id, course_id, member_id, last_read_seq) VALUES ('` + conv.String() + `', '` +
			b.course.String() + `', '` + b.kenM.String() + `', 1)`, "23514"},
		{`UPDATE conversation_read SET last_read_seq = 1 WHERE conversation_id = '` + conv.String() + `' AND member_id = '` + b.yukiM.String() + `'`, "23001"},
		{`UPDATE conversation_read SET member_id = '` + c.courseTutor.String() + `' WHERE conversation_id = '` + conv.String() + `' AND member_id = '` +
			b.yukiM.String() + `'`, "23001"},
	} {
		_, err := b.Pool.Exec(t.Context(), stmt.sql)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != stmt.code {
			t.Fatalf("%s: %v, want %s", stmt.sql, err, stmt.code)
		}
	}
}
