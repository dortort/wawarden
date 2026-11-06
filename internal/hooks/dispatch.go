package hooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/notify"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	pageSize       = 50
	probeEvery     = time.Second
	reconcileEvery = 10 * time.Second
	idleEvery      = 30 * time.Second
	dormantEvery   = time.Minute
	failedReadWait = 5 * time.Second
	firstRetry     = time.Second
	maxRetryWait   = 5 * time.Minute
	giveUpAfter    = 24 * time.Hour
	maxReplyBytes  = 4096
	timeLayout     = "2006-01-02T15:04:05.000Z07:00"

	headerWebhook   = "WaWarden-Webhook-Id"
	headerEventID   = "WaWarden-Event-Id"
	headerTimestamp = "WaWarden-Timestamp"
	headerSignature = "WaWarden-Signature"

	outcomeDelivered = "delivered"
	outcomeDropped   = "dropped"

	ReasonDestination = "destination_refused"
	ReasonStatus      = "http_status"
	ReasonNetwork     = "network_error"
	ReasonPayload     = "payload_invalid"
	ReasonExpired     = "retries_exhausted"
	ReasonRevoked     = "client_gone"
)

var errOptions = errors.New("hooks: the dispatcher needs a store, a client lookup, a feed, a change probe, a metrics registry and a logger")

// Hook is one stored webhook with its delivery position.
type Hook struct {
	ID     string
	Client string
	Spec   Spec
	Secret []byte
	Change int64
	High   int64
}

// Progress is what a worker stores after it handled messages: the new position, and the outcome counts to add.
type Progress struct {
	Change       int64
	High         int64
	Delivered    int64
	Failed       int64
	LastDelivery time.Time
	LastFailure  time.Time
	Failure      string
}

// Item is one message of a client's change feed.
type Item struct {
	Seq     int64
	Change  int64
	Live    bool
	Message dto.Message
}

// Page is a page of a client's change feed, read under the client's own read grant.
type Page struct {
	Items []Item
	Next  int64
	More  bool
}

type Store interface {
	Hooks(ctx context.Context) ([]Hook, error)
	Advance(ctx context.Context, id string, p Progress) error
}

type Clients interface {
	Lookup(ctx context.Context, id string) (*policy.Client, bool, error)
}

// Feed reads a client's changes; ok is false when the client may not read or hold webhooks now.
type Feed interface {
	Next(ctx context.Context, c *policy.Client, after int64, limit int) (page Page, ok bool, err error)
}

type Options struct {
	Store        Store
	Clients      Clients
	Feed         Feed
	Top          func(context.Context) (int64, error)
	Metrics      *metrics.Registry
	Logger       *slog.Logger
	AllowPrivate bool
	Now          func() time.Time
	HTTP         *http.Client
}

type Dispatcher struct {
	o          Options
	http       *http.Client
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	deliveries *metrics.CounterVec
	failures   *metrics.CounterVec

	kick    chan struct{}
	mu      sync.Mutex
	top     int64
	workers map[string]*worker
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	done    chan struct{}
}

type worker struct {
	hook   Hook
	wake   chan struct{}
	cancel context.CancelFunc
}

func New(o Options) (*Dispatcher, error) {
	if o.Store == nil || o.Clients == nil || o.Feed == nil || o.Top == nil || o.Metrics == nil || o.Logger == nil {
		return nil, errOptions
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	client := o.HTTP
	if client == nil {
		client = notify.NewClient(o.AllowPrivate)
	}
	d := &Dispatcher{
		o: o, http: client, now: now, sleep: sleep,
		deliveries: o.Metrics.CounterVec("wawarden_webhook_deliveries_total", "Client webhook events, by outcome: delivered, or dropped after the last attempt.", "outcome"),
		failures:   o.Metrics.CounterVec("wawarden_webhook_attempt_failures_total", "Client webhook attempts that failed, by reason.", "reason"),
		kick:       make(chan struct{}, 1),
		workers:    map[string]*worker{},
	}
	for _, outcome := range []string{outcomeDelivered, outcomeDropped} {
		d.deliveries.With(outcome)
	}
	for _, reason := range []string{ReasonDestination, ReasonStatus, ReasonNetwork, ReasonPayload} {
		d.failures.With(reason)
	}
	return d, nil
}

// Changed asks the dispatcher to reload the webhooks now, after one was created or deleted.
func (d *Dispatcher) Changed() {
	if d == nil {
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) Start(ctx context.Context) {
	if d == nil {
		return
	}
	ctx, d.cancel = context.WithCancel(ctx)
	d.done = make(chan struct{})
	safego.Go("hooks.dispatch", func() {
		defer close(d.done)
		d.run(ctx)
	})
}

func (d *Dispatcher) Stop(ctx context.Context) error {
	if d == nil || d.cancel == nil {
		return nil
	}
	d.cancel()
	stopped := make(chan struct{})
	safego.Go("hooks.stop", func() {
		<-d.done
		d.wg.Wait()
		close(stopped)
	})
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) run(ctx context.Context) {
	probe := time.NewTicker(probeEvery)
	defer probe.Stop()
	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()
	d.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			for id, w := range d.workers {
				w.cancel()
				delete(d.workers, id)
			}
			d.mu.Unlock()
			return
		case <-d.kick:
			d.reconcile(ctx)
		case <-reconcile.C:
			d.reconcile(ctx)
		case <-probe.C:
			d.probe(ctx)
		}
	}
}

func (d *Dispatcher) probe(ctx context.Context) {
	top, err := d.o.Top(ctx)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if top == d.top {
		return
	}
	d.top = top
	for _, w := range d.workers {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (d *Dispatcher) latest() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.top
}

func (d *Dispatcher) reconcile(ctx context.Context) {
	hooks, err := d.o.Store.Hooks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.o.Logger.Warn("the client webhooks cannot be read", slog.String("event", "webhooks_unread"), slog.String("error_type", errorType(err)))
		}
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	seen := map[string]bool{}
	for _, h := range hooks {
		seen[h.ID] = true
		if _, running := d.workers[h.ID]; running {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := &worker{hook: h, wake: make(chan struct{}, 1), cancel: cancel}
		d.workers[h.ID] = w
		d.wg.Add(1)
		safego.Go("hooks.worker", func() {
			defer d.wg.Done()
			d.work(wctx, w)
		})
	}
	for id, w := range d.workers {
		if !seen[id] {
			w.cancel()
			delete(d.workers, id)
		}
	}
}

func (d *Dispatcher) work(ctx context.Context, w *worker) {
	h := w.hook
	for ctx.Err() == nil {
		wait := d.pass(ctx, &h)
		if wait == 0 {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// pass reads and delivers the hook's pending messages and returns how long to wait before the next pass, 0 for at once.
func (d *Dispatcher) pass(ctx context.Context, h *Hook) time.Duration {
	if top := d.latest(); top != 0 && top <= h.Change {
		return idleEvery
	}
	c, found, err := d.o.Clients.Lookup(ctx, h.Client)
	if err != nil {
		return failedReadWait
	}
	if !found {
		return dormantEvery
	}
	page, ok, err := d.o.Feed.Next(ctx, c, h.Change, pageSize)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			d.o.Logger.Warn("a client webhook could not read its client's changes", slog.String("event", "webhook_read_failed"),
				slog.String("webhook", h.ID), slog.String("error_type", errorType(err)))
		}
		return failedReadWait
	case !ok:
		return dormantEvery
	}
	var p Progress
	dirty := false
	for _, item := range page.Items {
		if item.Seq > h.High && item.Live && h.Spec.Filter.Match(item.Message) {
			if dirty {
				if !d.save(ctx, h, &p) {
					return failedReadWait
				}
				dirty = false
			}
			reason, delivered := d.deliver(ctx, h, item, func() bool { return d.holds(ctx, h.Client) })
			if ctx.Err() != nil {
				return 0
			}
			if delivered {
				p.Delivered, p.LastDelivery = 1, d.now()
				d.deliveries.With(outcomeDelivered).Inc()
			} else {
				p.Failed, p.LastFailure, p.Failure = 1, d.now(), reason
				d.deliveries.With(outcomeDropped).Inc()
				d.o.Logger.Warn("a client webhook did not accept an event, which is dropped", slog.String("event", "webhook_dropped"),
					slog.String("webhook", h.ID), slog.String("client", h.Client), slog.String("reason", reason))
			}
			h.Change, h.High = item.Change, max(h.High, item.Seq)
			if !d.save(ctx, h, &p) {
				return failedReadWait
			}
			continue
		}
		h.Change, h.High, dirty = item.Change, max(h.High, item.Seq), true
	}
	if page.Next > h.Change {
		h.Change, dirty = page.Next, true
	}
	if dirty && !d.save(ctx, h, &p) {
		return failedReadWait
	}
	if page.More {
		return 0
	}
	return idleEvery
}

func (d *Dispatcher) save(ctx context.Context, h *Hook, p *Progress) bool {
	p.Change, p.High = h.Change, h.High
	if err := d.o.Store.Advance(context.WithoutCancel(ctx), h.ID, *p); err != nil {
		d.o.Logger.Warn("a client webhook's position cannot be stored", slog.String("event", "webhook_unsaved"),
			slog.String("webhook", h.ID), slog.String("error_type", errorType(err)))
		return false
	}
	*p = Progress{}
	return true
}

// EventID names one message's delivery to one webhook, the same on every attempt and after a restart.
func EventID(hook string, seq int64) string {
	sum := sha256.Sum256([]byte(hook + ":" + strconv.FormatInt(seq, 10)))
	return "evt_" + hex.EncodeToString(sum[:16])
}

// holds reports whether the client still exists, is neither revoked nor expired, and may hold webhooks, before a retry posts its message again.
func (d *Dispatcher) holds(ctx context.Context, id string) bool {
	c, found, err := d.o.Clients.Lookup(ctx, id)
	return err == nil && found && c != nil && c.AllowWebhooks && !c.Revoked && d.now().Before(c.ExpiresAt)
}

func (d *Dispatcher) deliver(ctx context.Context, h *Hook, item Item, holds func() bool) (reason string, delivered bool) {
	first := d.now()
	id := EventID(h.ID, item.Seq)
	msg := item.Message
	body, err := Body(Envelope{ID: id, Event: EventMessageCreated, Time: first.UTC().Format(timeLayout), Webhook: h.ID, Message: &msg},
		h.Spec.Payload, h.Spec.ContentType)
	if err != nil {
		d.failures.With(ReasonPayload).Inc()
		return ReasonPayload, false
	}
	wait := firstRetry
	for {
		status, retryAfter, err := d.post(ctx, h, id, body)
		if err == nil && status >= 200 && status < 300 {
			return "", true
		}
		reason, retry := ReasonNetwork, true
		switch {
		case notify.DestinationRefused(err):
			reason, retry = ReasonDestination, false
		case err == nil:
			reason, retry = ReasonStatus, notify.Retryable(status)
		}
		if ctx.Err() != nil {
			return reason, false
		}
		d.failures.With(reason).Inc()
		if !retry {
			return reason, false
		}
		next := wait/2 + mathrand.N(wait/2+1) //nolint:gosec // G404: retry jitter spreads load and needs no cryptographic randomness
		if retryAfter > 0 {
			next = retryAfter
		}
		if d.now().Add(next).Sub(first) > giveUpAfter {
			return ReasonExpired, false
		}
		if d.sleep(ctx, next) != nil {
			return reason, false
		}
		if !holds() {
			return ReasonRevoked, false
		}
		wait = min(wait*2, maxRetryWait)
	}
}

func (d *Dispatcher) post(ctx context.Context, h *Hook, id string, body []byte) (int, time.Duration, error) {
	timestamp := strconv.FormatInt(d.now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, h.Spec.Method, h.Spec.URL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	for name, value := range h.Spec.Headers {
		req.Header.Set(name, value)
	}
	contentType := h.Spec.ContentType + "; charset=utf-8"
	if h.Spec.ContentType == ContentJSON {
		contentType = ContentJSON
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "wawarden")
	req.Header.Set(headerWebhook, h.ID)
	req.Header.Set(headerEventID, id)
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerSignature, notify.Sign(SigningKey(h.Secret), id, timestamp, body))
	resp, err := d.http.Do(req) //nolint:gosec // G704: the client chose the URL, and the dialer checks every destination address
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReplyBytes))
	var retryAfter time.Duration
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		retryAfter = min(time.Duration(n)*time.Second, maxRetryWait)
	}
	return resp.StatusCode, retryAfter, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func errorType(err error) string { return fmt.Sprintf("%T", err) }

// SigningKey is the secret as the client holds it, 64 lower-case hexadecimal digits, whose bytes key the signature.
func SigningKey(secret []byte) []byte { return []byte(hex.EncodeToString(secret)) }
