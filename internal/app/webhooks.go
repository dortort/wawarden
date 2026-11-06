package app

import (
	"context"
	"errors"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/hooks"
	"github.com/dortort/wawarden/internal/store/admin"
)

// webhookService serves a client's webhook routes from the archive and tells the dispatcher of every change.
type webhookService struct {
	store   *admin.Webhooks
	changed func()
}

func (s webhookService) Create(ctx context.Context, client string, spec hooks.Spec) (api.WebhookView, string, error) {
	v, secret, err := s.store.Create(ctx, client, spec)
	if err != nil {
		return api.WebhookView{}, "", webhookError(err)
	}
	s.changed()
	return webhookView(v), string(hooks.SigningKey(secret)), nil
}

func (s webhookService) List(ctx context.Context, client string) ([]api.WebhookView, error) {
	views, err := s.store.List(ctx, client)
	if err != nil {
		return nil, err
	}
	out := make([]api.WebhookView, 0, len(views))
	for _, v := range views {
		out = append(out, webhookView(v))
	}
	return out, nil
}

func (s webhookService) Get(ctx context.Context, client, id string) (api.WebhookView, error) {
	v, err := s.store.Get(ctx, client, id)
	if err != nil {
		return api.WebhookView{}, webhookError(err)
	}
	return webhookView(v), nil
}

func (s webhookService) Delete(ctx context.Context, client, id string) error {
	if err := s.store.Delete(ctx, client, id); err != nil {
		return webhookError(err)
	}
	s.changed()
	return nil
}

func webhookError(err error) error {
	if errors.Is(err, admin.ErrWebhookNotFound) || errors.Is(err, admin.ErrNotFound) {
		return api.ErrWebhookNotFound
	}
	return err
}

func webhookView(v admin.Webhook) api.WebhookView {
	return api.WebhookView{
		ID: v.ID, Spec: v.Spec, CreatedAt: v.CreatedAt, Delivered: v.Delivered, Failed: v.Failed,
		LastDelivery: v.LastDelivery, LastFailure: v.LastFailure, Failure: v.Failure,
	}
}
