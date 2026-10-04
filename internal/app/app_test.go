package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
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

func init() {
	for _, pattern := range []string{"/debug/pprof/", "/debug/vars"} {
		http.DefaultServeMux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "debug")
		})
	}
}

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
	close  bool
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

func TestShutdownIsBoundedByTheGracePeriod(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a, logs := open(t, testConfig(t, ""), heldClient{entered: sync.OnceFunc(func() { close(entered) }), release: release})
	a.grace = 100 * time.Millisecond
	client := addr(t, a, "client")

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var runErr error
	go func() {
		runErr = a.Run(ctx)
		close(finished)
	}()
	t.Cleanup(func() {
		close(release)
		cancel()
		<-finished
	})

	inflight := make(chan error, 1)
	go func() {
		_, err := do(t, http.MethodGet, client, "/v1/held", bearer(syntheticClientToken))
		inflight <- err
	}()
	select {
	case <-entered:
	case err := <-inflight:
		t.Fatalf("the request never reached the authenticator: %v", err)
	}

	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Run still waits for a stuck request 10 s after shutdown began, want it to give up after its grace period")
	}
	if !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want the expired grace period reported", runErr)
	}
	if err := <-inflight; err == nil {
		t.Fatal("the stuck request completed although its connection was force-closed")
	}
	if stopped := logs.find("stopped"); len(stopped) != 1 || stopped[0]["level"] != "ERROR" {
		t.Fatalf("stopped events = %v, want one at ERROR", stopped)
	}
}

func TestUnsentBodiesDoNotHoldTheShutdown(t *testing.T) {
	a, _, stop := start(t, testConfig(t, token.NewAdmin()), noClients{})
	const head, unsent = "Host: wawarden.test\r\n", "Content-Length: 100000\r\n\r\nabc"
	tests := []struct {
		listener, request string
		status            int
	}{
		{listener: "client", request: "POST /v1/x HTTP/1.1\r\n" + head + unsent, status: http.StatusUnauthorized},
		{listener: "client", request: "POST /v1/x HTTP/1.1\r\n" + head + "Transfer-Encoding: chunked\r\n\r\n10\r\nabc", status: http.StatusUnauthorized},
		{listener: "client", request: "POST /v1/x HTTP/1.1\r\n" + head + "Expect: 100-continue\r\nContent-Length: 100000\r\n\r\n", status: http.StatusUnauthorized},
		{listener: "client", request: "POST /v1/x HTTP/1.1\r\n" + head + "Origin: https://attacker.example\r\n" + unsent, status: http.StatusForbidden},
		{listener: "client", request: "OPTIONS /v1/x HTTP/1.1\r\n" + head + unsent, status: http.StatusMethodNotAllowed},
		{listener: "admin", request: "POST /metrics HTTP/1.1\r\n" + head + unsent, status: http.StatusUnauthorized},
		{listener: "health", request: "GET /healthz HTTP/1.1\r\n" + head + unsent, status: http.StatusOK},
		{listener: "health", request: "POST /healthz HTTP/1.1\r\n" + head + unsent, status: http.StatusMethodNotAllowed},
		{listener: "health", request: "POST /elsewhere HTTP/1.1\r\n" + head + unsent, status: http.StatusNotFound},
	}
	for _, tt := range tests {
		if r := rawDo(t, addr(t, a, tt.listener), tt.request); r.status != tt.status || !r.close {
			t.Fatalf("%q on the %s listener = %d (close %v), want %d with Connection: close", tt.request, tt.listener, r.status, r.close, tt.status)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want a clean shutdown while answered callers still hold their bodies back", err)
	}
}

type stuckSet struct{ listenerSet }

func (s stuckSet) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return errors.Join(ctx.Err(), s.listenerSet.Shutdown(ctx))
}

type graceProbe struct {
	listenerSet
	expired chan<- bool
}

func (p graceProbe) Shutdown(ctx context.Context) error {
	p.expired <- ctx.Err() != nil
	return p.listenerSet.Shutdown(ctx)
}

func TestOneGracePeriodBoundsTheWholeShutdown(t *testing.T) {
	a, _ := open(t, testConfig(t, ""), noClients{})
	a.grace = 100 * time.Millisecond
	expired := make(chan bool, 1)
	a.serving = stuckSet{a.serving}
	a.health = graceProbe{a.health, expired}
	if err := run(t, a)(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want the expired grace period reported", err)
	}
	if !<-expired {
		t.Fatal("the health listener got a fresh grace period after the serving listeners used up theirs")
	}
}

type failingSet struct {
	listenerSet
	errs chan error
}

func (f failingSet) Err() <-chan error { return f.errs }

func TestListenerFailureStopsTheApp(t *testing.T) {
	tests := []struct {
		name string
		wrap func(*App, chan error)
	}{
		{name: "serving", wrap: func(a *App, errs chan error) { a.serving = failingSet{a.serving, errs} }},
		{name: "health", wrap: func(a *App, errs chan error) { a.health = failingSet{a.health, errs} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, logs := open(t, testConfig(t, ""), noClients{})
			health := addr(t, a, "health")
			errs := make(chan error, 1)
			tt.wrap(a, errs)

			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			var runErr error
			go func() {
				runErr = a.Run(ctx)
				close(finished)
			}()
			t.Cleanup(func() {
				cancel()
				<-finished
			})

			logs.waitFor(t, "ready")
			failure := errors.New("synthetic listener failure")
			errs <- failure
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("Run kept running after a listener failed")
			}
			if !errors.Is(runErr, failure) {
				t.Fatalf("Run = %v, want the listener failure", runErr)
			}
			if failed := logs.find("listener_failed"); len(failed) != 1 || failed[0]["level"] != "ERROR" || failed[0]["error"] != failure.Error() {
				t.Fatalf("listener_failed events = %v, want one at ERROR naming the failure", failed)
			}
			if _, err := do(t, http.MethodGet, health, "/healthz", nil); err == nil {
				t.Fatal("the health listener still answers after Run returned")
			}
		})
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

func rawDo(t *testing.T, to netip.AddrPort, request string) reply {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", to.String())
	if err != nil {
		t.Fatalf("dial %v: %v", to, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write to %v: %v", to, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no reply from %v to %q: %v", to, request, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the reply body from %v: %v", to, err)
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%q on %v carries %s", request, to, k)
		}
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(body), close: resp.Close}
}

func TestAsteriskOptionsGoesThroughTheHandlers(t *testing.T) {
	adminToken := token.NewAdmin()
	a, _, _ := start(t, testConfig(t, adminToken), oneClient{})
	refused := map[string]reply{
		"client": {status: http.StatusMethodNotAllowed, body: `{"error":"method_not_allowed"}`},
		"admin":  {status: http.StatusMethodNotAllowed, body: `{"error":"method_not_allowed"}`},
		"health": {status: http.StatusNotFound, body: `{"error":"not_found"}`},
	}
	browser := map[string]reply{
		"client": {status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		"admin":  {status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		"health": refused["health"],
	}
	for _, b := range a.Inventory() {
		for _, tt := range []struct {
			name, head string
			want       reply
		}{
			{name: "plain", head: "Connection: close\r\n", want: refused[b.Name]},
			{name: "with Origin", head: "Origin: https://attacker.example\r\nConnection: close\r\n", want: browser[b.Name]},
			{name: "with Sec-Fetch-Site", head: "Sec-Fetch-Site: cross-site\r\nConnection: close\r\n", want: browser[b.Name]},
			{name: "with an unsent body", head: "Content-Length: 10\r\n", want: refused[b.Name]},
		} {
			t.Run(b.Name+" "+tt.name, func(t *testing.T) {
				r := rawDo(t, b.Addr, "OPTIONS * HTTP/1.1\r\nHost: wawarden.test\r\n"+tt.head+"\r\n")
				if r.status != tt.want.status || r.body != tt.want.body {
					t.Fatalf("OPTIONS * = %d %q, want %d %q", r.status, r.body, tt.want.status, tt.want.body)
				}
				if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("OPTIONS * headers = %v, want no-store and nosniff", r.header)
				}
			})
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
