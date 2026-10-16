package wa

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/safego"
)

const (
	dialTimeout           = 10 * time.Second
	tlsTimeout            = 10 * time.Second
	responseHeaderTimeout = 20 * time.Second
	idleTimeout           = 90 * time.Second
	versionTimeout        = 20 * time.Second
	maxVersionPage        = 8 << 20
	maxRedirects          = 3
)

const (
	mediaJobRequests  = 6
	mediaStall        = 60 * time.Second
	mediaRAMDeadline  = 20 * time.Second
	mediaDiskBase     = 30 * time.Second
	mediaDiskRate     = 1 << 20
	mediaDeadlineCap  = 40 * time.Minute
	mediaMaxDeclared  = 2 << 30
	mediaWatchdogName = "wa.media.watchdog"
)

var (
	errOffline          = errors.New("wa: a test binary opens no network connection")
	errResponseTooLarge = errors.New("wa: the response is larger than its cap")
	errRedirect         = errors.New("wa: a redirect to another host or scheme is refused")
)

var (
	errRequestBound = &mediaTimeoutError{msg: "wa: the media request outlived its bound", deadline: true}
	errMediaStall   = &mediaTimeoutError{msg: "wa: the media request received nothing for too long"}
)

var (
	errMediaOversize = &mediaCanceledError{"wa: the media response is longer than the declared length allows"}
	errMediaEgress   = &mediaCanceledError{"wa: the media job has received its byte budget"}
	errMediaRequests = &mediaCanceledError{"wa: the media job has made its last request"}
	errMediaComplete = &mediaCanceledError{"wa: the media job has already received a complete body"}
	errMediaDeadline = &mediaCanceledError{"wa: the media job passed its deadline"}
	errMediaWatchdog = &mediaCanceledError{"wa: the media job's watchdog failed"}
)

type mediaTimeoutError struct {
	msg      string
	deadline bool
}

func (e *mediaTimeoutError) Error() string   { return e.msg }
func (e *mediaTimeoutError) Timeout() bool   { return true }
func (e *mediaTimeoutError) Temporary() bool { return true }
func (e *mediaTimeoutError) Is(target error) bool {
	return e.deadline && target == context.DeadlineExceeded
}

type mediaCanceledError struct{ msg string }

func (e *mediaCanceledError) Error() string        { return e.msg }
func (e *mediaCanceledError) Is(target error) bool { return target == context.Canceled }

type mediaThrottledError struct{ RetryAfter time.Duration }

func (e *mediaThrottledError) Error() string        { return "wa: the media server asked for fewer requests" }
func (e *mediaThrottledError) Is(target error) bool { return target == context.Canceled }

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func guarded(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if testing.Testing() {
			return nil, errOffline
		}
		return dial(ctx, network, addr)
	}
}

func systemDial() dialFunc {
	return guarded((&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext)
}

func newTransport(dial dialFunc) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           guarded(dial),
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleTimeout,
		MaxIdleConns:          4,
	}
}

type capped struct {
	next http.RoundTripper
	max  int64
}

func (c capped) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > c.max {
		_ = resp.Body.Close()
		return nil, errResponseTooLarge
	}
	resp.Body = &cappedBody{body: resp.Body, left: c.max}
	return resp, nil
}

type cappedBody struct {
	body io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.body.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.body.Close() }

func sameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
		return errRedirect
	}
	return nil
}

type mediaTransport struct {
	next  http.RoundTripper
	max   int64
	bound time.Duration
}

func (t mediaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if job, ok := req.Context().Value(mediaJobKey{}).(*mediaJob); ok {
		return job.roundTrip(t.next, req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), t.bound)
	resp, err := capped{next: t.next, max: t.max}.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, bounded(ctx, err)
	}
	resp.Body = &boundBody{ctx: ctx, cancel: cancel, body: resp.Body}
	return resp, nil
}

func bounded(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errRequestBound
	}
	return err
}

type boundBody struct {
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
}

func (b *boundBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && err != io.EOF {
		err = bounded(b.ctx, err)
	}
	return n, err
}

func (b *boundBody) Close() error {
	err := b.body.Close()
	b.cancel()
	return err
}

type mediaJobKey struct{}

type mediaTimer interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

type systemTimer struct{ t *time.Timer }

func newSystemTimer(d time.Duration) mediaTimer { return systemTimer{t: time.NewTimer(d)} }

func (s systemTimer) C() <-chan time.Time   { return s.t.C }
func (s systemTimer) Reset(d time.Duration) { s.t.Reset(d) }
func (s systemTimer) Stop()                 { s.t.Stop() }

func mediaDiskDeadline(declared int64) time.Duration {
	if declared >= int64((mediaDeadlineCap-mediaDiskBase)/time.Second)*mediaDiskRate {
		return mediaDeadlineCap
	}
	return mediaDiskBase + time.Duration(max(declared, 0))*time.Second/mediaDiskRate
}

type mediaJob struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	perRequest int64
	budget     int64
	stall      time.Duration
	timer      func(time.Duration) mediaTimer
	kick       chan struct{}
	done       chan struct{}

	mu       sync.Mutex
	requests int
	spent    int64
	complete bool
	active   *mediaRequest
	activity uint64
}

type mediaRequest struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func withMediaJob(parent context.Context, declared int64, deadline time.Duration) (context.Context, func()) {
	return startMediaJob(parent, declared, deadline, mediaStall, newSystemTimer)
}

func startMediaJob(parent context.Context, declared int64, deadline, stall time.Duration, timer func(time.Duration) mediaTimer) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	size := min(max(declared, 0), mediaMaxDeclared) + mediaOverhead
	j := &mediaJob{
		ctx:        ctx,
		cancel:     cancel,
		perRequest: size,
		budget:     2 * size,
		stall:      stall,
		timer:      timer,
		kick:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if declared < 0 || declared > mediaMaxDeclared {
		cancel(errMediaOversize)
	}
	safego.Go(mediaWatchdogName, j.watch(deadline))
	return context.WithValue(ctx, mediaJobKey{}, j), func() {
		cancel(nil)
		<-j.done
	}
}

func (j *mediaJob) watch(deadline time.Duration) func() {
	return func() {
		defer close(j.done)
		defer j.cancel(errMediaWatchdog)
		limit := j.timer(deadline)
		defer limit.Stop()
		stall := j.timer(j.stall)
		defer stall.Stop()
		var armed uint64
		for {
			select {
			case <-j.ctx.Done():
				return
			case <-limit.C():
				j.cancel(errMediaDeadline)
				return
			case <-j.kick:
				armed = j.arm(stall)
			case <-stall.C():
				armed = j.stalled(stall, armed)
			}
		}
	}
}

func (j *mediaJob) arm(stall mediaTimer) uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.armLocked(stall)
}

func (j *mediaJob) armLocked(stall mediaTimer) uint64 {
	if j.active == nil {
		stall.Stop()
	} else {
		stall.Reset(j.stall)
	}
	return j.activity
}

func (j *mediaJob) stalled(stall mediaTimer, armed uint64) uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.activity == armed && j.active != nil {
		j.active.cancel(errMediaStall)
		return armed
	}
	return j.armLocked(stall)
}

func (j *mediaJob) nudge() {
	select {
	case j.kick <- struct{}{}:
	default:
	}
}

func (j *mediaJob) end(cause error) error {
	j.cancel(cause)
	return context.Cause(j.ctx)
}

func (j *mediaJob) begin(parent context.Context) (*mediaRequest, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.ctx.Err() != nil:
		return nil, context.Cause(j.ctx)
	case j.complete:
		return nil, j.end(errMediaComplete)
	case j.requests >= mediaJobRequests:
		return nil, j.end(errMediaRequests)
	case j.spent >= j.budget:
		return nil, j.end(errMediaEgress)
	}
	j.requests++
	ctx, cancel := context.WithCancelCause(parent)
	r := &mediaRequest{ctx: ctx, cancel: cancel}
	j.active = r
	j.activity++
	j.nudge()
	return r, nil
}

func (j *mediaJob) finish(r *mediaRequest, complete bool) {
	j.mu.Lock()
	j.complete = j.complete || complete
	if j.active == r {
		j.active = nil
		j.activity++
	}
	j.mu.Unlock()
	r.cancel(nil)
	j.nudge()
}

func (j *mediaJob) translate(r *mediaRequest, err error) error {
	switch {
	case j.ctx.Err() != nil:
		return context.Cause(j.ctx)
	case errors.Is(context.Cause(r.ctx), errMediaStall):
		return errMediaStall
	}
	return err
}

func (j *mediaJob) reserve(want int64) int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := max(min(want, j.budget-j.spent), 0)
	j.spent += n
	return n
}

func (j *mediaJob) settle(reserved int64, n int) {
	j.mu.Lock()
	j.spent -= reserved - int64(n)
	if n > 0 {
		j.activity++
	}
	j.mu.Unlock()
	j.nudge()
}

func (j *mediaJob) screen(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		return &mediaThrottledError{RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.ContentLength > j.perRequest:
		return errMediaOversize
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if resp.ContentLength > j.budget-j.spent {
		return errMediaEgress
	}
	return nil
}

func (j *mediaJob) roundTrip(next http.RoundTripper, req *http.Request) (*http.Response, error) {
	r, err := j.begin(req.Context())
	if err != nil {
		return nil, err
	}
	resp, err := next.RoundTrip(req.WithContext(r.ctx))
	if err != nil {
		err = j.translate(r, err)
		j.finish(r, false)
		return nil, err
	}
	if cause := j.screen(resp); cause != nil {
		_ = resp.Body.Close()
		j.finish(r, false)
		return nil, j.end(cause)
	}
	resp.Body = &mediaJobBody{job: j, req: r, body: resp.Body, left: j.perRequest}
	return resp, nil
}

type mediaJobBody struct {
	job  *mediaJob
	req  *mediaRequest
	body io.ReadCloser
	left int64
}

func (b *mediaJobBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	allowed := b.job.reserve(min(int64(len(p)), b.left))
	if allowed == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		switch {
		case n > 0 && b.left <= 0:
			err = b.job.end(errMediaOversize)
		case n > 0:
			err = b.job.end(errMediaEgress)
		default:
			return 0, b.result(err)
		}
		b.job.finish(b.req, false)
		return 0, err
	}
	n, err := b.body.Read(p[:allowed])
	b.job.settle(allowed, n)
	b.left -= int64(n)
	return n, b.result(err)
}

func (b *mediaJobBody) result(err error) error {
	switch err {
	case nil:
		return nil
	case io.EOF:
		b.job.finish(b.req, true)
		return io.EOF
	}
	err = b.job.translate(b.req, err)
	b.job.finish(b.req, false)
	return err
}

func (b *mediaJobBody) Close() error {
	err := b.body.Close()
	b.job.finish(b.req, false)
	return err
}

func retryAfter(v string) time.Duration {
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(min(max(secs, 0), math.MaxInt64/int64(time.Second))) * time.Second
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	return max(time.Until(at), 0)
}
