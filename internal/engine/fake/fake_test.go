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
