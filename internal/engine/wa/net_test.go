package wa

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
)

const mediaTestURL = "https://cdn.example/media"

var (
	errMediaCut     = errors.New("synthetic: the connection was reset")
	errMediaAborted = errors.New("synthetic: the request was abandoned")
)

type mediaReply struct {
	status     int
	retryAfter string
	body       []byte
	length     int64
	hold       bool
	then       error
	drip       time.Duration
}

type mediaOrigin struct {
	mu        sync.Mutex
	replies   []mediaReply
	ctxs      []context.Context
	sent      int64
	closed    int
	realistic bool
}

func newMediaOrigin(replies ...mediaReply) *mediaOrigin { return &mediaOrigin{replies: replies} }

func (o *mediaOrigin) RoundTrip(req *http.Request) (*http.Response, error) {
	o.mu.Lock()
	o.ctxs = append(o.ctxs, req.Context())
	reply := o.replies[min(len(o.ctxs), len(o.replies))-1]
	o.mu.Unlock()
	ctx := req.Context()
	if reply.hold {
		<-ctx.Done()
		return nil, o.aborted(ctx)
	}
	resp := &http.Response{
		StatusCode:    cmpOr(reply.status, http.StatusOK),
		Header:        http.Header{},
		ContentLength: cmpOr(reply.length, -1),
		Body:          &mediaFakeBody{origin: o, ctx: ctx, data: reply.body, then: reply.then, drip: reply.drip},
		Request:       req,
	}
	if reply.retryAfter != "" {
		resp.Header.Set("Retry-After", reply.retryAfter)
	}
	return resp, nil
}

func cmpOr[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

func (o *mediaOrigin) aborted(ctx context.Context) error {
	if o.realistic {
		return context.Cause(ctx)
	}
	return errMediaAborted
}

func (o *mediaOrigin) served() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.ctxs)
}

func (o *mediaOrigin) ctx(i int) context.Context {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ctxs[i]
}

type mediaFakeBody struct {
	origin *mediaOrigin
	ctx    context.Context
	data   []byte
	then   error
	drip   time.Duration
}

func (b *mediaFakeBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		if b.then != nil {
			return 0, b.then
		}
		<-b.ctx.Done()
		return 0, b.origin.aborted(b.ctx)
	}
	if b.drip > 0 {
		t := time.NewTimer(b.drip)
		defer t.Stop()
		select {
		case <-b.ctx.Done():
			return 0, b.origin.aborted(b.ctx)
		case <-t.C:
		}
		p = p[:1]
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	b.origin.mu.Lock()
	b.origin.sent += int64(n)
	b.origin.mu.Unlock()
	return n, nil
}

func (b *mediaFakeBody) Close() error {
	b.origin.mu.Lock()
	b.origin.closed++
	b.origin.mu.Unlock()
	return nil
}

func mediaHTTPClient(rt http.RoundTripper, bound time.Duration) *http.Client {
	return &http.Client{Transport: mediaTransport{next: rt, max: 1 << 10, bound: bound}, Timeout: mediaBackstop, CheckRedirect: refuseRedirects}
}

func mediaFetch(ctx context.Context, c *http.Client) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaTestURL, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, nil
	}
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func checkRetryableTimeout(t *testing.T, what string, err error, deadline bool) {
	t.Helper()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("%s: %v is not a network timeout, so the protocol library would not retry it", what, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("%s: %v counts as a cancel, so the protocol library would stop instead of retrying", what, err)
	}
	if errors.Is(err, context.DeadlineExceeded) != deadline {
		t.Errorf("%s: errors.Is(%v, context.DeadlineExceeded) = %v, want %v", what, err, !deadline, deadline)
	}
}

func checkJobEnded(t *testing.T, err error, ctx context.Context, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Errorf("the request ended with %v, want %v, which is also a cancel", err, cause)
	}
	if got := context.Cause(ctx); got != cause {
		t.Errorf("the job ended with %v, want %v", got, cause)
	}
}

func TestARequestWithoutAJobEndsAtItsBoundWithTheClientTimeoutsErrorClasses(t *testing.T) {
	for name, reply := range map[string]mediaReply{
		"awaiting headers": {hold: true},
		"reading the body": {body: []byte("synthetic")},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				realistic := func() *mediaOrigin { return &mediaOrigin{replies: []mediaReply{reply}, realistic: true} }
				for which, c := range map[string]*http.Client{
					"the client's own timeout, as before":                {Transport: capped{next: realistic(), max: 1 << 10}, Timeout: time.Second},
					"the transport's request bound":                      mediaHTTPClient(realistic(), time.Second),
					"the transport's request bound over an opaque error": mediaHTTPClient(newMediaOrigin(reply), time.Second),
				} {
					start := time.Now()
					_, _, err := mediaFetch(t.Context(), c)
					checkRetryableTimeout(t, which, err, true)
					if time.Since(start) != time.Second {
						t.Errorf("%s: the request ended after %v, want 1s", which, time.Since(start))
					}
				}
			})
		})
	}
}

func TestARequestWithoutAJobKeepsTheHistoryCapAndReleasesItsBound(t *testing.T) {
	for name, tc := range map[string]struct {
		reply mediaReply
		want  error
	}{
		"at the cap":                     {reply: mediaReply{body: bytes.Repeat([]byte("s"), 1<<10), then: io.EOF}},
		"over the cap by its length":     {reply: mediaReply{body: bytes.Repeat([]byte("s"), 1<<10+1), length: 1<<10 + 1, then: io.EOF}, want: errResponseTooLarge},
		"over the cap in its body":       {reply: mediaReply{body: bytes.Repeat([]byte("s"), 1<<10+1), then: io.EOF}, want: errResponseTooLarge},
		"cut by a synthetic reset":       {reply: mediaReply{body: []byte("synthetic"), then: errMediaCut}, want: errMediaCut},
		"answered with a missing object": {reply: mediaReply{status: http.StatusNotFound, then: io.EOF}},
	} {
		t.Run(name, func(t *testing.T) {
			o := newMediaOrigin(tc.reply)
			c := &http.Client{Transport: mediaTransport{next: o, max: 1 << 10, bound: mediaRequestBound}}
			_, _, err := mediaFetch(t.Context(), c)
			if !errors.Is(err, tc.want) {
				t.Fatalf("the request ended with %v, want %v", err, tc.want)
			}
			ctx := o.ctx(0)
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > mediaRequestBound {
				t.Errorf("the request's deadline is %v away, want at most %v", time.Until(dl), mediaRequestBound)
			}
			if ctx.Err() == nil {
				t.Error("the request's bound was not released when it ended")
			}
		})
	}
}

func TestTheJobValueSurvivesTheContextTheHTTPClientDerives(t *testing.T) {
	ctx, stop := withMediaJob(t.Context(), 8, time.Hour)
	defer stop()
	o := newMediaOrigin(mediaReply{body: []byte("synthetic"), length: 41, then: io.EOF})
	_, _, err := mediaFetch(ctx, mediaHTTPClient(o, mediaRequestBound))
	if _, ok := o.ctx(0).Deadline(); !ok {
		t.Fatal("the HTTP client derived no context, so this test proves nothing")
	}
	checkJobEnded(t, err, ctx, errMediaOversize)
}

func TestAnAttachmentResponseLongerThanItsDeclaredLengthEndsTheJob(t *testing.T) {
	const declared = 100
	for name, tc := range map[string]struct {
		reply mediaReply
		want  error
		read  int
	}{
		"by its length":            {reply: mediaReply{body: make([]byte, declared+33), length: declared + 33, then: io.EOF}, want: errMediaOversize},
		"in its body":              {reply: mediaReply{body: make([]byte, declared+33), then: io.EOF}, want: errMediaOversize, read: declared + 32},
		"at its limit":             {reply: mediaReply{body: make([]byte, declared+32), length: declared + 32, then: io.EOF}, read: declared + 32},
		"at its limit, unlabelled": {reply: mediaReply{body: make([]byte, declared+32), then: io.EOF}, read: declared + 32},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, stop := withMediaJob(t.Context(), declared, time.Hour)
			defer stop()
			o := newMediaOrigin(tc.reply)
			_, got, err := mediaFetch(ctx, mediaHTTPClient(o, mediaRequestBound))
			if len(got) != tc.read {
				t.Errorf("the caller read %d bytes, want %d", len(got), tc.read)
			}
			if tc.want == nil {
				if err != nil || ctx.Err() != nil {
					t.Fatalf("a response within the limit ended with %v and the job with %v", err, context.Cause(ctx))
				}
				return
			}
			checkJobEnded(t, err, ctx, tc.want)
			if _, _, err := mediaFetch(ctx, mediaHTTPClient(o, mediaRequestBound)); !errors.Is(err, tc.want) || o.served() != 1 {
				t.Fatalf("a request after the job ended gave %v after %d requests, want %v after 1", err, o.served(), tc.want)
			}
		})
	}
}

func TestTheEgressBudgetHoldsAcrossRetries(t *testing.T) {
	const declared = 100
	cut := mediaReply{body: make([]byte, declared), then: errMediaCut}
	full := mediaReply{body: make([]byte, declared+mediaOverhead), then: errMediaCut}
	for name, tc := range map[string]struct {
		replies []mediaReply
		read    int
		served  int
	}{
		"cut in the body":                 {replies: []mediaReply{cut, cut, cut}, read: 2 * (declared + mediaOverhead), served: 3},
		"refused by its length":           {replies: []mediaReply{cut, cut, {body: make([]byte, declared), length: declared}}, read: 2 * declared, served: 3},
		"refused before the next request": {replies: []mediaReply{full}, read: 2 * (declared + mediaOverhead), served: 2},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, stop := withMediaJob(t.Context(), declared, time.Hour)
			defer stop()
			o := newMediaOrigin(tc.replies...)
			c := mediaHTTPClient(o, mediaRequestBound)
			var read int
			var err error
			for range mediaJobRequests {
				var got []byte
				_, got, err = mediaFetch(ctx, c)
				read += len(got)
				if errors.Is(err, context.Canceled) {
					break
				}
				if !errors.Is(err, errMediaCut) {
					t.Fatalf("a retryable failure ended with %v", err)
				}
			}
			checkJobEnded(t, err, ctx, errMediaEgress)
			if read != tc.read || o.served() != tc.served {
				t.Fatalf("the job read %d bytes in %d requests, want %d in %d", read, o.served(), tc.read, tc.served)
			}
			if budget := int64(2 * (declared + mediaOverhead)); o.sent > budget+1 {
				t.Fatalf("the origin sent %d bytes, want at most the budget of %d and one probe byte", o.sent, budget)
			}
		})
	}
}

func TestASeventhAttachmentRequestIsRefused(t *testing.T) {
	ctx, stop := withMediaJob(t.Context(), 100, time.Hour)
	defer stop()
	o := newMediaOrigin(mediaReply{status: http.StatusNotFound, then: io.EOF})
	c := mediaHTTPClient(o, mediaRequestBound)
	for i := range mediaJobRequests {
		if status, _, err := mediaFetch(ctx, c); status != http.StatusNotFound || err != nil || ctx.Err() != nil {
			t.Fatalf("request %d gave %d, %v and the job %v, want the 404 passed through", i+1, status, err, context.Cause(ctx))
		}
	}
	_, _, err := mediaFetch(ctx, c)
	checkJobEnded(t, err, ctx, errMediaRequests)
	if o.served() != mediaJobRequests {
		t.Fatalf("the origin served %d requests, want %d", o.served(), mediaJobRequests)
	}
}

func TestNoAttachmentRequestFollowsACompleteBody(t *testing.T) {
	ctx, stop := withMediaJob(t.Context(), 100, time.Hour)
	defer stop()
	o := newMediaOrigin(mediaReply{body: []byte("synthetic"), then: io.EOF})
	c := mediaHTTPClient(o, mediaRequestBound)
	if _, got, err := mediaFetch(ctx, c); err != nil || string(got) != "synthetic" {
		t.Fatalf("the first request gave %q, %v", got, err)
	}
	_, _, err := mediaFetch(ctx, c)
	checkJobEnded(t, err, ctx, errMediaComplete)
	if o.served() != 1 {
		t.Fatalf("the origin served %d requests, want 1", o.served())
	}
}

func TestAStallEndsOnlyItsRequestAsARetryableTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := withMediaJob(t.Context(), 100, time.Hour)
		defer stop()
		const gap = mediaStall - 10*time.Second
		o := newMediaOrigin(
			mediaReply{body: []byte("sy"), drip: gap},
			mediaReply{hold: true},
			mediaReply{body: []byte("synt"), drip: gap, then: io.EOF},
		)
		c := mediaHTTPClient(o, mediaRequestBound)
		for _, tc := range []struct {
			what string
			want time.Duration
		}{{"a body that stalls after two slow bytes", 2*gap + mediaStall}, {"stalled headers", mediaStall}} {
			what := tc.what
			start := time.Now()
			_, _, err := mediaFetch(ctx, c)
			if !errors.Is(err, errMediaStall) || time.Since(start) != tc.want {
				t.Fatalf("%s ended with %v after %v, want the stall after %v", what, err, time.Since(start), tc.want)
			}
			checkRetryableTimeout(t, what, err, false)
			if ctx.Err() != nil {
				t.Fatalf("%s ended the job with %v", what, context.Cause(ctx))
			}
		}
		start := time.Now()
		if _, got, err := mediaFetch(ctx, c); err != nil || string(got) != "synt" || time.Since(start) != 4*gap {
			t.Fatalf("a slow body that never stalls gave %q, %v after %v", got, err, time.Since(start))
		}
	})
}

func TestAThrottlingAnswerEndsTheJobWithItsRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct {
			status     int
			retryAfter string
			want       time.Duration
		}{
			{http.StatusTooManyRequests, "120", 2 * time.Minute},
			{http.StatusServiceUnavailable, time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
			{http.StatusServiceUnavailable, time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
			{http.StatusInternalServerError, "", 0},
			{http.StatusBadGateway, "soon", 0},
			{http.StatusGatewayTimeout, "-5", 0},
			{http.StatusTooManyRequests, "99999999999999999999", time.Duration(math.MaxInt64/int64(time.Second)) * time.Second},
		} {
			ctx, stop := withMediaJob(t.Context(), 100, time.Hour)
			o := newMediaOrigin(mediaReply{status: tc.status, retryAfter: tc.retryAfter, body: []byte("synthetic"), then: io.EOF})
			c := mediaHTTPClient(o, mediaRequestBound)
			_, _, err := mediaFetch(ctx, c)
			var throttled *mediaThrottledError
			if !errors.As(err, &throttled) || throttled.RetryAfter != tc.want || !errors.Is(err, context.Canceled) {
				t.Errorf("%d with Retry-After %q gave %v (%v), want a throttled cancel with %v", tc.status, tc.retryAfter, err, throttled, tc.want)
			}
			if cause := context.Cause(ctx); !errors.As(cause, &throttled) || throttled.RetryAfter != tc.want {
				t.Errorf("%d with Retry-After %q ended the job with %v", tc.status, tc.retryAfter, cause)
			}
			if _, _, err := mediaFetch(ctx, c); !errors.As(err, &throttled) || o.served() != 1 {
				t.Errorf("%d: a request after the throttle gave %v after %d requests", tc.status, err, o.served())
			}
			stop()
		}
	})
}

func TestTheJobDeadlineEndsTheJobAsACancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := withMediaJob(t.Context(), 100, mediaRAMDeadline)
		defer stop()
		o := newMediaOrigin(mediaReply{body: make([]byte, 100), drip: 15 * time.Second, then: io.EOF})
		c := mediaHTTPClient(o, mediaRequestBound)
		start := time.Now()
		_, _, err := mediaFetch(ctx, c)
		if time.Since(start) != mediaRAMDeadline {
			t.Fatalf("the job ended after %v, want %v", time.Since(start), mediaRAMDeadline)
		}
		checkJobEnded(t, err, ctx, errMediaDeadline)
		if _, _, err := mediaFetch(ctx, c); !errors.Is(err, errMediaDeadline) || o.served() != 1 {
			t.Fatalf("a request after the deadline gave %v after %d requests", err, o.served())
		}
	})
}

func TestTheMediaWatchdogExitsWhenItsJobEnds(t *testing.T) {
	for name, end := range map[string]func(stop func(), cancel context.CancelFunc){
		"stopped":                  func(stop func(), _ context.CancelFunc) { stop() },
		"its parent was cancelled": func(_ func(), cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, stop := withMediaJob(parent, 100, time.Hour)
			end(stop, cancel)
			job := ctx.Value(mediaJobKey{}).(*mediaJob)
			<-job.done
			for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > baseline; {
				if time.Now().After(deadline) {
					t.Fatalf("%d goroutines remain after the job ended, want %d", runtime.NumGoroutine(), baseline)
				}
				runtime.Gosched()
			}
			stop()
		})
	}
}

type mediaFailingTimer struct{ c chan time.Time }

func (f mediaFailingTimer) C() <-chan time.Time { return f.c }
func (mediaFailingTimer) Reset(time.Duration)   { panic("synthetic: the stall timer failed") }
func (mediaFailingTimer) Stop()                 {}

func TestAPanicInTheMediaWatchdogIsRecoveredCountedAndEndsTheJob(t *testing.T) {
	for name, timer := range map[string]func(time.Duration) mediaTimer{
		"starting":         func(time.Duration) mediaTimer { panic("synthetic: no timer") },
		"arming the stall": func(time.Duration) mediaTimer { return mediaFailingTimer{c: make(chan time.Time)} },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logger, logs := newLogger(slog.LevelInfo)
				reg := metrics.NewRegistry()
				safego.Install(logger, reg)
				ctx, stop := startMediaJob(t.Context(), 100, time.Hour, mediaStall, timer)
				defer stop()
				o := newMediaOrigin(mediaReply{hold: true})
				_, _, err := mediaFetch(ctx, mediaHTTPClient(o, mediaRequestBound))
				checkJobEnded(t, err, ctx, errMediaWatchdog)
				synctest.Wait()
				var b strings.Builder
				if err := reg.WriteText(&b); err != nil {
					t.Fatalf("WriteText: %v", err)
				}
				if !strings.Contains(b.String(), `wawarden_panics_total{name="`+mediaWatchdogName+`"} 1`+"\n") {
					t.Fatalf("the panic was not counted:\n%s", b.String())
				}
				if got := logs.events("panic"); len(got) != 1 || got[0]["name"] != mediaWatchdogName {
					t.Fatalf("panic events %v, want one for %s", got, mediaWatchdogName)
				}
			})
		})
	}
}

func TestEveryTerminalMediaCauseIsACancelAndTellsItsReason(t *testing.T) {
	throttled := &mediaThrottledError{RetryAfter: time.Minute}
	causes := []error{errMediaOversize, errMediaEgress, errMediaRequests, errMediaComplete, errMediaDeadline, errMediaWatchdog, throttled}
	for i, c := range causes {
		var netErr net.Error
		if !errors.Is(c, context.Canceled) || errors.As(c, &netErr) {
			t.Errorf("%v is not a plain cancel, so the protocol library could retry or try the next host", c)
		}
		for j, other := range causes {
			if i != j && errors.Is(c, other) {
				t.Errorf("%v cannot be told apart from %v", c, other)
			}
		}
	}
	checkRetryableTimeout(t, "a stall", errMediaStall, false)
	checkRetryableTimeout(t, "a request past its bound", errRequestBound, true)
}

func TestADeclaredLengthOutsideTheArchivesRangeEndsTheJobAtOnce(t *testing.T) {
	for _, declared := range []int64{-1, mediaMaxDeclared + 1, math.MaxInt64} {
		ctx, stop := withMediaJob(t.Context(), declared, time.Hour)
		o := newMediaOrigin(mediaReply{body: []byte("synthetic"), then: io.EOF})
		_, _, err := mediaFetch(ctx, mediaHTTPClient(o, mediaRequestBound))
		checkJobEnded(t, err, ctx, errMediaOversize)
		if o.served() != 0 {
			t.Errorf("a job declaring %d bytes made %d requests", declared, o.served())
		}
		stop()
	}
	ctx, stop := withMediaJob(t.Context(), mediaMaxDeclared, time.Hour)
	defer stop()
	if ctx.Err() != nil {
		t.Fatalf("a job declaring the largest length ended with %v", context.Cause(ctx))
	}
}

func TestTheDiskDeadlineGrowsWithTheDeclaredLengthUpToItsCap(t *testing.T) {
	for declared, want := range map[int64]time.Duration{
		-1:               30 * time.Second,
		0:                30 * time.Second,
		1 << 20:          31 * time.Second,
		mediaMaxDeclared: 30*time.Second + 2048*time.Second,
		2370 << 20:       40 * time.Minute,
		math.MaxInt64:    40 * time.Minute,
	} {
		if got := mediaDiskDeadline(declared); got != want {
			t.Errorf("the disk deadline for %d bytes is %v, want %v", declared, got, want)
		}
	}
	if mediaRAMDeadline != 20*time.Second || mediaStall != time.Minute || mediaJobRequests != 6 {
		t.Errorf("the RAM deadline is %v, the stall %v and the request cap %d", mediaRAMDeadline, mediaStall, mediaJobRequests)
	}
	if mediaDiskDeadline(mediaMaxDeclared) >= mediaBackstop {
		t.Errorf("the client's backstop of %v binds before the largest job's deadline", mediaBackstop)
	}
}
