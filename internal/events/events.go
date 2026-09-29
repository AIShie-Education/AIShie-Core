// Package events writes the event table: something that happened, recorded
// after it did, in the same transaction as the state change.
//
// A tool does not insert events. It calls Emit on its execution context, the
// pipeline collects them, and Flush writes them as the last thing the
// transaction does, and notifies whoever waits for a course's news (package
// wake), which PostgreSQL tells them once the transaction commits.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/wake"
)

// Event types. The catalogue grows with the tool catalogue.
const (
	ActionProposed  = "action.proposed"
	ActionApproved  = "action.approved"
	ActionRejected  = "action.rejected"
	ActionCancelled = "action.cancelled"
	ActionReviewed  = "action.reviewed"
	ActionEscalated = "action.escalated"

	GradeCreated      = "grade.created"
	GradePosted       = "grade.posted"
	GradeRegraded     = "grade.regraded"
	GradeTotalUpdated = "grade.total_updated"
	// A person put a number in place of a total worked out, took it away, or
	// wrote a comment on a total.
	GradeTotalOverridden      = "grade.total_overridden"
	GradeTotalOverrideCleared = "grade.total_override_cleared"
	GradeTotalCommented       = "grade.total_commented"
	// A student's totals stopped counting ungraded work as zero.
	GradeUngradedAsZeroUndone = "grade.ungraded_as_zero_undone"

	// A conversation's news is its two participants' and nobody else's
	// (docs/schema.md §2.8). Its subject is the conversation, and its
	// payload never holds what was written.
	ConversationOpened           = "conversation.opened"
	ConversationMessagePosted    = "conversation.message_posted"
	ConversationClosed           = "conversation.closed"
	ConversationMessageRetracted = "conversation.message_retracted"
)

// Event is one row of the feed.
//
// Payload carries ids and small facts only, never a score or feedback text:
// the feed is filtered by event type and scope, not by content, so content
// must be fetched through a read tool that authorizes it.
type Event struct {
	Type     string
	CourseID *uuid.UUID
	// ActionID is filled by the pipeline. It is the action whose feed entry
	// this is: for a proposal's approval, the proposal, so that the proposer
	// finds it.
	ActionID    *uuid.UUID
	SubjectType string
	SubjectID   *uuid.UUID
	// StudentMemberID and AssignmentID say whose the event is. Nil means it
	// belongs to no student (or no assignment) and that scope does not apply.
	StudentMemberID *uuid.UUID
	AssignmentID    *uuid.UUID
	Payload         map[string]any
}

// Buffer collects what one transaction emits, in order.
type Buffer struct {
	events []Event
}

func (b *Buffer) Emit(e Event) { b.events = append(b.events, e) }

func (b *Buffer) Len() int { return len(b.events) }

// Truncate drops everything emitted after the first n events, for when the
// work that emitted them is rolled back to a savepoint.
func (b *Buffer) Truncate(n int) {
	if n < len(b.events) {
		b.events = b.events[:n]
	}
}

// Drain hands every buffered event to emit, in order, and empties the buffer.
// It is how events from work done inside a nested savepoint join the outer
// transaction's events once that work is known to have succeeded.
func (b *Buffer) Drain(emit func(Event)) {
	for _, e := range b.events {
		emit(e)
	}
	b.events = nil
}

// lockNamespace is the first key of the two-key advisory lock taken per
// course. Arbitrary, but fixed: "AISE".
const lockNamespace = 0x41495345

// Flush writes the buffered events. Call it last, just before COMMIT.
//
// event.seq comes from a sequence, and sequences hand out numbers in the
// order they are asked, not the order transactions commit. Two writers could
// therefore commit seq 11 before seq 10, and a reader whose cursor had moved
// to 11 would never see 10. The feed is always read per course, so Flush
// takes a transaction-scoped advisory lock per course first: within a course,
// whoever gets a lower seq also commits first. Locks are taken in a fixed
// order so that two flushes touching the same two courses cannot deadlock.
func Flush(ctx context.Context, q *dbq.Queries, b *Buffer) error {
	if len(b.events) == 0 {
		return nil
	}
	// An event that names a student takes KEY SHARE on the student's seat
	// through its foreign key. Taken there, under the stream lock, it would
	// wait for anyone changing that seat — who, holding the seat FOR UPDATE,
	// then waits for the stream lock to write its own events. So the seats
	// come first, and the stream lock is never held while one is waited for.
	if seats := studentSeats(b.events); len(seats) > 0 {
		if err := q.ShareSeats(ctx, seats); err != nil {
			return fmt.Errorf("event seats: %w", err)
		}
	}
	// So with an event's assignment, which assignment.unpublish holds FOR
	// UPDATE until it has taken the stream lock to write its own event.
	if assignments := assignmentsOf(b.events); len(assignments) > 0 {
		if err := q.ShareAssignments(ctx, assignments); err != nil {
			return fmt.Errorf("event assignments: %w", err)
		}
	}
	for _, key := range lockKeys(b.events) {
		if err := q.LockEventStream(ctx, dbq.LockEventStreamParams{Namespace: lockNamespace, Stream: key}); err != nil {
			return fmt.Errorf("event stream lock: %w", err)
		}
	}
	var news wakeUps
	for _, e := range b.events {
		payload := []byte("{}")
		if len(e.Payload) > 0 {
			var err error
			if payload, err = json.Marshal(e.Payload); err != nil {
				return fmt.Errorf("event %s payload: %w", e.Type, err)
			}
		}
		seq, err := q.InsertEvent(ctx, dbq.InsertEventParams{
			Type:            e.Type,
			CourseID:        e.CourseID,
			ActionID:        e.ActionID,
			SubjectType:     e.SubjectType,
			SubjectID:       e.SubjectID,
			StudentMemberID: e.StudentMemberID,
			AssignmentID:    e.AssignmentID,
			Payload:         payload,
		})
		if err != nil {
			return fmt.Errorf("insert event %s: %w", e.Type, err)
		}
		news.add(e, seq)
	}
	if err := news.notify(ctx, q); err != nil {
		return fmt.Errorf("wake-ups: %w", err)
	}
	b.events = nil
	return nil
}

// wakeUps is the news one flush tells whoever waits for it (package wake):
// one notification for each course, type, and conversation or proposal, at
// the newest seq among its events. A grade posted to forty students is one
// wake-up, not forty.
type wakeUps struct {
	keys []wakeKey
	seqs map[wakeKey]int64
}

type wakeKey struct {
	course, conversation, action uuid.UUID
	kind                         string
}

// add counts e in, if it is news of a course. Its conversation is its subject
// when that is one; a proposal's news names its action, whose target, when
// that is a conversation, NotifyWake looks up: deciding an answer changes
// what the conversation waits for.
func (w *wakeUps) add(e Event, seq int64) {
	if e.CourseID == nil {
		return
	}
	k := wakeKey{course: *e.CourseID, kind: e.Type}
	if e.SubjectType == "conversation" && e.SubjectID != nil {
		k.conversation = *e.SubjectID
	} else if strings.HasPrefix(e.Type, "action.") && e.ActionID != nil {
		k.action = *e.ActionID
	}
	if w.seqs == nil {
		w.seqs = map[wakeKey]int64{}
	}
	if _, seen := w.seqs[k]; !seen {
		w.keys = append(w.keys, k)
	}
	w.seqs[k] = max(w.seqs[k], seq)
}

func (w *wakeUps) notify(ctx context.Context, q *dbq.Queries) error {
	if len(w.keys) == 0 {
		return nil
	}
	arg := dbq.NotifyWakeParams{Channel: wake.Channel}
	for _, k := range w.keys {
		arg.CourseIds = append(arg.CourseIds, k.course)
		arg.Kinds = append(arg.Kinds, k.kind)
		arg.Seqs = append(arg.Seqs, w.seqs[k])
		arg.ConversationIds = append(arg.ConversationIds, k.conversation)
		arg.ActionIds = append(arg.ActionIds, k.action)
	}
	return q.NotifyWake(ctx, arg)
}

// studentSeats returns the distinct students the events name.
func studentSeats(evs []Event) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, e := range evs {
		if e.StudentMemberID != nil && !seen[*e.StudentMemberID] {
			seen[*e.StudentMemberID] = true
			ids = append(ids, *e.StudentMemberID)
		}
	}
	return ids
}

// assignmentsOf returns the distinct assignments the events name.
func assignmentsOf(evs []Event) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, e := range evs {
		if e.AssignmentID != nil && !seen[*e.AssignmentID] {
			seen[*e.AssignmentID] = true
			ids = append(ids, *e.AssignmentID)
		}
	}
	return ids
}

// lockKeys returns one key per distinct course among the events, sorted.
// Events outside any course share stream 0.
func lockKeys(evs []Event) []int32 {
	seen := map[int32]struct{}{}
	var keys []int32
	for _, e := range evs {
		var k int32
		if e.CourseID != nil {
			h := fnv.New32a()
			_, _ = h.Write(e.CourseID[:]) // hash.Hash never returns an error
			k = int32(h.Sum32())          //nolint:gosec // a lock key; wrapping is fine
		}
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
