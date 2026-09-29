package wake

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// ApplicationName is what the listening connection calls itself to the
// server, as pg_stat_activity shows it.
const ApplicationName = "aishiterud wake"

// Listener keeps one connection of its own listening on Channel, and hands
// each notification to its hub. It is not the pool's: a connection that
// listens is held for as long as it does, and must not be handed to anyone
// else meanwhile.
type Listener struct {
	hub     *Hub
	connect func(ctx context.Context) (*pgx.Conn, error)
	log     *slog.Logger

	// MinBackoff and MaxBackoff bound the pause before connecting again
	// after a failure, doubled from one to the other, with jitter.
	// PingEvery is how long the connection may be quiet before it is asked
	// whether it is still there: one that went away without a word would
	// otherwise be listened to for good.
	MinBackoff, MaxBackoff, PingEvery time.Duration

	up atomic.Bool
}

// NewListener listens with connections made as cfg says: the pool's own
// settings, copied.
func NewListener(cfg *pgx.ConnConfig, hub *Hub, log *slog.Logger) *Listener {
	cfg = cfg.Copy()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = ApplicationName
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Listener{
		hub: hub, log: log,
		connect:    func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, cfg) },
		MinBackoff: 250 * time.Millisecond, MaxBackoff: 10 * time.Second, PingEvery: 30 * time.Second,
	}
}

// Up says whether the listener is listening now.
func (l *Listener) Up() bool { return l.up.Load() }

// Run listens until ctx is done, connecting again whenever the connection
// fails. Each time it is listening again, it wakes every waiting call, since
// what was committed while it was not was heard by nobody here.
func (l *Listener) Run(ctx context.Context) {
	backoff := l.MinBackoff
	failing := false
	for ctx.Err() == nil {
		err := l.listen(ctx, func() {
			backoff = l.MinBackoff
			if failing {
				l.log.Info("listening for wake-ups again", "channel", Channel)
				failing = false
			}
		})
		if ctx.Err() != nil {
			return
		}
		if !failing {
			l.log.Warn("not listening for wake-ups; calls that wait for news wait out their time until it is back", "err", err)
			failing = true
		}
		// Half to one and a half times the backoff: the instances of a
		// server whose database restarted do not all come back at once.
		backoff = max(backoff, time.Millisecond)
		pause := time.Duration(rand.Int64N(int64(backoff))) + backoff/2 //nolint:gosec // jitter, not a secret
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		backoff = min(2*backoff, l.MaxBackoff)
	}
}

// listen connects, listens, and hands on what it hears until the connection
// fails or ctx is done. listening is called once it listens.
func (l *Listener) listen(ctx context.Context, listening func()) error {
	conn, err := l.connect(ctx)
	if err != nil {
		return err
	}
	defer func() {
		l.up.Store(false)
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = conn.Close(closing)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
		return err
	}
	l.up.Store(true)
	listening()
	l.hub.WakeAll()
	for {
		quiet, cancel := context.WithTimeout(ctx, l.PingEvery)
		n, err := conn.WaitForNotification(quiet)
		cancel()
		switch {
		case err == nil:
			l.hand(n.Payload)
			continue
		case ctx.Err() != nil:
			return ctx.Err()
		case !errors.Is(err, context.DeadlineExceeded) || conn.IsClosed():
			return err
		}
		// Quiet for PingEvery: is anyone there?
		ping, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = conn.Ping(ping)
		cancel()
		if err != nil {
			return err
		}
	}
}

// hand passes a notification on. One that does not read as a Note was not
// written by Core, and wakes nobody.
func (l *Listener) hand(payload string) {
	var n Note
	if err := json.Unmarshal([]byte(payload), &n); err != nil || n.Kind == "" {
		l.log.Warn("a wake-up that is not Core's", "channel", Channel, "bytes", len(payload))
		return
	}
	l.hub.Publish(n)
}
