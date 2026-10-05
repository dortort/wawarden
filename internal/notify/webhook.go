package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
)

const (
	headerEventID   = "WaWarden-Event-Id"
	headerTimestamp = "WaWarden-Timestamp"
	headerSignature = "WaWarden-Signature"

	MinSecretBytes = 32

	queueSize      = 64
	maxAttempts    = 5
	maxRetryAfter  = time.Minute
	maxBackoffStep = 6
	maxReplyBytes  = 4096
	dialTimeout    = 5 * time.Second
	headerTimeout  = 10 * time.Second
	requestTimeout = 15 * time.Second
	idleTimeout    = 30 * time.Second

	dropQueueFull = "queue_full"
	dropFailed    = "failed"
	dropShutdown  = "shutdown"

	failDestination = "destination_refused"
	failStatus      = "http_status"
	failNetwork     = "network_error"
)

var (
	errURL          = errors.New("notify: the webhook URL must be https://host[:port][/path][?query], without credentials or a fragment")
	errSecret       = errors.New("notify: the webhook needs a signing secret of at least 32 bytes and a metrics registry")
	errDestination  = errors.New("notify: the webhook's destination address is refused")
	errShutdownDrop = errors.New("notify: the webhook dropped events at shutdown")
	alwaysRefused   = prefixes("0.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4", "100.100.100.200/32", "192.0.0.192/32", "::/128", "fe80::/10", "ff00::/8", "fd00:ec2::254/128", "64:ff9b:1::/48")
	privateUnlessOK = prefixes("100.64.0.0/10")
	nat64           = netip.MustParsePrefix("64:ff9b::/96")
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || strings.Contains(u.Hostname(), "%") ||
		u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errURL
	}
	return nil
}

type destination struct{ allowPrivate bool }

func (d destination) check(a netip.Addr) error {
	if a.Zone() != "" {
		return errDestination
	}
	a = a.Unmap()
	if nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	for _, p := range alwaysRefused {
		if p.Contains(a) {
			return errDestination
		}
	}
	if d.allowPrivate {
		return nil
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return errDestination
	}
	for _, p := range privateUnlessOK {
		if p.Contains(a) {
			return errDestination
		}
	}
	return nil
}

func (d destination) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errDestination
	}
	return d.check(ap.Addr())
}

func newDialer(allowPrivate bool) *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, Control: destination{allowPrivate: allowPrivate}.control}
}

func newHTTPClient(dialer *net.Dialer) *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
			MaxIdleConns:          1,
			IdleConnTimeout:       idleTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type delivery struct {
	id   string
	body []byte
}

type webhook struct {
	url     string
	secret  []byte
	client  *http.Client
	logger  *slog.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	dropped *metrics.CounterVec

	queue         chan delivery
	stopping      atomic.Bool
	shutdownDrops atomic.Int64
	stop          chan struct{}
	done          chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
}

func newWebhook(o Options, logger *slog.Logger, now func() time.Time) (*webhook, error) {
	if err := CheckURL(o.URL); err != nil {
		return nil, err
	}
	if len(o.Secret) < MinSecretBytes || o.Metrics == nil {
		return nil, errSecret
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &webhook{
		url:     o.URL,
		secret:  bytes.Clone(o.Secret),
		client:  newHTTPClient(newDialer(o.AllowPrivate)),
		logger:  logger,
		now:     now,
		sleep:   sleep,
		dropped: o.Metrics.CounterVec("wawarden_notify_dropped_total", "Notification events the webhook did not deliver, by reason.", "reason"),
		queue:   make(chan delivery, queueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}, nil
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

func eventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "evt_" + hex.EncodeToString(b[:])
}

func eventBody(id, event string, at time.Time, fields []slog.Attr) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	writeString(&b, id)
	b.WriteString(`,"event":`)
	writeString(&b, event)
	b.WriteString(`,"time":`)
	writeString(&b, at.UTC().Format(time.RFC3339Nano))
	for _, f := range fields {
		b.WriteByte(',')
		writeString(&b, f.Key)
		b.WriteByte(':')
		switch f.Value.Kind() {
		case slog.KindInt64:
			b.WriteString(strconv.FormatInt(f.Value.Int64(), 10))
		case slog.KindUint64:
			b.WriteString(strconv.FormatUint(f.Value.Uint64(), 10))
		case slog.KindString:
			writeString(&b, f.Value.String())
		default:
			b.WriteString("null")
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

func writeString(b *bytes.Buffer, s string) {
	quoted, _ := json.Marshal(s)
	b.Write(quoted)
}

func (h *webhook) enqueue(d delivery) {
	if h.stopping.Load() {
		h.dropAtShutdown()
		return
	}
	select {
	case h.queue <- d:
	default:
		h.dropped.With(dropQueueFull).Inc()
	}
}

func (h *webhook) run() {
	defer close(h.done)
	for {
		select {
		case d := <-h.queue:
			h.deliver(d)
		case <-h.stop:
			for {
				select {
				case d := <-h.queue:
					h.deliver(d)
				default:
					return
				}
			}
		}
	}
}

func (h *webhook) dropAtShutdown() {
	h.shutdownDrops.Add(1)
	h.dropped.With(dropShutdown).Inc()
}

func (h *webhook) shutdown(ctx context.Context) error {
	if h.stopping.Swap(true) {
		return nil
	}
	close(h.stop)
	select {
	case <-h.done:
	case <-ctx.Done():
		h.cancel()
		<-h.done
	}
	h.cancel()
	for len(h.queue) > 0 {
		<-h.queue
		h.dropAtShutdown()
	}
	n := h.shutdownDrops.Load()
	if n == 0 {
		return nil
	}
	h.logger.Warn("the notification webhook dropped events at shutdown", slog.String("event", "notify_dropped"), slog.Int64("count", n))
	return errors.Join(errShutdownDrop, ctx.Err())
}

func (h *webhook) deliver(d delivery) {
	for attempt := 1; ; attempt++ {
		if h.ctx.Err() != nil {
			h.dropAtShutdown()
			return
		}
		status, retryAfter, err := h.post(d)
		if err == nil && status >= 200 && status < 300 {
			return
		}
		reason, retry := failNetwork, true
		switch {
		case errors.Is(err, errDestination):
			reason, retry = failDestination, false
		case err == nil:
			reason, retry = failStatus, retryable(status)
		}
		if h.ctx.Err() != nil {
			h.dropAtShutdown()
			return
		}
		if !retry || attempt == maxAttempts {
			h.dropped.With(dropFailed).Inc()
			h.logger.Warn("the notification webhook did not accept an event", slog.String("event", "notify_failed"),
				slog.Int("attempts", attempt), slog.String("reason", reason), slog.Int("status", status))
			return
		}
		wait := backoff(attempt)
		if retryAfter > 0 {
			wait = retryAfter
		}
		if h.sleep(h.ctx, wait) != nil {
			h.dropAtShutdown()
			return
		}
	}
}

func retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func backoff(attempt int) time.Duration {
	base := time.Second << min(attempt-1, maxBackoffStep)
	return base/2 + mathrand.N(base/2+1) //nolint:gosec // G404: retry jitter spreads load and needs no cryptographic randomness
}

func sign(secret []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *webhook) post(d delivery) (int, time.Duration, error) {
	timestamp := strconv.FormatInt(h.now().Unix(), 10)
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.url, bytes.NewReader(d.body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wawarden")
	req.Header.Set(headerEventID, d.id)
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerSignature, sign(h.secret, d.id, timestamp, d.body))
	resp, err := h.client.Do(req) //nolint:gosec // G704: the operator configures the webhook URL, and the dialer checks every destination address
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReplyBytes))
	var retryAfter time.Duration
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		retryAfter = time.Duration(min(n, int(maxRetryAfter/time.Second))) * time.Second
	}
	return resp.StatusCode, retryAfter, nil
}
