package app

import (
	"io"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/engine/wa"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/stage"
	"github.com/dortort/wawarden/internal/store/ingest"
)

func TestTheMediaSealerUsesTheMediaReferenceKeyOnly(t *testing.T) {
	master := testMaster(t)
	sealer, err := mediaSealer(master)
	if err != nil {
		t.Fatalf("mediaSealer: %v", err)
	}
	sealed, err := sealer.SealMedia("SYNTHETIC-1", []byte("synthetic descriptor"))
	if err != nil {
		t.Fatalf("SealMedia: %v", err)
	}
	purposes := map[string][]byte{
		"log redact":  master.LogRedactKey(),
		"chat hmac":   master.ChatHMACKey(),
		"cursor seal": master.CursorSealKey(),
		"message ref": master.MessageRefKey(),
		"audit chain": master.AuditChainKey(),
		"media ref":   master.MediaRefKey(),
	}
	for name, key := range purposes {
		s, err := cursor.New(key, master.ID())
		if err != nil {
			t.Fatalf("cursor.New(%s): %v", name, err)
		}
		if _, err := s.OpenMedia("SYNTHETIC-1", sealed); (err == nil) != (name == "media ref") {
			t.Errorf("the %s key opens the media reference: %v", name, err == nil)
		}
	}
	if pt, err := sealer.OpenMedia("SYNTHETIC-2", sealed); err == nil {
		t.Fatalf("a media reference opened under another message id: %q", pt)
	}
}

func TestTheMediaSealerOpensWhatARetiredKeySealed(t *testing.T) {
	previous, current := rotatedMasters(t)
	old, err := mediaSealer(previous)
	if err != nil {
		t.Fatalf("mediaSealer: %v", err)
	}
	sealed, err := old.SealMedia("SYNTHETIC-1", []byte("synthetic descriptor"))
	if err != nil {
		t.Fatalf("SealMedia: %v", err)
	}
	now, err := mediaSealer(current)
	if err != nil {
		t.Fatalf("mediaSealer: %v", err)
	}
	if pt, err := now.OpenMedia("SYNTHETIC-1", sealed); err != nil || string(pt) != "synthetic descriptor" {
		t.Fatalf("OpenMedia after rotation = %q, %v", pt, err)
	}
}

func TestNoMasterKeyGivesNoMediaSealer(t *testing.T) {
	sealer, err := mediaSealer(nil)
	if err != nil || sealer != nil {
		t.Fatalf("mediaSealer(nil) = %#v, %v, want a nil interface", sealer, err)
	}
}

func TestMediaPartsFollowTheConfiguration(t *testing.T) {
	data := t.TempDir()
	for _, tt := range []struct {
		max       int64
		days      int
		retention time.Duration
	}{
		{max: 0, days: 30, retention: 30 * 24 * time.Hour},
		{max: 1 << 20, days: 1, retention: 24 * time.Hour},
		{max: 2 << 30, days: 365, retention: 365 * 24 * time.Hour},
	} {
		cfg := testConfig(t, "")
		cfg.MediaMaxBytes, cfg.MediaRefRetentionDays = tt.max, tt.days
		m, err := newMediaParts(cfg, data, testMaster(t))
		if err != nil {
			t.Fatalf("newMediaParts: %v", err)
		}
		if m.maxBytes != tt.max || m.retention != tt.retention || m.sealer == nil || (m.spool != nil) != (tt.max > 0) {
			t.Fatalf("media parts for %d bytes and %d days = %+v", tt.max, tt.days, m)
		}
	}
}

func TestTheAdapterOptionsCarryTheMediaParts(t *testing.T) {
	sealer, err := mediaSealer(testMaster(t))
	if err != nil {
		t.Fatalf("mediaSealer: %v", err)
	}
	spool, err := stage.New(stage.Options{Data: t.TempDir(), Local: true})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	m := mediaParts{maxBytes: 32 << 20, retention: 7 * 24 * time.Hour, sealer: sealer, spool: spool}
	o := m.adapterOptions(wa.Options{OwnerPhone: "+15550100009", HistoryMaxBytes: 1 << 20})
	if o.MediaMaxBytes != m.maxBytes || o.MediaRefRetention != m.retention || o.MediaSealer != sealer || o.MediaSpool != spool {
		t.Fatalf("adapter options carry %d bytes, %v, %v, %v; want %+v", o.MediaMaxBytes, o.MediaRefRetention, o.MediaSealer, o.MediaSpool, m)
	}
	if o.OwnerPhone != "+15550100009" || o.HistoryMaxBytes != 1<<20 {
		t.Fatalf("adapter options lost the other settings: %q, %d", o.OwnerPhone, o.HistoryMaxBytes)
	}
}

func TestTheReleaseEngineTakesTheMediaOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		max     int64
		master  bool
		started bool
	}{
		{name: "downloads off without a master key", started: true},
		{name: "downloads on in memory", max: 16 << 20, master: true, started: true},
		{name: "downloads on with a disk spool", max: 32 << 20, master: true, started: true},
		{name: "downloads on without a reference sealer", max: 1 << 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mediaConfig(t, tt.max)
			master := testMaster(t)
			if !tt.master {
				master = nil
			}
			a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), master, noClients{}, openWhatsApp)
			if err == nil {
				t.Cleanup(func() { _ = run(t, a)() })
			}
			if (err == nil) != tt.started {
				t.Fatalf("newAppWith = %v, want a start %v", err, tt.started)
			}
			if err != nil {
				requireReleased(t, cfg.DataDir)
			}
		})
	}
}

func TestTheAttachmentSwitchAndRetentionReachTheEngine(t *testing.T) {
	const (
		groupJID = "120363000000000041@g.us"
		bobPN    = "15550100042@s.whatsapp.net"
	)
	for _, tt := range []struct {
		name string
		max  int64
		days int
		want int
	}{
		{name: "downloads off", days: 30},
		{name: "downloads on", max: 1 << 20, days: 30, want: 1},
		{name: "downloads on past the retention", max: 1 << 20, days: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, "")
			cfg.MediaMaxBytes, cfg.MediaRefRetentionDays = tt.max, tt.days
			master := testMaster(t)
			stub := newStubClient()
			a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), master, noClients{},
				fixed(engineParts{client: stub, versions: stubVersions{}, decoder: stubDecoder{}}))
			if err != nil {
				t.Fatalf("newAppWith: %v", err)
			}
			run(t, a)
			<-stub.connected
			if !stub.emit(engine.Connected{}) {
				t.Fatal("the engine handler is missing")
			}
			waitUntil(t, "ready", a.engine.Ready)
			sealer, err := mediaSealer(master)
			if err != nil {
				t.Fatalf("mediaSealer: %v", err)
			}
			ref, err := sealer.SealMedia("M1", []byte("synthetic descriptor"))
			if err != nil {
				t.Fatalf("SealMedia: %v", err)
			}
			sent := time.Now().Add(-48 * time.Hour)
			stub.emit(engine.Group{Chat: groupJID, Subject: "Synthetic group", Timestamp: sent,
				Members: []engine.Participant{{User: "15550100009@s.whatsapp.net", Admin: true}, {User: bobPN}}})
			stub.emit(engine.Message{Chat: groupJID, ID: "M1", Sender: bobPN, Timestamp: sent, Kind: engine.KindMedia,
				MediaType: "image/jpeg", MediaRef: ref, MediaKind: engine.MediaImage, MediaBytes: 1024})
			group, _ := policy.Normalize(groupJID)
			bob, _ := policy.Normalize(bobPN)
			waitUntil(t, "M1 archived", func() bool {
				var ok bool
				_ = a.archive.Read(t.Context(), "test.archived", func(r *ingest.Reader) error {
					var err error
					_, ok, err = r.ResolveInChat(group, "M1", bob)
					return err
				})
				return ok
			})
			var stored int
			if err := a.archive.Write(t.Context(), "test.count_refs", func(tx *ingest.Tx) error {
				var err error
				stored, err = tx.PurgeMediaRefs(time.Now(), config.MaxMediaRefRetentionDays*24*time.Hour, false, 10)
				return err
			}); err != nil {
				t.Fatalf("PurgeMediaRefs: %v", err)
			}
			if stored != tt.want {
				t.Fatalf("%d media references stored, want %d", stored, tt.want)
			}
		})
	}
}
