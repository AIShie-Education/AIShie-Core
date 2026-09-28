// Package ratelimit bounds how fast one caller may call. It exists because of
// what the action log is: every attempt is recorded, denials included, so an
// agent stuck in a loop would otherwise write a row per call for as long as
// it liked. A call refused here was never attempted, and records nothing.
//
// The limits are per instance and in memory. That is deliberate: there is no
// shared cache in this system, and the aim is to stop a runaway client, not
// to meter usage. With N instances behind a balancer a caller gets up to N
// times the rate, which still stops a loop.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter holds one token bucket per key: an actor id, or for sign-in
// attempts an address, an email or a login ID.
type Limiter struct {
	perSecond float64
	burst     float64

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	swept   time.Time
}

// bucket is what a key may still call, as of when it was last seen. It fills
// by perSecond a second, up to burst.
type bucket struct {
	tokens float64
	seen   time.Time
}

// idleAfter is how long a key may go unseen before its bucket is forgotten.
// A forgotten bucket comes back full, so it must be long enough for any
// bucket to have refilled anyway.
const idleAfter = 10 * time.Minute

// New allows perMinute calls a minute per key, with bursts of up to burst.
// perMinute <= 0 means no limit.
func New(perMinute, burst int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{perSecond: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

// Allow reports whether key may make a call now and, if not, how long until
// it may. A nil Limiter allows everything.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	l.fill(b, now)
	l.sweep(now)
	if b.tokens < 1 {
		// We refuse rather than wait, and a refusal takes nothing.
		return false, time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// Refund gives key back a call Allow let it make, when that call turned out
// not to be what the limit is for. It never fills a bucket past its burst,
// and makes none for a key that has none: a new bucket starts full.
func (l *Limiter) Refund(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[key]; b != nil {
		l.fill(b, l.now())
		b.tokens = min(b.tokens+1, l.burst)
	}
}

// fill gives b what it has earned since it was last seen. Called with the
// lock held.
func (l *Limiter) fill(b *bucket, now time.Time) {
	if now.After(b.seen) {
		b.tokens = min(b.tokens+now.Sub(b.seen).Seconds()*l.perSecond, l.burst)
		b.seen = now
	}
}

// sweep forgets buckets nobody has used for a while, so that the map does not
// grow with every key ever seen. Called with the lock held.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idleAfter {
			delete(l.buckets, k)
		}
	}
}

// Len is the number of keys being tracked, for tests.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// SetClock replaces the clock, for tests.
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }
