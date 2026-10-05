package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
)

type lines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lines) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(l.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

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

func newNotifier(t *testing.T) (*Notifier, *lines, *clock) {
	t.Helper()
	out := &lines{}
	w := logx.NewWriter(out)
	w.SetKey(make([]byte, 32))
	c := &clock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	n, err := New(Options{Writer: w, Now: c.now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n, out, c
}

var baseKeys = []string{"event", "level", "msg", "time"}

func TestEveryEventIsOneLineWithAFixedSetOfFields(t *testing.T) {
	tests := []struct {
		event  string
		level  string
		fire   func(*Notifier)
		fields map[string]any
	}{
		{event: EventUnpaired, level: "WARN", fire: (*Notifier).Unpaired},
		{event: EventDisconnected, level: "WARN", fire: func(n *Notifier) { n.Disconnected("logged_out") }, fields: map[string]any{"reason": "logged_out"}},
		{event: EventPairRejected, level: "WARN", fire: func(n *Notifier) { n.PairRejected("before_save") }, fields: map[string]any{"stage": "before_save"}},
		{event: EventLogoutFailed, level: "WARN", fire: func(n *Notifier) { n.LogoutFailed(2, "*fmt.wrapError") }, fields: map[string]any{"attempt": float64(2), "error_type": "*fmt.wrapError"}},
		{event: EventQuarantine, level: "WARN", fire: func(n *Notifier) { n.Quarantine("history", 3) }, fields: map[string]any{"queue": "history", "attempts": float64(3)}},
		{event: EventRekeyConflict, level: "WARN", fire: func(n *Notifier) { n.RekeyConflict("mapping_contradicts") }, fields: map[string]any{"conflict": "mapping_contradicts"}},
		{event: EventIngestPaused, level: "WARN", fire: func(n *Notifier) { n.IngestPaused(1024, 268435456) }, fields: map[string]any{"free_bytes": float64(1024), "floor_bytes": float64(268435456)}},
		{event: EventAdminMutation, level: "INFO", fire: func(n *Notifier) { n.AdminMutation("pair", "already_paired") }, fields: map[string]any{"action": "pair", "outcome": "already_paired"}},
		{event: EventAdminAuthFailure, level: "WARN", fire: (*Notifier).AdminAuthFailure, fields: map[string]any{"count": float64(1)}},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			n, out, _ := newNotifier(t)
			tt.fire(n)
			recs := out.records(t)
			if len(recs) != 1 {
				t.Fatalf("%d lines, want one: %q", len(recs), out)
			}
			rec := recs[0]
			wantKeys := slices.Concat(baseKeys, slices.Collect(maps.Keys(tt.fields)))
			slices.Sort(wantKeys)
			if keys := slices.Sorted(maps.Keys(rec)); !slices.Equal(keys, wantKeys) {
				t.Fatalf("keys %q, want exactly %q", keys, wantKeys)
			}
			if rec["event"] != tt.event || rec["level"] != tt.level {
				t.Fatalf("event %v at %v, want %s at %s", rec["event"], rec["level"], tt.event, tt.level)
			}
			for k, v := range tt.fields {
				if !reflect.DeepEqual(rec[k], v) {
					t.Fatalf("%s = %#v, want %#v", k, rec[k], v)
				}
			}
			seen[tt.event] = true
		})
	}
	for _, event := range []string{EventAdminMutation, EventAdminAuthFailure, EventUnpaired, EventDisconnected, EventPairRejected,
		EventLogoutFailed, EventQuarantine, EventRekeyConflict, EventIngestPaused} {
		if !seen[event] {
			t.Errorf("no case for %s", event)
		}
	}
}

func TestValuesThatAreNotCodesNeverLeave(t *testing.T) {
	const canary = "15550100042"
	n, out, _ := newNotifier(t)
	for _, v := range []string{canary + "@s.whatsapp.net", canary, "Alice Example", "Logged_Out", "", "reason\nevent", strings.Repeat("a", 65), "a@b"} {
		n.Disconnected(v)
		n.PairRejected(v)
		n.Quarantine(v, 1)
		n.RekeyConflict(v)
		n.AdminMutation(v, v)
		n.LogoutFailed(1, v)
	}
	if strings.Contains(out.String(), canary) || strings.Contains(out.String(), "Alice") || strings.Contains(out.String(), "Logged_Out") {
		t.Fatalf("a value that is not a code reached the output:\n%s", out)
	}
	for _, rec := range out.records(t) {
		for _, key := range []string{"reason", "stage", "queue", "conflict", "action", "outcome", "error_type"} {
			if v, ok := rec[key]; ok && v != invalidValue {
				t.Fatalf("%s = %q, want %q", key, v, invalidValue)
			}
		}
	}
}

func TestErrorTypesAreGoTypeNames(t *testing.T) {
	for _, v := range []string{"*errors.errorString", "*fmt.wrapError", "context.deadlineExceededError", "*net.OpError", "*wa.storeError[github.com/x/y.Z]"} {
		if got := goType("error_type", v).Value.String(); got != v {
			t.Errorf("%q became %q", v, got)
		}
	}
	for _, v := range []string{"", "15550100001", "15550100001@s.whatsapp.net", "Alice", "*a b", "a.b c", "x." + strings.Repeat("A", 200)} {
		if got := goType("error_type", v).Value.String(); got != invalidValue {
			t.Errorf("%q became %q", v, got)
		}
	}
}

func authFailures(t *testing.T, out *lines) []float64 {
	t.Helper()
	var counts []float64
	for _, rec := range out.records(t) {
		if rec["event"] == EventAdminAuthFailure {
			counts = append(counts, rec["count"].(float64))
		}
	}
	return counts
}

func TestAdminAuthFailuresAreReportedAtMostOncePerMinute(t *testing.T) {
	n, out, c := newNotifier(t)
	n.AdminAuthFailure()
	for range 5 {
		c.advance(time.Second)
		n.AdminAuthFailure()
	}
	n.flushAuthFailures(false)
	if got := authFailures(t, out); !slices.Equal(got, []float64{1}) {
		t.Fatalf("admin_auth_failure counts %v within the first minute, want [1]", got)
	}
	c.advance(authFailureWindow)
	n.flushAuthFailures(false)
	if got := authFailures(t, out); !slices.Equal(got, []float64{1, 5}) {
		t.Fatalf("admin_auth_failure counts %v after the minute, want the 5 held back reported", got)
	}
	n.flushAuthFailures(false)
	c.advance(authFailureWindow)
	n.flushAuthFailures(false)
	if got := authFailures(t, out); len(got) != 2 {
		t.Fatalf("admin_auth_failure counts %v: nothing pending must report nothing", got)
	}
	n.AdminAuthFailure()
	n.AdminAuthFailure()
	n.Start(t.Context())
	if err := n.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := authFailures(t, out); !slices.Equal(got, []float64{1, 5, 1, 1}) {
		t.Fatalf("admin_auth_failure counts %v, want a failure after a quiet minute reported at once and the rest at Stop", got)
	}
}

func TestHeldBackAuthFailuresAreReportedByTheTicker(t *testing.T) {
	n, out, c := newNotifier(t)
	if n.flushEvery != 10*time.Second {
		t.Fatalf("held-back failures are checked every %v, want the documented ten seconds", n.flushEvery)
	}
	n.flushEvery = 5 * time.Millisecond
	n.AdminAuthFailure()
	n.AdminAuthFailure()
	n.AdminAuthFailure()
	n.Start(t.Context())
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	time.Sleep(50 * time.Millisecond)
	if got := authFailures(t, out); !slices.Equal(got, []float64{1}) {
		t.Fatalf("admin_auth_failure counts %v within the minute, want [1]", got)
	}
	c.advance(authFailureWindow)
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(authFailures(t, out), []float64{1, 2}) {
		if time.Now().After(deadline) {
			t.Fatalf("admin_auth_failure counts %v, want the two held back reported by the ticker without Stop", authFailures(t, out))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopBeforeStart(t *testing.T) {
	n, _, _ := newNotifier(t)
	if err := n.Stop(t.Context()); err == nil {
		t.Fatal("Stop before Start succeeded")
	}
}

func TestNewNeedsAWriter(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without a writer succeeded")
	}
}
