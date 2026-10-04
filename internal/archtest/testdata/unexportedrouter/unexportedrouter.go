package unexportedrouter

import (
	"context"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

func Register(r *api.router) {
	r.read("GET /forged", func(context.Context, policy.ReadGrant, *api.Request) (dto.Response, error) {
		return dto.Health{Status: "ok"}, nil
	})
}
