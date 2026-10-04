// Package app wires configuration, logging, metrics, handlers and listeners into the running service.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/listeners"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	listenerClient = "client"
	listenerAdmin  = "admin"
	listenerHealth = "health"

	shutdownGrace = 10 * time.Second
)

type App struct {
	logger  *slog.Logger
	ready   atomic.Bool
	serving *listeners.Set
	health  *listeners.Set
	grace   time.Duration

	afterDrain func()
}

func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				return slog.Time(slog.TimeKey, a.Value.Time().UTC())
			}
			return a
		},
	}))
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	return newApp(ctx, cfg, logger, noClients{})
}

func newApp(ctx context.Context, cfg config.Config, logger *slog.Logger, auth api.Authenticator) (*App, error) {
	reg := metrics.NewRegistry()
	safego.Install(logger, reg)
	info := buildinfo.Read()
	reg.GaugeVec("wawarden_build_info", "Build metadata of the running binary.", "version", "revision", "dev").
		With(info.Version, info.Revision, strconv.FormatBool(info.Dev)).Set(1)

	a := &App{logger: logger, grace: shutdownGrace}
	specs := []listeners.Spec{{
		Name:    listenerClient,
		Addr:    cfg.Listen,
		Handler: api.NewClientHandler(api.ClientDeps{Authenticator: auth, Metrics: reg}),
	}}
	if cfg.AdminCredential != nil {
		specs = append(specs, listeners.Spec{
			Name:    listenerAdmin,
			Addr:    cfg.AdminListen,
			Handler: api.NewAdminHandler(api.AdminDeps{Credential: *cfg.AdminCredential, Metrics: reg}),
		})
	}
	serving, err := listeners.Open(ctx, logger, specs)
	if err != nil {
		return nil, err
	}
	health, err := listeners.Open(ctx, logger, []listeners.Spec{{
		Name:    listenerHealth,
		Addr:    cfg.HealthListen,
		Handler: api.NewHealthHandler(a.ready.Load),
	}})
	if err != nil {
		return nil, errors.Join(err, serving.Shutdown(ctx))
	}
	a.serving, a.health = serving, health

	logger.Info("starting", slog.String("event", "starting"),
		slog.String("version", info.Version), slog.String("revision", info.Revision),
		slog.Bool("modified", info.Modified), slog.Bool("dev", info.Dev))
	if info.Dev {
		logger.Warn("development build, not for production use", slog.String("event", "dev_build"))
	}
	for _, b := range a.Inventory() {
		logger.Info("listening", slog.String("event", "listening"), slog.String("listener", b.Name), slog.String("address", b.Addr.String()))
		if !b.Addr.Addr().IsLoopback() {
			logger.Warn("listener reachable beyond loopback: plain HTTP carries bearer tokens, so securing the network path is the deployer's responsibility",
				slog.String("event", "listener_not_loopback"), slog.String("listener", b.Name), slog.String("address", b.Addr.String()))
		}
	}
	return a, nil
}

func (a *App) Inventory() []listeners.Bound {
	return append(a.serving.Inventory(), a.health.Inventory()...)
}

func (a *App) Run(ctx context.Context) error {
	a.ready.Store(true)
	a.serving.Serve()
	a.health.Serve()
	a.logger.Info("ready", slog.String("event", "ready"))

	var failure error
	select {
	case <-ctx.Done():
	case failure = <-a.serving.Err():
	case failure = <-a.health.Err():
	}
	a.ready.Store(false)
	if failure != nil {
		a.logger.Error("listener failed", slog.String("event", "listener_failed"), slog.String("error", failure.Error()))
	}
	a.logger.Info("shutting down", slog.String("event", "shutdown_started"))

	drained := a.shutdown(ctx, a.serving)
	if a.afterDrain != nil {
		a.afterDrain()
	}
	err := errors.Join(failure, drained, a.shutdown(ctx, a.health))
	if err != nil {
		a.logger.Error("stopped with errors", slog.String("event", "stopped"), slog.String("error", err.Error()))
		return err
	}
	a.logger.Info("stopped", slog.String("event", "stopped"))
	return nil
}

func (a *App) shutdown(ctx context.Context, s *listeners.Set) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.grace)
	defer cancel()
	return s.Shutdown(ctx)
}

type noClients struct{}

func (noClients) Authenticate(context.Context, string) (*policy.Client, bool) { return nil, false }
