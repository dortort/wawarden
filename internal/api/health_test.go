package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dortort/wawarden/internal/metrics"
)

func TestHealth(t *testing.T) {
	var ready atomic.Bool
	h := NewHealthHandler(ready.Load)
	tests := []struct {
		name   string
		ready  bool
		method string
		target string
		status int
		body   string
		allow  string
	}{
		{name: "ready", ready: true, method: http.MethodGet, target: "/healthz", status: http.StatusOK, body: `{"status":"ok"}`},
		{name: "ready with a query", ready: true, method: http.MethodGet, target: "/healthz?verbose=1", status: http.StatusOK, body: `{"status":"ok"}`},
		{name: "not ready", method: http.MethodGet, target: "/healthz", status: http.StatusServiceUnavailable, body: `{"status":"unavailable"}`},
		{name: "root", ready: true, method: http.MethodGet, target: "/", status: http.StatusNotFound, body: `{"error":"not_found"}`},
		{name: "trailing slash", ready: true, method: http.MethodGet, target: "/healthz/", status: http.StatusNotFound, body: `{"error":"not_found"}`},
		{name: "sub-path", ready: true, method: http.MethodGet, target: "/healthz/deep", status: http.StatusNotFound, body: `{"error":"not_found"}`},
		{name: "metrics", ready: true, method: http.MethodGet, target: "/metrics", status: http.StatusNotFound, body: `{"error":"not_found"}`},
		{name: "unknown path on POST", ready: true, method: http.MethodPost, target: "/missing", status: http.StatusNotFound, body: `{"error":"not_found"}`},
		{name: "post", ready: true, method: http.MethodPost, target: "/healthz", status: http.StatusMethodNotAllowed, body: `{"error":"method_not_allowed"}`, allow: "GET"},
		{name: "head", ready: true, method: http.MethodHead, target: "/healthz", status: http.StatusMethodNotAllowed, body: `{"error":"method_not_allowed"}`, allow: "GET"},
		{name: "options", ready: true, method: http.MethodOptions, target: "/healthz", status: http.StatusMethodNotAllowed, body: `{"error":"method_not_allowed"}`, allow: "GET"},
		{name: "delete while not ready", method: http.MethodDelete, target: "/healthz", status: http.StatusMethodNotAllowed, body: `{"error":"method_not_allowed"}`, allow: "GET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready.Store(tt.ready)
			rec := serve(h, newRequest(t, tt.method, tt.target, nil))
			if rec.Code != tt.status || rec.Body.String() != tt.body {
				t.Fatalf("response = %d %q, want %d %q", rec.Code, rec.Body.String(), tt.status, tt.body)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Fatalf("Allow = %q, want %q", got, tt.allow)
			}
			requireSecurityHeaders(t, rec.Header())
		})
	}
}

func TestHealthPanicIsAFixedInternalError(t *testing.T) {
	reg := metrics.NewRegistry()
	logs := installPanicReporter(t, reg)
	h := NewHealthHandler(func() bool { panic(secretPanic{text: panicCanary}) })
	rec := serve(h, newRequest(t, http.MethodGet, "/healthz", nil))
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
	requireSecurityHeaders(t, rec.Header())
	if got := metricValue(t, reg, `wawarden_panics_total{name="api.health"}`); got != "1" {
		t.Fatalf("wawarden_panics_total{name=\"api.health\"} = %q, want 1", got)
	}
	if strings.Contains(logs.String(), panicCanary) || strings.Contains(rec.Body.String(), panicCanary) {
		t.Fatal("the panic value leaked")
	}
}

func TestNewHealthHandlerRequiresAReadinessCheck(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	NewHealthHandler(nil)
}
