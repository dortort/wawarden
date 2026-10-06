package app

import (
	"context"
	"log/slog"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/store/admin"
)

type readAudit struct{ audit *admin.Audit }

func (r readAudit) Record(ctx context.Context, e api.ReadEvent) error {
	return r.audit.Record(ctx, admin.Event(e))
}

func sealers(master *keys.Master) (cursors, refs *cursor.Sealer, err error) {
	if master == nil {
		return nil, nil, nil
	}
	if cursors, err = cursor.New(master.CursorSealKey(), master.ID()); err != nil {
		return nil, nil, err
	}
	if refs, err = cursor.New(master.MessageRefKey(), master.ID()); err != nil {
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
