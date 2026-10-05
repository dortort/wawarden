package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	settleAfter    = 10 * time.Minute
	backupInterval = 30 * time.Second

	pairedAtKey = "backup_paired_at"
	takenAtKey  = "backup_taken_at"
)

type initialBackup struct {
	archive *ingest.Store
	status  func() engine.Status
	take    func(context.Context) error
	now     func() time.Time
	tried   bool
}

func (b *initialBackup) run(ctx context.Context, every time.Duration, logger *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := b.check(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("checking whether the initial backup is due failed", slog.String("event", "backup_check_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *initialBackup) check(ctx context.Context) error {
	st := b.status()
	if !st.Paired {
		return b.forget(ctx)
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
		return b.record(ctx, pairedAtKey)
	}
	since := time.UnixMilli(settled)
	if arrived && last.After(since) {
		since = last
	}
	if len(pending) > 0 || b.now().Before(since.Add(settleAfter)) || b.tried {
		return nil
	}
	b.tried = true
	if b.take(ctx) != nil {
		return nil
	}
	return b.record(ctx, takenAtKey)
}

func (b *initialBackup) record(ctx context.Context, key string) error {
	at := strconv.FormatInt(b.now().UnixMilli(), 10)
	return b.archive.Write(ctx, "backup.record", func(tx *ingest.Tx) error { return tx.SetSyncValue(key, at) })
}

func (b *initialBackup) forget(ctx context.Context) error {
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
