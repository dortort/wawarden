package app

import (
	"context"
	"errors"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/store/ingest"
)

type adminService struct {
	engine  *engine.Engine
	archive *ingest.Store
	version string
}

func (s adminService) Status(ctx context.Context) (api.AdminStatus, error) {
	st := s.engine.Status()
	c, err := s.archive.Admin().Counters(ctx)
	if err != nil {
		return api.AdminStatus{}, err
	}
	return api.AdminStatus{
		State:            string(st.State),
		Reason:           string(st.Reason),
		Paired:           st.Paired,
		Chats:            c.Chats,
		Messages:         c.Messages,
		BlobsPending:     c.BlobsPending,
		BlobsQuarantined: c.BlobsQuarantined,
		InboxBacklog:     c.InboxBacklog,
		InboxQuarantined: c.InboxQuarantined,
		LastIngest:       c.LastIngest,
		Version:          s.version,
	}, nil
}

func (s adminService) Pair(ctx context.Context) (string, error) {
	code, err := s.engine.Pair(ctx)
	if err != nil {
		return "", adminError(err)
	}
	return code, nil
}

func (s adminService) Reconnect(context.Context) error {
	return adminError(s.engine.Reconnect())
}

var adminErrors = []struct{ engine, api error }{
	{engine.ErrAlreadyPaired, api.ErrAlreadyPaired},
	{engine.ErrAlreadyConnected, api.ErrAlreadyConnected},
	{engine.ErrNotPaired, api.ErrNotPaired},
	{engine.ErrOwnerPhoneMissing, api.ErrOwnerPhoneMissing},
	{engine.ErrOwnerMismatch, api.ErrOwnerMismatch},
	{engine.ErrPairRateLimited, api.ErrRateLimited},
	{engine.ErrPairFailed, api.ErrPairFailed},
	{engine.ErrStopped, api.ErrEngineUnavailable},
}

func adminError(err error) error {
	for _, m := range adminErrors {
		if errors.Is(err, m.engine) {
			return m.api
		}
	}
	return err
}
