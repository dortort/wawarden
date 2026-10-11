// Package sender sends one text message for a client that holds a write grant, at most once per idempotency key.
package sender

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	PacerBase   = 3 * time.Second
	PacerJitter = 2 * time.Second

	settleBudget       = 5 * time.Second
	settleRetries      = 3
	settleBackoff      = 100 * time.Millisecond
	reasonAckTimeout   = "ack_timeout"
	reasonLeaseExpired = "lease_expired"
)

var (
	ErrInProgress     = errors.New("sender: a request with this idempotency key is still running")
	ErrNotConnected   = errors.New("sender: the engine is not connected, nothing was sent")
	ErrEngineBusy     = errors.New("sender: another send holds the engine, nothing was sent")
	ErrRejected       = errors.New("sender: WhatsApp did not accept the message")
	ErrOutcomeUnknown = errors.New("sender: whether WhatsApp received the message is unknown")
	ErrUnrecorded     = errors.New("sender: the result of the send could not be stored")
)

type Key = ingest.Key

func ParseKey(s string) (Key, bool) { return ingest.ParseKey(s) }

type Rejection struct{ Code int }

func (r *Rejection) Error() string { return ErrRejected.Error() }
func (r *Rejection) Unwrap() error { return ErrRejected }

type Failure struct {
	Err        error
	RetryAfter time.Duration
	Replayed   bool
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

type Store interface {
	ReserveSend(g policy.WriteGrant, ctx context.Context, in ingest.SendReservation) (ingest.Reservation, error)
	SettleSend(g policy.WriteGrant, ctx context.Context, in ingest.Settlement) (ingest.SentRow, error)
	ReleaseSend(g policy.WriteGrant, ctx context.Context, key ingest.Key) error
	ChatTimer(ctx context.Context, chat policy.CanonicalChat) (time.Duration, error)
}

type Message struct {
	ID         string
	Chat       policy.CanonicalChat
	Text       string
	Quote      *Quote
	Expiration time.Duration
}

type Quote struct {
	ID          string
	Participant policy.CanonicalChat
	Text        string
}

type Ack struct {
	Sender policy.CanonicalChat
	At     time.Time
}

type Engine interface {
	Ready() bool
	NewMessageID() string
	Send(ctx context.Context, m Message) (Ack, error)
}

type Notifier interface {
	SendOutcomeUnknown(reason string)
	SendUnrecorded()
}

type Options struct {
	Store         Store
	Engine        Engine
	Notifier      Notifier
	Holders       func(ctx context.Context, now time.Time) (int, error)
	PerMinute     int
	GlobalPerHour int
	Now           func() time.Time
	Pacer         func() time.Duration
}

type Service struct{ o Options }

type Reply struct {
	ID     string
	Sender policy.CanonicalChat
	Text   string
}

type Request struct {
	Key     Key
	Chat    policy.CanonicalChat
	ChatRef string
	Text    string
	Reply   *Reply
}

type Result struct {
	Sent     ingest.SentRow
	Replayed bool
}

func New(o Options) *Service {
	if o.Store == nil || o.Engine == nil || o.Notifier == nil || o.Holders == nil {
		panic("sender: New needs a store, an engine, a notifier and a holder count")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Pacer == nil {
		o.Pacer = DefaultPacer
	}
	return &Service{o: o}
}

func DefaultPacer() time.Duration {
	return PacerBase + rand.N(PacerJitter) //nolint:gosec // G404: pacer jitter blurs the send rhythm and needs no cryptographic randomness
}

func (s *Service) Send(g policy.WriteGrant, ctx context.Context, req Request) (Result, error) {
	if !g.Valid() || !g.Allows(req.Chat) {
		return Result{}, ingest.ErrNotAllowed
	}
	now := s.o.Now()
	holders, err := s.o.Holders(ctx, now)
	if err != nil {
		return Result{}, err
	}
	timer, err := s.o.Store.ChatTimer(ctx, req.Chat)
	if err != nil {
		return Result{}, err
	}
	var target *ingest.ReplyTarget
	var quote *Quote
	if req.Reply != nil {
		target = &ingest.ReplyTarget{ID: req.Reply.ID, Sender: req.Reply.Sender}
		quote = &Quote{ID: req.Reply.ID, Participant: req.Reply.Sender, Text: req.Reply.Text}
	}
	res, err := s.o.Store.ReserveSend(g, ctx, ingest.SendReservation{
		Key: req.Key, Chat: req.Chat, Fingerprint: ingest.Fingerprint(req.ChatRef, req.Text, target), MsgID: s.o.Engine.NewMessageID(), Now: now,
		Limits:   ingest.SendLimits{PerMinute: s.o.PerMinute, GlobalPerHour: s.o.GlobalPerHour, Holders: holders},
		PacerGap: s.o.Pacer(), Ready: s.o.Engine.Ready,
	})
	if err != nil {
		return Result{}, err
	}
	switch res.Kind {
	case ingest.Reserved:
	case ingest.ReplayedDone:
		return Result{Sent: res.Sent, Replayed: true}, nil
	case ingest.InProgress:
		return Result{}, &Failure{Err: ErrInProgress, RetryAfter: res.RetryAfter}
	case ingest.LeaseExpired:
		s.o.Notifier.SendOutcomeUnknown(reasonLeaseExpired)
		return Result{}, &Failure{Err: ErrOutcomeUnknown, Replayed: true}
	case ingest.ReplayedFailed:
		if res.Code == ingest.CodeRejected {
			return Result{}, &Failure{Err: ErrRejected, Replayed: true}
		}
		return Result{}, &Failure{Err: ErrOutcomeUnknown, Replayed: true}
	default:
		return Result{}, errors.New("sender: the store answered a reservation of an unknown kind")
	}
	ack, err := s.o.Engine.Send(ctx, Message{ID: res.MsgID, Chat: req.Chat, Text: req.Text, Quote: quote, Expiration: timer})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleBudget)
	defer cancel()
	switch {
	case err == nil:
		return s.recordAck(g, ctx, req, res.MsgID, ack, timer, target)
	case errors.Is(err, ErrNotConnected), errors.Is(err, ErrEngineBusy):
		if rerr := s.o.Store.ReleaseSend(g, ctx, req.Key); rerr != nil {
			return Result{}, rerr
		}
		return Result{}, err
	case errors.Is(err, ErrRejected):
		code := 0
		if r, ok := errors.AsType[*Rejection](err); ok {
			code = r.Code
		}
		_, _ = s.settle(g, ctx, ingest.Settlement{Key: req.Key, Outcome: ingest.SendRejected, WACode: max(code, 0)})
		return Result{}, &Failure{Err: ErrRejected}
	}
	s.o.Notifier.SendOutcomeUnknown(reasonAckTimeout)
	_, _ = s.settle(g, ctx, ingest.Settlement{Key: req.Key, Outcome: ingest.SendUnknown})
	return Result{}, &Failure{Err: ErrOutcomeUnknown}
}

func (s *Service) recordAck(g policy.WriteGrant, ctx context.Context, req Request, id string, ack Ack, timer time.Duration, target *ingest.ReplyTarget) (Result, error) {
	m := ingest.Message{Chat: req.Chat, ID: id, Sender: ack.Sender, FromMe: true, Origin: ingest.OriginLive, Timestamp: ack.At,
		Ingested: s.o.Now(), Kind: ingest.KindText, Text: req.Text}
	if timer > 0 && !ack.At.IsZero() {
		m.ExpiresAt = ack.At.Add(timer)
	}
	row, err := s.settle(g, ctx, ingest.Settlement{Key: req.Key, Outcome: ingest.SendAcked, AckedAt: ack.At, Sent: m, Reply: target})
	switch {
	case err == nil:
		return Result{Sent: row}, nil
	case errors.Is(err, ingest.ErrRewritePending):
		return Result{Sent: ingest.SentRow{Chat: req.Chat, ID: id, Sender: ack.Sender}}, nil
	case errors.Is(err, ingest.ErrNotPending):
		return Result{}, &Failure{Err: ErrOutcomeUnknown}
	}
	return Result{}, ErrUnrecorded
}

func (s *Service) settle(g policy.WriteGrant, ctx context.Context, in ingest.Settlement) (ingest.SentRow, error) {
	var row ingest.SentRow
	var err error
	for attempt := range settleRetries + 1 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				s.o.Notifier.SendUnrecorded()
				return ingest.SentRow{}, err
			case <-time.After(settleBackoff << (attempt - 1)):
			}
		}
		in.Now = s.o.Now()
		row, err = s.o.Store.SettleSend(g, ctx, in)
		if err == nil || errors.Is(err, ingest.ErrRewritePending) {
			return row, err
		}
		if errors.Is(err, ingest.ErrNotPending) || errors.Is(err, ingest.ErrInvalid) || errors.Is(err, ingest.ErrNotAllowed) {
			break
		}
	}
	s.o.Notifier.SendUnrecorded()
	return ingest.SentRow{}, err
}
