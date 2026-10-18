package ingest

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

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

func backfillStore(t *testing.T) *Store {
	t.Helper()
	s := openStore(t)
	newer := textMessage(t, alice, "NEW", alice, "synthetic newer")
	newer.Timestamp = epoch.Add(time.Hour)
	insert(t, s, newer)
	older := textMessage(t, alice, "OLD", ownerJID, "synthetic older")
	older.FromMe = true
	insert(t, s, older)
	return s
}

func backfill(t *testing.T, s *Store, g policy.AdminGrant, id string, at time.Time) (BackfillTicket, error) {
	t.Helper()
	return anchored(t, s, g, id, AnchorOldest, at)
}

func anchored(t *testing.T, s *Store, g policy.AdminGrant, id string, anchor AnchorKind, at time.Time) (BackfillTicket, error) {
	t.Helper()
	return s.ReserveBackfill(g, t.Context(), BackfillRequest{ID: id, Chat: chat(t, alice), Count: MaxBackfillCount, Anchor: anchor, Now: at})
}

func quotaRefused(t *testing.T, s *Store, g policy.AdminGrant, at time.Time, daily bool, retry time.Duration) {
	t.Helper()
	anchorRefused(t, s, g, AnchorOldest, at, daily, retry)
}

func anchorRefused(t *testing.T, s *Store, g policy.AdminGrant, anchor AnchorKind, at time.Time, daily bool, retry time.Duration) {
	t.Helper()
	_, err := anchored(t, s, g, "refused", anchor, at)
	var q *QuotaError
	if !errors.As(err, &q) || q.Daily != daily || q.RetryAfter != retry {
		t.Fatalf("ReserveBackfill (anchor %d) at %v = %v, want a quota refusal (daily %v) retrying after %v", anchor, at, err, daily, retry)
	}
}

func TestABackfillStartsAtTheChatsOldestMessage(t *testing.T) {
	s := backfillStore(t)
	g := adminGrant(t)
	if _, err := s.ReserveBackfill(g, t.Context(), BackfillRequest{ID: "P1", Chat: chat(t, bob), Count: 1, Now: epoch}); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("a chat without messages = %v, want ErrNoAnchor", err)
	}
	ticket, err := backfill(t, s, g, "P2", epoch)
	if err != nil {
		t.Fatalf("ReserveBackfill after a refusal without anchor = %v, want nothing consumed", err)
	}
	want := BackfillTicket{ID: "P2", Chat: chat(t, alice), Count: MaxBackfillCount, Anchor: BackfillAnchor{ID: "OLD", FromMe: true, Timestamp: epoch}}
	if ticket != want {
		t.Fatalf("ticket = %+v, want %+v", ticket, want)
	}
	if res := learn(t, s, aliceLID, alice, MappingSenderAlt); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID = %+v", res)
	}
	ticket, err = backfill(t, s, g, "P3", epoch.Add(BackfillInterval))
	if err != nil || ticket.Chat != chat(t, aliceLID) || ticket.Anchor.ID != "OLD" {
		t.Fatalf("a backfill of a re-keyed number = %+v, %v, want the LID chat", ticket, err)
	}
	for name, tt := range map[string]struct {
		g    policy.AdminGrant
		in   BackfillRequest
		want error
	}{
		"a zero grant":      {policy.AdminGrant{}, BackfillRequest{ID: "X", Chat: chat(t, alice), Count: 1, Now: epoch}, ErrNotAllowed},
		"no count":          {g, BackfillRequest{ID: "X", Chat: chat(t, alice), Count: 0, Now: epoch}, ErrInvalid},
		"too many messages": {g, BackfillRequest{ID: "X", Chat: chat(t, alice), Count: MaxBackfillCount + 1, Now: epoch}, ErrInvalid},
		"no request id":     {g, BackfillRequest{Chat: chat(t, alice), Count: 1, Now: epoch}, ErrInvalid},
		"no chat":           {g, BackfillRequest{ID: "X", Count: 1, Now: epoch}, ErrInvalid},
		"no time":           {g, BackfillRequest{ID: "X", Chat: chat(t, alice), Count: 1}, ErrInvalid},
		"an unknown anchor": {g, BackfillRequest{ID: "X", Chat: chat(t, alice), Count: 1, Anchor: AnchorNewest + 1, Now: epoch}, ErrInvalid},
	} {
		if _, err := s.ReserveBackfill(tt.g, t.Context(), tt.in); !errors.Is(err, tt.want) {
			t.Errorf("%s: ReserveBackfill = %v, want %v", name, err, tt.want)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM backfills"); n != 2 {
		t.Fatalf("%d backfill rows, want the two reserved", n)
	}
}

func TestANewestAnchoredBackfillStartsAtTheChatsNewestMessage(t *testing.T) {
	s := backfillStore(t)
	g := adminGrant(t)
	if _, err := s.ReserveBackfill(g, t.Context(), BackfillRequest{ID: "P1", Chat: chat(t, bob), Count: 1, Anchor: AnchorNewest, Now: epoch}); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("a chat without messages = %v, want ErrNoAnchor", err)
	}
	ticket, err := anchored(t, s, g, "P2", AnchorNewest, epoch)
	want := BackfillTicket{ID: "P2", Chat: chat(t, alice), Count: MaxBackfillCount, Anchor: BackfillAnchor{ID: "NEW", Timestamp: epoch.Add(time.Hour)}}
	if err != nil || ticket != want {
		t.Fatalf("ticket = %+v, %v, want %+v", ticket, err, want)
	}
	for _, id := range []string{"TIE-1", "TIE-2"} {
		tie := textMessage(t, alice, id, ownerJID, "synthetic tie")
		tie.FromMe = true
		if id == "TIE-1" {
			tie.Timestamp = epoch.Add(time.Hour)
		}
		insert(t, s, tie)
	}
	for i, tt := range []struct {
		anchor AnchorKind
		want   BackfillAnchor
	}{
		{AnchorNewest, BackfillAnchor{ID: "TIE-1", FromMe: true, Timestamp: epoch.Add(time.Hour)}},
		{AnchorOldest, BackfillAnchor{ID: "OLD", FromMe: true, Timestamp: epoch}},
	} {
		at := epoch.Add(time.Duration(i+1) * BackfillInterval)
		if ticket, err := anchored(t, s, g, "P-tie-"+strconv.Itoa(i), tt.anchor, at); err != nil || ticket.Anchor != tt.want {
			t.Fatalf("anchor %d over a timestamp tie = %+v, %v, want %+v, the tie broken by arrival", tt.anchor, ticket.Anchor, err, tt.want)
		}
	}
	if ticket, err := s.ReserveBackfill(g, t.Context(), BackfillRequest{ID: "P-default", Chat: chat(t, alice), Count: 1, Now: epoch.Add(3 * BackfillInterval)}); err != nil || ticket.Anchor.ID != "OLD" {
		t.Fatalf("a request that names no anchor = %+v, %v, want the oldest message", ticket.Anchor, err)
	}
}

func TestBothAnchorsShareOneQuota(t *testing.T) {
	s := backfillStore(t)
	g := adminGrant(t)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if _, err := anchored(t, s, g, "first", AnchorNewest, day); err != nil {
		t.Fatalf("ReserveBackfill: %v", err)
	}
	anchorRefused(t, s, g, AnchorOldest, day.Add(time.Minute), false, 4*time.Minute)
	anchorRefused(t, s, g, AnchorNewest, day.Add(time.Minute), false, 4*time.Minute)
	for i := 1; i < BackfillPerDay; i++ {
		anchor := AnchorKind(i % 2)
		if _, err := anchored(t, s, g, "day-"+strconv.Itoa(i), anchor, day.Add(time.Duration(i)*BackfillInterval)); err != nil {
			t.Fatalf("backfill %d of the day (anchor %d): %v", i+1, anchor, err)
		}
		if i < BackfillPerDay-1 {
			anchorRefused(t, s, g, 1-anchor, day.Add(time.Duration(i)*BackfillInterval+time.Minute), false, 4*time.Minute)
		}
	}
	last := day.Add((BackfillPerDay - 1) * BackfillInterval)
	for _, anchor := range []AnchorKind{AnchorOldest, AnchorNewest} {
		anchorRefused(t, s, g, anchor, last.Add(BackfillInterval), true, day.Add(24*time.Hour).Sub(last.Add(BackfillInterval)))
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM backfills"); n != BackfillPerDay {
		t.Fatalf("%d backfill rows, want %d", n, BackfillPerDay)
	}
}

func TestBackfillQuotas(t *testing.T) {
	s := backfillStore(t)
	g := adminGrant(t)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if _, err := backfill(t, s, g, "late", day.Add(-2*time.Minute)); err != nil {
		t.Fatalf("ReserveBackfill: %v", err)
	}
	quotaRefused(t, s, g, day.Add(2*time.Minute), false, time.Minute)
	quotaRefused(t, s, g, day.Add(3*time.Minute-time.Millisecond), false, time.Second)
	for i := range BackfillPerDay {
		if _, err := backfill(t, s, g, "day-"+string(rune('a'+i)), day.Add(3*time.Minute+time.Duration(i)*BackfillInterval)); err != nil {
			t.Fatalf("backfill %d of the day: %v", i+1, err)
		}
	}
	last := day.Add(3*time.Minute + (BackfillPerDay-1)*BackfillInterval)
	quotaRefused(t, s, g, last.Add(time.Minute), true, day.Add(24*time.Hour).Sub(last.Add(time.Minute)))
	quotaRefused(t, s, g, last.Add(BackfillInterval), true, day.Add(24*time.Hour).Sub(last.Add(BackfillInterval)))
	zone := time.FixedZone("east", 5*3600)
	quotaRefused(t, s, g, day.Add(24*time.Hour-time.Second).In(zone), true, time.Second)
	if _, err := backfill(t, s, g, "next-day", day.Add(24*time.Hour).In(zone)); err != nil {
		t.Fatalf("the first backfill of the next UTC day: %v", err)
	}
	read(t, s, func(r *Reader) error {
		for _, tt := range []struct {
			since time.Time
			want  int
		}{{day, BackfillPerDay + 1}, {day.Add(24 * time.Hour), 1}, {day.Add(-time.Hour), BackfillPerDay + 2}} {
			if n, err := r.BackfillsSince(tt.since); err != nil || n != tt.want {
				t.Errorf("BackfillsSince(%v) = %d, %v, want %d", tt.since, n, err, tt.want)
			}
		}
		if _, err := r.BackfillsSince(time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("BackfillsSince without a time = %v", err)
		}
		return nil
	})
}

func TestABackfillIsBoundCancelledAndCompletedByItsRequestID(t *testing.T) {
	s := backfillStore(t)
	g := adminGrant(t)
	if _, err := backfill(t, s, g, "provisional-1", epoch); err != nil {
		t.Fatalf("ReserveBackfill: %v", err)
	}
	if err := s.CancelBackfill(t.Context(), "provisional-1"); err != nil {
		t.Fatalf("CancelBackfill: %v", err)
	}
	if _, err := backfill(t, s, g, "provisional-2", epoch.Add(time.Second)); err != nil {
		t.Fatalf("a backfill after a cancelled one = %v, want the quota refunded", err)
	}
	if err := s.BindBackfill(t.Context(), "provisional-2", "3EB0REQUEST"); err != nil {
		t.Fatalf("BindBackfill: %v", err)
	}
	for name, err := range map[string]error{
		"a second bind":                 s.BindBackfill(t.Context(), "provisional-2", "3EB0OTHER"),
		"a cancel of the bound request": s.CancelBackfill(t.Context(), "provisional-2"),
		"a cancel of an unknown id":     s.CancelBackfill(t.Context(), "unknown"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
	for name, err := range map[string]error{
		"a bind to an invalid id": s.BindBackfill(t.Context(), "3EB0REQUEST", "has space"),
		"a cancel of no id":       s.CancelBackfill(t.Context(), ""),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	complete := func(id string, at time.Time) (bool, error) {
		var ok bool
		err := s.Write(t.Context(), "test.complete", func(tx *Tx) error {
			var err error
			ok, err = tx.CompleteBackfill(id, at)
			return err
		})
		return ok, err
	}
	if ok, err := complete("3EB0REQUEST", epoch.Add(time.Minute)); !ok || err != nil {
		t.Fatalf("CompleteBackfill = %v, %v", ok, err)
	}
	for _, id := range []string{"3EB0REQUEST", "provisional-2", "unknown", "has space"} {
		if ok, err := complete(id, epoch.Add(2*time.Minute)); ok || err != nil {
			t.Fatalf("CompleteBackfill(%q) = %v, %v, want false", id, ok, err)
		}
	}
	if _, err := complete("3EB0REQUEST", time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CompleteBackfill without a time = %v", err)
	}
	if err := s.CancelBackfill(t.Context(), "3EB0REQUEST"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a cancel of a completed request = %v, want ErrNotFound", err)
	}
	if got := scalar[string](t, s, "SELECT request_id || ' ' || completed_at FROM backfills"); got != "3EB0REQUEST "+formatMS(epoch.Add(time.Minute)) {
		t.Fatalf("backfills = %q", got)
	}
}
