// Package ratelimit keeps one token bucket per key on an injected clock and answers at once, never queueing a caller.
package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu       sync.Mutex
	now      func() time.Time
	interval time.Duration
	burst    time.Duration
	due      map[string]time.Time
	swept    time.Time
}

func New(perMinute, burst int, now func() time.Time) *Limiter {
	if perMinute < 1 || burst < 1 || now == nil {
		panic("ratelimit: New needs a positive rate, a positive burst and a clock")
	}
	interval := max(time.Minute/time.Duration(perMinute), 1)
	return &Limiter{now: now, interval: interval, burst: time.Duration(burst) * interval, due: make(map[string]time.Time)}
}

func (l *Limiter) Allow(key string) (retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	l.sweep(t)
	next := l.due[key]
	if next.Before(t) {
		next = t
	}
	next = next.Add(l.interval)
	if over := next.Sub(t) - l.burst; over > 0 {
		return over, false
	}
	l.due[key] = next
	return 0, true
}

func (l *Limiter) sweep(t time.Time) {
	if t.Sub(l.swept) < l.burst {
		return
	}
	l.swept = t
	for key, next := range l.due {
		if !next.After(t) {
			delete(l.due, key)
		}
	}
}
