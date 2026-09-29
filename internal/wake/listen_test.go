package wake_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/testdb"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// listening starts a listener on pool's database, and waits until it
// listens.
func listening(t *testing.T, pool *pgxpool.Pool, h *wake.Hub) *wake.Listener {
	t.Helper()
	l := wake.NewListener(pool.Config().ConnConfig, h, nil)
	l.MinBackoff, l.MaxBackoff, l.PingEvery = 20*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "the listener to listen", l.Up)
	return l
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func notify(t *testing.T, pool *pgxpool.Pool, payload string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "SELECT pg_notify($1, $2)", wake.Channel, payload); err != nil {
		t.Fatal(err)
	}
}

func note(t *testing.T, n wake.Note) string {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What is notified on the channel reaches the waiters it is news for, and
// what is not Core's wakes nobody; a listener that lost its connection
// connects again, wakes everyone, since it may have missed something, and
// hears what comes after.
func TestTheListenerHandsOnWhatItHearsAndComesBack(t *testing.T) {
	pool := testdb.New(t)
	h := wake.NewHub(wake.DefaultConfig)
	listening(t, pool, h)
	course := uuid.New()
	w := subscribe(t, h, uuid.New(), wake.Filter{CourseID: course})

	notify(t, pool, `not json`)
	notify(t, pool, note(t, wake.Note{CourseID: uuid.New(), Kind: "grade.posted", Seq: 1}))
	if why := w.Wait(context.Background(), time.Now().Add(200*time.Millisecond)); why != wake.TimedOut {
		t.Fatalf("ended %v: news of nothing it waits for woke it", why)
	}
	notify(t, pool, note(t, wake.Note{CourseID: course, Kind: "grade.posted", Seq: 2}))
	if why := w.Wait(context.Background(), time.Now().Add(soon)); why != wake.Woken {
		t.Fatalf("ended %v, want woken by its course's news", why)
	}

	// Its connection is cut from the server's side.
	var cut int
	if err := pool.QueryRow(context.Background(), `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = $1`, wake.ApplicationName).Scan(&cut); err != nil {
		t.Fatal(err)
	}
	if cut != 1 {
		t.Fatalf("%d listening connections cut, want 1", cut)
	}
	// Everyone waiting is woken once it listens again, to read again.
	if why := w.Wait(context.Background(), time.Now().Add(10*time.Second)); why != wake.Woken {
		t.Fatalf("ended %v, want woken when the listener came back", why)
	}
	// It is listening by then, and hears what comes after.
	notify(t, pool, note(t, wake.Note{CourseID: course, Kind: "grade.posted", Seq: 3}))
	if why := w.Wait(context.Background(), time.Now().Add(soon)); why != wake.Woken {
		t.Fatalf("ended %v, want woken by news after the listener came back", why)
	}
}

// Nothing is heard of a transaction rolled back, and what one commits is
// heard when it commits, not before.
func TestANotificationIsSentOnCommitAlone(t *testing.T) {
	pool := testdb.New(t)
	h := wake.NewHub(wake.DefaultConfig)
	listening(t, pool, h)
	course := uuid.New()
	w := subscribe(t, h, uuid.New(), wake.Filter{CourseID: course})
	ctx := context.Background()
	payload := note(t, wake.Note{CourseID: course, Kind: "grade.posted", Seq: 1})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", wake.Channel, payload); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if why := w.Wait(ctx, time.Now().Add(300*time.Millisecond)); why != wake.TimedOut {
		t.Fatalf("ended %v after a rollback", why)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", wake.Channel, payload); err != nil {
		t.Fatal(err)
	}
	if why := w.Wait(ctx, time.Now().Add(300*time.Millisecond)); why != wake.TimedOut {
		t.Fatalf("ended %v before the commit", why)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if why := w.Wait(ctx, time.Now().Add(soon)); why != wake.Woken {
		t.Fatalf("ended %v, want woken by the commit", why)
	}
}
