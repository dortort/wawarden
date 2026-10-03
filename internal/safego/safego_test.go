package safego_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
)

const canary = "canary-6f3a91d2-private-message-text"

type secretValue struct{ text string }

func (s secretValue) Error() string  { return s.text }
func (s secretValue) String() string { return s.text }

type lineWriter chan []byte

func (w lineWriter) Write(p []byte) (int, error) {
	w <- bytes.Clone(p)
	return len(p), nil
}

func install(t *testing.T) (*metrics.Registry, lineWriter) {
	t.Helper()
	lines := make(lineWriter, 4)
	reg := metrics.NewRegistry()
	safego.Install(slog.New(slog.NewJSONHandler(lines, nil)), reg)
	return reg, lines
}

func checkReport(t *testing.T, reg *metrics.Registry, line []byte, name, wantType string) {
	t.Helper()
	if bytes.Contains(line, []byte(canary)) {
		t.Fatalf("the log line leaks the panic value: %s", line)
	}
	var rec struct {
		Level     string `json:"level"`
		Event     string `json:"event"`
		Name      string `json:"name"`
		PanicType string `json:"panic_type"`
		Stack     string `json:"stack"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, line)
	}
	if rec.Level != "ERROR" || rec.Event != "panic" || rec.Name != name || rec.PanicType != wantType {
		t.Fatalf("report = %+v, want level ERROR, event panic, name %q, panic_type %q", rec, name, wantType)
	}
	if !strings.HasPrefix(rec.Stack, "goroutine ") || !strings.Contains(rec.Stack, "safego_test") {
		t.Fatalf("stack does not trace the panicking goroutine: %q", rec.Stack)
	}

	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(b.String(), canary) {
		t.Fatalf("metrics leak the panic value:\n%s", b.String())
	}
	if want := fmt.Sprintf("wawarden_panics_total{name=%q} 1\n", name); !strings.Contains(b.String(), want) {
		t.Fatalf("metrics lack %q:\n%s", want, b.String())
	}
}

func TestGoRecoversPanics(t *testing.T) {
	tests := []struct {
		name     string
		fn       func()
		wantType string
	}{
		{name: "string", fn: func() { panic(canary) }, wantType: "string"},
		{name: "error", fn: func() { panic(errors.New(canary)) }, wantType: "*errors.errorString"},
		{name: "wrapped error", fn: func() { panic(fmt.Errorf("wrapped: %w", secretValue{text: canary})) }, wantType: "*fmt.wrapError"},
		{name: "value with methods", fn: func() { panic(secretValue{text: canary}) }, wantType: "safego_test.secretValue"},
		{name: "pointer", fn: func() { panic(&secretValue{text: canary}) }, wantType: "*safego_test.secretValue"},
		{name: "runtime error", fn: func() {
			var s []string
			i := len(canary)
			_ = s[i]
		}, wantType: "runtime.boundsError"},
		{name: "nil", fn: func() { panic(nil) }, wantType: "*runtime.PanicNilError"},
		{name: "http abort sentinel", fn: func() { panic(http.ErrAbortHandler) }, wantType: "*errors.errorString"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, lines := install(t)
			safego.Go("worker", tt.fn)
			select {
			case line := <-lines:
				checkReport(t, reg, line, "worker", tt.wantType)
			case <-time.After(10 * time.Second):
				t.Fatal("the panic was not reported")
			}
		})
	}
}

func TestRecoverInHandler(t *testing.T) {
	reg, lines := install(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completed := false
		defer func() {
			if !completed {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		defer safego.Recover("handler")
		if r.URL.Path == "/panic" {
			panic(secretValue{text: canary})
		}
		w.WriteHeader(http.StatusNoContent)
		completed = true
	})

	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/fine", nil))
	if ok.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", ok.Code, http.StatusNoContent)
	}
	select {
	case line := <-lines:
		t.Fatalf("a request without a panic was reported: %s", line)
	default:
	}

	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), canary) {
		t.Fatalf("response = %d %q, want 500 without the panic value", failed.Code, failed.Body.String())
	}
	select {
	case line := <-lines:
		checkReport(t, reg, line, "handler", "safego_test.secretValue")
	default:
		t.Fatal("Recover did not report the panic before the handler returned")
	}
}

func checkNotReported(t *testing.T, reg *metrics.Registry, lines lineWriter) {
	t.Helper()
	select {
	case line := <-lines:
		t.Fatalf("http.ErrAbortHandler was reported as a panic: %s", line)
	default:
	}
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(b.String(), "wawarden_panics_total") {
		t.Fatalf("http.ErrAbortHandler was counted as a panic:\n%s", b.String())
	}
}

func TestRecoverPropagatesErrAbortHandler(t *testing.T) {
	reg, lines := install(t)
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", v)
			}
		}()
		defer safego.Recover("handler")
		panic(http.ErrAbortHandler)
	}()
	checkNotReported(t, reg, lines)
}

func TestRecoverLetsNetHTTPAbortTheResponse(t *testing.T) {
	reg, lines := install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer safego.Recover("handler")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the aborted handler produced a response with status %d", resp.StatusCode)
	}
	checkNotReported(t, reg, lines)
}

func TestInstallRejectsMissingDependencies(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(lineWriter(make(chan []byte, 1)), nil))
	tests := []struct {
		name   string
		logger *slog.Logger
		reg    *metrics.Registry
	}{
		{name: "nil logger", reg: metrics.NewRegistry()},
		{name: "nil registry", logger: logger},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			safego.Install(tt.logger, tt.reg)
		})
	}
}
