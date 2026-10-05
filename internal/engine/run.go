package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/safego"
)

type Engine struct {
	sup      *supervisor
	pipe     *pipeline
	hist     *historian
	cancel   context.CancelFunc
	unhook   func()
	done     chan struct{}
	started  bool
	stopping sync.Once
}

var (
	errOptions  = errors.New("engine: a client, a version source, a history decoder, an archive, a data directory, loggers and a metrics registry are required, and the history cap must be from 1 byte to 256 MiB")
	errPanicked = errors.New("engine: the call panicked")
)

func guarded(name string, fn func() error) (err error) {
	err = errPanicked
	defer safego.Recover(name)
	return fn()
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func randomJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d) //nolint:gosec // G404: reconnect jitter spreads retries and needs no cryptographic randomness
}

func New(opts Options) (*Engine, error) {
	if opts.Client == nil || opts.Versions == nil || opts.Decoder == nil || opts.Archive == nil || opts.DataDir == "" ||
		opts.Logger == nil || opts.Alerts == nil || opts.Metrics == nil || opts.HistoryMaxBytes <= 0 || opts.HistoryMaxBytes > MaxHistoryMaxBytes {
		return nil, errOptions
	}
	if opts.Clock == nil {
		opts.Clock = systemClock{}
	}
	if opts.Jitter == nil {
		opts.Jitter = randomJitter
	}
	sup := newSupervisor(opts)
	pipe := newPipeline(opts)
	hist := newHistorian(opts, pipe, func() bool { return sup.status().State == StateConnected })
	return &Engine{sup: sup, pipe: pipe, hist: hist, done: make(chan struct{})}, nil
}

func (e *Engine) Start(ctx context.Context, recentStarts int) {
	ctx, e.cancel = context.WithCancel(ctx)
	e.pipe.ctx = ctx
	e.sup.ctx = ctx
	e.unhook = e.sup.client.OnEvent(e.dispatch)
	e.started = true
	e.pipe.checkSpace()
	e.pipe.nextSpace = e.pipe.clock.Now().Add(spaceInterval)
	e.sup.start(ctx, recentStarts)
	safego.Go("engine.ingest", func() { e.loop(ctx) })
}

func (e *Engine) dispatch(ev Event) bool {
	switch v := ev.(type) {
	case Message, Group, HistoryNotification:
		if !e.sup.accepting() {
			e.pipe.counts.dropped.With(dropNotPaired).Inc()
			return true
		}
		if n, ok := v.(HistoryNotification); ok {
			return e.hist.accept(n)
		}
		return e.pipe.accept(v)
	case Connected:
		e.sup.handle(v)
		e.pipe.wake()
		return true
	}
	e.sup.handle(ev)
	return true
}

func (e *Engine) loop(ctx context.Context) {
	defer close(e.done)
	e.hist.clean(ctx)
	clock := e.pipe.clock
	for ctx.Err() == nil {
		e.pipe.drainInbox(ctx)
		e.hist.drain(ctx)
		next := e.pipe.nextSweep
		if e.pipe.nextSpace.Before(next) {
			next = e.pipe.nextSpace
		}
		select {
		case <-ctx.Done():
		case <-e.pipe.kick:
		case <-clock.After(max(next.Sub(clock.Now()), time.Millisecond)):
		}
	}
}

func (e *Engine) Stop(ctx context.Context) error {
	if !e.started {
		return nil
	}
	e.stopping.Do(func() {
		e.unhook()
		e.sup.stop()
		e.cancel()
	})
	for _, done := range []chan struct{}{e.sup.done, e.done} {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("engine: the workers did not stop in time: %w", ctx.Err())
		}
	}
	return nil
}

func (e *Engine) Status() Status { return e.sup.status() }

func (e *Engine) Pair(ctx context.Context) (string, error) { return e.sup.pair(ctx) }

func (e *Engine) Reconnect() error { return e.sup.reconnect() }
