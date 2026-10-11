package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const auditRetain = 90 * 24 * time.Hour

type auditCall struct {
	cutoff, now time.Time
	batch       int
}

type auditStub struct {
	calls   []auditCall
	results []admin.PruneResult
	err     error
	during  func()
}

func (s *auditStub) prune(_ context.Context, cutoff, now time.Time, batch int) (admin.PruneResult, error) {
	s.calls = append(s.calls, auditCall{cutoff: cutoff, now: now, batch: batch})
	if s.during != nil {
		s.during()
	}
	if s.err != nil {
		return admin.PruneResult{}, s.err
	}
	if len(s.results) == 0 {
		return admin.PruneResult{}, nil
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r, nil
}

func auditTrigger(t *testing.T, archive *ingest.Store) (*trigger, *auditStub) {
	t.Helper()
	tr := newTrigger(t, archive)
	stub := &auditStub{}
	tr.b.audit, tr.b.retain = stub.prune, auditRetain
	return tr, stub
}

func TestTheAuditPruneWaitsTenMinutesAfterTheFirstCheckThenRunsOncePerSlot(t *testing.T) {
	tr, stub := auditTrigger(t, triggerArchive(t))
	tr.b.take = nil
	tr.at(t, 0)
	tr.at(t, 10*time.Minute-time.Millisecond)
	if len(stub.calls) != 0 {
		t.Fatalf("the audit prune ran %d times in the first ten minutes", len(stub.calls))
	}
	tr.at(t, 10*time.Minute)
	now := backupEpoch.Add(10 * time.Minute)
	if want := []auditCall{{cutoff: now.Add(-auditRetain), now: now, batch: ingest.PruneBatch}}; !slices.Equal(stub.calls, want) {
		t.Fatalf("audit prune calls %+v, want %+v", stub.calls, want)
	}
	tr.at(t, 2*time.Hour)
	tr.on(t, day(1, 3, 0, 0).Add(-time.Millisecond))
	if len(stub.calls) != 1 {
		t.Fatalf("the audit prune ran %d times in one slot", len(stub.calls))
	}
	tr.on(t, day(1, 3, 0, 0))
	if len(stub.calls) != 2 || !stub.calls[1].cutoff.Equal(day(1, 3, 0, 0).Add(-auditRetain)) {
		t.Fatalf("the next slot gave the calls %+v", stub.calls)
	}
	if tr.takes != 0 || len(tr.logs.find("audit_pruned")) != 0 {
		t.Fatal("a scheduler without a recipient took a backup, or a prune that removed nothing was logged")
	}
}

func TestTheAuditPruneFollowsTheNightlyBackupAndPrecedesTheBlobPrune(t *testing.T) {
	archive := triggerArchive(t)
	seedLast(t, archive, day(0, 3, 0, 0))
	seedProcessed(t, archive, 1, day(1, 2, 0, 0).Add(-blobRetention))
	tr, stub := auditTrigger(t, archive)
	tr.at(t, 0)
	if len(tr.logs.find("history_blobs_pruned")) != 0 {
		t.Fatal("the blob row was pruned before it aged out")
	}
	var order []string
	stub.during = func() {
		order = append(order, "audit")
		if tr.takes != 1 || len(tr.logs.find("history_blobs_pruned")) != 0 {
			t.Errorf("the audit prune ran after %d backups and with the blob rows pruned: %v", tr.takes, tr.logs.find("history_blobs_pruned"))
		}
	}
	tr.on(t, day(1, 3, 0, 0))
	if len(stub.calls) != 1 || tr.takes != 1 || len(tr.logs.find("history_blobs_pruned")) != 1 || !slices.Equal(order, []string{"audit"}) {
		t.Fatalf("at the slot: %d audit prunes, %d backups, blob prunes %v", len(stub.calls), tr.takes, tr.logs.find("history_blobs_pruned"))
	}
}

func TestTheAuditPruneWaitsForAnOwedBackup(t *testing.T) {
	archive := triggerArchive(t)
	seedLast(t, archive, day(0, 3, 0, 0))
	tr, stub := auditTrigger(t, archive)
	tr.at(t, 0)
	tr.fail = true
	tr.on(t, day(1, 3, 0, 0))
	tr.on(t, day(1, 3, 5, 0))
	if tr.takes != 1 || len(stub.calls) != 0 {
		t.Fatalf("with the night's backup failed: %d backups, %d audit prunes, want 1 and none", tr.takes, len(stub.calls))
	}
	tr.fail = false
	tr.on(t, day(1, 3, 15, 0))
	if tr.takes != 2 || len(stub.calls) != 1 {
		t.Fatalf("after the retried backup: %d backups, %d audit prunes, want 2 and 1", tr.takes, len(stub.calls))
	}
	tr.on(t, day(2, 3, 0, 0))
	if tr.takes != 2 || len(tr.logs.find("backup_skipped")) != 1 || len(stub.calls) != 2 {
		t.Fatalf("a slot skipped as unchanged: %d backups, %d audit prunes, want the prune to run", tr.takes, len(stub.calls))
	}
	tr.b.take = nil
	tr.on(t, day(4, 3, 0, 0))
	if len(stub.calls) != 3 {
		t.Fatal("without a recipient the audit prune waited for a backup")
	}
}

func TestTheAuditPruneWaitsForTheFirstBackup(t *testing.T) {
	tr, stub := auditTrigger(t, triggerArchive(t))
	tr.at(t, 0)
	tr.at(t, 2*time.Hour)
	if tr.takes != 0 || len(stub.calls) != 0 {
		t.Fatalf("unpaired with a recipient: %d backups, %d audit prunes, want none", tr.takes, len(stub.calls))
	}
	tr.status, tr.fail = pairedStatus, true
	tr.at(t, 3*time.Hour)
	tr.at(t, 3*time.Hour+settleAfter)
	tr.on(t, day(1, 3, 0, 0))
	if tr.takes != 2 || len(stub.calls) != 0 {
		t.Fatalf("with the first backup failing: %d backups, %d audit prunes, want 2 and none", tr.takes, len(stub.calls))
	}
	tr.fail = false
	tr.on(t, day(1, 3, 30, 0))
	if tr.takes != 3 || len(stub.calls) != 1 {
		t.Fatalf("after the first backup succeeded: %d backups, %d audit prunes, want 3 and 1", tr.takes, len(stub.calls))
	}
}

func TestTheAuditPruneLoopsToTheEndAndLogsOnce(t *testing.T) {
	tr, stub := auditTrigger(t, triggerArchive(t))
	tr.b.take = nil
	stub.results = []admin.PruneResult{
		{Rows: ingest.PruneBatch, Through: ingest.PruneBatch, AnchorHead: "00112233445566778899aabbccddeeff"},
		{Rows: ingest.PruneBatch, Through: 2 * ingest.PruneBatch, AnchorHead: "112233445566778899aabbccddeeff00"},
		{Rows: 7, Through: 2*ingest.PruneBatch + 7, AnchorHead: "2233445566778899aabbccddeeff0011"},
	}
	tr.at(t, 0)
	tr.at(t, 10*time.Minute)
	if len(stub.calls) != 4 {
		t.Fatalf("%d audit prune batches, want 4: three that removed rows and one that found none", len(stub.calls))
	}
	pruned := tr.logs.find("audit_pruned")
	if len(pruned) != 1 || pruned[0]["rows"] != float64(2*ingest.PruneBatch+7) || pruned[0]["through"] != float64(2*ingest.PruneBatch+7) ||
		pruned[0]["anchor_head"] != "2233445566778899aabbccddeeff0011" || pruned[0]["level"] != "INFO" || len(pruned[0]) != 7 {
		t.Fatalf("audit_pruned %v, want one line with the rows, the anchor row and its head only", pruned)
	}
}

func TestAChainMismatchIsLoggedAndEndsTheAuditPruneUntilTheNextSlot(t *testing.T) {
	tr, stub := auditTrigger(t, triggerArchive(t))
	tr.b.take = nil
	stub.results = []admin.PruneResult{{Rows: 5, Through: 5, AnchorHead: "00112233445566778899aabbccddeeff"}}
	stub.during = func() {
		if len(stub.calls) == 2 {
			stub.err = fmt.Errorf("admin: synthetic wrap: %w", admin.ErrChainMismatch)
		}
	}
	tr.at(t, 0)
	tr.at(t, 10*time.Minute)
	tr.at(t, 11*time.Minute)
	refused, pruned := tr.logs.find("audit_prune_refused"), tr.logs.find("audit_pruned")
	if len(stub.calls) != 2 || len(refused) != 1 || refused[0]["reason"] != admin.ReasonChainMismatch || refused[0]["level"] != "WARN" || len(refused[0]) != 5 ||
		len(pruned) != 1 || pruned[0]["rows"] != float64(5) {
		t.Fatalf("%d batches, audit_prune_refused %v, audit_pruned %v; want the refusal logged once with its reason only and the batch before it reported", len(stub.calls), refused, pruned)
	}
	tr.on(t, day(1, 3, 0, 0))
	if len(stub.calls) != 3 || len(tr.logs.find("audit_prune_refused")) != 2 {
		t.Fatalf("the next slot gave %d batches and %d refusals, want the prune tried and refused again", len(stub.calls), len(tr.logs.find("audit_prune_refused")))
	}
}

func TestTheAuditPruneStopsAtItsTimeLimit(t *testing.T) {
	tr, stub := auditTrigger(t, triggerArchive(t))
	tr.b.take = nil
	tr.b.started = backupEpoch.Add(-time.Hour)
	stub.results = []admin.PruneResult{{Rows: ingest.PruneBatch, Through: ingest.PruneBatch}, {Rows: ingest.PruneBatch, Through: 2 * ingest.PruneBatch}}
	calls := 0
	tr.b.now = func() time.Time {
		calls++
		return backupEpoch.Add(time.Duration(calls-1) * 16 * time.Minute)
	}
	if err := tr.b.pruneAudit(t.Context()); err != nil {
		t.Fatalf("pruneAudit: %v", err)
	}
	if pruned := tr.logs.find("audit_pruned"); len(stub.calls) != 1 || len(pruned) != 1 || pruned[0]["rows"] != float64(ingest.PruneBatch) {
		t.Fatalf("%d batches and audit_pruned %v, want one batch before the limit", len(stub.calls), pruned)
	}
}

func TestTheAuditPruneStopsOnCancellationAndRetriesAFailure(t *testing.T) {
	tr, stub := auditTrigger(t, triggerArchive(t))
	tr.b.take = nil
	tr.b.started = backupEpoch.Add(-time.Hour)
	stub.results = []admin.PruneResult{{Rows: 5, Through: 5}, {Rows: 5, Through: 10}}
	ctx, cancel := context.WithCancel(t.Context())
	stub.during = cancel
	if err := tr.b.pruneAudit(ctx); err != nil || len(stub.calls) != 1 {
		t.Fatalf("pruneAudit cancelled during its first batch = %v after %d batches, want it stopped after one", err, len(stub.calls))
	}

	tr, stub = auditTrigger(t, triggerArchive(t))
	tr.b.take = nil
	stub.err = errors.New("synthetic prune failure")
	tr.at(t, 0)
	tr.clock.set(backupEpoch.Add(10 * time.Minute))
	if err := tr.b.check(t.Context()); !errors.Is(err, stub.err) {
		t.Fatalf("check with a failing audit prune = %v", err)
	}
	stub.err = nil
	tr.at(t, 11*time.Minute)
	if len(stub.calls) != 2 {
		t.Fatalf("%d audit prune calls, want the failed run retried at the next check", len(stub.calls))
	}

	tr, stub = auditTrigger(t, triggerArchive(t))
	tr.b.take, tr.b.retain = nil, 0
	tr.at(t, 0)
	tr.at(t, time.Hour)
	if len(stub.calls) != 0 {
		t.Fatal("a scheduler without a retention pruned the audit chain")
	}
}

func TestTheServicePrunesAuditRowsOlderThanTheRetention(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.AuditRetention = 30
	a, logs := open(t, cfg, noClients{})
	t.Cleanup(func() { _ = a.closeStores() })
	now := time.Now()
	for _, at := range []time.Time{now.Add(-40 * 24 * time.Hour), now.Add(-31 * 24 * time.Hour), now.Add(-29 * 24 * time.Hour), now} {
		if err := a.archive.Audit().Record(t.Context(), admin.Event{At: at, Client: "aaaaaaaa", Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	clock := &testClock{now: now}
	a.backup.now = clock.Now
	if err := a.backup.check(t.Context()); err != nil {
		t.Fatalf("check: %v", err)
	}
	clock.set(now.Add(settleAfter))
	if err := a.backup.check(t.Context()); err != nil {
		t.Fatalf("check: %v", err)
	}
	pruned := logs.find("audit_pruned")
	if len(pruned) != 1 || pruned[0]["rows"] != float64(2) || pruned[0]["through"] != float64(2) {
		t.Fatalf("audit_pruned %v, want the two rows older than 30 days", pruned)
	}
}
