//go:build dev

package fake

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/engine"
)

const (
	ownerPhone = "+15550100009"
	ownerJID   = "15550100009:7@s.whatsapp.net"
	historyCap = 1 << 20
)

func newClient(t *testing.T, wrong bool) (*Client, *recorder) {
	t.Helper()
	c, err := New(Options{OwnerPhone: ownerPhone, WrongAccount: wrong, HistoryMaxBytes: historyCap, Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := &recorder{events: make(chan engine.Event, 1024), refuse: map[string]int{}}
	t.Cleanup(c.OnEvent(r.handle))
	t.Cleanup(c.Disconnect)
	return c, r
}

type recorder struct {
	events chan engine.Event
	refuse map[string]int
}

func (r *recorder) handle(ev engine.Event) bool {
	r.events <- ev
	if m, ok := ev.(engine.Message); ok && r.refuse[m.ID] > 0 {
		r.refuse[m.ID]--
		return false
	}
	return true
}

func (r *recorder) next(t *testing.T) engine.Event {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event within ten seconds")
		return nil
	}
}

func (r *recorder) quiet(t *testing.T) {
	t.Helper()
	select {
	case ev := <-r.events:
		t.Fatalf("unexpected event %#v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func pair(t *testing.T, c *Client, r *recorder) {
	t.Helper()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	r.quiet(t)
	code, err := c.PairPhone(t.Context(), "15550100009")
	if err != nil || code != pairingCode {
		t.Fatalf("PairPhone = %q, %v", code, err)
	}
}

func TestMarkerIsInDevBinaries(t *testing.T) {
	scanned := strings.Join([]string{"wawarden", "fake", "engine", "not", "for", "release"}, "-")
	if marker != scanned {
		t.Fatalf("marker = %q, want %q", marker, scanned)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the running test binary, from os.Executable
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if !bytes.Contains(data, []byte(scanned)) {
		t.Fatal("a binary holding the fake engine does not contain its marker")
	}
}

func TestNewRefusesUnusableOptions(t *testing.T) {
	for _, opts := range []Options{
		{OwnerPhone: ownerPhone},
		{OwnerPhone: ownerPhone, HistoryMaxBytes: engine.MaxHistoryMaxBytes + 1},
		{OwnerPhone: "15550100009", HistoryMaxBytes: historyCap},
		{OwnerPhone: "+15550100021", HistoryMaxBytes: historyCap},
		{OwnerPhone: "+15550100029", HistoryMaxBytes: historyCap},
	} {
		if _, err := New(opts); !errors.Is(err, errOptions) {
			t.Fatalf("New(%+v) = %v, want a refusal", opts, err)
		}
	}
	if _, err := New(Options{HistoryMaxBytes: historyCap}); err != nil {
		t.Fatalf("New without an owner: %v", err)
	}
}

func TestPairingRefusesAnotherNumberAndNeedsAConnection(t *testing.T) {
	c, r := newClient(t, false)
	if _, err := c.PairPhone(t.Context(), "15550100009"); !errors.Is(err, errOffline) {
		t.Fatalf("PairPhone before Connect = %v", err)
	}
	if err := c.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.PairPhone(t.Context(), "15550100008"); !errors.Is(err, errNotOwner) {
		t.Fatalf("PairPhone with another number = %v", err)
	}
	r.quiet(t)
	if c.Paired() || c.Account() != "" {
		t.Fatal("a refused pairing paired the client")
	}
}

func TestPairsTheOwnerAndPlaysTheScriptAcrossADrop(t *testing.T) {
	c, r := newClient(t, false)
	pair(t, c, r)
	if p, ok := r.next(t).(engine.Paired); !ok || p.JID != ownerJID {
		t.Fatalf("first event %#v, want Paired for the owner", p)
	}
	if !c.Paired() || c.Account() != "15550100009" {
		t.Fatalf("Paired %v, Account %q", c.Paired(), c.Account())
	}
	if _, ok := r.next(t).(engine.Connected); !ok {
		t.Fatal("pairing was not followed by Connected")
	}
	var played []engine.Event
	for {
		ev := r.next(t)
		played = append(played, ev)
		if _, ok := ev.(engine.Disconnected); ok {
			break
		}
	}
	r.quiet(t)
	if err := c.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, ok := r.next(t).(engine.Connected); !ok {
		t.Fatal("a reconnection did not start with Connected")
	}
	for len(played) < len(c.script) {
		played = append(played, r.next(t))
	}
	r.quiet(t)
	for i, ev := range played {
		if !sameEvent(ev, c.script[i]) {
			t.Fatalf("event %d = %#v, want %#v", i, ev, c.script[i])
		}
		if m, ok := ev.(engine.Message); ok && m.Timestamp.IsZero() {
			t.Fatalf("message %s has no timestamp", m.ID)
		}
	}
}

func sameEvent(a, b engine.Event) bool {
	switch x := a.(type) {
	case engine.Message:
		y, ok := b.(engine.Message)
		return ok && x.ID == y.ID && x.Chat == y.Chat && x.Kind == y.Kind
	case engine.Group:
		y, ok := b.(engine.Group)
		return ok && x.Chat == y.Chat && x.Subject == y.Subject
	case engine.HistoryNotification:
		y, ok := b.(engine.HistoryNotification)
		return ok && x.Ref.ID == y.Ref.ID && x.Sender == y.Sender
	}
	return a == b
}

func TestARefusedEventIsDeliveredAgain(t *testing.T) {
	c, r := newClient(t, false)
	r.refuse["FAKE-L01"] = 2
	pair(t, c, r)
	var ids []string
	for len(ids) < 4 {
		if m, ok := r.next(t).(engine.Message); ok {
			ids = append(ids, m.ID)
		}
	}
	if want := []string{"FAKE-L01", "FAKE-L01", "FAKE-L01", "FAKE-L02"}; !slices.Equal(ids, want) {
		t.Fatalf("messages %q, want %q", ids, want)
	}
}

func TestDisconnectAndUnhookingStopDelivery(t *testing.T) {
	c, r := newClient(t, false)
	seen := 0
	c.OnEvent(func(ev engine.Event) bool {
		r.events <- ev
		if seen++; seen == 3 {
			c.Disconnect()
		}
		return true
	})
	pair(t, c, r)
	r.next(t)
	r.next(t)
	r.next(t)
	r.quiet(t)
	if !c.Paired() {
		t.Fatal("Disconnect unpaired the client")
	}
	if err := c.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, ok := r.next(t).(engine.Connected); !ok {
		t.Fatal("a paired client did not report Connected")
	}
	unhook := c.OnEvent(func(engine.Event) bool { return true })
	unhook()
	if c.deliver(engine.Connected{}) {
		t.Fatal("a cancelled handler still receives events")
	}
}

func TestAWrongAccountIsPairedAndThenLoggedOut(t *testing.T) {
	c, r := newClient(t, true)
	pair(t, c, r)
	if p, ok := r.next(t).(engine.Paired); !ok || p.JID != "15550100029:7@s.whatsapp.net" {
		t.Fatalf("first event %#v, want Paired for the wrong account", p)
	}
	if _, ok := r.next(t).(engine.Connected); !ok {
		t.Fatal("pairing was not followed by Connected")
	}
	if c.Account() != wrongAccount {
		t.Fatalf("Account = %q", c.Account())
	}
	r.quiet(t)
	if err := c.Logout(t.Context()); err != nil || c.Paired() || c.Account() != "" {
		t.Fatalf("Logout = %v, Paired %v, Account %q", err, c.Paired(), c.Account())
	}
}

func TestTheVersionSourceAgreesWithTheClient(t *testing.T) {
	c, _ := newClient(t, false)
	v, err := c.Latest(t.Context())
	if err != nil || v.IsZero() || v.Less(c.Version()) {
		t.Fatalf("Latest = %v, %v with the client at %v", v, err, c.Version())
	}
	c.SetVersion(engine.Version{2, 3000, 2})
	if c.Version() != (engine.Version{2, 3000, 2}) {
		t.Fatal("SetVersion was not kept")
	}
}

func inflated(t *testing.T, compressed []byte, limit int64) []byte {
	t.Helper()
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zlib: %v", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, limit))
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	return raw
}

func notification(t *testing.T, c *Client, id string) engine.HistoryNotification {
	t.Helper()
	for _, ev := range c.script {
		if n, ok := ev.(engine.HistoryNotification); ok && n.Ref.ID == id {
			return n
		}
	}
	t.Fatalf("the script holds no notification %s", id)
	return engine.HistoryNotification{}
}

func download(t *testing.T, c *Client, id string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := c.DownloadHistory(t.Context(), notification(t, c, id).Ref, &b); err != nil {
		t.Fatalf("DownloadHistory(%s): %v", id, err)
	}
	return b.Bytes()
}

func TestHistoryBlobs(t *testing.T) {
	c, _ := newClient(t, false)
	bootstrap := notification(t, c, bootstrapID)
	if !bootstrap.FromMe || bootstrap.Sender != "15550100009@s.whatsapp.net" || len(bootstrap.Ref.Inline) == 0 {
		t.Fatalf("bootstrap notification %+v", bootstrap)
	}
	h, err := c.Decode(inflated(t, bootstrap.Ref.Inline, historyCap+1))
	if err != nil || len(h.Conversations) != 2 || !slices.Equal(h.LIDMappings, []engine.LIDMapping{{PN: bob, LID: bobLID}}) || len(h.Contacts) != 1 {
		t.Fatalf("bootstrap history %+v, %v", h, err)
	}
	if n := notification(t, c, device5ID); n.Sender != "15550100009:5@s.whatsapp.net" {
		t.Fatalf("device notification sender %q", n.Sender)
	}

	if h, err := c.Decode(inflated(t, download(t, c, recentID), historyCap+1)); err != nil || len(h.Conversations) != 1 || h.Conversations[0].Chat != carol {
		t.Fatalf("recent history %+v, %v", h, err)
	}

	oversized := download(t, c, oversizedID)
	if len(oversized) > historyCap {
		t.Fatalf("the oversized blob is %d bytes compressed: it must pass the download historyCap and fail only when inflated", len(oversized))
	}
	if n := len(inflated(t, oversized, historyCap+2)); n != historyCap+1 {
		t.Fatalf("the oversized blob inflates to %d bytes, want one byte past the historyCap", n)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.DownloadHistory(ctx, notification(t, c, oversizedID).Ref, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled download = %v", err)
	}

	if _, err := c.Decode(inflated(t, download(t, c, poisonID), historyCap+1)); err == nil {
		t.Fatal("the poison blob decoded")
	}
	if err := c.DownloadHistory(t.Context(), engine.HistoryRef{ID: "FAKE-UNKNOWN"}, io.Discard); !errors.Is(err, errUnknownBlob) {
		t.Fatalf("an unknown blob = %v", err)
	}
	if err := c.AckHistory(t.Context(), bootstrap.Ref); err != nil {
		t.Fatalf("AckHistory: %v", err)
	}
}

func TestTheScriptCoversEveryKind(t *testing.T) {
	c, _ := newClient(t, false)
	kinds := map[engine.Kind]bool{}
	drops := 0
	for _, ev := range c.script {
		switch e := ev.(type) {
		case engine.Message:
			kinds[e.Kind] = true
		case engine.Disconnected:
			drops++
		}
	}
	for _, k := range []engine.Kind{engine.KindText, engine.KindMedia, engine.KindReaction, engine.KindPollUpdate, engine.KindEdit, engine.KindRevoke, engine.KindOther} {
		if !kinds[k] {
			t.Errorf("the script has no %s message", k)
		}
	}
	if drops != 1 {
		t.Errorf("the script drops the connection %d times, want once", drops)
	}
}

func connected(t *testing.T) (*Client, *recorder) {
	t.Helper()
	c, r := newClient(t, false)
	pair(t, c, r)
	r.next(t)
	if _, ok := r.next(t).(engine.Connected); !ok {
		t.Fatal("pairing was not followed by Connected")
	}
	return c, r
}

func TestTheScriptAnnouncesTheOwnersIdentityFirst(t *testing.T) {
	c, _ := newClient(t, false)
	if id, ok := c.script[0].(engine.Identity); !ok || id.PN != "15550100009@s.whatsapp.net" || id.LID != ownerLID {
		t.Fatalf("first script event %#v", c.script[0])
	}
	if ownerLID == aliceLID || ownerLID == bobLID {
		t.Fatal("the owner's LID is a contact's")
	}
}

func TestSendsFollowTheSyntheticChats(t *testing.T) {
	c, _ := newClient(t, false)
	if _, err := c.Send(t.Context(), engine.SendRequest{ID: "X", Chat: alice, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Fatalf("a send before a connection = %v", err)
	}
	c, _ = connected(t)
	for _, tt := range []struct {
		chat   string
		want   error
		sender string
	}{
		{alice, nil, "15550100009@s.whatsapp.net"},
		{bobLID, nil, ownerLID},
		{carol, engine.ErrOutcomeUnknown, ""},
		{group, engine.ErrRecipientUnavailable, ""},
		{"15550100099@s.whatsapp.net", engine.ErrRecipientUnavailable, ""},
	} {
		id := c.NewMessageID()
		sent, err := c.Send(t.Context(), engine.SendRequest{ID: id, Chat: tt.chat, Text: "synthetic"})
		if !errors.Is(err, tt.want) || tt.want == nil && (sent.ID != id || sent.Chat != tt.chat || sent.Sender != tt.sender || sent.Timestamp.IsZero()) {
			t.Errorf("a send to %s = %+v, %v, want %v", tt.chat, sent, err, tt.want)
		}
	}
	if _, err := c.Send(t.Context(), engine.SendRequest{ID: "X", Chat: alice}); !errors.Is(err, engine.ErrRejected) {
		t.Errorf("a send without text = %v", err)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Send(ended, engine.SendRequest{ID: "X", Chat: alice, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Errorf("a send with an ended context = %v", err)
	}
	sent := c.Sent()
	if len(sent) != 3 || sent[0].Chat != alice || sent[1].Chat != bobLID || sent[2].Chat != carol {
		t.Fatalf("Sent() = %+v, want the two delivered sends and the one with an unknown outcome", sent)
	}
	sent[0].Chat = bob
	if c.Sent()[0].Chat != alice {
		t.Fatal("Sent() hands out the fake's own record")
	}
}

func TestMessageIDsAreUniqueAcrossClients(t *testing.T) {
	a, _ := newClient(t, false)
	b, _ := newClient(t, false)
	seen := map[string]bool{}
	for range 50 {
		for _, c := range []*Client{a, b} {
			id := c.NewMessageID()
			if !strings.HasPrefix(id, "FAKE-S") || len(id) > 128 || seen[id] {
				t.Fatalf("NewMessageID = %q", id)
			}
			seen[id] = true
		}
	}
	if decimal(0) != "0" || decimal(7) != "7" || decimal(1234567890) != "1234567890" {
		t.Fatal("decimal")
	}
}

func TestADropOrALogoutEndsSending(t *testing.T) {
	c, r := connected(t)
	c.Disconnect()
	if _, err := c.Send(t.Context(), engine.SendRequest{ID: "X", Chat: alice, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Fatalf("a send after Disconnect = %v", err)
	}
	if err := c.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	for {
		if _, ok := r.next(t).(engine.Connected); ok {
			break
		}
	}
	if _, err := c.Send(t.Context(), engine.SendRequest{ID: "X", Chat: alice, Text: "synthetic"}); err != nil {
		t.Fatalf("a send after the reconnection = %v", err)
	}
	if err := c.Logout(t.Context()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := c.Backfill(t.Context(), engine.BackfillRequest{Chat: alice, AnchorID: "FAKE-H01", AnchorTime: time.Now(), Count: 1}); !errors.Is(err, engine.ErrNotConnected) {
		t.Fatalf("a backfill after Logout = %v", err)
	}

	w, wr := newClient(t, true)
	pair(t, w, wr)
	wr.next(t)
	wr.next(t)
	if _, err := w.Send(t.Context(), engine.SendRequest{ID: "X", Chat: alice, Text: "synthetic"}); !errors.Is(err, engine.ErrNotConnected) {
		t.Fatalf("a send from the wrong account = %v", err)
	}
}

func TestABackfillIsAnsweredOnceWithOlderHistory(t *testing.T) {
	c, r := connected(t)
	var mu sync.Mutex
	offers := 0
	c.OnEvent(func(ev engine.Event) bool {
		n, ok := ev.(engine.HistoryNotification)
		if !ok || n.Ref.SyncType != engine.SyncOnDemand {
			return true
		}
		mu.Lock()
		offers++
		first := offers == 1
		mu.Unlock()
		r.events <- ev
		return !first
	})
	anchor := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for name, req := range map[string]engine.BackfillRequest{
		"no anchor":       {Chat: alice, AnchorTime: anchor, Count: 5},
		"no anchor time":  {Chat: alice, AnchorID: "FAKE-H01", Count: 5},
		"no messages":     {Chat: alice, AnchorID: "FAKE-H01", AnchorTime: anchor},
		"more than fifty": {Chat: alice, AnchorID: "FAKE-H01", AnchorTime: anchor, Count: 51},
		"an unknown chat": {Chat: "15550100099@s.whatsapp.net", AnchorID: "FAKE-H01", AnchorTime: anchor, Count: 5},
	} {
		if _, err := c.Backfill(t.Context(), req); !errors.Is(err, engine.ErrRejected) {
			t.Errorf("%s: Backfill = %v", name, err)
		}
	}
	id, err := c.Backfill(t.Context(), engine.BackfillRequest{Chat: group, AnchorID: "FAKE-H03", AnchorTime: anchor, Count: 5})
	if err != nil || !strings.HasPrefix(id, "FAKE-BF") {
		t.Fatalf("Backfill = %q, %v", id, err)
	}
	refused, ok := r.next(t).(engine.HistoryNotification)
	answer, again := r.next(t).(engine.HistoryNotification)
	r.quiet(t)
	if !ok || !again || refused.Ref.ID != answer.Ref.ID {
		t.Fatalf("offers %+v and %+v: a refused answer is offered again until it is accepted, then never", refused, answer)
	}
	if answer.Ref.RequestID != id || !answer.FromMe || answer.Sender != "15550100009@s.whatsapp.net" || !strings.HasPrefix(answer.Ref.ID, onDemandID) {
		t.Fatalf("answer %+v", answer)
	}
	var b bytes.Buffer
	if err := c.DownloadHistory(t.Context(), answer.Ref, &b); err != nil {
		t.Fatalf("DownloadHistory: %v", err)
	}
	h, err := c.Decode(inflated(t, b.Bytes(), historyCap+1))
	if err != nil || len(h.Conversations) != 1 || h.Conversations[0].Chat != group || len(h.Conversations[0].Messages) != 5 {
		t.Fatalf("the answer's history %+v, %v", h, err)
	}
	for _, m := range h.Conversations[0].Messages {
		if !m.Timestamp.Before(anchor) || m.Sender != alice {
			t.Fatalf("message %+v is not an older message of a member", m)
		}
	}
}

func TestABackfillAnchoredAtAPlayedMessageRedeliversTheMessagesBeforeIt(t *testing.T) {
	c, r := connected(t)
	var played []engine.Event
	for {
		ev := r.next(t)
		played = append(played, ev)
		if _, ok := ev.(engine.Disconnected); ok {
			break
		}
	}
	if err := c.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, ok := r.next(t).(engine.Connected); !ok {
		t.Fatal("a reconnection did not start with Connected")
	}
	for len(played) < len(c.script) {
		played = append(played, r.next(t))
	}
	r.quiet(t)
	key := func(m engine.Message) string {
		return m.ID + " " + m.Sender + " " + m.MediaRef + " " + m.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	var direct []string
	var anchor engine.Message
	for _, ev := range played {
		if m, ok := ev.(engine.Message); ok && m.Chat == alice {
			direct = append(direct, key(anchor))
			anchor = m
		}
	}
	direct = direct[1:]
	slices.Reverse(direct)
	for _, count := range []int{3, maxBackfill} {
		id, err := c.Backfill(t.Context(), engine.BackfillRequest{Chat: aliceLID, AnchorID: anchor.ID, AnchorTime: anchor.Timestamp, Count: count})
		if err != nil {
			t.Fatalf("Backfill: %v", err)
		}
		answer, ok := r.next(t).(engine.HistoryNotification)
		if !ok || answer.Ref.RequestID != id {
			t.Fatalf("answer %+v", answer)
		}
		var b bytes.Buffer
		if err := c.DownloadHistory(t.Context(), answer.Ref, &b); err != nil {
			t.Fatalf("DownloadHistory: %v", err)
		}
		h, err := c.Decode(inflated(t, b.Bytes(), historyCap+1))
		if err != nil || len(h.Conversations) != 1 || h.Conversations[0].Chat != aliceLID {
			t.Fatalf("the answer's history %+v, %v", h, err)
		}
		var got []string
		for _, m := range h.Conversations[0].Messages {
			got = append(got, key(m))
		}
		if want := direct[:min(count, len(direct))]; !slices.Equal(got, want) {
			t.Fatalf("%d messages before %s = %q, want the ones played before it in its chat, newest first: %q", count, anchor.ID, got, want)
		}
	}
	if !slices.ContainsFunc(direct, func(k string) bool { return strings.Contains(k, " "+refPhoto+" ") }) {
		t.Fatal("the direct chat re-delivers no attachment")
	}
}
