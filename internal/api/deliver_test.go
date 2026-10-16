package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

const actionMediaProbe = "rest.media"

var (
	syntheticTag  = [16]byte{0x5e, 0x17, 0x7e, 0x71, 0xc0, 0xde, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}
	syntheticETag = `"5e177e71c0de00010203040506070809"`
	deliveryHeads = []string{"Content-Range", "Accept-Ranges", "ETag", "Content-Disposition", "Content-Security-Policy", "Cross-Origin-Resource-Policy"}
)

type finishes struct {
	calls    int
	sent     int64
	complete bool
}

func (f *finishes) record(sent int64, complete bool) {
	f.calls++
	f.sent, f.complete = sent, complete
}

type delivery struct {
	t        *testing.T
	data     []byte
	body     *fakeReaderAt
	clock    *clock
	expires  time.Time
	audit    *fakeAuditor
	failures *auditFailures
	finished *finishes
	writer   *deadlineWriter
	ctx      context.Context
}

func newDelivery(t *testing.T, size int) *delivery {
	data := syntheticBytes(size)
	return &delivery{
		t: t, data: data, body: &fakeReaderAt{data: data}, clock: newClock(), expires: testNow.Add(10 * time.Minute),
		audit: &fakeAuditor{}, failures: &auditFailures{}, finished: &finishes{}, writer: newDeadlineWriter(), ctx: t.Context(),
	}
}

func (d *delivery) attachment() dto.Attachment {
	d.t.Helper()
	a, err := dto.NewAttachment("document", "", "application/pdf", int64(len(d.data)), syntheticTag, d.body, d.expires, d.finished.record)
	if err != nil {
		d.t.Fatalf("NewAttachment: %v", err)
	}
	return a
}

func (d *delivery) request(method string, header http.Header) *http.Request {
	d.t.Helper()
	r := newRequest(d.t, method, "/v1/messages/SYNTHETIC/media", header)
	return r.WithContext(withClient(d.ctx, liveClient("client-x")))
}

func (d *delivery) serve(method string, header http.Header) {
	d.t.Helper()
	setSecurityHeaders(d.writer.Header())
	w, r := audited(d.writer, d.request(method, header), d.audit, d.failures.record, fixedNow)
	n := noteFrom(r.Context())
	n.action, n.chat, n.message = actionMediaProbe, groupChat, syntheticMessageRef
	deliver(w, r, d.attachment(), d.clock.now)
}

func (d *delivery) headerRow(bytes int64, reason string) ReadEvent {
	return ReadEvent{
		At: testNow, Client: "client-x", Action: actionMediaProbe, Chat: groupChat, OK: reason == outcomeOK, Reason: reason, Peer: "192.0.2.1",
		Message: syntheticMessageRef, Bytes: bytes,
	}
}

func (d *delivery) sentRow(bytes int64, reason string) ReadEvent {
	row := d.headerRow(bytes, reason)
	row.Action = actionMediaSent
	return row
}

func (d *delivery) requireAudit(want ...ReadEvent) {
	d.t.Helper()
	if got := d.audit.recorded(); !slices.Equal(got, want) {
		d.t.Fatalf("audit = %+v\nwant %+v", got, want)
	}
	if len(d.failures.actions) != 0 {
		d.t.Fatalf("audit failures %v", d.failures.actions)
	}
}

func (d *delivery) requireFinished(sent int64, complete bool) {
	d.t.Helper()
	if f := d.finished; f.calls != 1 || f.sent != sent || f.complete != complete {
		d.t.Fatalf("finished %d times, last with %d bytes complete %v; want once with %d bytes complete %v", f.calls, f.sent, f.complete, sent, complete)
	}
}

func requireNoDeliveryHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for _, name := range deliveryHeads {
		if v := h.Values(name); len(v) != 0 {
			t.Fatalf("an error answer carries %s: %q", name, v)
		}
	}
}

func TestDeliveryRenewsTheWriteDeadlineBeforeEachChunk(t *testing.T) {
	d := newDelivery(t, 2*deliveryChunk+1000)
	d.body.hook = func(int) { d.clock.advance(10 * time.Second) }
	d.serve(http.MethodGet, nil)
	rec := d.writer.ResponseRecorder
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), d.data) {
		t.Fatalf("response = %d with %d bytes, want 200 with the %d source bytes", rec.Code, rec.Body.Len(), len(d.data))
	}
	wantEvents := []string{"deadline", "header", "deadline", "write", "deadline", "write", "deadline", "write"}
	if !slices.Equal(d.writer.events, wantEvents) {
		t.Fatalf("events = %v, want %v", d.writer.events, wantEvents)
	}
	wantDeadlines := []time.Time{testNow.Add(30 * time.Second), testNow.Add(40 * time.Second), testNow.Add(50 * time.Second), testNow.Add(60 * time.Second)}
	if !slices.Equal(d.writer.deadlines, wantDeadlines) {
		t.Fatalf("deadlines = %v, want %v", d.writer.deadlines, wantDeadlines)
	}
	if got := d.body.reads(); got != 3 {
		t.Fatalf("ReadAt calls = %d, want 3 chunks", got)
	}
	size := int64(len(d.data))
	d.requireAudit(d.headerRow(size, outcomeOK), d.sentRow(size, outcomeOK))
	d.requireFinished(size, true)
	requireSecurityHeaders(t, rec.Header())
}

func TestTheWriteDeadlineNeverPassesTheJobExpiry(t *testing.T) {
	d := newDelivery(t, 2*deliveryChunk+1000)
	d.expires = testNow.Add(45 * time.Second)
	d.body.hook = func(int) { d.clock.advance(20 * time.Second) }
	d.serve(http.MethodGet, nil)
	wantDeadlines := []time.Time{testNow.Add(30 * time.Second), d.expires, d.expires}
	if !slices.Equal(d.writer.deadlines, wantDeadlines) {
		t.Fatalf("deadlines = %v, want %v", d.writer.deadlines, wantDeadlines)
	}
	if got := d.writer.Body.Len(); got != 2*deliveryChunk {
		t.Fatalf("body = %d bytes, want the two chunks read before the expiry", got)
	}
	d.requireAudit(d.headerRow(int64(len(d.data)), outcomeOK), d.sentRow(2*deliveryChunk, reasonExpired))
	d.requireFinished(2*deliveryChunk, false)
}

func TestAReplacedAuditWriterStopsWithoutReading(t *testing.T) {
	d := newDelivery(t, deliveryChunk)
	d.audit.fail = true
	d.serve(http.MethodGet, nil)
	requireError(t, d.writer.ResponseRecorder, http.StatusInternalServerError, codeInternal)
	if got := d.body.reads(); got != 0 {
		t.Fatalf("ReadAt calls = %d after a failed header row, want 0", got)
	}
	d.audit.fail = false
	d.requireAudit()
	d.requireFinished(0, false)
	requireSecurityHeaders(t, d.writer.Header())
}

func TestTheCompletionRowCarriesTheAcceptedBytesAndItsReason(t *testing.T) {
	size := 2*deliveryChunk + 1000
	tests := []struct {
		name   string
		setup  func(*delivery, context.CancelFunc)
		sent   int64
		reason string
	}{
		{name: "every byte accepted", setup: func(*delivery, context.CancelFunc) {}, sent: int64(size), reason: outcomeOK},
		{name: "the write deadline lapsed", setup: func(d *delivery, _ context.CancelFunc) {
			d.writer.failWrites = map[int]writeFailure{2: {accepted: 100, err: fmt.Errorf("synthetic write: %w", os.ErrDeadlineExceeded)}}
		}, sent: deliveryChunk + 100, reason: reasonWriteTimeout},
		{name: "the connection broke", setup: func(d *delivery, _ context.CancelFunc) {
			d.writer.failWrites = map[int]writeFailure{2: {accepted: 7, err: errors.New("synthetic: connection reset")}}
		}, sent: deliveryChunk + 7, reason: reasonClientGone},
		{name: "the client left", setup: func(d *delivery, cancel context.CancelFunc) {
			d.body.hook = func(call int) {
				if call == 1 {
					cancel()
				}
			}
		}, sent: deliveryChunk, reason: reasonClientGone},
		{name: "the deadline could not be renewed", setup: func(d *delivery, _ context.CancelFunc) {
			d.writer.failDeadlines = map[int]error{3: errors.New("synthetic: connection closed")}
		}, sent: deliveryChunk, reason: reasonClientGone},
		{name: "the job expired", setup: func(d *delivery, _ context.CancelFunc) {
			d.body.errs = map[int]error{2: fmt.Errorf("synthetic: %w", errDeliveryExpired)}
		}, sent: deliveryChunk, reason: reasonExpired},
		{name: "the clock passed the expiry", setup: func(d *delivery, _ context.CancelFunc) {
			d.body.hook = func(call int) {
				if call == 3 {
					d.clock.advance(10 * time.Minute)
				}
			}
		}, sent: 2 * deliveryChunk, reason: reasonExpired},
		{name: "the job was dropped", setup: func(d *delivery, _ context.CancelFunc) {
			d.body.errs = map[int]error{2: fmt.Errorf("synthetic: %w", errDeliveryDropped)}
		}, sent: deliveryChunk, reason: reasonRevoked},
		{name: "the spool failed", setup: func(d *delivery, _ context.CancelFunc) {
			d.body.errs = map[int]error{3: errors.New("synthetic: spool read failed")}
		}, sent: 2 * deliveryChunk, reason: reasonReadFailed},
		{name: "the spool is shorter than its size", setup: func(d *delivery, _ context.CancelFunc) {
			d.body.data = d.data[:deliveryChunk+10]
		}, sent: deliveryChunk, reason: reasonReadFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDelivery(t, size)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			d.ctx = ctx
			tt.setup(d, cancel)
			d.serve(http.MethodGet, nil)
			if !bytes.Equal(d.writer.Body.Bytes(), d.data[:tt.sent]) {
				t.Fatalf("body = %d bytes, want the first %d source bytes", d.writer.Body.Len(), tt.sent)
			}
			d.requireAudit(d.headerRow(int64(size), outcomeOK), d.sentRow(tt.sent, tt.reason))
			d.requireFinished(tt.sent, tt.reason == outcomeOK)
		})
	}
}

func TestAHeadDeliveryWritesNoBody(t *testing.T) {
	d := newDelivery(t, deliveryChunk+5)
	d.serve(http.MethodHead, nil)
	rec := d.writer.ResponseRecorder
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || d.writer.writes != 0 {
		t.Fatalf("response = %d with %d bytes in %d writes, want 200 with no body", rec.Code, rec.Body.Len(), d.writer.writes)
	}
	if got, want := rec.Header().Get("Content-Length"), strconv.Itoa(len(d.data)); got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
	if got := d.body.reads(); got != 0 {
		t.Fatalf("ReadAt calls = %d for HEAD, want 0", got)
	}
	d.requireAudit(d.headerRow(0, outcomeOK), d.sentRow(0, outcomeOK))
	d.requireFinished(0, false)
	requireSecurityHeaders(t, rec.Header())
}

func TestDeliveryHonoursOneRange(t *testing.T) {
	size := 3000
	tests := []struct {
		name     string
		header   http.Header
		status   int
		first    int
		last     int
		complete bool
	}{
		{name: "no range", status: http.StatusOK, last: size - 1, complete: true},
		{name: "a single range", header: http.Header{"Range": {"bytes=10-19"}}, status: http.StatusPartialContent, first: 10, last: 19},
		{name: "a suffix", header: http.Header{"Range": {"bytes=-100"}}, status: http.StatusPartialContent, first: size - 100, last: size - 1},
		{name: "an open range from the start", header: http.Header{"Range": {"bytes=0-"}}, status: http.StatusPartialContent, last: size - 1, complete: true},
		{name: "several ranges", header: http.Header{"Range": {"bytes=0-1,5-9"}}, status: http.StatusOK, last: size - 1, complete: true},
		{name: "two range lines", header: http.Header{"Range": {"bytes=0-1", "bytes=5-9"}}, status: http.StatusOK, last: size - 1, complete: true},
		{name: "a matching If-Range", header: http.Header{"Range": {"bytes=10-19"}, "If-Range": {syntheticETag}}, status: http.StatusPartialContent, first: 10, last: 19},
		{name: "a stale If-Range", header: http.Header{"Range": {"bytes=10-19"}, "If-Range": {`"00000000000000000000000000000000"`}}, status: http.StatusOK, last: size - 1, complete: true},
		{name: "a weak If-Range", header: http.Header{"Range": {"bytes=10-19"}, "If-Range": {"W/" + syntheticETag}}, status: http.StatusOK, last: size - 1, complete: true},
		{name: "a dated If-Range", header: http.Header{"Range": {"bytes=10-19"}, "If-Range": {"Fri, 02 Jan 2026 03:04:05 GMT"}}, status: http.StatusOK, last: size - 1, complete: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDelivery(t, size)
			d.serve(http.MethodGet, tt.header)
			rec := d.writer.ResponseRecorder
			want := d.data[tt.first : tt.last+1]
			if rec.Code != tt.status || !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("response = %d with %d bytes, want %d with bytes %d-%d", rec.Code, rec.Body.Len(), tt.status, tt.first, tt.last)
			}
			wantRange := ""
			if tt.status == http.StatusPartialContent {
				wantRange = fmt.Sprintf("bytes %d-%d/%d", tt.first, tt.last, size)
			}
			if got := rec.Header().Get("Content-Range"); got != wantRange {
				t.Fatalf("Content-Range = %q, want %q", got, wantRange)
			}
			n := int64(len(want))
			d.requireAudit(d.headerRow(n, outcomeOK), d.sentRow(n, outcomeOK))
			d.requireFinished(n, tt.complete)
			requireSecurityHeaders(t, rec.Header())
		})
	}
}

func TestAnUnsatisfiableRangeAnswers416WithTheSize(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			d := newDelivery(t, 3000)
			d.serve(method, http.Header{"Range": {"bytes=3000-"}})
			rec := d.writer.ResponseRecorder
			requireError(t, rec, http.StatusRequestedRangeNotSatisfiable, codeRangeNotSatisfiable)
			if got := rec.Header().Get("Content-Range"); got != "bytes */3000" {
				t.Fatalf("Content-Range = %q, want bytes */3000", got)
			}
			for _, name := range deliveryHeads[1:] {
				if v := rec.Header().Values(name); len(v) != 0 {
					t.Fatalf("a 416 carries %s: %q", name, v)
				}
			}
			if got := d.body.reads(); got != 0 {
				t.Fatalf("ReadAt calls = %d, want 0", got)
			}
			d.requireAudit(d.headerRow(0, codeRangeNotSatisfiable))
			d.requireFinished(0, false)
			requireSecurityHeaders(t, rec.Header())
		})
	}
}

func TestAnExpiredJobAnswersGoneWithoutReading(t *testing.T) {
	d := newDelivery(t, 3000)
	d.expires = testNow
	d.serve(http.MethodGet, nil)
	requireError(t, d.writer.ResponseRecorder, http.StatusGone, codeMediaExpired)
	if got := d.body.reads(); got != 0 || len(d.writer.deadlines) != 0 {
		t.Fatalf("ReadAt calls = %d and deadlines %v for an expired job, want neither", got, d.writer.deadlines)
	}
	d.requireAudit(d.headerRow(0, codeMediaExpired))
	d.requireFinished(0, false)
	requireNoDeliveryHeaders(t, d.writer.Header())
	requireSecurityHeaders(t, d.writer.Header())
}

func TestDeliveryNeverStreamsWithoutADeadlineOrAnAuditRow(t *testing.T) {
	t.Run("a writer without a write deadline", func(t *testing.T) {
		d := newDelivery(t, 3000)
		rec := httptest.NewRecorder()
		setSecurityHeaders(rec.Header())
		w, r := audited(rec, d.request(http.MethodGet, nil), d.audit, d.failures.record, fixedNow)
		noteFrom(r.Context()).action = actionMediaProbe
		deliver(w, r, d.attachment(), d.clock.now)
		requireError(t, rec, http.StatusInternalServerError, codeInternal)
		requireNoDeliveryHeaders(t, rec.Header())
		if got := d.body.reads(); got != 0 {
			t.Fatalf("ReadAt calls = %d, want 0", got)
		}
		d.requireFinished(0, false)
	})
	t.Run("a writer without an audit row", func(t *testing.T) {
		d := newDelivery(t, 3000)
		deliver(d.writer, d.request(http.MethodGet, nil), d.attachment(), d.clock.now)
		requireError(t, d.writer.ResponseRecorder, http.StatusInternalServerError, codeInternal)
		if got := d.body.reads(); got != 0 || len(d.writer.deadlines) != 0 {
			t.Fatalf("ReadAt calls = %d and deadlines %v, want neither", got, d.writer.deadlines)
		}
		d.requireFinished(0, false)
	})
	t.Run("an attachment built outside its constructor", func(t *testing.T) {
		d := newDelivery(t, 3000)
		w, r := audited(d.writer, d.request(http.MethodGet, nil), d.audit, d.failures.record, fixedNow)
		deliver(w, r, dto.Attachment{}, d.clock.now)
		requireError(t, d.writer.ResponseRecorder, http.StatusInternalServerError, codeInternal)
		if len(d.writer.deadlines) != 0 {
			t.Fatalf("deadlines %v, want none", d.writer.deadlines)
		}
	})
}

func TestARouteReturningAnAttachmentDelivers(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			d := newDelivery(t, 3000)
			rt := newRouter(d.clock.now, policy.AdminCredential{})
			rt.read("GET /probe", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
				return d.attachment(), nil
			})
			setSecurityHeaders(d.writer.Header())
			r := newRequest(t, method, "/probe", nil)
			w, r := audited(d.writer, r.WithContext(withClient(t.Context(), liveClient("client-x"))), d.audit, d.failures.record, fixedNow)
			rt.ServeHTTP(w, r)
			rec := d.writer.ResponseRecorder
			want := d.data
			if method == http.MethodHead {
				want = nil
			}
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) || rec.Header().Get("ETag") != syntheticETag {
				t.Fatalf("response = %d with %d bytes and ETag %q", rec.Code, rec.Body.Len(), rec.Header().Get("ETag"))
			}
			d.requireFinished(int64(len(want)), method == http.MethodGet)
			requireSecurityHeaders(t, rec.Header())
		})
	}
}

func TestABufferedAttachmentAnswersAnInternalError(t *testing.T) {
	d := newDelivery(t, 3000)
	rec := httptest.NewRecorder()
	writeResponse(rec, http.StatusOK, d.attachment())
	requireError(t, rec, http.StatusInternalServerError, codeInternal)

	rt := newRouter(fixedNow, policy.AdminCredential{})
	rt.read("GET /probe", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
		return new(d.attachment()), nil
	})
	rec = serve(rt, withClientRequest(t, liveClient("client-x"), http.MethodGet, "/probe"))
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
	if got := d.body.reads(); got != 0 || d.finished.calls != 0 {
		t.Fatalf("ReadAt calls = %d and finishes %d for a buffered attachment, want neither", got, d.finished.calls)
	}
}

func TestAMediaStatusSetsItsStatusAndRetryAfter(t *testing.T) {
	tests := []struct {
		state      string
		status     int
		retryAfter string
	}{
		{state: dto.MediaFetching, status: http.StatusAccepted, retryAfter: "2"},
		{state: dto.MediaReady, status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			rt := newRouter(fixedNow, policy.AdminCredential{})
			rt.read("POST /probe", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
				return dto.MediaStatus{AttachmentInfo: dto.AttachmentInfo{Kind: "image", State: tt.state}}, nil
			})
			rec := serve(rt, withClientRequest(t, liveClient("client-x"), http.MethodPost, "/probe"))
			var body struct {
				State string `json:"state"`
			}
			if rec.Code != tt.status || rec.Header().Get("Retry-After") != tt.retryAfter || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.State != tt.state {
				t.Fatalf("response = %d, Retry-After %q, body %q; want %d, %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String(), tt.status, tt.retryAfter)
			}
		})
	}
}

func TestTheMediaRefusalsCarryTheirStatus(t *testing.T) {
	tests := []struct {
		err    *codedError
		status int
		code   string
	}{
		{err: errMediaUnavailable, status: http.StatusNotFound, code: "media_unavailable"},
		{err: errMediaNotRequested, status: http.StatusConflict, code: "media_not_requested"},
		{err: errMediaExpired, status: http.StatusGone, code: "media_expired"},
		{err: errMediaTooLarge, status: http.StatusUnprocessableEntity, code: "media_too_large"},
		{err: errMediaUnsupported, status: http.StatusUnprocessableEntity, code: "media_unsupported"},
		{err: errMediaFailed, status: http.StatusBadGateway, code: "media_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			rt := newRouter(fixedNow, policy.AdminCredential{})
			rt.read("GET /probe", func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
				return nil, tt.err
			})
			rec := serve(rt, withClientRequest(t, liveClient("client-x"), http.MethodGet, "/probe"))
			requireError(t, rec, tt.status, tt.code)
			if rec.Header().Get("Retry-After") != "" {
				t.Fatalf("Retry-After = %q, want none", rec.Header().Get("Retry-After"))
			}
		})
	}
}
