package ingest

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	writerID = "aaaaaaaa"
	otherID  = "bbbbbbbb"
	ownLID   = "100000000000009@lid"
)

var (
	farFuture     = epoch.Add(300 * 24 * time.Hour)
	defaultLimits = SendLimits{PerMinute: 10, GlobalPerHour: 30, Holders: 1}
)

func grantFor(t *testing.T, id string, firstContact bool, chats ...string) policy.WriteGrant {
	t.Helper()
	write := make(map[policy.CanonicalChat]struct{}, len(chats))
	for _, c := range chats {
		write[chat(t, c)] = struct{}{}
	}
	g, ok := policy.DecideWrite(&policy.Client{ID: id, Write: write, ExpiresAt: farFuture, AllowFirstContact: firstContact}, epoch)
	if !ok {
		t.Fatal("DecideWrite refused the test client")
	}
	return g
}

func key(t *testing.T, s string) Key {
	t.Helper()
	k, ok := ParseKey(s)
	if !ok {
		t.Fatalf("ParseKey(%q) refused a test key", s)
	}
	return k
}

func request(t *testing.T, k, chatJID, text string, at time.Time) SendReservation {
	t.Helper()
	return SendReservation{Key: key(t, k), Chat: chat(t, chatJID), Fingerprint: Fingerprint("ref:"+chatJID, text, nil), MsgID: "MSG-" + k,
		Now: at, Limits: defaultLimits, Ready: func() bool { return true }}
}

func reserved(t *testing.T, s *Store, g policy.WriteGrant, in SendReservation) {
	t.Helper()
	if res, err := s.ReserveSend(g, t.Context(), in); err != nil || res.Kind != Reserved || res.MsgID != in.MsgID {
		t.Fatalf("ReserveSend = %+v, %v, want the key reserved", res, err)
	}
}

func replay(t *testing.T, s *Store, g policy.WriteGrant, in SendReservation) Reservation {
	t.Helper()
	res, err := s.ReserveSend(g, t.Context(), in)
	if err != nil {
		t.Fatalf("ReserveSend = %v, want an answer from the ledger", err)
	}
	return res
}

func sentText(t *testing.T, chatJID, id, senderJID string, at time.Time) Message {
	t.Helper()
	return Message{Chat: chat(t, chatJID), ID: id, Sender: chat(t, senderJID), FromMe: true, Origin: OriginLive, Timestamp: at,
		Kind: KindText, Text: "synthetic sent text", Ingested: at}
}

func acked(t *testing.T, s *Store, g policy.WriteGrant, in SendReservation, senderJID string, at time.Time) SentRow {
	t.Helper()
	row, err := s.SettleSend(g, t.Context(), Settlement{Key: in.Key, Outcome: SendAcked, Now: at, AckedAt: at, Sent: sentText(t, in.Chat.JID(), in.MsgID, senderJID, at)})
	if err != nil {
		t.Fatalf("SettleSend: %v", err)
	}
	return row
}

func settle(t *testing.T, s *Store, g policy.WriteGrant, in Settlement) {
	t.Helper()
	if _, err := s.SettleSend(g, t.Context(), in); err != nil {
		t.Fatalf("SettleSend: %v", err)
	}
}

func ledger(t *testing.T, s *Store, client, k string) string {
	t.Helper()
	return scalar[string](t, s, "SELECT coalesce((SELECT state || ' ' || coalesce(result, '-') || ' ' || counted FROM idempotency WHERE client_id = ? AND key = ?), 'none')", client, k)
}

func inboundStore(t *testing.T) *Store {
	t.Helper()
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "IN1", alice, "synthetic inbound"))
	return s
}

func TestEightReservationsOfOneKeyProceedOnce(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	in := request(t, "same-key-01", alice, "synthetic", epoch)
	var wg sync.WaitGroup
	kinds, errs := make([]ReserveKind, 8), make([]error, 8)
	for i := range 8 {
		wg.Go(func() {
			res, err := s.ReserveSend(g, t.Context(), in)
			kinds[i], errs[i] = res.Kind, err
		})
	}
	wg.Wait()
	counts := map[ReserveKind]int{}
	for i := range 8 {
		if errs[i] != nil {
			t.Fatalf("reservation %d: %v", i, errs[i])
		}
		counts[kinds[i]]++
	}
	if counts[Reserved] != 1 || counts[InProgress] != 7 {
		t.Fatalf("eight reservations of one key gave %v, want one reserved and seven in progress", counts)
	}
	other := in
	other.Fingerprint = Fingerprint("ref:"+alice, "another text", nil)
	if _, err := s.ReserveSend(g, t.Context(), other); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("another request under a pending key = %v, want ErrKeyReused", err)
	}
	if got := ledger(t, s, writerID, "same-key-01"); got != "pending - 1" {
		t.Fatalf("ledger = %q, want one pending row", got)
	}
}

func TestReplaysFollowTheLedger(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	at := epoch.Add(time.Second)

	done := request(t, "done-key-01", alice, "synthetic", epoch)
	reserved(t, s, g, done)
	row := acked(t, s, g, done, ownerJID, at)
	if row.Chat != chat(t, alice) || row.ID != done.MsgID || row.Sender != chat(t, ownerJID) || !row.Inserted {
		t.Fatalf("SettleSend = %+v", row)
	}
	done.Now = epoch.Add(time.Minute)
	if res := replay(t, s, g, done); res.Kind != ReplayedDone || res.MsgID != done.MsgID || res.Status != 201 || !res.AckedAt.Equal(at) ||
		res.Sent != (SentRow{Chat: row.Chat, ID: row.ID, Sender: row.Sender}) {
		t.Fatalf("replay of a done send = %+v", res)
	}
	reused := done
	reused.Fingerprint = Fingerprint("ref:"+alice, "another text", nil)
	if _, err := s.ReserveSend(g, t.Context(), reused); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("another request under a done key = %v, want ErrKeyReused", err)
	}

	rejected := request(t, "rejected-01", alice, "synthetic", epoch.Add(10*time.Second))
	reserved(t, s, g, rejected)
	settle(t, s, g, Settlement{Key: rejected.Key, Outcome: SendRejected, Now: at, WACode: 403})
	unknown := request(t, "unknown-001", alice, "synthetic", epoch.Add(20*time.Second))
	reserved(t, s, g, unknown)
	settle(t, s, g, Settlement{Key: unknown.Key, Outcome: SendUnknown, Now: at})
	for _, tt := range []struct {
		in           SendReservation
		status, wa   int
		code, stored string
	}{
		{rejected, 502, 403, CodeRejected, `failed {"status":502,"code":"send_rejected","wa":403} 1`},
		{unknown, 504, 0, CodeUnknown, `failed {"status":504,"code":"send_outcome_unknown"} 1`},
	} {
		tt.in.Now = epoch.Add(time.Hour)
		if res := replay(t, s, g, tt.in); res.Kind != ReplayedFailed || res.Status != tt.status || res.Code != tt.code || res.WACode != tt.wa || res.MsgID != tt.in.MsgID {
			t.Fatalf("replay of a failed send = %+v, want %d %s", res, tt.status, tt.code)
		}
		if got := ledger(t, s, writerID, tt.in.Key.Value()); got != tt.stored {
			t.Fatalf("ledger = %q, want %q", got, tt.stored)
		}
		tt.in.Fingerprint = Fingerprint("ref:"+alice, "another text", nil)
		if _, err := s.ReserveSend(g, t.Context(), tt.in); !errors.Is(err, ErrKeyReused) {
			t.Fatalf("another request under a failed key = %v, want ErrKeyReused", err)
		}
	}
	if got := ledger(t, s, writerID, done.Key.Value()); got != `done {"status":201,"ts":`+formatMS(at)+`} 1` {
		t.Fatalf("ledger = %q", got)
	}
}

func TestAPendingKeyAnswersInProgressUntilItsLeaseEnds(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	in := request(t, "lease-key-1", alice, "synthetic", epoch)
	reserved(t, s, g, in)
	for _, tt := range []struct {
		age  time.Duration
		want time.Duration
	}{
		{0, 5 * time.Second},
		{-time.Hour, 5 * time.Second},
		{20500 * time.Millisecond, 5 * time.Second},
		{21 * time.Second, 4 * time.Second},
		{24500 * time.Millisecond, time.Second},
		{30 * time.Second, time.Second},
		{SendLease - time.Millisecond, time.Second},
	} {
		in.Now = epoch.Add(tt.age)
		if res := replay(t, s, g, in); res.Kind != InProgress || res.RetryAfter != tt.want || res.MsgID != in.MsgID {
			t.Fatalf("replay at %v = %+v, want in progress, retry after %v", tt.age, res, tt.want)
		}
	}
	in.Now = epoch.Add(SendLease)
	if res := replay(t, s, g, in); res.Kind != LeaseExpired || res.Status != 504 || res.Code != CodeUnknown || res.MsgID != in.MsgID {
		t.Fatalf("replay at the lease = %+v, want the outcome unknown", res)
	}
	if got := ledger(t, s, writerID, "lease-key-1"); got != `failed {"status":504,"code":"send_outcome_unknown"} 1` {
		t.Fatalf("ledger after the lease = %q", got)
	}
	in.Now = epoch.Add(SendLease + time.Second)
	if res := replay(t, s, g, in); res.Kind != ReplayedFailed || res.Status != 504 || res.Code != CodeUnknown {
		t.Fatalf("replay after the lease = %+v", res)
	}
	if _, err := s.SettleSend(g, t.Context(), Settlement{Key: in.Key, Outcome: SendAcked, Now: in.Now, AckedAt: in.Now, Sent: sentText(t, alice, in.MsgID, ownerJID, in.Now)}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a late ack = %v, want ErrNotPending", err)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages WHERE id = ?", in.MsgID); n != 0 {
		t.Fatal("a late ack stored the message")
	}
}

func TestRecoverSendsFailsEveryPendingSend(t *testing.T) {
	s := inboundStore(t)
	g, other := grantFor(t, writerID, false, alice), grantFor(t, otherID, false, alice)
	first, second, done := request(t, "pending-01", alice, "a", epoch), request(t, "pending-02", alice, "b", epoch.Add(time.Second)), request(t, "done-key-02", alice, "c", epoch.Add(2*time.Second))
	reserved(t, s, g, first)
	reserved(t, s, other, second)
	reserved(t, s, g, done)
	acked(t, s, g, done, ownerJID, epoch.Add(2*time.Second))
	if _, err := s.RecoverSends(t.Context(), time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RecoverSends without a time = %v", err)
	}
	if n, err := s.RecoverSends(t.Context(), epoch.Add(5*time.Second)); n != 2 || err != nil {
		t.Fatalf("RecoverSends = %d, %v, want the two pending sends", n, err)
	}
	if n, err := s.RecoverSends(t.Context(), epoch.Add(6*time.Second)); n != 0 || err != nil {
		t.Fatalf("a second RecoverSends = %d, %v", n, err)
	}
	for _, tt := range []struct {
		g  policy.WriteGrant
		in SendReservation
	}{{g, first}, {other, second}} {
		if res := replay(t, s, tt.g, tt.in); res.Kind != ReplayedFailed || res.Status != 504 || res.Code != CodeUnknown {
			t.Fatalf("replay after recovery = %+v", res)
		}
	}
	if got := ledger(t, s, writerID, "done-key-02"); !strings.HasPrefix(got, "done ") {
		t.Fatalf("recovery touched a done send: %q", got)
	}
	next := request(t, "pending-03", alice, "d", epoch.Add(10*time.Second))
	next.Limits.PerMinute = 2
	var rl *RateLimitError
	if _, err := s.ReserveSend(g, t.Context(), next); !errors.As(err, &rl) || rl.Reason != ReasonMinute {
		t.Fatalf("a send after recovery = %v, want the recovered send still counted", err)
	}
}

func TestSendRecordsExpireAfterADay(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	in := request(t, "expiry-key1", alice, "synthetic", epoch)
	reserved(t, s, g, in)
	settle(t, s, g, Settlement{Key: in.Key, Outcome: SendUnknown, Now: epoch})
	stale := request(t, "expiry-key2", alice, "synthetic", epoch.Add(time.Hour))
	reserved(t, s, g, stale)
	reused := request(t, "expiry-key1", alice, "another text", epoch.Add(SendTTL-time.Millisecond))
	if _, err := s.ReserveSend(g, t.Context(), reused); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("a request a millisecond before expiry = %v, want ErrKeyReused", err)
	}
	var swept int
	write(t, s, func(tx *Tx) error {
		var err error
		swept, err = tx.SweepSends(epoch.Add(SendTTL - time.Millisecond))
		return err
	})
	if swept != 0 {
		t.Fatalf("SweepSends before expiry removed %d rows", swept)
	}
	reused.Now = epoch.Add(SendTTL)
	reserved(t, s, g, reused)
	write(t, s, func(tx *Tx) error {
		if _, err := tx.SweepSends(time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("SweepSends without a time = %v", err)
		}
		var err error
		swept, err = tx.SweepSends(epoch.Add(time.Hour + SendTTL))
		return err
	})
	if swept != 1 || ledger(t, s, writerID, "expiry-key2") != "none" || ledger(t, s, writerID, "expiry-key1") == "none" {
		t.Fatalf("SweepSends removed %d rows, want only the expired one", swept)
	}
}

func TestReleaseAndSettleActOnlyOnAPendingSend(t *testing.T) {
	s := inboundStore(t)
	g, other := grantFor(t, writerID, false, alice), grantFor(t, otherID, false, alice)
	in := request(t, "release-k01", alice, "synthetic", epoch)
	reserved(t, s, g, in)
	if err := s.ReleaseSend(other, t.Context(), in.Key); !errors.Is(err, ErrNotPending) {
		t.Fatalf("another client's release = %v, want ErrNotPending", err)
	}
	if _, err := s.SettleSend(other, t.Context(), Settlement{Key: in.Key, Outcome: SendUnknown, Now: epoch}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("another client's settle = %v, want ErrNotPending", err)
	}
	bad := Settlement{Key: in.Key, Outcome: SendAcked, Now: epoch, AckedAt: epoch, Sent: sentText(t, alice, "MSG-another", ownerJID, epoch)}
	if _, err := s.SettleSend(g, t.Context(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an ack for another message id = %v, want ErrInvalid", err)
	}
	for name, tt := range map[string]struct {
		g    policy.WriteGrant
		in   Settlement
		want error
	}{
		"a zero grant":        {policy.WriteGrant{}, Settlement{Key: in.Key, Outcome: SendUnknown, Now: epoch}, ErrNotAllowed},
		"no outcome":          {g, Settlement{Key: in.Key, Now: epoch}, ErrInvalid},
		"no time":             {g, Settlement{Key: in.Key, Outcome: SendUnknown}, ErrInvalid},
		"no key":              {g, Settlement{Outcome: SendUnknown, Now: epoch}, ErrInvalid},
		"an ack with no time": {g, Settlement{Key: in.Key, Outcome: SendAcked, Now: epoch, Sent: sentText(t, alice, in.MsgID, ownerJID, epoch)}, ErrInvalid},
		"a negative code":     {g, Settlement{Key: in.Key, Outcome: SendRejected, Now: epoch, WACode: -1}, ErrInvalid},
	} {
		if _, err := s.SettleSend(tt.g, t.Context(), tt.in); !errors.Is(err, tt.want) {
			t.Errorf("%s: SettleSend = %v, want %v", name, err, tt.want)
		}
	}
	if got := ledger(t, s, writerID, "release-k01"); got != "pending - 1" || scalar[int](t, s, "SELECT count(*) FROM messages WHERE from_me = 1") != 0 {
		t.Fatalf("a refused settle changed the ledger (%q) or stored a message", got)
	}
	if err := s.ReleaseSend(policy.WriteGrant{}, t.Context(), in.Key); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a release under a zero grant = %v", err)
	}
	if err := s.ReleaseSend(g, t.Context(), Key{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a release of the zero key = %v", err)
	}
	if err := s.ReleaseSend(g, t.Context(), in.Key); err != nil {
		t.Fatalf("ReleaseSend: %v", err)
	}
	if got := ledger(t, s, writerID, "release-k01"); got != "none" {
		t.Fatalf("ledger after release = %q", got)
	}
	if err := s.ReleaseSend(g, t.Context(), in.Key); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a second release = %v, want ErrNotPending", err)
	}
	if _, err := s.SettleSend(g, t.Context(), Settlement{Key: in.Key, Outcome: SendUnknown, Now: epoch}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a settle after release = %v, want ErrNotPending", err)
	}
	reserved(t, s, g, in)
	acked(t, s, g, in, ownerJID, epoch)
	if _, err := s.SettleSend(g, t.Context(), Settlement{Key: in.Key, Outcome: SendUnknown, Now: epoch}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a second settle = %v, want ErrNotPending", err)
	}
	if err := s.ReleaseSend(g, t.Context(), in.Key); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a release of a done send = %v, want ErrNotPending", err)
	}
}

func TestTheLedgerHoldsNoTextAndNoChat(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	const text = "synthetic secret words"
	for i, outcome := range []SendOutcome{SendAcked, SendRejected, SendUnknown} {
		in := request(t, "content-0"+string(rune('1'+i)), alice, text, epoch.Add(time.Duration(i)*time.Minute))
		reserved(t, s, g, in)
		st := Settlement{Key: in.Key, Outcome: outcome, Now: in.Now, AckedAt: in.Now, WACode: 479, Sent: sentText(t, alice, in.MsgID, ownerJID, in.Now)}
		st.Sent.Text = text
		settle(t, s, g, st)
	}
	all := scalar[string](t, s, "SELECT group_concat(client_id || key || state || hex(fingerprint) || msg_id || reserved_at || next_ok_at || counted || coalesce(settled_at, '') || coalesce(result, '') || expires_at, '|') FROM idempotency")
	for _, leak := range []string{"secret", alice, "15550100001", "ref:"} {
		if strings.Contains(all, leak) {
			t.Fatalf("the ledger holds %q: %s", leak, all)
		}
	}
}

func TestTheSendAndBackfillLimitsAreTheDocumentedOnes(t *testing.T) {
	for name, c := range map[string]struct{ got, want time.Duration }{
		"send lease":         {SendLease, 60 * time.Second},
		"send record expiry": {SendTTL, 24 * time.Hour},
		"backfill interval":  {BackfillInterval, 5 * time.Minute},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if BackfillPerDay != 20 || MaxBackfillCount != 50 {
		t.Errorf("backfills per day = %d and most messages per backfill = %d, want 20 and 50", BackfillPerDay, MaxBackfillCount)
	}
}
