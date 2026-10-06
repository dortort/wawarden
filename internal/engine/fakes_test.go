package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/notify"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	ownerPhone = "+15550100009"
	owner      = "15550100009@s.whatsapp.net"
	ownerDev   = "15550100009:12@s.whatsapp.net"
	bobDev     = "15550100002:3@s.whatsapp.net"
	alice      = "15550100001@s.whatsapp.net"
	bob        = "15550100002@s.whatsapp.net"
	carol      = "15550100003@s.whatsapp.net"
	aliceLID   = "100000000000001@lid"
	bobLID     = "100000000000002@lid"
	group      = "120363000000000001@g.us"
	group2     = "120363000000000002@g.us"
)

var epoch = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) events(name string) []map[string]any {
	var out []map[string]any
	for line := range strings.Lines(b.String()) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == name {
			out = append(out, rec)
		}
	}
	return out
}

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func newClock() *fakeClock { return &fakeClock{now: epoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

type stoppingClock struct {
	Clock
	stop context.CancelFunc
}

func (c stoppingClock) Now() time.Time {
	c.stop()
	return c.Clock.Now()
}

type gatedClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []gatedWaiter
}

type gatedWaiter struct {
	at time.Time
	ch chan time.Time
}

func newGatedClock() *gatedClock { return &gatedClock{now: epoch} }

func (c *gatedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gatedClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, gatedWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *gatedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

func (c *gatedClock) due(within time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		if !w.at.After(c.now.Add(within)) {
			return true
		}
	}
	return false
}

func (c *gatedClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

type fakeClient struct {
	mu           sync.Mutex
	paired       bool
	account      string
	connected    bool
	version      Version
	connectErrs  []error
	logoutErrs   []error
	oneSocket    bool
	pairErr      error
	calls        []string
	handler      func(Event) bool
	blobs        map[string][]byte
	downloadErr  error
	onAck        func(HistoryRef)
	onWrite      func()
	onConnect    func()
	onPair       func()
	onDisconnect func()
	acks         []string
	called       chan string
}

func newClient(paired bool) *fakeClient {
	return &fakeClient{paired: paired, account: accountOf(ownerDev), version: Version{2, 3000, 100}, blobs: map[string][]byte{}, called: make(chan string, 1024)}
}

func (f *fakeClient) record(call string) {
	f.calls = append(f.calls, call)
	select {
	case f.called <- call:
	default:
	}
}

func (f *fakeClient) Connect(context.Context) error {
	f.mu.Lock()
	if f.oneSocket && f.connected {
		f.record("connect_while_connected")
		f.mu.Unlock()
		return errors.New("synthetic: websocket is already connected")
	}
	f.record("connect")
	var err error
	if len(f.connectErrs) > 0 {
		err, f.connectErrs = f.connectErrs[0], f.connectErrs[1:]
	}
	hook := f.onConnect
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = f.connected || err == nil
	return err
}

func (f *fakeClient) isConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	f.record("disconnect")
	f.connected = false
	hook := f.onDisconnect
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (f *fakeClient) Paired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paired
}

func (f *fakeClient) setPaired(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paired = p
}

func (f *fakeClient) Account() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.paired {
		return ""
	}
	return f.account
}

func accountOf(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	return user
}

func (f *fakeClient) pairAs(jid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paired, f.account = true, accountOf(jid)
}

func (f *fakeClient) PairPhone(_ context.Context, digits string) (string, error) {
	f.mu.Lock()
	f.record("pair:" + digits)
	hook, err := f.onPair, f.pairErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return "", err
	}
	return "SYNT-HETC", nil
}

func (f *fakeClient) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logoutErrs) > 0 {
		var err error
		err, f.logoutErrs = f.logoutErrs[0], f.logoutErrs[1:]
		f.record("logout_failed")
		return err
	}
	f.record("logout")
	f.paired, f.connected = false, false
	return nil
}

func (f *fakeClient) Version() Version {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version
}

func (f *fakeClient) SetVersion(v Version) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connected {
		f.record("set_version_while_connected")
	}
	f.record("set_version")
	f.version = v
}

func (f *fakeClient) DownloadHistory(_ context.Context, ref HistoryRef, dst io.Writer) error {
	f.mu.Lock()
	data, err, hook := f.blobs[ref.ID], f.downloadErr, f.onWrite
	f.record("download:" + ref.ID)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(data, 4096) {
		if hook != nil {
			hook()
		}
		if _, err := dst.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeClient) AckHistory(_ context.Context, ref HistoryRef) error {
	f.mu.Lock()
	hook := f.onAck
	f.record("ack:" + ref.ID)
	f.acks = append(f.acks, ref.ID)
	f.mu.Unlock()
	if hook != nil {
		hook(ref)
	}
	return nil
}

func (f *fakeClient) OnEvent(handler func(Event) bool) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.handler = nil
	}
}

func (f *fakeClient) emit(ev Event) bool {
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return false
	}
	return handler(ev)
}

func (f *fakeClient) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeClient) count(call string) int {
	n := 0
	for _, c := range f.history() {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fakeClient) waitFor(t *testing.T, call string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case c := <-f.called:
			if c == call {
				return
			}
		case <-timeout:
			t.Fatalf("no %s call; calls %v", call, f.history())
		}
	}
}

type versionResult struct {
	v   Version
	err error
}

type fakeVersions struct {
	mu      sync.Mutex
	results []versionResult
	calls   int
}

func (v *fakeVersions) Latest(context.Context) (Version, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if len(v.results) == 0 {
		return Version{}, errors.New("synthetic: no version")
	}
	r := v.results[0]
	if len(v.results) > 1 {
		v.results = v.results[1:]
	}
	return r.v, r.err
}

func (v *fakeVersions) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

type rig struct {
	t      *testing.T
	client *fakeClient
	clock  *fakeClock
	reg    *metrics.Registry
	logs   *syncBuffer
	opts   Options
}

type option func(*Options)

func withOwner(phone string) option { return func(o *Options) { o.OwnerPhone = phone } }

func newRig(t *testing.T, options ...option) *rig {
	t.Helper()
	logs := &syncBuffer{}
	w := logx.NewWriter(logs)
	w.SetKey(make([]byte, 32))
	r := &rig{t: t, client: newClient(true), clock: newClock(), reg: metrics.NewRegistry(), logs: logs}
	notifier, err := notify.New(notify.Options{Writer: w})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	r.opts = Options{
		Client: r.client, Versions: &fakeVersions{}, DataDir: t.TempDir(), OwnerPhone: ownerPhone, HistoryMaxBytes: DefaultHistoryMaxBytes,
		Logger: logx.New(w, slog.LevelDebug), Notify: notifier, Metrics: r.reg, Clock: r.clock,
		Jitter: func(time.Duration) time.Duration { return 0 },
	}
	for _, o := range options {
		o(&r.opts)
	}
	return r
}

func (r *rig) counter(name string, labels ...string) float64 {
	r.t.Helper()
	var buf bytes.Buffer
	if err := r.reg.WriteText(&buf); err != nil {
		r.t.Fatalf("WriteText: %v", err)
	}
	want := name
	if len(labels) == 2 {
		want = name + `{` + labels[0] + `="` + labels[1] + `"}`
	}
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), " ")
		if ok && key == want {
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				r.t.Fatalf("metric %s: %v", key, err)
			}
			return f
		}
	}
	return 0
}

func (r *rig) alerts(event string) []map[string]any { return r.logs.events(event) }

func (r *rig) reportPanics() {
	r.t.Helper()
	safego.Install(r.opts.Logger, r.reg)
	r.t.Cleanup(func() { safego.Install(logx.New(logx.NewWriter(io.Discard), slog.LevelError), metrics.NewRegistry()) })
}

func crashes(t *testing.T, fn func()) {
	t.Helper()
	finished := make(chan bool)
	go func() {
		returned := false
		defer func() { finished <- returned }()
		fn()
		returned = true
	}()
	if <-finished {
		t.Fatal("the worker did not reach the crash")
	}
}
