//go:build dev

package app

import (
	"context"
	"log/slog"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine/fake"
	"github.com/dortort/wawarden/internal/logx"
)

func engineFor(cfg config.Config) engineSource {
	if cfg.Dev.FakeEngine {
		return openFake
	}
	return openWhatsApp
}

func openFake(_ context.Context, cfg config.Config, _ *logx.Writer, _, alerts *slog.Logger) (engineParts, error) {
	client, err := fake.New(fake.Options{OwnerPhone: cfg.OwnerPhone, WrongAccount: cfg.Dev.FakeWrongAccount, HistoryMaxBytes: cfg.HistoryMaxBytes})
	if err != nil {
		return engineParts{}, err
	}
	alerts.Warn("the fake engine replaces WhatsApp: it plays synthetic events and opens no session store and no connection",
		slog.String("event", "fake_engine"), slog.Bool("wrong_account", cfg.Dev.FakeWrongAccount))
	return engineParts{client: client, versions: client, decoder: client}, nil
}
