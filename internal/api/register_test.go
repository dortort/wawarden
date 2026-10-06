package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

func withClientRequest(t *testing.T, c *policy.Client, method, target string) *http.Request {
	t.Helper()
	r := newRequest(t, method, target, nil)
	return r.WithContext(withClient(r.Context(), c))
}

func TestReadRouteRunsOnlyWithADecidedGrant(t *testing.T) {
	tests := []struct {
		name   string
		client *policy.Client
		allow  bool
	}{
		{name: "live client with an empty read set", client: liveClient("client-empty"), allow: true},
		{name: "read-all client", client: &policy.Client{ID: "client-all", ReadAll: true, ExpiresAt: testNow.Add(time.Hour)}, allow: true},
		{name: "no client", client: nil},
		{name: "revoked", client: &policy.Client{ID: "client-revoked", ReadAll: true, ExpiresAt: testNow.Add(time.Hour), Revoked: true}},
		{name: "expired", client: &policy.Client{ID: "client-expired", ReadAll: true, ExpiresAt: testNow}},
		{name: "never expiring", client: &policy.Client{ID: "client-forever", ReadAll: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRouter(fixedNow, policy.AdminCredential{})
			var grants []policy.ReadGrant
			rt.read("GET /probe", func(ctx context.Context, g policy.ReadGrant, _ *Request) (dto.Response, error) {
				if clientFrom(ctx) != tt.client {
					t.Error("the handler did not receive the request context")
				}
				grants = append(grants, g)
				return dto.Health{Status: "ok"}, nil
			})

			got := serve(rt, withClientRequest(t, tt.client, http.MethodGet, "/probe"))
			if !tt.allow {
				if len(grants) != 0 {
					t.Fatal("the handler ran without a grant")
				}
				requireSameResponse(t, got, serve(rt, withClientRequest(t, tt.client, http.MethodGet, "/missing")))
				return
			}
			if got.Code != http.StatusOK || got.Body.String() != `{"status":"ok"}` {
				t.Fatalf("response = %d %q, want 200 from the handler", got.Code, got.Body.String())
			}
			if len(grants) != 1 || !grants[0].Valid() || grants[0].Client() != tt.client.ID {
				t.Fatalf("handler grants = %+v, want one valid grant for %q", grants, tt.client.ID)
			}
		})
	}
}

func TestWriteRouteRefusesClientsWithoutWriteChats(t *testing.T) {
	tests := []struct {
		name   string
		client *policy.Client
	}{
		{name: "no client", client: nil},
		{name: "live client with empty sets", client: liveClient("client-empty")},
		{name: "write set holding only the zero chat", client: &policy.Client{
			ID: "client-zero", Write: map[policy.CanonicalChat]struct{}{{}: {}}, ExpiresAt: testNow.Add(time.Hour),
		}},
		{name: "read-all client", client: &policy.Client{ID: "client-all", ReadAll: true, ExpiresAt: testNow.Add(time.Hour)}},
		{name: "revoked", client: &policy.Client{ID: "client-revoked", ExpiresAt: testNow.Add(time.Hour), Revoked: true}},
		{name: "expired", client: &policy.Client{ID: "client-expired", ExpiresAt: testNow.Add(-time.Second)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRouter(fixedNow, policy.AdminCredential{})
			rt.write("POST /probe", func(context.Context, policy.WriteGrant, *Request) (dto.Response, error) {
				t.Error("the handler ran without a grant")
				return dto.Health{Status: "ok"}, nil
			})
			got := serve(rt, withClientRequest(t, tt.client, http.MethodPost, "/probe"))
			requireSameResponse(t, got, serve(rt, withClientRequest(t, tt.client, http.MethodPost, "/missing")))
		})
	}
}

func TestAdminRouteDecidesOnTheConfiguredHash(t *testing.T) {
	adminSecret := token.NewAdmin()
	cred, err := policy.ParseAdminCredential(token.Hash(adminSecret))
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	tests := []struct {
		name   string
		cred   policy.AdminCredential
		header http.Header
		allow  bool
	}{
		{name: "configured token", cred: cred, header: bearer(adminSecret), allow: true},
		{name: "lower-case scheme", cred: cred, header: http.Header{"Authorization": {"bearer " + adminSecret}}, allow: true},
		{name: "other token", cred: cred, header: bearer(token.NewAdmin())},
		{name: "no authorization", cred: cred},
		{name: "other scheme", cred: cred, header: http.Header{"Authorization": {"Basic " + adminSecret}}},
		{name: "unconfigured credential", cred: policy.AdminCredential{}, header: bearer(adminSecret)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRouter(fixedNow, tt.cred)
			var grants []policy.AdminGrant
			rt.admin("GET /probe", func(_ context.Context, g policy.AdminGrant, _ *Request) (dto.Response, error) {
				grants = append(grants, g)
				return dto.Health{Status: "ok"}, nil
			})
			got := serve(rt, newRequest(t, http.MethodGet, "/probe", tt.header))
			if !tt.allow {
				if len(grants) != 0 {
					t.Fatal("the handler ran without a grant")
				}
				requireSameResponse(t, got, serve(rt, newRequest(t, http.MethodGet, "/missing", tt.header)))
				return
			}
			if got.Code != http.StatusOK || len(grants) != 1 || !grants[0].Valid() {
				t.Fatalf("response = %d with grants %+v, want 200 with one valid grant", got.Code, grants)
			}
		})
	}
}

func TestRouterAnswersForTheMuxWithUniformBodies(t *testing.T) {
	rt := newRouter(fixedNow, policy.AdminCredential{})
	rt.read("GET /probe", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
		return dto.Health{Status: "ok"}, nil
	})
	tests := []struct {
		name   string
		method string
		target string
		status int
		code   string
		allow  string
	}{
		{name: "unknown path", method: http.MethodGet, target: "/missing", status: http.StatusNotFound, code: codeNotFound},
		{name: "wrong method", method: http.MethodDelete, target: "/probe", status: http.StatusMethodNotAllowed, code: codeMethodNotAllowed, allow: "GET, HEAD"},
		{name: "path needing cleaning", method: http.MethodGet, target: "/other/../probe", status: http.StatusNotFound, code: codeNotFound},
		{name: "doubled slash", method: http.MethodGet, target: "//probe", status: http.StatusNotFound, code: codeNotFound},
		{name: "trailing slash", method: http.MethodGet, target: "/probe/", status: http.StatusNotFound, code: codeNotFound},
		{name: "debug path", method: http.MethodGet, target: "/debug/pprof/", status: http.StatusNotFound, code: codeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serve(rt, withClientRequest(t, liveClient("client-live"), tt.method, tt.target))
			requireError(t, got, tt.status, tt.code)
			wantKeys := []string{"Content-Length", "Content-Type"}
			if tt.allow != "" {
				wantKeys = append(wantKeys, "Allow")
			}
			var keys []string
			for k := range got.Header() {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			slices.Sort(wantKeys)
			if !slices.Equal(keys, wantKeys) || got.Header().Get("Allow") != tt.allow {
				t.Fatalf("headers = %v, want exactly %v with Allow %q", got.Header(), wantKeys, tt.allow)
			}
		})
	}
}

func TestHandlerFailuresAreInternalErrors(t *testing.T) {
	rt := newRouter(fixedNow, policy.AdminCredential{})
	rt.read("GET /error", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
		return dto.Health{Status: "ok"}, errors.New("synthetic failure carrying request content")
	})
	rt.read("GET /nothing", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
		return nil, nil
	})
	for _, target := range []string{"/error", "/nothing"} {
		t.Run(target, func(t *testing.T) {
			requireError(t, serve(rt, withClientRequest(t, liveClient("client-live"), http.MethodGet, target)), http.StatusInternalServerError, codeInternal)
		})
	}
}

func TestRegistrationRecordsEveryRouteWithItsClass(t *testing.T) {
	rt := newRouter(fixedNow, policy.AdminCredential{})
	ok := dto.Health{Status: "ok"}
	rt.read("GET /r", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) { return ok, nil })
	rt.write("POST /w", func(context.Context, policy.WriteGrant, *Request) (dto.Response, error) { return ok, nil })
	rt.admin("GET /a", func(context.Context, policy.AdminGrant, *Request) (dto.Response, error) { return ok, nil })
	rt.mcp("POST /m", http.NotFoundHandler())
	want := []route{{pattern: "GET /r", class: classRead}, {pattern: "POST /w", class: classWrite}, {pattern: "GET /a", class: classAdmin}, {pattern: "POST /m", class: classMCP}}
	if !slices.Equal(rt.routes, want) {
		t.Fatalf("routes = %+v, want %+v", rt.routes, want)
	}
}

func TestRequestCarriesNoFieldWithoutAnAccessor(t *testing.T) {
	if fields, methods := reflect.TypeFor[Request]().NumField(), reflect.TypeFor[*Request]().NumMethod(); fields != 0 && methods == 0 {
		t.Fatalf("Request has %d fields and no method to read them: a field arrives together with its accessor", fields)
	}
}
