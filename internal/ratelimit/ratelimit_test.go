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

// A call given back is the key's to make again. A refund never fills a
// bucket past its burst, nor makes one for a key that has none.
func TestACallGivenBackCanBeMadeAgain(t *testing.T) {
	now := time.Now()
	l := New(60, 2)
	l.SetClock(func() time.Time { return now })

	l.Refund("agent")
	if n := l.Len(); n != 0 {
		t.Fatalf("a refund to a key never seen: %d keys tracked, want none", n)
	}
	l.Allow("agent")
	l.Allow("agent")
	if ok, _ := l.Allow("agent"); ok {
		t.Fatal("a third call got through a burst of two")
	}
	l.Refund("agent")
	if ok, _ := l.Allow("agent"); !ok {
		t.Fatal("the call given back was refused")
	}
	if ok, _ := l.Allow("agent"); ok {
		t.Fatal("one call given back let two through")
	}
	for range 5 {
		l.Refund("agent")
	}
	for i := range 2 {
		if ok, _ := l.Allow("agent"); !ok {
			t.Fatalf("call %d after five refunds was refused", i+1)
		}
	}
	if ok, _ := l.Allow("agent"); ok {
		t.Fatal("five refunds filled a bucket of two past its burst")
	}
}

func TestNoLimit(t *testing.T) {
	l := New(0, 0)
	for range 1000 {
		if ok, _ := l.Allow("x"); !ok {
			t.Fatal("a nil limiter refused")
		}
		l.Refund("x")
	}
}
