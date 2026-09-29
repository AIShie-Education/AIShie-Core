package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/wake"
)

// Long polling: a read that finds nothing new, asked to wait (wait_s),
// answers as soon as something it reads is committed, from any connection,
// and otherwise when its time is up; it waits holding nothing of the
// database's, and each time it reads it is authorized as any read is.

// waking is build on a platform whose reads may wait, as serve's may.
func waking(t *testing.T, cfg wake.Config) (*built, *wake.Hub) {
	t.Helper()
	p, hub := testkit.NewPlatformWaking(t, cfg)
	return buildOn(t, p), hub
}

// read is one read, as a client makes it, with its own context.
type read struct {
	out pipeline.Outcome
	err error
	at  time.Time // when it answered
}

// reading makes the read in the background.
func (b *built) reading(ctx context.Context, actor uuid.UUID, name string, args m) <-chan read {
	c := make(chan read, 1)
	raw, _ := json.Marshal(args)
	go func() {
		out, err := b.P.Invoke(ctx, pipeline.Caller{ActorID: actor}, name, raw, "")
		c <- read{out, err, time.Now()}
	}()
	return c
}

func answered(t *testing.T, c <-chan read, within time.Duration) read {
	t.Helper()
	select {
	case r := <-c:
		if r.err != nil {
			t.Fatalf("the read failed: %v", r.err)
		}
		return r
	case <-time.After(within):
		t.Fatalf("no answer within %v", within)
	}
	return read{}
}

func notYet(t *testing.T, c <-chan read, for_ time.Duration) {
	t.Helper()
	select {
	case r := <-c:
		t.Fatalf("answered already: %s %v", r.out.Result, r.err)
	case <-time.After(for_):
	}
}

// waitingNow waits until n calls wait in hub.
func waitingNow(t *testing.T, hub *wake.Hub, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); hub.Waiting() != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls wait, not %d", hub.Waiting(), n)
		}
	}
}

func resultOf[T any](t *testing.T, r read) T {
	t.Helper()
	return testkit.Result[T](t, r.out)
}

// An agent's inbox that waits hears a question as soon as it is asked, and
// meanwhile holds no connection to the database.
func TestAnInboxThatWaitsHearsAQuestionAtOnce(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	inbox := b.reading(context.Background(), b.tutor, "conversation.inbox", m{"course_id": b.course, "wait_s": 10})
	waitingNow(t, hub, 1)
	notYet(t, inbox, 300*time.Millisecond)
	if n := b.Pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections held while waiting", n)
	}

	conversation, question := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	asked := time.Now()
	r := answered(t, inbox, 5*time.Second)
	latency := r.at.Sub(asked)
	t.Logf("the inbox answered %v after the question was committed", latency)
	if latency > 500*time.Millisecond { // some 0.1 s is the aim; a busy test machine gets some slack
		t.Fatalf("the inbox answered %v after the question", latency)
	}
	got := resultOf[tools.ConversationInboxOut](t, r).Conversations
	if len(got) != 1 || got[0].ID != conversation || *got[0].LatestOpenerMessageID != question {
		t.Fatalf("inbox: %+v", got)
	}
	waitingNow(t, hub, 0)
}

// A reader of a conversation that waits hears the answer, and then, with no
// message written, its closing; a change it has not seen answers at once.
func TestMessagesThatWaitHearAMessageAndAClose(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	conversation, question := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	args := func(after int, extra m) m {
		a := m{"course_id": b.course, "conversation_id": conversation, "after_seq": after, "wait_s": 10}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}

	messages := b.reading(context.Background(), b.yuki, "conversation.messages", args(1, nil))
	waitingNow(t, hub, 1)
	notYet(t, messages, 200*time.Millisecond)
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conversation, question, "Start with chapter one."))
	got := resultOf[tools.ConversationMessagesOut](t, answered(t, messages, 5*time.Second))
	if len(got.Messages) != 1 || got.Messages[0].Seq != 2 || got.Conversation.State != tools.StateAnswered {
		t.Fatalf("after the answer: %+v", got)
	}

	messages = b.reading(context.Background(), b.yuki, "conversation.messages", args(2, m{"seen_state": tools.StateAnswered}))
	waitingNow(t, hub, 1)
	notYet(t, messages, 200*time.Millisecond)
	b.do(t, b.tutor, "conversation.close", m{"course_id": b.course, "conversation_id": conversation, "reason": "done"})
	got = resultOf[tools.ConversationMessagesOut](t, answered(t, messages, 5*time.Second))
	if len(got.Messages) != 0 || got.Conversation.State != tools.StateClosed {
		t.Fatalf("after the close: %+v", got)
	}

	// Closed while the reader was not looking: it says what it last saw,
	// and is told at once.
	start := time.Now()
	got = resultOf[tools.ConversationMessagesOut](t, answered(t,
		b.reading(context.Background(), b.yuki, "conversation.messages", args(2, m{"seen_state": tools.StateAnswered})), 5*time.Second))
	if waited := time.Since(start); waited > time.Second || got.Conversation.State != tools.StateClosed {
		t.Fatalf("a close not yet seen: %v, %+v", waited, got)
	}
}

// Waiting changes nothing of who may read: whoever may not read a
// conversation, or an inbox, is refused at once, as without waiting; and a
// call that waits is authorized again each time it reads, so that what a
// seat lost meanwhile it does not read.
func TestWaitingIsAuthorizedAsReadingIs(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	conversation, _ := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	start := time.Now()
	b.try(t, b.ken, "conversation.messages", m{"course_id": b.course, "conversation_id": conversation, "after_seq": 1, "wait_s": 10},
		apperr.NotFound)
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("refused after %v", waited)
	}
	r := answered(t, b.reading(context.Background(), b.yuki, "conversation.inbox", m{"course_id": b.course, "wait_s": 10}), 5*time.Second)
	if r.out.Status != domain.StatusDenied || r.out.Error == nil || r.out.Error.Details["reason"] != string(domain.CeilingConversationsAreWithAgents) {
		t.Fatalf("a person's inbox: %+v", r.out)
	}

	// Yuki waits on her conversation; her seat is removed meanwhile, which
	// closes it: what she reads next, she is refused.
	messages := b.reading(context.Background(), b.yuki, "conversation.messages",
		m{"course_id": b.course, "conversation_id": conversation, "after_seq": 1, "wait_s": 10})
	waitingNow(t, hub, 1)
	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM})
	r = answered(t, messages, 5*time.Second)
	if r.out.Status != domain.StatusDenied {
		t.Fatalf("read on after her seat was removed: %+v", r.out)
	}
}

// wait_s at 0, the default, is the call as it was; so is any wait_s on a
// server that lets no call wait. More than MaxWaitSeconds is refused.
func TestNotWaitingIsAsBefore(t *testing.T) {
	check := func(t *testing.T, b *built) {
		t.Helper()
		conversation, _ := b.open(t, b.yuki, b.tutorM, "Where do I start?")
		for _, tc := range []struct {
			actor uuid.UUID
			tool  string
			args  m
		}{
			{b.tutor, "conversation.inbox", m{"course_id": b.course}},
			{b.yuki, "conversation.messages", m{"course_id": b.course, "conversation_id": conversation, "after_seq": 1}},
			{b.yuki, "conversation.messages", m{"course_id": b.course, "conversation_id": conversation}},
			{b.yuki, "event.list", m{"course_id": b.course, "since_seq": 1 << 40}},
			{b.tutor, "event.list", m{"course_id": b.course}},
		} {
			plain := b.do(t, tc.actor, tc.tool, tc.args)
			tc.args["wait_s"] = 0
			start := time.Now()
			zero := b.do(t, tc.actor, tc.tool, tc.args)
			if string(plain.Result) != string(zero.Result) || time.Since(start) > time.Second {
				t.Errorf("%s with wait_s 0: %s, without: %s", tc.tool, zero.Result, plain.Result)
			}
		}
		for _, bad := range []int{-1, tool.MaxWaitSeconds + 1} {
			if _, err := b.Call(b.tutor, "conversation.inbox", m{"course_id": b.course, "wait_s": bad}, ""); !apperr.Is(err, apperr.InvalidArgument) {
				t.Errorf("wait_s %d: %v", bad, err)
			}
		}
		if _, err := b.Call(b.yuki, "conversation.messages",
			m{"course_id": b.course, "conversation_id": conversation, "before_seq": 2, "wait_s": 5}, ""); !apperr.Is(err, apperr.InvalidArgument) {
			t.Errorf("wait_s with before_seq: %v", err)
		}
	}
	t.Run("wait_s 0", func(t *testing.T) {
		b, _ := waking(t, wake.DefaultConfig)
		check(t, b)
	})
	t.Run("a server that lets no call wait", func(t *testing.T) {
		b := build(t)
		start := time.Now()
		b.do(t, b.tutor, "conversation.inbox", m{"course_id": b.course, "wait_s": 5})
		if waited := time.Since(start); waited > time.Second {
			t.Fatalf("waited %v on a server with nowhere to wait", waited)
		}
		check(t, b)
	})
}

// A call past the bounds on waiting answers at once, with what it read; one
// whose client has gone stops waiting; one whose time is up reads again and
// answers; a news that turns out to be nothing new leaves it waiting; and
// the server shutting down ends every wait.
func TestWaitsEnd(t *testing.T) {
	b, hub := waking(t, wake.Config{MaxWaiters: 10, MaxPerActor: 1})
	inbox := m{"course_id": b.course, "wait_s": 20}

	ctx, cancel := context.WithCancel(context.Background())
	first := b.reading(ctx, b.tutor, "conversation.inbox", inbox)
	waitingNow(t, hub, 1)
	start := time.Now()
	second := answered(t, b.reading(context.Background(), b.tutor, "conversation.inbox", inbox), 5*time.Second)
	if waited := time.Since(start); waited > time.Second || len(resultOf[tools.ConversationInboxOut](t, second).Conversations) != 0 {
		t.Fatalf("a wait past the actor's bound: %v %s", waited, second.out.Result)
	}
	cancel()
	if r := answered(t, first, 2*time.Second); r.out.Status != domain.StatusExecuted {
		t.Fatalf("a wait whose client went: %+v", r.out)
	}
	waitingNow(t, hub, 0)

	// News that is none: woken, it reads nothing new, and waits on.
	start = time.Now()
	short := b.reading(context.Background(), b.tutor, "conversation.inbox", m{"course_id": b.course, "wait_s": 2})
	waitingNow(t, hub, 1)
	hub.Publish(wake.Note{CourseID: b.course, Kind: events.ConversationMessagePosted, RespondentMemberID: b.tutorM})
	notYet(t, short, time.Second)
	r := answered(t, short, 3*time.Second)
	if waited := time.Since(start); waited < 2*time.Second || len(resultOf[tools.ConversationInboxOut](t, r).Conversations) != 0 {
		t.Fatalf("a wait of 2s: %v, %s", waited, r.out.Result)
	}

	long := b.reading(context.Background(), b.tutor, "conversation.inbox", inbox)
	waitingNow(t, hub, 1)
	hub.Shutdown()
	answered(t, long, 2*time.Second)
	start = time.Now()
	answered(t, b.reading(context.Background(), b.tutor, "conversation.inbox", inbox), 2*time.Second)
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %v on a server shutting down", waited)
	}
}

// The feed waits too: an agent that waits on it hears of what it may see as
// soon as it happens.
func TestTheFeedWaits(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	now := testkit.Result[tools.EventListOut](t, b.do(t, b.tutor, "event.list", m{"course_id": b.course})).NextSeq
	feed := b.reading(context.Background(), b.tutor, "event.list", m{"course_id": b.course, "since_seq": now, "wait_s": 10})
	waitingNow(t, hub, 1)
	notYet(t, feed, 200*time.Millisecond)
	conversation, _ := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	got := resultOf[tools.EventListOut](t, answered(t, feed, 5*time.Second))
	if len(got.Events) == 0 || *got.Events[0].SubjectID != conversation {
		t.Fatalf("the feed after a question: %+v", got)
	}
}
