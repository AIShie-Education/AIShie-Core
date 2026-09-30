package tools_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// Drafts: while an agent writes an answer, whoever reads the conversation
// watches it come — what the agent is doing, and the text, where they would
// see the answer once posted. Only the respondent writes one, while the
// conversation waits for its answer; nothing of it is recorded; the answer,
// posted or proposed, takes its place.

func draftArgs(b *built, conversation uuid.UUID, attempt string, version int, extra m) m {
	a := m{"course_id": b.course, "conversation_id": conversation, "attempt": attempt, "version": version}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// draft writes a draft as actor, insisting it was carried out, and not as an
// action.
func (b *built) draft(t *testing.T, actor, conversation uuid.UUID, attempt string, version int, extra m) tools.ConversationDraftOut {
	t.Helper()
	out := b.do(t, actor, "conversation.draft", draftArgs(b, conversation, attempt, version, extra))
	if out.ActionID != nil || out.ReviewState != "" {
		t.Fatalf("a draft came back as an action: %+v", out)
	}
	return testkit.Result[tools.ConversationDraftOut](t, out)
}

// refused is what a call that did not go through says: its error, whether it
// was refused before it was attempted or denied.
func (b *built) refused(t *testing.T, actor uuid.UUID, name string, args m) *apperr.Error {
	t.Helper()
	out, err := b.Call(actor, name, args, "")
	if err != nil {
		e, ok := apperr.As(err)
		if !ok {
			t.Fatalf("%s: %v", name, err)
		}
		return e
	}
	if out.Status == domain.StatusExecuted || out.Status == domain.StatusProposed || out.Error == nil {
		t.Fatalf("%s went through: %+v", name, out)
	}
	if out.ActionID != nil {
		t.Fatalf("%s was recorded: %+v", name, out)
	}
	return out.Error
}

func wantRefusal(t *testing.T, e *apperr.Error, code apperr.Code, reason string) {
	t.Helper()
	if e.Code != code || (reason != "" && e.Details["reason"] != reason) {
		t.Fatalf("refused %s %v (%s), want %s %s", e.Code, e.Details["reason"], e.Message, code, reason)
	}
}

// seen is the draft of a conversation as actor reads it; conversation.get
// and conversation.messages say the same of it.
func (b *built) seen(t *testing.T, actor, conversation uuid.UUID) *tools.DraftView {
	t.Helper()
	args := m{"course_id": b.course, "conversation_id": conversation}
	got := testkit.Result[tools.ConversationGetOut](t, b.do(t, actor, "conversation.get", args)).Draft
	read := testkit.Result[tools.ConversationMessagesOut](t, b.do(t, actor, "conversation.messages", args)).Draft
	if !reflect.DeepEqual(got, read) {
		t.Fatalf("conversation.get says %+v of the draft, conversation.messages %+v", got, read)
	}
	return got
}

func steps(s ...string) []m {
	out := make([]m, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		out = append(out, m{"kind": s[i], "state": s[i+1]})
	}
	return out
}

// unbounded is build on a platform whose drafts are written as often as
// anyone likes, for a test that writes more than DraftWritesPerSecond of
// them in a second and is about something else.
func unbounded(t *testing.T) *built {
	t.Helper()
	return buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Drafts = ratelimit.New(60_000_000, 1_000_000) }))
}

func (b *built) drafts(t *testing.T, conversation uuid.UUID) int {
	t.Helper()
	return b.Count(`SELECT count(*) FROM conversation_draft WHERE conversation_id = $1`, conversation)
}

// Only the respondent writes a conversation's draft, while it waits for an
// answer; nobody else, and not once it is answered or closed. Nothing of it
// is recorded: no action, refused or not, and no event.
func TestWhoWritesADraft(t *testing.T) {
	c := newCast(t)
	b := c.built
	conv, q := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	actions, events := b.Count(`SELECT count(*) FROM action`), b.Count(`SELECT count(*) FROM event`)

	got := b.draft(t, b.tutor, conv, "a1", 1, m{"text": "Start with", "steps": []m{{"kind": "reading_document", "target": " HW3.pdf ", "state": "done"},
		{"kind": "writing", "state": "running"}}})
	if !got.Stored || got.Version != 1 {
		t.Fatalf("the respondent's draft: %+v", got)
	}
	d := b.seen(t, b.yuki, conv)
	if d == nil || d.Attempt != "a1" || d.Version != 1 || d.Text == nil || *d.Text != "Start with" || d.TextHidden ||
		len(d.Steps) != 2 || d.Steps[0].Target == nil || *d.Steps[0].Target != "HW3.pdf" || d.Steps[1].Target != nil || d.Steps[1].State != "running" {
		t.Fatalf("the draft, as Yuki reads it: %+v", d)
	}

	// Nobody else: the one who asked, a person, answers nothing; nor does
	// another person; an agent that answers here is not this one's
	// respondent; one that answers nothing is denied.
	wantRefusal(t, b.refused(t, b.yuki, "conversation.draft", draftArgs(b, conv, "a1", 2, nil)), apperr.Forbidden,
		string(domain.CeilingConversationsAreWithAgents))
	wantRefusal(t, b.refused(t, b.ken, "conversation.draft", draftArgs(b, conv, "a1", 2, nil)), apperr.Forbidden,
		string(domain.CeilingConversationsAreWithAgents))
	wantRefusal(t, b.refused(t, b.seatActor(t, c.courseTutor), "conversation.draft", draftArgs(b, conv, "a1", 2, nil)), apperr.Forbidden,
		"not_the_respondent")
	wantRefusal(t, b.refused(t, b.grader, "conversation.draft", draftArgs(b, conv, "a1", 2, nil)), apperr.Forbidden, "permission_denied")
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, uuid.New(), "a1", 2, nil)), apperr.NotFound, "")

	// What it is written with is held to what it may be.
	long := func(n int) string { return strings.Repeat("字", n) }
	for name, args := range map[string]m{
		"no attempt":               draftArgs(b, conv, "", 2, nil),
		"a long attempt":           draftArgs(b, conv, long(65), 2, nil),
		"an attempt on two lines":  draftArgs(b, conv, "a\n1", 2, nil),
		"version 0":                draftArgs(b, conv, "a1", 0, nil),
		"a long text":              draftArgs(b, conv, "a1", 2, m{"text": long(20001)}),
		"too many steps":           draftArgs(b, conv, "a1", 2, m{"steps": steps(strings.Split(strings.Repeat("thinking done ", 21), " ")...)}),
		"a step of no known kind":  draftArgs(b, conv, "a1", 2, m{"steps": steps("dreaming", "running")}),
		"a step in no known state": draftArgs(b, conv, "a1", 2, m{"steps": steps("thinking", "paused")}),
		"a long target":            draftArgs(b, conv, "a1", 2, m{"steps": []m{{"kind": "tool", "state": "done", "target": long(121)}}}),
		"a target on two lines":    draftArgs(b, conv, "a1", 2, m{"steps": []m{{"kind": "tool", "state": "done", "target": "a\nb"}}}),
		"a field it does not take": draftArgs(b, conv, "a1", 2, m{"idempotency_key": "k"}),
	} {
		t.Run(name, func(t *testing.T) {
			wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", args), apperr.InvalidArgument, "")
		})
	}
	// At the bounds, it is taken.
	b.draft(t, b.tutor, conv, long(64), 1, m{"text": long(20000), "steps": []m{{"kind": "tool", "state": "done", "target": long(120)}}})
	b.draft(t, b.tutor, conv, "a1", 2, m{"text": "", "steps": steps(strings.Split(strings.Repeat("thinking done ", 20), " ")...)})
	if d := b.seen(t, b.tutor, conv); d == nil || d.Text == nil || *d.Text != "" || len(d.Steps) != 20 {
		t.Fatalf("a draft at the bounds: %+v", d)
	}

	if n := b.Count(`SELECT count(*) FROM action`); n != actions {
		t.Fatalf("%d actions recorded for drafts", n-actions)
	}
	if n := b.Count(`SELECT count(*) FROM event`); n != events {
		t.Fatalf("%d events written for drafts", n-events)
	}

	// Answered, the conversation waits for nothing, and takes no draft;
	// nor does one closed.
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Start with chapter one."))
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a2", 1, nil)), apperr.Conflict, "conversation_not_awaiting")
	b.ask(t, b.yuki, conv, "And then?")
	b.draft(t, b.tutor, conv, "a2", 1, nil)
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a2", 2, nil)), apperr.Conflict, "conversation_not_awaiting")

	// Once the opener may no longer address the respondent, it writes no
	// draft for them, as it would post no answer.
	conv, _ = b.open(t, b.yuki, b.tutorM, "One more thing")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"member_read": "autonomous"}})
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 1, nil)), apperr.Forbidden, "not_addressable")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"member_read": "denied"}})
	b.draft(t, b.tutor, conv, "a1", 1, nil)

	// An archived course takes no draft, as it takes no write.
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 2, nil)), apperr.Forbidden, "course_archived")
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.draft'`); n != 0 {
		t.Fatalf("%d drafts recorded", n)
	}
}

// A write that is not newer than the draft kept is passed over: a lower or
// the same version of the attempt, anything of an attempt that is over, the
// end of an attempt that is not the one kept. What a write leaves out, the
// attempt keeps; a new attempt starts from nothing.
func TestDraftWritesArePassedOverUnlessNewer(t *testing.T) {
	b := unbounded(t)
	conv, _ := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	text := func(d *tools.DraftView) string {
		if d == nil || d.Text == nil {
			return "<none>"
		}
		return *d.Text
	}

	if got := b.draft(t, b.tutor, conv, "a1", 2, m{"text": "two", "steps": steps("thinking", "done")}); !got.Stored || got.Version != 2 {
		t.Fatalf("version 2: %+v", got)
	}
	for _, v := range []int{1, 2} {
		if got := b.draft(t, b.tutor, conv, "a1", v, m{"text": "late"}); got.Stored || got.Version != 2 {
			t.Fatalf("version %d after 2: %+v", v, got)
		}
	}
	if d := b.seen(t, b.yuki, conv); text(d) != "two" || d.Version != 2 {
		t.Fatalf("after late writes: %+v", d)
	}
	// Steps alone keep the text; text alone keeps the steps.
	b.draft(t, b.tutor, conv, "a1", 3, m{"steps": steps("thinking", "done", "writing", "running")})
	if d := b.seen(t, b.yuki, conv); text(d) != "two" || len(d.Steps) != 2 {
		t.Fatalf("steps alone: %+v", d)
	}
	b.draft(t, b.tutor, conv, "a1", 4, m{"text": "four"})
	if d := b.seen(t, b.yuki, conv); text(d) != "four" || len(d.Steps) != 2 || d.Version != 4 {
		t.Fatalf("text alone: %+v", d)
	}
	// Another attempt replaces it whole, whatever its version.
	if got := b.draft(t, b.tutor, conv, "a2", 1, m{"steps": steps("thinking", "running")}); !got.Stored {
		t.Fatalf("a new attempt: %+v", got)
	}
	if d := b.seen(t, b.yuki, conv); d.Attempt != "a2" || d.Version != 1 || d.Text != nil || len(d.Steps) != 1 {
		t.Fatalf("a new attempt, before it writes text: %+v", d)
	}
	// The end of an attempt that is not the one kept ends nothing.
	if got := b.draft(t, b.tutor, conv, "a1", 9, m{"done": true}); got.Stored || got.Version != 1 {
		t.Fatalf("the end of an earlier attempt: %+v", got)
	}
	if d := b.seen(t, b.yuki, conv); d == nil || d.Attempt != "a2" {
		t.Fatalf("after the end of an earlier attempt: %+v", d)
	}
	// Its own end, at its version, ends it: the draft is gone, and what of
	// the attempt comes late is passed over.
	b.draft(t, b.tutor, conv, "a2", 2, m{"text": "two"})
	if got := b.draft(t, b.tutor, conv, "a2", 2, m{"done": true}); !got.Stored || got.Version != 0 {
		t.Fatalf("the attempt's end: %+v", got)
	}
	if d := b.seen(t, b.yuki, conv); d != nil {
		t.Fatalf("the draft of an attempt that ended: %+v", d)
	}
	for _, v := range []int{1, 3} {
		if got := b.draft(t, b.tutor, conv, "a2", v, m{"text": "late"}); got.Stored || got.Version != 0 {
			t.Fatalf("version %d of an attempt that ended: %+v", v, got)
		}
	}
	if got := b.draft(t, b.tutor, conv, "a2", 3, m{"done": true}); got.Stored {
		t.Fatalf("an attempt ended twice: %+v", got)
	}
	if d := b.seen(t, b.yuki, conv); d != nil {
		t.Fatalf("a late write brought an ended attempt back: %+v", d)
	}
	// The next attempt is shown.
	b.draft(t, b.tutor, conv, "a3", 1, m{"text": "three"})
	if d := b.seen(t, b.yuki, conv); text(d) != "three" {
		t.Fatalf("the next attempt: %+v", d)
	}
}

// The draft goes when the answer takes its place, posted or proposed, and
// when the conversation is closed, by a participant or by a seat's removal.
// One nobody has written for two minutes is none, and is replaced by any
// write.
func TestADraftGoesWithWhatTakesItsPlace(t *testing.T) {
	b := build(t)
	now := time.Now()
	b.P.SetClock(func() time.Time { return now })

	// Posted.
	conv, q := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	b.draft(t, b.tutor, conv, "a1", 1, m{"text": "Start"})
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Start with chapter one."))
	if n := b.drafts(t, conv); n != 0 || b.seen(t, b.yuki, conv) != nil {
		t.Fatal("the draft outlived the answer posted")
	}

	// Proposed: gone, and not back when the proposal is rejected, which
	// leaves the question waiting again.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	q = b.ask(t, b.yuki, conv, "And then?")
	b.draft(t, b.tutor, conv, "a1", 1, m{"text": "Then"})
	proposed := b.MustCall(b.tutor, "conversation.answer", answerArgs(b, conv, q, "Then chapter two."), "answer:1")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the answer: %+v", proposed)
	}
	if n := b.drafts(t, conv); n != 0 {
		t.Fatal("the draft outlived the answer proposed")
	}
	if got := b.conversation(t, b.yuki, conv); got.State != tools.StateReplyPendingApproval || got.Draft != nil {
		t.Fatalf("the conversation, its answer waiting: %+v", got)
	}
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 2, nil)), apperr.Conflict, "conversation_not_awaiting")
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "reject", "reason": "Too short."})
	if got := b.conversation(t, b.yuki, conv); got.State != tools.StateAwaitingAnswer || got.Draft != nil {
		t.Fatalf("the conversation, its answer rejected: %+v", got)
	}

	// Stale: a draft nobody has written for two minutes is none, and any
	// write takes its place, whatever it was.
	b.draft(t, b.tutor, conv, "a2", 5, m{"text": "Again"})
	now = now.Add(tools.DraftTTL + time.Second)
	if d := b.seen(t, b.yuki, conv); d != nil {
		t.Fatalf("a stale draft: %+v", d)
	}
	if got := b.draft(t, b.tutor, conv, "a2", 1, nil); !got.Stored || got.Version != 1 {
		t.Fatalf("a write over a stale draft: %+v", got)
	}
	if d := b.seen(t, b.yuki, conv); d == nil || d.Version != 1 || d.Text != nil {
		t.Fatalf("what replaced a stale draft: %+v", d)
	}

	// Closed by a participant.
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
	if n := b.drafts(t, conv); n != 0 {
		t.Fatal("the draft outlived the conversation's closing")
	}

	// Closed by the removal of the opener's seat.
	conv, _ = b.open(t, b.yuki, b.tutorM, "Last one")
	b.draft(t, b.tutor, conv, "a1", 1, m{"text": "Well"})
	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM})
	if n := b.drafts(t, conv); n != 0 {
		t.Fatal("the draft outlived the removal of the opener's seat")
	}
}

// At most DraftWritesPerSecond writes a second per conversation, counted
// only once the caller is known to write it: nobody else spends its
// allowance, and each conversation has its own.
func TestADraftIsWrittenAtMostTenTimesASecond(t *testing.T) {
	now := time.Now()
	limit := ratelimit.New(60*tools.DraftWritesPerSecond, tools.DraftWritesPerSecond)
	limit.SetClock(func() time.Time { return now })
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Drafts = limit }))
	conv, _ := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	other, _ := b.open(t, b.yuki, b.tutorM, "And another thing")

	for range 3 * tools.DraftWritesPerSecond {
		wantRefusal(t, b.refused(t, b.yuki, "conversation.draft", draftArgs(b, conv, "a1", 1, nil)), apperr.Forbidden, "")
	}
	for v := 1; v <= tools.DraftWritesPerSecond; v++ {
		b.draft(t, b.tutor, conv, "a1", v, m{"text": strings.Repeat("x", v)})
	}
	e := b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 11, nil))
	wantRefusal(t, e, apperr.RateLimited, "draft_rate")
	if e.Details["retry_after_seconds"] != 1 {
		t.Fatalf("when to try again: %+v", e.Details)
	}
	b.draft(t, b.tutor, other, "a1", 1, nil)

	now = now.Add(time.Second / tools.DraftWritesPerSecond)
	b.draft(t, b.tutor, conv, "a1", 11, nil)
	wantRefusal(t, b.refused(t, b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 12, nil)), apperr.RateLimited, "draft_rate")
}

// Who sees a draft's text is who would see the answer: while the
// respondent's answers are posted at once, everyone who reads the
// conversation; otherwise its writer and whoever would decide the answer,
// and the others — its opener, and staff overseeing the opener who would
// not decide it — its steps alone, and that its text is held back. Whoever
// may not read the conversation finds none.
func TestWhoSeesADraftsText(t *testing.T) {
	c := newCast(t)
	b := c.built
	sees := func(t *testing.T, actor, conv uuid.UUID, text bool) {
		t.Helper()
		d := b.seen(t, actor, conv)
		if d == nil || len(d.Steps) != 1 || d.Steps[0].Kind != "reading_document" {
			t.Fatalf("the draft, its steps: %+v", d)
		}
		switch {
		case text && (d.Text == nil || *d.Text != "So far" || d.TextHidden):
			t.Fatalf("the draft, its text shown: %+v", d)
		case !text && (d.Text != nil || !d.TextHidden):
			t.Fatalf("the draft, its text held back: %+v", d)
		}
	}
	unread := func(t *testing.T, actor, conv uuid.UUID) {
		t.Helper()
		for _, name := range []string{"conversation.get", "conversation.messages"} {
			wantRefusal(t, b.refused(t, actor, name, m{"course_id": b.course, "conversation_id": conv}), apperr.NotFound, "")
		}
	}
	level := func(t *testing.T, seat uuid.UUID, l string) {
		t.Helper()
		b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"conversation_answer": l}})
	}
	drafted := func(t *testing.T, opener, respondent uuid.UUID) uuid.UUID {
		t.Helper()
		conv, _ := b.open(t, opener, respondent, "Where do I start?")
		b.draft(t, b.seatActor(t, respondent), conv, "a1", 1, m{"text": "So far", "steps": []m{{"kind": "reading_document", "target": "HW3", "state": "running"}}})
		return conv
	}
	// Mori, a TA, decides nothing, and so oversees nobody; given
	// action_decide, he oversees every student and decides what they are
	// answered, being of nobody's party.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": c.taM, "perms": m{"action_decide": "autonomous"}})

	t.Run("the tutor listed for Yuki, answering without approval", func(t *testing.T) {
		conv := drafted(t, b.yuki, b.tutorM)
		sees(t, b.yuki, conv, true)  // the opener
		sees(t, b.sato, conv, true)  // an overseer
		sees(t, b.tutor, conv, true) // the respondent
		sees(t, c.ta, conv, true)
		unread(t, b.ken, conv)
	})
	t.Run("the tutor listed for Yuki, whose answers wait for approval", func(t *testing.T) {
		level(t, b.tutorM, "confirm_required")
		defer level(t, b.tutorM, "autonomous")
		conv := drafted(t, b.yuki, b.tutorM)
		sees(t, b.yuki, conv, false) // the opener
		sees(t, b.sato, conv, true)  // a decider
		sees(t, c.ta, conv, true)    // a decider
		sees(t, b.tutor, conv, true) // the respondent, who writes it
		unread(t, b.ken, conv)
	})
	t.Run("the tutor listed for Yuki, whose answers are reviewed after", func(t *testing.T) {
		level(t, b.tutorM, "pending_review")
		defer level(t, b.tutorM, "autonomous")
		conv := drafted(t, b.yuki, b.tutorM)
		sees(t, b.yuki, conv, false)
		sees(t, b.sato, conv, true)
	})
	t.Run("Sato's own course tutor, whose answers wait for approval", func(t *testing.T) {
		level(t, c.courseTutor, "confirm_required")
		defer level(t, c.courseTutor, "autonomous")
		conv := drafted(t, b.ken, c.courseTutor)
		sees(t, b.ken, conv, false) // the opener
		sees(t, b.sato, conv, true) // its owner, who decides freely, decides its answers
		sees(t, c.ta, conv, true)   // a decider of no party of its
		// Sato's own decisions wait for a confirmation: his tutor's answers
		// are someone else's to decide, and he oversees Ken as anyone else
		// who would not decide them: he sees what Ken sees.
		b.Exec(`UPDATE course_member SET perm_action_decide = 'confirm_required' WHERE id = $1`, b.satoM)
		sees(t, b.sato, conv, false)
		b.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = $1`, b.satoM)
		// He asks it himself: he opened it, and would decide the answer.
		mine := drafted(t, b.sato, c.courseTutor)
		sees(t, b.sato, mine, true)
		unread(t, b.ken, mine)
		sees(t, c.ta, mine, true)
	})
	t.Run("Sato's own course tutor, answering without approval", func(t *testing.T) {
		conv := drafted(t, b.ken, c.courseTutor)
		sees(t, b.ken, conv, true)
		b.Exec(`UPDATE course_member SET perm_action_decide = 'confirm_required' WHERE id = $1`, b.satoM)
		defer b.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = $1`, b.satoM)
		sees(t, b.sato, conv, true) // an overseer sees what the opener sees
		unread(t, b.yuki, conv)
	})
	t.Run("Yuki's own agent, whose answers wait for approval", func(t *testing.T) {
		level(t, c.yukiBot, "confirm_required")
		defer level(t, c.yukiBot, "autonomous")
		conv := drafted(t, b.yuki, c.yukiBot)
		sees(t, b.yuki, conv, false) // its owner decides nothing here
		sees(t, b.sato, conv, true)
		sees(t, c.bot, conv, true)
	})
}

// A reader of a conversation that says what it has seen of the draft
// (seen_draft_version) hears it written, and gone, as soon as it is; a
// write passed over is nothing new; a reader that says nothing of drafts is
// woken by none, and hears the answer.
func TestAWaitingReaderHearsTheDraft(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	conv, q := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	args := func(seen ...int) m {
		a := m{"course_id": b.course, "conversation_id": conv, "after_seq": 1, "wait_s": 10, "seen_state": tools.StateAwaitingAnswer}
		if len(seen) > 0 {
			a["seen_draft_version"] = seen[0]
		}
		return a
	}
	draftIn := func(r read) *tools.DraftView {
		t.Helper()
		got := resultOf[tools.ConversationMessagesOut](t, r)
		if len(got.Messages) != 0 {
			t.Fatalf("messages: %+v", got.Messages)
		}
		return got.Draft
	}

	watcher := b.reading(context.Background(), b.yuki, "conversation.messages", args(0))
	waitingNow(t, hub, 1)
	notYet(t, watcher, 200*time.Millisecond)
	b.draft(t, b.tutor, conv, "a1", 1, m{"text": "Start"})
	if d := draftIn(answered(t, watcher, 5*time.Second)); d == nil || d.Version != 1 || *d.Text != "Start" {
		t.Fatalf("the first draft: %+v", d)
	}

	watcher = b.reading(context.Background(), b.yuki, "conversation.messages", args(1))
	waitingNow(t, hub, 1)
	b.draft(t, b.tutor, conv, "a1", 1, m{"text": "late"}) // passed over, and heard by nobody
	notYet(t, watcher, 300*time.Millisecond)
	b.draft(t, b.tutor, conv, "a1", 2, m{"text": "Start with"})
	if d := draftIn(answered(t, watcher, 5*time.Second)); d == nil || d.Version != 2 || *d.Text != "Start with" {
		t.Fatalf("the second draft: %+v", d)
	}

	// Said to have seen what it has not, it is answered at once.
	start := time.Now()
	if d := draftIn(answered(t, b.reading(context.Background(), b.yuki, "conversation.messages", args(1)), 5*time.Second)); d == nil ||
		d.Version != 2 || time.Since(start) > time.Second {
		t.Fatalf("a draft not yet seen: %v, %+v", time.Since(start), d)
	}

	// One reader says nothing of drafts, another has seen version 2: the
	// draft written wakes the second alone; its end, too.
	plain := b.reading(context.Background(), b.yuki, "conversation.messages", args())
	watcher = b.reading(context.Background(), b.yuki, "conversation.messages", args(2))
	waitingNow(t, hub, 2)
	b.draft(t, b.tutor, conv, "a1", 3, m{"text": "Start with chapter"})
	if d := draftIn(answered(t, watcher, 5*time.Second)); d == nil || d.Version != 3 {
		t.Fatalf("the third draft: %+v", d)
	}
	watcher = b.reading(context.Background(), b.yuki, "conversation.messages", args(3))
	waitingNow(t, hub, 2)
	b.draft(t, b.tutor, conv, "a1", 4, m{"done": true})
	if d := draftIn(answered(t, watcher, 5*time.Second)); d != nil {
		t.Fatalf("the draft, its attempt over: %+v", d)
	}
	notYet(t, plain, 300*time.Millisecond)
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Start with chapter one."))
	if got := resultOf[tools.ConversationMessagesOut](t, answered(t, plain, 5*time.Second)); len(got.Messages) != 1 || got.Draft != nil {
		t.Fatalf("the answer: %+v", got)
	}
	waitingNow(t, hub, 0)

	// Nothing holds a draft's reader to anything its reading does not.
	if _, err := b.Call(b.yuki, "conversation.messages", m{"course_id": b.course, "conversation_id": conv, "seen_draft_version": -1}, ""); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("seen_draft_version -1: %v", err)
	}
}

// A draft and what takes its place never pass each other: a draft written
// while the answer is being posted or proposed, the question withdrawn, or
// the conversation closed, waits for it and finds nothing to wait for; the
// answer, the withdrawal, or the close, made while a draft is being written
// waits for it and deletes it.
func TestADraftAndItsAnswerNeverPassEachOther(t *testing.T) {
	b := unbounded(t)
	ctx := context.Background()
	conv, _ := b.open(t, b.yuki, b.tutorM, "Where do I start?")

	// Something holds the conversation as posting an answer does
	// (TouchConversation), and posts it: the draft under way waits, and is
	// refused once it is posted.
	tx, err := b.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE conversation SET last_message_at = now(), last_author_member_id = respondent_member_id WHERE id = $1`, conv); err != nil {
		t.Fatal(err)
	}
	refused := make(chan error, 1)
	go func() {
		_, err := b.Call(b.tutor, "conversation.draft", draftArgs(b, conv, "a1", 1, m{"text": "Start"}), "")
		refused <- err
	}()
	b.blocked(t, 1, nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-refused:
		if e, ok := apperr.As(err); !ok || e.Details["reason"] != "conversation_not_awaiting" {
			t.Fatalf("a draft written while its answer was posted: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the draft never came back")
	}
	if n := b.drafts(t, conv); n != 0 {
		t.Fatal("a draft outlived the answer it waited for")
	}

	// The opener's question being withdrawn: the retraction holds the
	// conversation as LockConversationForAnswer does and has written its
	// row; the draft under way waits, and is refused once it is withdrawn.
	withdrawing, q := b.open(t, b.yuki, b.tutorM, "Never mind")
	var action uuid.UUID
	if err := b.Pool.QueryRow(ctx, `SELECT created_by_action_id FROM conversation_message WHERE id = $1`, q).Scan(&action); err != nil {
		t.Fatal(err)
	}
	wtx, err := b.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wtx.Rollback(ctx) }()
	if _, err := wtx.Exec(ctx, `SELECT 1 FROM conversation WHERE id = $1 FOR NO KEY UPDATE`, withdrawing); err != nil {
		t.Fatal(err)
	}
	if _, err := wtx.Exec(ctx, `INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id)
		VALUES ($1, $2, $3, $4)`, q, b.course, b.yukiM, action); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := b.Call(b.tutor, "conversation.draft", draftArgs(b, withdrawing, "a1", 1, m{"text": "Never"}), "")
		refused <- err
	}()
	b.blocked(t, 1, nil)
	if err := wtx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-refused:
		if e, ok := apperr.As(err); !ok || e.Details["reason"] != "conversation_not_awaiting" {
			t.Fatalf("a draft written while its question was withdrawn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the draft never came back")
	}
	if n := b.drafts(t, withdrawing); n != 0 {
		t.Fatal("a draft outlived the question it waited for")
	}

	// A draft being written holds the conversation as LockConversationForDraft
	// does, and has written its row: the answer, proposed or posted, and the
	// close wait for it, and delete what it wrote.
	during := func(t *testing.T, conv uuid.UUID, then func(done chan pipeline.Outcome)) {
		t.Helper()
		tx, err := b.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM conversation WHERE id = $1 FOR SHARE`, conv); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_draft (conversation_id, course_id, attempt, version, body)
			VALUES ($1, $2, 'a1', 1, 'Start') ON CONFLICT (conversation_id) DO UPDATE SET version = conversation_draft.version + 1`, conv, b.course); err != nil {
			t.Fatal(err)
		}
		done := make(chan pipeline.Outcome, 1)
		then(done)
		b.blocked(t, 1, done)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case out := <-done:
			if out.Status != domain.StatusExecuted && out.Status != domain.StatusProposed {
				t.Fatalf("what waited for the draft: %+v", out)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("what waited for the draft never came back")
		}
		if n := b.drafts(t, conv); n != 0 {
			t.Fatal("the draft it waited for outlived it")
		}
	}
	t.Run("posted", func(t *testing.T) {
		conv, q := b.open(t, b.yuki, b.tutorM, "And another thing")
		during(t, conv, func(done chan pipeline.Outcome) {
			b.start(t, done, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Chapter one."))
		})
	})
	t.Run("proposed", func(t *testing.T) {
		b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
		defer b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "autonomous"}})
		conv, q := b.open(t, b.yuki, b.tutorM, "And one more")
		during(t, conv, func(done chan pipeline.Outcome) {
			b.start(t, done, b.tutor, "conversation.answer", answerArgs(b, conv, q, "Chapter two."))
		})
	})
	t.Run("withdrawn", func(t *testing.T) {
		conv, q := b.open(t, b.yuki, b.tutorM, "Oops, not this")
		during(t, conv, func(done chan pipeline.Outcome) {
			b.start(t, done, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": q})
		})
		if got := b.conversation(t, b.yuki, conv); got.State != tools.StateAnswered || got.Draft != nil {
			t.Fatalf("the conversation, its question withdrawn: %+v", got)
		}
	})
	t.Run("closed", func(t *testing.T) {
		during(t, conv, func(done chan pipeline.Outcome) {
			b.start(t, done, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
		})
	})
}
