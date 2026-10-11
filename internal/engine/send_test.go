package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

const ownerLID = "100000000000009@lid"

func TestARejectionCarriesTheServersNumber(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code int
		text string
	}{
		{&RejectedError{Code: 479}, 479, "engine: WhatsApp rejected the message with code 479"},
		{fmt.Errorf("wrapped: %w", &RejectedError{Code: 463}), 463, "wrapped: engine: WhatsApp rejected the message with code 463"},
		{&RejectedError{}, 0, "engine: WhatsApp rejected the message"},
	} {
		if !errors.Is(tt.err, ErrRejected) || RejectionCode(tt.err) != tt.code || tt.err.Error() != tt.text {
			t.Errorf("%v: Is(ErrRejected) %v, code %d", tt.err, errors.Is(tt.err, ErrRejected), RejectionCode(tt.err))
		}
	}
	for _, err := range []error{nil, ErrRejected, ErrOutcomeUnknown} {
		if RejectionCode(err) != 0 {
			t.Errorf("RejectionCode(%v) = %d", err, RejectionCode(err))
		}
	}
	for _, err := range []error{ErrNotConnected, ErrRecipientUnavailable, ErrOutcomeUnknown, ErrSendBusy} {
		if errors.Is(err, ErrRejected) {
			t.Errorf("%v reads as a rejection", err)
		}
	}
}

func TestHistoryRefsKeepTheirStoredFormWithoutAnOnDemandAnswer(t *testing.T) {
	plain, err := json.Marshal(HistoryRef{ID: "HS1", DirectPath: "/synthetic"})
	if err != nil || string(plain) != `{"id":"HS1","direct_path":"/synthetic"}` {
		t.Fatalf("a plain ref encodes as %s, %v", plain, err)
	}
	answer := HistoryRef{ID: "HS2", DirectPath: "/synthetic", SyncType: SyncOnDemand, RequestID: "3EB0SYNTHETIC"}
	encoded, err := json.Marshal(answer)
	if err != nil || string(encoded) != `{"id":"HS2","direct_path":"/synthetic","sync_type":"ON_DEMAND","request_id":"3EB0SYNTHETIC"}` {
		t.Fatalf("an on-demand ref encodes as %s, %v", encoded, err)
	}
	var back HistoryRef
	if err := json.Unmarshal(encoded, &back); err != nil || back.SyncType != SyncOnDemand || back.RequestID != answer.RequestID {
		t.Fatalf("decoded %+v, %v", back, err)
	}
}

func readyEngine(t *testing.T) (*histRig, *Engine) {
	t.Helper()
	r := newHistRig(t)
	e := newEngine(t, r)
	e.Start(t.Context(), 1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	r.client.waitFor(t, "connect")
	r.client.emit(Connected{})
	eventually(t, "the engine is ready", e.Ready)
	return r, e
}

func heldEngine(t *testing.T) (*histRig, *Engine) {
	t.Helper()
	r := newHistRig(t)
	e := newEngine(t, r)
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	return r, e
}

func TestSendsAndBackfillsNeedAConnectedPairedEngine(t *testing.T) {
	r := newHistRig(t)
	e := newEngine(t, r)
	held, heldEngine := heldEngine(t)
	for name, e := range map[string]*Engine{"never started": e, "held by its restart budget": heldEngine} {
		if e.Ready() {
			t.Errorf("%s: the engine is ready", name)
		}
		if _, err := e.Send(t.Context(), SendRequest{ID: "A", Chat: alice, Text: "synthetic"}); !errors.Is(err, ErrNotConnected) {
			t.Errorf("%s: Send = %v", name, err)
		}
		if _, err := e.Backfill(t.Context(), BackfillRequest{Chat: alice, AnchorID: "M1", AnchorTime: epoch, Count: 5}); !errors.Is(err, ErrNotConnected) {
			t.Errorf("%s: Backfill = %v", name, err)
		}
	}
	if r.client.count("send:A")+held.client.count("send:A")+r.client.count("backfill:M1")+held.client.count("backfill:M1") != 0 {
		t.Fatal("a refused call reached the client")
	}

	r, e = readyEngine(t)
	if id := e.NewMessageID(); !strings.HasPrefix(id, "TEST-") {
		t.Fatalf("NewMessageID = %q", id)
	}
	sent, err := e.Send(t.Context(), SendRequest{ID: "A", Chat: alice, Text: "synthetic"})
	if err != nil || sent != (Sent{ID: "A", Chat: alice, Sender: owner, Timestamp: epoch}) {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
	if id, err := e.Backfill(t.Context(), BackfillRequest{Chat: alice, AnchorID: "M1", AnchorTime: epoch, Count: 5}); err != nil || !strings.HasPrefix(id, "TEST-BF") {
		t.Fatalf("Backfill = %q, %v", id, err)
	}
	r.client.setPaired(false)
	if e.Ready() {
		t.Fatal("an unpaired engine is ready")
	}
	if _, err := e.Send(t.Context(), SendRequest{ID: "B", Chat: alice, Text: "synthetic"}); !errors.Is(err, ErrNotConnected) || r.client.count("send:B") != 0 {
		t.Fatalf("Send while unpaired = %v", err)
	}
	r.client.setPaired(true)
	r.client.emit(Disconnected{})
	eventually(t, "the drop is seen", func() bool { return !e.Ready() })
	if _, err := e.Send(t.Context(), SendRequest{ID: "C", Chat: alice, Text: "synthetic"}); !errors.Is(err, ErrNotConnected) || r.client.count("send:C") != 0 {
		t.Fatalf("Send after a drop = %v", err)
	}
}

func TestASecondSendIsRefusedWhileOneRuns(t *testing.T) {
	r, e := readyEngine(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r.client.mu.Lock()
	r.client.onSend = func(SendRequest) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
	}
	r.client.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := e.Send(context.Background(), SendRequest{ID: "A", Chat: alice, Text: "synthetic"})
		done <- err
	}()
	<-entered
	start := time.Now()
	if _, err := e.Send(t.Context(), SendRequest{ID: "B", Chat: bob, Text: "synthetic"}); !errors.Is(err, ErrSendBusy) {
		t.Errorf("a concurrent send = %v", err)
	}
	if _, err := e.Backfill(t.Context(), BackfillRequest{Chat: alice, AnchorID: "M1", AnchorTime: epoch, Count: 5}); !errors.Is(err, ErrSendBusy) {
		t.Errorf("a concurrent backfill = %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the refusals waited %v", waited)
	}
	r.client.mu.Lock()
	r.client.onSend = nil
	r.client.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first send = %v", err)
	}
	if r.client.count("send:B") != 0 || r.client.count("backfill:M1") != 0 {
		t.Fatalf("a refused call reached the client: %v", r.client.history())
	}
	if _, err := e.Send(t.Context(), SendRequest{ID: "C", Chat: bob, Text: "synthetic"}); err != nil {
		t.Fatalf("a send after the first finished = %v", err)
	}
}

func TestOutcomesCrossTheEngineAsTheirClassOrAsUnknown(t *testing.T) {
	r, e := readyEngine(t)
	for _, tt := range []struct {
		err  error
		want error
	}{
		{ErrNotConnected, ErrNotConnected},
		{ErrRecipientUnavailable, ErrRecipientUnavailable},
		{&RejectedError{Code: 479}, ErrRejected},
		{ErrOutcomeUnknown, ErrOutcomeUnknown},
		{errors.New("synthetic"), ErrOutcomeUnknown},
		{context.Canceled, ErrOutcomeUnknown},
		{ErrSendBusy, ErrOutcomeUnknown},
	} {
		r.client.mu.Lock()
		r.client.sendErrs, r.client.backfillErrs = []error{tt.err}, []error{tt.err}
		r.client.mu.Unlock()
		sent, err := e.Send(t.Context(), SendRequest{ID: "A", Chat: alice, Text: "synthetic"})
		if !errors.Is(err, tt.want) || sent != (Sent{}) {
			t.Errorf("a send failing with %v = %+v, %v, want %v", tt.err, sent, err, tt.want)
		}
		if _, err := e.Backfill(t.Context(), BackfillRequest{Chat: alice, AnchorID: "M1", AnchorTime: epoch, Count: 5}); !errors.Is(err, tt.want) {
			t.Errorf("a backfill failing with %v = %v, want %v", tt.err, err, tt.want)
		}
	}
	r.client.mu.Lock()
	r.client.sendErrs = []error{&RejectedError{Code: 479}}
	r.client.mu.Unlock()
	if _, err := e.Send(t.Context(), SendRequest{ID: "A", Chat: alice, Text: "synthetic"}); RejectionCode(err) != 479 {
		t.Fatalf("the server's number was lost: %v", err)
	}
}

func TestAPanickingSendIsAnUnknownOutcomeAndFreesTheSlot(t *testing.T) {
	r, e := readyEngine(t)
	r.reportPanics()
	r.client.mu.Lock()
	r.client.onSend = func(SendRequest) { panic("synthetic") }
	r.client.mu.Unlock()
	if _, err := e.Send(t.Context(), SendRequest{ID: "A", Chat: alice, Text: "synthetic"}); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a panicking send = %v", err)
	}
	if r.counter("wawarden_panics_total", "name", "engine.send") != 1 {
		t.Fatal("the panic was not counted")
	}
	r.client.mu.Lock()
	r.client.onSend = nil
	r.client.mu.Unlock()
	if _, err := e.Send(t.Context(), SendRequest{ID: "B", Chat: alice, Text: "synthetic"}); err != nil {
		t.Fatalf("a send after the panic = %v", err)
	}
}

func lidSource(t *testing.T, r *histRig, lid string) (string, bool) {
	t.Helper()
	db := r.inspect()
	var source string
	err := db.QueryRowContext(t.Context(), "SELECT source FROM lid_map WHERE lid = ?", lid).Scan(&source)
	if err != nil {
		return "", false
	}
	return source, true
}

func TestTheOwnersIdentitySeedsTheirOwnPair(t *testing.T) {
	r, _ := heldEngine(t)
	own := text(alice, "M1", owner, "an own message before the identity is known")
	own.FromMe = true
	r.ingest(own)
	for _, id := range []Identity{{PN: owner, LID: ownerLID}, {PN: owner, LID: ownerLID}} {
		if !r.client.emit(id) {
			t.Fatal("the identity was refused")
		}
	}
	if r.counter("wawarden_rekey_conflicts_total", "conflict", "mapping_contradicts") != 0 {
		t.Fatal("a repeated identity was counted as a conflict")
	}
	if f, ok := r.find(alice, "M1", ownerLID); !ok || !f.FromMe {
		t.Fatalf("the own message is not found under the owner's LID: %+v, %v", f, ok)
	}
	if source, ok := lidSource(t, r, ownerLID); !ok || source != "self" {
		t.Fatalf("lid_map source %q, %v", source, ok)
	}
}

func TestAnIdentityThatIsNotTheOwnersIsDropped(t *testing.T) {
	r, _ := heldEngine(t)
	for _, id := range []Identity{
		{PN: alice, LID: ownerLID},
		{PN: owner, LID: owner},
		{PN: ownerLID, LID: owner},
		{PN: "", LID: ""},
		{PN: owner, LID: group},
	} {
		if !r.client.emit(id) {
			t.Fatalf("%+v was refused rather than dropped", id)
		}
	}
	if r.dropped("owner_unknown") != 1 || r.dropped("invalid") != 4 {
		t.Fatalf("drops owner_unknown %v, invalid %v", r.dropped("owner_unknown"), r.dropped("invalid"))
	}
	r.client.setPaired(false)
	if !r.client.emit(Identity{PN: owner, LID: ownerLID}) || r.dropped("not_paired") != 1 {
		t.Fatalf("an identity while unpaired: not_paired drops %v", r.dropped("not_paired"))
	}
	if _, ok := lidSource(t, r, ownerLID); ok {
		t.Fatal("a dropped identity was learned")
	}

	unknown := newHistRig(t, withOwner(""))
	e := newEngine(t, unknown)
	e.Start(t.Context(), MaxRecentStarts+1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	if !unknown.client.emit(Identity{PN: owner, LID: ownerLID}) || unknown.dropped("owner_unknown") != 1 {
		t.Fatalf("an identity without a configured owner: owner_unknown drops %v", unknown.dropped("owner_unknown"))
	}
}

func TestAContradictedIdentityRaisesTheRekeyAlert(t *testing.T) {
	r, _ := heldEngine(t)
	r.write(func(tx *ingest.Tx) error {
		_, err := tx.LearnLID(chat(t, "100000000000099@lid"), chat(t, owner), ingest.MappingHistory, epoch)
		return err
	})
	if !r.client.emit(Identity{PN: owner, LID: ownerLID}) {
		t.Fatal("a contradicted identity was refused")
	}
	if r.counter("wawarden_rekey_conflicts_total", "conflict", "mapping_contradicts") != 1 || len(r.alerts("rekey_conflict")) != 1 {
		t.Fatalf("conflicts %v, alerts %v", r.counter("wawarden_rekey_conflicts_total", "conflict", "mapping_contradicts"), r.alerts("rekey_conflict"))
	}
	if strings.Contains(r.logs.String(), "15550100009") || strings.Contains(r.logs.String(), "100000000000009") {
		t.Fatal("the alert names the owner's identity")
	}
}

func TestAnIdentityIsRefusedWhenTheArchiveCannotStoreIt(t *testing.T) {
	r := newPipeRig(t)
	if err := r.archive.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.p.identity(Identity{PN: owner, LID: ownerLID}) {
		t.Fatal("an identity was acknowledged although its write failed")
	}
	if r.counter("wawarden_ingest_refused_total", "reason", "store_error") != 1 {
		t.Fatal("the refusal was not counted")
	}
}

func adminGrant(t *testing.T) policy.AdminGrant {
	t.Helper()
	secret := token.NewAdmin()
	cred, err := policy.ParseAdminCredential(token.Hash(secret))
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	g, ok := policy.DecideAdmin(cred, secret)
	if !ok {
		t.Fatal("DecideAdmin refused the token")
	}
	return g
}

func requestBackfill(t *testing.T, r *histRig, provisional, requestID string, at time.Time) {
	t.Helper()
	if _, err := r.archive.ReserveBackfill(adminGrant(t), t.Context(), ingest.BackfillRequest{ID: provisional, Chat: chat(t, alice), Count: 50, Now: at}); err != nil {
		t.Fatalf("ReserveBackfill: %v", err)
	}
	if err := r.archive.BindBackfill(t.Context(), provisional, requestID); err != nil {
		t.Fatalf("BindBackfill: %v", err)
	}
}

func TestAnOnDemandAnswerCompletesItsBackfill(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	r.ingest(dm("M1", alice, "the anchor"))
	requestBackfill(t, r, "PROVISIONAL1", "3EB0REQUEST1", epoch)
	requestBackfill(t, r, "PROVISIONAL2", "3EB0REQUEST2", epoch.Add(ingest.BackfillInterval))
	older := History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "H0", Sender: alice, Timestamp: epoch.Add(-time.Hour), Kind: KindText, Text: "older"}}}}}
	r.client.blobs["HS1"] = r.blob("synthetic on-demand answer", older)
	r.client.blobs["HS2"] = r.blob("synthetic bootstrap naming a request", older)
	r.notify(HistoryRef{ID: "HS1", DirectPath: "/synthetic", SyncType: SyncOnDemand, RequestID: "3EB0REQUEST1"})
	r.notify(HistoryRef{ID: "HS2", DirectPath: "/synthetic", SyncType: "INITIAL_BOOTSTRAP", RequestID: "3EB0REQUEST2"})
	r.drainHistory()
	if _, ok := r.find(alice, "H0", alice); !ok {
		t.Fatal("the answer's history was not applied")
	}
	r.write(func(tx *ingest.Tx) error {
		if again, err := tx.CompleteBackfill("3EB0REQUEST1", epoch.Add(time.Hour)); again || err != nil {
			t.Errorf("the answered request completes again: %v, %v", again, err)
		}
		if first, err := tx.CompleteBackfill("3EB0REQUEST2", epoch.Add(time.Hour)); !first || err != nil {
			t.Errorf("a bootstrap blob completed a request: %v, %v", first, err)
		}
		return nil
	})
}
