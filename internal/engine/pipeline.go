package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	maxMessageBytes = 512 << 10
	maxAttempts     = 3
	retryBase       = time.Second
	sweepInterval   = time.Minute
	sweepBatch      = 100
	spaceInterval   = 30 * time.Second
	maxAlerted      = 1024
)

type pipeline struct {
	archive *ingest.Store
	clock   Clock
	owner   policy.CanonicalChat
	logger  *slog.Logger
	notify  Notifier
	counts  *counters
	kick    chan struct{}
	paused  atomic.Bool
	ctx     context.Context

	nextSpace time.Time
	nextSweep time.Time
	alerted   map[conflict]bool

	beforeApply func(seq int64)
	space       func() (ingest.Space, error)
}

func newPipeline(o Options) *pipeline {
	var owner policy.CanonicalChat
	if digits := ownerDigits(o.OwnerPhone); digits != "" {
		owner, _ = policy.Normalize(digits + "@s.whatsapp.net")
	}
	return &pipeline{
		archive: o.Archive, clock: o.Clock, owner: owner, logger: o.Logger, notify: o.Notify,
		counts: newCounters(o.Metrics), kick: make(chan struct{}, 1), alerted: map[conflict]bool{},
	}
}

func (p *pipeline) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *pipeline) write(ctx context.Context, op string, fn func(*ingest.Tx) error) error {
	err := p.archive.Write(ctx, op, fn)
	if errors.Is(err, ingest.ErrRewritePending) {
		p.logger.Warn("the full-text index rewrite after a deletion failed and stays due", slog.String("event", "index_rewrite_pending"), slog.String("operation", op))
		return nil
	}
	return err
}

func (p *pipeline) record(out outcome) {
	for range out.inserted {
		p.counts.ingested.Inc()
	}
	for _, reason := range out.dropped {
		p.counts.dropped.With(reason).Inc()
	}
	for _, c := range out.conflicts {
		p.counts.conflicts.With(string(c.kind)).Inc()
		if p.alerted[c] {
			continue
		}
		if len(p.alerted) == maxAlerted {
			clear(p.alerted)
		}
		p.alerted[c] = true
		p.notify.RekeyConflict(string(c.kind))
	}
}

func (p *pipeline) accept(ev Event) bool {
	var chat string
	switch e := ev.(type) {
	case Message:
		if e.size() > maxMessageBytes {
			p.counts.dropped.With(dropTooLarge).Inc()
			return true
		}
		chat = e.Chat
	case Group:
		chat = e.Chat
	case ContactName:
		chat = e.User
	}
	if _, ok := policy.Normalize(chat); !ok {
		p.counts.dropped.With(dropChat).Inc()
		return true
	}
	if p.paused.Load() {
		p.counts.refused.With(refusedPaused).Inc()
		return false
	}
	payload, err := encodePayload(ev)
	if err != nil {
		p.counts.dropped.With(dropInvalid).Inc()
		return true
	}
	err = p.write(p.ctx, "engine.inbox_append", func(tx *ingest.Tx) error {
		_, err := tx.AppendInbox(payload, p.clock.Now())
		return err
	})
	switch {
	case errors.Is(err, ingest.ErrInboxFull):
		p.counts.refused.With(refusedBacklog).Inc()
		return false
	case err != nil:
		p.counts.refused.With(refusedStore).Inc()
		return false
	}
	p.wake()
	return true
}

func (p *pipeline) checkSpace() {
	check := p.archive.CheckSpace
	if p.space != nil {
		check = p.space
	}
	space, err := check()
	if err != nil {
		p.logger.Warn("the free space of the data directory cannot be read", slog.String("event", "free_space_unknown"), slog.String("error_type", fmt.Sprintf("%T", err)))
		return
	}
	p.paused.Store(space.Paused)
	switch {
	case space.Changed && space.Paused:
		p.notify.IngestPaused(space.Free, space.Floor)
	case space.Changed:
		p.logger.Info("ingest resumed: the data directory has free space again", slog.String("event", "ingest_resumed"), slog.Uint64("free_bytes", space.Free), slog.Uint64("floor_bytes", space.Floor))
	}
}

func (p *pipeline) tick(ctx context.Context) bool {
	now := p.clock.Now()
	if !now.Before(p.nextSpace) {
		p.checkSpace()
		p.nextSpace = now.Add(spaceInterval)
	}
	if !now.Before(p.nextSweep) {
		p.sweep(ctx)
		p.nextSweep = now.Add(sweepInterval)
	}
	return !p.paused.Load()
}

func (p *pipeline) drainInbox(ctx context.Context) {
	for ctx.Err() == nil && p.tick(ctx) {
		var item ingest.InboxItem
		var ok bool
		err := p.archive.Read(ctx, "engine.inbox_next", func(r *ingest.Reader) error {
			var err error
			item, ok, err = r.NextInbox()
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Warn("reading the inbox failed", slog.String("event", "ingest_failed"), slog.String("queue", queueInbox), slog.String("error_type", fmt.Sprintf("%T", err)))
			_ = wait(ctx, p.clock, retryBase)
			return
		}
		if !ok {
			return
		}
		p.process(ctx, item)
	}
}

func (p *pipeline) process(ctx context.Context, item ingest.InboxItem) {
	run := context.WithoutCancel(ctx)
	var attempts int
	if err := p.write(run, "engine.inbox_attempt", func(tx *ingest.Tx) error {
		var err error
		attempts, err = tx.RecordInboxAttempt(item.Seq)
		return err
	}); err != nil {
		p.logger.Warn("recording an inbox attempt failed", slog.String("event", "ingest_failed"), slog.String("queue", queueInbox), slog.String("error_type", fmt.Sprintf("%T", err)))
		_ = wait(ctx, p.clock, retryBase)
		return
	}
	if attempts > maxAttempts {
		p.quarantine(run, item.Seq, attempts-1)
		return
	}
	var out outcome
	err := guarded("engine.ingest", func() error {
		if p.beforeApply != nil {
			p.beforeApply(item.Seq)
		}
		var err error
		out, err = p.apply(run, item)
		return err
	})
	switch {
	case err == nil:
		p.record(out)
		return
	case errors.Is(err, ingest.ErrInvalid):
		if err := p.write(run, "engine.inbox_drop", func(tx *ingest.Tx) error { return tx.DeleteInbox(item.Seq) }); err == nil {
			p.counts.dropped.With(dropInvalid).Inc()
		}
		return
	}
	p.logger.Warn("applying an inbox row failed", slog.String("event", "ingest_failed"), slog.String("queue", queueInbox), slog.Int("attempt", attempts), slog.String("error_type", fmt.Sprintf("%T", err)))
	if attempts >= maxAttempts {
		p.quarantine(run, item.Seq, attempts)
		return
	}
	_ = wait(ctx, p.clock, retryBase<<(attempts-1))
}

func (p *pipeline) apply(ctx context.Context, item ingest.InboxItem) (outcome, error) {
	ev, err := decodePayload(item.Payload)
	if err != nil {
		return outcome{}, err
	}
	var out outcome
	err = p.write(ctx, "engine.ingest", func(tx *ingest.Tx) error {
		a := &applier{tx: tx, owner: p.owner, now: p.clock.Now()}
		var err error
		switch e := ev.(type) {
		case Message:
			err = a.message(e, ingest.OriginLive)
		case Group:
			err = a.group(e, ingest.OriginLive)
		case ContactName:
			err = a.contactName(e)
		}
		if err != nil {
			return err
		}
		out = a.out
		return tx.DeleteInbox(item.Seq)
	})
	return out, err
}

func (p *pipeline) quarantine(ctx context.Context, seq int64, attempts int) {
	if err := p.write(ctx, "engine.inbox_quarantine", func(tx *ingest.Tx) error { return tx.QuarantineInbox(seq) }); err != nil {
		p.logger.Warn("quarantining an inbox row failed", slog.String("event", "ingest_failed"), slog.String("queue", queueInbox), slog.String("error_type", fmt.Sprintf("%T", err)))
		return
	}
	p.counts.quarantined.With(queueInbox).Inc()
	p.notify.Quarantine(queueInbox, attempts)
}

func (p *pipeline) sweep(ctx context.Context) {
	for ctx.Err() == nil {
		var purged int
		if err := p.write(ctx, "engine.expiry_sweep", func(tx *ingest.Tx) error {
			var err error
			purged, err = tx.PurgeExpired(p.clock.Now(), sweepBatch)
			return err
		}); err != nil {
			if ctx.Err() == nil {
				p.logger.Warn("purging expired messages failed", slog.String("event", "expiry_sweep_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
			}
			return
		}
		if purged < sweepBatch {
			return
		}
	}
}
