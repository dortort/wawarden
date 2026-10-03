package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	keyLive     = "synthetic-live-client"
	keyReadAll  = "synthetic-read-all-client"
	keyRevoked  = "synthetic-revoked-client"
	keyExpired  = "synthetic-expired-client"
	panicCanary = "canary-0d5e7c31-private-message-text"
)

type fakeAuthenticator map[string]*policy.Client

func (f fakeAuthenticator) Authenticate(_ context.Context, presented string) (*policy.Client, bool) {
	c, ok := f[presented]
	return c, ok
}

type panickingAuthenticator struct{}

func (panickingAuthenticator) Authenticate(context.Context, string) (*policy.Client, bool) {
	panic(secretPanic{text: panicCanary})
}

type secretPanic struct{ text string }

func (s secretPanic) Error() string { return s.text }

type clientFixture struct {
	handler http.Handler
	reg     *metrics.Registry
	clock   *clock

	mu    sync.Mutex
	reads []policy.ReadGrant
}

func newClientFixture(t *testing.T) *clientFixture {
	t.Helper()
	f := &clientFixture{reg: metrics.NewRegistry(), clock: newClock()}
	f.handler = NewClientHandler(ClientDeps{
		Authenticator: fakeAuthenticator{
			keyLive:    liveClient("client-live"),
			keyReadAll: &policy.Client{ID: "client-all", ReadAll: true, ExpiresAt: testNow.Add(24 * time.Hour)},
			keyRevoked: &policy.Client{ID: "client-revoked", ReadAll: true, ExpiresAt: testNow.Add(24 * time.Hour), Revoked: true},
			keyExpired: &policy.Client{ID: "client-expired", ReadAll: true, ExpiresAt: testNow.Add(-time.Second)},
		},
		Metrics: f.reg,
		Now:     f.clock.now,
	})
	rt := f.handler.(*pipeline).router
	rt.read("GET /probe/read", func(_ context.Context, g policy.ReadGrant, _ *Request) (dto.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads = append(f.reads, g)
		return dto.Health{Status: "ok"}, nil
	})
	rt.write("POST /probe/write", func(context.Context, policy.WriteGrant, *Request) (dto.Response, error) {
		t.Error("the write probe ran without a grant")
		return dto.Health{Status: "ok"}, nil
	})
	rt.read("GET /probe/panic", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
		panic(secretPanic{text: panicCanary})
	})
	rt.read("GET /probe/abort", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
		panic(http.ErrAbortHandler)
	})
	return f
}

func (f *clientFixture) grants() []policy.ReadGrant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]policy.ReadGrant(nil), f.reads...)
}

func (f *clientFixture) failures(t *testing.T) string {
	t.Helper()
	return metricValue(t, f.reg, "wawarden_auth_failures_total")
}

func headerWith(base http.Header, key, value string) http.Header {
	h := base.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Add(key, value)
	return h
}

func TestBrowserRequestsAreForbiddenBeforeAnythingElse(t *testing.T) {
	f := newClientFixture(t)
	tests := []struct {
		name   string
		method string
		target string
		header http.Header
	}{
		{name: "origin with a valid key", method: http.MethodGet, target: "/probe/read", header: headerWith(bearer(keyLive), "Origin", "https://app.example.test")},
		{name: "empty origin with a valid key", method: http.MethodGet, target: "/probe/read", header: headerWith(bearer(keyLive), "Origin", "")},
		{name: "opaque origin without a key", method: http.MethodGet, target: "/probe/read", header: headerWith(nil, "Origin", "null")},
		{name: "fetch metadata with a valid key", method: http.MethodGet, target: "/probe/read", header: headerWith(bearer(keyLive), "Sec-Fetch-Site", "same-origin")},
		{name: "fetch metadata without a key", method: http.MethodGet, target: "/probe/read", header: headerWith(nil, "Sec-Fetch-Site", "none")},
		{name: "origin on OPTIONS", method: http.MethodOptions, target: "/probe/read", header: headerWith(nil, "Origin", "https://app.example.test")},
		{name: "origin on an unknown path", method: http.MethodGet, target: "/missing", header: headerWith(nil, "Origin", "https://app.example.test")},
		{name: "origin on a panicking route", method: http.MethodGet, target: "/probe/panic", header: headerWith(bearer(keyLive), "Origin", "https://app.example.test")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(f.handler, newRequest(t, tt.method, tt.target, tt.header))
			requireError(t, rec, http.StatusForbidden, codeForbidden)
			requireSecurityHeaders(t, rec.Header())
			if rec.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("a refused browser request carries an authentication challenge")
			}
		})
	}
	if got := f.failures(t); got != "0" {
		t.Fatalf("authentication failures = %s, want 0: the browser guard must run before authentication", got)
	}
	if len(f.grants()) != 0 {
		t.Fatal("a handler ran for a browser request")
	}
}

func TestOptionsIsRefusedBeforeAuthentication(t *testing.T) {
	f := newClientFixture(t)
	tests := []struct {
		name   string
		target string
		header http.Header
	}{
		{name: "existing route with a valid key", target: "/probe/read", header: bearer(keyLive)},
		{name: "existing route without a key", target: "/probe/read"},
		{name: "unknown path without a key", target: "/missing"},
		{name: "asterisk form", target: "*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(f.handler, newRequest(t, http.MethodOptions, tt.target, tt.header))
			requireError(t, rec, http.StatusMethodNotAllowed, codeMethodNotAllowed)
			requireSecurityHeaders(t, rec.Header())
			if allow := rec.Header().Values("Allow"); len(allow) != 0 {
				t.Fatalf("Allow = %q: OPTIONS must be refused before routing", allow)
			}
		})
	}
	if got := f.failures(t); got != "0" {
		t.Fatalf("authentication failures = %s, want 0: OPTIONS must be refused before authentication", got)
	}
}

func TestAuthenticationPrecedesRoutingAndTheBody(t *testing.T) {
	f := newClientFixture(t)
	credentials := []struct {
		name   string
		header http.Header
	}{
		{name: "no authorization"},
		{name: "unknown key", header: bearer("synthetic-unknown-client")},
		{name: "other scheme", header: http.Header{"Authorization": {"Basic " + keyLive}}},
		{name: "key without scheme", header: http.Header{"Authorization": {keyLive}}},
	}
	targets := []struct {
		method string
		target string
	}{
		{method: http.MethodGet, target: "/probe/read"},
		{method: http.MethodPost, target: "/probe/write"},
		{method: http.MethodDelete, target: "/probe/read"},
		{method: http.MethodGet, target: "/missing"},
		{method: http.MethodPost, target: "/missing"},
	}
	count := 0
	for _, c := range credentials {
		reference := serve(f.handler, newRequest(t, http.MethodGet, "/probe/read", c.header))
		for _, tt := range targets {
			t.Run(c.name+" "+tt.method+" "+tt.target, func(t *testing.T) {
				rec := serve(f.handler, newRequest(t, tt.method, tt.target, c.header))
				requireError(t, rec, http.StatusUnauthorized, codeUnauthorized)
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
				}
				requireSecurityHeaders(t, rec.Header())
				requireSameResponse(t, rec, reference)
			})
			count++
		}
		count++
	}
	if got, want := f.failures(t), strconv.Itoa(count); got != want {
		t.Fatalf("authentication failures = %s, want %s", got, want)
	}
	if len(f.grants()) != 0 {
		t.Fatal("a handler ran for an unauthenticated request")
	}
}

func TestAuthenticatedRoutingErrorsAreUniform(t *testing.T) {
	f := newClientFixture(t)
	tests := []struct {
		name   string
		method string
		target string
		status int
		code   string
		allow  string
	}{
		{name: "unknown path", method: http.MethodGet, target: "/missing", status: http.StatusNotFound, code: codeNotFound},
		{name: "wrong method on a read route", method: http.MethodDelete, target: "/probe/read", status: http.StatusMethodNotAllowed, code: codeMethodNotAllowed, allow: "GET, HEAD"},
		{name: "wrong method on a write route", method: http.MethodGet, target: "/probe/write", status: http.StatusMethodNotAllowed, code: codeMethodNotAllowed, allow: "POST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(f.handler, newRequest(t, tt.method, tt.target, bearer(keyLive)))
			requireError(t, rec, tt.status, tt.code)
			requireSecurityHeaders(t, rec.Header())
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Fatalf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
	if got := f.failures(t); got != "0" {
		t.Fatalf("authentication failures = %s, want 0", got)
	}
}

func fetch(t *testing.T, srv *httptest.Server, method, path string, header http.Header) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatalf("close body: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	h := resp.Header.Clone()
	h.Del("Date")
	return resp.StatusCode, h, body
}

func TestDeniedRoutesAreIndistinguishableFromUnknownOnes(t *testing.T) {
	f := newClientFixture(t)
	srv := httptest.NewServer(f.handler)
	defer srv.Close()

	wantStatus, wantHeader, wantBody := fetch(t, srv, http.MethodGet, "/missing", bearer(keyLive))
	if wantStatus != http.StatusNotFound || string(wantBody) != `{"error":"not_found"}` {
		t.Fatalf("reference = %d %q, want the uniform 404", wantStatus, wantBody)
	}
	tests := []struct {
		name   string
		method string
		path   string
		key    string
	}{
		{name: "read route for a revoked client", method: http.MethodGet, path: "/probe/read", key: keyRevoked},
		{name: "read route for an expired client", method: http.MethodGet, path: "/probe/read", key: keyExpired},
		{name: "write route for a client without write chats", method: http.MethodPost, path: "/probe/write", key: keyLive},
		{name: "write route for a read-all client", method: http.MethodPost, path: "/probe/write", key: keyReadAll},
		{name: "unknown path for a revoked client", method: http.MethodGet, path: "/missing", key: keyRevoked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, header, body := fetch(t, srv, tt.method, tt.path, bearer(tt.key))
			if status != wantStatus || !reflect.DeepEqual(header, wantHeader) || string(body) != string(wantBody) {
				t.Fatalf("response = %d %v %q\nwant       %d %v %q", status, header, body, wantStatus, wantHeader, wantBody)
			}
		})
	}
	if len(f.grants()) != 0 {
		t.Fatal("a handler ran for a denied client")
	}
}

func TestHandlersReceiveValidGrants(t *testing.T) {
	f := newClientFixture(t)
	for _, key := range []string{keyLive, keyReadAll} {
		rec := serve(f.handler, newRequest(t, http.MethodGet, "/probe/read", bearer(key)))
		if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
			t.Fatalf("response = %d %q, want 200 from the probe", rec.Code, rec.Body.String())
		}
		requireSecurityHeaders(t, rec.Header())
	}
	got := f.grants()
	if len(got) != 2 {
		t.Fatalf("handler ran %d times, want 2", len(got))
	}
	for i, want := range []struct {
		client string
		all    bool
	}{{client: "client-live"}, {client: "client-all", all: true}} {
		if !got[i].Valid() || got[i].Client() != want.client || got[i].All() != want.all {
			t.Fatalf("grant %d: valid %v, client %q, all %v; want a valid grant for %q with all %v",
				i, got[i].Valid(), got[i].Client(), got[i].All(), want.client, want.all)
		}
	}
}

func TestPanicsBecomeFixedInternalErrors(t *testing.T) {
	f := newClientFixture(t)
	logs := installPanicReporter(t, f.reg)
	rec := serve(f.handler, newRequest(t, http.MethodGet, "/probe/panic", bearer(keyLive)))
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
	requireSecurityHeaders(t, rec.Header())

	panicking := NewClientHandler(ClientDeps{Authenticator: panickingAuthenticator{}, Metrics: metrics.NewRegistry(), Now: fixedNow})
	rec = serve(panicking, newRequest(t, http.MethodGet, "/probe/read", bearer(keyLive)))
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
	requireSecurityHeaders(t, rec.Header())

	if got := metricValue(t, f.reg, `wawarden_panics_total{name="api.client"}`); got != "2" {
		t.Fatalf("wawarden_panics_total{name=\"api.client\"} = %q, want 2", got)
	}
	out := logs.String()
	if strings.Contains(out, panicCanary) || strings.Contains(exposition(t, f.reg), panicCanary) {
		t.Fatalf("the panic value leaked:\n%s", out)
	}
	if strings.Count(out, `"panic_type":"api.secretPanic"`) != 2 {
		t.Fatalf("panic reports do not name the panic type:\n%s", out)
	}
}

func TestAbortHandlerPanicsReachNetHTTP(t *testing.T) {
	f := newClientFixture(t)
	logs := installPanicReporter(t, f.reg)
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", v)
			}
		}()
		f.handler.ServeHTTP(rec, newRequest(t, http.MethodGet, "/probe/abort", bearer(keyLive)))
	}()
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("the aborted response was written: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if out := logs.String(); out != "" {
		t.Fatalf("http.ErrAbortHandler was logged as a panic:\n%s", out)
	}
	if strings.Contains(exposition(t, f.reg), "wawarden_panics_total") {
		t.Fatal("http.ErrAbortHandler was counted as a panic")
	}
}

func TestEveryClientResponseCarriesSecurityHeaders(t *testing.T) {
	f := newClientFixture(t)
	installPanicReporter(t, f.reg)
	browser := headerWith(bearer(keyLive), "Origin", "https://app.example.test")
	steps := []struct {
		name   string
		method string
		target string
		header http.Header
		status int
	}{
		{name: "success", method: http.MethodGet, target: "/probe/read", header: bearer(keyLive), status: http.StatusOK},
		{name: "unauthenticated", method: http.MethodGet, target: "/probe/read", status: http.StatusUnauthorized},
		{name: "browser", method: http.MethodGet, target: "/probe/read", header: browser, status: http.StatusForbidden},
		{name: "options", method: http.MethodOptions, target: "/probe/read", status: http.StatusMethodNotAllowed},
		{name: "unknown path", method: http.MethodGet, target: "/missing", header: bearer(keyLive), status: http.StatusNotFound},
		{name: "wrong method", method: http.MethodPut, target: "/probe/read", header: bearer(keyLive), status: http.StatusMethodNotAllowed},
		{name: "denied", method: http.MethodGet, target: "/probe/read", header: bearer(keyRevoked), status: http.StatusNotFound},
		{name: "panic", method: http.MethodGet, target: "/probe/panic", header: bearer(keyLive), status: http.StatusInternalServerError},
	}
	for _, s := range steps {
		rec := serve(f.handler, newRequest(t, s.method, s.target, s.header))
		if rec.Code != s.status {
			t.Fatalf("%s: status = %d, want %d", s.name, rec.Code, s.status)
		}
		requireSecurityHeaders(t, rec.Header())
	}
	for range failureBurst {
		serve(f.handler, newRequest(t, http.MethodGet, "/probe/read", nil))
	}
	rec := serve(f.handler, newRequest(t, http.MethodGet, "/probe/read", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled: status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	requireSecurityHeaders(t, rec.Header())
}

func TestFailedAuthenticationIsThrottled(t *testing.T) {
	f := newClientFixture(t)
	unauthenticated := func(header http.Header) *httptest.ResponseRecorder {
		return serve(f.handler, newRequest(t, http.MethodGet, "/probe/read", header))
	}
	expect := func(step string, header http.Header, status int) {
		t.Helper()
		rec := unauthenticated(header)
		switch status {
		case http.StatusUnauthorized:
			requireError(t, rec, status, codeUnauthorized)
		case http.StatusTooManyRequests:
			requireError(t, rec, status, codeTooManyRequests)
			if rec.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("%s: a throttled response carries an authentication challenge", step)
			}
		}
	}

	for range failureBurst {
		expect("burst", nil, http.StatusUnauthorized)
	}
	expect("beyond the burst", nil, http.StatusTooManyRequests)
	expect("unknown key beyond the burst", bearer("synthetic-unknown-client"), http.StatusTooManyRequests)
	if rec := unauthenticated(bearer(keyLive)); rec.Code != http.StatusOK {
		t.Fatalf("a valid key was throttled: status %d", rec.Code)
	}
	if rec := unauthenticated(bearer(keyRevoked)); rec.Code != http.StatusNotFound {
		t.Fatalf("an authenticated but denied key was throttled: status %d", rec.Code)
	}
	if got := f.failures(t); got != "32" {
		t.Fatalf("authentication failures = %s, want 32", got)
	}

	f.clock.advance(failureRefill)
	expect("one refilled token", nil, http.StatusUnauthorized)
	expect("refill spent", nil, http.StatusTooManyRequests)
	f.clock.advance(failureRefill / 2)
	expect("half a token", nil, http.StatusTooManyRequests)
	f.clock.advance(failureRefill / 2)
	expect("the other half", nil, http.StatusUnauthorized)

	f.clock.advance(time.Hour)
	for range failureBurst {
		expect("refilled burst", nil, http.StatusUnauthorized)
	}
	expect("refill is capped at the burst", nil, http.StatusTooManyRequests)
	if got := f.failures(t); got != "67" {
		t.Fatalf("authentication failures = %s, want 67", got)
	}
}

func TestNewClientHandlerRequiresItsDependencies(t *testing.T) {
	tests := []struct {
		name string
		deps ClientDeps
	}{
		{name: "no authenticator", deps: ClientDeps{Metrics: metrics.NewRegistry()}},
		{name: "no registry", deps: ClientDeps{Authenticator: fakeAuthenticator{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			NewClientHandler(tt.deps)
		})
	}
}
