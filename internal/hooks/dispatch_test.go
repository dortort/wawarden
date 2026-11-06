package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
)

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type fakeStore struct {
	mu       sync.Mutex
	hooks    []Hook
	progress []Progress
	fail     error
}

func (s *fakeStore) Hooks(context.Context) ([]Hook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Hook(nil), s.hooks...), nil
}

func (s *fakeStore) Advance(_ context.Context, _ string, p Progress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.progress = append(s.progress, p)
	return nil
}

func (s *fakeStore) saved() []Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Progress(nil), s.progress...)
}

type fakeClients map[string]*policy.Client

func (c fakeClients) Lookup(_ context.Context, id string) (*policy.Client, bool, error) {
	cl, ok := c[id]
	return cl, ok, nil
}

type fakeFeed struct {
	mu    sync.Mutex
	items []Item
	ok    bool
	err   error
	calls []int64
}

func (f *fakeFeed) Next(_ context.Context, c *policy.Client, after int64, limit int) (Page, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, after)
	allowed := f.ok && c.AllowWebhooks && !c.Revoked
	if f.err != nil || !allowed {
		return Page{}, allowed, f.err
	}
	var page Page
	page.Next = after
	for _, it := range f.items {
		if it.Seq > after {
			if len(page.Items) == limit {
				page.More = true
				break
			}
			page.Items = append(page.Items, it)
			page.Next = it.Seq
		}
	}
	return page, true, nil
}

type received struct {
	method, contentType, auth, webhook, id, timestamp, signature, body string
}

type receiver struct {
	mu       sync.Mutex
	got      []received
	statuses []int
	server   *httptest.Server
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{statuses: statuses}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, received{
			method: req.Method, contentType: req.Header.Get("Content-Type"), auth: req.Header.Get("Authorization"), webhook: req.Header.Get(headerWebhook),
			id: req.Header.Get(headerEventID), timestamp: req.Header.Get(headerTimestamp), signature: req.Header.Get(headerSignature), body: string(body),
		})
		status := http.StatusNoContent
		if len(r.statuses) > 0 {
			status, r.statuses = r.statuses[0], r.statuses[1:]
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

func ptr(s string) *string { return &s }

func item(seq int64, live, fromMe bool, body string) Item {
	return Item{Seq: seq, Live: live, Message: dto.Message{
		Ref: "m1_" + strconv.FormatInt(seq, 10), Chat: strings.Repeat("a", 32), FromMe: fromMe, Kind: "text", Text: ptr(body), Origin: "peer", Untrusted: true,
	}}
}

type fixture struct {
	d        *Dispatcher
	store    *fakeStore
	feed     *fakeFeed
	recv     *receiver
	reg      *metrics.Registry
	hook     Hook
	progress Progress
	clients  fakeClients
	slept    []time.Duration
}

func newFixture(t *testing.T, spec Spec, statuses ...int) *fixture {
	t.Helper()
	recv := newReceiver(t, statuses...)
	spec.URL = recv.server.URL + "/in"
	spec, err := Normalize(spec)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	f := &fixture{store: &fakeStore{}, feed: &fakeFeed{ok: true}, recv: recv, reg: metrics.NewRegistry()}
	f.hook = Hook{ID: "wh_0123456789abcdef", Client: "aaaaaaaa", Spec: spec, Secret: []byte(strings.Repeat("s", 32))}
	f.store.hooks = []Hook{f.hook}
	f.clients = fakeClients{"aaaaaaaa": {ID: "aaaaaaaa", AllowWebhooks: true, ReadAll: true, ExpiresAt: testNow.Add(time.Hour)}}
	f.d, err = New(Options{Store: f.store, Clients: f.clients, Feed: f.feed, Top: func(context.Context) (int64, error) { return 1 << 40, nil },
		Metrics: f.reg, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return testNow }, HTTP: recv.server.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.d.sleep = func(_ context.Context, d time.Duration) error {
		f.slept = append(f.slept, d)
		return nil
	}
	return f
}

func (f *fixture) pass(ctx context.Context) time.Duration { return f.d.pass(ctx, &f.hook, &f.progress) }

// drain runs passes until one asks to wait, as the worker does.
func (f *fixture) drain(ctx context.Context) time.Duration {
	for range 100 {
		if wait := f.pass(ctx); wait != 0 {
			return wait
		}
	}
	return 0
}

func TestAWebhookDeliversItsLiveMatchingMessagesInStoredOrder(t *testing.T) {
	f := newFixture(t, Spec{Method: "PUT", Headers: map[string]string{"Authorization": "Bearer synthetic"}, Filter: Filter{Direction: DirectionInbound}})
	f.hook.High = 2
	f.feed.items = []Item{
		item(1, true, false, "before the webhook"),
		item(3, false, false, "history"),
		item(4, true, true, "the owner's"),
		item(5, true, false, "first"),
		item(6, true, false, "second"),
	}
	if wait := f.drain(t.Context()); wait != idleEvery {
		t.Fatalf("pass waits %v, want %v", wait, idleEvery)
	}
	got := f.recv.requests()
	if len(got) != 2 {
		t.Fatalf("%d deliveries, want 2: %+v", len(got), got)
	}
	for i, want := range []string{"first", "second"} {
		r := got[i]
		if r.method != http.MethodPut || r.contentType != ContentJSON || r.auth != "Bearer synthetic" || r.webhook != f.hook.ID ||
			r.id != EventID(f.hook.ID, int64(5+i)) || r.timestamp != strconv.FormatInt(testNow.Unix(), 10) || !strings.Contains(r.body, `"text":"`+want+`"`) {
			t.Fatalf("delivery %d = %+v", i, r)
		}
		mac := hmac.New(sha256.New, []byte(hex.EncodeToString(f.hook.Secret)))
		mac.Write([]byte(r.id + "." + r.timestamp + "." + r.body))
		if r.signature != "v1="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("delivery %d signature %s does not verify with the secret as the client holds it", i, r.signature)
		}
	}
	if f.hook.High != 6 {
		t.Fatalf("position %d, want 6", f.hook.High)
	}
	saved := f.store.saved()
	last := saved[len(saved)-1]
	var delivered int64
	for _, p := range saved {
		delivered += p.Delivered
	}
	if last.High != 6 || delivered != 2 {
		t.Fatalf("saved %+v", saved)
	}
	if f.drain(t.Context()); len(f.recv.requests()) != 2 {
		t.Fatal("a second pass delivered again")
	}
}

func TestATemplateShapesTheBody(t *testing.T) {
	f := newFixture(t, Spec{ContentType: ContentText, Payload: "New: {{message.text}} ({{webhook}})"})
	f.feed.items = []Item{item(1, true, false, "hello")}
	f.drain(t.Context())
	got := f.recv.requests()
	if len(got) != 1 || got[0].body != "New: hello (wh_0123456789abcdef)" || got[0].contentType != "text/plain; charset=utf-8" {
		t.Fatalf("deliveries %+v", got)
	}
}

func TestARetryableAnswerIsRetriedAndAPermanentOneDropped(t *testing.T) {
	f := newFixture(t, Spec{}, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusOK, http.StatusBadRequest)
	f.feed.items = []Item{item(1, true, false, "retried"), item(2, true, false, "dropped"), item(3, true, false, "after")}
	f.drain(t.Context())
	got := f.recv.requests()
	if len(got) != 5 || got[0].id != got[2].id || got[3].id == got[2].id {
		t.Fatalf("%d requests, want 3 attempts of the first event, 1 of the second and 1 of the third: %+v", len(got), got)
	}
	if len(f.slept) != 2 {
		t.Fatalf("slept %v, want two waits", f.slept)
	}
	var delivered, failed int64
	var failure string
	for _, p := range f.store.saved() {
		delivered, failed = delivered+p.Delivered, failed+p.Failed
		if p.Failure != "" {
			failure = p.Failure
		}
	}
	if delivered != 2 || failed != 1 || failure != ReasonStatus || f.hook.High != 3 {
		t.Fatalf("delivered %d, failed %d (%s), at %d", delivered, failed, failure, f.hook.High)
	}
	if v := counter(t, f.reg, `wawarden_webhook_deliveries_total{outcome="dropped"}`); v != "1" {
		t.Fatalf("dropped counter %s", v)
	}
}

func TestRetriesStopOnceTheClientIsRevoked(t *testing.T) {
	f := newFixture(t, Spec{}, http.StatusServiceUnavailable, http.StatusOK)
	f.d.o.Clients = fakeClients{"aaaaaaaa": {ID: "aaaaaaaa", AllowWebhooks: true, ReadAll: true, Revoked: true, ExpiresAt: testNow.Add(time.Hour)}}
	f.feed.items = []Item{item(1, true, false, "x")}
	h := f.hook
	reason, delivered := f.d.deliver(t.Context(), &h, f.feed.items[0], func() bool { return f.d.holds(t.Context(), h.Client) })
	if delivered || reason != ReasonRevoked || len(f.recv.requests()) != 1 {
		t.Fatalf("deliver = %q, %v after %d attempts, want one attempt and then client_gone", reason, delivered, len(f.recv.requests()))
	}
}

func TestRetriesGiveUpAfterADay(t *testing.T) {
	f := newFixture(t, Spec{}, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway,
		http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway,
		http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway,
		http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway)
	clock := testNow
	f.d.o.Clients = fakeClients{"aaaaaaaa": {ID: "aaaaaaaa", AllowWebhooks: true, ReadAll: true, ExpiresAt: testNow.Add(90 * 24 * time.Hour)}}
	f.d.now = func() time.Time { return clock }
	f.d.sleep = func(_ context.Context, d time.Duration) error {
		clock = clock.Add(d * 60)
		return nil
	}
	f.feed.items = []Item{item(1, true, false, "never")}
	f.drain(t.Context())
	saved := f.store.saved()
	if len(saved) == 0 || saved[len(saved)-1].Failure != ReasonExpired || f.hook.High != 1 {
		t.Fatalf("saved %+v, want the event dropped after the retries ran out", saved)
	}
}

func TestADormantClientAndAFailingFeedDeliverNothing(t *testing.T) {
	f := newFixture(t, Spec{})
	f.feed.items = []Item{item(1, true, false, "x")}
	f.feed.ok = false
	if wait := f.pass(t.Context()); wait != dormantEvery || f.hook.High != 0 {
		t.Fatalf("pass for a dormant client waits %v at %d", wait, f.hook.High)
	}
	f.feed.ok, f.feed.err = true, errors.New("synthetic busy")
	if wait := f.pass(t.Context()); wait != failedReadWait || f.hook.High != 0 {
		t.Fatalf("pass with a failing feed waits %v at %d", wait, f.hook.High)
	}
	f.hook.Client = "bbbbbbbb"
	if wait := f.pass(t.Context()); wait != dormantEvery {
		t.Fatalf("pass for an unknown client waits %v", wait)
	}
	if len(f.recv.requests()) != 0 {
		t.Fatal("a dormant webhook delivered")
	}
}

func TestAnUnsavedOutcomeIsStoredBeforeAnythingMoreIsPosted(t *testing.T) {
	f := newFixture(t, Spec{})
	f.store.fail = errors.New("synthetic write failure")
	f.feed.items = []Item{item(1, true, false, "x"), item(2, true, false, "y")}
	for range 2 {
		if wait := f.pass(t.Context()); wait != failedReadWait {
			t.Fatalf("pass waits %v, want %v", wait, failedReadWait)
		}
	}
	if n, reads := len(f.recv.requests()), len(f.feed.calls); n != 1 || reads != 1 {
		t.Fatalf("%d deliveries after %d reads, want the passes to stop after the first unsaved one", n, reads)
	}
	f.store.fail = nil
	if wait := f.drain(t.Context()); wait != idleEvery || len(f.recv.requests()) != 2 {
		t.Fatalf("drain waits %v after %d deliveries", wait, len(f.recv.requests()))
	}
	saved := f.store.saved()
	if len(saved) < 2 || saved[0].Delivered != 1 || saved[0].High != 1 || !saved[0].LastDelivery.Equal(testNow) || saved[1].Delivered != 1 || saved[1].High != 2 {
		t.Fatalf("saved %+v, want the unsaved delivery stored before the next one", saved)
	}
}

func TestAnUnsavedOutcomeAtTheTopIsStoredWithoutANewMessage(t *testing.T) {
	f := newFixture(t, Spec{})
	f.store.fail = errors.New("synthetic write failure")
	f.feed.items = []Item{item(1, true, false, "x")}
	if wait := f.pass(t.Context()); wait != failedReadWait {
		t.Fatalf("pass waits %v, want %v", wait, failedReadWait)
	}
	f.d.o.Top = func(context.Context) (int64, error) { return 1, nil }
	f.d.probe(t.Context())
	f.store.fail = nil
	if wait := f.pass(t.Context()); wait != idleEvery {
		t.Fatalf("pass at the top waits %v, want %v", wait, idleEvery)
	}
	if saved := f.store.saved(); len(saved) != 1 || saved[0].Delivered != 1 || saved[0].High != 1 {
		t.Fatalf("saved %+v, want the delivery stored once the store recovers", saved)
	}
}

func TestAClientRevokedAfterAnEventPostsNothingMore(t *testing.T) {
	f := newFixture(t, Spec{})
	f.feed.items = []Item{item(1, true, false, "x"), item(2, true, false, "y"), item(3, true, false, "z")}
	if wait := f.pass(t.Context()); wait != 0 || len(f.recv.requests()) != 1 {
		t.Fatalf("first pass waits %v after %d deliveries", wait, len(f.recv.requests()))
	}
	f.clients["aaaaaaaa"].Revoked = true
	if wait := f.drain(t.Context()); wait != dormantEvery || len(f.recv.requests()) != 1 || f.hook.High != 1 {
		t.Fatalf("drain waits %v after %d deliveries at %d, want the rest of the page kept", wait, len(f.recv.requests()), f.hook.High)
	}
}

func TestAClientRevokedDuringRetriesPostsNothingMore(t *testing.T) {
	f := newFixture(t, Spec{}, http.StatusServiceUnavailable)
	f.d.sleep = func(context.Context, time.Duration) error {
		f.clients["aaaaaaaa"].Revoked = true
		return nil
	}
	f.feed.items = []Item{item(1, true, false, "x"), item(2, true, false, "y"), item(3, true, false, "z")}
	if wait := f.drain(t.Context()); wait != dormantEvery || len(f.recv.requests()) != 1 || f.hook.High != 1 {
		t.Fatalf("drain waits %v after %d requests at %d, want one attempt and the rest of the page kept", wait, len(f.recv.requests()), f.hook.High)
	}
	if saved := f.store.saved(); len(saved) != 1 || saved[0].Failure != ReasonRevoked {
		t.Fatalf("saved %+v, want the event dropped as client_gone", saved)
	}
}

func TestTheProbeSkipsAnIdleHook(t *testing.T) {
	f := newFixture(t, Spec{})
	f.d.o.Top = func(context.Context) (int64, error) { return 5, nil }
	f.d.probe(t.Context())
	f.hook.High = 5
	if wait := f.pass(t.Context()); wait != idleEvery || len(f.feed.calls) != 0 {
		t.Fatalf("pass at the top waits %v after %d reads", wait, len(f.feed.calls))
	}
}

func TestTheDispatcherRunsAndStops(t *testing.T) {
	f := newFixture(t, Spec{})
	f.feed.items = []Item{item(1, true, false, "x")}
	f.d.Start(t.Context())
	f.d.Changed()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.recv.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(f.recv.requests()) != 1 {
		t.Fatalf("%d deliveries, want 1", len(f.recv.requests()))
	}
	var nilDispatcher *Dispatcher
	nilDispatcher.Start(t.Context())
	nilDispatcher.Changed()
	if err := nilDispatcher.Stop(ctx); err != nil {
		t.Fatalf("Stop of no dispatcher: %v", err)
	}
}

func TestTheDefaultClientRefusesALoopbackDestination(t *testing.T) {
	f := newFixture(t, Spec{})
	d, err := New(Options{Store: f.store, Clients: fakeClients{"aaaaaaaa": {ID: "aaaaaaaa", AllowWebhooks: true, ReadAll: true, ExpiresAt: testNow.Add(time.Hour)}},
		Feed: f.feed, Top: func(context.Context) (int64, error) { return 1, nil }, Metrics: metrics.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.feed.items = []Item{item(1, true, false, "x")}
	h := f.hook
	var p Progress
	d.pass(t.Context(), &h, &p)
	saved := f.store.saved()
	if len(f.recv.requests()) != 0 || len(saved) == 0 || saved[len(saved)-1].Failure != ReasonDestination {
		t.Fatalf("a loopback webhook reached the receiver or was not dropped as refused: %+v", saved)
	}
}

func TestNewNeedsItsParts(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, errOptions) {
		t.Fatalf("New without parts = %v", err)
	}
}

func counter(t *testing.T, reg *metrics.Registry, series string) string {
	t.Helper()
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	for line := range strings.SplitSeq(b.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

type cancellingTransport struct{ cancel context.CancelFunc }

func (c cancellingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestADeliveryAcceptedAsTheDispatcherStopsIsStored(t *testing.T) {
	f := newFixture(t, Spec{})
	ctx, cancel := context.WithCancel(t.Context())
	f.d.http = &http.Client{Transport: cancellingTransport{cancel: cancel}}
	f.feed.items = []Item{item(1, true, false, "accepted"), item(2, true, false, "never sent")}
	f.pass(ctx)
	saved := f.store.saved()
	if len(saved) != 1 || saved[0].Delivered != 1 || saved[0].High != 1 {
		t.Fatalf("saved %+v, want the accepted delivery stored with its position and nothing after it", saved)
	}
}
