package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Now()
	l := New(60, 5) // one a second, bursts of five
	l.SetClock(func() time.Time { return now })

	for i := range 5 {
		if ok, _ := l.Allow("agent"); !ok {
			t.Fatalf("call %d of the burst was refused", i+1)
		}
	}
	ok, wait := l.Allow("agent")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("sixth call: ok=%v retry after %s, want refused for about a second", ok, wait)
	}
	// Refusing must not cost a token: a client that keeps hammering gets back
	// in at the same moment as one that waited.
	for range 50 {
		l.Allow("agent")
	}
	// One key's loop is nobody else's problem.
	if ok, _ := l.Allow("someone else"); !ok {
		t.Fatal("another key was refused")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("agent"); !ok {
		t.Fatal("still refused after the wait it was told")
	}
	if ok, _ := l.Allow("agent"); ok {
		t.Fatal("a second call got through on one second's allowance")
	}

	// Idle keys are forgotten.
	now = now.Add(idleAfter + 2*time.Minute)
	l.Allow("a new key")
	if n := l.Len(); n != 1 {
		t.Fatalf("%d keys tracked, want only the one just seen", n)
	}
}

func TestNoLimit(t *testing.T) {
	l := New(0, 0)
	for range 1000 {
		if ok, _ := l.Allow("x"); !ok {
			t.Fatal("a nil limiter refused")
		}
	}
}
