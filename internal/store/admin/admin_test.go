package admin_test

import (
	"bytes"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)

func chat(t *testing.T, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q)", jid)
	}
	return c
}

func TestCountersCarryCountsAndATimeOnly(t *testing.T) {
	typ := reflect.TypeFor[admin.Counters]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type != reflect.TypeFor[int64]() && f.Type != reflect.TypeFor[time.Time]() {
			t.Errorf("Counters.%s is a %v: the status route carries counts and times, never an identifier", f.Name, f.Type)
		}
	}
}

func TestCounters(t *testing.T) {
	w := logx.NewWriter(&bytes.Buffer{})
	w.SetKey(make([]byte, 32))
	s, err := ingest.Open(t.Context(), ingest.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logx.New(w, slog.LevelInfo)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	empty, err := s.Admin().Counters(t.Context())
	if err != nil || empty != (admin.Counters{}) {
		t.Fatalf("Counters of an empty archive = %+v, %v", empty, err)
	}
	ingested := time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC)
	err = s.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		for _, m := range []struct{ chat, id string }{
			{"15550100001@s.whatsapp.net", "A1"}, {"15550100001@s.whatsapp.net", "A2"}, {"120363000000000001@g.us", "G1"},
		} {
			if _, _, err := tx.InsertMessage(ingest.Message{
				Chat: chat(t, m.chat), ID: m.id, Sender: chat(t, "15550100001@s.whatsapp.net"), Origin: ingest.OriginLive,
				Timestamp: ingested.Add(-time.Hour), Kind: ingest.KindText, Text: "synthetic", Ingested: ingested,
			}); err != nil {
				return err
			}
		}
		for _, id := range []string{"pending-1", "pending-2", "quarantined", "processed"} {
			if _, err := tx.RecordBlob(id, []byte("ref"), ingested); err != nil {
				return err
			}
		}
		if err := tx.QuarantineBlob("quarantined"); err != nil {
			return err
		}
		if err := tx.MarkBlobProcessed("processed", ingested); err != nil {
			return err
		}
		var last int64
		for range 3 {
			seq, err := tx.AppendInbox([]byte("event"), ingested)
			if err != nil {
				return err
			}
			last = seq
		}
		return tx.QuarantineInbox(last)
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Admin().Counters(t.Context())
	want := admin.Counters{Chats: 2, Messages: 3, BlobsPending: 2, BlobsQuarantined: 1, InboxBacklog: 2, InboxQuarantined: 1, LastIngest: ingested}
	if err != nil || got != want {
		t.Fatalf("Counters = %+v, %v, want %+v", got, err, want)
	}
}

func TestMediaCountsCarryCountsOnly(t *testing.T) {
	typ := reflect.TypeFor[admin.MediaCounts]()
	for i := range typ.NumField() {
		if f := typ.Field(i); f.Type != reflect.TypeFor[int64]() {
			t.Errorf("MediaCounts.%s is a %v: the status route carries counts, never an identifier", f.Name, f.Type)
		}
	}
}

func TestMediaCountsSplitLiveAndExpiredReferencesUntilTheSweep(t *testing.T) {
	w := logx.NewWriter(&bytes.Buffer{})
	w.SetKey(make([]byte, 32))
	s, err := ingest.Open(t.Context(), ingest.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logx.New(w, slog.LevelInfo)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC)
	if got, err := s.Admin().MediaCounts(t.Context(), now); err != nil || got != (admin.MediaCounts{}) {
		t.Fatalf("MediaCounts of an empty archive = %+v, %v", got, err)
	}
	alice := chat(t, "15550100001@s.whatsapp.net")
	fill := func(tx *ingest.Tx) error {
		for _, id := range []string{"M1", "M2", "M3"} {
			if _, _, err := tx.InsertMessage(ingest.Message{
				Chat: alice, ID: id, Sender: alice, Origin: ingest.OriginLive, Timestamp: now.Add(-time.Minute), Kind: ingest.KindMedia,
				MediaType: "image/jpeg", Ingested: now.Add(-time.Minute),
				Media: &ingest.Media{Ref: "synthetic-ref-" + id, Kind: ingest.MediaImage, Declared: 1024, Retention: 24 * time.Hour},
			}); err != nil {
				return err
			}
		}
		if err := tx.ExpireMediaRef(alice, "M3", alice, now); err != nil {
			return err
		}
		for _, id := range []string{"R1", "R2"} {
			if err := tx.PutRevokeTombstone(alice, id, alice, now); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.Write(t.Context(), "test.fill", fill); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Admin().MediaCounts(t.Context(), now)
	if want := (admin.MediaCounts{RefsLive: 2, RefsExpired: 1, Tombstones: 2}); err != nil || got != want {
		t.Fatalf("MediaCounts = %+v, %v, want %+v", got, err, want)
	}
	later := now.Add(48 * time.Hour)
	if got, err := s.Admin().MediaCounts(t.Context(), later); err != nil || got != (admin.MediaCounts{RefsExpired: 3, Tombstones: 2}) {
		t.Fatalf("MediaCounts after every expiry = %+v, %v, want three expired references and two tombstones", got, err)
	}
	err = s.Write(t.Context(), "test.sweep", func(tx *ingest.Tx) error {
		_, err := tx.PurgeMediaRefs(now, 24*time.Hour, true, 100)
		return err
	})
	if err != nil {
		t.Fatalf("PurgeMediaRefs: %v", err)
	}
	got, err = s.Admin().MediaCounts(t.Context(), now)
	if want := (admin.MediaCounts{RefsLive: 2, Tombstones: 2}); err != nil || got != want {
		t.Fatalf("MediaCounts after the sweep = %+v, %v, want %+v", got, err, want)
	}
}
