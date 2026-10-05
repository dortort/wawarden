package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine/wa"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/session"
)

func openWhatsApp(ctx context.Context, cfg config.Config, out *logx.Writer, logger, alerts *slog.Logger) (engineParts, error) {
	store, err := session.Open(ctx, session.Options{DataDir: cfg.DataDir, UID: cfg.UID, Profile: session.Profile(cfg.StorageProfile), Logger: logger})
	if err != nil {
		return engineParts{}, err
	}
	device, err := store.Device(ctx)
	if err != nil {
		return engineParts{}, errors.Join(err, store.Close())
	}
	client, err := wa.New(wa.Options{
		Device: device, Devices: store, OwnerPhone: cfg.OwnerPhone, HistoryMaxBytes: cfg.HistoryMaxBytes,
		Writer: out, LogLevel: cfg.LogLevel, Alerts: alerts, UnsafeDebug: cfg.UnsafeDebug,
	})
	if err != nil {
		return engineParts{}, errors.Join(err, store.Close())
	}
	logger.Info("session opened", slog.String("event", "session_opened"), slog.Bool("paired", client.Paired()))
	return engineParts{client: client, versions: wa.NewVersions(), decoder: client, session: store}, nil
}
