package wa

import (
	"context"

	"github.com/dortort/wawarden/internal/engine"
)

func (c *Client) FetchMedia(context.Context, engine.MediaRequest) (engine.Staged, error) {
	return nil, engine.ErrMediaUnavailable
}
