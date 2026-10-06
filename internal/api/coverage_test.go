package api

import (
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

func init() {
	for _, pattern := range []string{"/debug/pprof/", "/debug/vars"} {
		http.DefaultServeMux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "debug")
		})
	}
}

func TestRoutesMatchTheGoldenList(t *testing.T) {
	golden := map[string][]route{
		"client": {
			{pattern: "GET /v1/me", class: classRead},
			{pattern: "GET /v1/chats", class: classRead},
			{pattern: "GET /v1/chats/{ref}", class: classRead},
			{pattern: "GET /v1/chats/{ref}/messages", class: classRead},
			{pattern: "GET /v1/search", class: classRead},
			{pattern: "GET /v1/changes", class: classRead},
			{pattern: "GET /v1/messages/{mref}", class: classRead},
		},
		"admin": {
			{pattern: "GET /metrics", class: classAdmin},
			{pattern: "GET /admin/v1/status", class: classAdmin},
			{pattern: "POST /admin/v1/pair", class: classAdmin},
			{pattern: "POST /admin/v1/reconnect", class: classAdmin},
			{pattern: "POST /admin/v1/clients", class: classAdmin},
			{pattern: "GET /admin/v1/clients", class: classAdmin},
			{pattern: "GET /admin/v1/clients/{id}", class: classAdmin},
			{pattern: "POST /admin/v1/clients/{id}/revoke", class: classAdmin},
			{pattern: "GET /admin/v1/chats", class: classAdmin},
		},
	}
	handlers := map[string]http.Handler{
		"client": NewClientHandler(testClientDeps(t, fakeAuthenticator{}, nil)),
		"admin":  NewAdminHandler(AdminDeps{Metrics: metrics.NewRegistry(), Service: &fakeAdmin{}, Events: &fakeEvents{}}),
	}
	if len(handlers) != len(golden) {
		t.Fatalf("%d handlers for %d golden lists", len(handlers), len(golden))
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			p, ok := h.(*pipeline)
			if !ok {
				t.Fatalf("the %s handler is a %T, not the pipeline whose routes are recorded", name, h)
			}
			for _, r := range p.router.routes {
				if !slices.Contains([]class{classRead, classWrite, classAdmin}, r.class) {
					t.Errorf("route %q has no policy class (%q)", r.pattern, r.class)
				}
			}
			if !slices.Equal(p.router.routes, golden[name]) {
				t.Fatalf("routes = %+v, golden list = %+v: update the golden list with the change", p.router.routes, golden[name])
			}
		})
	}
}

func TestDebugEndpointsAreNeverServed(t *testing.T) {
	paths := []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline", "/debug/vars", "/debug/anything"}
	for _, path := range []string{"/debug/pprof/", "/debug/vars"} {
		if _, pattern := http.DefaultServeMux.Handler(newRequest(t, http.MethodGet, path, nil)); pattern == "" {
			t.Fatalf("the default mux does not serve %s, so this test proves nothing", path)
		}
	}

	adminSecret := token.NewAdmin()
	cred, err := policy.ParseAdminCredential(token.Hash(adminSecret))
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	handlers := []struct {
		name    string
		handler http.Handler
		header  http.Header
	}{
		{name: "client", handler: newClientFixture(t).handler, header: bearer(keyLive)},
		{name: "admin", handler: NewAdminHandler(AdminDeps{Credential: cred, Metrics: metrics.NewRegistry(), Service: &fakeAdmin{}, Events: &fakeEvents{}, Now: fixedNow}), header: bearer(adminSecret)},
		{name: "health", handler: NewHealthHandler(func() bool { return true })},
	}
	for _, h := range handlers {
		for _, path := range paths {
			t.Run(h.name+path, func(t *testing.T) {
				rec := serve(h.handler, newRequest(t, http.MethodGet, path, h.header))
				requireError(t, rec, http.StatusNotFound, codeNotFound)
				requireSecurityHeaders(t, rec.Header())
			})
		}
	}
}
