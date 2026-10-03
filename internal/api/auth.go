package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/dortort/wawarden/internal/policy"
)

type clientKey struct{}

func withClient(ctx context.Context, c *policy.Client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

func clientFrom(ctx context.Context) *policy.Client {
	c, _ := ctx.Value(clientKey{}).(*policy.Client)
	return c
}

func bearerToken(h http.Header) (string, bool) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, presented, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || presented == "" || strings.ContainsAny(presented, " \t") {
		return "", false
	}
	return presented, true
}
