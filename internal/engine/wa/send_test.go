package wa

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/session"
)

const (
	peerChat  = "15550100001@s.whatsapp.net"
	peerLIDs  = "100000000000001@lid"
	groupChat = "120363000000000001@g.us"
)

type wireCall struct {
	ctx   context.Context
	to    types.JID
	msg   *waE2E.Message
	extra whatsmeow.SendRequestExtra
	err   error
}

type fakeWire struct {
	mu    sync.Mutex
	calls []wireCall
	resp  whatsmeow.SendResponse
	err   error
}

func (w *fakeWire) transmit(ctx context.Context, _ *whatsmeow.Client, to types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, wireCall{ctx: ctx, to: to, msg: msg, extra: extra, err: ctx.Err()})
	resp := w.resp
	if resp.ID == "" && w.err == nil {
		resp.ID = extra.ID
	}
	return resp, w.err
}

func (w *fakeWire) peer(ctx context.Context, cli *whatsmeow.Client, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	return w.transmit(ctx, cli, cli.Store.GetJID().ToNonAD(), msg, whatsmeow.SendRequestExtra{Peer: true})
}

func (w *fakeWire) sent() []wireCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wireCall(nil), w.calls...)
}

func wiredRig(t *testing.T, device *store.Device) (*rig, *fakeWire) {
	t.Helper()
	r := newRig(t, device)
	w := &fakeWire{}
	r.c.transmit, r.c.transmitPeer = w.transmit, w.peer
	r.c.online = func(*whatsmeow.Client) bool { return true }
	r.c.now = func() time.Time { return epoch }
	return r, w
}

func TestRecipientsAreBuiltFromTheCanonicalKeyOnly(t *testing.T) {
	for _, tt := range []struct {
		key    string
		server string
	}{
		{peerChat, types.DefaultUserServer},
		{peerLIDs, types.HiddenUserServer},
		{groupChat, types.GroupServer},
		{"15550100001-1600000000@g.us", types.GroupServer},
	} {
		jid, ok := address(tt.key, true)
		if !ok || jid.Server != tt.server || jid.Device != 0 || jid.RawAgent != 0 || jid.String() != tt.key {
			t.Errorf("address(%q) = %#v, %v", tt.key, jid, ok)
		}
	}
	for _, key := range []string{
		"", "15550100001", "@s.whatsapp.net", "15550100001@", "15550100001@c.us", "15550100001:7@s.whatsapp.net", "15550100001.1:2@s.whatsapp.net",
		"100000000000001:3@lid", "+15550100001@s.whatsapp.net", "15550100001@S.WHATSAPP.NET", " 15550100001@s.whatsapp.net", "15550100001@s.whatsapp.net ",
		"a@b@s.whatsapp.net", "status@broadcast", "120363000000000002@newsletter", "15550100001@bot", "15550100001@hosted", "120363000000000001@g.us@g.us",
		"0x1@s.whatsapp.net", "1e5@lid",
	} {
		if jid, ok := address(key, true); ok {
			t.Errorf("address(%q) = %#v, want a refusal", key, jid)
		}
	}
	if _, ok := address(groupChat, false); ok {
		t.Error("a group passed as a quoted participant")
	}
	if jid, ok := address(peerLIDs, false); !ok || jid.String() != peerLIDs {
		t.Errorf("a LID participant = %#v, %v", jid, ok)
	}
}

func TestEveryCanonicalUserKeyRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a seeded generator makes the property test reproducible
	for range 2000 {
		digits := make([]byte, 1+rng.IntN(15))
		for i := range digits {
			digits[i] = "0123456789"[rng.IntN(10)]
		}
		for _, server := range []string{"s.whatsapp.net", "lid", "g.us"} {
			key := string(digits) + "@" + server
			jid, ok := address(key, true)
			if !ok || jid.String() != key || jid.User != string(digits) || jid.Device != 0 {
				t.Fatalf("address(%q) = %#v, %v", key, jid, ok)
			}
			parsed, err := types.ParseJID(key)
			if err != nil || parsed != jid {
				t.Fatalf("address(%q) = %#v, the library parses %#v, %v", key, jid, parsed, err)
			}
		}
	}
}

func TestOutgoingMessagesMatchTheirGoldenEncoding(t *testing.T) {
	quote := &engine.SendQuote{ID: "3EB0QUOTED", Participant: peerLIDs, Text: "synthetic quoted text"}
	for _, tt := range []struct {
		name   string
		req    engine.SendRequest
		want   *waE2E.Message
		golden string
	}{
		{
			name:   "plain text",
			req:    engine.SendRequest{ID: "X", Chat: peerChat, Text: "synthetic plain text"},
			want:   &waE2E.Message{Conversation: proto.String("synthetic plain text")},
			golden: "0a1473796e74686574696320706c61696e2074657874",
		},
		{
			name: "a reply",
			req:  engine.SendRequest{ID: "X", Chat: peerChat, Text: "synthetic reply", Quote: quote},
			want: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("synthetic reply"), ContextInfo: &waE2E.ContextInfo{
				StanzaID: proto.String("3EB0QUOTED"), Participant: proto.String(peerLIDs), QuotedMessage: &waE2E.Message{Conversation: proto.String("synthetic quoted text")},
			}}},
			golden: "324e0a0f73796e746865746963207265706c798a013a0a0a3345423051554f5445441213313030303030303030303030303031406c69641a170a1573796e7468657469632071756f7465642074657874",
		},
		{
			name: "a timer",
			req:  engine.SendRequest{ID: "X", Chat: peerChat, Text: "synthetic expiring text", Expiration: 7 * 24 * time.Hour},
			want: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("synthetic expiring text"), ContextInfo: &waE2E.ContextInfo{
				Expiration: proto.Uint32(604800),
			}}},
			golden: "32210a1773796e746865746963206578706972696e6720746578748a0105c80180f524",
		},
		{
			name: "a reply with a timer in a group",
			req: engine.SendRequest{ID: "X", Chat: groupChat, Text: "synthetic expiring reply", Expiration: 86400*time.Second + 999*time.Millisecond,
				Quote: &engine.SendQuote{ID: "3EB0QUOTED", Participant: "15550100002@s.whatsapp.net", Text: "synthetic quoted text"}},
			want: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("synthetic expiring reply"), ContextInfo: &waE2E.ContextInfo{
				StanzaID: proto.String("3EB0QUOTED"), Participant: proto.String("15550100002@s.whatsapp.net"),
				QuotedMessage: &waE2E.Message{Conversation: proto.String("synthetic quoted text")}, Expiration: proto.Uint32(86400),
			}}},
			golden: "32630a1873796e746865746963206578706972696e67207265706c798a01460a0a3345423051554f544544121a313535353031303030303240732e77686174736170702e6e65741a170a1573796e7468657469632071756f7465642074657874c80180a305",
		},
	} {
		got, ok := outgoing(tt.req)
		if !ok || !proto.Equal(got, tt.want) {
			t.Errorf("%s: %v, %v", tt.name, got, ok)
			continue
		}
		if got.GetExtendedTextMessage().GetContextInfo().GetRemoteJID() != "" {
			t.Errorf("%s names a remote chat", tt.name)
		}
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(got)
		if err != nil || hex.EncodeToString(encoded) != tt.golden {
			t.Errorf("%s encodes as %x, %v", tt.name, encoded, err)
		}
	}
}

func TestOutgoingMessagesRefuseWhatCannotBeSentFaithfully(t *testing.T) {
	quote := engine.SendQuote{ID: "3EB0QUOTED", Participant: peerLIDs, Text: "synthetic quoted text"}
	for name, change := range map[string]func(*engine.SendRequest){
		"no text":                        func(r *engine.SendRequest) { r.Text = "" },
		"a quote without an id":          func(r *engine.SendRequest) { r.Quote.ID = "" },
		"a quote without text":           func(r *engine.SendRequest) { r.Quote.Text = "" },
		"a quote without a participant":  func(r *engine.SendRequest) { r.Quote.Participant = "" },
		"a group as quoted participant":  func(r *engine.SendRequest) { r.Quote.Participant = groupChat },
		"a device as quoted participant": func(r *engine.SendRequest) { r.Quote.Participant = "15550100002:3@s.whatsapp.net" },
		"a timer under a second":         func(r *engine.SendRequest) { r.Expiration = time.Second - 1 },
		"a negative timer":               func(r *engine.SendRequest) { r.Expiration = -time.Hour },
		"a timer past 32 bits":           func(r *engine.SendRequest) { r.Expiration = (math.MaxUint32 + 1) * time.Second },
	} {
		q := quote
		req := engine.SendRequest{ID: "X", Chat: peerChat, Text: "synthetic", Quote: &q}
		change(&req)
		if m, ok := outgoing(req); ok {
			t.Errorf("%s was built: %v", name, m)
		}
	}
	if m, ok := outgoing(engine.SendRequest{Text: "synthetic", Expiration: math.MaxUint32 * time.Second}); !ok || m.GetExtendedTextMessage().GetContextInfo().GetExpiration() != math.MaxUint32 {
		t.Errorf("the longest timer = %v, %v", m, ok)
	}
}

func TestSendAddressesTheRequestAndNeverLetsTheCallerCancelTheLibrary(t *testing.T) {
	r, w := wiredRig(t, pairedDevice())
	w.resp = whatsmeow.SendResponse{ID: "3EB0SENT", Timestamp: epoch.Add(time.Minute), Sender: types.JID{User: "15550100099", Device: 4, Server: types.DefaultUserServer},
		Chat: types.NewJID("100000000000099", types.HiddenUserServer)}
	ctx, cancel := context.WithCancel(t.Context())
	r.c.transmit = func(c context.Context, cli *whatsmeow.Client, to types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		cancel()
		return w.transmit(c, cli, to, msg, extra)
	}
	start := time.Now()
	sent, err := r.c.Send(ctx, engine.SendRequest{ID: "3EB0SENT", Chat: peerChat, Text: "synthetic"})
	end := time.Now()
	if err != nil {
		t.Fatalf("Send = %v", err)
	}
	if want := (engine.Sent{ID: "3EB0SENT", Chat: peerChat, Sender: owner.String(), Timestamp: epoch.Add(time.Minute)}); sent != want {
		t.Fatalf("Sent = %+v, want %+v: the requested chat and the owner's own form, never the library's answer", sent, want)
	}
	calls := w.sent()
	if len(calls) != 1 {
		t.Fatalf("%d library calls", len(calls))
	}
	call := calls[0]
	if call.to != types.NewJID("15550100001", types.DefaultUserServer) || call.extra != (whatsmeow.SendRequestExtra{ID: "3EB0SENT", Timeout: 12 * time.Second}) {
		t.Fatalf("the library was called for %v with %+v", call.to, call.extra)
	}
	deadline, ok := call.ctx.Deadline()
	if !ok || deadline.Before(start.Add(20*time.Second)) || deadline.After(end.Add(20*time.Second)) {
		t.Fatalf("the library's context has deadline %v, %v after the call started", deadline.Sub(start), ok)
	}
	if call.err != nil || ctx.Err() == nil {
		t.Fatalf("the caller's cancellation reached the library: %v", call.err)
	}
}

func TestTheSentRowTakesTheOwnersFormThatDedupsAgainstHistory(t *testing.T) {
	r, w := wiredRig(t, pairedDevice())
	noLID := pairedDevice()
	noLID.LID = types.EmptyJID
	bare, bareWire := wiredRig(t, noLID)
	for _, tt := range []struct {
		r      *rig
		chat   string
		sender string
	}{
		{r, peerChat, "15550100009@s.whatsapp.net"},
		{r, peerLIDs, "100000000000009@lid"},
		{r, groupChat, "15550100009@s.whatsapp.net"},
		{bare, peerLIDs, "15550100009@s.whatsapp.net"},
	} {
		sent, err := tt.r.c.Send(t.Context(), engine.SendRequest{ID: "3EB0OWN", Chat: tt.chat, Text: "synthetic"})
		if err != nil || sent.Sender != tt.sender || sent.Chat != tt.chat || !sent.Timestamp.Equal(epoch) {
			t.Errorf("a send to %s = %+v, %v, want sender %s and the clock's time when the server named none", tt.chat, sent, err, tt.sender)
		}
	}
	if len(w.sent()) != 3 || len(bareWire.sent()) != 1 {
		t.Fatalf("library calls %d and %d", len(w.sent()), len(bareWire.sent()))
	}
	if got := w.sent()[1].to; got != types.NewJID("100000000000001", types.HiddenUserServer) {
		t.Fatalf("a LID chat was addressed as %v", got)
	}
}

func TestSendClassifiesEveryLibraryOutcome(t *testing.T) {
	written := whatsmeow.SendResponse{ID: "3EB0X", DebugTimings: whatsmeow.MessageDebugTimings{Send: time.Millisecond, Resp: time.Millisecond}}
	retried := whatsmeow.SendResponse{ID: "3EB0X", DebugTimings: whatsmeow.MessageDebugTimings{Send: time.Millisecond, Resp: time.Millisecond, Retry: time.Millisecond}}
	unsent := whatsmeow.SendResponse{ID: "3EB0X", DebugTimings: whatsmeow.MessageDebugTimings{Send: time.Millisecond}}
	unknown, notConnected, unavailable := engine.ErrOutcomeUnknown, engine.ErrNotConnected, engine.ErrRecipientUnavailable
	rejected := engine.ErrRejected
	for _, tt := range []struct {
		name string
		resp whatsmeow.SendResponse
		err  error
		want error
		code int
	}{
		{"a nil client", whatsmeow.SendResponse{}, whatsmeow.ErrClientIsNil, notConnected, 0},
		{"no device", whatsmeow.SendResponse{}, whatsmeow.ErrNotLoggedIn, notConnected, 0},
		{"no socket for the device list", unsent, fmt.Errorf("failed to get device list: %w", whatsmeow.ErrNotConnected), notConnected, 0},
		{"no socket for the message", unsent, fmt.Errorf("failed to send message node: %w", whatsmeow.ErrNotConnected), notConnected, 0},
		{"a closed frame socket", unsent, fmt.Errorf("failed to send message node: %w", socket.ErrSocketClosed), notConnected, 0},
		{"a query cut by a disconnect", unsent, fmt.Errorf("failed to get device list: %w", whatsmeow.ErrIQDisconnected), notConnected, 0},
		{"not a member", unsent, fmt.Errorf("failed to get group members: %w", whatsmeow.ErrNotInGroup), unavailable, 0},
		{"no such group", unsent, fmt.Errorf("failed to get group members: %w", whatsmeow.ErrGroupNotFound), unavailable, 0},
		{"a server error", written, fmt.Errorf("%w %d", whatsmeow.ErrServerReturnedError, 479), rejected, 479},
		{"a server error past 16 bits", written, fmt.Errorf("%w %d", whatsmeow.ErrServerReturnedError, 70000), rejected, 0},
		{"a server error without a number", written, whatsmeow.ErrServerReturnedError, rejected, 0},
		{"no session", unsent, fmt.Errorf("%w with synthetic", whatsmeow.ErrNoSession), rejected, 0},
		{"an unknown server", unsent, fmt.Errorf("%w x", whatsmeow.ErrUnknownServer), rejected, 0},
		{"a device recipient", whatsmeow.SendResponse{}, whatsmeow.ErrRecipientADJID, rejected, 0},
		{"no ack in time", unsent, whatsmeow.ErrMessageTimedOut, unknown, 0},
		{"a disconnect after the frame", written, &whatsmeow.DisconnectedError{Action: "message send"}, unknown, 0},
		{"a bare no-socket after the retry wrote the frame again", retried, whatsmeow.ErrNotConnected, unknown, 0},
		{"a closed socket on the retry", retried, socket.ErrSocketClosed, unknown, 0},
		{"the deadline in the ack wait", unsent, context.DeadlineExceeded, unknown, 0},
		{"the deadline inside the frame write", unsent, fmt.Errorf("failed to send message node: %w", context.DeadlineExceeded), unknown, 0},
		{"a cancellation", unsent, context.Canceled, unknown, 0},
		{"a failed write", unsent, fmt.Errorf("failed to send message node: %w", errors.New("synthetic broken pipe")), unknown, 0},
		{"a phone number not on WhatsApp", whatsmeow.SendResponse{ID: "3EB0X"}, errors.New("no LID found for 15550100001@s.whatsapp.net from server"), unknown, 0},
	} {
		r, w := wiredRig(t, pairedDevice())
		w.resp, w.err = tt.resp, tt.err
		sent, err := r.c.Send(t.Context(), engine.SendRequest{ID: "3EB0X", Chat: peerChat, Text: "synthetic"})
		if !errors.Is(err, tt.want) || engine.RejectionCode(err) != tt.code || sent != (engine.Sent{}) {
			t.Errorf("%s: Send = %+v, %v, want %v", tt.name, sent, err, tt.want)
		}
		if strings.Contains(err.Error(), "15550100001") || strings.Contains(err.Error(), "synthetic") {
			t.Errorf("%s: the error %q carries the library's text", tt.name, err)
		}
	}
	r, w := wiredRig(t, pairedDevice())
	w.resp = whatsmeow.SendResponse{ID: "3EB0OTHER"}
	if _, err := r.c.Send(t.Context(), engine.SendRequest{ID: "3EB0X", Chat: peerChat, Text: "synthetic"}); !errors.Is(err, unknown) {
		t.Fatalf("an answer for another id = %v", err)
	}
}

func TestSendRefusesBeforeTheLibrary(t *testing.T) {
	r, w := wiredRig(t, pairedDevice())
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.c.Send(ended, engine.SendRequest{ID: "3EB0X", Chat: peerChat, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Errorf("a send with an ended context = %v", err)
	}
	for name, req := range map[string]engine.SendRequest{
		"no id":              {Chat: peerChat, Text: "synthetic"},
		"no text":            {ID: "3EB0X", Chat: peerChat},
		"a device chat":      {ID: "3EB0X", Chat: "15550100001:2@s.whatsapp.net", Text: "synthetic"},
		"a broadcast":        {ID: "3EB0X", Chat: "status@broadcast", Text: "synthetic"},
		"a legacy phone key": {ID: "3EB0X", Chat: "15550100001@c.us", Text: "synthetic"},
		"a bad quote":        {ID: "3EB0X", Chat: peerChat, Text: "synthetic", Quote: &engine.SendQuote{ID: "3EB0Q", Participant: groupChat, Text: "q"}},
	} {
		if _, err := r.c.Send(t.Context(), req); !errors.Is(err, engine.ErrRejected) || engine.RejectionCode(err) != 0 {
			t.Errorf("%s: Send = %v, want a rejection without a server number", name, err)
		}
	}
	if len(w.sent()) != 0 {
		t.Fatalf("refused requests reached the library: %+v", w.sent())
	}

	plain, plainWire := wiredRig(t, pairedDevice())
	plain.c.online = loggedIn
	if _, err := plain.c.Send(t.Context(), engine.SendRequest{ID: "3EB0X", Chat: peerChat, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Errorf("a send on a client without a socket = %v", err)
	}
	if _, err := plain.c.Backfill(t.Context(), engine.BackfillRequest{Chat: peerChat, AnchorID: "3EB0A", AnchorTime: epoch, Count: 50}); !errors.Is(err, engine.ErrNotConnected) {
		t.Errorf("a backfill on a client without a socket = %v", err)
	}
	unpaired, unpairedWire := wiredRig(t, &store.Device{})
	if _, err := unpaired.c.Send(t.Context(), engine.SendRequest{ID: "3EB0X", Chat: peerChat, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Errorf("a send without an own identity = %v", err)
	}
	if len(plainWire.sent()) != 0 || len(unpairedWire.sent()) != 0 {
		t.Fatal("the pre-check let a call through")
	}
}

type socketDouble struct{ connected, loggedIn bool }

func (s socketDouble) IsConnected() bool { return s.connected }
func (s socketDouble) IsLoggedIn() bool  { return s.loggedIn }

func TestThePreCheckNeedsAnOpenSocketThatIsLoggedIn(t *testing.T) {
	for _, s := range []socketDouble{{}, {connected: true}, {loggedIn: true}} {
		if usable(s) {
			t.Errorf("%+v passed the pre-check", s)
		}
	}
	if s := (socketDouble{connected: true, loggedIn: true}); !usable(s) {
		t.Errorf("%+v failed the pre-check", s)
	}
}

func TestBackfillAsksThePrimaryDeviceForHistoryBeforeTheAnchor(t *testing.T) {
	r, w := wiredRig(t, pairedDevice())
	w.resp = whatsmeow.SendResponse{ID: "3EB0REQUEST"}
	anchor := time.Date(2026, 9, 1, 8, 30, 15, 0, time.UTC)
	id, err := r.c.Backfill(t.Context(), engine.BackfillRequest{Chat: peerLIDs, AnchorID: "3EB0OLDEST", AnchorFromMe: true, AnchorTime: anchor, Count: 50})
	if err != nil || id != "3EB0REQUEST" {
		t.Fatalf("Backfill = %q, %v", id, err)
	}
	calls := w.sent()
	if len(calls) != 1 || calls[0].to != owner || !calls[0].extra.Peer {
		t.Fatalf("library calls %+v", calls)
	}
	if _, ok := calls[0].ctx.Deadline(); !ok {
		t.Fatal("the history request has no deadline")
	}
	pm := calls[0].msg.GetProtocolMessage()
	req := pm.GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
	if pm.GetType() != waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE || req.GetChatJID() != peerLIDs || req.GetOldestMsgID() != "3EB0OLDEST" ||
		!req.GetOldestMsgFromMe() || req.GetOnDemandMsgCount() != 50 || req.GetOldestMsgTimestampMS() != anchor.Unix() {
		t.Fatalf("the request is %v", calls[0].msg)
	}

	w.resp, w.err = whatsmeow.SendResponse{ID: "3EB0REQUEST", DebugTimings: whatsmeow.MessageDebugTimings{Resp: time.Millisecond}}, &whatsmeow.DisconnectedError{Action: "message send"}
	if id, err := r.c.Backfill(t.Context(), engine.BackfillRequest{Chat: groupChat, AnchorID: "3EB0OLDEST", AnchorTime: anchor, Count: 1}); id != "3EB0REQUEST" || !errors.Is(err, engine.ErrOutcomeUnknown) {
		t.Fatalf("an interrupted request = %q, %v, want its id with an unknown outcome", id, err)
	}

	for name, req := range map[string]engine.BackfillRequest{
		"no anchor":           {Chat: peerChat, AnchorTime: anchor, Count: 10},
		"no anchor time":      {Chat: peerChat, AnchorID: "3EB0A", Count: 10},
		"no messages":         {Chat: peerChat, AnchorID: "3EB0A", AnchorTime: anchor},
		"more than fifty":     {Chat: peerChat, AnchorID: "3EB0A", AnchorTime: anchor, Count: 51},
		"a device chat":       {Chat: "15550100001:2@s.whatsapp.net", AnchorID: "3EB0A", AnchorTime: anchor, Count: 10},
		"a newsletter":        {Chat: "120363000000000002@newsletter", AnchorID: "3EB0A", AnchorTime: anchor, Count: 10},
		"a negative quantity": {Chat: peerChat, AnchorID: "3EB0A", AnchorTime: anchor, Count: -1},
	} {
		if _, err := r.c.Backfill(t.Context(), req); !errors.Is(err, engine.ErrRejected) {
			t.Errorf("%s: Backfill = %v", name, err)
		}
	}
	if len(w.sent()) != 2 {
		t.Fatalf("refused history requests reached the library: %d calls", len(w.sent()))
	}
}

func TestMessageIDsAreTheLibrarysOwn(t *testing.T) {
	r := newRig(t, pairedDevice())
	seen := map[string]bool{}
	for range 100 {
		id := r.c.NewMessageID()
		if len(id) != 22 || !strings.HasPrefix(id, "3EB0") || strings.Trim(id, "0123456789ABCDEF") != "" || seen[id] {
			t.Fatalf("NewMessageID = %q", id)
		}
		seen[id] = true
	}
}

func TestTheOwnersIdentityFollowsTheConnection(t *testing.T) {
	r := newRig(t, pairedDevice())
	r.dispatch(&events.Connected{})
	got := r.events()
	if len(got) != 2 || got[0] != (engine.Connected{}) || got[1] != (engine.Identity{PN: "15550100009@s.whatsapp.net", LID: "100000000000009@lid"}) {
		t.Fatalf("events after a connection %#v", got)
	}
	r.setAck(false)
	if r.dispatch(&events.Connected{}) {
		t.Fatal("a refused Connected was acknowledged")
	}

	noLID := pairedDevice()
	noLID.LID = types.EmptyJID
	deviceLID := pairedDevice()
	deviceLID.LID = types.JID{User: ownerLID.User, Device: 12, Server: types.HiddenUserServer}
	for name, device := range map[string]*store.Device{"unpaired": {}, "without a LID": noLID} {
		r := newRig(t, device)
		r.dispatch(&events.Connected{})
		if got := r.events(); len(got) != 1 {
			t.Errorf("%s: events after a connection %#v", name, got)
		}
	}
	d := newRig(t, deviceLID)
	d.dispatch(&events.Connected{})
	if got := d.events(); len(got) != 2 || got[1] != (engine.Identity{PN: "15550100009@s.whatsapp.net", LID: "100000000000009@lid"}) {
		t.Fatalf("the identity keeps a device part: %#v", got)
	}
	d.dispatch(&events.Disconnected{})
	if got := d.events(); len(got) != 3 || got[2] != (engine.Disconnected{}) {
		t.Fatalf("events after a drop %#v", got)
	}
}

func storedDevice(t *testing.T) (*store.Device, *session.Store) {
	t.Helper()
	w, _ := newWriter()
	sess, err := session.Open(t.Context(), session.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: "local", Logger: logx.New(w, slog.LevelInfo)})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	device := sess.NewDevice()
	id := types.JID{User: owner.User, Device: 12, Server: types.DefaultUserServer}
	device.ID, device.LID = &id, ownerLID
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{1},
		AccountSignature:    bytes.Repeat([]byte{2}, 64),
		AccountSignatureKey: bytes.Repeat([]byte{3}, 32),
		DeviceSignature:     bytes.Repeat([]byte{4}, 64),
	}
	if err := device.Save(t.Context()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stored, err := sess.Device(t.Context())
	if err != nil || stored.ID == nil {
		t.Fatalf("the saved device was not read back: %v", err)
	}
	return stored, sess
}

func TestTheProductionCallsWriteNothingWithoutASocket(t *testing.T) {
	device, _ := storedDevice(t)
	r := newRig(t, device)
	cli := r.c.current()
	for _, chat := range []string{peerChat, peerLIDs, groupChat} {
		to, _ := address(chat, true)
		resp, err := transmitMessage(t.Context(), cli, to, &waE2E.Message{Conversation: proto.String("synthetic")}, whatsmeow.SendRequestExtra{ID: "3EB0OFFLINE", Timeout: ackTimeout})
		if !errors.Is(classify(resp, err), engine.ErrNotConnected) || resp.DebugTimings.Resp != 0 || resp.DebugTimings.Retry != 0 {
			t.Errorf("a send to %s without a socket = %v (%+v), want nothing written", chat, err, resp.DebugTimings)
		}
	}
	resp, err := transmitPeerMessage(t.Context(), cli, cli.BuildHistorySyncRequest(&types.MessageInfo{MessageSource: types.MessageSource{Chat: peer}, ID: "3EB0A", Timestamp: epoch}, 1))
	if err == nil || resp.DebugTimings.Send != 0 || resp.DebugTimings.Resp != 0 || resp.DebugTimings.Retry != 0 {
		t.Errorf("a history request without a socket = %v (%+v), want nothing written", err, resp.DebugTimings)
	}
}
