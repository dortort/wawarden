package control

import (
	"context"
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

func Grant(c *policy.Client) (policy.ReadGrant, bool) {
	return policy.DecideRead(c, time.Now())
}

func Respond(context.Context, policy.ReadGrant, *api.Request) (dto.Response, error) {
	return dto.Health{Status: "ok"}, nil
}

func Health() http.Handler {
	return api.NewHealthHandler(func() bool { return true })
}
