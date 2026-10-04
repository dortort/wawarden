package app

import (
	"bytes"
	"context"
	"encoding/json"
	_ "expvar"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // registers the profiling routes on the default mux so the tests can prove no listener serves them
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

const syntheticClientToken = "synthetic-client-token-for-tests"

type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written != nil {
		close(b.written)
		b.written = nil
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written == nil {
		b.written = make(chan struct{})
	}
	return b.written
}

func (b *syncBuffer) waitFor(t *testing.T, event string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		changed := b.changed()
		if len(b.find(event)) > 0 {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("no %s event in %v", event, b.events())
		}
	}
}

func (b *syncBuffer) events() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

func (b *syncBuffer) find(event string) []map[string]any {
	var out []map[string]any
	for _, rec := range b.events() {
		if rec["event"] == event {
			out = append(out, rec)
		}
	}
	return out
}

type oneClient struct{}

func (oneClient) Authenticate(_ context.Context, presented string) (*policy.Client, bool) {
	if presented != syntheticClientToken {
		return nil, false
	}
	return &policy.Client{ID: "synthetic-client", ExpiresAt: time.Now().Add(time.Hour)}, true
}

type heldClient struct {
	entered func()
	release <-chan struct{}
}

func (h heldClient) Authenticate(context.Context, string) (*policy.Client, bool) {
	h.entered()
	<-h.release
	return nil, false
}

func testConfig(t *testing.T, adminToken string) config.Config {
	t.Helper()
	loopback := netip.MustParseAddrPort("127.0.0.1:0")
	cfg := config.Config{DataDir: t.TempDir(), Listen: loopback, AdminListen: loopback, HealthListen: loopback}
	if adminToken != "" {
		cred, err := policy.ParseAdminCredential(token.Hash(adminToken))
		if err != nil {
			t.Fatalf("ParseAdminCredential: %v", err)
		}
		cfg.AdminCredential = &cred
	}
	return cfg
}

func open(t *testing.T, cfg config.Config, auth api.Authenticator) (*App, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	a, err := newApp(t.Context(), cfg, NewLogger(logs, nil), auth)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	return a, logs
}

func run(t *testing.T, a *App) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	stop := sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	t.Cleanup(func() { _ = stop() })
	return stop
}

func start(t *testing.T, cfg config.Config, auth api.Authenticator) (*App, *syncBuffer, func() error) {
	t.Helper()
	a, logs := open(t, cfg, auth)
	return a, logs, run(t, a)
}

func addr(t *testing.T, a *App, name string) netip.AddrPort {
	t.Helper()
	for _, b := range a.Inventory() {
		if b.Name == name {
			return b.Addr
		}
	}
	t.Fatalf("no %s listener in %+v", name, a.Inventory())
	return netip.AddrPort{}
}

type reply struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, method string, to netip.AddrPort, path string, header http.Header) (reply, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+to.String()+path, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return reply{}, err
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s %s on %v carries %s", method, path, to, k)
		}
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}, nil
}

func mustDo(t *testing.T, method string, to netip.AddrPort, path string, header http.Header) reply {
	t.Helper()
	r, err := do(t, method, to, path, header)
	if err != nil {
		t.Fatalf("%s %s on %v: %v", method, path, to, err)
	}
	return r
}

func bearer(credential string) http.Header {
	return http.Header{"Authorization": {"Bearer " + credential}}
}

func TestListenersPerConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		adminToken string
		want       []string
	}{
		{name: "without an admin hash", want: []string{"client", "health"}},
		{name: "with an admin hash", adminToken: token.NewAdmin(), want: []string{"client", "admin", "health"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, logs, stop := start(t, testConfig(t, tt.adminToken), noClients{})
			var names []string
			for _, b := range a.Inventory() {
				names = append(names, b.Name)
				if !b.Addr.Addr().IsLoopback() || b.Addr.Port() == 0 {
					t.Fatalf("%s bound to %v, want a loopback address with a real port", b.Name, b.Addr)
				}
			}
			if !slices.Equal(names, tt.want) {
				t.Fatalf("listeners = %v, want exactly %v", names, tt.want)
			}
			if got := len(logs.find("listening")); got != len(tt.want) {
				t.Fatalf("%d listening events, want %d", got, len(tt.want))
			}
			if err := stop(); err != nil {
				t.Fatalf("Run = %v, want a clean shutdown", err)
			}
		})
	}
}

func TestHealthTurnsUnavailableWhenShutdownBegins(t *testing.T) {
	a, logs := open(t, testConfig(t, ""), noClients{})
	health, client := addr(t, a, "health"), addr(t, a, "client")
	var duringDrain reply
	var duringDrainErr, clientDuringDrain error
	a.afterDrain = func() {
		duringDrain, duringDrainErr = do(t, http.MethodGet, health, "/healthz", nil)
		_, clientDuringDrain = do(t, http.MethodGet, client, "/", nil)
	}
	stop := run(t, a)

	if r := mustDo(t, http.MethodGet, health, "/healthz", nil); r.status != http.StatusOK || r.body != `{"status":"ok"}` {
		t.Fatalf("/healthz while running = %d %q, want 200 ok", r.status, r.body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if duringDrainErr != nil || duringDrain.status != http.StatusServiceUnavailable || duringDrain.body != `{"status":"unavailable"}` {
		t.Fatalf("/healthz after shutdown began = %d %q %v, want 503 unavailable", duringDrain.status, duringDrain.body, duringDrainErr)
	}
	if clientDuringDrain == nil {
		t.Fatal("the client listener still answered after it was drained")
	}
	if _, err := do(t, http.MethodGet, health, "/healthz", nil); err == nil {
		t.Fatal("the health listener still answers after Run returned")
	}
	want := []string{"starting", "listening", "listening", "ready", "shutdown_started", "stopped"}
	if buildinfo.Dev {
		want = slices.Insert(want, 1, "dev_build")
	}
	var got []string
	for _, rec := range logs.events() {
		event, _ := rec["event"].(string)
		got = append(got, event)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestShutdownWithARequestInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseHeld := sync.OnceFunc(func() { close(release) })
	a, logs := open(t, testConfig(t, ""), heldClient{entered: sync.OnceFunc(func() { close(entered) }), release: release})
	health, client := addr(t, a, "health"), addr(t, a, "client")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	wait := sync.OnceValue(func() error { return <-done })
	t.Cleanup(func() {
		releaseHeld()
		cancel()
		_ = wait()
	})

	type result struct {
		reply
		err error
	}
	inflight := make(chan result, 1)
	go func() {
		r, err := do(t, http.MethodGet, client, "/v1/held", bearer(syntheticClientToken))
		inflight <- result{r, err}
	}()
	select {
	case <-entered:
	case r := <-inflight:
		t.Fatalf("the request never reached the authenticator: %d %q %v", r.status, r.body, r.err)
	}

	cancel()
	logs.waitFor(t, "shutdown_started")
	if r := mustDo(t, http.MethodGet, health, "/healthz", nil); r.status != http.StatusServiceUnavailable || r.body != `{"status":"unavailable"}` {
		t.Fatalf("/healthz while a request is in flight during shutdown = %d %q, want 503 unavailable", r.status, r.body)
	}
	select {
	case r := <-inflight:
		t.Fatalf("the in-flight request ended before it was released: %d %q %v", r.status, r.body, r.err)
	default:
	}
	if stopped := logs.find("stopped"); len(stopped) != 0 {
		t.Fatalf("Run stopped while a request was in flight: %v", stopped)
	}

	releaseHeld()
	if r := <-inflight; r.err != nil || r.status != http.StatusUnauthorized || r.body != `{"error":"unauthorized"}` {
		t.Fatalf("the in-flight request after cancellation = %d %q %v, want it to complete with 401", r.status, r.body, r.err)
	}
	if err := wait(); err != nil {
		t.Fatalf("Run = %v, want a clean shutdown once the in-flight request completed", err)
	}
}

func TestClientListenerRequiresAuthentication(t *testing.T) {
	a, _, _ := start(t, testConfig(t, ""), noClients{})
	client := addr(t, a, "client")
	for _, tt := range []struct {
		method, path string
		header       http.Header
	}{
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/v1/me", header: bearer(syntheticClientToken)},
		{method: http.MethodPost, path: "/v1/chats/x/messages", header: bearer(token.NewAdmin())},
	} {
		r := mustDo(t, tt.method, client, tt.path, tt.header)
		if r.status != http.StatusUnauthorized || r.body != `{"error":"unauthorized"}` || r.header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s %s = %d %q, want 401 with WWW-Authenticate: Bearer", tt.method, tt.path, r.status, r.body)
		}
	}
}

func TestAdminServesMetricsOnlyWithItsToken(t *testing.T) {
	adminToken := token.NewAdmin()
	a, _, _ := start(t, testConfig(t, adminToken), noClients{})
	admin := addr(t, a, "admin")

	for _, header := range []http.Header{nil, bearer(token.NewAdmin()), bearer(token.Hash(adminToken)), {"Authorization": {adminToken}}} {
		if r := mustDo(t, http.MethodGet, admin, "/metrics", header); r.status != http.StatusUnauthorized || strings.Contains(r.body, "wawarden_") {
			t.Fatalf("/metrics with %v = %d %q, want 401", header, r.status, r.body)
		}
	}
	for _, other := range []string{"client", "health"} {
		r := mustDo(t, http.MethodGet, addr(t, a, other), "/metrics", bearer(adminToken))
		if r.status == http.StatusOK || strings.Contains(r.body, "wawarden_") {
			t.Fatalf("the %s listener served /metrics: %d %q", other, r.status, r.body)
		}
	}

	r := mustDo(t, http.MethodGet, admin, "/metrics", bearer(adminToken))
	if r.status != http.StatusOK || r.header.Get("Content-Type") != metrics.ContentType {
		t.Fatalf("/metrics with the admin token = %d %q (%s)", r.status, r.body, r.header.Get("Content-Type"))
	}
	for _, want := range []string{
		"wawarden_admin_auth_failures_total 4\n",
		"wawarden_auth_failures_total 1\n",
		`wawarden_build_info{version="dev",revision="`,
		`",dev="` + strconv.FormatBool(buildinfo.Dev) + `"} 1` + "\n",
	} {
		if !strings.Contains(r.body, want) {
			t.Fatalf("/metrics lacks %q:\n%s", want, r.body)
		}
	}
}

func TestDebugEndpointsAreNotServed(t *testing.T) {
	for _, path := range []string{"/debug/pprof/", "/debug/vars"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		if _, pattern := http.DefaultServeMux.Handler(req); pattern == "" {
			t.Fatalf("the default mux does not serve %s, so this test proves nothing", path)
		}
	}
	adminToken := token.NewAdmin()
	a, _, _ := start(t, testConfig(t, adminToken), oneClient{})
	credentials := map[string]http.Header{"client": bearer(syntheticClientToken), "admin": bearer(adminToken), "health": nil}
	for _, b := range a.Inventory() {
		for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/heap", "/debug/vars"} {
			r := mustDo(t, http.MethodGet, b.Addr, path, credentials[b.Name])
			if r.status != http.StatusNotFound || r.body != `{"error":"not_found"}` {
				t.Fatalf("%s on the %s listener = %d %q, want the uniform 404", path, b.Name, r.status, r.body)
			}
		}
	}
	if r := mustDo(t, http.MethodGet, addr(t, a, "client"), "/debug/pprof/", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("/debug/pprof/ without a token = %d, want 401", r.status)
	}
}

func TestNoListenerSendsAccessControlHeaders(t *testing.T) {
	adminToken := token.NewAdmin()
	a, _, _ := start(t, testConfig(t, adminToken), oneClient{})
	credentials := map[string]http.Header{"client": bearer(syntheticClientToken), "admin": bearer(adminToken), "health": nil}
	for _, b := range a.Inventory() {
		for _, extra := range []http.Header{
			nil,
			{"Origin": {"https://attacker.example"}},
			{"Sec-Fetch-Site": {"cross-site"}},
			{"Access-Control-Request-Method": {"POST"}, "Origin": {"https://attacker.example"}},
		} {
			for _, method := range []string{http.MethodGet, http.MethodOptions, http.MethodPost} {
				header := credentials[b.Name].Clone()
				if header == nil {
					header = http.Header{}
				}
				for k, v := range extra {
					header[k] = v
				}
				for _, path := range []string{"/", "/healthz", "/metrics"} {
					mustDo(t, method, b.Addr, path, header)
				}
			}
		}
	}
}

func TestNonLoopbackListenerWarns(t *testing.T) {
	tests := []struct {
		name                string
		listen, adminListen string
		warned              []string
	}{
		{name: "client on every ipv4 address", listen: "0.0.0.0:0", adminListen: "127.0.0.1:0", warned: []string{"client"}},
		{name: "admin on every ipv4 address", listen: "127.0.0.1:0", adminListen: "0.0.0.0:0", warned: []string{"admin"}},
		{name: "loopback", listen: "127.0.0.1:0", adminListen: "127.0.0.1:0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, token.NewAdmin())
			cfg.Listen = netip.MustParseAddrPort(tt.listen)
			cfg.AdminListen = netip.MustParseAddrPort(tt.adminListen)
			_, logs, stop := start(t, cfg, noClients{})
			if err := stop(); err != nil {
				t.Fatalf("Run = %v", err)
			}
			var warned []string
			for _, rec := range logs.find("listener_not_loopback") {
				name, _ := rec["listener"].(string)
				if rec["level"] != "WARN" {
					t.Fatalf("the %s warning was logged at %v, want WARN", name, rec["level"])
				}
				warned = append(warned, name)
			}
			if !slices.Equal(warned, tt.warned) {
				t.Fatalf("non-loopback warnings for %v, want exactly %v", warned, tt.warned)
			}
		})
	}
}

func TestDevBuildWarns(t *testing.T) {
	_, logs, stop := start(t, testConfig(t, ""), noClients{})
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	starting := logs.find("starting")
	if len(starting) != 1 {
		t.Fatalf("starting events = %v", starting)
	}
	dev, _ := starting[0]["dev"].(bool)
	if warned := len(logs.find("dev_build")) == 1; warned != dev {
		t.Fatalf("dev build = %v but dev_build warning logged = %v", dev, warned)
	}
}

func TestLoggerWritesUTCJSON(t *testing.T) {
	at := time.Date(2026, time.January, 2, 3, 4, 5, 6_000_000, time.FixedZone("synthetic", -4*60*60))
	record := slog.NewRecord(at, slog.LevelInfo, "message", 0)
	record.AddAttrs(slog.String("event", "probe"))
	var buf bytes.Buffer
	if err := NewLogger(&buf, nil).Handler().Handle(t.Context(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var rec struct {
		Time  string `json:"time"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v: %q", err, buf.String())
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil || !strings.HasSuffix(rec.Time, "Z") || !ts.Equal(at) || rec.Event != "probe" {
		t.Fatalf("record = %+v (%v), want %v written in UTC and the event attribute", rec, err, at)
	}
}
