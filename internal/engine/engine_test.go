package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newEngine(t *testing.T, r *histRig) *Engine {
	t.Helper()
	o := r.opts
	o.Clock, o.Jitter = nil, nil
	o.Metrics = metrics.NewRegistry()
	r.reg = o.Metrics
	o.Versions = &fakeVersions{results: []versionResult{{v: current}}}
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestEngineRunsTheSupervisorAndTheWorkers(t *testing.T) {
	r := newHistRig(t)
	e := newEngine(t, r)
	e.Start(t.Context(), 1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	r.client.waitFor(t, "connect")
	r.client.emit(Connected{})
	eventually(t, "the engine is connected", func() bool { return e.Status() == Status{State: StateConnected, Paired: true} })

	compressed := r.blob("synthetic end to end", History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "H1", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "history"}}}}})
	r.client.mu.Lock()
	r.client.blobs["HS1"] = compressed
	r.client.mu.Unlock()
	for _, ev := range []Event{
		dm("M1", alice, "live"),
		text("status@broadcast", "S1", alice, "dropped"),
		HistoryNotification{Sender: owner, FromMe: true, Ref: HistoryRef{ID: "HS1"}},
		Group{Chat: group, Subject: "Synthetic Group", Timestamp: epoch},
	} {
		if !r.client.emit(ev) {
			t.Fatalf("%T was not acknowledged", ev)
		}
	}
	eventually(t, "the live message and the history are stored", func() bool {
		_, live := r.find(alice, "M1", alice)
		_, old := r.find(alice, "H1", alice)
		return live && old
	})
	if r.counter("wawarden_connected") != 1 || r.counter("wawarden_paired") != 1 {
		t.Fatal("the gauges do not show a paired, connected engine")
	}
	if err := e.Reconnect(); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("Reconnect = %v", err)
	}
	if _, err := e.Pair(t.Context()); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("Pair = %v", err)
	}

	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := e.Status(); st.State != StateDisconnected || st.Reason != ReasonShutdown {
		t.Fatalf("status after Stop %+v", st)
	}
	if r.client.emit(dm("M2", alice, "after stop")) {
		t.Fatal("the engine still handles events after Stop")
	}
	if r.counter("wawarden_connected") != 0 {
		t.Fatal("the Connected gauge is not 0 after Stop")
	}
}

func TestEngineSweepsExpiredMessages(t *testing.T) {
	r := newHistRig(t)
	m := dm("M1", alice, "expired already")
	m.Timestamp = time.Now().Add(-time.Hour)
	m.Expiration = time.Minute
	r.ingest(m)
	e := newEngine(t, r)
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	eventually(t, "the expired message is purged", func() bool {
		f, _ := r.find(alice, "M1", alice)
		return f.Text == ""
	})
	if st := e.Status(); st.Reason != ReasonRestartBudget || r.client.count("connect") != 0 {
		t.Fatalf("status %+v calls %v", st, r.client.history())
	}
}

func TestNewRefusesIncompleteOptions(t *testing.T) {
	r := newHistRig(t)
	for name, change := range map[string]func(*Options){
		"no client":        func(o *Options) { o.Client = nil },
		"no versions":      func(o *Options) { o.Versions = nil },
		"no decoder":       func(o *Options) { o.Decoder = nil },
		"no archive":       func(o *Options) { o.Archive = nil },
		"no data dir":      func(o *Options) { o.DataDir = "" },
		"no logger":        func(o *Options) { o.Logger = nil },
		"no alerts":        func(o *Options) { o.Alerts = nil },
		"no metrics":       func(o *Options) { o.Metrics = nil },
		"no history cap":   func(o *Options) { o.HistoryMaxBytes = 0 },
		"history cap over": func(o *Options) { o.HistoryMaxBytes = MaxHistoryMaxBytes + 1 },
	} {
		o := r.opts
		change(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: New accepted", name)
		}
	}
	o := r.opts
	o.Metrics = metrics.NewRegistry()
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}
