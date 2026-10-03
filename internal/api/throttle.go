package api

import (
	"sync"
	"time"
)

const (
	failureBurst  = 30
	failureRefill = time.Second
)

type bucket struct {
	mu     sync.Mutex
	now    func() time.Time
	tokens float64
	last   time.Time
}

func newBucket(now func() time.Time) *bucket {
	return &bucket{now: now, tokens: failureBurst, last: now()}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	if t.After(b.last) {
		b.tokens = min(failureBurst, b.tokens+float64(t.Sub(b.last))/float64(failureRefill))
		b.last = t
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
