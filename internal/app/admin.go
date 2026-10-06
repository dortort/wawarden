package app

import (
	"context"
	"errors"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
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
	n, err := s.archive.Clients().Counts(ctx)
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
		Clients:          api.ClientCounts{Active: n.Active, Expired: n.Expired, Revoked: n.Revoked, AllChatsActive: n.AllChatsActive},
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

func (s adminService) CreateClient(ctx context.Context, spec policy.ClientSpec) (api.ClientView, string, error) {
	c, credential, err := s.archive.Clients().Create(ctx, spec)
	if err != nil {
		return api.ClientView{}, "", err
	}
	return clientView(c), credential, nil
}

func (s adminService) Clients(ctx context.Context) ([]api.ClientView, error) {
	list, err := s.archive.Clients().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.ClientView, 0, len(list))
	for _, c := range list {
		out = append(out, clientView(c))
	}
	return out, nil
}

func (s adminService) Client(ctx context.Context, id string) (api.ClientView, error) {
	c, err := s.archive.Clients().Get(ctx, id)
	if err != nil {
		return api.ClientView{}, clientError(err)
	}
	return clientView(c), nil
}

func (s adminService) RevokeClient(ctx context.Context, id string) (api.ClientView, error) {
	c, err := s.archive.Clients().Revoke(ctx, id)
	if err != nil {
		return api.ClientView{}, clientError(err)
	}
	return clientView(c), nil
}

func (s adminService) Chats(ctx context.Context, match string, limit int) ([]api.ChatEntry, bool, error) {
	chats, truncated, err := s.archive.Admin().Chats(ctx, match, limit)
	if err != nil {
		return nil, false, err
	}
	out := make([]api.ChatEntry, 0, len(chats))
	for _, c := range chats {
		out = append(out, api.ChatEntry{Chat: c.Chat, Ref: c.Ref, Name: c.Name})
	}
	return out, truncated, nil
}

func clientError(err error) error {
	if errors.Is(err, admin.ErrNotFound) {
		return api.ErrClientNotFound
	}
	return err
}

func clientView(c admin.Client) api.ClientView {
	chats := func(in []admin.ClientChat) []api.ClientChatView {
		out := make([]api.ClientChatView, 0, len(in))
		for _, cc := range in {
			out = append(out, api.ClientChatView{Chat: cc.Chat, Known: cc.Known, Name: cc.Name})
		}
		return out
	}
	return api.ClientView{
		ID: c.ID, Name: c.Name, State: c.State, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, RevokedAt: c.RevokedAt,
		AllChats: c.AllChats, AllowFirstContact: c.AllowFirstContact, ReadCount: c.ReadCount, WriteCount: c.WriteCount,
		Read: chats(c.Read), Write: chats(c.Write),
	}
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
