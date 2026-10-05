// Package notify is the one place that emits operational events: one fixed-shape JSON line on standard output for each, also posted, signed, to an optional webhook.
package notify

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	EventAdminMutation    = "admin_mutation"
	EventAdminAuthFailure = "admin_auth_failure"
	EventUnpaired         = "unpaired"
	EventDisconnected     = "disconnected"
	EventPairRejected     = "pair_rejected"
	EventLogoutFailed     = "logout_failed"
	EventQuarantine       = "quarantine"
	EventRekeyConflict    = "rekey_conflict"
	EventIngestPaused     = "ingest_paused"
)

const (
	authFailureWindow = time.Minute
	flushInterval     = 10 * time.Second
	invalidValue      = "invalid"
	maxGoTypeBytes    = 128
)

var (
	codeShape     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	goTypeShape   = regexp.MustCompile(`^\**[a-z][a-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*(\[[A-Za-z0-9_./*,\[\]]+\])?$`)
	errNoWriter   = errors.New("notify: a log writer is required")
	errNotStarted = errors.New("notify: Stop before Start")
)

type Options struct {
	Writer       *logx.Writer
	Metrics      *metrics.Registry
	URL          string
	Secret       []byte
	AllowPrivate bool
	Now          func() time.Time
}

type Notifier struct {
	logger *slog.Logger
	now    func() time.Time
	hook   *webhook

	authMu      sync.Mutex
	authPending int64
	authLast    time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

func New(o Options) (*Notifier, error) {
	if o.Writer == nil {
		return nil, errNoWriter
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	n := &Notifier{logger: logx.New(o.Writer, slog.LevelInfo), now: now}
	if o.URL != "" {
		hook, err := newWebhook(o, n.logger, now)
		if err != nil {
			return nil, err
		}
		n.hook = hook
	}
	return n, nil
}

func (n *Notifier) Start(ctx context.Context) {
	ctx, n.cancel = context.WithCancel(ctx)
	n.done = make(chan struct{})
	if n.hook != nil {
		safego.Go("notify.webhook", n.hook.run)
	}
	safego.Go("notify.flush", func() {
		defer close(n.done)
		tick := time.NewTicker(flushInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				n.flushAuthFailures(false)
			}
		}
	})
}

func (n *Notifier) Stop(ctx context.Context) error {
	if n.cancel == nil {
		return errNotStarted
	}
	n.cancel()
	var err error
	select {
	case <-n.done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	n.flushAuthFailures(true)
	if n.hook != nil {
		err = errors.Join(err, n.hook.shutdown(ctx))
	}
	return err
}

func (n *Notifier) Unpaired() {
	n.emit(slog.LevelWarn, "no WhatsApp device is paired: the engine makes no connection until pairing is requested", EventUnpaired)
}

func (n *Notifier) Disconnected(reason string) {
	n.emit(slog.LevelWarn, "the engine is disconnected from WhatsApp", EventDisconnected, code("reason", reason))
}

func (n *Notifier) PairRejected(stage string) {
	n.emit(slog.LevelWarn, "pairing was rejected: the account is not the owner's", EventPairRejected, code("stage", stage))
}

func (n *Notifier) LogoutFailed(attempt int, errorType string) {
	n.emit(slog.LevelWarn, "logging out the rejected device failed", EventLogoutFailed,
		slog.Int("attempt", attempt), goType("error_type", errorType))
}

func (n *Notifier) Quarantine(queue string, attempts int) {
	n.emit(slog.LevelWarn, "an item failed three times and was quarantined", EventQuarantine,
		code("queue", queue), slog.Int("attempts", attempts))
}

func (n *Notifier) RekeyConflict(conflict string) {
	n.emit(slog.LevelWarn, "an identity mapping contradicts the archive and was not applied", EventRekeyConflict, code("conflict", conflict))
}

func (n *Notifier) IngestPaused(freeBytes, floorBytes uint64) {
	n.emit(slog.LevelWarn, "ingest paused: the data directory is below its free-space floor", EventIngestPaused,
		slog.Uint64("free_bytes", freeBytes), slog.Uint64("floor_bytes", floorBytes))
}

func (n *Notifier) AdminMutation(action, outcome string) {
	n.emit(slog.LevelInfo, "an admin route that changes the service's state was called", EventAdminMutation,
		code("action", action), code("outcome", outcome))
}

func (n *Notifier) AdminAuthFailure() {
	n.authMu.Lock()
	n.authPending++
	count, due := n.dueLocked(false)
	n.authMu.Unlock()
	if due {
		n.authFailures(count)
	}
}

func (n *Notifier) flushAuthFailures(force bool) {
	n.authMu.Lock()
	count, due := n.dueLocked(force)
	n.authMu.Unlock()
	if due {
		n.authFailures(count)
	}
}

func (n *Notifier) dueLocked(force bool) (int64, bool) {
	now := n.now()
	if n.authPending == 0 || !force && !n.authLast.IsZero() && now.Sub(n.authLast) < authFailureWindow {
		return 0, false
	}
	count := n.authPending
	n.authPending, n.authLast = 0, now
	return count, true
}

func (n *Notifier) authFailures(count int64) {
	n.emit(slog.LevelWarn, "admin authentication failed", EventAdminAuthFailure, slog.Int64("count", count))
}

func (n *Notifier) emit(level slog.Level, msg, event string, fields ...slog.Attr) {
	n.logger.LogAttrs(context.Background(), level, msg, append([]slog.Attr{slog.String("event", event)}, fields...)...)
	if n.hook != nil {
		id := eventID()
		n.hook.enqueue(delivery{id: id, body: eventBody(id, event, n.now(), fields)})
	}
}

func code(key, value string) slog.Attr {
	if !codeShape.MatchString(value) {
		value = invalidValue
	}
	return slog.String(key, value)
}

func goType(key, value string) slog.Attr {
	if len(value) > maxGoTypeBytes || !goTypeShape.MatchString(value) {
		value = invalidValue
	}
	return slog.String(key, value)
}
