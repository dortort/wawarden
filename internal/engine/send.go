package engine

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

var (
	ErrNotConnected         = errors.New("engine: not connected, nothing was sent")
	ErrRecipientUnavailable = errors.New("engine: the recipient cannot be reached from this account")
	ErrRejected             = errors.New("engine: WhatsApp rejected the message")
	ErrOutcomeUnknown       = errors.New("engine: whether WhatsApp received the message is unknown")
	ErrSendBusy             = errors.New("engine: another send is in progress")
)

type RejectedError struct{ Code int }

func (e *RejectedError) Error() string {
	if e.Code == 0 {
		return ErrRejected.Error()
	}
	return ErrRejected.Error() + " with code " + strconv.Itoa(e.Code)
}

func (e *RejectedError) Unwrap() error { return ErrRejected }

func RejectionCode(err error) int {
	if r, ok := errors.AsType[*RejectedError](err); ok {
		return r.Code
	}
	return 0
}

type SendRequest struct {
	ID         string
	Chat       string
	Text       string
	Quote      *SendQuote
	Expiration time.Duration
}

type SendQuote struct {
	ID          string
	Participant string
	Text        string
}

type Sent struct {
	ID        string
	Chat      string
	Sender    string
	Timestamp time.Time
}

type BackfillRequest struct {
	Chat         string
	AnchorID     string
	AnchorFromMe bool
	AnchorTime   time.Time
	Count        int
}

func (e *Engine) Ready() bool {
	st := e.sup.status()
	return st.State == StateConnected && st.Paired && e.sup.accepting()
}

func (e *Engine) NewMessageID() string { return e.sup.client.NewMessageID() }

func (e *Engine) Send(ctx context.Context, req SendRequest) (Sent, error) {
	var sent Sent
	err := e.exclusive("engine.send", func() error {
		var err error
		sent, err = e.sup.client.Send(ctx, req)
		return err
	})
	if err != nil {
		return Sent{}, err
	}
	return sent, nil
}

func (e *Engine) Backfill(ctx context.Context, req BackfillRequest) (string, error) {
	var id string
	err := e.exclusive("engine.backfill", func() error {
		var err error
		id, err = e.sup.client.Backfill(ctx, req)
		return err
	})
	return id, err
}

func (e *Engine) exclusive(name string, fn func() error) error {
	if !e.Ready() {
		return ErrNotConnected
	}
	if !e.sending.CompareAndSwap(false, true) {
		return ErrSendBusy
	}
	defer e.sending.Store(false)
	err := guarded(name, fn)
	switch {
	case err == nil, errors.Is(err, ErrNotConnected), errors.Is(err, ErrRecipientUnavailable), errors.Is(err, ErrRejected), errors.Is(err, ErrOutcomeUnknown):
		return err
	}
	return ErrOutcomeUnknown
}

func (p *pipeline) identity(id Identity) bool {
	pn, okPN := policy.Normalize(id.PN)
	lid, okLID := policy.Normalize(id.LID)
	if !okPN || !okLID || pn.Kind() != policy.PhoneChat || lid.Kind() != policy.LIDChat {
		p.counts.dropped.With(dropInvalid).Inc()
		return true
	}
	if !p.owner.Valid() || pn != p.owner {
		p.counts.dropped.With(dropOwnerUnknown).Inc()
		return true
	}
	var out outcome
	if err := p.write(p.ctx, "engine.identity", func(tx *ingest.Tx) error {
		a := &applier{tx: tx, owner: p.owner, now: p.clock.Now()}
		err := a.mapping(lid, pn, ingest.MappingSelf)
		out = a.out
		return err
	}); err != nil {
		p.counts.refused.With(refusedStore).Inc()
		return false
	}
	p.record(out)
	return true
}
