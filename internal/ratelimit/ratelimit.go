// Package ratelimit is an in-memory token bucket keyed by string.
//
// State lives in the process. With several tilld instances behind a load balancer
// each instance enforces its own limit, so the effective limit is N times higher.
// Account lockout (persisted in Postgres) is the cross-instance control.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	mu      sync.Mutex
	burst   float64
	perSec  float64
	maxKeys int
	now     func() time.Time
	buckets map[string]*bucket
}

// New allows burst requests at once, refilling perMinute tokens per minute.
func New(burst int, perMinute float64) *Limiter {
	return &Limiter{
		burst: float64(burst), perSec: perMinute / 60, maxKeys: 10000,
		now: time.Now, buckets: map[string]*bucket{},
	}
}

// Allow takes one token. When it returns false, retryAfter is how long until one is available.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= l.maxKeys {
			l.sweep(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * l.perSec
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	if l.perSec <= 0 {
		return false, time.Minute
	}
	wait := time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// sweep drops buckets that have refilled completely: they are equivalent to absent keys.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, k)
		}
	}
}

func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
