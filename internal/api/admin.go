package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	actionPair      = "pair"
	actionReconnect = "reconnect"
	outcomeOK       = "ok"

	// The listeners' 30-second write timeout would cut off a slower answer and lose the code.
	pairTimeout = 25 * time.Second
)

var (
	ErrAlreadyPaired     error = &codedError{status: http.StatusConflict, code: "already_paired"}
	ErrAlreadyConnected  error = &codedError{status: http.StatusConflict, code: "already_connected"}
	ErrNotPaired         error = &codedError{status: http.StatusConflict, code: "not_paired"}
	ErrOwnerPhoneMissing error = &codedError{status: http.StatusConflict, code: "owner_phone_missing"}
	ErrOwnerMismatch     error = &codedError{status: http.StatusConflict, code: "owner_mismatch"}
	ErrRateLimited       error = &codedError{status: http.StatusTooManyRequests, code: "rate_limited"}
	ErrPairFailed        error = &codedError{status: http.StatusBadGateway, code: "pair_failed"}
	ErrEngineUnavailable error = &codedError{status: http.StatusServiceUnavailable, code: "engine_unavailable"}
)

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
	LastIngest       time.Time
	Version          string
}

type AdminService interface {
	Status(ctx context.Context) (AdminStatus, error)
	Pair(ctx context.Context) (code string, err error)
	Reconnect(ctx context.Context) error
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
}

func mutation(events AdminEvents, action string, run func(context.Context) (dto.Response, error)) func(context.Context, policy.AdminGrant, *Request) (dto.Response, error) {
	return func(ctx context.Context, _ policy.AdminGrant, r *Request) (dto.Response, error) {
		var empty struct{}
		if err := r.DecodeJSON(&empty); err != nil {
			events.AdminMutation(action, outcome(err))
			return nil, err
		}
		resp, err := run(ctx)
		events.AdminMutation(action, outcome(err))
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
		},
		LastIngestAt: last,
		Version:      s.Version,
	}
}
