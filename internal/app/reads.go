package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/store/admin"
)

type readAudit struct{ audit *admin.Audit }

func (r readAudit) Record(ctx context.Context, e api.ReadEvent) error {
	return r.audit.Record(ctx, admin.Event(e))
}

func auditFailures(reg *metrics.Registry, alerts *slog.Logger) func(action string, err error) {
	failures := reg.Counter("wawarden_audit_failures_total", "Audit rows that could not be written once their response had begun.")
	return func(action string, err error) {
		failures.Inc()
		alerts.Warn("an audit row could not be written once its response had begun",
			slog.String("event", "audit_failed"), slog.String("action", action), slog.String("error_type", fmt.Sprintf("%T", err)))
	}
}

func sealers(master *keys.Master) (cursors, refs *cursor.Sealer, err error) {
	if master == nil {
		return nil, nil, nil
	}
	var retiredCursors, retiredRefs []cursor.Retired
	for _, k := range master.Retired() {
		retiredCursors = append(retiredCursors, cursor.Retired{Key: k.CursorSealKey(), ID: k.ID()})
		retiredRefs = append(retiredRefs, cursor.Retired{Key: k.MessageRefKey(), ID: k.ID()})
	}
	if cursors, err = cursor.New(master.CursorSealKey(), master.ID(), retiredCursors...); err != nil {
		return nil, nil, err
	}
	if refs, err = cursor.NewRef(master.MessageRefKey(), master.ID(), retiredRefs...); err != nil {
		return nil, nil, err
	}
	return cursors, refs, nil
}

func warnUnsafeRateCaps(cfg config.Config, alerts *slog.Logger) {
	if cfg.UnsafeRateCaps {
		alerts.Warn("the upper caps of the read and search budgets are lifted: a client can read the archive faster than the documented limits",
			slog.String("event", "unsafe_rate_caps"), slog.Int("read_per_minute", cfg.ReadPerMinute), slog.Int("search_per_minute", cfg.SearchPerMinute))
	}
}
