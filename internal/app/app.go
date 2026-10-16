// Package app wires configuration, logging, metrics, handlers and listeners into the running service.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/backup"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/listeners"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/notify"
	"github.com/dortort/wawarden/internal/safego"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	listenerClient = "client"
	listenerAdmin  = "admin"
	listenerHealth = "health"

	shutdownGrace = 10 * time.Second
	emfInterval   = time.Minute
)

var (
	errHealthNotLoopback = errors.New("app: the unauthenticated health listener must be bound to a loopback address")
	errBackupNotStopped  = errors.New("app: the backup did not stop in time")
	errNoMasterKey       = errors.New("app: the service needs the master key")

	sendRejectReasons = []string{"scope", "first_contact", "budget_minute", "budget_hour", "budget_global", "pacer"}
)

type Refusal = ingest.Refusal

type listenerSet interface {
	Inventory() []listeners.Bound
	Serve()
	Err() <-chan error
	Shutdown(context.Context) error
}

type App struct {
	logger      *slog.Logger
	ready       atomic.Bool
	archive     *ingest.Store
	master      *keys.Master
	session     deviceStore
	engine      *engine.Engine
	notify      *notify.Notifier
	emf         *metrics.EMF
	emfEvery    time.Duration
	backup      *scheduler
	backupEvery time.Duration
	starts      int
	recovered   int
	serving     listenerSet
	health      listenerSet
	grace       time.Duration

	afterDrain func()
}

type deviceStore interface {
	Healthy() bool
	Close() error
	Backup(ctx context.Context, staging string, write func(name string, size int64, r io.Reader) error) error
	SchemaVersion() int
	Changes(ctx context.Context) (int64, error)
	Size() (int64, error)
}

type engineParts struct {
	client   engine.Client
	versions engine.VersionSource
	decoder  engine.HistoryDecoder
	session  deviceStore
}

type engineSource func(ctx context.Context, cfg config.Config, media mediaParts, out *logx.Writer, logger, alerts *slog.Logger) (engineParts, error)

func New(ctx context.Context, cfg config.Config, out *logx.Writer, master *keys.Master) (*App, error) {
	if master == nil {
		return nil, errNoMasterKey
	}
	return newAppWith(ctx, cfg, out, master, nil, engineFor(cfg))
}

func newAppWith(ctx context.Context, cfg config.Config, out *logx.Writer, master *keys.Master, auth api.Authenticator, source engineSource) (*App, error) {
	if !cfg.HealthListen.Addr().IsLoopback() {
		return nil, errHealthNotLoopback
	}
	logger := logx.New(out, cfg.LogLevel)
	alerts := logx.New(out, slog.LevelWarn)
	processMemory.apply(logger)
	reg := metrics.NewRegistry()
	safego.Install(logger, reg)
	info := buildinfo.Read()
	reg.GaugeVec("wawarden_build_info", "Build metadata of the running binary.", "version", "revision", "dev").
		With(info.Version, info.Revision, strconv.FormatBool(info.Dev)).Set(1)
	reg.Counter("wawarden_policy_denials_total", "Client requests that the client's grant did not allow.")
	sendsRejected := reg.CounterVec("wawarden_sends_rejected_total", "Sends refused by a scope, the first-contact rule, a budget or the pacer.", "reason")
	for _, reason := range sendRejectReasons {
		sendsRejected.With(reason)
	}
	rejected := func(reason string) { sendsRejected.With(reason).Inc() }

	notifier, err := notify.New(notify.Options{
		Writer: out, Metrics: reg, URL: cfg.Notify.URL, Secret: cfg.Notify.Secret, AllowPrivate: cfg.Notify.AllowPrivate,
	})
	if err != nil {
		return nil, err
	}
	archive, err := ingest.Open(ctx, ingest.Options{
		DataDir:      cfg.DataDir,
		UID:          cfg.UID,
		Profile:      ingest.Profile(cfg.StorageProfile),
		MinFreeBytes: cfg.MinFreeBytes,
		Logger:       logger,
		Master:       master,
		AuditOut:     out,
	})
	if err != nil {
		return nil, err
	}
	if auth == nil {
		auth = archive.Clients()
	}
	cursors, refs, err := sealers(master)
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	warnUnsafeRateCaps(cfg, alerts)
	warnUnsafeSendCaps(cfg, alerts)
	starts, err := archive.RecordStart(ctx, time.Now())
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	recovered, err := archive.RecoverSends(ctx, time.Now())
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	if err := backup.RemoveStaging(cfg.DataDir); err != nil {
		return nil, errors.Join(fmt.Errorf("app: remove the staging of an interrupted backup: %w", err), archive.Close())
	}
	data, err := purgeSpool(cfg.DataDir)
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	media, err := newMediaParts(cfg, data, master)
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	logger.Info("archive opened", slog.String("event", "archive_opened"),
		slog.Int("schema_version", archive.SchemaVersion()), slog.String("profile", string(archive.Profile())),
		slog.Bool("ofd_locking", archive.OFDLocking()), slog.Int("recent_starts", starts))

	a := &App{logger: logger, archive: archive, master: master, notify: notifier, emfEvery: emfInterval, backupEvery: backupInterval, starts: starts, recovered: recovered, grace: shutdownGrace}
	if cfg.MetricsEMF {
		a.emf = metrics.NewEMF(reg, out, time.Now)
	}
	unpairs := &unpairCounter{Notifier: notifier}
	parts, err := source(ctx, cfg, media, out, logger, alerts)
	if err != nil {
		return nil, errors.Join(err, archive.Close())
	}
	a.session = parts.session
	if a.engine, err = engine.New(engine.Options{
		Client: parts.client, Versions: parts.versions, Decoder: parts.decoder, Archive: archive, DataDir: cfg.DataDir,
		OwnerPhone: cfg.OwnerPhone, HistoryMaxBytes: cfg.HistoryMaxBytes, Logger: logger, Notify: unpairs, Metrics: reg,
		MediaOn: media.maxBytes > 0, MediaRefRetention: media.retention,
	}); err != nil {
		return nil, errors.Join(err, a.closeStores())
	}
	a.backup = &scheduler{
		archive: archive, status: a.engine.Status, unpairs: unpairs.count.Load, logger: logger, now: time.Now,
		minute: cfg.BackupMinute, keep: cfg.BackupKeep, retain: time.Duration(cfg.AuditRetention) * 24 * time.Hour,
		gauge: reg.Gauge("wawarden_backup_last_success_timestamp_seconds", "Unix time at which the newest successful backup began, 0 until one succeeds."),
	}
	if master != nil {
		a.backup.audit = archive.Audit().Prune
	}
	switch {
	case cfg.BackupRecipient == "":
		alerts.Warn("no backup recipient is configured: no backup is ever taken", slog.String("event", "backup_disabled"))
	case a.session == nil:
		alerts.Warn("the engine opens no device store: no backup is ever taken", slog.String("event", "backup_disabled"))
	default:
		taker, err := backup.New(backup.Options{
			DataDir: cfg.DataDir, UID: cfg.UID, Recipient: cfg.BackupRecipient, Version: info.Version,
			Archive: archive, Session: a.session, Notify: notifier, Space: archive.Space,
		})
		if err != nil {
			return nil, errors.Join(err, a.closeStores())
		}
		a.backup.take, a.backup.prune, a.backup.changes = taker.Take, taker.Prune, changesOf(archive, a.session)
	}
	if err := a.backup.load(ctx); err != nil {
		return nil, errors.Join(err, a.closeStores())
	}
	specs := []listeners.Spec{{
		Name: listenerClient,
		Addr: cfg.Listen,
		Handler: api.NewClientHandler(api.ClientDeps{
			Authenticator: auth, Metrics: reg, Archive: archive.Scoped(), Audit: readAudit{audit: archive.Audit()},
			Session: func() string { return string(a.engine.Status().State) }, Cursors: cursors, Refs: refs,
			Limits: api.ReadLimits{ReadsPerMinute: cfg.ReadPerMinute, SearchesPerMinute: cfg.SearchPerMinute},
			Sender: sendService{sender: newSender(cfg, archive, a.engine, notifier), rejected: rejected}, SendRejected: rejected,
			AuditFailed: auditFailures(reg, alerts),
		}),
	}}
	if cfg.AdminCredential != nil {
		specs = append(specs, listeners.Spec{
			Name: listenerAdmin,
			Addr: cfg.AdminListen,
			Handler: api.NewAdminHandler(api.AdminDeps{
				Credential: *cfg.AdminCredential,
				Metrics:    reg,
				Service:    adminService{engine: a.engine, archive: archive, version: info.Version, now: time.Now},
				Events:     notifier,
			}),
		})
	}
	serving, err := listeners.Open(ctx, logger, specs)
	if err != nil {
		return nil, errors.Join(err, a.closeStores())
	}
	health, err := listeners.Open(ctx, logger, []listeners.Spec{{
		Name:    listenerHealth,
		Addr:    cfg.HealthListen,
		Handler: api.NewHealthHandler(a.healthy),
	}})
	if err != nil {
		return nil, errors.Join(err, serving.Shutdown(ctx), a.closeStores())
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

func changesOf(archive, session backup.Database) func(context.Context) ([2]int64, error) {
	return func(ctx context.Context) ([2]int64, error) {
		a, err := archive.Changes(ctx)
		if err != nil {
			return [2]int64{}, err
		}
		s, err := session.Changes(ctx)
		return [2]int64{a, s}, err
	}
}

func (a *App) healthy() bool {
	return a.ready.Load() && a.archive.Healthy() && (a.session == nil || a.session.Healthy())
}

func (a *App) announceKeyChange(ctx context.Context) {
	if a.master == nil {
		return
	}
	id, ok, err := a.archive.NewestAuditKeyID(ctx)
	switch {
	case err != nil:
		a.logger.Warn("the key id of the newest audit row cannot be read", slog.String("event", "audit_key_unread"), slog.String("error_type", fmt.Sprintf("%T", err)))
	case !ok || id == a.master.ID():
	default:
		previous := "missing"
		if _, retired := a.master.Lookup(id); retired {
			previous = "retired"
		}
		a.notify.AuditKeyChanged(id, a.master.ID(), previous)
	}
}

func (a *App) closeStores() error {
	err := a.archive.Close()
	if a.session != nil {
		err = errors.Join(err, a.session.Close())
	}
	return err
}

func (a *App) Inventory() []listeners.Bound {
	return append(a.serving.Inventory(), a.health.Inventory()...)
}

func (a *App) Run(ctx context.Context) error {
	a.notify.Start(context.WithoutCancel(ctx))
	a.announceKeyChange(ctx)
	for range a.recovered {
		a.notify.SendOutcomeUnknown(reasonRestart)
	}
	a.engine.Start(context.WithoutCancel(ctx), a.starts)
	emitting, stopEmitting := context.WithCancel(context.WithoutCancel(ctx))
	defer stopEmitting()
	emitted := make(chan struct{})
	if a.emf != nil {
		safego.Go("metrics.emf", func() {
			defer close(emitted)
			a.emitEvery(emitting)
		})
	} else {
		close(emitted)
	}
	backingUp, stopBackingUp := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackingUp()
	backedUp := make(chan struct{})
	safego.Go("backup.schedule", func() {
		defer close(backedUp)
		a.backup.run(backingUp, a.backupEvery)
	})
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
	stopped := a.engine.Stop(grace)
	stopBackingUp()
	var backupStopped error
	select {
	case <-backedUp:
	case <-grace.Done():
		backupStopped = errBackupNotStopped
	}
	stopEmitting()
	<-emitted
	if a.emf != nil {
		a.emit()
	}
	err := errors.Join(failure, drained, stopped, backupStopped, a.notify.Stop(grace), a.closeStores(), a.health.Shutdown(grace))
	if err != nil {
		a.logger.Error("stopped with errors", slog.String("event", "stopped"), slog.String("error", err.Error()))
		return err
	}
	a.logger.Info("stopped", slog.String("event", "stopped"))
	return nil
}

func (a *App) emitEvery(ctx context.Context) {
	tick := time.NewTicker(a.emfEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.emit()
		}
	}
}

func (a *App) emit() {
	if err := a.emf.Emit(); err != nil {
		a.logger.Warn("writing the embedded-metric-format lines failed", slog.String("event", "emf_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
	}
}
