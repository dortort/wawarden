package listeners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
)

var loopback = netip.MustParseAddrPort("127.0.0.1:0")

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

func newLogger(t *testing.T) (*slog.Logger, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	safego.Install(logger, metrics.NewRegistry())
	return logger, logs
}

func text(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
}

func openSet(t *testing.T, specs ...Spec) *Set {
	t.Helper()
	logger, _ := newLogger(t)
	s, err := Open(t.Context(), logger, specs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func get(t *testing.T, addr netip.AddrPort, path string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr.String()+path, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func TestServersAreHardened(t *testing.T) {
	s := openSet(t, Spec{Name: "one", Addr: loopback, Handler: text("one")}, Spec{Name: "two", Addr: loopback, Handler: text("two")})
	for _, srv := range s.servers {
		t.Run(srv.name, func(t *testing.T) {
			got := srv.http
			tests := []struct {
				name      string
				got, want any
			}{
				{name: "ReadHeaderTimeout", got: got.ReadHeaderTimeout, want: 5 * time.Second},
				{name: "ReadTimeout", got: got.ReadTimeout, want: 15 * time.Second},
				{name: "WriteTimeout", got: got.WriteTimeout, want: 30 * time.Second},
				{name: "IdleTimeout", got: got.IdleTimeout, want: 60 * time.Second},
				{name: "MaxHeaderBytes", got: got.MaxHeaderBytes, want: 16 * 1024},
				{name: "ErrorLog set", got: got.ErrorLog != nil, want: true},
				{name: "Handler set", got: got.Handler != nil, want: true},
			}
			for _, tt := range tests {
				if tt.got != tt.want {
					t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
				}
			}
		})
	}
}

func TestErrorLogIsStructured(t *testing.T) {
	logger, logs := newLogger(t)
	s, err := Open(t.Context(), logger, []Spec{{Name: "noisy", Addr: loopback, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		w.WriteHeader(http.StatusNoContent)
	})}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Serve()
	if status, _, err := get(t, s.Inventory()[0].Addr, "/"); err != nil || status != http.StatusNoContent {
		t.Fatalf("GET = %d, %v; want 204", status, err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	var rec struct {
		Level    string `json:"level"`
		Event    string `json:"event"`
		Listener string `json:"listener"`
		Msg      string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(logs.String()), &rec); err != nil {
		t.Fatalf("the server error log is not one JSON line: %v: %q", err, logs.String())
	}
	if rec.Level != "WARN" || rec.Event != "http_server_error" || rec.Listener != "noisy" || !strings.Contains(rec.Msg, "superfluous") {
		t.Fatalf("server error log = %+v", rec)
	}
}

func TestInventoryReportsNamesAndBoundAddresses(t *testing.T) {
	s := openSet(t, Spec{Name: "first", Addr: loopback, Handler: text("first")}, Spec{Name: "second", Addr: loopback, Handler: text("second")})
	s.Serve()
	inv := s.Inventory()
	if len(inv) != 2 || inv[0].Name != "first" || inv[1].Name != "second" {
		t.Fatalf("Inventory = %+v, want first and second in order", inv)
	}
	for _, b := range inv {
		if !b.Addr.Addr().IsLoopback() || b.Addr.Port() == 0 {
			t.Fatalf("%s is bound to %v, want a loopback address with a real port", b.Name, b.Addr)
		}
		status, body, err := get(t, b.Addr, "/")
		if err != nil || status != http.StatusOK || body != b.Name {
			t.Fatalf("GET %s = %d %q %v, want 200 %q", b.Name, status, body, err, b.Name)
		}
	}
}

func TestOpenRefusesIncompleteSpecs(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
	}{
		{name: "no name", spec: Spec{Addr: loopback, Handler: text("x")}},
		{name: "no address", spec: Spec{Name: "x", Handler: text("x")}},
		{name: "no handler, which would serve the default mux", spec: Spec{Name: "x", Addr: loopback}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, _ := newLogger(t)
			if s, err := Open(t.Context(), logger, []Spec{tt.spec}); err == nil {
				_ = s.Shutdown(t.Context())
				t.Fatal("Open accepted an incomplete spec")
			}
		})
	}
}

func TestOpenReleasesEarlierListenersOnFailure(t *testing.T) {
	taken := openSet(t, Spec{Name: "taken", Addr: loopback, Handler: text("x")})
	busy := taken.Inventory()[0].Addr
	logger, _ := newLogger(t)
	_, err := Open(t.Context(), logger, []Spec{
		{Name: "first", Addr: loopback, Handler: text("x")},
		{Name: "clash", Addr: busy, Handler: text("x")},
	})
	if err == nil || !strings.Contains(err.Error(), "clash") {
		t.Fatalf("Open = %v, want an error naming the clashing listener", err)
	}
}

func TestShutdownIsGraceful(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := openSet(t, Spec{Name: "slow", Addr: loopback, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	})})
	began := make(chan struct{})
	s.servers[0].http.RegisterOnShutdown(sync.OnceFunc(func() { close(began) }))
	s.Serve()
	addr := s.Inventory()[0].Addr

	type result struct {
		status int
		body   string
		err    error
	}
	inflight := make(chan result, 1)
	go func() {
		status, body, err := get(t, addr, "/")
		inflight <- result{status, body, err}
	}()
	<-entered

	shutdown := make(chan error, 1)
	go func() { shutdown <- s.Shutdown(t.Context()) }()
	<-began
	if _, _, err := get(t, addr, "/"); err == nil {
		t.Fatal("a new connection was served after shutdown began")
	}
	close(release)

	if r := <-inflight; r.err != nil || r.status != http.StatusOK || r.body != "finished" {
		t.Fatalf("in-flight request = %d %q %v, want it to complete", r.status, r.body, r.err)
	}
	if err := <-shutdown; err != nil {
		t.Fatalf("Shutdown = %v, want a clean drain", err)
	}
	select {
	case err := <-s.Err():
		t.Fatalf("a graceful shutdown surfaced %v", err)
	default:
	}
}

func TestShutdownIsBounded(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := openSet(t, Spec{Name: "stuck", Addr: loopback, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})})
	s.Serve()
	inflight := make(chan error, 1)
	go func() {
		_, _, err := get(t, s.Inventory()[0].Addr, "/")
		inflight <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := s.Shutdown(ctx)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("Shutdown = %v, want the expired grace period reported for the stuck listener", err)
	}
	if err := <-inflight; err == nil {
		t.Fatal("the stuck request completed although its connection was force-closed")
	}
}

func TestListenerErrorSurfaces(t *testing.T) {
	s := openSet(t, Spec{Name: "fragile", Addr: loopback, Handler: text("x")})
	s.Serve()
	if err := s.servers[0].ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-s.Err():
		if !strings.Contains(err.Error(), "fragile") {
			t.Fatalf("error %q does not name the listener", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failed listener was not reported")
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown after a failure = %v", err)
	}
}
