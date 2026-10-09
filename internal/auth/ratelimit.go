package auth

import (
	"sync"
	"time"
)

// Limiter is a keyed token-bucket rate limiter kept in memory.
// Keys are things like "ip:203.0.113.5" or "email:a@b.c".
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter allows burst events immediately, then one every interval.
func NewLimiter(burst int, interval time.Duration) *Limiter {
	return &Limiter{
		rate:    1 / interval.Seconds(),
		burst:   float64(burst),
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// WithClock replaces the time source; used to share the server's clock.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allow consumes one token for key and reports whether it was available.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gc drops buckets that have refilled completely; they carry no state.
func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}
