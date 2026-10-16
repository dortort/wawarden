package wa

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"
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
