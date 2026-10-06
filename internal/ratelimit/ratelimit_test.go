package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/ratelimit"
)

var start = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBurstThenOnePerInterval(t *testing.T) {
	c := &clock{t: start}
	l := ratelimit.New(600, 60, c.now)
	for i := range 60 {
		if _, ok := l.Allow("alpha"); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	wait, ok := l.Allow("alpha")
	if ok || wait != 100*time.Millisecond {
		t.Fatalf("Allow after the burst = %v, %v, want a refusal for 100ms", wait, ok)
	}
	if _, ok := l.Allow("bravo"); !ok {
		t.Fatal("a second key shares the first key's budget")
	}
	c.advance(99 * time.Millisecond)
	if wait, ok := l.Allow("alpha"); ok || wait != time.Millisecond {
		t.Fatalf("Allow 1ms early = %v, %v, want a refusal for 1ms", wait, ok)
	}
	c.advance(time.Millisecond)
	if _, ok := l.Allow("alpha"); !ok {
		t.Fatal("a full interval later the request was refused")
	}
	if _, ok := l.Allow("alpha"); ok {
		t.Fatal("one interval admitted two requests")
	}
}

func TestSlowRatesKeepTheirBurst(t *testing.T) {
	c := &clock{t: start}
	l := ratelimit.New(60, 6, c.now)
	for range 6 {
		if _, ok := l.Allow("alpha"); !ok {
			t.Fatal("the burst was refused")
		}
	}
	if wait, ok := l.Allow("alpha"); ok || wait != time.Second {
		t.Fatalf("Allow after the burst = %v, %v, want a refusal for 1s", wait, ok)
	}
}

func TestIdleKeysAreEvicted(t *testing.T) {
	c := &clock{t: start}
	l := ratelimit.New(600, 60, c.now)
	for _, key := range []string{"alpha", "bravo", "charlie"} {
		l.Allow(key)
	}
	if n := l.Len(); n != 3 {
		t.Fatalf("Len = %d, want 3", n)
	}
	c.advance(6 * time.Second)
	l.Allow("delta")
	if n := l.Len(); n != 1 {
		t.Fatalf("Len after a full refill = %d, want only the new key", n)
	}
}

func TestNewRefusesNonsense(t *testing.T) {
	for _, tt := range []struct {
		name             string
		perMinute, burst int
		now              func() time.Time
	}{
		{name: "zero rate", perMinute: 0, burst: 1, now: time.Now},
		{name: "negative burst", perMinute: 1, burst: -1, now: time.Now},
		{name: "no clock", perMinute: 1, burst: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New accepted it")
				}
			}()
			ratelimit.New(tt.perMinute, tt.burst, tt.now)
		})
	}
}

func TestPropertyAdmissionsNeverExceedBurstPlusRate(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		perMinute := rapid.IntRange(1, 1200).Draw(t, "per_minute")
		burst := rapid.IntRange(1, 60).Draw(t, "burst")
		c := &clock{t: start}
		l := ratelimit.New(perMinute, burst, c.now)
		interval := time.Minute / time.Duration(perMinute)
		keys := []string{"alpha", "bravo"}
		accepted := map[string][]time.Time{}
		steps := rapid.IntRange(1, 300).Draw(t, "steps")
		for range steps {
			c.advance(time.Duration(rapid.Int64Range(0, int64(3*interval)).Draw(t, "gap")))
			key := rapid.SampledFrom(keys).Draw(t, "key")
			wait, ok := l.Allow(key)
			if !ok {
				if wait <= 0 || wait > interval {
					t.Fatalf("a refusal asked to wait %v with an interval of %v", wait, interval)
				}
				c.advance(wait)
				if _, ok := l.Allow(key); !ok {
					t.Fatalf("the request was refused again after waiting the %v it was told", wait)
				}
			}
			accepted[key] = append(accepted[key], c.now())
		}
		for key, times := range accepted {
			for i := range times {
				for j := i; j < len(times); j++ {
					window := times[j].Sub(times[i])
					if limit := float64(burst) + float64(window)/float64(interval); float64(j-i+1) > limit {
						t.Fatalf("key %s: %d admissions in %v, above burst %d plus rate (%v per interval)", key, j-i+1, window, burst, interval)
					}
				}
			}
		}
	})
}
