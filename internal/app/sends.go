package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sender"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	reasonFirstContact = "first_contact"
	reasonPacer        = "pacer"
	reasonRestart      = "restart"
	engineBusyWait     = time.Second
)

var errUnparsedKey = errors.New("app: the api passed an idempotency key the store does not accept")

type sendEngine struct{ engine *engine.Engine }

func (e sendEngine) Ready() bool          { return e.engine.Ready() }
func (e sendEngine) NewMessageID() string { return e.engine.NewMessageID() }

func (e sendEngine) Send(ctx context.Context, m sender.Message) (sender.Ack, error) {
	sent, err := e.engine.Send(ctx, engineRequest(m))
	if err != nil {
		return sender.Ack{}, senderError(err)
	}
	return ackOf(sent), nil
}

func engineRequest(m sender.Message) engine.SendRequest {
	req := engine.SendRequest{ID: m.ID, Chat: m.Chat.JID(), Text: m.Text, Expiration: m.Expiration}
	if m.Quote != nil {
		req.Quote = &engine.SendQuote{ID: m.Quote.ID, Participant: m.Quote.Participant.JID(), Text: m.Quote.Text}
	}
	return req
}

func ackOf(sent engine.Sent) sender.Ack {
	own, _ := policy.Normalize(sent.Sender)
	return sender.Ack{Sender: own, At: sent.Timestamp}
}

func senderError(err error) error {
	switch {
	case errors.Is(err, engine.ErrNotConnected):
		return sender.ErrNotConnected
	case errors.Is(err, engine.ErrSendBusy):
		return sender.ErrEngineBusy
	case errors.Is(err, engine.ErrRecipientUnavailable):
		return &sender.Rejection{}
	case errors.Is(err, engine.ErrRejected):
		return &sender.Rejection{Code: engine.RejectionCode(err)}
	}
	return sender.ErrOutcomeUnknown
}

type sendService struct {
	sender   *sender.Service
	rejected func(reason string)
}

func (s sendService) Send(g policy.WriteGrant, ctx context.Context, req api.SendRequest) (api.Sent, error) {
	key, ok := sender.ParseKey(req.Key)
	if !ok {
		return api.Sent{}, errUnparsedKey
	}
	in := sender.Request{Key: key, Chat: req.Chat, ChatRef: req.ChatRef, Text: req.Text}
	if req.Reply != nil {
		in.Reply = &sender.Reply{ID: req.Reply.ID, Sender: req.Reply.Sender, Seq: req.Reply.Seq, Text: req.Reply.Text}
	}
	res, err := s.sender.Send(g, ctx, in)
	if err != nil {
		failure, _ := errors.AsType[*sender.Failure](err)
		return api.Sent{Replayed: failure != nil && failure.Replayed}, s.answer(err)
	}
	return api.Sent{Chat: res.Sent.Chat, ID: res.Sent.ID, Sender: res.Sent.Sender, Replayed: res.Replayed}, nil
}

func (s sendService) answer(err error) error {
	if limited, ok := errors.AsType[*ingest.RateLimitError](err); ok {
		s.rejected(limited.Reason.String())
		return api.RateLimited(limited.RetryAfter)
	}
	if failure, ok := errors.AsType[*sender.Failure](err); ok && errors.Is(err, sender.ErrInProgress) {
		return api.InProgress(failure.RetryAfter)
	}
	switch {
	case errors.Is(err, ingest.ErrFirstContact):
		s.rejected(reasonFirstContact)
		return api.ErrFirstContact
	case errors.Is(err, sender.ErrEngineBusy):
		s.rejected(reasonPacer)
		return api.RateLimited(engineBusyWait)
	case errors.Is(err, ingest.ErrKeyReused):
		return api.ErrKeyReused
	case errors.Is(err, ingest.ErrNotReady), errors.Is(err, sender.ErrNotConnected):
		return api.ErrNotConnected
	case errors.Is(err, ingest.ErrNotAllowed):
		return api.ErrNotFound
	case errors.Is(err, sender.ErrRejected):
		return api.ErrSendRejected
	case errors.Is(err, sender.ErrOutcomeUnknown):
		return api.ErrSendOutcomeUnknown
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, admin.ErrUnavailable):
		return api.ErrBusy
	}
	return err
}

func newSender(cfg config.Config, archive *ingest.Store, e *engine.Engine, notifier sender.Notifier) *sender.Service {
	return sender.New(sender.Options{
		Store: archive, Engine: sendEngine{engine: e}, Notifier: notifier, Holders: archive.Clients().WriteHolders,
		PerMinute: cmp.Or(cfg.SendPerMinute, config.DefaultSendPerMinute), GlobalPerHour: cmp.Or(cfg.SendGlobal, config.DefaultSendGlobalPerHour),
	})
}

func warnUnsafeSendCaps(cfg config.Config, alerts *slog.Logger) {
	if cfg.UnsafeSendCaps {
		alerts.Warn("the upper caps of the send limits are lifted: clients can send to WhatsApp faster than the documented anti-ban limits",
			slog.String("event", "unsafe_send_caps"), slog.Int("send_global_per_hour", cfg.SendGlobal), slog.Int("send_per_client_per_minute", cfg.SendPerMinute))
	}
}
