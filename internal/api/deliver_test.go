package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

func TestTheMediaRefusalsCarryTheirStatus(t *testing.T) {
	tests := []struct {
		err    *codedError
		status int
		code   string
	}{
		{err: errMediaUnavailable, status: http.StatusNotFound, code: "media_unavailable"},
		{err: errMediaNotRequested, status: http.StatusConflict, code: "media_not_requested"},
		{err: errMediaExpired, status: http.StatusGone, code: "media_expired"},
		{err: errMediaTooLarge, status: http.StatusUnprocessableEntity, code: "media_too_large"},
		{err: errMediaUnsupported, status: http.StatusUnprocessableEntity, code: "media_unsupported"},
		{err: errMediaFailed, status: http.StatusBadGateway, code: "media_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			rt := newRouter(fixedNow, policy.AdminCredential{})
			rt.read("GET /probe", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
				return nil, tt.err
			})
			rec := serve(rt, withClientRequest(t, liveClient("client-x"), http.MethodGet, "/probe"))
			requireError(t, rec, tt.status, tt.code)
			if rec.Header().Get("Retry-After") != "" {
				t.Fatalf("Retry-After = %q, want none", rec.Header().Get("Retry-After"))
			}
		})
	}
}
