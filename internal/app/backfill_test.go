package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

type scriptedBackfiller struct {
	ids   int
	calls []engine.BackfillRequest
	id    string
	err   error
}

func (b *scriptedBackfiller) NewMessageID() string {
	b.ids++
	return "3EB0PROVISIONAL" + strconv.Itoa(b.ids)
}

func (b *scriptedBackfiller) Backfill(_ context.Context, req engine.BackfillRequest) (string, error) {
	b.calls = append(b.calls, req)
	return b.id, b.err
}

func backfillArchive(t *testing.T) (*ingest.Store, policy.CanonicalChat, time.Time) {
	t.Helper()
	w := logx.NewWriter(&syncBuffer{})
	w.SetKey(make([]byte, 32))
	s, err := ingest.Open(t.Context(), ingest.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1, Logger: logx.New(w, slog.LevelDebug)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	group, _ := policy.Normalize("120363000000000001@g.us")
	member, _ := policy.Normalize("15550100002@s.whatsapp.net")
	oldest := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	if err := s.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		for i, id := range []string{"OLDEST", "NEWER"} {
			at := oldest.Add(time.Duration(i) * time.Hour)
			if _, _, err := tx.InsertMessage(ingest.Message{Chat: group, ID: id, Sender: member, Origin: ingest.OriginLive, Timestamp: at,
				Kind: ingest.KindText, Text: "synthetic", Ingested: at}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return s, group, oldest
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

func backfillsSince(t *testing.T, s *ingest.Store, since time.Time) int {
	t.Helper()
	var n int
	if err := s.Read(t.Context(), "test.count", func(r *ingest.Reader) error {
		var err error
		n, err = r.BackfillsSince(since)
		return err
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return n
}

func TestABackfillAsksForTheMessagesBeforeTheOldestRow(t *testing.T) {
	s, group, oldest := backfillArchive(t)
	g := adminGrant(t)
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	e := &scriptedBackfiller{id: "3EB0REQUEST"}
	id, err := requestBackfill(g, t.Context(), s, e, now, group, 20, api.BackfillOldest)
	if err != nil || id != "3EB0REQUEST" {
		t.Fatalf("requestBackfill = %q, %v", id, err)
	}
	want := engine.BackfillRequest{Chat: group.JID(), AnchorID: "OLDEST", AnchorTime: oldest, Count: 20}
	if len(e.calls) != 1 || !reflect.DeepEqual(e.calls[0], want) {
		t.Fatalf("engine requests = %+v, want %+v", e.calls, want)
	}
	if err := s.Write(t.Context(), "test.complete", func(tx *ingest.Tx) error {
		done, err := tx.CompleteBackfill("3EB0REQUEST", now.Add(time.Minute))
		if err == nil && !done {
			err = errors.New("the request id was not bound")
		}
		return err
	}); err != nil {
		t.Fatalf("CompleteBackfill: %v", err)
	}
	_, err = requestBackfill(g, t.Context(), s, e, now.Add(time.Minute), group, 20, api.BackfillOldest)
	if !reflect.DeepEqual(err, api.BackfillQuota(4*time.Minute)) {
		t.Fatalf("a second request within five minutes = %v", err)
	}
	if n := backfillsSince(t, s, now.Truncate(24*time.Hour)); n != 1 {
		t.Fatalf("%d backfills today, want 1", n)
	}
}

func TestANewestAnchoredBackfillAsksForTheMessagesBeforeTheNewestRow(t *testing.T) {
	s, group, oldest := backfillArchive(t)
	g := adminGrant(t)
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	e := &scriptedBackfiller{id: "3EB0NEWEST"}
	id, err := requestBackfill(g, t.Context(), s, e, now, group, 20, api.BackfillNewest)
	if err != nil || id != "3EB0NEWEST" {
		t.Fatalf("requestBackfill = %q, %v", id, err)
	}
	want := engine.BackfillRequest{Chat: group.JID(), AnchorID: "NEWER", AnchorTime: oldest.Add(time.Hour), Count: 20}
	if len(e.calls) != 1 || !reflect.DeepEqual(e.calls[0], want) {
		t.Fatalf("engine requests = %+v, want %+v", e.calls, want)
	}
	for _, anchor := range []api.BackfillAnchor{api.BackfillOldest, api.BackfillNewest} {
		if _, err := requestBackfill(g, t.Context(), s, e, now.Add(time.Minute), group, 20, anchor); !reflect.DeepEqual(err, api.BackfillQuota(4*time.Minute)) {
			t.Fatalf("a request anchored %d a minute after a newest-anchored one = %v, want the shared quota", anchor, err)
		}
	}
	if len(e.calls) != 1 {
		t.Fatalf("%d engine requests, want 1", len(e.calls))
	}
}

func TestABackfillThatSentNothingIsRefunded(t *testing.T) {
	for _, tt := range []struct {
		id   string
		err  error
		want error
	}{
		{err: engine.ErrNotConnected, want: api.ErrNotConnected},
		{err: engine.ErrSendBusy, want: api.RateLimited(time.Second)},
		{id: "3EB0NOTSENT", err: engine.ErrNotConnected, want: api.ErrNotConnected},
		{id: "3EB0NOTSENT", err: engine.ErrSendBusy, want: api.RateLimited(time.Second)},
	} {
		s, group, _ := backfillArchive(t)
		g := adminGrant(t)
		now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
		e := &scriptedBackfiller{id: tt.id, err: tt.err}
		if _, err := requestBackfill(g, t.Context(), s, e, now, group, 50, api.BackfillOldest); !reflect.DeepEqual(err, tt.want) {
			t.Fatalf("requestBackfill with %v = %v, want %v", tt.err, err, tt.want)
		}
		if n := backfillsSince(t, s, now.Truncate(24*time.Hour)); n != 0 {
			t.Fatalf("%d backfills counted after %v, want none", n, tt.err)
		}
		e.err, e.id = nil, "3EB0RETRY"
		if id, err := requestBackfill(g, t.Context(), s, e, now, group, 50, api.BackfillOldest); err != nil || id != "3EB0RETRY" {
			t.Fatalf("the retry = %q, %v", id, err)
		}
	}
}

func TestABackfillThatMayHaveLeftStaysCounted(t *testing.T) {
	for _, tt := range []struct {
		id   string
		err  error
		want error
	}{
		{id: "3EB0REJECTED", err: &engine.RejectedError{Code: 479}, want: api.ErrSendRejected},
		{err: engine.ErrRecipientUnavailable, want: api.ErrSendRejected},
		{id: "3EB0UNKNOWN", err: engine.ErrOutcomeUnknown, want: api.ErrSendOutcomeUnknown},
		{err: errors.New("synthetic unclassified"), want: api.ErrSendOutcomeUnknown},
	} {
		s, group, _ := backfillArchive(t)
		g := adminGrant(t)
		now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
		e := &scriptedBackfiller{id: tt.id, err: tt.err}
		if _, err := requestBackfill(g, t.Context(), s, e, now, group, 50, api.BackfillOldest); !reflect.DeepEqual(err, tt.want) {
			t.Fatalf("requestBackfill with %v = %v, want %v", tt.err, err, tt.want)
		}
		if n := backfillsSince(t, s, now.Truncate(24*time.Hour)); n != 1 {
			t.Fatalf("%d backfills counted after %v, want 1", n, tt.err)
		}
		if tt.id != "" {
			if err := s.Write(t.Context(), "test.complete", func(tx *ingest.Tx) error {
				done, err := tx.CompleteBackfill(tt.id, now.Add(time.Minute))
				if err == nil && !done {
					err = errors.New("the request id was not bound")
				}
				return err
			}); err != nil {
				t.Fatalf("CompleteBackfill: %v", err)
			}
		}
	}
}

func TestABackfillOfAChatWithoutRowsIsRefused(t *testing.T) {
	s, _, _ := backfillArchive(t)
	empty, _ := policy.Normalize("120363000000000009@g.us")
	e := &scriptedBackfiller{id: "3EB0X"}
	for _, anchor := range []api.BackfillAnchor{api.BackfillOldest, api.BackfillNewest} {
		if _, err := requestBackfill(adminGrant(t), t.Context(), s, e, time.Now(), empty, 50, anchor); !errors.Is(err, api.ErrBackfillNoAnchor) || len(e.calls) != 0 {
			t.Fatalf("a chat without rows (anchor %d) = %v after %d engine calls", anchor, err, len(e.calls))
		}
	}
}

func TestTheStatusCountsOnlyTheBackfillsRequestedSinceUTCMidnight(t *testing.T) {
	a, err := newAppWith(t.Context(), testConfig(t, ""), logx.NewWriter(&syncBuffer{}), nil, noClients{}, idle())
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	t.Cleanup(func() { _ = run(t, a)() })
	group, _ := policy.Normalize("120363000000000001@g.us")
	member, _ := policy.Normalize("15550100002@s.whatsapp.net")
	midnight := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if err := a.archive.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		_, _, err := tx.InsertMessage(ingest.Message{Chat: group, ID: "OLDEST", Sender: member, Origin: ingest.OriginLive, Timestamp: midnight.Add(-48 * time.Hour),
			Kind: ingest.KindText, Text: "synthetic", Ingested: midnight.Add(-48 * time.Hour)})
		return err
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i, at := range []time.Time{midnight.Add(-10 * time.Minute), midnight} {
		if _, err := requestBackfill(adminGrant(t), t.Context(), a.archive, &scriptedBackfiller{id: "3EB0REQUEST" + strconv.Itoa(i)}, at, group, 20, api.BackfillOldest); err != nil {
			t.Fatalf("requestBackfill at %v: %v", at, err)
		}
	}
	st, err := adminService{engine: a.engine, archive: a.archive, now: func() time.Time { return midnight.Add(time.Hour) }, pause: &api.MediaPause{}}.Status(t.Context())
	if err != nil || st.BackfillsToday != 1 {
		t.Fatalf("Status = %d backfills today, %v; want 1", st.BackfillsToday, err)
	}
}
