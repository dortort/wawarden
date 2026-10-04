package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

type adminFixture struct {
	handler     http.Handler
	reg         *metrics.Registry
	clock       *clock
	adminSecret string
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	f := &adminFixture{reg: metrics.NewRegistry(), clock: newClock(), adminSecret: token.NewAdmin()}
	cred, err := policy.ParseAdminCredential(token.Hash(f.adminSecret))
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	f.handler = NewAdminHandler(AdminDeps{Credential: cred, Metrics: f.reg, Now: f.clock.now})
	return f
}

func (f *adminFixture) failures(t *testing.T) string {
	t.Helper()
	return metricValue(t, f.reg, "wawarden_admin_auth_failures_total")
}

func TestAdminRefusesEveryOtherCredential(t *testing.T) {
	f := newAdminFixture(t)
	refused := []struct {
		name   string
		header http.Header
	}{
		{name: "missing"},
		{name: "empty header", header: http.Header{"Authorization": {""}}},
		{name: "empty token", header: http.Header{"Authorization": {"Bearer "}}},
		{name: "wrong token", header: bearer(token.NewAdmin())},
		{name: "configured hash presented as the token", header: bearer(token.Hash(f.adminSecret))},
		{name: "configured token with a suffix", header: bearer(f.adminSecret + "0")},
		{name: "configured token without a scheme", header: http.Header{"Authorization": {f.adminSecret}}},
		{name: "configured token under another scheme", header: http.Header{"Authorization": {"Basic " + f.adminSecret}}},
	}
	count := 0
	for _, tt := range refused {
		for _, target := range []string{"/metrics", "/missing"} {
			t.Run(tt.name+" "+target, func(t *testing.T) {
				rec := serve(f.handler, newRequest(t, http.MethodGet, target, tt.header))
				requireError(t, rec, http.StatusUnauthorized, codeUnauthorized)
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
				}
				requireSecurityHeaders(t, rec.Header())
			})
			count++
		}
	}
	if got := f.failures(t); got != strconv.Itoa(count) {
		t.Fatalf("admin authentication failures = %s, want %d", got, count)
	}

	for range failureBurst - count {
		serve(f.handler, newRequest(t, http.MethodGet, "/metrics", bearer(token.NewAdmin())))
	}
	for _, tt := range refused {
		requireError(t, serve(f.handler, newRequest(t, http.MethodGet, "/metrics", tt.header)), http.StatusTooManyRequests, codeTooManyRequests)
	}
	if got, want := f.failures(t), strconv.Itoa(failureBurst+len(refused)); got != want {
		t.Fatalf("admin authentication failures = %s, want %s", got, want)
	}

	rec := serve(f.handler, newRequest(t, http.MethodGet, "/metrics", bearer(f.adminSecret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("the configured token was throttled: status %d", rec.Code)
	}
	f.clock.advance(failureRefill)
	requireError(t, serve(f.handler, newRequest(t, http.MethodGet, "/metrics", nil)), http.StatusUnauthorized, codeUnauthorized)

	before, err := strconv.Atoi(f.failures(t))
	if err != nil {
		t.Fatalf("admin authentication failures: %v", err)
	}
	forms := 0
	for _, tt := range refused {
		for _, contentType := range formContentTypes {
			for _, target := range []string{"/metrics", "/missing"} {
				t.Run(tt.name+" POST "+target+" as "+contentType, func(t *testing.T) {
					f.clock.advance(time.Hour)
					requireUnreadFormRefused(t, serve(f.handler, newFormRequest(t, target, contentType, tt.header)))
				})
				forms++
			}
		}
	}
	if got, want := f.failures(t), strconv.Itoa(before+forms); got != want {
		t.Fatalf("admin authentication failures = %s, want %s", got, want)
	}
}

func TestAdminMetricsServesTheExposition(t *testing.T) {
	f := newAdminFixture(t)
	serve(f.handler, newRequest(t, http.MethodGet, "/metrics", bearer(token.NewAdmin())))

	rec := serve(f.handler, newRequest(t, http.MethodGet, "/metrics", bearer(f.adminSecret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	requireSecurityHeaders(t, rec.Header())
	body := rec.Body.String()
	if body != exposition(t, f.reg) {
		t.Fatalf("body is not the registry exposition:\n%s", body)
	}
	if !strings.Contains(body, "\nwawarden_admin_auth_failures_total 1\n") {
		t.Fatalf("exposition lacks the admin failure counter:\n%s", body)
	}
	if got := f.failures(t); got != "1" {
		t.Fatalf("admin authentication failures = %s, want 1", got)
	}
}

func TestAdminRoutingErrorsAreUniform(t *testing.T) {
	f := newAdminFixture(t)
	tests := []struct {
		name   string
		method string
		target string
		status int
		code   string
		allow  string
	}{
		{name: "unknown path", method: http.MethodGet, target: "/missing", status: http.StatusNotFound, code: codeNotFound},
		{name: "wrong method", method: http.MethodPost, target: "/metrics", status: http.StatusMethodNotAllowed, code: codeMethodNotAllowed, allow: "GET, HEAD"},
		{name: "options", method: http.MethodOptions, target: "/metrics", status: http.StatusMethodNotAllowed, code: codeMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(f.handler, newRequest(t, tt.method, tt.target, bearer(f.adminSecret)))
			requireError(t, rec, tt.status, tt.code)
			requireSecurityHeaders(t, rec.Header())
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Fatalf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
	rec := serve(f.handler, newRequest(t, http.MethodGet, "/metrics", headerWith(bearer(f.adminSecret), "Origin", "https://app.example.test")))
	requireError(t, rec, http.StatusForbidden, codeForbidden)
	if got := f.failures(t); got != "0" {
		t.Fatalf("admin authentication failures = %s, want 0", got)
	}
}

func TestUnconfiguredAdminCredentialRefusesEverything(t *testing.T) {
	reg := metrics.NewRegistry()
	h := NewAdminHandler(AdminDeps{Metrics: reg, Now: fixedNow})
	for _, header := range []http.Header{nil, bearer(token.NewAdmin()), bearer("")} {
		requireError(t, serve(h, newRequest(t, http.MethodGet, "/metrics", header)), http.StatusUnauthorized, codeUnauthorized)
	}
}

func TestNewAdminHandlerRequiresARegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	NewAdminHandler(AdminDeps{})
}
