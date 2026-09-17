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

	"golang.org/x/time/rate"
)

// Limiter holds one token bucket per key: an actor id, or for sign-in
// attempts an address or an email.
type Limiter struct {
	perSecond rate.Limit
	burst     int

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	swept   time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
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
	return &Limiter{perSecond: rate.Limit(float64(perMinute) / 60), burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow reports whether key may make a call now and, if not, how long until
// it may. A nil Limiter allows everything.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{lim: rate.NewLimiter(l.perSecond, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	l.sweep(now)
	l.mu.Unlock()

	r := b.lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Minute
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now) // we refuse rather than wait, so the token goes back
		return false, d
	}
	return true, 0
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
