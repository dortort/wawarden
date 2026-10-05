package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
)

var testSecret = []byte(strings.Repeat("synthetic-secret-", 3))

type received struct {
	header http.Header
	body   []byte
}

type receiver struct {
	mu      sync.Mutex
	got     []received
	answers []func(http.ResponseWriter)
	srv     *httptest.Server
}

func newReceiver(t *testing.T, answers ...func(http.ResponseWriter)) *receiver {
	t.Helper()
	r := &receiver{answers: answers}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, received{header: req.Header.Clone(), body: body})
		answer := func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }
		if len(r.answers) > 0 {
			answer, r.answers = r.answers[0], r.answers[1:]
		}
		r.mu.Unlock()
		answer(w)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

type hookRig struct {
	n     *Notifier
	out   *lines
	reg   *metrics.Registry
	clock *clock
	mu    sync.Mutex
	slept []time.Duration
}

func newHookRig(t *testing.T, url string, allowPrivate bool, roots *x509.CertPool) *hookRig {
	t.Helper()
	r := &hookRig{out: &lines{}, reg: metrics.NewRegistry(), clock: &clock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}}
	w := logx.NewWriter(r.out)
	w.SetKey(make([]byte, 32))
	n, err := New(Options{Writer: w, Metrics: r.reg, URL: url, Secret: testSecret, AllowPrivate: allowPrivate, Now: r.clock.now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.hook.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	n.hook.sleep = func(ctx context.Context, d time.Duration) error {
		r.mu.Lock()
		r.slept = append(r.slept, d)
		r.mu.Unlock()
		r.clock.advance(d)
		return ctx.Err()
	}
	r.n = n
	return r
}

func roots(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

func (r *hookRig) stop(t *testing.T) {
	t.Helper()
	if err := r.n.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func (r *hookRig) dropped(t *testing.T, reason string) string {
	t.Helper()
	var b strings.Builder
	if err := r.reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	for line := range strings.Lines(b.String()) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), `wawarden_notify_dropped_total{reason="`+reason+`"} `); ok {
			return v
		}
	}
	return "0"
}

func verify(secret []byte, h http.Header, body []byte, now time.Time) bool {
	id, ts := h.Get("WaWarden-Event-Id"), h.Get("WaWarden-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || err != nil || now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	for _, got := range strings.Split(h.Get("WaWarden-Signature"), ",") {
		if hmac.Equal([]byte(strings.TrimSpace(got)), []byte(want)) {
			return true
		}
	}
	return false
}

func TestEventsArePostedSignedWithTheSameFields(t *testing.T) {
	recv := newReceiver(t)
	r := newHookRig(t, recv.srv.URL+"/hooks/wawarden?team=synthetic", true, roots(recv.srv))
	r.n.Start(t.Context())
	r.n.Quarantine("history", 3)
	r.n.AdminMutation("pair", "ok")
	r.stop(t)

	got := recv.requests()
	lines := r.out.records(t)
	if len(got) != 2 || len(lines) != 2 {
		t.Fatalf("%d posts and %d lines, want two of each", len(got), len(lines))
	}
	ids := map[string]bool{}
	for i, req := range got {
		if !verify(testSecret, req.header, req.body, r.clock.now()) {
			t.Fatalf("post %d does not verify: %v %s", i, req.header, req.body)
		}
		if req.header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type %q", req.header.Get("Content-Type"))
		}
		var body map[string]any
		if err := json.Unmarshal(req.body, &body); err != nil {
			t.Fatalf("body %s: %v", req.body, err)
		}
		if body["id"] != req.header.Get("WaWarden-Event-Id") || body["time"] != "2026-10-05T12:00:00Z" {
			t.Fatalf("body %v does not carry the event id and time", body)
		}
		ids[body["id"].(string)] = true
		for k, v := range lines[i] {
			if k != "time" && k != "level" && k != "msg" && body[k] != v {
				t.Fatalf("the posted %s = %v, the line has %v", k, body[k], v)
			}
		}
		if len(body) != len(lines[i])-3+2 {
			t.Fatalf("posted keys %v, line keys %v: the body is the line's event and fields plus id and time", body, lines[i])
		}
	}
	if len(ids) != 2 {
		t.Fatal("two events shared an id")
	}
	tampered := got[0].body
	tampered[len(tampered)-2] ^= 1
	if verify(testSecret, got[0].header, tampered, r.clock.now()) || verify([]byte("another secret"), got[1].header, got[1].body, r.clock.now()) ||
		verify(testSecret, got[1].header, got[1].body, r.clock.now().Add(6*time.Minute)) {
		t.Fatal("a tampered body, another secret or a stale timestamp verified")
	}
}

func TestRetriesKeepTheIDAndHonourRetryAfter(t *testing.T) {
	recv := newReceiver(t, status(http.StatusServiceUnavailable), func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}, func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusRequestTimeout)
	}, status(http.StatusTooEarly))
	r := newHookRig(t, recv.srv.URL, true, roots(recv.srv))
	r.n.Start(t.Context())
	r.n.Unpaired()
	r.stop(t)
	got := recv.requests()
	if len(got) != 5 {
		t.Fatalf("%d attempts, want 5: four retryable answers and a success", len(got))
	}
	for _, req := range got {
		if req.header.Get("WaWarden-Event-Id") != got[0].header.Get("WaWarden-Event-Id") {
			t.Fatal("a retry changed the event id")
		}
	}
	if got[0].header.Get("WaWarden-Timestamp") == got[4].header.Get("WaWarden-Timestamp") {
		t.Fatal("a retry kept the first timestamp")
	}
	r.mu.Lock()
	slept := append([]time.Duration(nil), r.slept...)
	r.mu.Unlock()
	if len(slept) != 4 || slept[0] < 500*time.Millisecond || slept[0] > time.Second || slept[1] != 7*time.Second || slept[2] != time.Minute ||
		slept[3] < 4*time.Second || slept[3] > 8*time.Second {
		t.Fatalf("waits %v, want a jittered second, Retry-After 7 s, Retry-After capped at a minute, then a jittered 8 s", slept)
	}
	if r.dropped(t, dropFailed) != "0" {
		t.Fatal("a delivered event was counted as dropped")
	}
}

func TestPermanentFailuresAreNotRetried(t *testing.T) {
	target := newReceiver(t)
	tests := []struct {
		name    string
		answer  func(http.ResponseWriter)
		reason  string
		status  float64
		retries bool
	}{
		{name: "client error", answer: status(http.StatusBadRequest), reason: failStatus, status: 400},
		{name: "redirect", answer: func(w http.ResponseWriter) {
			w.Header().Set("Location", target.srv.URL)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}, reason: failStatus, status: 307},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recv := newReceiver(t, tt.answer)
			r := newHookRig(t, recv.srv.URL, true, roots(recv.srv))
			r.n.Start(t.Context())
			r.n.Unpaired()
			r.stop(t)
			if n := len(recv.requests()); n != 1 {
				t.Fatalf("%d attempts, want 1", n)
			}
			failed := r.out.records(t)
			if last := failed[len(failed)-1]; last["event"] != "notify_failed" || last["reason"] != tt.reason || last["status"] != tt.status || last["attempts"] != float64(1) {
				t.Fatalf("last line %v", last)
			}
			if r.dropped(t, dropFailed) != "1" {
				t.Fatal("the failure was not counted")
			}
		})
	}
	if len(target.requests()) != 0 {
		t.Fatal("a redirect was followed")
	}
}

func TestTheDestinationIsCheckedWhenDialling(t *testing.T) {
	recv := newReceiver(t)
	r := newHookRig(t, recv.srv.URL, false, roots(recv.srv))
	r.n.Start(t.Context())
	r.n.Unpaired()
	r.stop(t)
	if n := len(recv.requests()); n != 0 {
		t.Fatalf("the receiver on loopback got %d requests without WAWARDEN_NOTIFY_ALLOW_PRIVATE", n)
	}
	recs := r.out.records(t)
	if last := recs[len(recs)-1]; last["event"] != "notify_failed" || last["reason"] != failDestination || last["attempts"] != float64(1) {
		t.Fatalf("last line %v, want one refused attempt", last)
	}
	if strings.Contains(r.out.String(), recv.srv.URL) || strings.Contains(r.out.String(), "127.0.0.1") {
		t.Fatal("the webhook's address reached the log")
	}
}

func TestNetworkErrorsAreRetried(t *testing.T) {
	recv := newReceiver(t)
	url := recv.srv.URL
	recv.srv.Close()
	r := newHookRig(t, url, true, nil)
	r.n.Start(t.Context())
	r.n.Unpaired()
	r.stop(t)
	recs := r.out.records(t)
	if last := recs[len(recs)-1]; last["reason"] != failNetwork || last["attempts"] != float64(maxAttempts) || last["status"] != float64(0) {
		t.Fatalf("last line %v, want %d attempts failing on the network", last, maxAttempts)
	}
}

func TestDestinations(t *testing.T) {
	tests := []struct {
		addr          string
		strict, loose bool
	}{
		{"169.254.169.254", false, false},
		{"169.254.170.2", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"fd00:ec2::254", false, false},
		{"fe80::1", false, false},
		{"100.100.100.200", false, false},
		{"192.0.0.192", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
		{"255.255.255.255", false, false},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.1.2.3", false, true},
		{"172.16.0.1", false, true},
		{"192.168.0.5", false, true},
		{"fd12::1", false, true},
		{"100.64.0.1", false, true},
		{"192.0.2.10", true, true},
		{"2001:db8::10", true, true},
	}
	for _, tt := range tests {
		a := netip.MustParseAddr(tt.addr)
		if got := (destination{}).check(a) == nil; got != tt.strict {
			t.Errorf("%s allowed by default: %v, want %v", tt.addr, got, tt.strict)
		}
		if got := (destination{allowPrivate: true}).check(a) == nil; got != tt.loose {
			t.Errorf("%s allowed with private destinations: %v, want %v", tt.addr, got, tt.loose)
		}
	}
	if (destination{allowPrivate: true}).control("tcp", "not an address", nil) == nil {
		t.Fatal("an unparsable address was allowed")
	}
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"https://hooks.example.test/x", "https://hooks.example.test:8443/x?team=a", "https://[::1]/x"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v", u, err)
		}
	}
	withUser := (&url.URL{Scheme: "https", User: url.UserPassword("synthetic", "synthetic"), Host: "hooks.example.test", Path: "/x"}).String()
	port := ":443"
	for _, u := range []string{"", "http://hooks.example.test/x", withUser, "https://hooks.example.test/x#f",
		"https:///x", "https:hooks.example.test", "ftp://hooks.example.test", "hooks.example.test/x", "https://" + port + "/x"} {
		if err := CheckURL(u); err == nil {
			t.Errorf("CheckURL(%q) accepted", u)
		}
	}
}

func TestNewRefusesAWebhookWithoutItsSecret(t *testing.T) {
	w := logx.NewWriter(io.Discard)
	for name, o := range map[string]Options{
		"plain http":   {Writer: w, Metrics: metrics.NewRegistry(), URL: "http://hooks.example.test/x", Secret: testSecret},
		"short secret": {Writer: w, Metrics: metrics.NewRegistry(), URL: "https://hooks.example.test/x", Secret: testSecret[:MinSecretBytes-1]},
		"no registry":  {Writer: w, URL: "https://hooks.example.test/x", Secret: testSecret},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New accepted", name)
		}
	}
}

func TestASlowReceiverBlocksNeitherEmitNorStop(t *testing.T) {
	release := make(chan struct{})
	stalled := func(w http.ResponseWriter) { <-release }
	answers := make([]func(http.ResponseWriter), queueSize+10)
	for i := range answers {
		answers[i] = stalled
	}
	recv := newReceiver(t, answers...)
	t.Cleanup(func() { close(release) })
	r := newHookRig(t, recv.srv.URL, true, roots(recv.srv))
	r.n.Start(t.Context())
	began := time.Now()
	for range queueSize + 10 {
		r.n.Unpaired()
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("emitting took %v behind a stalled receiver", took)
	}
	if got, err := strconv.Atoi(r.dropped(t, dropQueueFull)); err != nil || got < 9 {
		t.Fatalf("queue_full drops = %d, want at least the overflow of 9", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	began = time.Now()
	if err := r.n.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("Stop took %v behind a stalled receiver, want it bounded by its context", took)
	}
	full, _ := strconv.Atoi(r.dropped(t, dropQueueFull))
	shutdown, _ := strconv.Atoi(r.dropped(t, dropShutdown))
	if full+shutdown != queueSize+10 {
		t.Fatalf("drops: %d queue_full and %d shutdown, want all %d events accounted for", full, shutdown, queueSize+10)
	}
	r.n.Unpaired()
	if got, _ := strconv.Atoi(r.dropped(t, dropShutdown)); got != shutdown+1 {
		t.Fatal("an event after Stop was not counted as a shutdown drop")
	}
	if err := r.n.Stop(t.Context()); err != nil {
		t.Fatalf("a second Stop: %v", err)
	}
}

func TestStopWithAnExpiredContextStillFlushes(t *testing.T) {
	recv := newReceiver(t)
	r := newHookRig(t, recv.srv.URL, true, roots(recv.srv))
	r.n.Start(t.Context())
	r.n.AdminAuthFailure()
	r.n.AdminAuthFailure()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	began := time.Now()
	_ = r.n.Stop(ctx)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("Stop with an expired context took %v", took)
	}
	if got := authFailures(t, r.out); len(got) != 2 || got[1] != 1 {
		t.Fatalf("admin_auth_failure counts %v, want the held-back failure reported even when the grace period is over", got)
	}
}

func TestNoPostedEventCarriesACanary(t *testing.T) {
	const canary = "15550100042"
	recv := newReceiver(t)
	r := newHookRig(t, recv.srv.URL, true, roots(recv.srv))
	r.n.Start(t.Context())
	for _, v := range []string{canary + "@s.whatsapp.net", canary, "Alice " + canary} {
		r.n.Disconnected(v)
		r.n.PairRejected(v)
		r.n.LogoutFailed(1, v)
		r.n.Quarantine(v, 3)
		r.n.RekeyConflict(v)
		r.n.AdminMutation(v, v)
	}
	r.n.Unpaired()
	r.n.IngestPaused(1, 2)
	r.n.AdminAuthFailure()
	r.stop(t)
	got := recv.requests()
	if len(got) != 21 {
		t.Fatalf("%d posts, want 21", len(got))
	}
	for _, req := range got {
		if strings.Contains(string(req.body), canary) || strings.Contains(fmt.Sprint(req.header), canary) {
			t.Fatalf("a posted event carries the canary: %v %s", req.header, req.body)
		}
	}
	if strings.Contains(r.out.String(), canary) {
		t.Fatal("a line carries the canary")
	}
}
