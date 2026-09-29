package wake_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/wake"
)

// soon is long enough for a wake-up that is coming to come, and short enough
// for a test to wait for one that is not.
const soon = 2 * time.Second

func subscribe(t *testing.T, h *wake.Hub, actor uuid.UUID, f wake.Filter) *wake.Waiter {
	t.Helper()
	w, ok := h.Subscribe(actor, f)
	if !ok {
		t.Fatal("no room to wait")
	}
	t.Cleanup(w.Close)
	return w
}

// waitFor runs w.Wait in the background, to see what ends it.
func waitFor(ctx context.Context, w *wake.Waiter, until time.Time) <-chan wake.Why {
	c := make(chan wake.Why, 1)
	go func() { c <- w.Wait(ctx, until) }()
	return c
}

func TestAWaiterIsWokenByItsNewsAlone(t *testing.T) {
	course, other := uuid.New(), uuid.New()
	conversation, seat := uuid.New(), uuid.New()
	inbox := wake.Filter{CourseID: course, RespondentMemberID: seat, Kinds: []string{"conversation.message_posted"}}
	reader := wake.Filter{CourseID: course, ConversationID: conversation}
	feed := wake.Filter{CourseID: course}
	posted := wake.Note{CourseID: course, Kind: "conversation.message_posted", Seq: 7, ConversationID: conversation,
		OpenerMemberID: uuid.New(), RespondentMemberID: seat}

	cases := []struct {
		name string
		f    wake.Filter
		n    wake.Note
		want bool
	}{
		{"a question to the seat, in its inbox", inbox, posted, true},
		{"a question to another seat", inbox, func() wake.Note { n := posted; n.RespondentMemberID = uuid.New(); return n }(), false},
		{"a question to the seat in another course", inbox, func() wake.Note { n := posted; n.CourseID = other; return n }(), false},
		{"news the inbox does not wait for", inbox, func() wake.Note { n := posted; n.Kind = "conversation.closed"; return n }(), false},
		{"a message in the conversation read", reader, posted, true},
		{"the conversation read, closed", reader, func() wake.Note { n := posted; n.Kind = "conversation.closed"; return n }(), true},
		{"another conversation", reader, func() wake.Note { n := posted; n.ConversationID = uuid.New(); return n }(), false},
		{"news that names no conversation", reader, wake.Note{CourseID: course, Kind: "grade.posted", Seq: 8}, false},
		{"anything in the course, for its feed", feed, wake.Note{CourseID: course, Kind: "grade.posted", Seq: 8}, true},
		{"another course's feed", feed, wake.Note{CourseID: other, Kind: "grade.posted", Seq: 8}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.Matches(tc.n); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
			h := wake.NewHub(wake.DefaultConfig)
			w := subscribe(t, h, uuid.New(), tc.f)
			ended := waitFor(context.Background(), w, time.Now().Add(soon))
			h.Publish(tc.n)
			if tc.want {
				if why := <-ended; why != wake.Woken {
					t.Fatalf("the wait ended %v, want woken", why)
				}
				return
			}
			select {
			case why := <-ended:
				t.Fatalf("the wait ended %v; news for someone else woke it", why)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// A wake-up that comes while the call is reading, between two waits, is not
// lost: the next wait ends at once. Two are one.
func TestAWakeUpWaitsForTheNextWait(t *testing.T) {
	h := wake.NewHub(wake.DefaultConfig)
	course := uuid.New()
	w := subscribe(t, h, uuid.New(), wake.Filter{CourseID: course})
	h.Publish(wake.Note{CourseID: course, Kind: "grade.posted", Seq: 1})
	h.Publish(wake.Note{CourseID: course, Kind: "grade.posted", Seq: 2})
	if why := w.Wait(context.Background(), time.Now().Add(soon)); why != wake.Woken {
		t.Fatalf("the first wait ended %v, want woken", why)
	}
	start := time.Now()
	if why := w.Wait(context.Background(), time.Now().Add(150*time.Millisecond)); why != wake.TimedOut {
		t.Fatalf("the second wait ended %v, want timed out: two wake-ups before a wait are one", why)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("the second wait lasted %v", waited)
	}
}

func TestAWaitEnds(t *testing.T) {
	course := uuid.New()
	f := wake.Filter{CourseID: course}

	t.Run("when its time is up", func(t *testing.T) {
		h := wake.NewHub(wake.DefaultConfig)
		w := subscribe(t, h, uuid.New(), f)
		start := time.Now()
		if why := w.Wait(context.Background(), start.Add(200*time.Millisecond)); why != wake.TimedOut {
			t.Fatalf("ended %v, want timed out", why)
		}
		if waited := time.Since(start); waited < 150*time.Millisecond || waited > soon {
			t.Fatalf("waited %v for 200ms", waited)
		}
	})
	t.Run("when its client goes", func(t *testing.T) {
		h := wake.NewHub(wake.DefaultConfig)
		w := subscribe(t, h, uuid.New(), f)
		ctx, cancel := context.WithCancel(context.Background())
		ended := waitFor(ctx, w, time.Now().Add(time.Minute))
		cancel()
		select {
		case why := <-ended:
			if why != wake.Cancelled {
				t.Fatalf("ended %v, want cancelled", why)
			}
		case <-time.After(soon):
			t.Fatal("still waiting after its context was cancelled")
		}
	})
	t.Run("when the server shuts down, and none waits after", func(t *testing.T) {
		h := wake.NewHub(wake.DefaultConfig)
		w := subscribe(t, h, uuid.New(), f)
		ended := waitFor(context.Background(), w, time.Now().Add(time.Minute))
		h.Shutdown()
		h.Shutdown() // twice is once
		select {
		case why := <-ended:
			if why != wake.ShutDown {
				t.Fatalf("ended %v, want shut down", why)
			}
		case <-time.After(soon):
			t.Fatal("still waiting after the hub shut down")
		}
		if _, ok := h.Subscribe(uuid.New(), f); ok {
			t.Fatal("a call was given a place to wait on a hub that has shut down")
		}
	})
	t.Run("when the listener has connected again, whatever it waits for", func(t *testing.T) {
		h := wake.NewHub(wake.DefaultConfig)
		a := subscribe(t, h, uuid.New(), wake.Filter{CourseID: course, ConversationID: uuid.New()})
		b := subscribe(t, h, uuid.New(), wake.Filter{CourseID: uuid.New()})
		h.WakeAll()
		for _, w := range []*wake.Waiter{a, b} {
			if why := w.Wait(context.Background(), time.Now().Add(soon)); why != wake.Woken {
				t.Fatalf("ended %v, want woken", why)
			}
		}
	})
}

// Past its bounds the hub gives no place to wait, and the call answers at
// once; a place given back is there for the next.
func TestTheHubIsBounded(t *testing.T) {
	course := uuid.New()
	f := wake.Filter{CourseID: course}

	t.Run("per actor", func(t *testing.T) {
		h := wake.NewHub(wake.Config{MaxWaiters: 100, MaxPerActor: 16})
		agent := uuid.New()
		var ws []*wake.Waiter
		for range 16 {
			ws = append(ws, subscribe(t, h, agent, f))
		}
		if _, ok := h.Subscribe(agent, f); ok {
			t.Fatal("a seventeenth wait for one actor")
		}
		subscribe(t, h, uuid.New(), f) // another actor's is not counted with it
		ws[0].Close()
		ws[0].Close() // twice is once
		subscribe(t, h, agent, f)
		if got := h.Waiting(); got != 17 {
			t.Fatalf("waiting = %d, want 17", got)
		}
	})
	t.Run("per process", func(t *testing.T) {
		h := wake.NewHub(wake.Config{MaxWaiters: 3, MaxPerActor: 16})
		var ws []*wake.Waiter
		for range 3 {
			ws = append(ws, subscribe(t, h, uuid.New(), f))
		}
		if _, ok := h.Subscribe(uuid.New(), f); ok {
			t.Fatal("a fourth wait in a hub of three")
		}
		ws[1].Close()
		subscribe(t, h, uuid.New(), f)
	})
	t.Run("off", func(t *testing.T) {
		h := wake.NewHub(wake.Config{})
		if _, ok := h.Subscribe(uuid.New(), f); ok {
			t.Fatal("a wait in a hub that lets none wait")
		}
	})
	t.Run("under a crowd", func(t *testing.T) {
		h := wake.NewHub(wake.Config{MaxWaiters: 50, MaxPerActor: 50})
		var wg sync.WaitGroup
		var mu sync.Mutex
		got := 0
		for range 200 {
			wg.Go(func() {
				if w, ok := h.Subscribe(uuid.New(), f); ok {
					mu.Lock()
					got++
					mu.Unlock()
					h.Publish(wake.Note{CourseID: course, Kind: "grade.posted"})
					defer w.Close()
				}
			})
		}
		wg.Wait()
		if got < 50 || h.Waiting() != 0 {
			t.Fatalf("%d waited of 200 in a hub of 50, and %d still wait", got, h.Waiting())
		}
	})
}
