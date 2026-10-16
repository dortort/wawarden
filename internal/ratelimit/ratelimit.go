// Package ratelimit keeps one token bucket per key and weighted sliding-hour logs on an injected clock, and answers at once, never queueing a caller.
package ratelimit

import (
	"slices"
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
	next, over := l.next(key, t)
	if over > 0 {
		return over, false
	}
	l.due[key] = next
	return 0, true
}

func (l *Limiter) Peek(key string) (retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, over := l.next(key, l.now()); over > 0 {
		return over, false
	}
	return 0, true
}

func (l *Limiter) next(key string, t time.Time) (next time.Time, over time.Duration) {
	next = l.due[key]
	if next.Before(t) {
		next = t
	}
	next = next.Add(l.interval)
	return next, next.Sub(t) - l.burst
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

type HourLog struct {
	mu      sync.Mutex
	now     func() time.Time
	limit   int64
	entries []entry
}

type entry struct {
	at time.Time
	w  int64
}

func NewHourLog(limit int64, now func() time.Time) *HourLog {
	if limit < 1 || now == nil {
		panic("ratelimit: NewHourLog needs a positive limit and a clock")
	}
	return &HourLog{now: now, limit: limit}
}

func (h *HourLog) Peek(w int64) (retryAfter time.Duration, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fits(w, h.now())
}

func (h *HourLog) Commit(w int64) (retryAfter time.Duration, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.now()
	if retryAfter, ok = h.fits(w, t); ok {
		h.record(w, t)
	}
	return retryAfter, ok
}

func (h *HourLog) Debit(w int64) {
	if w < 1 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record(w, h.now())
}

func (h *HourLog) fits(w int64, t time.Time) (retryAfter time.Duration, ok bool) {
	if w < 1 || w > h.limit {
		return time.Hour, false
	}
	room := h.limit - w
	for i := len(h.entries) - 1; i >= 0; i-- {
		end := h.entries[i].at.Add(time.Hour)
		if !end.After(t) {
			break
		}
		if h.entries[i].w > room {
			return end.Sub(t), false
		}
		room -= h.entries[i].w
	}
	return 0, true
}

func (h *HourLog) record(w int64, t time.Time) {
	aged := 0
	for aged < len(h.entries) && !h.entries[aged].at.Add(time.Hour).After(t) {
		aged++
	}
	h.entries = slices.Delete(h.entries, 0, aged)
	// A clock that stepped back records at the newest time, keeping the log sorted and no entry ageing early.
	if n := len(h.entries); n > 0 && h.entries[n-1].at.After(t) {
		t = h.entries[n-1].at
	}
	h.entries = append(h.entries, entry{at: t, w: w})
}
