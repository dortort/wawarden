package ratelimit_test

import (
	"math"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
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

func TestPeekNeverConsumes(t *testing.T) {
	c := &clock{t: start}
	l := ratelimit.New(6, 2, c.now)
	for i := range 100 {
		if wait, ok := l.Peek("alpha"); !ok || wait != 0 {
			t.Fatalf("Peek %d on a full bucket = %v, %v, want an admission", i+1, wait, ok)
		}
	}
	if n := l.Len(); n != 0 {
		t.Fatalf("Len after peeking = %d, want 0", n)
	}
	for i := range 2 {
		if _, ok := l.Allow("alpha"); !ok {
			t.Fatalf("Allow %d of the burst was refused after peeking", i+1)
		}
	}
	for range 100 {
		if wait, ok := l.Peek("alpha"); ok || wait != 10*time.Second {
			t.Fatalf("Peek on an empty bucket = %v, %v, want a refusal for 10s", wait, ok)
		}
	}
	c.advance(10 * time.Second)
	if _, ok := l.Allow("alpha"); !ok {
		t.Fatal("refused peeks consumed the refill")
	}
}

func TestPropertyPeekAnswersWhatAllowWould(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		perMinute := rapid.IntRange(1, 1200).Draw(t, "per_minute")
		burst := rapid.IntRange(1, 10).Draw(t, "burst")
		c := &clock{t: start}
		l := ratelimit.New(perMinute, burst, c.now)
		interval := time.Minute / time.Duration(perMinute)
		keys := []string{"alpha", "bravo"}
		steps := rapid.IntRange(1, 200).Draw(t, "steps")
		for range steps {
			c.advance(time.Duration(rapid.Int64Range(0, int64(2*interval)).Draw(t, "gap")))
			key := rapid.SampledFrom(keys).Draw(t, "key")
			peeks := rapid.IntRange(1, 3).Draw(t, "peeks")
			var peekWait time.Duration
			var peekOK bool
			for range peeks {
				peekWait, peekOK = l.Peek(key)
			}
			if !rapid.Bool().Draw(t, "allow") {
				continue
			}
			if wait, ok := l.Allow(key); wait != peekWait || ok != peekOK {
				t.Fatalf("Peek answered %v, %v but Allow then answered %v, %v", peekWait, peekOK, wait, ok)
			}
		}
	})
}

const gib = int64(1) << 30

func TestHourLogCountLimitIsExact(t *testing.T) {
	c := &clock{t: start}
	h := ratelimit.NewHourLog(60, c.now)
	if _, ok := h.Commit(1); !ok {
		t.Fatal("the first fetch of the hour was refused")
	}
	c.advance(time.Minute)
	for i := range 59 {
		if _, ok := h.Commit(1); !ok {
			t.Fatalf("fetch %d of 60 was refused", i+2)
		}
	}
	wait, ok := h.Commit(1)
	if ok || wait != 59*time.Minute {
		t.Fatalf("fetch 61 = %v, %v, want a refusal for the 59m until the first fetch's hour ends", wait, ok)
	}
	c.advance(wait - time.Nanosecond)
	if wait, ok := h.Peek(1); ok || wait != time.Nanosecond {
		t.Fatalf("Peek 1ns before the first fetch ages out = %v, %v, want a refusal for 1ns", wait, ok)
	}
	c.advance(time.Nanosecond)
	if _, ok := h.Commit(1); !ok {
		t.Fatal("fetch 61 was refused when the first fetch's hour ended")
	}
	if wait, ok := h.Commit(1); ok || wait != time.Minute {
		t.Fatalf("fetch 62 = %v, %v, want a refusal for the 1m until the next fetches age out", wait, ok)
	}
}

func TestHourLogByteLimitIsExact(t *testing.T) {
	c := &clock{t: start}
	h := ratelimit.NewHourLog(4*gib, c.now)
	if _, ok := h.Commit(gib); !ok {
		t.Fatal("the first GiB was refused")
	}
	c.advance(10 * time.Minute)
	if _, ok := h.Commit(2 * gib); !ok {
		t.Fatal("the next 2 GiB were refused")
	}
	c.advance(10 * time.Minute)
	if wait, ok := h.Peek(gib + 1); ok || wait != 40*time.Minute {
		t.Fatalf("Peek one byte over the limit = %v, %v, want a refusal for 40m", wait, ok)
	}
	if _, ok := h.Peek(gib); !ok {
		t.Fatal("Peek of exactly the remaining GiB was refused")
	}
	if _, ok := h.Commit(gib); !ok {
		t.Fatal("Commit of exactly the remaining GiB was refused")
	}
	if wait, ok := h.Peek(1); ok || wait != 40*time.Minute {
		t.Fatalf("Peek of one byte on a full log = %v, %v, want a refusal for 40m", wait, ok)
	}
	c.advance(40 * time.Minute)
	if _, ok := h.Peek(gib); !ok {
		t.Fatal("the first GiB still counts after its hour ended")
	}
	if wait, ok := h.Peek(gib + 1); ok || wait != 10*time.Minute {
		t.Fatalf("Peek one byte over the freed GiB = %v, %v, want a refusal for the 10m until the 2 GiB age out", wait, ok)
	}
	if wait, ok := h.Peek(4*gib + 1); ok || wait != time.Hour {
		t.Fatalf("Peek above the limit = %v, %v, want a refusal for an hour", wait, ok)
	}
	large := ratelimit.NewHourLog(64*gib, c.now)
	if _, ok := large.Commit(64 * gib); !ok {
		t.Fatal("a 64 GiB log refused 64 GiB")
	}
	if wait, ok := large.Peek(1); ok || wait != time.Hour {
		t.Fatalf("Peek on a full 64 GiB log = %v, %v, want a refusal for an hour", wait, ok)
	}
}

func TestPeekingSeveralBucketsCommitsNothing(t *testing.T) {
	c := &clock{t: start}
	client := ratelimit.New(6, 2, c.now)
	count := ratelimit.NewHourLog(60, c.now)
	size := ratelimit.NewHourLog(4*gib, c.now)
	if _, ok := size.Commit(4*gib - 100); !ok {
		t.Fatal("filling the byte log was refused")
	}
	for range 1000 {
		_, clientOK := client.Peek("alpha")
		_, countOK := count.Peek(1)
		wait, sizeOK := size.Peek(200)
		if !clientOK || !countOK || sizeOK || wait != time.Hour {
			t.Fatalf("peeks = %v, %v, %v (%v), want the client and count to fit and the bytes to wait an hour", clientOK, countOK, sizeOK, wait)
		}
	}
	if n, m, k := client.Len(), count.Len(), size.Len(); n != 0 || m != 0 || k != 1 {
		t.Fatalf("after peeking, Len = %d, %d, %d, want 0, 0, 1", n, m, k)
	}
	for i := range 2 {
		if _, ok := client.Allow("alpha"); !ok {
			t.Fatalf("the client's request %d of its burst was refused after peeking", i+1)
		}
	}
	if _, ok := client.Allow("alpha"); ok {
		t.Fatal("the client's burst grew")
	}
	for i := range 60 {
		if _, ok := count.Commit(1); !ok {
			t.Fatalf("fetch %d of 60 was refused after peeking", i+1)
		}
	}
	if _, ok := count.Commit(1); ok {
		t.Fatal("the count log admitted a 61st fetch")
	}
	if _, ok := size.Commit(100); !ok {
		t.Fatal("the last 100 bytes were refused after peeking")
	}
	if _, ok := size.Peek(1); ok {
		t.Fatal("the byte log admitted a byte over its limit")
	}
}

func TestDebitBelowZeroBlocksUntilItAgesOut(t *testing.T) {
	c := &clock{t: start}
	h := ratelimit.NewHourLog(100, c.now)
	if _, ok := h.Commit(60); !ok {
		t.Fatal("the declared 60 were refused")
	}
	c.advance(10 * time.Minute)
	h.Debit(150)
	for _, w := range []int64{1, 100} {
		if wait, ok := h.Peek(w); ok || wait != time.Hour {
			t.Fatalf("Peek(%d) with a balance of -110 = %v, %v, want a refusal for the hour the debit counts", w, wait, ok)
		}
	}
	if wait, ok := h.Commit(1); ok || wait != time.Hour {
		t.Fatalf("Commit(1) with a balance of -110 = %v, %v, want a refusal for an hour", wait, ok)
	}
	c.advance(50 * time.Minute)
	if wait, ok := h.Peek(1); ok || wait != 10*time.Minute {
		t.Fatalf("Peek(1) once only the debit counts = %v, %v, want a refusal for 10m", wait, ok)
	}
	c.advance(10*time.Minute - time.Nanosecond)
	if wait, ok := h.Peek(1); ok || wait != time.Nanosecond {
		t.Fatalf("Peek(1) 1ns before the debit ages out = %v, %v, want a refusal for 1ns", wait, ok)
	}
	c.advance(time.Nanosecond)
	if _, ok := h.Commit(100); !ok {
		t.Fatal("the whole limit was refused after the debit aged out")
	}
}

func TestHourLogHasNoRefund(t *testing.T) {
	c := &clock{t: start}
	h := ratelimit.NewHourLog(10, c.now)
	if _, ok := h.Commit(10); !ok {
		t.Fatal("the full limit was refused")
	}
	c.advance(time.Minute)
	for _, w := range []int64{0, -1, -10, math.MinInt64} {
		h.Debit(w)
		if wait, ok := h.Commit(w); ok || wait != time.Hour {
			t.Fatalf("Commit(%d) = %v, %v, want a refusal for an hour", w, wait, ok)
		}
	}
	if wait, ok := h.Commit(11); ok || wait != time.Hour {
		t.Fatalf("Commit above the limit = %v, %v, want a refusal for an hour", wait, ok)
	}
	if wait, ok := h.Commit(1); ok || wait != 59*time.Minute {
		t.Fatalf("Commit(1) after attempted refunds = %v, %v, want a refusal for 59m", wait, ok)
	}
	if n := h.Len(); n != 1 {
		t.Fatalf("Len = %d, want only the first commit", n)
	}
	var methods []string
	for m := range reflect.TypeFor[*ratelimit.HourLog]().Methods() {
		methods = append(methods, m.Name)
	}
	if want := []string{"Commit", "Debit", "Len", "Peek"}; !slices.Equal(methods, want) {
		t.Fatalf("HourLog exports %q, want %q with Len test-only: a method that lowers the log is a refund path", methods, want)
	}
}

func TestHourLogClockSteppingBackCountsNoLess(t *testing.T) {
	c := &clock{t: start}
	h := ratelimit.NewHourLog(2, c.now)
	c.advance(30 * time.Minute)
	if _, ok := h.Commit(1); !ok {
		t.Fatal("the first fetch was refused")
	}
	c.advance(-30 * time.Minute)
	if wait, ok := h.Peek(2); ok || wait != 90*time.Minute {
		t.Fatalf("Peek(2) after the clock stepped back = %v, %v, want a refusal for 90m", wait, ok)
	}
	if _, ok := h.Commit(1); !ok {
		t.Fatal("the second fetch was refused")
	}
	c.advance(90*time.Minute - time.Nanosecond)
	if wait, ok := h.Peek(1); ok || wait != time.Nanosecond {
		t.Fatalf("Peek(1) 1ns before the newest entry ages out = %v, %v, want a refusal for 1ns", wait, ok)
	}
	c.advance(time.Nanosecond)
	if _, ok := h.Peek(2); !ok {
		t.Fatal("the log was still full after every entry's hour ended")
	}
}

func TestHourLogKeepsOnlyTheLastHour(t *testing.T) {
	c := &clock{t: start}
	h := ratelimit.NewHourLog(1000, c.now)
	for i := range 5 * 60 {
		if _, ok := h.Commit(1); !ok {
			t.Fatalf("commit %d was refused", i+1)
		}
		h.Debit(1)
		if n := h.Len(); n > 120 {
			t.Fatalf("Len after %d minutes = %d, want at most the 120 entries of the last hour", i+1, n)
		}
		c.advance(time.Minute)
	}
	if n := h.Len(); n != 120 {
		t.Fatalf("Len = %d, want the 120 entries of the last hour", n)
	}
}

func TestHourLogAdmitsExactlyItsLimitConcurrently(t *testing.T) {
	const workers, perWorker, limit = 16, 200, 1000
	c := &clock{t: start}
	h := ratelimit.NewHourLog(limit, c.now)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
				h.Peek(1)
				if _, ok := h.Commit(1); ok {
					admitted.Add(1)
				}
				h.Debit(0)
			}
		})
	}
	wg.Wait()
	if n := admitted.Load(); n != limit {
		t.Fatalf("%d concurrent commits admitted %d, want exactly %d", workers*perWorker, n, limit)
	}
}

func TestDebitAndPeekAreSafeConcurrently(t *testing.T) {
	const workers, rounds = 16, 200
	c := &clock{t: start}
	h := ratelimit.NewHourLog(1000, c.now)
	l := ratelimit.New(6, 2, c.now)
	var committed, allowed atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for range rounds {
				if w%2 == 0 {
					if _, ok := h.Commit(1); ok {
						committed.Add(1)
					}
					if _, ok := l.Allow("alpha"); ok {
						allowed.Add(1)
					}
				} else {
					h.Debit(1)
					l.Peek("alpha")
				}
			}
		})
	}
	wg.Wait()
	if n := committed.Load(); n > 1000 {
		t.Fatalf("commits racing debits admitted %d, want at most 1000", n)
	}
	if n := allowed.Load(); n != 2 {
		t.Fatalf("allows racing peeks admitted %d, want the burst of 2", n)
	}
	if _, ok := h.Peek(1); ok {
		t.Fatal("the hour log admits more weight after its debits filled it")
	}
}

func TestNewHourLogRefusesNonsense(t *testing.T) {
	for _, tt := range []struct {
		name  string
		limit int64
		now   func() time.Time
	}{
		{name: "zero limit", limit: 0, now: time.Now},
		{name: "negative limit", limit: -1, now: time.Now},
		{name: "no clock", limit: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewHourLog accepted it")
				}
			}()
			ratelimit.NewHourLog(tt.limit, tt.now)
		})
	}
}

type record struct {
	at time.Time
	w  int64
}

func counting(log []record, t time.Time) int64 {
	var sum int64
	for _, r := range log {
		if !r.at.After(t) && t.Sub(r.at) < time.Hour {
			sum += r.w
		}
	}
	return sum
}

func modelPeek(log []record, limit, w int64, t time.Time) (time.Duration, bool) {
	if w < 1 || w > limit {
		return time.Hour, false
	}
	if counting(log, t)+w <= limit {
		return 0, true
	}
	for _, r := range log {
		if end := r.at.Add(time.Hour); end.After(t) && counting(log, end)+w <= limit {
			return end.Sub(t), false
		}
	}
	return time.Hour, false
}

func TestPropertyHourLogMatchesARollingHourModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		limit := rapid.Int64Range(1, 1000).Draw(t, "limit")
		c := &clock{t: start}
		h := ratelimit.NewHourLog(limit, c.now)
		var log []record
		steps := rapid.IntRange(1, 300).Draw(t, "steps")
		for range steps {
			c.advance(time.Duration(rapid.Int64Range(0, int64(20*time.Minute)).Draw(t, "gap")))
			switch rapid.IntRange(0, 9).Draw(t, "action") {
			case 0:
				d := rapid.Int64Range(-limit, 2*limit).Draw(t, "debit")
				h.Debit(d)
				if d > 0 {
					log = append(log, record{at: c.now(), w: d})
				}
			case 1:
				w := rapid.SampledFrom([]int64{math.MinInt64, -1, 0, limit + 1, math.MaxInt64}).Draw(t, "invalid")
				if wait, ok := h.Peek(w); ok || wait != time.Hour {
					t.Fatalf("Peek(%d) with limit %d = %v, %v, want a refusal for an hour", w, limit, wait, ok)
				}
				if wait, ok := h.Commit(w); ok || wait != time.Hour {
					t.Fatalf("Commit(%d) with limit %d = %v, %v, want a refusal for an hour", w, limit, wait, ok)
				}
			default:
				w := rapid.Int64Range(1, limit).Draw(t, "weight")
				wantWait, wantOK := modelPeek(log, limit, w, c.now())
				wait, ok := h.Peek(w)
				if wait != wantWait || ok != wantOK {
					t.Fatalf("Peek(%d) = %v, %v, want %v, %v from the rolling hour", w, wait, ok, wantWait, wantOK)
				}
				if !ok {
					if wait <= 0 {
						t.Fatalf("a refusal asked to wait %v", wait)
					}
					before := h.Len()
					if again, ok := h.Commit(w); ok || again != wait {
						t.Fatalf("Commit after a refused Peek = %v, %v, want the same refusal for %v", again, ok, wait)
					}
					if n := h.Len(); n != before {
						t.Fatalf("a refused Commit changed Len from %d to %d", before, n)
					}
					if !rapid.Bool().Draw(t, "wait") {
						continue
					}
					c.advance(wait - time.Nanosecond)
					if _, ok := h.Peek(w); ok {
						t.Fatalf("Peek(%d) was admitted 1ns before the %v it was told to wait", w, wait)
					}
					c.advance(time.Nanosecond)
				}
				if wait, ok := h.Commit(w); !ok {
					t.Fatalf("Commit(%d) was refused for %v where the rolling hour has room", w, wait)
				}
				log = append(log, record{at: c.now(), w: w})
				if sum := counting(log, c.now()); sum > limit {
					t.Fatalf("an admission left %d in the hour, above the limit %d", sum, limit)
				}
			}
		}
	})
}
