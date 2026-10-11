package wa

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	ackTimeout   = 12 * time.Second
	sendDeadline = 20 * time.Second
	maxBackfill  = 50
)

type transmitter func(ctx context.Context, cli *whatsmeow.Client, to types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)

type peerTransmitter func(ctx context.Context, cli *whatsmeow.Client, msg *waE2E.Message) (whatsmeow.SendResponse, error)

func transmitMessage(ctx context.Context, cli *whatsmeow.Client, to types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return cli.SendMessage(ctx, to, msg, extra)
}

func transmitPeerMessage(ctx context.Context, cli *whatsmeow.Client, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	return cli.SendPeerMessage(ctx, msg)
}

type socketState interface {
	IsConnected() bool
	IsLoggedIn() bool
}

func usable(s socketState) bool { return s.IsConnected() && s.IsLoggedIn() }

func loggedIn(cli *whatsmeow.Client) bool { return usable(cli) }

func (c *Client) NewMessageID() string { return c.current().GenerateMessageID() }

func (c *Client) Send(ctx context.Context, req engine.SendRequest) (engine.Sent, error) {
	cli := c.current()
	own, ok := c.ready(ctx, cli, req.Chat)
	if !ok {
		return engine.Sent{}, engine.ErrNotConnected
	}
	to, ok := address(req.Chat, true)
	if !ok || req.ID == "" {
		return engine.Sent{}, &engine.RejectedError{}
	}
	msg, ok := outgoing(req)
	if !ok {
		return engine.Sent{}, &engine.RejectedError{}
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendDeadline)
	defer cancel()
	resp, err := c.transmit(sctx, cli, to, msg, whatsmeow.SendRequestExtra{ID: req.ID, Timeout: ackTimeout})
	switch {
	case err != nil:
		return engine.Sent{}, classify(resp, err)
	case resp.ID != req.ID:
		return engine.Sent{}, engine.ErrOutcomeUnknown
	}
	at := resp.Timestamp
	if at.IsZero() {
		at = c.now()
	}
	return engine.Sent{ID: req.ID, Chat: req.Chat, Sender: own, Timestamp: at}, nil
}

func (c *Client) Backfill(ctx context.Context, req engine.BackfillRequest) (string, error) {
	cli := c.current()
	if _, ok := c.ready(ctx, cli, req.Chat); !ok {
		return "", engine.ErrNotConnected
	}
	chat, ok := address(req.Chat, true)
	if !ok || req.AnchorID == "" || req.AnchorTime.IsZero() || req.Count < 1 || req.Count > maxBackfill {
		return "", &engine.RejectedError{}
	}
	msg := cli.BuildHistorySyncRequest(&types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: req.AnchorFromMe},
		ID:            req.AnchorID,
		Timestamp:     req.AnchorTime,
	}, req.Count)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendDeadline)
	defer cancel()
	resp, err := c.transmitPeer(sctx, cli, msg)
	if err != nil {
		return resp.ID, classify(resp, err)
	}
	return resp.ID, nil
}

func (c *Client) ready(ctx context.Context, cli *whatsmeow.Client, chat string) (string, bool) {
	if ctx.Err() != nil || !c.online(cli) {
		return "", false
	}
	own := cli.Store.GetJID().ToNonAD()
	if lid := cli.Store.GetLID().ToNonAD(); strings.HasSuffix(chat, "@"+types.HiddenUserServer) && lid.Server == types.HiddenUserServer && lid.User != "" {
		own = lid
	}
	if own.User == "" {
		return "", false
	}
	return own.String(), true
}

func address(key string, groups bool) (types.JID, bool) {
	chat, ok := policy.Normalize(key)
	if !ok || chat.JID() != key {
		return types.EmptyJID, false
	}
	var server string
	switch chat.Kind() {
	case policy.PhoneChat:
		server = types.DefaultUserServer
	case policy.LIDChat:
		server = types.HiddenUserServer
	case policy.GroupChat:
		if !groups {
			return types.EmptyJID, false
		}
		server = types.GroupServer
	default:
		return types.EmptyJID, false
	}
	user, _, _ := strings.Cut(key, "@")
	jid := types.NewJID(user, server)
	if jid.Device != 0 || jid.RawAgent != 0 || jid.String() != key {
		return types.EmptyJID, false
	}
	return jid, true
}

func outgoing(req engine.SendRequest) (*waE2E.Message, bool) {
	if req.Text == "" {
		return nil, false
	}
	if req.Quote == nil && req.Expiration == 0 {
		return &waE2E.Message{Conversation: proto.String(req.Text)}, true
	}
	info := &waE2E.ContextInfo{}
	if q := req.Quote; q != nil {
		participant, ok := address(q.Participant, false)
		if !ok || q.ID == "" || q.Text == "" {
			return nil, false
		}
		info.StanzaID = proto.String(q.ID)
		info.Participant = proto.String(participant.String())
		info.QuotedMessage = &waE2E.Message{Conversation: proto.String(q.Text)}
	}
	if req.Expiration != 0 {
		secs := req.Expiration / time.Second
		if secs < 1 || secs > math.MaxUint32 {
			return nil, false
		}
		info.Expiration = proto.Uint32(uint32(secs))
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(req.Text), ContextInfo: info}}, true
}

func classify(resp whatsmeow.SendResponse, err error) error {
	written := resp.DebugTimings.Resp != 0 || resp.DebugTimings.Retry != 0
	_, disconnected := errors.AsType[*whatsmeow.DisconnectedError](err)
	switch {
	case errors.Is(err, whatsmeow.ErrServerReturnedError):
		return &engine.RejectedError{Code: serverCode(err)}
	case written, errors.Is(err, whatsmeow.ErrMessageTimedOut), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return engine.ErrOutcomeUnknown
	case errors.Is(err, whatsmeow.ErrClientIsNil), errors.Is(err, whatsmeow.ErrNotLoggedIn), errors.Is(err, whatsmeow.ErrNotConnected),
		errors.Is(err, socket.ErrSocketClosed), disconnected:
		return engine.ErrNotConnected
	case errors.Is(err, whatsmeow.ErrNotInGroup), errors.Is(err, whatsmeow.ErrGroupNotFound):
		return engine.ErrRecipientUnavailable
	case errors.Is(err, whatsmeow.ErrNoSession), errors.Is(err, whatsmeow.ErrUnknownServer), errors.Is(err, whatsmeow.ErrRecipientADJID):
		return &engine.RejectedError{}
	}
	return engine.ErrOutcomeUnknown
}

func serverCode(err error) int {
	_, digits, ok := strings.Cut(err.Error(), whatsmeow.ErrServerReturnedError.Error()+" ")
	if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0
	}
	code, err := strconv.Atoi(digits)
	if err != nil || code > math.MaxUint16 {
		return 0
	}
	return code
}
