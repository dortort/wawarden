package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	actionPair       = "pair"
	actionReconnect  = "reconnect"
	actionBackfill   = "backfill"
	actionMediaPause = "media_pause"
	chatRefChars     = 32
	downloadsOff     = "off"
	downloadsOn      = "on"
	downloadsPaused  = "paused"
	maxBackfillCount = 50
	outcomeOK        = "ok"
	warningAllChats  = "all_chats_client"
	warningMedia     = "media_write_client"

	// The listeners' 30-second write timeout would cut off a slower answer and lose the code.
	pairTimeout = 25 * time.Second
)

var (
	ErrAlreadyPaired     error = &codedError{status: http.StatusConflict, code: "already_paired"}
	ErrAlreadyConnected  error = &codedError{status: http.StatusConflict, code: "already_connected"}
	ErrNotPaired         error = &codedError{status: http.StatusConflict, code: "not_paired"}
	ErrOwnerPhoneMissing error = &codedError{status: http.StatusConflict, code: "owner_phone_missing"}
	ErrOwnerMismatch     error = &codedError{status: http.StatusConflict, code: "owner_mismatch"}
	ErrRateLimited       error = &codedError{status: http.StatusTooManyRequests, code: codeRateLimited}
	ErrPairFailed        error = &codedError{status: http.StatusBadGateway, code: "pair_failed"}
	ErrEngineUnavailable error = &codedError{status: http.StatusServiceUnavailable, code: "engine_unavailable"}
	ErrBackfillNoAnchor  error = &codedError{status: http.StatusUnprocessableEntity, code: codeBackfillNoAnchor}
)

func BackfillQuota(wait time.Duration) error {
	return &codedError{status: http.StatusTooManyRequests, code: codeBackfillQuota, retryAfter: wait}
}

type AdminStatus struct {
	State            string
	Reason           string
	Paired           bool
	Chats            int64
	Messages         int64
	BlobsPending     int64
	BlobsQuarantined int64
	InboxBacklog     int64
	InboxQuarantined int64
	BackfillsToday   int64
	MediaRefsLive    int64
	MediaRefsExpired int64
	RevokeTombstones int64
	Downloads        bool
	MediaPaused      bool
	Clients          ClientCounts
	LastIngest       time.Time
	Version          string
}

type ClientCounts struct {
	Active           int64
	Expired          int64
	Revoked          int64
	AllChatsActive   int64
	MediaWriteActive int64
}

type AdminService interface {
	Status(ctx context.Context) (AdminStatus, error)
	Pair(ctx context.Context) (code string, err error)
	Reconnect(ctx context.Context) error
	Backfill(g policy.AdminGrant, ctx context.Context, chat policy.CanonicalChat, count int) (requestID string, err error)
	PauseMedia(paused bool)
	ClientService
}

type backfillRequest struct {
	Chat  string `json:"chat"`
	Count *int   `json:"count"`
}

type mediaPauseRequest struct {
	Paused *bool `json:"paused"`
}

type AdminEvents interface {
	AdminMutation(action, outcome string)
	AdminAuthFailure()
}

func registerAdmin(rt *router, svc AdminService, events AdminEvents) {
	rt.admin("GET /admin/v1/status", func(ctx context.Context, _ policy.AdminGrant, _ *Request) (dto.Response, error) {
		s, err := svc.Status(ctx)
		if err != nil {
			return nil, err
		}
		return statusResponse(s), nil
	})
	rt.admin("POST /admin/v1/pair", mutation(events, actionPair, func(ctx context.Context) (dto.Response, error) {
		ctx, cancel := context.WithTimeout(ctx, pairTimeout)
		defer cancel()
		code, err := svc.Pair(ctx)
		if err != nil {
			return nil, err
		}
		return dto.Pairing{Code: code}, nil
	}))
	rt.admin("POST /admin/v1/reconnect", mutation(events, actionReconnect, func(ctx context.Context) (dto.Response, error) {
		if err := svc.Reconnect(ctx); err != nil {
			return nil, err
		}
		return dto.Accepted{Status: "accepted"}, nil
	}))
	rt.admin("POST /admin/v1/backfill", decodedMutation(events, actionBackfill, maxBodyBytes, func(ctx context.Context, g policy.AdminGrant, _ *Request, body backfillRequest) (dto.Response, error) {
		count := maxBackfillCount
		if body.Count != nil {
			count = *body.Count
		}
		if count < 1 || count > maxBackfillCount {
			return nil, errBadBody
		}
		chat, err := backfillChat(ctx, svc, body.Chat)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, sendDeadline)
		defer cancel()
		id, err := svc.Backfill(g, ctx, chat, count)
		if err != nil {
			return nil, err
		}
		return dto.BackfillRequested{RequestID: id}, nil
	}))
	rt.admin("POST /admin/v1/media", decodedMutation(events, actionMediaPause, maxBodyBytes, func(_ context.Context, _ policy.AdminGrant, _ *Request, body mediaPauseRequest) (dto.Response, error) {
		if body.Paused == nil {
			return nil, errBadBody
		}
		svc.PauseMedia(*body.Paused)
		return dto.MediaPause{Paused: *body.Paused}, nil
	}))
	registerClients(rt, svc, events)
}

func backfillChat(ctx context.Context, svc ClientService, v string) (policy.CanonicalChat, error) {
	if chat, ok := policy.Normalize(v); ok {
		return chat, nil
	}
	if len(v) != chatRefChars || strings.Trim(v, "0123456789abcdef") != "" {
		return policy.CanonicalChat{}, errBadBody
	}
	chats, _, err := svc.Chats(ctx, v, chatListLimit)
	if err != nil {
		return policy.CanonicalChat{}, err
	}
	for _, c := range chats {
		if c.Ref == v {
			return c.Chat, nil
		}
	}
	return policy.CanonicalChat{}, errNotFound
}

func mutation(events AdminEvents, action string, run func(context.Context) (dto.Response, error)) func(context.Context, policy.AdminGrant, *Request) (dto.Response, error) {
	return decodedMutation(events, action, maxBodyBytes, func(ctx context.Context, _ policy.AdminGrant, _ *Request, _ struct{}) (dto.Response, error) {
		return run(ctx)
	})
}

func decodedMutation[T any](events AdminEvents, action string, limit int64, run func(context.Context, policy.AdminGrant, *Request, T) (dto.Response, error)) func(context.Context, policy.AdminGrant, *Request) (dto.Response, error) {
	return func(ctx context.Context, g policy.AdminGrant, r *Request) (resp dto.Response, err error) {
		result := codeInternal
		defer func() { events.AdminMutation(action, result) }()
		var body T
		if err = r.decodeJSON(&body, limit); err == nil {
			resp, err = run(ctx, g, r, body)
		}
		result = outcome(err)
		return resp, err
	}
}

func outcome(err error) string {
	if err == nil {
		return outcomeOK
	}
	if refusal, ok := errors.AsType[*codedError](err); ok {
		return refusal.code
	}
	return codeInternal
}

func statusResponse(s AdminStatus) dto.Status {
	warnings := []string{}
	if s.Clients.AllChatsActive > 0 {
		warnings = append(warnings, warningAllChats)
	}
	if s.Clients.MediaWriteActive > 0 {
		warnings = append(warnings, warningMedia)
	}
	downloads := downloadsOff
	switch {
	case s.Downloads && s.MediaPaused:
		downloads = downloadsPaused
	case s.Downloads:
		downloads = downloadsOn
	}
	var last *string
	if !s.LastIngest.IsZero() {
		at := s.LastIngest.UTC().Format(time.RFC3339)
		last = &at
	}
	return dto.Status{
		State:  s.State,
		Reason: s.Reason,
		Paired: s.Paired,
		Counts: dto.StatusCounts{
			Chats:            s.Chats,
			Messages:         s.Messages,
			BlobsPending:     s.BlobsPending,
			BlobsQuarantined: s.BlobsQuarantined,
			InboxBacklog:     s.InboxBacklog,
			InboxQuarantined: s.InboxQuarantined,
			BackfillsToday:   s.BackfillsToday,
			MediaRefsLive:    s.MediaRefsLive,
			MediaRefsExpired: s.MediaRefsExpired,
			RevokeTombstones: s.RevokeTombstones,
		},
		Clients: dto.StatusClients{
			Active:         s.Clients.Active,
			Expired:        s.Clients.Expired,
			Revoked:        s.Clients.Revoked,
			AllChatsActive: s.Clients.AllChatsActive,
		},
		Warnings:     warnings,
		Downloads:    downloads,
		LastIngestAt: last,
		Version:      s.Version,
	}
}
