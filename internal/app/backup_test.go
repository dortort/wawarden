package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
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
	b         *scheduler
	clock     *testClock
	logs      *syncBuffer
	status    engine.Status
	unpairs   uint64
	takes     int
	fail      bool
	session   int64
	protected []string
	during    func()
}

func newTrigger(t *testing.T, archive *ingest.Store) *trigger {
	t.Helper()
	tr := &trigger{clock: &testClock{now: backupEpoch}, logs: &syncBuffer{}, status: engine.Status{State: engine.StateUnpaired}}
	tr.restart(t, archive)
	return tr
}

func (tr *trigger) restart(t *testing.T, archive *ingest.Store) *scheduler {
	t.Helper()
	tr.b = &scheduler{
		archive: archive, status: func() engine.Status { return tr.status }, unpairs: func() uint64 { return tr.unpairs }, now: tr.clock.Now,
		minute: config.DefaultBackupMinute, keep: config.DefaultBackupKeep, gauge: &metrics.Gauge{}, logger: logx.New(logx.NewWriter(tr.logs), slog.LevelInfo),
		take: func(context.Context) (string, error) {
			tr.takes++
			if tr.during != nil {
				tr.during()
			}
			if tr.fail {
				return "", errors.New("synthetic backup failure")
			}
			return tr.clock.Now().UTC().Format("20060102T150405Z") + ".age", nil
		},
		prune: func(_ context.Context, keep int, protect string) (int, int, error) {
			if keep != config.DefaultBackupKeep {
				return 0, 0, errors.New("synthetic: unexpected count kept")
			}
			tr.protected = append(tr.protected, protect)
			return 0, 1, nil
		},
		changes: func(ctx context.Context) ([2]int64, error) {
			n, err := archive.Changes(ctx)
			return [2]int64{n, tr.session}, err
		},
	}
	if err := tr.b.load(t.Context()); err != nil {
		t.Fatalf("load: %v", err)
	}
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

func (tr *trigger) on(t *testing.T, at time.Time) int {
	t.Helper()
	return tr.at(t, at.Sub(backupEpoch))
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
	tr.restart(t, archive)
	if tr.at(t, 3*time.Hour) != 1 {
		t.Fatal("a second backup of the same device was taken after a restart")
	}

	tr.status = engine.Status{State: engine.StateUnpaired}
	if tr.at(t, 4*time.Hour) != 1 || syncValue(t, archive, pairedAtKey) != "" || syncValue(t, archive, takenAtKey) != "" {
		t.Fatal("unpairing did not clear the record")
	}
	tr.status = pairedStatus
	tr.restart(t, archive)
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

func TestAFailedInitialBackupIsRetriedAfterABackoffThatDoublesAndResets(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status, tr.fail = pairedStatus, true
	tr.at(t, 0)
	if tr.at(t, settleAfter) != 1 || syncValue(t, archive, takenAtKey) != "" || syncValue(t, archive, lastAtKey) != "" {
		t.Fatal("a failed backup was recorded as taken")
	}
	failedAt := settleAfter
	for i, wait := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		if tr.at(t, failedAt+wait-time.Millisecond) != i+1 {
			t.Fatalf("failure %d was retried before %v", i+1, wait)
		}
		if tr.at(t, failedAt+wait) != i+2 {
			t.Fatalf("failure %d was not retried after %v", i+1, wait)
		}
		failedAt += wait
	}
	if len(tr.protected) != 0 {
		t.Fatalf("pruned with %v after failed backups", tr.protected)
	}
	tr.fail = false
	if tr.at(t, failedAt+4*time.Hour) != 8 || syncValue(t, archive, takenAtKey) == "" {
		t.Fatal("the backup was not retried once it could succeed")
	}
	if len(tr.protected) != 1 {
		t.Fatalf("pruned with %v, want once after the success", tr.protected)
	}
	next := lastSlot(backupEpoch.Add(failedAt+4*time.Hour), config.DefaultBackupMinute).AddDate(0, 0, 1)
	tr.fail = true
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue("test_change", "1") })
	if tr.on(t, next) != 9 {
		t.Fatal("the nightly backup was not taken")
	}
	if tr.on(t, next.Add(15*time.Minute-time.Millisecond)) != 9 || tr.on(t, next.Add(15*time.Minute)) != 10 {
		t.Fatal("a success did not reset the backoff to 15 minutes")
	}
	if len(tr.protected) != 1 {
		t.Fatalf("pruned with %v after failed nightly backups", tr.protected)
	}
}

func TestPairingTimeSurvivesARestart(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status = pairedStatus
	tr.at(t, 0)
	tr.restart(t, archive)
	if tr.at(t, settleAfter) != 1 {
		t.Fatal("a restart moved the reference away from the recorded pairing")
	}
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue(takenAtKey, "") })
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue(pairedAtKey, "not a time") })
	tr.restart(t, archive)
	if tr.at(t, 2*settleAfter) != 1 || syncValue(t, archive, pairedAtKey) != "1791202800000" {
		t.Fatalf("an unreadable pairing record became %q, want the current time", syncValue(t, archive, pairedAtKey))
	}
}

func day(n int, hour, minute, second int) time.Time {
	return time.Date(2026, 10, 5+n, hour, minute, second, 0, time.UTC)
}

func seedLast(t *testing.T, archive *ingest.Store, at time.Time) {
	t.Helper()
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue(lastAtKey, strconv.FormatInt(at.UnixMilli(), 10)) })
}

func TestTheSlotRule(t *testing.T) {
	for _, tt := range []struct {
		name      string
		now, last time.Time
		owed      bool
	}{
		{name: "never backed up", now: day(3, 3, 0, 0)},
		{name: "one millisecond before the slot", now: day(1, 3, 0, 0).Add(-time.Millisecond), last: day(0, 3, 0, 0), owed: false},
		{name: "at the slot", now: day(1, 3, 0, 0), last: day(0, 3, 0, 0), owed: true},
		{name: "after the slot, backed up after the previous one", now: day(1, 2, 59, 0), last: day(0, 3, 0, 10)},
		{name: "after the slot, backed up before it", now: day(1, 3, 5, 0), last: day(0, 3, 0, 10), owed: true},
		{name: "backed up at the slot", now: day(1, 3, 5, 0), last: day(1, 3, 0, 0)},
		{name: "a stored time in the future", now: day(1, 3, 5, 0), last: day(3, 0, 0, 0), owed: true},
		{name: "a slot just after midnight", now: day(1, 0, 0, 30), last: day(0, 23, 59, 0), owed: false},
	} {
		if got := owed(tt.now, tt.last, config.DefaultBackupMinute); got != tt.owed {
			t.Errorf("%s: owed = %v, want %v", tt.name, got, tt.owed)
		}
	}
	if got := lastSlot(day(1, 0, 0, 30), 0); !got.Equal(day(1, 0, 0, 0)) {
		t.Errorf("lastSlot at 00:00:30 for 00:00 = %v", got)
	}
	if got := lastSlot(day(1, 23, 58, 0), 23*60+59); !got.Equal(day(0, 23, 59, 0)) {
		t.Errorf("lastSlot at 23:58 for 23:59 = %v", got)
	}
	if got := lastSlot(time.Date(2026, 10, 6, 1, 0, 0, 0, time.FixedZone("synthetic", 3*3600)), config.DefaultBackupMinute); !got.Equal(day(0, 3, 0, 0)) {
		t.Errorf("lastSlot of a zoned time = %v, want the slot in UTC", got)
	}
}

func TestNoNightlyBackupBeforeAFirstSuccess(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	for d := range 3 {
		writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue("test_change", strconv.Itoa(d)) })
		if tr.on(t, day(d+1, 3, 0, 0)) != 0 || tr.on(t, day(d+1, 12, 0, 0)) != 0 {
			t.Fatal("a service that never backed up took a nightly backup")
		}
	}
	if tr.b.gauge.Value() != 0 {
		t.Fatalf("the gauge reads %v before any backup", tr.b.gauge.Value())
	}
}

func TestANightlyBackupIsOwedExactlyAtTheSlot(t *testing.T) {
	archive := triggerArchive(t)
	seedLast(t, archive, day(0, 12, 0, 0))
	tr := newTrigger(t, archive)
	if got := tr.b.gauge.Value(); got != float64(day(0, 12, 0, 0).Unix()) {
		t.Fatalf("the gauge reads %v after the start, want the stored time", got)
	}
	if tr.on(t, day(1, 3, 0, 0).Add(-time.Millisecond)) != 0 {
		t.Fatal("a backup was taken before the slot")
	}
	if tr.on(t, day(1, 3, 0, 0)) != 1 || syncValue(t, archive, lastAtKey) != strconv.FormatInt(day(1, 3, 0, 0).UnixMilli(), 10) {
		t.Fatalf("no backup at the slot, last %q", syncValue(t, archive, lastAtKey))
	}
	if got := tr.b.gauge.Value(); got != float64(day(1, 3, 0, 0).Unix()) {
		t.Fatalf("the gauge reads %v after the backup", got)
	}
	if tr.on(t, day(1, 3, 0, 30)) != 1 || tr.on(t, day(1, 23, 0, 0)) != 1 {
		t.Fatal("a second backup was taken for one slot")
	}
	if len(tr.protected) != 1 || tr.protected[0] != "20261006T030000Z.age" {
		t.Fatalf("pruned with %v, want once with the new file protected", tr.protected)
	}
	if syncValue(t, archive, takenAtKey) != "" || syncValue(t, archive, pairedAtKey) != "" {
		t.Fatal("a nightly backup recorded an initial backup")
	}
}

func TestARestartBeforeTheSlotWaitsAndOneAfterItCatchesUp(t *testing.T) {
	for _, tt := range []struct {
		name  string
		at    time.Time
		takes int
	}{
		{name: "restart at 02:59", at: day(1, 2, 59, 0), takes: 0},
		{name: "restart at 03:05", at: day(1, 3, 5, 0), takes: 1},
		{name: "restart after three days down", at: day(3, 10, 0, 0), takes: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			archive := triggerArchive(t)
			seedLast(t, archive, day(0, 3, 0, 10))
			tr := newTrigger(t, archive)
			if tr.on(t, tt.at) != tt.takes || tr.on(t, tt.at.Add(backupInterval)) != tt.takes {
				t.Fatalf("%d backups, want %d", tr.takes, tt.takes)
			}
			if tr.on(t, tt.at.Add(2*time.Hour)) != 1 || tr.on(t, tt.at.Add(3*time.Hour)) != 1 {
				t.Fatalf("%d backups by the end of the day, want one", tr.takes)
			}
		})
	}
}

func TestAStoredTimeInTheFutureIsOwed(t *testing.T) {
	archive := triggerArchive(t)
	seedLast(t, archive, day(30, 0, 0, 0))
	tr := newTrigger(t, archive)
	if tr.on(t, day(1, 12, 0, 0)) != 1 || syncValue(t, archive, lastAtKey) != strconv.FormatInt(day(1, 12, 0, 0).UnixMilli(), 10) {
		t.Fatal("a stored time later than now silenced the backups")
	}
	if tr.on(t, day(1, 13, 0, 0)) != 1 {
		t.Fatal("a second backup was taken for one slot")
	}
}

func TestAnUnchangedServiceSkipsTheSlotOnceAndARestartBacksUpAnyway(t *testing.T) {
	archive := triggerArchive(t)
	seedLast(t, archive, day(0, 3, 0, 0))
	tr := newTrigger(t, archive)
	if tr.on(t, day(1, 3, 0, 0)) != 1 {
		t.Fatal("the first nightly backup after the start was skipped")
	}
	if tr.on(t, day(2, 3, 0, 0)) != 1 || tr.on(t, day(2, 3, 0, 30)) != 1 {
		t.Fatal("a backup was taken although nothing was written since the last one")
	}
	if skipped := tr.logs.find("backup_skipped"); len(skipped) != 1 || skipped[0]["reason"] != "unchanged" || skipped[0]["level"] != "INFO" {
		t.Fatalf("backup_skipped events %v, want one", skipped)
	}
	tr.session++
	if tr.on(t, day(2, 4, 0, 0)) != 1 {
		t.Fatal("a skipped slot was taken later the same day")
	}
	if tr.on(t, day(3, 3, 0, 0)) != 2 {
		t.Fatal("a change to the device store did not make the next slot back up")
	}
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.SetSyncValue("test_change", "1") })
	if tr.on(t, day(4, 3, 0, 0)) != 3 {
		t.Fatal("a change to the archive did not make the next slot back up")
	}
	if tr.on(t, day(5, 3, 0, 0)) != 3 {
		t.Fatal("the slot after a backup with no write in between was not skipped")
	}
	tr.restart(t, archive)
	if tr.on(t, day(6, 3, 0, 0)) != 4 {
		t.Fatal("the first slot after a restart was skipped")
	}
	if n := len(tr.logs.find("backup_skipped")); n != 2 {
		t.Fatalf("%d backup_skipped events, want one per skipped slot", n)
	}
}

func TestTheInitialAndTheNightlyBackupShareTheirTimeAndUnpairingKeepsIt(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status = pairedStatus
	tr.at(t, 0)
	if tr.at(t, settleAfter) != 1 {
		t.Fatal("the initial backup was not taken")
	}
	initial := strconv.FormatInt(backupEpoch.Add(settleAfter).UnixMilli(), 10)
	if syncValue(t, archive, lastAtKey) != initial || syncValue(t, archive, takenAtKey) != initial {
		t.Fatalf("the initial backup recorded last %q and taken %q, want both %s", syncValue(t, archive, lastAtKey), syncValue(t, archive, takenAtKey), initial)
	}
	tr.status = engine.Status{State: engine.StateUnpaired}
	if tr.at(t, time.Hour) != 1 || syncValue(t, archive, takenAtKey) != "" || syncValue(t, archive, lastAtKey) != initial {
		t.Fatal("unpairing cleared the time of the newest backup or kept the initial record")
	}
	tr.restart(t, archive)
	if tr.on(t, day(1, 3, 0, 0)) != 2 || syncValue(t, archive, lastAtKey) != strconv.FormatInt(day(1, 3, 0, 0).UnixMilli(), 10) {
		t.Fatal("an unpaired service did not take its nightly backup")
	}
}

func TestAVersionZeroThreeRecordIsTheBaseline(t *testing.T) {
	archive := triggerArchive(t)
	writeArchive(t, archive, func(tx *ingest.Tx) error {
		return tx.SetSyncValue(takenAtKey, strconv.FormatInt(day(0, 12, 0, 0).UnixMilli(), 10))
	})
	tr := newTrigger(t, archive)
	if tr.b.gauge.Value() != float64(day(0, 12, 0, 0).Unix()) {
		t.Fatalf("the gauge reads %v", tr.b.gauge.Value())
	}
	if tr.on(t, day(0, 23, 0, 0)) != 0 || tr.on(t, day(1, 3, 0, 0)) != 1 {
		t.Fatal("a service upgraded after its initial backup does not take nightly backups")
	}
}

func TestAStopDuringABackupIsNotRetried(t *testing.T) {
	archive := triggerArchive(t)
	seedLast(t, archive, day(0, 3, 0, 0))
	tr := newTrigger(t, archive)
	tr.clock.set(day(1, 3, 0, 0))
	ctx, cancel := context.WithCancel(t.Context())
	tr.during, tr.fail = cancel, true
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.b.run(ctx, time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler did not stop")
	}
	if tr.takes != 1 || !tr.b.retryAt.IsZero() || syncValue(t, archive, lastAtKey) != strconv.FormatInt(day(0, 3, 0, 0).UnixMilli(), 10) {
		t.Fatalf("%d backups, retry at %v after a stop during the backup", tr.takes, tr.b.retryAt)
	}
	if n := len(tr.logs.find("backup_check_failed")); n != 0 {
		t.Fatalf("%d backup_check_failed lines after a stop", n)
	}
}

func seedProcessed(t *testing.T, archive *ingest.Store, n int, at time.Time) {
	t.Helper()
	for start := 0; start < n; start += 1000 {
		writeArchive(t, archive, func(tx *ingest.Tx) error {
			for i := start; i < min(start+1000, n); i++ {
				id := "old-" + strconv.Itoa(i)
				if _, err := tx.RecordBlob(id, []byte("ref"), at); err != nil {
					return err
				}
				if err := tx.MarkBlobProcessed(id, at); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

func TestABackupThatCannotBeRecordedWaitsBeforeTheNextAttempt(t *testing.T) {
	archive := triggerArchive(t)
	tr := newTrigger(t, archive)
	tr.status = pairedStatus
	tr.at(t, 0)
	tr.during = func() { _ = archive.Close() }
	tr.clock.set(backupEpoch.Add(settleAfter))
	if err := tr.b.check(t.Context()); err == nil || tr.takes != 1 {
		t.Fatalf("check = %v after %d backups, want the failed record", err, tr.takes)
	}
	if !tr.b.retryAt.Equal(backupEpoch.Add(settleAfter + 15*time.Minute)) {
		t.Fatalf("the next attempt is due at %v, want 15 minutes later", tr.b.retryAt)
	}
}

func TestProcessedBlobRowsArePrunedOncePerSlotWithoutARecipient(t *testing.T) {
	archive := triggerArchive(t)
	old := backupEpoch.Add(-31 * 24 * time.Hour)
	seedProcessed(t, archive, ingest.PruneBatch+1, old)
	writeArchive(t, archive, func(tx *ingest.Tx) error {
		for _, id := range []string{"young", "quarantined", "pending"} {
			if _, err := tx.RecordBlob(id, []byte("ref"), old); err != nil {
				return err
			}
		}
		return errors.Join(tx.MarkBlobProcessed("young", backupEpoch.Add(-29*24*time.Hour)), tx.QuarantineBlob("quarantined"))
	})
	tr := newTrigger(t, archive)
	tr.b.take = nil
	tr.at(t, 0)
	if pruned := tr.logs.find("history_blobs_pruned"); len(pruned) != 1 || pruned[0]["rows"] != float64(ingest.PruneBatch+1) {
		t.Fatalf("history_blobs_pruned %v, want every old processed row in one run", pruned)
	}
	present := func(id string) bool {
		var inserted bool
		writeArchive(t, archive, func(tx *ingest.Tx) error {
			var err error
			inserted, err = tx.RecordBlob(id, []byte("ref"), backupEpoch)
			return err
		})
		return !inserted
	}
	for id, want := range map[string]bool{"old-0": false, "old-10000": false, "young": true, "quarantined": true, "pending": true} {
		if present(id) != want {
			t.Errorf("row %s present = %v, want %v", id, !want, want)
		}
	}
	writeArchive(t, archive, func(tx *ingest.Tx) error { return tx.MarkBlobProcessed("old-0", old) })
	tr.at(t, time.Hour)
	if !present("old-0") {
		t.Fatal("the rows were pruned twice in one slot")
	}
	tr.on(t, day(1, 3, 0, 0))
	if present("old-0") || !present("young") {
		t.Fatal("the next slot did not prune the rows that aged out")
	}
	if tr.takes != 0 || len(tr.logs.find("backup_skipped")) != 0 {
		t.Fatal("a scheduler without a recipient took a backup")
	}
}

func TestTheBlobPruneStopsAtItsTimeLimit(t *testing.T) {
	archive := triggerArchive(t)
	old := backupEpoch.Add(-31 * 24 * time.Hour)
	seedProcessed(t, archive, 2*ingest.PruneBatch+1, old)
	tr := newTrigger(t, archive)
	calls := 0
	tr.b.now = func() time.Time {
		calls++
		return backupEpoch.Add(time.Duration(calls-1) * 16 * time.Minute)
	}
	if err := tr.b.pruneBlobs(t.Context()); err != nil {
		t.Fatalf("pruneBlobs: %v", err)
	}
	if pruned := tr.logs.find("history_blobs_pruned"); len(pruned) != 1 || pruned[0]["rows"] != float64(ingest.PruneBatch) {
		t.Fatalf("history_blobs_pruned %v, want one batch before the limit", pruned)
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
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), nil, noClients{},
		fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}, session: sess}))
	if err != nil {
		_ = sess.Close()
		t.Fatalf("newAppWith: %v", err)
	}
	clock := &testClock{now: time.Now()}
	a.backup.now = clock.Now
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
	if a.backup.take != nil {
		t.Fatal("a backup can be taken without a recipient")
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

func TestTheBackupGaugeIsLoadedAtStart(t *testing.T) {
	cfg := testConfig(t, "")
	a, _ := open(t, cfg, noClients{})
	if a.backup.gauge.Value() != 0 {
		t.Fatalf("the gauge reads %v on a new archive", a.backup.gauge.Value())
	}
	seedLast(t, a.archive, day(0, 3, 0, 1))
	if err := run(t, a)(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	b, _ := open(t, cfg, noClients{})
	if got := b.backup.gauge.Value(); got != float64(day(0, 3, 0, 1).Unix()) {
		t.Fatalf("the gauge reads %v after a restart, want the stored time", got)
	}
	if err := run(t, b)(); err != nil {
		t.Fatalf("Run = %v", err)
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
