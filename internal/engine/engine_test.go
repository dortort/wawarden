package engine

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/store/ingest"
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

func TestEnginePausesAndResumesWithTheFreeSpace(t *testing.T) {
	r := newHistRig(t)
	r.ingestOpts.MinFreeBytes = math.MaxUint64
	r.reopen()
	r.appendRaw(mustPayload(t, dm("M1", alice, "waits in the inbox")))
	clock := newGatedClock()
	o := r.opts
	o.Clock, o.Metrics = clock, metrics.NewRegistry()
	r.reg = o.Metrics
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var freed, resumed atomic.Bool
	e.pipe.space = func() (ingest.Space, error) {
		if freed.Load() {
			return ingest.Space{Free: 1 << 40, Floor: 1 << 30, Changed: !resumed.Swap(true)}, nil
		}
		return r.archive.CheckSpace()
	}
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	if a := r.alerts("ingest_paused"); len(a) != 1 || a[0]["floor_bytes"] != float64(math.MaxUint64) {
		t.Fatalf("ingest_paused alerts %v", a)
	}
	eventually(t, "the worker waits for its next check", func() bool { return clock.waiting() == 1 })
	if _, ok := r.find(alice, "M1", alice); ok {
		t.Fatal("the engine applied the inbox while ingest is paused")
	}
	if r.client.emit(dm("M2", alice, "while paused")) || r.counter("wawarden_ingest_refused_total", "reason", "paused") != 1 {
		t.Fatal("the engine acknowledged a message while ingest is paused")
	}
	freed.Store(true)
	clock.advance(spaceInterval)
	eventually(t, "the inbox drains after the free space returns", func() bool {
		_, ok := r.find(alice, "M1", alice)
		return ok
	})
	if len(r.logs.events("ingest_resumed")) != 1 {
		t.Fatalf("no ingest_resumed event: %s", r.logs)
	}
	if !r.client.emit(dm("M2", alice, "after the pause")) {
		t.Fatal("the engine refused a message after ingest resumed")
	}
}

type clockedDecoder struct {
	inner HistoryDecoder
	clock *gatedClock
}

func (d clockedDecoder) Decode(blob []byte) (History, error) {
	d.clock.mu.Lock()
	d.clock.now = d.clock.now.Add(31 * time.Second)
	d.clock.mu.Unlock()
	return d.inner.Decode(blob)
}

func TestHistorySyncChecksTheFreeSpaceBetweenBatches(t *testing.T) {
	r := newHistRig(t)
	for i := 1; i <= 3; i++ {
		id := strconv.Itoa(i)
		r.notify(HistoryRef{ID: "HB" + id, Inline: r.blob("synthetic blob "+id, History{Conversations: []Conversation{{Chat: alice, Messages: []Message{
			{ID: "S" + id, Sender: alice, Timestamp: epoch, Kind: KindText, Text: "history " + id},
		}}}})})
	}
	clock := newGatedClock()
	o := r.opts
	o.Decoder, o.Clock, o.Metrics = clockedDecoder{inner: r.decoder, clock: clock}, clock, metrics.NewRegistry()
	r.reg = o.Metrics
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var freed atomic.Bool
	e.pipe.space = lowAfter(clock, epoch.Add(45*time.Second), freed.Load)
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	eventually(t, "ingest pauses in the middle of the history sync", func() bool { return len(r.alerts("ingest_paused")) == 1 })
	eventually(t, "the worker waits for its next check", func() bool { return clock.waiting() == 1 })
	r.must(alice, "S1", alice)
	for _, id := range []string{"S2", "S3"} {
		if _, ok := r.find(alice, id, alice); ok {
			t.Fatalf("%s was applied after ingest paused", id)
		}
	}
	var pending []ingest.Blob
	if err := r.archive.Read(t.Context(), "test.pending", func(rd *ingest.Reader) error {
		var err error
		pending, err = rd.PendingBlobs(pendingWindow)
		return err
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(pending) != 2 || pending[0].ID != "HB2" || pending[0].Attempts != 0 || pending[1].Attempts != 0 {
		t.Fatalf("pending blobs %+v, want HB2 and HB3 without an attempt", pending)
	}
	if r.client.emit(dm("M1", alice, "while paused")) {
		t.Fatal("a message was acknowledged while ingest is paused")
	}
	freed.Store(true)
	clock.advance(spaceInterval)
	eventually(t, "the history sync resumes", func() bool {
		_, two := r.find(alice, "S2", alice)
		_, three := r.find(alice, "S3", alice)
		return two && three
	})
	if len(r.logs.events("ingest_resumed")) != 1 {
		t.Fatalf("no ingest_resumed event: %s", r.logs)
	}
	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT group_concat(attempts || (processed_at IS NOT NULL), ',') FROM history_blobs"); got != "11,11,11" {
		t.Fatalf("blob rows %q, want each processed after one attempt", got)
	}
}

func TestContentIsDroppedWhileNoDeviceIsPaired(t *testing.T) {
	r := newHistRig(t)
	r.client.setPaired(false)
	e := newEngine(t, r)
	e.Start(t.Context(), 1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	eventually(t, "the engine waits for pairing", func() bool { return e.Status().State == StateUnpaired })
	if _, err := e.Pair(t.Context()); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	r.client.pairAs(bobDev)
	r.client.emit(Paired{JID: bobDev})
	for _, ev := range []Event{
		dm("M1", bob, "from the rejected account"),
		Group{Chat: group, Subject: "Rejected Account Group", Timestamp: epoch},
		HistoryNotification{Sender: bob, FromMe: true, Ref: HistoryRef{ID: "HS1", Inline: []byte("synthetic")}},
	} {
		if !r.client.emit(ev) {
			t.Fatalf("%T from the rejected account was refused instead of dropped", ev)
		}
	}
	if got := r.counter("wawarden_ingest_dropped_total", "reason", "not_paired"); got != 3 {
		t.Fatalf("not_paired drops %v, want 3", got)
	}
	r.client.waitFor(t, "logout")
	eventually(t, "the rejected device is logged out", func() bool {
		e.sup.mu.Lock()
		defer e.sup.mu.Unlock()
		return !e.sup.rejected && !e.sup.loggingOut
	})
	r.client.emit(LoggedOut{})
	if !r.client.emit(dm("M3", bob, "in flight from the rejected account")) {
		t.Fatal("a message after the LoggedOut was refused instead of dropped")
	}
	if st := e.Status(); st != (Status{State: StateDisconnected, Reason: ReasonLoggedOut}) {
		t.Fatalf("status %+v", st)
	}
	if got := r.counter("wawarden_ingest_dropped_total", "reason", "not_paired"); got != 4 {
		t.Fatalf("not_paired drops after the LoggedOut %v, want 4", got)
	}
	if _, err := e.Pair(t.Context()); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	r.client.pairAs(ownerDev)
	r.client.emit(Paired{JID: ownerDev})
	if !r.client.emit(dm("M2", alice, "after the owner paired")) {
		t.Fatal("a message after the owner paired was refused")
	}
	eventually(t, "the owner's traffic is stored", func() bool {
		_, ok := r.find(alice, "M2", alice)
		return ok
	})
	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT (SELECT count(*) FROM inbox) || ' ' || (SELECT count(*) FROM history_blobs) || ' ' || (SELECT group_concat(id) FROM messages) || ' ' || (SELECT count(*) FROM chats WHERE jid != ?)", alice); got != "0 0 M2 0" {
		t.Fatalf("inbox rows, blobs, messages and other chats = %q, want only the owner's message", got)
	}
}

func TestARejectedAccountIsNeitherConnectedNorStoredWhenItsLogoutFails(t *testing.T) {
	r := newHistRig(t)
	r.client.setPaired(false)
	r.client.failLogouts(1000)
	clock := newGatedClock()
	startEngine := func(recent int) *Engine {
		o := r.opts
		o.Clock, o.Metrics = clock, metrics.NewRegistry()
		o.Versions = &fakeVersions{results: []versionResult{{v: current}}}
		r.reg = o.Metrics
		e, err := New(o)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		e.Start(t.Context(), recent)
		t.Cleanup(func() { _ = e.Stop(context.Background()) })
		return e
	}
	content := []Event{
		dm("W1", bob, "from the rejected account"),
		Group{Chat: group, Subject: "Rejected Account Group", Timestamp: epoch},
		HistoryNotification{Sender: bob, FromMe: true, Ref: HistoryRef{ID: "HS1", Inline: []byte("synthetic")}},
	}
	rejectedEverywhere := func(e *Engine, logouts int) {
		t.Helper()
		eventually(t, "the logout failed", func() bool { return r.client.count("logout_failed") == logouts })
		if err := e.Reconnect(); !errors.Is(err, ErrNotPaired) {
			t.Fatalf("Reconnect = %v, want ErrNotPaired", err)
		}
		r.client.emit(Connected{})
		for _, ev := range content {
			if !r.client.emit(ev) {
				t.Fatalf("%T from the rejected account was refused instead of dropped", ev)
			}
		}
		if got := r.counter("wawarden_ingest_dropped_total", "reason", "not_paired"); got != float64(len(content)) {
			t.Fatalf("not_paired drops %v", got)
		}
		if st := e.Status(); st != (Status{State: StateUnpaired, Paired: true}) {
			t.Fatalf("status %+v", st)
		}
	}

	e := startEngine(1)
	eventually(t, "the engine waits for pairing", func() bool { return e.Status().State == StateUnpaired })
	if _, err := e.Pair(t.Context()); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	r.client.pairAs(bobDev)
	r.client.emit(Paired{JID: bobDev})
	rejectedEverywhere(e, 1)
	r.client.emit(LoggedOut{})
	for _, ev := range content {
		r.client.emit(ev)
	}
	if got := r.counter("wawarden_ingest_dropped_total", "reason", "not_paired"); got != float64(2*len(content)) {
		t.Fatalf("not_paired drops after a LoggedOut %v", got)
	}
	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	e = startEngine(2)
	mismatch := Status{State: StateDisconnected, Reason: ReasonOwnerMismatch, Paired: true}
	eventually(t, "the engine refuses the stored device", func() bool { return e.Status() == mismatch })
	if err := e.Reconnect(); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("Reconnect = %v, want ErrOwnerMismatch", err)
	}
	r.client.emit(Connected{})
	for _, ev := range content {
		if !r.client.emit(ev) {
			t.Fatalf("%T from the stored device of another account was refused instead of dropped", ev)
		}
	}
	if got := r.counter("wawarden_ingest_dropped_total", "reason", "not_paired"); got != float64(len(content)) {
		t.Fatalf("not_paired drops %v", got)
	}
	if st := e.Status(); st != mismatch {
		t.Fatalf("status %+v", st)
	}
	if a := r.alerts("pair_rejected"); len(a) != 1 || a[0]["stage"] != "after_pairing" {
		t.Fatalf("pair_rejected alerts %v", a)
	}
	if a := r.alerts("disconnected"); len(a) != 2 || a[0]["reason"] != "logged_out" || a[1]["reason"] != "owner_mismatch" {
		t.Fatalf("disconnected alerts %v", a)
	}
	if r.client.count("connect") != 1 || r.client.count("logout_failed") != 1 {
		t.Fatalf("calls %v: want only the pairing's connect and the first start's failed logout", r.client.history())
	}
	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT (SELECT count(*) FROM inbox) || ' ' || (SELECT count(*) FROM messages) || ' ' || (SELECT count(*) FROM history_blobs) || ' ' || (SELECT count(*) FROM chats)"); got != "0 0 0 0" {
		t.Fatalf("inbox rows, messages, blobs and chats = %q, want none", got)
	}
}

type panickingDecoder struct{}

func (panickingDecoder) Decode([]byte) (History, error) {
	panic("synthetic canary decoder panic 15550100001@s.whatsapp.net")
}

func TestADecoderPanicIsAFailedAttemptAndIngestGoesOn(t *testing.T) {
	r := newHistRig(t)
	clock := newGatedClock()
	o := r.opts
	o.Decoder, o.Clock, o.Metrics = panickingDecoder{}, clock, metrics.NewRegistry()
	r.reg = o.Metrics
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.reportPanics()
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	if !r.client.emit(HistoryNotification{Sender: owner, FromMe: true, Ref: HistoryRef{ID: "HS1", Inline: deflate(t, []byte("synthetic blob"))}}) {
		t.Fatal("the history notification was not acknowledged")
	}
	eventually(t, "the first attempt waits for its retry", func() bool { return clock.due(time.Second) })
	if !r.client.emit(dm("M1", alice, "live after the panic")) {
		t.Fatal("a live message was not acknowledged")
	}
	clock.advance(time.Second)
	eventually(t, "the live message is applied", func() bool {
		_, ok := r.find(alice, "M1", alice)
		return ok
	})
	eventually(t, "the second attempt waits for its retry", func() bool { return clock.due(2 * time.Second) })
	clock.advance(2 * time.Second)
	eventually(t, "the blob is quarantined", func() bool { return len(r.alerts("quarantine")) == 1 })
	if err := e.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	attempts := 0
	for _, f := range r.logs.events("ingest_failed") {
		if f["queue"] == "history" && f["attempt"] != nil {
			attempts++
		}
	}
	if a := r.alerts("quarantine"); a[0]["queue"] != "history" || a[0]["attempts"] != float64(3) || attempts != 3 {
		t.Fatalf("quarantine alerts %v, failed attempts %d", a, attempts)
	}
	if r.counter("wawarden_panics_total", "name", "engine.ingest") != 3 {
		t.Fatalf("panics counted %v, want 3", r.counter("wawarden_panics_total", "name", "engine.ingest"))
	}
	if strings.Contains(r.logs.String(), "canary") {
		t.Fatalf("the panic value reached the log: %s", r.logs)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT attempts || ' ' || quarantined FROM history_blobs"); got != "3 1" {
		t.Fatalf("blob row %q", got)
	}
}

func TestEngineStartDeletesHistoryLeftByACrash(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	r.downloaded("HS1")
	r.downloaded("HS2")
	r.write(func(tx *ingest.Tx) error {
		if err := tx.MarkBlobProcessed("HS1", epoch); err != nil {
			return err
		}
		return tx.QuarantineBlob("HS2")
	})
	e := newEngine(t, r)
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	eventually(t, "the start deletes the plaintext of settled blobs", func() bool { return len(r.historyFiles()) == 0 })
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
