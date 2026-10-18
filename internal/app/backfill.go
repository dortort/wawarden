package app

import (
	"context"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const backfillBookkeeping = 5 * time.Second

type backfillStore interface {
	ReserveBackfill(g policy.AdminGrant, ctx context.Context, in ingest.BackfillRequest) (ingest.BackfillTicket, error)
	BindBackfill(ctx context.Context, provisional, requestID string) error
	CancelBackfill(ctx context.Context, provisional string) error
}

type backfiller interface {
	NewMessageID() string
	Backfill(ctx context.Context, req engine.BackfillRequest) (string, error)
}

func requestBackfill(g policy.AdminGrant, ctx context.Context, store backfillStore, e backfiller, now time.Time, chat policy.CanonicalChat, count int,
	anchor api.BackfillAnchor) (string, error) {
	kind := ingest.AnchorNewest + 1
	switch anchor {
	case api.BackfillOldest:
		kind = ingest.AnchorOldest
	case api.BackfillNewest:
		kind = ingest.AnchorNewest
	}
	provisional := e.NewMessageID()
	ticket, err := store.ReserveBackfill(g, ctx, ingest.BackfillRequest{ID: provisional, Chat: chat, Count: count, Anchor: kind, Now: now})
	if quota, ok := errors.AsType[*ingest.QuotaError](err); ok {
		return "", api.BackfillQuota(quota.RetryAfter)
	}
	switch {
	case errors.Is(err, ingest.ErrNoAnchor):
		return "", api.ErrBackfillNoAnchor
	case err != nil:
		return "", err
	}
	id, err := e.Backfill(ctx, engine.BackfillRequest{Chat: ticket.Chat.JID(), AnchorID: ticket.Anchor.ID, AnchorFromMe: ticket.Anchor.FromMe,
		AnchorTime: ticket.Anchor.Timestamp, Count: ticket.Count})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backfillBookkeeping)
	defer cancel()
	if errors.Is(err, engine.ErrNotConnected) || errors.Is(err, engine.ErrSendBusy) {
		if cerr := store.CancelBackfill(ctx, ticket.ID); cerr != nil {
			return "", cerr
		}
		if errors.Is(err, engine.ErrSendBusy) {
			return "", api.RateLimited(engineBusyWait)
		}
		return "", api.ErrNotConnected
	}
	if id != "" {
		if berr := store.BindBackfill(ctx, ticket.ID, id); berr != nil {
			return "", berr
		}
	}
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, engine.ErrRecipientUnavailable), errors.Is(err, engine.ErrRejected):
		return "", api.ErrSendRejected
	}
	return "", api.ErrSendOutcomeUnknown
}
