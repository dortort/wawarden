package sender_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sender"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	propPerMinute = 2
	propGlobal    = 6
	propHolders   = 2
	propShare     = propGlobal / propHolders
)

var (
	propChats  = []string{alice, bob, carol, groupJID}
	propKeys   = []string{"prop-key-0001", "prop-key-0002", "prop-key-0003", "prop-key-0004", "prop-key-0005"}
	propTexts  = []string{"synthetic a", "synthetic b"}
	propSteps  = []time.Duration{time.Second, 3 * time.Second, 20 * time.Second, 61 * time.Second, 10 * time.Minute, 61 * time.Minute, 25 * time.Hour}
	propWrites = map[string][]string{writerID: {alice, bob, carol, groupJID}, otherID: {alice, groupJID}}
)

type outcome uint8

const (
	outAck outcome = iota
	outNotConnected
	outBusy
	outRejected
	outUnknown
)

func (o outcome) err() error {
	switch o {
	case outNotConnected:
		return sender.ErrNotConnected
	case outBusy:
		return sender.ErrEngineBusy
	case outRejected:
		return &sender.Rejection{Code: 479}
	case outUnknown:
		return sender.ErrOutcomeUnknown
	}
	return nil
}

func (o outcome) nothingSent() bool { return o == outNotConnected || o == outBusy }

type answer struct {
	class    string
	id       string
	replayed bool
}

func classify(t *rapid.T, res sender.Result, err error) answer {
	if err == nil {
		return answer{class: "sent", id: res.Sent.ID, replayed: res.Replayed}
	}
	fl, isFailure := failure(err)
	replayed := isFailure && fl.Replayed
	for _, c := range []struct {
		err   error
		class string
	}{
		{ingest.ErrKeyReused, "key_reused"}, {ingest.ErrFirstContact, "first_contact"}, {ingest.ErrNotReady, "not_connected"},
		{ingest.ErrNotAllowed, "not_found"}, {sender.ErrNotConnected, "not_connected"}, {sender.ErrEngineBusy, "rate_limited"},
		{sender.ErrRejected, "send_rejected"}, {sender.ErrOutcomeUnknown, "send_outcome_unknown"}, {sender.ErrInProgress, "request_in_progress"},
	} {
		if errors.Is(err, c.err) {
			return answer{class: c.class, replayed: replayed}
		}
	}
	if rl, ok := errors.AsType[*ingest.RateLimitError](err); ok && rl.RetryAfter >= time.Second && rl.RetryAfter <= time.Hour {
		return answer{class: "rate_limited"}
	}
	t.Fatalf("an answer outside the fixed table: %v", err)
	return answer{}
}

type record struct {
	chat, text string
	reserved   time.Time
	first      answer
}

type sendModel struct {
	t        *rapid.T
	dir      string
	store    *ingest.Store
	clock    *clock
	engine   *engine
	svc      *sender.Service
	pacer    time.Duration
	grants   map[string]policy.WriteGrant
	records  map[[2]string]record
	counted  map[string][]time.Time
	outcomes map[[2]string][]time.Time
	paced    []time.Time
}

func newSendModel(t *rapid.T, parent string, pacer time.Duration) *sendModel {
	dir, err := os.MkdirTemp(parent, "sender")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	m := &sendModel{t: t, dir: dir, clock: &clock{now: epoch}, pacer: pacer, grants: map[string]policy.WriteGrant{},
		records: map[[2]string]record{}, counted: map[string][]time.Time{}, outcomes: map[[2]string][]time.Time{}}
	m.engine = newEngine(m.clock)
	m.open()
	inbound(t, m.store, alice, bob)
	for id, chats := range propWrites {
		m.grants[id] = grantFor(t, id, false, chats...)
	}
	return m
}

func (m *sendModel) open() {
	m.store = openStore(m.t, m.dir)
	m.svc = sender.New(sender.Options{Store: m.store, Engine: m.engine, Notifier: &notes{}, PerMinute: propPerMinute, GlobalPerHour: propGlobal,
		Holders: func(context.Context, time.Time) (int, error) { return propHolders, nil }, Now: m.clock.Now, Pacer: func() time.Duration { return m.pacer }})
}

func (m *sendModel) close() {
	_ = m.store.Close()
	_ = os.RemoveAll(m.dir)
}

func (m *sendModel) within(times []time.Time, window time.Duration) int {
	now := m.clock.Now()
	n := 0
	for _, at := range times {
		if at.After(now.Add(-window)) {
			n++
		}
	}
	return n
}

func (m *sendModel) expected(client, chatJID string, ready bool) string {
	now := m.clock.Now()
	switch {
	case !slices.Contains(propWrites[client], chatJID):
		return "not_found"
	case chatJID == carol:
		return "first_contact"
	case !ready:
		return "not_connected"
	case m.within(m.counted[client], time.Minute) >= propPerMinute || m.within(m.counted[client], time.Hour) >= propShare:
		return "rate_limited"
	case len(m.paced) > 0 && now.Before(m.paced[len(m.paced)-1].Add(m.pacer)):
		return "rate_limited"
	}
	return ""
}

func (m *sendModel) send(t *rapid.T) {
	client := rapid.SampledFrom([]string{writerID, otherID}).Draw(t, "client")
	chatJID := rapid.SampledFrom(propChats).Draw(t, "chat")
	k := rapid.SampledFrom(propKeys).Draw(t, "key")
	text := rapid.SampledFrom(propTexts).Draw(t, "text")
	out := rapid.SampledFrom([]outcome{outAck, outNotConnected, outBusy, outRejected, outUnknown}).Draw(t, "outcome")
	ready := rapid.IntRange(0, 9).Draw(t, "ready") > 0
	m.engine.set(func(e *engine) { e.outcome, e.ready = out.err(), ready })
	before := len(m.engine.sent())
	now := m.clock.Now()
	id := [2]string{client, k}
	prior, known := m.records[id]
	if known && !now.Before(prior.reserved.Add(ingest.SendTTL)) {
		delete(m.records, id)
		known = false
	}
	res, err := m.svc.Send(m.grants[client], context.Background(), request(t, k, chatJID, text))
	got := classify(t, res, err)
	calls := m.engine.sent()[before:]
	if len(calls) > 1 {
		t.Fatalf("one request made %d engine sends", len(calls))
	}
	if len(calls) == 1 {
		if !slices.Contains(propWrites[client], calls[0].msg.Chat.JID()) {
			t.Fatalf("the engine sent to %s outside the client's write set", calls[0].msg.Chat.JID())
		}
		for _, at := range m.outcomes[id] {
			if now.Before(at.Add(ingest.SendTTL)) {
				t.Fatalf("a second engine send for one key within its lifetime")
			}
		}
	}
	if known && slices.Contains(propWrites[client], chatJID) {
		if prior.chat != chatJID || prior.text != text {
			if got.class != "key_reused" || len(calls) != 0 {
				t.Fatalf("a reused key answered %+v", got)
			}
			return
		}
		want := prior.first
		want.replayed = true
		if got != want || len(calls) != 0 {
			t.Fatalf("replay = %+v, want %+v", got, want)
		}
		return
	}
	want := m.expected(client, chatJID, ready)
	if want != "" {
		if got.class != want || len(calls) != 0 {
			t.Fatalf("answer %+v with %d engine sends, want %s and none", got, len(calls), want)
		}
		return
	}
	if len(calls) != 1 {
		t.Fatalf("an admitted send made %d engine sends (answer %+v)", len(calls), got)
	}
	wantClass := map[outcome]string{outAck: "sent", outNotConnected: "not_connected", outBusy: "rate_limited", outRejected: "send_rejected", outUnknown: "send_outcome_unknown"}[out]
	if got.class != wantClass || got.replayed || (out == outAck && got.id != calls[0].msg.ID) {
		t.Fatalf("answer %+v for engine outcome %d", got, out)
	}
	if out.nothingSent() {
		return
	}
	m.counted[client] = append(m.counted[client], now)
	m.paced = append(m.paced, now)
	m.outcomes[id] = append(m.outcomes[id], now)
	m.records[id] = record{chat: chatJID, text: text, reserved: now, first: got}
}

func (m *sendModel) checkWindows(t *rapid.T) {
	var all []time.Time
	for _, times := range m.counted {
		all = append(all, times...)
		for i := range times {
			minute, hour := 0, 0
			for _, other := range times[:i+1] {
				if other.After(times[i].Add(-time.Minute)) {
					minute++
				}
				if other.After(times[i].Add(-time.Hour)) {
					hour++
				}
			}
			if minute > propPerMinute || hour > propShare {
				t.Fatalf("%d sends in a minute and %d in an hour for one client", minute, hour)
			}
		}
	}
	slices.SortFunc(all, func(a, b time.Time) int { return a.Compare(b) })
	for i := range all {
		hour := 0
		for _, other := range all[:i+1] {
			if other.After(all[i].Add(-time.Hour)) {
				hour++
			}
		}
		if hour > propGlobal {
			t.Fatalf("%d sends in an hour across clients", hour)
		}
		if i > 0 && all[i].Sub(all[i-1]) < m.pacer {
			t.Fatalf("two sends %v apart under a pacer of %v", all[i].Sub(all[i-1]), m.pacer)
		}
	}
}

func runSendModel(t *testing.T, pacer time.Duration) {
	parent := t.TempDir()
	rapid.Check(t, func(t *rapid.T) {
		m := newSendModel(t, parent, pacer)
		defer m.close()
		t.Repeat(map[string]func(*rapid.T){
			"send": m.send,
			"advance": func(t *rapid.T) {
				m.clock.Advance(rapid.SampledFrom(propSteps).Draw(t, "step"))
			},
			"restart": func(t *rapid.T) {
				_ = m.store.Close()
				m.open()
				if n, err := m.store.RecoverSends(context.Background(), m.clock.Now()); err != nil || n != 0 {
					t.Fatalf("RecoverSends = %d, %v, want nothing pending between requests", n, err)
				}
			},
			"": m.checkWindows,
		})
	})
}

func TestSendsFollowTheLedgerWithoutAPacer(t *testing.T) { runSendModel(t, 0) }

func TestSendsFollowTheLedgerUnderThePacer(t *testing.T) { runSendModel(t, sender.PacerBase) }
