package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/dortort/wawarden/internal/safego"
	"github.com/dortort/wawarden/internal/stage"
)

const spoolSweepInterval = time.Hour

func purgeSpool(dataDir string) (string, error) {
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("app: resolve the data directory: %w", err)
	}
	if _, err := stage.Purge(data); err != nil {
		return "", fmt.Errorf("app: remove the attachments staged before this start: %w", err)
	}
	return data, nil
}

func (a *App) sweepSpool(ctx context.Context) (stop func()) {
	if a.spool == nil {
		return func() {}
	}
	sweeping, cancel := context.WithCancel(context.WithoutCancel(ctx))
	swept := make(chan struct{})
	safego.Go("stage.sweep", func() {
		defer close(swept)
		tick := time.NewTicker(a.sweepEvery)
		defer tick.Stop()
		a.sweepOn(sweeping, tick.C)
	})
	return func() {
		cancel()
		<-swept
	}
}

func (a *App) sweepOn(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if _, err := a.spool.Sweep(); err != nil {
				a.logger.Warn("removing stray staged attachments failed", slog.String("event", "spool_sweep_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
			}
		}
	}
}
