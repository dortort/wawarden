package api

import (
	"context"
	"net/http"
	"time"

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

func clockOrSystem(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}
