package api

import (
	"context"
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
)

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*policy.Client, bool)
}

type ClientDeps struct {
	Authenticator Authenticator
	Metrics       *metrics.Registry
	Now           func() time.Time
}

func NewClientHandler(d ClientDeps) http.Handler {
	if d.Authenticator == nil || d.Metrics == nil {
		panic("api: NewClientHandler needs an Authenticator and a metrics registry")
	}
	now := clockOrSystem(d.Now)
	return &pipeline{
		name:     "api.client",
		router:   newRouter(now, policy.AdminCredential{}),
		failures: d.Metrics.Counter("wawarden_auth_failures_total", "Failed client authentications."),
		throttle: newBucket(now),
		authenticate: func(r *http.Request) (*http.Request, bool) {
			presented, ok := bearerToken(r.Header)
			if !ok {
				return r, false
			}
			c, ok := d.Authenticator.Authenticate(r.Context(), presented)
			if !ok || c == nil {
				return r, false
			}
			return r.WithContext(withClient(r.Context(), c)), true
		},
	}
}

type AdminDeps struct {
	Credential policy.AdminCredential
	Metrics    *metrics.Registry
	Now        func() time.Time
}

func NewAdminHandler(d AdminDeps) http.Handler {
	if d.Metrics == nil {
		panic("api: NewAdminHandler needs a metrics registry")
	}
	now := clockOrSystem(d.Now)
	p := &pipeline{
		name:     "api.admin",
		router:   newRouter(now, d.Credential),
		failures: d.Metrics.Counter("wawarden_admin_auth_failures_total", "Failed admin authentications."),
		throttle: newBucket(now),
		authenticate: func(r *http.Request) (*http.Request, bool) {
			presented, _ := bearerToken(r.Header)
			_, ok := policy.DecideAdmin(d.Credential, presented)
			return r, ok
		},
	}
	p.router.admin("GET /metrics", func(context.Context, policy.AdminGrant, *Request) (dto.Response, error) {
		return dto.Metrics(d.Metrics), nil
	})
	return p
}

func NewHealthHandler(ready func() bool) http.Handler {
	if ready == nil {
		panic("api: NewHealthHandler needs a readiness check")
	}
	serve := func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		ignoreBody(w, r)
		switch {
		case r.URL.Path != "/healthz":
			writeError(w, http.StatusNotFound, codeNotFound)
		case r.Method != http.MethodGet:
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed)
		case ready():
			writeResponse(w, http.StatusOK, dto.Health{Status: "ok"})
		default:
			writeResponse(w, http.StatusServiceUnavailable, dto.Health{Status: "unavailable"})
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recovering("api.health", w, r, serve)
	})
}

func clockOrSystem(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}
