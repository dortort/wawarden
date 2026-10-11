package sender_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sender"
	"github.com/dortort/wawarden/internal/store/ingest"
)

func TestEightConcurrentRequestsOfOneKeySendOnce(t *testing.T) {
	f := newFixture(t, 10, 30)
	g := grantFor(t, writerID, false, alice)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	f.engine.set(func(e *engine) { e.gate, e.entered = gate, entered })
	req := request(t, "same-key-0001", alice, "synthetic hello")
	type answer struct {
		res sender.Result
		err error
	}
	answers := make(chan answer, 8)
	go func() {
		res, err := f.svc.Send(g, t.Context(), req)
		answers <- answer{res, err}
	}()
	<-entered
	var wg sync.WaitGroup
	for range 7 {
		wg.Go(func() {
			res, err := f.svc.Send(g, t.Context(), req)
			answers <- answer{res, err}
		})
	}
	wg.Wait()
	close(gate)
	var sent, inProgress int
	var first sender.Result
	for range 8 {
		a := <-answers
		switch fl, ok := failure(a.err); {
		case a.err == nil && !a.res.Replayed:
			sent++
			first = a.res
		case ok && errors.Is(fl, sender.ErrInProgress) && fl.RetryAfter >= time.Second && fl.RetryAfter <= 5*time.Second:
			inProgress++
		default:
			t.Fatalf("answer = %+v, %v", a.res, a.err)
		}
	}
	if sent != 1 || inProgress != 7 || len(f.engine.sent()) != 1 {
		t.Fatalf("%d sent, %d in progress, %d engine sends, want 1, 7 and 1", sent, inProgress, len(f.engine.sent()))
	}
	again, err := f.svc.Send(g, t.Context(), req)
	if err != nil || !again.Replayed || again.Sent.ID != first.Sent.ID || again.Sent.Chat != first.Sent.Chat || again.Sent.Sender != first.Sent.Sender {
		t.Fatalf("replay = %+v, %v, want %+v replayed", again, err, first)
	}
	if len(f.engine.sent()) != 1 {
		t.Fatal("a replay reached the engine")
	}
}

func TestAKeyReusedForAnotherRequestIsRefusedWhilePendingAndAfter(t *testing.T) {
	f := newFixture(t, 10, 30)
	g := grantFor(t, writerID, false, alice, bob)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	f.engine.set(func(e *engine) { e.gate, e.entered = gate, entered })
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Send(g, t.Context(), request(t, "reused-key-01", alice, "synthetic one"))
		done <- err
	}()
	<-entered
	reply := request(t, "reused-key-01", alice, "synthetic one")
	reply.Reply = &sender.Reply{ID: "IN0", Sender: chat(t, alice), Text: "synthetic inbound"}
	others := []sender.Request{request(t, "reused-key-01", alice, "synthetic two"), request(t, "reused-key-01", bob, "synthetic one"), reply}
	for _, other := range others {
		if _, err := f.svc.Send(g, t.Context(), other); !errors.Is(err, ingest.ErrKeyReused) {
			t.Fatalf("a different request under a pending key = %v, want ErrKeyReused", err)
		}
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.engine.set(func(e *engine) { e.gate, e.entered = nil, nil })
	for _, other := range others {
		if _, err := f.svc.Send(g, t.Context(), other); !errors.Is(err, ingest.ErrKeyReused) {
			t.Fatalf("a different request under a settled key = %v, want ErrKeyReused", err)
		}
	}
	if n := len(f.engine.sent()); n != 1 {
		t.Fatalf("%d engine sends, want 1", n)
	}
}

func TestNothingSentReleasesTheKeyAndRefundsTheBudget(t *testing.T) {
	for _, notSent := range []error{sender.ErrNotConnected, sender.ErrEngineBusy} {
		t.Run(notSent.Error(), func(t *testing.T) {
			f := newFixture(t, 1, 30)
			g := grantFor(t, writerID, false, alice)
			req := request(t, "release-key-1", alice, "synthetic retry")
			f.engine.set(func(e *engine) { e.outcome = notSent })
			if _, err := f.svc.Send(g, t.Context(), req); !errors.Is(err, notSent) {
				t.Fatalf("Send = %v, want %v", err, notSent)
			}
			f.engine.set(func(e *engine) { e.outcome = nil })
			res, err := f.svc.Send(g, t.Context(), req)
			if err != nil || res.Replayed {
				t.Fatalf("the retry = %+v, %v, want a fresh send inside a budget of one", res, err)
			}
			if n := len(f.engine.sent()); n != 2 {
				t.Fatalf("%d engine sends, want 2", n)
			}
		})
	}
}

func TestRejectedAndUnknownSendsAreStoredAndNeverResent(t *testing.T) {
	for _, tt := range []struct {
		name    string
		outcome error
		want    error
		events  []string
	}{
		{name: "rejected with a code", outcome: &sender.Rejection{Code: 463}, want: sender.ErrRejected},
		{name: "recipient unavailable", outcome: &sender.Rejection{}, want: sender.ErrRejected},
		{name: "unknown", outcome: sender.ErrOutcomeUnknown, want: sender.ErrOutcomeUnknown, events: []string{"ack_timeout"}},
		{name: "unclassified", outcome: errors.New("synthetic unclassified failure"), want: sender.ErrOutcomeUnknown, events: []string{"ack_timeout"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, 10, 30)
			g := grantFor(t, writerID, false, alice)
			req := request(t, "stored-fail-1", alice, "synthetic text")
			f.engine.set(func(e *engine) { e.outcome = tt.outcome })
			_, err := f.svc.Send(g, t.Context(), req)
			fl, ok := failure(err)
			if !ok || !errors.Is(err, tt.want) || fl.Replayed {
				t.Fatalf("Send = %v, want %v", err, tt.want)
			}
			f.engine.set(func(e *engine) { e.outcome = nil })
			f.clock.Advance(time.Hour)
			_, err = f.svc.Send(g, t.Context(), req)
			if fl, ok := failure(err); !ok || !errors.Is(err, tt.want) || !fl.Replayed {
				t.Fatalf("the retry = %v, want %v replayed", err, tt.want)
			}
			if n := len(f.engine.sent()); n != 1 {
				t.Fatalf("%d engine sends, want 1", n)
			}
			if unknown, unrecorded := f.notes.counts(); !slices.Equal(unknown, tt.events) || unrecorded != 0 {
				t.Fatalf("events %q and %d unrecorded, want %q", unknown, unrecorded, tt.events)
			}
		})
	}
}

func TestRefusalsBeforeTheEngineSendNothing(t *testing.T) {
	f := newFixture(t, 1, 30)
	g := grantFor(t, writerID, false, alice, carol, groupJID)
	if _, err := f.svc.Send(g, t.Context(), request(t, "first-contact", carol, "synthetic")); !errors.Is(err, ingest.ErrFirstContact) {
		t.Fatalf("a first contact = %v", err)
	}
	if _, err := f.svc.Send(grantFor(t, writerID, false, alice), t.Context(), request(t, "out-of-scope", bob, "synthetic")); !errors.Is(err, ingest.ErrNotAllowed) {
		t.Fatalf("a chat outside the grant = %v", err)
	}
	f.engine.set(func(e *engine) { e.ready = false })
	if _, err := f.svc.Send(g, t.Context(), request(t, "not-ready-01", alice, "synthetic")); !errors.Is(err, ingest.ErrNotReady) {
		t.Fatalf("an engine that is not ready = %v", err)
	}
	f.engine.set(func(e *engine) { e.ready = true })
	if _, err := f.svc.Send(g, t.Context(), request(t, "group-first-1", groupJID, "synthetic")); err != nil {
		t.Fatalf("a group send = %v", err)
	}
	f.clock.Advance(5 * time.Second)
	_, err := f.svc.Send(g, t.Context(), request(t, "over-minute-1", alice, "synthetic"))
	if rl, ok := errors.AsType[*ingest.RateLimitError](err); !ok || rl.Reason != ingest.ReasonMinute || rl.RetryAfter != 55*time.Second {
		t.Fatalf("a send over the minute budget = %v", err)
	}
	if n := len(f.engine.sent()); n != 1 {
		t.Fatalf("%d engine sends, want only the group's", n)
	}
}

func TestTheChatTimerAndTheQuoteReachTheEngineAndTheRow(t *testing.T) {
	f := newFixture(t, 10, 30)
	err := f.store.Write(t.Context(), "test.timer", func(tx *ingest.Tx) error {
		m := ingest.Message{Chat: chat(t, alice), ID: "TIMED", Sender: chat(t, alice), Origin: ingest.OriginLive, Timestamp: epoch.Add(-time.Minute),
			Kind: ingest.KindText, Text: "synthetic vanishing", Ingested: epoch, ExpiresAt: epoch.Add(-time.Minute + 7*24*time.Hour)}
		_, _, err := tx.InsertMessage(m)
		return err
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	g := grantFor(t, writerID, false, alice)
	req := request(t, "timed-reply-1", alice, "synthetic answer")
	req.Reply = &sender.Reply{ID: "TIMED", Sender: chat(t, alice), Text: "synthetic vanishing"}
	res, err := f.svc.Send(g, t.Context(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := f.engine.sent()
	if len(calls) != 1 {
		t.Fatalf("%d engine sends", len(calls))
	}
	m := calls[0].msg
	if m.Expiration != 7*24*time.Hour || m.Quote == nil || *m.Quote != (sender.Quote{ID: "TIMED", Participant: chat(t, alice), Text: "synthetic vanishing"}) ||
		m.Chat != chat(t, alice) || m.Text != "synthetic answer" || m.ID != res.Sent.ID {
		t.Fatalf("engine message = %+v", m)
	}
	readAll, _ := policy.DecideRead(&policy.Client{ID: writerID, ReadAll: true, ExpiresAt: farFuture}, epoch)
	row, found, err := f.store.Scoped().Message(readAll, t.Context(), res.Sent.Chat, res.Sent.ID, res.Sent.Sender)
	if err != nil || !found || row.SentBy != writerID || !row.FromMe || row.ReplyID != "TIMED" || !row.QuoteVerified || row.Text != "synthetic answer" {
		t.Fatalf("stored row = %+v, %v, %v", row, found, err)
	}
	for _, at := range []time.Time{epoch.Add(7*24*time.Hour - 30*time.Second), epoch.Add(7 * 24 * time.Hour)} {
		var purged int
		err := f.store.Write(t.Context(), "test.purge", func(tx *ingest.Tx) error {
			var err error
			purged, err = tx.PurgeExpired(at, 10)
			return err
		})
		if err != nil || purged != 1 {
			t.Fatalf("PurgeExpired(%v) = %d, %v, want the earlier timed row, then the sent one", at, purged, err)
		}
	}
}

func TestAFailedSettlementIsRetriedThenReported(t *testing.T) {
	f := newFixture(t, 10, 30)
	g := grantFor(t, writerID, false, alice)
	busy := &settleFault{Store: f.store, fails: 3, err: context.DeadlineExceeded}
	res, err := f.service(busy, 10, 30).Send(g, t.Context(), request(t, "settle-retry1", alice, "synthetic"))
	if err != nil || res.Sent.ID == "" {
		t.Fatalf("Send after three failed settlements = %+v, %v, want the fourth to store it", res, err)
	}
	f.clock.Advance(5 * time.Second)
	broken := &settleFault{Store: f.store, fails: 4, err: context.DeadlineExceeded}
	if _, err := f.service(broken, 10, 30).Send(g, t.Context(), request(t, "settle-fail-1", alice, "synthetic")); !errors.Is(err, sender.ErrUnrecorded) {
		t.Fatalf("Send after four failed settlements = %v, want ErrUnrecorded", err)
	}
	if _, unrecorded := f.notes.counts(); unrecorded != 1 {
		t.Fatalf("%d send_unrecorded events, want 1", unrecorded)
	}
	f.clock.Advance(ingest.SendLease)
	_, err = f.svc.Send(g, t.Context(), request(t, "settle-fail-1", alice, "synthetic"))
	if fl, ok := failure(err); !ok || !errors.Is(err, sender.ErrOutcomeUnknown) || !fl.Replayed {
		t.Fatalf("the retry after the lease = %v, want the outcome unknown", err)
	}
	if unknown, _ := f.notes.counts(); !slices.Equal(unknown, []string{"lease_expired"}) {
		t.Fatalf("events %q", unknown)
	}
	if n := len(f.engine.sent()); n != 2 {
		t.Fatalf("%d engine sends, want 2", n)
	}
}

func TestAClientThatLeavesAfterTheAckStillHasItsSendStored(t *testing.T) {
	f := newFixture(t, 10, 30)
	g := grantFor(t, writerID, false, alice)
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	o := f.options(f.store, 10, 30)
	o.Engine = leavingClient{engine: f.engine, leave: leave}
	req := request(t, "leaving-key-1", alice, "synthetic")
	res, err := sender.New(o).Send(g, ctx, req)
	if err != nil || res.Replayed || res.Sent.ID == "" {
		t.Fatalf("Send with the client gone after the ack = %+v, %v, want the stored row", res, err)
	}
	if _, unrecorded := f.notes.counts(); unrecorded != 0 {
		t.Fatalf("%d send_unrecorded events, want 0", unrecorded)
	}
	again, err := f.svc.Send(g, t.Context(), req)
	if err != nil || !again.Replayed || again.Sent.ID != res.Sent.ID {
		t.Fatalf("the retry = %+v, %v, want the stored send replayed", again, err)
	}
}

func TestTheHourShareDividesTheGlobalBudgetByTheWriteHolders(t *testing.T) {
	f := newFixture(t, 10, 6)
	o := f.options(f.store, 10, 6)
	o.Holders = func(context.Context, time.Time) (int, error) { return 3, nil }
	svc := sender.New(o)
	g := grantFor(t, writerID, false, alice)
	for _, k := range []string{"share-key-001", "share-key-002"} {
		if _, err := svc.Send(g, t.Context(), request(t, k, alice, "synthetic")); err != nil {
			t.Fatalf("a send inside the share = %v", err)
		}
		f.clock.Advance(sender.PacerBase)
	}
	_, err := svc.Send(g, t.Context(), request(t, "share-key-003", alice, "synthetic"))
	if rl, ok := errors.AsType[*ingest.RateLimitError](err); !ok || rl.Reason != ingest.ReasonHour || rl.RetryAfter != time.Hour-2*sender.PacerBase {
		t.Fatalf("the third send in the hour with a share of two = %v", err)
	}
	if n := len(f.engine.sent()); n != 2 {
		t.Fatalf("%d engine sends, want 2", n)
	}
}

func TestAnAckAfterTheLeaseEndedIsUnknownAndReported(t *testing.T) {
	f := newFixture(t, 10, 30)
	g := grantFor(t, writerID, false, alice)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	f.engine.set(func(e *engine) { e.gate, e.entered = gate, entered })
	req := request(t, "late-ack-0001", alice, "synthetic")
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Send(g, t.Context(), req)
		done <- err
	}()
	<-entered
	f.clock.Advance(ingest.SendLease)
	_, err := f.svc.Send(g, t.Context(), req)
	if fl, ok := failure(err); !ok || !errors.Is(err, sender.ErrOutcomeUnknown) || !fl.Replayed {
		t.Fatalf("a retry after the lease = %v", err)
	}
	close(gate)
	err = <-done
	if fl, ok := failure(err); !ok || !errors.Is(err, sender.ErrOutcomeUnknown) || fl.Replayed {
		t.Fatalf("the late ack = %v, want the outcome unknown", err)
	}
	if unknown, unrecorded := f.notes.counts(); !slices.Equal(unknown, []string{"lease_expired"}) || unrecorded != 1 {
		t.Fatalf("events %q and %d unrecorded", unknown, unrecorded)
	}
}

func TestTheDefaultPacerWaitsThreeToFiveSeconds(t *testing.T) {
	var lowest, highest time.Duration = time.Hour, 0
	for range 10000 {
		d := sender.DefaultPacer()
		lowest, highest = min(lowest, d), max(highest, d)
	}
	if lowest < 3*time.Second || highest >= 5*time.Second || highest-lowest < time.Second {
		t.Fatalf("pacer gaps from %v to %v, want spread over [3s, 5s)", lowest, highest)
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted options without a store")
		}
	}()
	sender.New(sender.Options{})
}

func TestARevokedGatewaySendLeavesNoTextOnDisk(t *testing.T) {
	const textCanary, keyCanary = "canary-text-5e0c1d gateway", "canary-key-4b9a77"
	f := newFixture(t, 10, 30)
	g := grantFor(t, writerID, false, alice)
	res, err := f.svc.Send(g, t.Context(), request(t, keyCanary, alice, textCanary))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := f.store.Write(t.Context(), "test.revoke", func(tx *ingest.Tx) error {
		found, ok, err := tx.ResolveInChat(res.Sent.Chat, res.Sent.ID, res.Sent.Sender)
		if err != nil || !ok {
			return errors.Join(err, errors.New("the sent row is not in the archive"))
		}
		return tx.ApplyRevoke(found.Ref)
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var scanned int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		scanned++
		if bytes.Contains(b, []byte(textCanary)) || bytes.Contains(b, []byte("5e0c1d")) {
			t.Fatalf("%s holds the revoked text", e.Name())
		}
	}
	if scanned == 0 {
		t.Fatal("no database file was scanned")
	}
}
