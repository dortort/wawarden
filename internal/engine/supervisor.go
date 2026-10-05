package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

type State string

const (
	StateUnpaired     State = "unpaired"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateDisconnected State = "disconnected"
)

type Reason string

const (
	ReasonOutdated       Reason = "outdated"
	ReasonReplaced       Reason = "replaced"
	ReasonLoggedOut      Reason = "logged_out"
	ReasonTemporaryBan   Reason = "temporary_ban"
	ReasonCATRefresh     Reason = "cat_refresh"
	ReasonConnectFailure Reason = "connect_failure"
	ReasonRestartBudget  Reason = "restart_budget"
	ReasonShutdown       Reason = "shutdown"
)

type Status struct {
	State  State
	Reason Reason
	Paired bool
}

var (
	ErrAlreadyPaired     = errors.New("engine: a device is already paired")
	ErrOwnerPhoneMissing = errors.New("engine: the owner's phone number is not configured")
	ErrPairRateLimited   = errors.New("engine: three pairing attempts were made in the last hour")
	ErrPairFailed        = errors.New("engine: pairing failed")
	ErrNotPaired         = errors.New("engine: no device is paired")
	ErrAlreadyConnected  = errors.New("engine: already connected")
	ErrStopped           = errors.New("engine: stopped")
)

const (
	MaxRecentStarts = 5
	pairAttempts    = 3
	pairWindow      = time.Hour
	versionRetries  = 3
	versionBackoff  = time.Second
	reconnectBase   = 2 * time.Second
	reconnectCap    = 5 * time.Minute
	logoutTimeout   = 30 * time.Second
)

type intent uint8

const (
	idle intent = iota
	refresh
	connect
)

type supervisor struct {
	client    Client
	versions  VersionSource
	owner     string
	logger    *slog.Logger
	alerts    *slog.Logger
	clock     Clock
	jitter    func(time.Duration) time.Duration
	paired    *metrics.Gauge
	connected *metrics.Gauge
	kick      chan struct{}
	done      chan struct{}

	pairMu sync.Mutex

	mu         sync.Mutex
	ctx        context.Context
	state      State
	reason     Reason
	gen        uint64
	next       intent
	outdated   bool
	attempt    int
	dialing    bool
	dropped    bool
	early      bool
	awaiting   bool
	pairs      []time.Time
	cancelStep context.CancelFunc
}

func newSupervisor(o Options) *supervisor {
	return &supervisor{
		client: o.Client, versions: o.Versions, owner: ownerDigits(o.OwnerPhone), logger: o.Logger, alerts: o.Alerts,
		clock: o.Clock, jitter: o.Jitter, kick: make(chan struct{}, 1), done: make(chan struct{}), state: StateUnpaired,
		paired:    o.Metrics.Gauge("wawarden_paired", "1 while a WhatsApp device is paired, 0 otherwise."),
		connected: o.Metrics.Gauge("wawarden_connected", "1 while the engine is connected to WhatsApp, 0 otherwise."),
	}
}

func ownerDigits(e164 string) string {
	digits, _ := policy.OwnerDigits(e164)
	return digits
}

func (s *supervisor) start(ctx context.Context, recentStarts int) {
	s.begin(ctx, recentStarts)
	safego.Go("engine.supervisor", func() { s.run(ctx) })
}

func (s *supervisor) begin(ctx context.Context, recentStarts int) {
	s.mu.Lock()
	s.ctx = ctx
	if recentStarts > MaxRecentStarts {
		s.setLocked(StateDisconnected, ReasonRestartBudget)
	} else {
		s.setLocked(StateConnecting, "")
		s.next, s.outdated = refresh, false
	}
	s.mu.Unlock()
	s.updateGauges()
}

func (s *supervisor) stop() {
	s.mu.Lock()
	s.setLocked(StateDisconnected, ReasonShutdown)
	s.mu.Unlock()
	s.client.Disconnect()
	s.updateGauges()
}

func (s *supervisor) status() Status {
	paired := s.client.Paired()
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{State: s.state, Reason: s.reason, Paired: paired}
}

func (s *supervisor) setLocked(state State, reason Reason) {
	s.gen++
	if s.cancelStep != nil && state != StateConnected {
		s.cancelStep()
		s.cancelStep = nil
	}
	s.dialing, s.dropped, s.early, s.awaiting = false, false, false, false
	s.next = idle
	if s.state == state && s.reason == reason {
		return
	}
	s.state, s.reason = state, reason
	s.logger.Info("engine state changed", slog.String("event", "engine_state"), slog.String("state", string(state)), slog.String("reason", string(reason)))
	if state == StateDisconnected && reason != ReasonShutdown {
		s.alerts.Warn("the engine is disconnected from WhatsApp", slog.String("event", "disconnected"), slog.String("reason", string(reason)))
	}
}

func (s *supervisor) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *supervisor) updateGauges() {
	paired := s.client.Paired()
	s.mu.Lock()
	connected := s.state == StateConnected
	s.mu.Unlock()
	s.paired.Set(gauge(paired))
	s.connected.Set(gauge(connected))
}

func gauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (s *supervisor) run(ctx context.Context) {
	defer close(s.done)
	for ctx.Err() == nil {
		if s.step(ctx) {
			continue
		}
		select {
		case <-ctx.Done():
		case <-s.kick:
		}
	}
}

func (s *supervisor) step(ctx context.Context) bool {
	s.mu.Lock()
	act, gen, attempt, outdated := s.next, s.gen, s.attempt, s.outdated
	s.next = idle
	if act == idle {
		s.mu.Unlock()
		return false
	}
	step, cancel := context.WithCancel(ctx)
	defer cancel()
	s.cancelStep = cancel
	s.mu.Unlock()
	if act == refresh {
		s.refresh(step, gen, outdated)
	} else {
		s.connect(step, gen, attempt)
	}
	s.updateGauges()
	return true
}

func (s *supervisor) current(gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen == gen
}

func (s *supervisor) refresh(ctx context.Context, gen uint64, outdated bool) {
	delay := versionBackoff
	for try := 0; ; try++ {
		have := s.client.Version()
		v, err := s.versions.Latest(ctx)
		usable := err == nil && !v.IsZero() && !v.Less(have) && (v != have || !outdated)
		if usable && v != have {
			if !s.current(gen) {
				return
			}
			s.client.Disconnect()
			s.client.SetVersion(v)
			s.logger.Info("protocol version updated", slog.String("event", "version_updated"), slog.String("version", fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])))
		}
		if usable {
			s.refreshed(gen)
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("protocol version refresh failed", slog.String("event", "version_refresh_failed"), slog.Int("attempt", try+1))
		if try == versionRetries {
			break
		}
		if wait(ctx, s.clock, delay) != nil {
			return
		}
		delay *= 2
	}
	s.mu.Lock()
	if s.gen == gen {
		s.setLocked(StateDisconnected, ReasonOutdated)
	}
	s.mu.Unlock()
}

func (s *supervisor) refreshed(gen uint64) {
	paired := s.client.Paired()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		return
	}
	if !paired {
		s.setLocked(StateUnpaired, "")
		return
	}
	s.next, s.attempt = connect, 0
	s.wake()
}

func (s *supervisor) backoff(attempt int) time.Duration {
	d := reconnectBase
	for i := 1; i < attempt && d < reconnectCap; i++ {
		d *= 2
	}
	d = min(d, reconnectCap)
	half := d / 2
	return half + min(max(s.jitter(d-half), 0), d-half-1)
}

func (s *supervisor) connect(ctx context.Context, gen uint64, attempt int) {
	if attempt > 0 && wait(ctx, s.clock, s.backoff(attempt)) != nil {
		return
	}
	s.mu.Lock()
	if s.gen != gen {
		s.mu.Unlock()
		return
	}
	s.dialing = true
	s.mu.Unlock()
	err := s.client.Connect(ctx)
	s.mu.Lock()
	superseded := s.gen != gen
	stale := superseded && err == nil && s.state == StateDisconnected
	dropped, early := !superseded && s.dropped, !superseded && s.early
	if !superseded {
		s.dialing, s.dropped, s.early = false, false, false
	}
	switch {
	case superseded:
	case err == nil && early && !dropped:
		s.setLocked(StateConnected, "")
		s.attempt = 0
	case err == nil && !dropped:
		s.awaiting = true
	default:
		if err != nil {
			s.logger.Warn("connecting to WhatsApp failed", slog.String("event", "connect_failed"), slog.Int("attempt", attempt+1), slog.String("error_type", fmt.Sprintf("%T", err)))
		}
		s.next, s.attempt = connect, attempt+1
		s.wake()
	}
	s.mu.Unlock()
	if stale {
		s.client.Disconnect()
	}
}

func (s *supervisor) handle(ev Event) {
	var paired bool
	if _, ok := ev.(Disconnected); ok {
		paired = s.client.Paired()
	}
	var logout, disconnect bool
	s.mu.Lock()
	if s.reason == ReasonShutdown {
		s.mu.Unlock()
		return
	}
	switch e := ev.(type) {
	case Connected:
		switch {
		case s.state != StateConnecting:
		case s.awaiting:
			s.setLocked(StateConnected, "")
			s.attempt = 0
		case s.dialing:
			s.early = true
		}
	case Disconnected:
		switch {
		case s.state != StateConnected && s.state != StateConnecting:
		case !paired:
			s.setLocked(StateUnpaired, "")
		case s.state == StateConnected || s.awaiting:
			attempt := s.attempt + 1
			s.setLocked(StateConnecting, "")
			s.next, s.attempt = connect, attempt
			s.wake()
		case s.dialing:
			s.dropped = true
		}
	case ClientOutdated:
		if s.trying() {
			s.setLocked(StateConnecting, "")
			s.next, s.outdated = refresh, true
			s.wake()
		}
	case LoggedOut:
		disconnect = s.permanentLocked(ReasonLoggedOut, true)
	case StreamReplaced:
		disconnect = s.permanentLocked(ReasonReplaced, false)
	case TemporaryBan:
		disconnect = s.permanentLocked(ReasonTemporaryBan, false)
	case CATRefreshFailed:
		disconnect = s.permanentLocked(ReasonCATRefresh, false)
	case ConnectFailure:
		disconnect = s.permanentLocked(ReasonConnectFailure, false)
	case PairRejected:
		s.alerts.Warn("pairing was rejected: the account is not the owner's", slog.String("event", "pair_rejected"), slog.String("stage", "before_save"))
		s.setLocked(StateUnpaired, "")
	case Paired:
		if s.isOwner(e.JID) {
			s.setLocked(StateConnecting, "")
			s.awaiting = true
			break
		}
		s.alerts.Warn("pairing was rejected: the account is not the owner's", slog.String("event", "pair_rejected"), slog.String("stage", "after_pairing"))
		s.setLocked(StateUnpaired, "")
		logout = true
	}
	ctx := s.ctx
	s.mu.Unlock()
	if disconnect {
		s.client.Disconnect()
	}
	if logout {
		safego.Go("engine.logout", func() { s.logout(ctx) })
	}
	s.updateGauges()
}

func (s *supervisor) trying() bool {
	return s.state == StateConnecting || s.state == StateConnected
}

func (s *supervisor) permanentLocked(reason Reason, always bool) bool {
	if !always && !s.trying() {
		return false
	}
	s.setLocked(StateDisconnected, reason)
	return true
}

func (s *supervisor) isOwner(jid string) bool {
	c, ok := policy.Normalize(jid)
	if !ok || c.Kind() != policy.PhoneChat || s.owner == "" {
		return false
	}
	return c.JID() == s.owner+"@s.whatsapp.net"
}

func (s *supervisor) logout(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logoutTimeout)
	defer cancel()
	if err := s.client.Logout(ctx); err != nil {
		s.logger.Warn("logging out the rejected device failed", slog.String("event", "logout_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
	}
	s.updateGauges()
}

func (s *supervisor) pair(ctx context.Context) (string, error) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.status().Reason == ReasonShutdown {
		return "", ErrStopped
	}
	if s.client.Paired() {
		return "", ErrAlreadyPaired
	}
	if s.owner == "" {
		return "", ErrOwnerPhoneMissing
	}
	now := s.clock.Now()
	s.mu.Lock()
	if s.reason == ReasonShutdown {
		s.mu.Unlock()
		return "", ErrStopped
	}
	recent := s.pairs[:0]
	for _, t := range s.pairs {
		if now.Sub(t) < pairWindow {
			recent = append(recent, t)
		}
	}
	s.pairs = recent
	if len(s.pairs) >= pairAttempts {
		s.mu.Unlock()
		return "", ErrPairRateLimited
	}
	s.pairs = append(s.pairs, now)
	dial := s.state != StateConnecting && s.state != StateConnected
	if dial {
		s.setLocked(StateConnecting, "")
	}
	gen := s.gen
	s.mu.Unlock()
	s.logger.Info("pairing started", slog.String("event", "pairing_started"))
	if dial {
		if err := s.client.Connect(ctx); err != nil {
			s.mu.Lock()
			if s.gen == gen {
				s.setLocked(StateUnpaired, "")
			}
			s.mu.Unlock()
			s.logger.Warn("pairing failed", slog.String("event", "pair_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
			return "", ErrPairFailed
		}
	}
	code, err := s.client.PairPhone(ctx, s.owner)
	if err != nil {
		s.logger.Warn("pairing failed", slog.String("event", "pair_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
		return "", ErrPairFailed
	}
	return code, nil
}

func (s *supervisor) reconnect() error {
	paired := s.client.Paired()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.reason == ReasonShutdown:
		return ErrStopped
	case s.state == StateDisconnected && s.reason == ReasonOutdated:
		s.setLocked(StateConnecting, "")
		s.next, s.outdated = refresh, true
	case !paired:
		return ErrNotPaired
	case s.state == StateConnected:
		return ErrAlreadyConnected
	default:
		s.setLocked(StateConnecting, "")
		s.next, s.attempt = connect, 0
	}
	s.wake()
	return nil
}
