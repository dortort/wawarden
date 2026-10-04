// Package app wires configuration, logging, metrics, handlers and listeners into the running service.
package app

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/listeners"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	listenerClient = "client"
	listenerAdmin  = "admin"
	listenerHealth = "health"

	shutdownGrace = 10 * time.Second
)

var errHealthNotLoopback = errors.New("app: the unauthenticated health listener must be bound to a loopback address")

type Refusal = ingest.Refusal

type listenerSet interface {
	Inventory() []listeners.Bound
	Serve()
	Err() <-chan error
	Shutdown(context.Context) error
}

type App struct {
	logger  *slog.Logger
	ready   atomic.Bool
	archive *ingest.Store
	serving listenerSet
	health  listenerSet
	grace   time.Duration

	afterDrain func()
}

func New(ctx context.Context, cfg config.Config, out *logx.Writer) (*App, error) {
	return newApp(ctx, cfg, out, noClients{})
}

func newApp(ctx context.Context, cfg config.Config, out *logx.Writer, auth api.Authenticator) (*App, error) {
	if !cfg.HealthListen.Addr().IsLoopback() {
		return nil, errHealthNotLoopback
	}
	logger := logx.New(out, cfg.LogLevel)
	alerts := logx.New(out, slog.LevelWarn)
	reg := metrics.NewRegistry()
	safego.Install(logger, reg)
	info := buildinfo.Read()
	reg.GaugeVec("wawarden_build_info", "Build metadata of the running binary.", "version", "revision", "dev").
		With(info.Version, info.Revision, strconv.FormatBool(info.Dev)).Set(1)

	archive, err := ingest.Open(ctx, ingest.Options{
		DataDir:      cfg.DataDir,
		UID:          cfg.UID,
		Profile:      ingest.Profile(cfg.StorageProfile),
		MinFreeBytes: cfg.MinFreeBytes,
		Logger:       logger,
	})
	if err != nil {
		return nil, err
	}
	starts, err := archive.RecordStart(ctx, time.Now())
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	logger.Info("archive opened", slog.String("event", "archive_opened"),
		slog.Int("schema_version", archive.SchemaVersion()), slog.String("profile", string(archive.Profile())),
		slog.Bool("ofd_locking", archive.OFDLocking()), slog.Int("recent_starts", starts))

	a := &App{logger: logger, archive: archive, grace: shutdownGrace}
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
		return nil, errors.Join(err, archive.Close())
	}
	health, err := listeners.Open(ctx, logger, []listeners.Spec{{
		Name:    listenerHealth,
		Addr:    cfg.HealthListen,
		Handler: api.NewHealthHandler(a.healthy),
	}})
	if err != nil {
		return nil, errors.Join(err, serving.Shutdown(ctx), archive.Close())
	}
	a.serving, a.health = serving, health

	logger.Info("starting", slog.String("event", "starting"),
		slog.String("version", info.Version), slog.String("revision", info.Revision),
		slog.Bool("modified", info.Modified), slog.Bool("dev", info.Dev))
	if info.Dev {
		alerts.Warn("development build, not for production use", slog.String("event", "dev_build"))
	}
	for _, b := range a.Inventory() {
		logger.Info("listening", slog.String("event", "listening"), slog.String("listener", b.Name), slog.String("address", b.Addr.String()))
		if !b.Addr.Addr().IsLoopback() {
			alerts.Warn("listener reachable beyond loopback: plain HTTP carries bearer tokens, so securing the network path is the deployer's responsibility",
				slog.String("event", "listener_not_loopback"), slog.String("listener", b.Name), slog.String("address", b.Addr.String()))
		}
	}
	return a, nil
}

func (a *App) healthy() bool { return a.ready.Load() && a.archive.Healthy() }

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

	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.grace)
	defer cancel()
	drained := a.serving.Shutdown(grace)
	if a.afterDrain != nil {
		a.afterDrain()
	}
	err := errors.Join(failure, drained, a.archive.Close(), a.health.Shutdown(grace))
	if err != nil {
		a.logger.Error("stopped with errors", slog.String("event", "stopped"), slog.String("error", err.Error()))
		return err
	}
	a.logger.Info("stopped", slog.String("event", "stopped"))
	return nil
}

type noClients struct{}

func (noClients) Authenticate(context.Context, string) (*policy.Client, bool) { return nil, false }
