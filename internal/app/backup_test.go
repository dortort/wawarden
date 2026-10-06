package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/session"
)

var backupEpoch = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

type trigger struct {
	b       *initialBackup
	clock   *testClock
	status  engine.Status
	unpairs uint64
	takes   int
	fail    bool
}

func newTrigger(t *testing.T, archive *ingest.Store) *trigger {
	t.Helper()
	tr := &trigger{clock: &testClock{now: backupEpoch}, status: engine.Status{State: engine.StateUnpaired}}
	tr.b = tr.restart(archive)
	return tr
}

func (tr *trigger) restart(archive *ingest.Store) *initialBackup {
	tr.b = &initialBackup{archive: archive, status: func() engine.Status { return tr.status }, unpairs: func() uint64 { return tr.unpairs }, now: tr.clock.Now, take: func(context.Context) error {
		tr.takes++
		if tr.fail {
			return errors.New("synthetic backup failure")
		}
		return nil
	}}
	return tr.b
}

func (tr *trigger) at(t *testing.T, offset time.Duration) int {
	t.Helper()
	tr.clock.set(backupEpoch.Add(offset))
	if err := tr.b.check(t.Context()); err != nil {
		t.Fatalf("check at +%v: %v", offset, err)
	}
	return tr.takes
}

func triggerArchive(t *testing.T) *ingest.Store {
	t.Helper()
	s, err := ingest.Open(t.Context(), ingest.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1,
		Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)})
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func syncValue(t *testing.T, s *ingest.Store, key string) string {
	t.Helper()
	var v string
	if err := s.Read(t.Context(), "test.sync", func(r *ingest.Reader) error {
		var err error
		v, _, err = r.SyncValue(key)
		return err
	}); err != nil {
		t.Fatalf("SyncValue: %v", err)
	}
	return v
}

func writeArchive(t *testing.T, s *ingest.Store, fn func(*ingest.Tx) error) {
	t.Helper()
	if err := s.Write(t.Context(), "test.write", fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

var pairedStatus = engine.Status{State: engine.StateConnected, Paired: true}

func TestTheInitialBackupWaitsForTheHistorySyncToSettle(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	if tr.at(t, 0) != 0 || syncValue(t, archive, pairedAtKey) != "" {
		t.Fatal("an unpaired device was recorded or backed up")
	}
	tr.status = engine.Status{State: engine.StateDisconnected, Reason: engine.ReasonOwnerMismatch, Paired: true}
	if tr.at(t, time.Hour) != 0 || syncValue(t, archive, pairedAtKey) != "" {
		t.Fatal("a device that is not the owner's was recorded or backed up")
	}

	tr.status = pairedStatus
	if tr.at(t, 0) != 0 || syncValue(t, archive, pairedAtKey) != "1791201600000" {
		t.Fatalf("pairing was recorded as %q", syncValue(t, archive, pairedAtKey))
	}
	if tr.at(t, settleAfter-time.Millisecond) != 0 {
		t.Fatal("a backup was taken before ten minutes had passed since pairing")
	}
	writeArchive(t, archive, func(tx *ingest.Tx) error {
		_, err := tx.RecordBlob("blob-1", []byte("ref"), backupEpoch.Add(5*time.Minute))
		return err
	})
	if tr.at(t, time.Hour) != 0 {
		t.Fatal("a backup was taken while a history blob was unprocessed")
	}
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.MarkBlobProcessed("blob-1", backupEpoch.Add(6*time.Minute)) })
	if tr.at(t, 15*time.Minute-time.Millisecond) != 0 {
		t.Fatal("a backup was taken before ten minutes had passed since the last blob arrived")
	}
	if tr.at(t, 15*time.Minute) != 1 || syncValue(t, archive, takenAtKey) == "" {
		t.Fatal("no backup was recorded once the history sync settled")
	}
	if tr.at(t, 2*time.Hour) != 1 {
		t.Fatal("a second backup was taken in the same process")
	}
	tr.restart(archive)
	if tr.at(t, 3*time.Hour) != 1 {
		t.Fatal("a second backup of the same device was taken after a restart")
	}

	tr.status = engine.Status{State: engine.StateUnpaired}
	if tr.at(t, 4*time.Hour) != 1 || syncValue(t, archive, pairedAtKey) != "" || syncValue(t, archive, takenAtKey) != "" {
		t.Fatal("unpairing did not clear the record")
	}
	tr.status = pairedStatus
	tr.restart(archive)
	tr.at(t, 5*time.Hour)
	if tr.at(t, 5*time.Hour+settleAfter-time.Millisecond) != 1 {
		t.Fatal("the next device was backed up before its sync settled")
	}
	if tr.at(t, 5*time.Hour+settleAfter) != 2 {
		t.Fatal("the next paired device was not backed up")
	}
}

func TestADeviceRepairedWithoutARestartIsBackedUpInTurn(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status = pairedStatus
	tr.at(t, 0)
	if tr.at(t, settleAfter) != 1 {
		t.Fatal("the first device was not backed up")
	}
	tr.status = engine.Status{State: engine.StateUnpaired}
	tr.at(t, time.Hour)
	tr.status = pairedStatus
	tr.at(t, 2*time.Hour)
	if tr.at(t, 2*time.Hour+settleAfter-time.Millisecond) != 1 {
		t.Fatal("the next device was backed up before its sync settled")
	}
	if tr.at(t, 2*time.Hour+settleAfter) != 2 || syncValue(t, archive, takenAtKey) == "" {
		t.Fatal("the next paired device was not backed up in the same process")
	}
}

func TestADeviceRepairedBetweenTwoChecksIsBackedUpInTurn(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status = pairedStatus
	tr.at(t, 0)
	if tr.at(t, settleAfter) != 1 {
		t.Fatal("the first device was not backed up")
	}
	tr.unpairs++
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := tr.b.check(cancelled); err == nil {
		t.Fatal("clearing the record succeeded on a cancelled context")
	}
	if tr.at(t, time.Hour) != 1 || syncValue(t, archive, pairedAtKey) != "" || syncValue(t, archive, takenAtKey) != "" {
		t.Fatal("an unpairing reported between two checks did not clear the record")
	}
	tr.at(t, 2*time.Hour)
	if tr.at(t, 2*time.Hour+settleAfter-time.Millisecond) != 1 {
		t.Fatal("the next device was backed up before its sync settled")
	}
	if tr.at(t, 2*time.Hour+settleAfter) != 2 || syncValue(t, archive, takenAtKey) == "" {
		t.Fatal("a device paired again between two checks was not backed up")
	}
	if tr.at(t, 3*time.Hour) != 2 {
		t.Fatal("an unpairing was counted twice")
	}
}

func TestAFailedInitialBackupIsRetriedOnlyAfterARestart(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status, tr.fail = pairedStatus, true
	tr.at(t, 0)
	if tr.at(t, settleAfter) != 1 || syncValue(t, archive, takenAtKey) != "" {
		t.Fatal("a failed backup was recorded as taken")
	}
	if tr.at(t, time.Hour) != 1 {
		t.Fatal("a failed backup was retried in the same process")
	}
	tr.fail = false
	tr.restart(archive)
	if tr.at(t, 2*time.Hour) != 2 || syncValue(t, archive, takenAtKey) == "" {
		t.Fatal("the backup was not retried after a restart")
	}
}

func TestPairingTimeSurvivesARestart(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status = pairedStatus
	tr.at(t, 0)
	tr.restart(archive)
	if tr.at(t, settleAfter) != 1 {
		t.Fatal("a restart moved the reference away from the recorded pairing")
	}
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue(takenAtKey, "") })
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue(pairedAtKey, "not a time") })
	tr.restart(archive)
	if tr.at(t, 2*settleAfter) != 1 || syncValue(t, archive, pairedAtKey) != "1791202800000" {
		t.Fatalf("an unreadable pairing record became %q, want the current time", syncValue(t, archive, pairedAtKey))
	}
}

func pairedApp(t *testing.T, client *stubClient, recipient string) (*App, *syncBuffer, *testClock, string) {
	t.Helper()
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes, cfg.BackupRecipient = "+15550100009", config.DefaultHistoryMaxBytes, recipient
	logger := logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)
	sess, err := session.Open(t.Context(), session.Options{DataDir: cfg.DataDir, UID: os.Geteuid(), Profile: session.Profile(config.StorageLocal), Logger: logger})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{},
		fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}, session: sess}))
	if err != nil {
		_ = sess.Close()
		t.Fatalf("newAppWith: %v", err)
	}
	clock := &testClock{now: time.Now()}
	if a.backup != nil {
		a.backup.now = clock.Now
	}
	a.backupEvery = 5 * time.Millisecond
	return a, logs, clock, cfg.DataDir
}

func TestTheServiceTakesOneEncryptedBackupOnceTheSyncSettles(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	a, logs, clock, dir := pairedApp(t, newStubClient(), id.Recipient().String())
	if a.backup == nil || len(logs.find("backup_disabled")) != 0 {
		t.Fatal("a configured recipient did not enable the backup")
	}
	var checks atomic.Int64
	unpairs := a.backup.unpairs
	a.backup.unpairs = func() uint64 {
		checks.Add(1)
		return unpairs()
	}
	stop := run(t, a)
	waitUntil(t, "the pairing is recorded", func() bool { return syncValue(t, a.archive, pairedAtKey) != "" })
	checked := checks.Load() + 2
	waitUntil(t, "a whole check after the pairing", func() bool { return checks.Load() >= checked })
	if len(logs.find("backup_done")) != 0 {
		t.Fatal("a backup was taken before the sync settled")
	}
	clock.set(clock.Now().Add(settleAfter))
	waitUntil(t, "the backup is done", func() bool { return len(logs.find("backup_done")) == 1 })
	checked = checks.Load() + 2
	waitUntil(t, "a whole check after the backup", func() bool { return checks.Load() >= checked })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if done, failed := logs.find("backup_done"), logs.find("backup_failed"); len(done) != 1 || len(failed) != 0 {
		t.Fatalf("backup_done %v, backup_failed %v: want exactly one backup", done, failed)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "backups"))
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".age") {
		t.Fatalf("backups/ holds %v, %v: want one encrypted file", entries, err)
	}
	f, err := os.Open(filepath.Clean(filepath.Join(dir, "backups", entries[0].Name())))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	r, err := age.Decrypt(f, id)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("decrypt the body: %v", err)
	}
}

func TestTheEngineReportsEveryUnpairingToTheBackupTrigger(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	client := newStubClient()
	a, _, _, _ := pairedApp(t, client, id.Recipient().String())
	a.backupEvery = time.Hour
	stop := run(t, a)
	waitUntil(t, "the pairing is recorded", func() bool { return syncValue(t, a.archive, pairedAtKey) != "" })
	setPaired := func(paired bool) {
		client.mu.Lock()
		defer client.mu.Unlock()
		client.unpaired = !paired
	}
	setPaired(false)
	client.emit(engine.Disconnected{})
	setPaired(true)
	if n := a.backup.unpairs(); n != 1 {
		t.Fatalf("the backup trigger saw %d unpairings, want 1", n)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestWithoutARecipientNoBackupIsEverTaken(t *testing.T) {
	a, logs, _, dir := pairedApp(t, newStubClient(), "")
	if a.backup != nil {
		t.Fatal("a backup trigger exists without a recipient")
	}
	disabled := logs.find("backup_disabled")
	if len(disabled) != 1 || disabled[0]["level"] != "WARN" {
		t.Fatalf("backup_disabled events %v, want one warning at start", disabled)
	}
	stop := run(t, a)
	waitUntil(t, "the service is ready", func() bool { return len(logs.find("ready")) == 1 })
	time.Sleep(20 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backups/ exists without a recipient: %v", err)
	}
	if len(logs.find("backup_disabled")) != 1 || len(logs.find("backup_done")) != 0 || len(logs.find("backup_failed")) != 0 {
		t.Fatal("backup events without a recipient")
	}
}

func TestAnInterruptedBackupsStagingIsRemovedAtStart(t *testing.T) {
	cfg := testConfig(t, "")
	staging := filepath.Join(cfg.DataDir, "backups", "tmp")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "archive.stage"), []byte("synthetic plaintext"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	a, _ := open(t, cfg, noClients{})
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staging directory survived the start: %v", err)
	}
	if err := run(t, a)(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
