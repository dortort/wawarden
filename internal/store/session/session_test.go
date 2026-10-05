package session

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

func options(t *testing.T, dir string) Options {
	t.Helper()
	return Options{DataDir: dir, UID: os.Geteuid(), Profile: db.Local, Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(t.Context(), options(t, dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func save(t *testing.T, s *Store, user string) {
	t.Helper()
	d := s.NewDevice()
	jid := types.JID{User: user, Device: 12, Server: types.DefaultUserServer}
	d.ID = &jid
	d.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{1},
		AccountSignature:    bytes.Repeat([]byte{2}, 64),
		AccountSignatureKey: bytes.Repeat([]byte{3}, 32),
		DeviceSignature:     bytes.Repeat([]byte{4}, 64),
	}
	if err := d.Save(t.Context()); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestOpenCreatesAPrivateUpgradedDeviceStore(t *testing.T) {
	saved := syscall.Umask(0)
	defer syscall.Umask(saved)
	dir := t.TempDir()
	s := open(t, dir)
	fi, err := os.Lstat(filepath.Join(dir, "session.db"))
	if err != nil || fi.Mode() != 0o600 {
		t.Fatalf("session.db: %v, mode %v under umask 0, want 0600", err, fi.Mode())
	}
	var version int
	if err := s.db.RawHandle().QueryRowContext(t.Context(), "SELECT version FROM whatsmeow_version").Scan(&version); err != nil || version < 1 {
		t.Fatalf("the device store's schema version = %d, %v, want an upgraded store", version, err)
	}
	d, err := s.Device(t.Context())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if d.ID != nil {
		t.Fatalf("an empty store gave a paired device %v", d.ID)
	}
	if !s.Healthy() {
		t.Fatal("an open store reports unhealthy")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.Healthy() {
		t.Fatal("a closed store reports healthy")
	}
}

func TestTheStoredDeviceSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	save(t, first, "15550100009")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := open(t, dir)
	d, err := second.Device(t.Context())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if d.ID == nil || d.ID.User != "15550100009" || d.ID.Device != 12 {
		t.Fatalf("the reopened store gave device %v, want the saved one", d.ID)
	}
	if fresh := second.NewDevice(); fresh.ID != nil || fresh.Container == nil {
		t.Fatalf("NewDevice gave %v without a container %v, want an unpaired device of this store", fresh.ID, fresh.Container == nil)
	}
}

func TestMoreThanOneStoredDeviceIsRefused(t *testing.T) {
	s := open(t, t.TempDir())
	save(t, s, "15550100009")
	save(t, s, "15550100008")
	if _, err := s.Device(t.Context()); !errors.Is(err, errManyDevices) {
		t.Fatalf("Device with two stored devices = %v, want errManyDevices", err)
	}
}

func TestOpenRefusesASessionFileItCannotTrust(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the refusal under test needs a world-readable session.db
		t.Fatalf("Chmod: %v", err)
	}
	s, err := Open(t.Context(), options(t, dir))
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a world-readable session.db")
	}
	if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "session_db_permissions" {
		t.Fatalf("Open = %v, want the refusal session_db_permissions", err)
	}
}

func TestOpenRefusesAStoreItCannotUpgrade(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.db.RawHandle().ExecContext(t.Context(), "UPDATE whatsmeow_version SET version = 1000, compat = 1000"); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := Open(t.Context(), options(t, dir))
	if err == nil {
		_ = again.Close()
		t.Fatal("Open accepted a device store newer than the protocol library knows")
	}
	if _, err := os.Lstat(filepath.Join(dir, "session.db")); errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused upgrade removed session.db")
	}
	o := options(t, dir)
	d, err := db.Open(t.Context(), db.Session, db.Options{DataDir: o.DataDir, UID: o.UID, Profile: o.Profile, Logger: o.Logger})
	if err != nil {
		t.Fatalf("session.db is still held after a refused upgrade: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
