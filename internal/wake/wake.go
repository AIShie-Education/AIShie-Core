// Package wake tells a call that waits for news (wait_s) that there may be
// some, as soon as it is committed.
//
// Whatever writes an event in a course also notifies the PostgreSQL channel
// Channel, in the same transaction (events.Flush), and PostgreSQL delivers the
// notification when, and only if, that transaction commits, to every Core
// instance listening. Each instance keeps one connection listening (Listener)
// and hands what it hears to its Hub, which wakes the calls waiting in this
// process that the news concerns. A woken call reads again, through the
// whole pipeline, as it read the first time.
//
// A notification only says where to look; it is never what a caller is told.
// One that is lost, while the listening connection is down say, costs the
// calls it would have woken their wait and nothing else: every wait has an
// end, after which the call reads again whatever it heard, and the listener
// wakes every waiting call each time it has connected again.
package wake

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Channel is the channel Core notifies and listens on.
const Channel = "aishiteru_wake"

// Note is one notification: news of kind, the type of the event that told
// it, at seq in a course's feed. News of a conversation, and of a proposal to
// write in one, names the conversation and its two participants; other news
// leaves them zero. What it holds is ids, never content, and it is well
// under the 8000 bytes a notification may carry.
//
// News of an answer's draft (KindDraft) is told by no event: it is in no
// feed, and its seq is 0.
type Note struct {
	CourseID           uuid.UUID `json:"course_id"`
	Kind               string    `json:"kind"`
	Seq                int64     `json:"seq"`
	ConversationID     uuid.UUID `json:"conversation_id"`
	OpenerMemberID     uuid.UUID `json:"opener_member_id"`
	RespondentMemberID uuid.UUID `json:"respondent_member_id"`
}

// KindDraft is the kind of the news that an answer's draft was written, or
// ended (conversation.draft). It comes many times a second while an answer
// is written, and wakes only a call that asks for it (Filter.Drafts).
const KindDraft = "conversation.draft"

// Filter is the news a waiting call is woken by: of its course, and, where
// they are set, of one conversation, of conversations addressed to one seat,
// and of some kinds alone. A zero field, or no kinds, asks nothing of it.
// News of a draft wakes only a filter that says Drafts: a reader of the
// conversation that shows its draft as it is written.
type Filter struct {
	CourseID           uuid.UUID
	ConversationID     uuid.UUID
	RespondentMemberID uuid.UUID
	Kinds              []string
	Drafts             bool
}

// Matches says whether n is news f waits for.
func (f Filter) Matches(n Note) bool {
	switch {
	case n.CourseID != f.CourseID:
		return false
	case n.Kind == KindDraft && !f.Drafts:
		return false
	case f.ConversationID != uuid.Nil && n.ConversationID != f.ConversationID:
		return false
	case f.RespondentMemberID != uuid.Nil && n.RespondentMemberID != f.RespondentMemberID:
		return false
	case len(f.Kinds) > 0 && !slices.Contains(f.Kinds, n.Kind):
		return false
	}
	return true
}

// Config bounds how many calls wait at once in one process, and how many of
// them one actor's. Past either, a call answers at once with what it read,
// as it would have without waiting. Zero lets none wait.
type Config struct {
	MaxWaiters  int
	MaxPerActor int
}

// The defaults: an agent long-polling the inboxes of sixteen courses, and a
// thousand waiting calls in all, each a connection held and a goroutine.
const (
	DefaultMaxWaiters  = 1000
	DefaultMaxPerActor = 16
)

// DefaultConfig is Config at its defaults.
var DefaultConfig = Config{MaxWaiters: DefaultMaxWaiters, MaxPerActor: DefaultMaxPerActor}

// Hub hands notifications to the calls waiting in this process. Its methods
// are safe to call from any goroutine.
type Hub struct {
	cfg Config

	mu       sync.Mutex
	byCourse map[uuid.UUID]map[*Waiter]struct{}
	perActor map[uuid.UUID]int
	waiting  int
	closed   bool
	done     chan struct{}
}

func NewHub(cfg Config) *Hub {
	return &Hub{cfg: cfg, byCourse: map[uuid.UUID]map[*Waiter]struct{}{}, perActor: map[uuid.UUID]int{}, done: make(chan struct{})}
}

// Waiter is one call's place in the hub, from Subscribe until Close.
type Waiter struct {
	hub   *Hub
	actor uuid.UUID
	f     Filter
	// woken holds one wake-up: any that come while one is held are the same
	// news to a call that is about to read again anyway.
	woken chan struct{}
	once  sync.Once
}

// Subscribe gives a call made by actor a place to wait for news f matches, or
// false if the hub is full, the actor has as many as it may, or the hub is
// shutting down: the call then answers without waiting. A call subscribes
// before it first reads, so that no news committed after that read passes it
// by, and it closes the waiter when it is done.
func (h *Hub) Subscribe(actor uuid.UUID, f Filter) (*Waiter, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.waiting >= h.cfg.MaxWaiters || h.perActor[actor] >= h.cfg.MaxPerActor {
		return nil, false
	}
	w := &Waiter{hub: h, actor: actor, f: f, woken: make(chan struct{}, 1)}
	in := h.byCourse[f.CourseID]
	if in == nil {
		in = map[*Waiter]struct{}{}
		h.byCourse[f.CourseID] = in
	}
	in[w] = struct{}{}
	h.perActor[actor]++
	h.waiting++
	return w, true
}

// Close gives the waiter's place back. It may be called more than once.
func (w *Waiter) Close() {
	w.once.Do(func() {
		h := w.hub
		h.mu.Lock()
		defer h.mu.Unlock()
		in := h.byCourse[w.f.CourseID]
		delete(in, w)
		if len(in) == 0 {
			delete(h.byCourse, w.f.CourseID)
		}
		if h.perActor[w.actor]--; h.perActor[w.actor] <= 0 {
			delete(h.perActor, w.actor)
		}
		h.waiting--
	})
}

func (w *Waiter) wake() {
	select {
	case w.woken <- struct{}{}:
	default:
	}
}

// Publish wakes every waiter n is news for.
func (h *Hub) Publish(n Note) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.byCourse[n.CourseID] {
		if w.f.Matches(n) {
			w.wake()
		}
	}
}

// WakeAll wakes every waiter, to read again: news may have been missed.
func (h *Hub) WakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, in := range h.byCourse {
		for w := range in {
			w.wake()
		}
	}
}

// Shutdown ends every wait, now and to come: the server is stopping, and
// the calls waiting answer with what they read, rather than keep it
// waiting for them.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}

// Waiting is how many calls wait now.
func (h *Hub) Waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waiting
}

// Why a wait ended.
type Why int

const (
	// Woken: there may be news; read again.
	Woken Why = iota
	// TimedOut: the time the call asked to wait is up.
	TimedOut
	// Cancelled: the call's context is done, its client gone.
	Cancelled
	// ShutDown: the server is stopping.
	ShutDown
)

func (y Why) String() string {
	return [...]string{"woken", "timed_out", "cancelled", "shut_down"}[y]
}

// Wait waits for news, until then at the latest, and says why it stopped.
// A wake-up that came since the last Wait, while the call was reading, ends
// it at once.
func (w *Waiter) Wait(ctx context.Context, until time.Time) Why {
	t := time.NewTimer(time.Until(until))
	defer t.Stop()
	select {
	case <-w.woken:
		return Woken
	case <-ctx.Done():
		return Cancelled
	case <-w.hub.done:
		return ShutDown
	case <-t.C:
		return TimedOut
	}
}

// holdKey carries what lets a waiting call keep its connection open.
type holdKey struct{}

// WithHold gives the calls made with ctx a way to ask their transport to keep
// the connection open for a wait, until a time and whatever margin the
// transport adds to answer after it. An HTTP server bounds each request, and
// a call that waits for news needs longer than one that does not.
func WithHold(ctx context.Context, hold func(until time.Time)) context.Context {
	return context.WithValue(ctx, holdKey{}, hold)
}

// Hold asks the transport that ctx came through to keep the connection open
// until then, if it said how (WithHold).
func Hold(ctx context.Context, until time.Time) {
	if hold, ok := ctx.Value(holdKey{}).(func(time.Time)); ok {
		hold(until)
	}
}
