package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// hash is a payload hash, as the action log holds one.
var hash = strings.Repeat("0", 64)

// heard is what a connection listening on the channel hears within wait.
func heard(t *testing.T, conn *pgx.Conn, wait time.Duration) []wake.Note {
	t.Helper()
	var out []wake.Note
	for {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		n, err := conn.WaitForNotification(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(n.Payload) >= 8000 {
			t.Errorf("a notification of %d bytes; PostgreSQL takes fewer than 8000", len(n.Payload))
		}
		var note wake.Note
		if err := json.Unmarshal([]byte(n.Payload), &note); err != nil {
			t.Fatalf("%q: %v", n.Payload, err)
		}
		out = append(out, note)
		wait = 200 * time.Millisecond // the rest of one commit's come together
	}
}

// Flush notifies what it writes, once per course, type, and conversation or
// proposal, and PostgreSQL says so when the transaction commits and never
// when it rolls back. News of a conversation, and of a proposal to answer in
// one, names the conversation and its two participants.
func TestFlushNotifiesOnCommitAlone(t *testing.T) {
	c := testkit.NewCS101(t, 1)
	ctx := context.Background()
	yuki := c.Students[0].Member
	conversation := ids.New()
	c.Exec(`INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id) VALUES ($1, $2, $3, $4)`,
		conversation, c.Course, yuki, c.GraderM)
	proposal := ids.New()
	if _, err := c.Q.InsertAction(ctx, dbq.InsertActionParams{ID: proposal, ActorID: c.Grader, CourseID: &c.Course, MemberID: &c.GraderM,
		ActionType: "conversation.answer", TargetType: "conversation", TargetID: &conversation, Payload: []byte("{}"),
		PayloadHash: hash, IdempotencyKey: "k", AuthzResult: dbq.AutonomyLevelConfirmRequired, Status: "proposed", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.ConnectConfig(ctx, c.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "LISTEN "+wake.Channel); err != nil {
		t.Fatal(err)
	}

	flush := func(commit bool) {
		t.Helper()
		buf := &events.Buffer{}
		action := ids.New()
		buf.Emit(events.Event{Type: events.ConversationMessagePosted, CourseID: &c.Course, ActionID: &action,
			SubjectType: "conversation", SubjectID: &conversation})
		buf.Emit(events.Event{Type: events.ActionRejected, CourseID: &c.Course, ActionID: &proposal,
			SubjectType: "action", SubjectID: &proposal})
		for _, s := range c.Students { // one grade posted to each: one wake-up in all
			buf.Emit(events.Event{Type: events.GradePosted, CourseID: &c.Course, ActionID: &action,
				SubjectType: "grade", StudentMemberID: &s.Member})
		}
		buf.Emit(events.Event{Type: events.GradePosted, CourseID: &c.Course, ActionID: &action, SubjectType: "grade"})
		buf.Emit(events.Event{Type: "actor.updated", ActionID: &action, SubjectType: "actor"}) // no course's news
		tx, err := c.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// So that the actions the events name exist: the rows are written
		// for this test alone.
		if _, err := dbq.New(tx).InsertAction(ctx, dbq.InsertActionParams{ID: action, ActorID: c.Sato, CourseID: &c.Course,
			MemberID: &c.SatoM, ActionType: "conversation.ask", TargetType: "conversation", TargetID: &conversation,
			Payload: []byte("{}"), PayloadHash: hash, IdempotencyKey: action.String(), AuthzResult: dbq.AutonomyLevelConfirmRequired,
			Status: "proposed", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := events.Flush(ctx, dbq.New(tx), buf); err != nil {
			t.Fatal(err)
		}
		if got := heard(t, conn, 300*time.Millisecond); len(got) != 0 {
			t.Fatalf("heard %+v before the transaction ended", got)
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	flush(false)
	if got := heard(t, conn, 500*time.Millisecond); len(got) != 0 {
		t.Fatalf("heard %+v of a transaction rolled back", got)
	}

	flush(true)
	got := heard(t, conn, 5*time.Second)
	var newest int64
	if err := c.Pool.QueryRow(ctx, `SELECT max(seq) FROM event WHERE course_id = $1`, c.Course).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	want := []wake.Note{
		{CourseID: c.Course, Kind: events.ConversationMessagePosted, Seq: newest - 3, ConversationID: conversation,
			OpenerMemberID: yuki, RespondentMemberID: c.GraderM},
		{CourseID: c.Course, Kind: events.ActionRejected, Seq: newest - 2, ConversationID: conversation,
			OpenerMemberID: yuki, RespondentMemberID: c.GraderM},
		{CourseID: c.Course, Kind: events.GradePosted, Seq: newest},
	}
	if len(got) != len(want) {
		t.Fatalf("heard %d notifications, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("notification %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
