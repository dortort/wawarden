package inspect

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/session"
)

var epoch = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const canary = "15550100042"

func chat(t *testing.T, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q)", jid)
	}
	return c
}

func master(t *testing.T) *keys.Master {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	m, r := keys.LoadFile(path)
	if r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	return m
}

func seeded(t *testing.T) (archive, device string, deviceVersion int) {
	t.Helper()
	dir := t.TempDir()
	logger := logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)
	out := logx.NewWriter(io.Discard)
	out.SetKey(make([]byte, 32))
	s, err := ingest.Open(t.Context(), ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger, Master: master(t), AuditOut: out})
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}
	if err := s.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		for _, m := range []struct{ chat, id string }{{canary + "@s.whatsapp.net", "A1"}, {canary + "@s.whatsapp.net", "A2"}, {"120363000000000001@g.us", "G1"}} {
			if _, _, err := tx.InsertMessage(ingest.Message{Chat: chat(t, m.chat), ID: m.id, Sender: chat(t, canary+"@s.whatsapp.net"), Origin: ingest.OriginLive,
				Timestamp: epoch, Kind: ingest.KindText, Text: "synthetic words about the harbour", Ingested: epoch}); err != nil {
				return err
			}
		}
		for _, id := range []string{"M1", "M2", "M3"} {
			if _, _, err := tx.InsertMessage(ingest.Message{Chat: chat(t, canary+"@s.whatsapp.net"), ID: id, Sender: chat(t, canary+"@s.whatsapp.net"),
				Origin: ingest.OriginLive, Timestamp: epoch, Kind: ingest.KindMedia, MediaType: "image/jpeg", Ingested: epoch,
				Media: &ingest.Media{Ref: "synthetic-ref-" + id, Kind: ingest.MediaImage, Declared: 1024, Retention: 50 * 365 * 24 * time.Hour}}); err != nil {
				return err
			}
		}
		if err := tx.ExpireMediaRef(chat(t, canary+"@s.whatsapp.net"), "M2", chat(t, canary+"@s.whatsapp.net"), epoch); err != nil {
			return err
		}
		for _, id := range []string{"R1", "R2", "R3"} {
			if err := tx.PutRevokeTombstone(chat(t, canary+"@s.whatsapp.net"), id, chat(t, canary+"@s.whatsapp.net"), epoch); err != nil {
				return err
			}
		}
		if err := tx.SetPushName(chat(t, canary+"@s.whatsapp.net"), "Alice Example", epoch, ingest.OriginLive); err != nil {
			return err
		}
		for _, id := range []string{"pending-1", "pending-2", "quarantined", "processed"} {
			if _, err := tx.RecordBlob(id, []byte("ref"), epoch); err != nil {
				return err
			}
		}
		if err := errors.Join(tx.QuarantineBlob("quarantined"), tx.MarkBlobProcessed("processed", epoch)); err != nil {
			return err
		}
		var last int64
		for range 3 {
			seq, err := tx.AppendInbox([]byte("event"), epoch)
			if err != nil {
				return err
			}
			last = seq
		}
		return tx.QuarantineInbox(last)
	}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	var ids []string
	for _, name := range []string{"agent", "other"} {
		c, _, err := s.Clients().Create(t.Context(), policy.ClientSpec{Name: name, AllChats: true})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, c.ID)
	}
	if _, err := s.Clients().Revoke(t.Context(), ids[0]); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := s.Audit().Record(t.Context(), admin.Event{At: epoch, Client: ids[1], Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sess, err := session.Open(t.Context(), session.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	deviceVersion = sess.SchemaVersion()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	archiveName, deviceName := Files()
	return filepath.Join(dir, archiveName), filepath.Join(dir, deviceName), deviceVersion
}

func TestTheFilesAndTheLatestVersionAreTheStores(t *testing.T) {
	if a, s := Files(); a != "archive.db" || s != "session.db" {
		t.Fatalf("Files = %q, %q", a, s)
	}
	if LatestArchiveVersion() != 5 {
		t.Fatalf("LatestArchiveVersion = %d, want 5", LatestArchiveVersion())
	}
	if free, err := FreeBytes(t.TempDir()); err != nil || free == 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
	if _, err := FreeBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("FreeBytes of a missing directory succeeded")
	}
}

func TestCheckCountsBothCopies(t *testing.T) {
	archivePath, devicePath, deviceVersion := seeded(t)
	before, err := os.ReadFile(archivePath) //nolint:gosec // G304: the test's own archive
	if err != nil {
		t.Fatal(err)
	}
	sides, _ := filepath.Glob(archivePath + "-*")
	a, err := CheckArchive(t.Context(), archivePath)
	want := Archive{Integrity: Integrity{OK: true, Counted: true}, SchemaVersion: 5, Chats: 2, Messages: 6, BlobsPending: 2, BlobsQuarantined: 1,
		InboxBacklog: 2, InboxQuarantined: 1, ClientsActive: 1, AuditRows: 4, MediaRefsLive: 2, MediaRefsExpired: 1, RevokeTombstones: 3}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Fatalf("CheckArchive = %+v, %v, want %+v", a, err, want)
	}
	d, err := CheckDevice(t.Context(), devicePath)
	if err != nil || !reflect.DeepEqual(d, Device{Integrity: Integrity{OK: true, Counted: true}, SchemaVersion: deviceVersion}) || deviceVersion < 1 {
		t.Fatalf("CheckDevice = %+v, %v, want version %d and nothing paired", d, err, deviceVersion)
	}
	after, err := os.ReadFile(archivePath) //nolint:gosec // G304: the test's own archive
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the check changed the copy: %v", err)
	}
	if after, _ := filepath.Glob(archivePath + "-*"); !slices.Equal(after, sides) {
		t.Fatalf("the check left %v beside the copy, which had %v", after, sides)
	}
}

func TestTheReportsHoldCountsOnly(t *testing.T) {
	for _, v := range []any{Archive{}, Device{}} {
		ty := reflect.TypeOf(v)
		for i := range ty.NumField() {
			f := ty.Field(i)
			switch f.Type {
			case reflect.TypeFor[int](), reflect.TypeFor[int64](), reflect.TypeFor[bool](), reflect.TypeFor[Integrity]():
			default:
				t.Errorf("%s.%s is a %v: a report carries counts, never an identifier", ty.Name(), f.Name, f.Type)
			}
		}
	}
}

func tamper(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(t.Context(), statement)
	if err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		t.Fatalf("%s changed %d rows, %v", statement, n, err)
	}
}

func TestACopyOlderThanTheMediaTablesIsCountedWithoutThem(t *testing.T) {
	archivePath, _, _ := seeded(t)
	db, err := sql.Open("sqlite", "file:"+archivePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"DROP TABLE media_refs", "DROP TABLE revoke_tombstones", "PRAGMA user_version = 4"} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := CheckArchive(t.Context(), archivePath)
	want := Archive{Integrity: Integrity{OK: true, Counted: true}, SchemaVersion: 4, Chats: 2, Messages: 6, BlobsPending: 2, BlobsQuarantined: 1,
		InboxBacklog: 2, InboxQuarantined: 1, ClientsActive: 1, AuditRows: 4}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Fatalf("CheckArchive of a version 4 copy = %+v, %v, want %+v", a, err, want)
	}
}

func TestATamperedFullTextIndexFailsTheIntegrityCheck(t *testing.T) {
	archivePath, _, _ := seeded(t)
	tamper(t, archivePath, "UPDATE messages_fts_data SET block = zeroblob(length(block)) WHERE id > 10")
	a, err := CheckArchive(t.Context(), archivePath)
	if err != nil || a.OK || a.Counted || len(a.Problems) == 0 || len(a.Problems) > 5 || a.Messages != 0 {
		t.Fatalf("CheckArchive of a tampered index = %+v, %v", a, err)
	}
	if !strings.Contains(a.Problems[0], "fts5") {
		t.Fatalf("the integrity check reported %q, want the full-text index's corruption", a.Problems)
	}
}

func TestACopyThatIsNotADatabaseFailsTheIntegrityCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(path, bytes.Repeat([]byte("synthetic "), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := CheckArchive(t.Context(), path)
	if err != nil || a.OK || len(a.Problems) != 1 || a.Problems[0] != notReadable {
		t.Fatalf("CheckArchive = %+v, %v", a, err)
	}
	d, err := CheckDevice(t.Context(), path)
	if err != nil || d.OK || d.Counted {
		t.Fatalf("CheckDevice = %+v, %v", d, err)
	}
}

func TestASwappedCopyIsCheckedButNotCounted(t *testing.T) {
	archivePath, devicePath, _ := seeded(t)
	a, err := CheckArchive(t.Context(), devicePath)
	if err != nil || !a.OK || a.Counted {
		t.Fatalf("CheckArchive of the device store = %+v, %v", a, err)
	}
	d, err := CheckDevice(t.Context(), archivePath)
	if err != nil || !d.OK || d.Counted {
		t.Fatalf("CheckDevice of the archive = %+v, %v", d, err)
	}
}

func TestAStopEndsTheCheck(t *testing.T) {
	archivePath, _, _ := seeded(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := CheckArchive(ctx, archivePath); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckArchive after a stop = %v", err)
	}
	if a, err := CheckArchive(t.Context(), filepath.Join(t.TempDir(), "missing")); err != nil || a.OK || a.Counted {
		t.Fatalf("CheckArchive of a missing copy = %+v, %v", a, err)
	}
}
