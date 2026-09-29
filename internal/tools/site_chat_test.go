package tools_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Site chat: whether people in the site may start conversations with an
// agent and ask it. What runs the agent says so, with a credential of the
// agent's, and it holds only while that credential is live, the agent active
// and its owner too. Its owner may switch it off, never on.

// runtime makes a call as agent with the token credential, as the program
// that runs it does.
func (b *built) runtime(t *testing.T, agent, credential uuid.UUID, name string, args m) pipeline.Outcome {
	t.Helper()
	out, err := b.CallWith(pipeline.Caller{ActorID: agent, CredentialID: credential}, name, args, "rt-"+uuid.NewString())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// siteChat is whether owner's agent takes conversations in the site, as
// agent.get and agent.list both say it; they must agree.
func (b *built) siteChat(t *testing.T, owner, agent uuid.UUID) bool {
	t.Helper()
	got := testkit.Result[tools.AgentGetOut](t, b.do(t, owner, "agent.get", m{"actor_id": agent})).SiteChat
	for _, a := range testkit.Result[tools.AgentListOut](t, b.do(t, owner, "agent.list", m{})).Agents {
		if a.ActorID == agent && a.SiteChat != got {
			t.Fatalf("agent.get says site_chat %v, agent.list %v", got, a.SiteChat)
		}
	}
	return got
}

// seatSiteChat is what member.get and member.list say of a seat's site
// chat, which must agree: nil for a person's seat.
func (b *built) seatSiteChat(t *testing.T, seat uuid.UUID) *bool {
	t.Helper()
	got := b.memberView(t, seat).SiteChat
	say := func(v *bool) string {
		if v == nil {
			return "nothing"
		}
		return fmt.Sprint(*v)
	}
	for _, v := range testkit.Result[tools.MemberListOut](t, b.do(t, b.sato, "member.list", m{"course_id": b.course, "limit": 200})).Members {
		if v.ID == seat && say(v.SiteChat) != say(got) {
			t.Fatalf("member.get says site_chat %s, member.list %s", say(got), say(v.SiteChat))
		}
	}
	return got
}

func TestWhatRunsAnAgentSaysItTakesConversationsInTheSite(t *testing.T) {
	b := build(t)
	tutor := b.agent(t, b.sato, "Course tutor")
	seat := b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})

	// Nothing has said so: an agent operated from an external tool, as far
	// as the site can tell.
	if b.siteChat(t, b.sato, tutor) {
		t.Fatal("an agent nothing has declared takes conversations in the site")
	}
	if got := b.seatSiteChat(t, seat); got == nil || *got {
		t.Fatalf("its seat's site_chat: %v", got)
	}
	if got := b.seatSiteChat(t, b.yukiM); got != nil {
		t.Fatalf("a person's seat says site_chat: %v", *got)
	}

	// A runtime starts it, with a token of the agent's.
	token := b.SiteChat(tutor)
	if !b.siteChat(t, b.sato, tutor) {
		t.Fatal("the agent its runtime declared does not take conversations in the site")
	}
	if got := b.seatSiteChat(t, seat); got == nil || !*got {
		t.Fatalf("its seat's site_chat, declared: %v", got)
	}
	var who uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT site_chat_credential_id FROM actor WHERE id = $1`, tutor).Scan(&who); err != nil || who != token {
		t.Fatalf("declared with %v (%v), want the runtime's token %v", who, err, token)
	}

	// It stops, and says so; and starts again.
	off := b.runtime(t, tutor, token, "me.site_chat", m{"on": false})
	if off.Status != domain.StatusExecuted || testkit.Result[tools.SiteChatOut](t, off).SiteChat || b.siteChat(t, b.sato, tutor) {
		t.Fatalf("switched off by the agent: %+v", off)
	}
	on := b.runtime(t, tutor, token, "me.site_chat", m{"on": true})
	if on.Status != domain.StatusExecuted || !testkit.Result[tools.SiteChatOut](t, on).SiteChat || !b.siteChat(t, b.sato, tutor) {
		t.Fatalf("switched on again: %+v", on)
	}

	// Only with the credential it calls with, and only for an agent.
	out := b.MustCall(tutor, "me.site_chat", m{"on": true}, "no-token")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.FailedPrecondition || reason(out) != "no_credential" {
		t.Fatalf("declared with no credential: %+v", out)
	}
	yukis := b.session(t, b.yuki)
	out = b.runtime(t, b.yuki, yukis, "me.site_chat", m{"on": true})
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.FailedPrecondition || reason(out) != "not_an_agent" {
		t.Fatalf("a person declaring site chat: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM actor WHERE kind <> 'agent' AND site_chat_credential_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d people hold a declaration", n)
	}
}

// It holds only while the credential that declared it is live, the agent
// active and its owner too: nothing is left behind to say otherwise.
func TestSiteChatLastsNoLongerThanItsCredentialItsAgentAndItsOwner(t *testing.T) {
	b := build(t)
	tutor := b.agent(t, b.sato, "Course tutor")
	b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	on := func(want bool, when string) {
		t.Helper()
		if got := b.siteChat(t, b.sato, tutor); got != want {
			t.Fatalf("%s: site_chat %v, want %v", when, got, want)
		}
	}

	token := b.SiteChat(tutor)
	b.do(t, b.sato, "agent.revoke_credential", m{"actor_id": tutor, "credential_id": token})
	on(false, "its runtime's token revoked")

	token = b.SiteChat(tutor)
	on(true, "declared again with a new token")
	b.Exec(`UPDATE credential SET expires_at = now() - interval '1 second' WHERE id = $1`, token)
	on(false, "its runtime's token expired")

	b.SiteChat(tutor)
	b.do(t, b.sato, "agent.suspend", m{"actor_id": tutor})
	on(false, "the agent suspended")
	b.do(t, b.sato, "agent.reactivate", m{"actor_id": tutor})
	on(true, "the agent reactivated, its runtime's token still working")

	// Yuki's own agent, while she is suspended, as the instructor sees its
	// seat.
	helper := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, helper, m{})
	b.SiteChat(helper)
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.yuki})
	if got := b.seatSiteChat(t, seat); got == nil || *got {
		t.Fatalf("its owner suspended, the agent's seat says site_chat %v", got)
	}
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": b.yuki})
	if !b.siteChat(t, b.yuki, helper) {
		t.Fatal("its owner reactivated, the agent does not take conversations in the site")
	}
}

// Its owner switches it off, and never on: only what runs the agent knows
// that it answers.
func TestAnOwnerSwitchesSiteChatOffAndNeverOn(t *testing.T) {
	b := build(t)
	tutor := b.agent(t, b.sato, "Course tutor")
	b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	token := b.SiteChat(tutor)

	b.try(t, b.ken, "agent.update", m{"actor_id": tutor, "site_chat": false}, apperr.NotFound)
	b.do(t, b.sato, "agent.update", m{"actor_id": tutor, "site_chat": false})
	if b.siteChat(t, b.sato, tutor) {
		t.Fatal("switched off by its owner, the agent still takes conversations in the site")
	}
	b.try(t, b.sato, "agent.update", m{"actor_id": tutor, "site_chat": true}, apperr.InvalidArgument)
	if b.siteChat(t, b.sato, tutor) {
		t.Fatal("its owner switched it on")
	}
	b.try(t, b.sato, "agent.update", m{"actor_id": tutor}, apperr.InvalidArgument)
	b.do(t, b.sato, "agent.update", m{"actor_id": tutor, "display_name": "CS101 tutor"})
	if got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.sato, "agent.get", m{"actor_id": tutor})); got.DisplayName != "CS101 tutor" || got.SiteChat {
		t.Fatalf("renamed: %+v", got)
	}

	// What runs it says so again, with the token it still has.
	if out := b.runtime(t, tutor, token, "me.site_chat", m{"on": true}); !testkit.Result[tools.SiteChatOut](t, out).SiteChat {
		t.Fatalf("declared again by its runtime: %+v", out)
	}
}

// refusedElsewhere insists a call was refused as a question to an agent that
// takes no conversations in the site.
func refusedElsewhere(t *testing.T, what string, out pipeline.Outcome) {
	t.Helper()
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.FailedPrecondition || reason(out) != "agent_answers_elsewhere" {
		t.Fatalf("%s: %+v, want failed_precondition agent_answers_elsewhere", what, out)
	}
}

// An agent nothing runs here is asked nothing here: it is not offered, and a
// conversation with it is neither opened nor carried on. What was written
// stays readable, and may be closed and retracted, and the agent answers
// what it was asked. People are asked as ever.
func TestAnAgentOperatedFromOutsideIsNotAskedInTheSite(t *testing.T) {
	b := build(t)
	tutor := b.agent(t, b.sato, "Course tutor")
	seat := b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	opening := m{"course_id": b.course, "respondent_member_id": seat, "body": "What does HW3 ask for?"}

	// Nothing has said it answers here.
	if _, ok := b.respondents(t, b.yuki)[seat]; ok {
		t.Fatal("an agent operated from outside is offered to Yuki")
	}
	refusedElsewhere(t, "opening a conversation with it", b.MustCall(b.yuki, "conversation.open", opening, "before"))

	// Its runtime starts: it is offered, asked, and answers.
	token := b.SiteChat(tutor)
	if _, ok := b.respondents(t, b.yuki)[seat]; !ok {
		t.Fatal("an agent whose runtime says it answers here is not offered to Yuki")
	}
	conv, first := b.open(t, b.yuki, seat, "What does HW3 ask for?")
	if in := b.inbox(t, tutor); len(in) != 1 || in[0].ID != conv {
		t.Fatalf("the tutor's inbox: %+v", in)
	}
	b.do(t, tutor, "conversation.answer", answerArgs(b, conv, first, "An essay with a thesis."))
	second := b.ask(t, b.yuki, conv, "How long should it be?")

	// Its runtime's token is revoked, as when its hosting ends.
	b.do(t, b.sato, "agent.revoke_credential", m{"actor_id": tutor, "credential_id": token})
	if _, ok := b.respondents(t, b.yuki)[seat]; ok {
		t.Fatal("an agent whose runtime's token is revoked is still offered")
	}
	refusedElsewhere(t, "asking it more", b.MustCall(b.yuki, "conversation.ask",
		m{"course_id": b.course, "conversation_id": conv, "body": "Are you there?"}, "after-ask"))
	refusedElsewhere(t, "opening another", b.MustCall(b.yuki, "conversation.open", opening, "after-open"))

	// What was written stays: read by both, answered, retracted, closed.
	if msgs := b.messages(t, b.yuki, conv); len(msgs) != 3 || msgs[2].ID != second {
		t.Fatalf("the conversation, as Yuki reads it: %+v", msgs)
	}
	if got := b.conversation(t, tutor, conv); got.State != tools.StateAwaitingAnswer {
		t.Fatalf("the conversation, as the tutor reads it: %+v", got)
	}
	if in := b.inbox(t, tutor); len(in) != 1 || in[0].ID != conv {
		t.Fatalf("what the tutor was asked is gone from its inbox: %+v", in)
	}
	b.do(t, tutor, "conversation.answer", answerArgs(b, conv, second, "About a thousand words."))
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": second})
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})

	// A person answers nothing, in the site or anywhere: Mori, a TA, is not
	// seated answering, is offered to nobody, and is asked nothing, since
	// conversations are with agents.
	mori := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Mori"})).ActorID
	aboveCeiling(t, "seating a person to answer", b.MustCall(b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta",
		"perms": m{"conversation_answer": "autonomous"}}, "mori-answers"), "conversation_answer", "conversations_are_with_agents")
	moriM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta"})).MemberID
	if _, ok := b.respondents(t, b.sato)[moriM]; ok {
		t.Fatal("a person is offered as a respondent")
	}
	withAgents(t, "asking a person", b.MustCall(b.sato, "conversation.open",
		m{"course_id": b.course, "respondent_member_id": moriM, "body": "Can you mark HW3 by Friday?"}, "ask-mori"))
}

// Its owner's word, or a suspension, takes an agent out of the site's
// conversations: it is neither offered nor asked more. What runs it says so
// again to bring it back after its owner's word; a suspension lifted brings
// it back by itself, its runtime's token still working.
func TestAnOwnerOrASuspensionTakesAnAgentOutOfTheSite(t *testing.T) {
	c := newCast(t)
	b := c.built
	tutor := b.seatActor(t, c.courseTutor)
	conv, _ := b.open(t, b.ken, c.courseTutor, "Is the midterm open book?")
	offered := func(want bool, when string) {
		t.Helper()
		if _, ok := b.respondents(t, b.ken)[c.courseTutor]; ok != want {
			t.Fatalf("%s: offered to Ken %v, want %v", when, ok, want)
		}
	}
	ask := m{"course_id": b.course, "conversation_id": conv, "body": "Hello?"}

	b.do(t, b.sato, "agent.update", m{"actor_id": tutor, "site_chat": false})
	offered(false, "switched off by its owner")
	refusedElsewhere(t, "asking it, switched off by its owner", b.MustCall(b.ken, "conversation.ask", ask, "owner-off"))
	refusedElsewhere(t, "opening with it, switched off by its owner", b.MustCall(b.ken, "conversation.open",
		m{"course_id": b.course, "respondent_member_id": c.courseTutor, "body": "Hello?"}, "owner-off-open"))
	b.try(t, b.sato, "agent.update", m{"actor_id": tutor, "site_chat": true}, apperr.InvalidArgument)
	offered(false, "its owner trying to switch it on")

	b.SiteChat(tutor)
	offered(true, "its runtime saying so again")
	b.ask(t, b.ken, conv, "And is the final?")

	b.do(t, b.sato, "agent.suspend", m{"actor_id": tutor})
	offered(false, "the agent suspended")
	b.do(t, b.sato, "agent.reactivate", m{"actor_id": tutor})
	offered(true, "the agent reactivated")
}
