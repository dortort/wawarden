package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/engine/wa"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/stage"
	"github.com/dortort/wawarden/internal/store/session"
)

type mediaParts struct {
	maxBytes  int64
	retention time.Duration
	sealer    wa.MediaSealer
	spool     *stage.Spool
}

func newMediaParts(cfg config.Config, data string, master *keys.Master) (mediaParts, error) {
	m := mediaParts{maxBytes: cfg.MediaMaxBytes, retention: time.Duration(cfg.MediaRefRetentionDays) * 24 * time.Hour}
	var err error
	if m.sealer, err = mediaSealer(master); err != nil {
		return mediaParts{}, err
	}
	if m.maxBytes > 0 {
		if m.spool, err = stage.New(stage.Options{Data: data, Local: cfg.StorageProfile == config.StorageLocal, MinFree: cfg.MinFreeBytes, UID: cfg.UID}); err != nil {
			return mediaParts{}, err
		}
	}
	return m, nil
}

func (m mediaParts) adapterOptions(o wa.Options) wa.Options {
	o.MediaMaxBytes, o.MediaRefRetention, o.MediaSealer, o.MediaSpool = m.maxBytes, m.retention, m.sealer, m.spool
	return o
}

func mediaSealer(master *keys.Master) (wa.MediaSealer, error) {
	if master == nil {
		return nil, nil
	}
	var retired []cursor.Retired
	for _, k := range master.Retired() {
		retired = append(retired, cursor.Retired{Key: k.MediaRefKey(), ID: k.ID()})
	}
	sealer, err := cursor.New(master.MediaRefKey(), master.ID(), retired...)
	if err != nil {
		return nil, err
	}
	return sealer, nil
}

func openWhatsApp(ctx context.Context, cfg config.Config, media mediaParts, out *logx.Writer, logger, alerts *slog.Logger) (engineParts, error) {
	store, err := session.Open(ctx, session.Options{DataDir: cfg.DataDir, UID: cfg.UID, Profile: session.Profile(cfg.StorageProfile), Logger: logger})
	if err != nil {
		return engineParts{}, err
	}
	device, err := store.Device(ctx)
	if err != nil {
		return engineParts{}, errors.Join(err, store.Close())
	}
	client, err := wa.New(media.adapterOptions(wa.Options{
		Device: device, Devices: store, OwnerPhone: cfg.OwnerPhone, HistoryMaxBytes: cfg.HistoryMaxBytes,
		Writer: out, LogLevel: cfg.LogLevel, Alerts: alerts, UnsafeDebug: cfg.UnsafeDebug,
	}))
	if err != nil {
		return engineParts{}, errors.Join(err, store.Close())
	}
	logger.Info("session opened", slog.String("event", "session_opened"), slog.Bool("paired", client.Paired()))
	return engineParts{client: client, versions: wa.NewVersions(), decoder: client, session: store}, nil
}
