package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sanitize"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	actionSend = "rest.send"

	headerIdempotencyKey = "Idempotency-Key"
	headerReplayed       = "Idempotent-Replayed"

	// The listeners' 30-second write timeout must leave room for the settlement and the audit row after the engine call.
	sendDeadline = 25 * time.Second

	minKeyBytes      = 8
	maxKeyBytes      = 128
	keyPattern       = "^[\\x21-\\x7e]{8,128}$"
	maxOutboundChars = 4096
	maxSendBodyBytes = 64 << 10
	reasonScope      = "scope"
	notConnected     = 5 * time.Second
)

var (
	ErrNotFound           error = errNotFound
	ErrBusy               error = errBusy
	ErrKeyReused          error = &codedError{status: http.StatusUnprocessableEntity, code: codeKeyReused}
	ErrFirstContact       error = &codedError{status: http.StatusForbidden, code: codeFirstContact}
	ErrNotConnected       error = &codedError{status: http.StatusServiceUnavailable, code: codeNotConnected, retryAfter: notConnected}
	ErrSendRejected       error = &codedError{status: http.StatusBadGateway, code: codeSendRejected}
	ErrSendOutcomeUnknown error = &codedError{status: http.StatusGatewayTimeout, code: codeSendOutcomeUnknown}

	errBadKey     = &codedError{status: http.StatusBadRequest, code: codeInvalidKey}
	errBadText    = &codedError{status: http.StatusBadRequest, code: codeInvalidText}
	errNoReadBack = errors.New("api: the sent message is not in the archive")
)

func RateLimited(wait time.Duration) error {
	return &codedError{status: http.StatusTooManyRequests, code: codeRateLimited, retryAfter: wait}
}

func InProgress(wait time.Duration) error {
	return &codedError{status: http.StatusConflict, code: codeInProgress, retryAfter: wait}
}

type SendService interface {
	Send(g policy.WriteGrant, ctx context.Context, req SendRequest) (Sent, error)
}

type SendRequest struct {
	Key     string
	Chat    policy.CanonicalChat
	ChatRef string
	Text    string
	Reply   *SendReply
}

type SendReply struct {
	ID     string
	Sender policy.CanonicalChat
	Seq    int64
	Text   string
}

// Sent reports a replay on a stored failure as well, beside the error.
type Sent struct {
	Chat     policy.CanonicalChat
	ID       string
	Sender   policy.CanonicalChat
	Replayed bool
}

func WellFormedKey(key string) bool {
	if len(key) < minKeyBytes || len(key) > maxKeyBytes {
		return false
	}
	for i := range len(key) {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

type writes struct {
	reads    *reads
	sender   SendService
	rejected func(reason string)
	now      func() time.Time
}

type writeHandler func(context.Context, policy.WriteGrant, *Request) (dto.Response, policy.CanonicalChat, error)

type replayedError struct{ error }

func (e replayedError) Unwrap() error { return e.error }

type sendBody struct {
	Text    *string `json:"text"`
	ReplyTo *string `json:"reply_to"`
}

func registerWrites(rt *router, w *writes) {
	rt.write("POST /v1/chats/{ref}/messages", notedWrite(actionSend, w.send))
}

func notedWrite(action string, h writeHandler) func(context.Context, policy.WriteGrant, *Request) (dto.Response, error) {
	return func(ctx context.Context, g policy.WriteGrant, r *Request) (dto.Response, error) {
		n := noteFrom(ctx)
		if n != nil {
			n.action = action
		}
		resp, chat, err := h(ctx, g, r)
		if errors.Is(err, scoped.ErrBusy) {
			err = errBusy
		}
		if n != nil {
			n.chat = chat
			if refusal, ok := errors.AsType[*codedError](err); ok {
				n.reason = refusal.code
			}
		}
		return resp, err
	}
}

func (w *writes) send(ctx context.Context, g policy.WriteGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	var body sendBody
	if err := r.decodeJSON(&body, maxSendBodyBytes); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	if body.Text == nil {
		return nil, policy.CanonicalChat{}, errBadBody
	}
	keys := r.req.Header.Values(headerIdempotencyKey)
	if len(keys) != 1 || !WellFormedKey(keys[0]) {
		return nil, policy.CanonicalChat{}, errBadKey
	}
	res, chat, err := w.sendText(ctx, g, r.req.PathValue("ref"), *body.Text, body.ReplyTo, keys[0])
	if res.Replayed {
		r.w.Header().Set(headerReplayed, "true")
	}
	if err != nil {
		return nil, chat, err
	}
	return res, chat, nil
}

func (w *writes) sendText(ctx context.Context, g policy.WriteGrant, ref, text string, replyTo *string, key string) (dto.SendResult, policy.CanonicalChat, error) {
	none := policy.CanonicalChat{}
	if !sanitize.Outbound(text) {
		return dto.SendResult{}, none, errBadText
	}
	rg, ok := policy.DecideRead(clientFrom(ctx), w.now())
	if !ok {
		return dto.SendResult{}, none, errNotFound
	}
	c, found, err := w.reads.archive.Chat(rg, ctx, ref)
	if err := missing(found, err); err != nil {
		return dto.SendResult{}, none, err
	}
	if !g.Allows(c.Chat) {
		w.rejected(reasonScope)
		return dto.SendResult{}, none, errNotFound
	}
	var reply *SendReply
	if replyTo != nil {
		if reply, err = w.replyTarget(ctx, rg, c.Chat, *replyTo); err != nil {
			return dto.SendResult{}, none, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, sendDeadline)
	defer cancel()
	sent, err := w.sender.Send(g, ctx, SendRequest{Key: key, Chat: c.Chat, ChatRef: c.Ref, Text: text, Reply: reply})
	if err != nil {
		if sent.Replayed {
			err = replayedError{err}
		}
		return dto.SendResult{Replayed: sent.Replayed}, c.Chat, err
	}
	m, found, err := w.reads.archive.Message(rg, context.WithoutCancel(ctx), sent.Chat, sent.ID, sent.Sender)
	switch {
	case err != nil:
		return dto.SendResult{Replayed: sent.Replayed}, c.Chat, err
	case !found:
		return dto.SendResult{Replayed: sent.Replayed}, c.Chat, errNoReadBack
	}
	item, err := w.reads.messageItem(g.Client(), m)
	if err != nil {
		return dto.SendResult{Replayed: sent.Replayed}, c.Chat, err
	}
	return dto.SendResult{Message: &item, Replayed: sent.Replayed, Session: w.reads.state()}, c.Chat, nil
}

func (w *writes) replyTarget(ctx context.Context, rg policy.ReadGrant, chat policy.CanonicalChat, mref string) (*SendReply, error) {
	ref, err := w.reads.refs.OpenRef(rg.Client(), mref)
	if err != nil {
		return nil, errNotFound
	}
	refChat, chatOK := policy.Normalize(ref.Chat)
	sender, senderOK := policy.Normalize(ref.Sender)
	if !chatOK || !senderOK {
		return nil, errNotFound
	}
	m, found, err := w.reads.archive.Message(rg, ctx, refChat, ref.ID, sender)
	if err := missing(found, err); err != nil {
		return nil, err
	}
	if m.Chat != chat || m.Revoked || m.Text == "" {
		return nil, errNotFound
	}
	return &SendReply{ID: m.ID, Sender: m.Sender, Seq: m.Position.Seq, Text: m.Text}, nil
}

func replayed(err error) bool {
	_, ok := errors.AsType[replayedError](err)
	return ok
}

func retryAfterSeconds(err error) *int {
	refusal, ok := errors.AsType[*codedError](err)
	if !ok || refusal.retryAfter <= 0 {
		return nil
	}
	seconds := int(max((refusal.retryAfter+time.Second-1)/time.Second, 1))
	return &seconds
}
