package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/wake"
)

// NewPlatformWaking is NewPlatform on a pipeline whose reads may wait for
// news (wait_s), as serve's do: they wait in the hub it returns, which a
// listener on the test's database keeps told.
func NewPlatformWaking(t testing.TB, cfg wake.Config) (*Platform, *wake.Hub) {
	t.Helper()
	hub := wake.NewHub(cfg)
	p := newPlatform(t, func(fs *blob.FSStore) blob.Store { return fs }, pipeline.Config{ProposalTTL: pipeline.DefaultProposalTTL, Wake: hub})
	Listen(t, p.Pool, hub)
	return p, hub
}

// NewCS101Waking is NewCS101 on NewPlatformWaking.
func NewCS101Waking(t testing.TB, students int, cfg wake.Config) (*CS101, *wake.Hub) {
	t.Helper()
	p, hub := NewPlatformWaking(t, cfg)
	return cs101On(p, students), hub
}

// Listen keeps hub told of what is committed in pool's database until the
// test ends, and returns once it listens.
func Listen(t testing.TB, pool *pgxpool.Pool, hub *wake.Hub) {
	t.Helper()
	l := wake.NewListener(pool.Config().ConnConfig, hub, nil)
	l.MinBackoff, l.MaxBackoff = 20*time.Millisecond, 200*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(10 * time.Second); !l.Up(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the listener never listened")
		}
	}
}
