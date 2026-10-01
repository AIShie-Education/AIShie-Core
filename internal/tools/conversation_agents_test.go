package tools_test

import (
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

// Conversations are between a person and an agent: a person asks, an agent
// answers. A person is nobody's respondent, answers nothing, and holds
// conversation_answer at denied, whatever anyone says; people talk to
// people elsewhere.

// withAgents insists a call was refused because conversations are with
// agents: failed, for a person named as a respondent; denied, for a person
// answering, whose seat holds no conversation_answer.
func withAgents(t *testing.T, what string, out pipeline.Outcome) {
	t.Helper()
	if (out.Status != domain.StatusFailed && out.Status != domain.StatusDenied) || out.Error == nil ||
		out.Error.Code != apperr.Forbidden || out.Error.Details["reason"] != "conversations_are_with_agents" {
		t.Fatalf("%s: %+v, want refused conversations_are_with_agents", what, out)
	}
}

func TestAPersonIsNobodysRespondent(t *testing.T) {
	c := newCast(t)
	b := c.built

	// Offered: agents alone, to a student and to the instructor.
	for _, who := range []uuid.UUID{b.yuki, b.sato} {
		for id, r := range b.respondents(t, who) {
			if r.Kind != "agent" {
				t.Fatalf("a person is offered as a respondent: %s %+v", id, r)
			}
		}
	}
	if _, ok := b.respondents(t, b.yuki)[c.courseTutor]; !ok {
		t.Fatal("the course tutor is not offered")
	}

	// Asked: refused, whoever asks whom, and before anything else is
	// looked at; nothing is written.
	for _, tc := range []struct {
		name              string
		asker, respondent uuid.UUID
	}{
		{"a student, the instructor", b.yuki, b.satoM},
		{"a student, a TA", b.yuki, c.taM},
		{"a student, another student", b.yuki, b.kenM},
		{"the instructor, a student", b.sato, b.yukiM},
		{"the instructor, the TA", b.sato, c.taM},
	} {
		withAgents(t, tc.name, b.MustCall(tc.asker, "conversation.open",
			m{"course_id": b.course, "respondent_member_id": tc.respondent, "body": "Hello?"}, "open-"+uuid.NewString()))
	}
	if n := b.Count(`SELECT count(*) FROM conversation`); n != 0 {
		t.Fatalf("%d conversations were opened with people", n)
	}
	// A question that would wait for approval is refused before it is
	// queued: nobody is asked to approve what could never run.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"conversation_ask": "confirm_required"}})
	withAgents(t, "proposing to ask the instructor", b.MustCall(b.yuki, "conversation.open",
		m{"course_id": b.course, "respondent_member_id": b.satoM, "body": "Hello?"}, "propose"))
	if n := b.Count(`SELECT count(*) FROM action WHERE status = 'proposed' AND action_type = 'conversation.open'`); n != 0 {
		t.Fatal("a conversation with a person waits for approval")
	}
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"conversation_ask": "autonomous"}})

	// The database holds it too, for whatever the application misses.
	_, err := b.Pool.Exec(t.Context(), `INSERT INTO conversation (course_id, opener_member_id, respondent_member_id) VALUES ($1, $2, $3)`,
		b.course, b.yukiM, b.satoM)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("the database took a conversation with a person as its respondent: %v", err)
	}

	// An agent is asked, and answers, as ever.
	conv, q := b.open(t, b.yuki, c.courseTutor, "Where do I start?")
	b.do(t, b.seatActor(t, c.courseTutor), "conversation.answer", answerArgs(b, conv, q, "At the reading list."))
}

func TestAPersonAnswersNothing(t *testing.T) {
	c := newCast(t)
	b := c.built
	conv, q := b.open(t, b.yuki, c.courseTutor, "Is HW3 due on Friday?")

	// Neither the one who asked, nor the instructor, nor the TA answers in
	// it: denied, and told why. Nothing is written.
	for who, name := range map[uuid.UUID]string{b.yuki: "the asker", b.sato: "the instructor", c.ta: "the TA"} {
		out := b.MustCall(who, "conversation.answer", answerArgs(b, conv, q, "Yes."), "answer-"+uuid.NewString())
		if out.Status != domain.StatusDenied {
			t.Fatalf("%s answering: %+v", name, out)
		}
		withAgents(t, name+" answering", out)
		// Nor reads an inbox of questions to answer.
		withAgents(t, name+" reading an inbox", b.MustCall(who, "conversation.inbox", m{"course_id": b.course}, ""))
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("a person's answer was written")
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.answer' AND status = 'denied'
		AND result->'error'->'details'->>'reason' = 'conversations_are_with_agents'`); n != 3 {
		t.Fatalf("%d denied answers say why on record", n)
	}
	// An agent that answers nothing here is denied as ever, and not told it
	// is a person.
	if out := b.MustCall(b.grader, "conversation.inbox", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied || reason(out) != "permission_denied" {
		t.Fatalf("an agent that answers nothing, reading an inbox: %+v", out)
	}

	// A conversation from before, when a person could be asked: the person
	// answers nothing in it, and the asker asks nothing more; both read it.
	b.Exec(`ALTER TABLE conversation DISABLE TRIGGER conversation_respondent_is_agent`)
	old := uuid.New()
	b.Exec(`INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id) VALUES ($1, $2, $3, $4)`, old, b.course, b.kenM, c.taM)
	b.Exec(`ALTER TABLE conversation ENABLE TRIGGER conversation_respondent_is_agent`)
	withAgents(t, "asking a person in a conversation from before", b.MustCall(b.ken, "conversation.ask",
		m{"course_id": b.course, "conversation_id": old, "body": "Are you there?"}, "old-ask"))
	b.conversation(t, b.ken, old)
	b.do(t, b.ken, "conversation.close", m{"course_id": b.course, "conversation_id": old})

	// Nobody passes a close of theirs off as the system's.
	b.try(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv, "reason": "conversations_are_with_agents"}, apperr.InvalidArgument)
}

// A person's seat holds conversation_answer at denied at most: the views say
// so, with the reason, as they say every ceiling; naming more is refused
// with that reason; a preset's level is cut down; and the row is written
// denied whatever it is told.
func TestAPersonsSeatAnswersNothing(t *testing.T) {
	c := newCast(t)
	b := c.built
	for _, seat := range []uuid.UUID{b.satoM, b.yukiM, c.taM} {
		v := b.memberView(t, seat)
		if v.Perms["conversation_answer"] != "denied" || v.PermCeilings["conversation_answer"] != "denied" ||
			v.PermCeilingReasons["conversation_answer"] != "conversations_are_with_agents" {
			t.Fatalf("a person's seat: %+v", v)
		}
	}
	for _, who := range []uuid.UUID{b.sato, b.yuki} {
		if me := b.membership(t, who); me.Perms["conversation_answer"] != "denied" || me.PermCeilings["conversation_answer"] != "denied" ||
			me.PermCeilingReasons["conversation_answer"] != "conversations_are_with_agents" {
			t.Fatalf("a person's own membership: %+v", me)
		}
	}
	// An agent's seat answers as it was given, with no ceiling on it.
	for _, seat := range []uuid.UUID{b.tutorM, c.courseTutor} {
		if v := b.memberView(t, seat); v.Perms["conversation_answer"] != "autonomous" || v.PermCeilings["conversation_answer"] != "autonomous" ||
			v.PermCeilingReasons["conversation_answer"] != "" {
			t.Fatalf("an agent's seat: %+v", v)
		}
	}

	// Named above it, refused, however it is asked.
	for _, level := range []string{"confirm_required", "pending_review", "autonomous"} {
		aboveCeiling(t, "raising a student to answer "+level, b.MustCall(b.sato, "member.update_perms",
			m{"course_id": b.course, "member_id": b.kenM, "perms": m{"conversation_answer": level}}, "raise-"+level),
			"conversation_answer", "conversations_are_with_agents")
	}
	aboveCeiling(t, "raising every TA", b.MustCall(b.sato, "member.update_perms_bulk",
		m{"course_id": b.course, "role": "ta", "perms": m{"conversation_answer": "autonomous"}}, "bulk"),
		"conversation_answer", "conversations_are_with_agents")
	newcomer := b.person(t, "Newcomer", "")
	aboveCeiling(t, "seating a person to answer", b.MustCall(b.sato, "member.add",
		m{"course_id": b.course, "actor_id": newcomer, "preset": "student", "perms": m{"conversation_answer": "autonomous"}}, "seat"),
		"conversation_answer", "conversations_are_with_agents")
	// Lowering, and saying what it is, go through.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"conversation_answer": "denied"}})

	// A preset for people that still says more is cut down, not refused.
	b.Exec(`INSERT INTO permission_preset (name, role, student_scope, assignment_scope, perm_document_read, perm_conversation_answer)
		VALUES ('old-helper', 'ta', 'all', 'all', 'autonomous', 'autonomous')`)
	seat := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": newcomer, "preset": "old-helper"})).MemberID
	if v := b.memberView(t, seat); v.Perms["conversation_answer"] != "denied" || v.Perms["document_read"] != "autonomous" {
		t.Fatalf("a person seated with a preset that says they answer: %+v", v.Perms)
	}

	// Told otherwise behind everyone's back, the row says denied; an
	// agent's row is written as it is told.
	b.Exec(`UPDATE course_member SET perm_conversation_answer = 'autonomous' WHERE id = ANY($1)`, []uuid.UUID{b.yukiM, b.satoM, b.graderM})
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = ANY($1) AND perm_conversation_answer = 'denied'`, []uuid.UUID{b.yukiM, b.satoM}); n != 2 {
		t.Fatal("a person's row holds conversation_answer")
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND perm_conversation_answer = 'autonomous'`, b.graderM); n != 1 {
		t.Fatal("an agent's row was not written as it was told")
	}

	// Whoever decides actions lets an agent answer, as far as they decide;
	// someone who manages the members and decides nothing lets none.
	manager := b.person(t, "Manager", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": manager, "preset": "instructor", "perms": m{"action_decide": "denied"}})
	helper := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Helper"})).ActorID
	out := b.MustCall(manager, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "course_tutor"}, "manager-seats")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != "conversation_answer" {
		t.Fatalf("someone who decides nothing letting an agent answer: %+v", out)
	}
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "course_tutor"})
}
