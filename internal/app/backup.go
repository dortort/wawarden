package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	settleAfter    = 10 * time.Minute
	backupInterval = 30 * time.Second

	firstRetry = 15 * time.Minute
	lastRetry  = 4 * time.Hour

	blobRetention = 30 * 24 * time.Hour
	pruneLimit    = 15 * time.Minute

	pairedAtKey = "backup_paired_at"
	takenAtKey  = "backup_taken_at"
	lastAtKey   = "backup_last_at"
)

type scheduler struct {
	archive *ingest.Store
	status  func() engine.Status
	unpairs func() uint64
	take    func(context.Context) (string, error)
	prune   func(ctx context.Context, keep int, protect string) (removed, kept int, err error)
	changes func(context.Context) ([2]int64, error)
	audit   func(ctx context.Context, cutoff, now time.Time, batch int) (admin.PruneResult, error)
	gauge   *metrics.Gauge
	logger  *slog.Logger
	now     func() time.Time
	minute  int
	keep    int
	retain  time.Duration

	seen      uint64
	loaded    bool
	last      time.Time
	retryAt   time.Time
	backoff   time.Duration
	taken     bool
	lastTaken [2]int64
	skipped   time.Time
	blobSlot  time.Time
	auditSlot time.Time
	started   time.Time
}

type unpairCounter struct {
	engine.Notifier
	count atomic.Uint64
}

func (u *unpairCounter) Unpaired() {
	u.count.Add(1)
	u.Notifier.Unpaired()
}

func lastSlot(now time.Time, minute int) time.Time {
	u := now.UTC()
	s := time.Date(u.Year(), u.Month(), u.Day(), 0, minute, 0, 0, time.UTC)
	if s.After(u) {
		s = s.AddDate(0, 0, -1)
	}
	return s
}

func owed(now, last time.Time, minute int) bool {
	return !last.IsZero() && (last.After(now) || last.Before(lastSlot(now, minute)))
}

func (b *scheduler) run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := b.check(ctx); err != nil && ctx.Err() == nil {
			b.logger.Warn("a scheduled backup step failed", slog.String("event", "backup_check_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *scheduler) check(ctx context.Context) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	var errs []error
	for _, step := range []func(context.Context) error{b.initial, b.nightly, b.pruneAudit, b.pruneBlobs} {
		if errs = append(errs, step(ctx)); ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func (b *scheduler) load(ctx context.Context) error {
	if b.loaded {
		return nil
	}
	var last, taken string
	if err := b.archive.Read(ctx, "backup.load", func(r *ingest.Reader) error {
		var err error
		if last, _, err = r.SyncValue(lastAtKey); err != nil {
			return err
		}
		taken, _, err = r.SyncValue(takenAtKey)
		return err
	}); err != nil {
		return err
	}
	for _, v := range []string{last, taken} {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 && time.UnixMilli(ms).After(b.last) {
			b.last = time.UnixMilli(ms)
		}
	}
	b.loaded = true
	b.setGauge()
	return nil
}

func (b *scheduler) setGauge() {
	if b.gauge != nil && !b.last.IsZero() {
		b.gauge.Set(float64(b.last.UnixMilli()) / 1000)
	}
}

func (b *scheduler) initial(ctx context.Context) error {
	if b.take == nil {
		return nil
	}
	unpairs := b.unpairs()
	st := b.status()
	if !st.Paired || unpairs != b.seen {
		if err := b.forget(ctx); err != nil {
			return err
		}
		b.seen = unpairs
		return nil
	}
	if st.State == engine.StateUnpaired || st.Reason == engine.ReasonOwnerMismatch {
		return nil
	}
	var pairedAt, takenAt string
	var pending []ingest.Blob
	var last time.Time
	var arrived bool
	if err := b.archive.Read(ctx, "backup.initial", func(r *ingest.Reader) error {
		var err error
		if pairedAt, _, err = r.SyncValue(pairedAtKey); err != nil {
			return err
		}
		if takenAt, _, err = r.SyncValue(takenAtKey); err != nil {
			return err
		}
		if pending, err = r.PendingBlobs(1); err != nil {
			return err
		}
		last, arrived, err = r.LastBlobAt()
		return err
	}); err != nil {
		return err
	}
	if takenAt != "" {
		return nil
	}
	settled, err := strconv.ParseInt(pairedAt, 10, 64)
	if err != nil {
		return b.record(ctx, b.now(), pairedAtKey)
	}
	since := time.UnixMilli(settled)
	if arrived && last.After(since) {
		since = last
	}
	if len(pending) > 0 || b.now().Before(since.Add(settleAfter)) || b.now().Before(b.retryAt) {
		return nil
	}
	return b.backup(ctx, takenAtKey)
}

func (b *scheduler) nightly(ctx context.Context) error {
	now := b.now()
	if b.take == nil || !owed(now, b.last, b.minute) || now.Before(b.retryAt) {
		return nil
	}
	slot := lastSlot(now, b.minute)
	if slot.Equal(b.skipped) {
		return nil
	}
	changes, err := b.changes(ctx)
	if err != nil {
		return err
	}
	if b.taken && changes == b.lastTaken {
		b.skipped = slot
		b.logger.Info("no database changed since the last backup: this slot's backup is skipped", slog.String("event", "backup_skipped"), slog.String("reason", "unchanged"))
		return nil
	}
	return b.backup(ctx)
}

func (b *scheduler) backup(ctx context.Context, keys ...string) error {
	began := b.now()
	name, err := b.take(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.retry()
		}
		return nil
	}
	b.backoff, b.retryAt = 0, time.Time{}
	b.last = began
	b.setGauge()
	if err := b.record(ctx, began, append(keys, lastAtKey)...); err != nil {
		b.retry()
		return err
	}
	if b.lastTaken, err = b.changes(ctx); err != nil {
		b.taken = false
		return err
	}
	b.taken = true
	_, _, err = b.prune(ctx, b.keep, name)
	return err
}

func (b *scheduler) retry() {
	b.backoff = min(max(2*b.backoff, firstRetry), lastRetry)
	b.retryAt = b.now().Add(b.backoff)
}

func (b *scheduler) pruneBlobs(ctx context.Context) error {
	now := b.now()
	slot := lastSlot(now, b.minute)
	if slot.Equal(b.blobSlot) {
		return nil
	}
	cutoff, until := now.Add(-blobRetention), now.Add(pruneLimit)
	total := 0
	for {
		var n int
		if err := b.archive.Write(ctx, "backup.prune_blobs", func(tx *ingest.Tx) error {
			var err error
			n, err = tx.PruneProcessedBlobs(cutoff)
			return err
		}); err != nil {
			return err
		}
		total += n
		if n < ingest.PruneBatch || !b.now().Before(until) || ctx.Err() != nil {
			break
		}
	}
	b.blobSlot = slot
	if total > 0 {
		b.logger.Info("processed history blob rows older than 30 days were deleted", slog.String("event", "history_blobs_pruned"), slog.Int("rows", total))
	}
	return nil
}

func (b *scheduler) pruneAudit(ctx context.Context) error {
	now := b.now()
	if b.started.IsZero() {
		b.started = now
	}
	slot := lastSlot(now, b.minute)
	if b.audit == nil || b.retain <= 0 || slot.Equal(b.auditSlot) || now.Before(b.started.Add(settleAfter)) ||
		(b.take != nil && (b.last.IsZero() || owed(now, b.last, b.minute)) && !slot.Equal(b.skipped)) {
		return nil
	}
	cutoff, until := now.Add(-b.retain), now.Add(pruneLimit)
	var pruned admin.PruneResult
	for {
		res, err := b.audit(ctx, cutoff, now, ingest.PruneBatch)
		if err != nil {
			return err
		}
		if res.Rows > 0 {
			pruned.Rows, pruned.Through, pruned.AnchorHead = pruned.Rows+res.Rows, res.Through, res.AnchorHead
		}
		if res.Rows == 0 || !b.now().Before(until) || ctx.Err() != nil {
			break
		}
	}
	b.auditSlot = slot
	if pruned.Rows > 0 {
		b.logger.Info("audit rows older than the retention were removed up to the signed audit anchor", slog.String("event", "audit_pruned"),
			slog.Int64("through", pruned.Through), slog.Int64("rows", pruned.Rows), slog.String("anchor_head", pruned.AnchorHead))
	}
	return nil
}

func (b *scheduler) record(ctx context.Context, at time.Time, keys ...string) error {
	v := strconv.FormatInt(at.UnixMilli(), 10)
	return b.archive.Write(ctx, "backup.record", func(tx *ingest.Tx) error {
		for _, key := range keys {
			if err := tx.SetSyncValue(key, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *scheduler) forget(ctx context.Context) error {
	var recorded bool
	if err := b.archive.Read(ctx, "backup.initial", func(r *ingest.Reader) error {
		for _, key := range []string{pairedAtKey, takenAtKey} {
			v, _, err := r.SyncValue(key)
			if err != nil {
				return err
			}
			recorded = recorded || v != ""
		}
		return nil
	}); err != nil || !recorded {
		return err
	}
	return b.archive.Write(ctx, "backup.forget", func(tx *ingest.Tx) error {
		return errors.Join(tx.SetSyncValue(pairedAtKey, ""), tx.SetSyncValue(takenAtKey, ""))
	})
}
