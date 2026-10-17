package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	keyMedia      = "synthetic-media-reader"
	keyMedia2     = "synthetic-second-media-reader"
	keyMedia3     = "synthetic-third-media-reader"
	keyNoMedia    = "synthetic-reader-without-media"
	keyMediaAlice = "synthetic-alice-media-reader"

	smallSize = 3000
	diskSize  = 20 << 20
)

type fetchAnswer struct {
	spool MediaSpool
	err   error
	panic bool
}

type fakeMedia struct {
	mu         sync.Mutex
	precheck   error
	prechecked []int64
	fetches    []MediaFetch
	waits      map[string]chan fetchAnswer
}

func (f *fakeMedia) Precheck(declared int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prechecked = append(f.prechecked, declared)
	return f.precheck
}

func (f *fakeMedia) wait(id string) chan fetchAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.waits == nil {
		f.waits = map[string]chan fetchAnswer{}
	}
	if f.waits[id] == nil {
		f.waits[id] = make(chan fetchAnswer, 1)
	}
	return f.waits[id]
}

func (f *fakeMedia) Fetch(ctx context.Context, _ scoped.SealedRef, req MediaFetch) (MediaSpool, error) {
	f.mu.Lock()
	f.fetches = append(f.fetches, req)
	f.mu.Unlock()
	select {
	case a := <-f.wait(req.ID):
		if a.panic {
			panic(secretPanic{text: panicCanary})
		}
		return a.spool, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeMedia) answer(id string, a fetchAnswer) { f.wait(id) <- a }

func (f *fakeMedia) fetched() []MediaFetch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetches)
}

func (f *fakeMedia) refuse(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.precheck = err
}

type fakeSpool struct {
	mu         sync.Mutex
	data       []byte
	orig       []byte
	fetched    int64
	served     string
	normalised bool
	inMemory   bool
	erasures   int
	reads      int
	hook       func(call int)
}

func newFakeSpool(n int, inMemory bool) *fakeSpool {
	data := syntheticBytes(n)
	return &fakeSpool{data: data, orig: bytes.Clone(data), fetched: int64(n), served: "application/pdf", inMemory: inMemory}
}

func (s *fakeSpool) Size() int64        { return int64(len(s.orig)) }
func (s *fakeSpool) Fetched() int64     { return s.fetched }
func (s *fakeSpool) ServedType() string { return s.served }
func (s *fakeSpool) Format() string     { return "pdf" }
func (s *fakeSpool) Normalised() bool   { return s.normalised }
func (s *fakeSpool) InMemory() bool     { return s.inMemory }

func (s *fakeSpool) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	s.reads++
	call, hook := s.reads, s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.erasures > 0 {
		return 0, errors.New("synthetic: the spool was erased")
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *fakeSpool) Erase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erasures++
	clear(s.data)
}

func (s *fakeSpool) state() (erasures int, intact bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.erasures, bytes.Equal(s.data, s.orig)
}

type mediaOptions struct {
	off          bool
	max          int64
	perMinute    int
	perHour      int
	bytesPerHour int64
}

type mediaFixture struct {
	t       *testing.T
	clock   *clock
	archive *fakeArchive
	audit   *fakeAuditor
	media   *fakeMedia
	reg     *metrics.Registry
	handler http.Handler
	m       *mediaRegistry
	reads   *reads
	refs    *cursor.Sealer
	clients fakeAuthenticator
	stop    context.CancelFunc
	stopped chan struct{}
	ticks   chan time.Time

	mu         sync.Mutex
	revoked    map[string]bool
	liveErr    error
	livePanics bool
}

func mediaClient(id string, read ...policy.CanonicalChat) *policy.Client {
	c := readClient(id, read...)
	c.AllowMedia = true
	return c
}

func newMediaFixture(t *testing.T, o mediaOptions) *mediaFixture {
	t.Helper()
	f := &mediaFixture{
		t: t, clock: newClock(), archive: &fakeArchive{}, audit: &fakeAuditor{}, media: &fakeMedia{}, reg: metrics.NewRegistry(),
		stopped: make(chan struct{}), ticks: make(chan time.Time), revoked: map[string]bool{},
	}
	f.clients = fakeAuthenticator{
		keyMedia:      mediaClient("client-media", groupChat),
		keyMedia2:     mediaClient("client-media2", groupChat),
		keyMedia3:     mediaClient("client-media3", groupChat),
		keyNoMedia:    readClient("client-nomedia", groupChat),
		keyMediaAlice: mediaClient("client-media-alice", aliceChat),
	}
	f.archive.messages = []scoped.Message{syntheticMessage(groupChat, groupRef, 1, aliceChat)}
	deps := testClientDeps(t, f.clients, f.clock.now)
	deps.Archive, deps.Audit, deps.Metrics = f.archive, f.audit, f.reg
	f.refs = deps.Refs
	if !o.off {
		root, stop := context.WithCancel(context.Background())
		f.stop = stop
		t.Cleanup(func() {
			stop()
			<-f.stopped
		})
		deps.Media = &MediaDeps{
			Service: f.media, MaxBytes: cmpOr(o.max, 100<<20), PerMinute: cmpOr(o.perMinute, 30), GlobalPerHour: cmpOr(o.perHour, 600),
			GlobalBytesPerHour: cmpOr(o.bytesPerHour, 64<<30), Root: root, ClientLive: f.clientLive, Stopped: func() { close(f.stopped) },
			sweeps: f.ticks,
		}
	}
	p, s := newClientHandler(deps)
	f.handler, f.m, f.reads = p, s.media, s
	return f
}

func cmpOr[T int | int64](v, fallback T) T {
	if v == 0 {
		return fallback
	}
	return v
}

func (f *mediaFixture) clientLive(_ context.Context, client string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.livePanics {
		panic(secretPanic{text: panicCanary})
	}
	return !f.revoked[client], f.liveErr
}

func (f *mediaFixture) revoke(client string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[client] = true
	for _, c := range f.clients {
		if c.ID == client {
			c.Revoked = true
		}
	}
}

func (f *mediaFixture) attach(id, kind string, declared int64) {
	f.archive.mu.Lock()
	defer f.archive.mu.Unlock()
	f.archive.attachments = append(f.archive.attachments, scoped.Attachment{
		Chat: groupChat, ID: id, Sender: aliceChat, Kind: kind, Declared: declared, Expires: testNow.Add(30 * 24 * time.Hour), Usable: true,
	})
}

func (f *mediaFixture) withdraw(id string, keepRow bool) {
	f.archive.mu.Lock()
	defer f.archive.mu.Unlock()
	f.archive.attachments = slices.DeleteFunc(f.archive.attachments, func(a scoped.Attachment) bool { return a.ID == id })
	if keepRow {
		f.archive.attachments = append(f.archive.attachments, scoped.Attachment{Chat: groupChat, ID: id, Sender: aliceChat})
	}
}

func (f *mediaFixture) mref(key, id string) string {
	f.t.Helper()
	ref, err := f.refs.SealRef(f.clients[key].ID, cursor.Ref{Chat: groupChat.JID(), ID: id, Sender: aliceChat.JID()})
	if err != nil {
		f.t.Fatalf("SealRef: %v", err)
	}
	return ref
}

func (f *mediaFixture) key(key, id string) mediaKey {
	return mediaKey{client: f.clients[key].ID, chat: groupChat, id: id, sender: aliceChat}
}

func (f *mediaFixture) request(method, key, mref string, header http.Header) *deadlineWriter {
	f.t.Helper()
	h := bearer(key)
	for name, values := range header {
		h[name] = values
	}
	w := newDeadlineWriter()
	f.handler.ServeHTTP(w, newRequest(f.t, method, "/v1/messages/"+mref+"/media", h))
	return w
}

func (f *mediaFixture) post(key, id string) *deadlineWriter {
	return f.request(http.MethodPost, key, f.mref(key, id), nil)
}

func (f *mediaFixture) get(key, id string, header http.Header) *deadlineWriter {
	return f.request(http.MethodGet, key, f.mref(key, id), header)
}

func (f *mediaFixture) view(key, id string) mediaView {
	v, _ := f.m.lookup(f.key(key, id), false)
	return v
}

func (f *mediaFixture) fetches(n int) {
	f.t.Helper()
	f.await(strconv.Itoa(n)+" fetches", func() bool { return len(f.media.fetched()) == n })
}

func (f *mediaFixture) await(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *mediaFixture) settled(key, id string) mediaView {
	f.t.Helper()
	f.await("the job to settle", func() bool { return f.view(key, id).state != mediaFetching })
	return f.view(key, id)
}

func (f *mediaFixture) ready(key, id string, spool *fakeSpool) mediaView {
	f.t.Helper()
	if w := f.post(key, id); w.Code != http.StatusAccepted {
		f.t.Fatalf("POST = %d %s, want 202", w.Code, w.Body.String())
	}
	f.media.answer(id, fetchAnswer{spool: spool})
	v := f.settled(key, id)
	if v.state != mediaReady {
		f.t.Fatalf("the job settled in state %d, want ready", v.state)
	}
	return v
}

func (f *mediaFixture) slots() (fetching, staged int) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return f.m.fetching, f.m.staged
}

func (f *mediaFixture) holds(j *mediaJob) int {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return j.holds
}

func (f *mediaFixture) metric(series string) string { return metricValue(f.t, f.reg, series) }

func statusOf(t *testing.T, w *deadlineWriter) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return body
}

func requireWriterError(t *testing.T, w *deadlineWriter, status int, code string) {
	t.Helper()
	requireError(t, w.ResponseRecorder, status, code)
}

func TestMediaNotFoundAnswersAreIdenticalWhetherDownloadsAreOnOrOff(t *testing.T) {
	for _, off := range []bool{false, true} {
		f := newMediaFixture(t, mediaOptions{off: off})
		f.attach("MEDIA1", "document", smallSize)
		f.archive.attachments = append(f.archive.attachments, scoped.Attachment{
			Chat: aliceChat, ID: "MEDIA2", Sender: aliceChat, Kind: "document", Declared: smallSize, Expires: testNow.Add(time.Hour), Usable: true,
		})
		foreign := f.mref(keyMedia2, "MEDIA1")
		alice, err := f.refs.SealRef("client-media", cursor.Ref{Chat: aliceChat.JID(), ID: "MEDIA2", Sender: aliceChat.JID()})
		if err != nil {
			t.Fatalf("SealRef: %v", err)
		}
		cases := []struct{ name, key, mref string }{
			{"garbage", keyMedia, "not-a-sealed-ref"},
			{"no media grant", keyNoMedia, f.mref(keyNoMedia, "MEDIA1")},
			{"sealed for another client", keyMedia, foreign},
			{"out of scope", keyMedia, alice},
			{"missing", keyMedia, f.mref(keyMedia, "MISSING1")},
			{"a chat outside the grant", keyMediaAlice, f.mref(keyMediaAlice, "MEDIA1")},
		}
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodHead} {
			reference := f.request(method, keyMedia, "not-a-sealed-ref", nil)
			if method != http.MethodHead {
				requireWriterError(t, reference, http.StatusNotFound, codeNotFound)
			}
			for _, c := range cases {
				got := f.request(method, c.key, c.mref, nil)
				requireSameResponse(t, got.ResponseRecorder, reference.ResponseRecorder)
			}
		}
		if got := f.media.fetched(); len(got) != 0 {
			t.Fatalf("fetches %v for refused requests", got)
		}
		w := f.post(keyMedia, "MEDIA1")
		if off {
			requireWriterError(t, w, http.StatusNotFound, codeMediaUnavailable)
		} else if w.Code != http.StatusAccepted {
			t.Fatalf("POST with downloads on = %d %s", w.Code, w.Body.String())
		}
	}
}

func TestAMessageWithoutAUsableReferenceIsUnavailableAndDropsItsJob(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	spool := newFakeSpool(smallSize, true)
	f.ready(keyMedia, "MEDIA1", spool)
	f.withdraw("MEDIA1", true)
	requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), http.StatusNotFound, codeMediaUnavailable)
	if v := f.view(keyMedia, "MEDIA1"); v.state != 0 {
		t.Fatalf("the job survived an ineligible lookup in state %d", v.state)
	}
	if erasures, _ := spool.state(); erasures != 1 {
		t.Fatalf("the spool was erased %d times, want once", erasures)
	}
	requireWriterError(t, f.post(keyMedia, "MEDIA1"), http.StatusNotFound, codeMediaUnavailable)
	f.withdraw("MEDIA1", false)
	f.attach("MEDIA1", "document", smallSize)
	f.ready(keyMedia, "MEDIA1", newFakeSpool(smallSize, true))
	f.withdraw("MEDIA1", false)
	requireWriterError(t, f.post(keyMedia, "MEDIA1"), http.StatusNotFound, codeNotFound)
	if v := f.view(keyMedia, "MEDIA1"); v.state != 0 {
		t.Fatalf("the job survived a lookup that found no message, in state %d", v.state)
	}
}

func TestTheMediaRoutesTakeNoQuery(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		w := newDeadlineWriter()
		f.handler.ServeHTTP(w, newRequest(t, method, "/v1/messages/"+f.mref(keyMedia, "MEDIA1")+"/media?part=1", bearer(keyMedia)))
		requireWriterError(t, w, http.StatusBadRequest, codeInvalidQuery)
	}
	if got := f.media.fetched(); len(got) != 0 {
		t.Fatalf("fetches %v for refused requests", got)
	}
}

func TestTheJobStateMachine(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), http.StatusConflict, codeMediaNotRequested)

	w := f.post(keyMedia, "MEDIA1")
	body := statusOf(t, w)
	if w.Code != http.StatusAccepted || w.Header().Get("Retry-After") != "2" || body["state"] != "fetching" || body["reason"] != "fetching" ||
		body["retry_after_seconds"] != float64(2) || body["delivery"] != "metadata" || body["size_declared"] != float64(smallSize) ||
		body["rest_route"] != "/v1/messages/{mref}/media" || body["untrusted"] != true || body["notice"] != mediaNotice || body["kind"] != "document" {
		t.Fatalf("POST = %d %q Retry-After %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if w := f.get(keyMedia, "MEDIA1", nil); w.Code != http.StatusAccepted || statusOf(t, w)["state"] != "fetching" || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("GET while fetching = %d %q", w.Code, w.Body.String())
	}
	if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
		t.Fatalf("a second POST while fetching = %d %q", w.Code, w.Body.String())
	}
	f.fetches(1)
	if got := f.media.fetched(); len(got) != 1 || got[0] != (MediaFetch{Chat: groupChat, ID: "MEDIA1", Sender: aliceChat, Kind: "document", Declared: smallSize}) {
		t.Fatalf("fetches = %+v, want one for the stored row", got)
	}

	spool := newFakeSpool(smallSize, true)
	f.media.answer("MEDIA1", fetchAnswer{spool: spool})
	f.settled(keyMedia, "MEDIA1")
	w = f.post(keyMedia, "MEDIA1")
	body = statusOf(t, w)
	if w.Code != http.StatusOK || w.Header().Get("Retry-After") != "" || body["state"] != "ready" || body["served_type"] != "application/pdf" ||
		body["format"] != "pdf" || body["bytes"] != float64(smallSize) || body["parts"] != float64(1) || body["reason"] != nil ||
		body["expires_at"] != "2026-01-02T03:14:05.000Z" || body["retry_after_seconds"] != nil {
		t.Fatalf("POST on a ready job = %d %q", w.Code, w.Body.String())
	}
	if len(body) != 17 {
		t.Fatalf("the status carries %d fields: %q", len(body), w.Body.String())
	}

	w = f.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=10-19"}})
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), spool.orig[10:20]) {
		t.Fatalf("a ranged GET = %d with %d bytes", w.Code, w.Body.Len())
	}
	if v := f.view(keyMedia, "MEDIA1"); v.state != mediaReady {
		t.Fatal("a partial delivery erased the job")
	}
	w = f.request(http.MethodHead, keyMedia, f.mref(keyMedia, "MEDIA1"), nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(smallSize) {
		t.Fatalf("HEAD = %d with %d bytes", w.Code, w.Body.Len())
	}
	w = f.get(keyMedia, "MEDIA1", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), spool.orig) || w.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("GET = %d with %d bytes of %q", w.Code, w.Body.Len(), w.Header().Get("Content-Type"))
	}
	requireSecurityHeaders(t, w.Header())
	if erasures, _ := spool.state(); erasures != 1 {
		t.Fatalf("the spool was erased %d times after the whole-body delivery, want once", erasures)
	}
	requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), http.StatusConflict, codeMediaNotRequested)
	if fetching, staged := f.slots(); fetching != 0 || staged != 0 {
		t.Fatalf("slots after the job: %d fetching and %d staged", fetching, staged)
	}
}

func TestAWholeRangeCompletesTheJob(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	spool := newFakeSpool(smallSize, true)
	f.ready(keyMedia, "MEDIA1", spool)
	w := f.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-"}})
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), spool.orig) {
		t.Fatalf("GET bytes=0- = %d with %d bytes", w.Code, w.Body.Len())
	}
	if erasures, _ := spool.state(); erasures != 1 || f.view(keyMedia, "MEDIA1").state != 0 {
		t.Fatalf("a 206 covering the whole file left the job, with %d erasures", erasures)
	}
}

func TestEachTerminalCodeIsKeptByItsTombstone(t *testing.T) {
	cases := []struct {
		name   string
		answer fetchAnswer
		status int
		code   string
		result string
	}{
		{"expired", fetchAnswer{err: ErrMediaExpired}, http.StatusGone, codeMediaExpired, "expired"},
		{"failed", fetchAnswer{err: ErrMediaFailed}, http.StatusBadGateway, codeMediaFailed, "failed"},
		{"unsupported", fetchAnswer{err: ErrMediaUnsupported}, http.StatusUnprocessableEntity, codeMediaUnsupported, "unsupported"},
		{"too large", fetchAnswer{err: ErrMediaTooLarge}, http.StatusUnprocessableEntity, codeMediaTooLarge, "too_large"},
		{"unavailable", fetchAnswer{err: ErrMediaUnavailable}, http.StatusNotFound, codeMediaUnavailable, "unavailable"},
		{"an unclassified error", fetchAnswer{err: errors.New("synthetic library text")}, http.StatusBadGateway, codeMediaFailed, "failed"},
		{"a refusal no fetch answers", fetchAnswer{err: ErrNotFound}, http.StatusBadGateway, codeMediaFailed, "failed"},
		{"no spool and no error", fetchAnswer{}, http.StatusBadGateway, codeMediaFailed, "failed"},
		{"a panic", fetchAnswer{panic: true}, http.StatusBadGateway, codeMediaFailed, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMediaFixture(t, mediaOptions{})
			logs := installPanicReporter(t, f.reg)
			f.attach("MEDIA1", "document", smallSize)
			f.post(keyMedia, "MEDIA1")
			f.media.answer("MEDIA1", c.answer)
			f.settled(keyMedia, "MEDIA1")
			requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), c.status, c.code)
			requireWriterError(t, f.post(keyMedia, "MEDIA1"), c.status, c.code)
			if got := len(f.media.fetched()); got != 1 {
				t.Fatalf("%d fetches, want the tombstone to answer without a second", got)
			}
			if got := f.metric(`wawarden_media_fetches_total{result="` + c.result + `"}`); got != "1" {
				t.Fatalf("fetches{result=%q} = %q, want 1", c.result, got)
			}
			if fetching, staged := f.slots(); fetching != 0 || staged != 0 {
				t.Fatalf("a tombstone holds %d fetch and %d staged slots", fetching, staged)
			}
			if c.answer.panic && f.metric(`wawarden_panics_total{name="api.media.job"}`) != "1" {
				t.Fatalf("the job's panic was not counted: %s", logs.String())
			}
			f.clock.advance(mediaFailedLife - time.Second)
			requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), c.status, c.code)
			f.clock.advance(time.Second)
			if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
				t.Fatalf("POST after the tombstone's life = %d", w.Code)
			}
			f.fetches(2)
		})
	}
}

func TestAThrottledFetchAnswersBusyUntilItsRetryAfter(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.post(keyMedia, "MEDIA1")
	f.media.answer("MEDIA1", fetchAnswer{err: Busy(90 * time.Second)})
	f.settled(keyMedia, "MEDIA1")
	f.clock.advance(30 * time.Second)
	w := f.get(keyMedia, "MEDIA1", nil)
	requireWriterError(t, w, http.StatusServiceUnavailable, codeBusy)
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want the 60 s left", got)
	}
	if got := f.metric(`wawarden_media_fetches_total{result="throttled"}`); got != "1" {
		t.Fatalf("fetches{result=throttled} = %q", got)
	}
	f.clock.advance(60 * time.Second)
	if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
		t.Fatalf("POST after the wait = %d", w.Code)
	}
	f.fetches(2)
}

func TestALostConnectionEndsTheJobWithoutATombstone(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.post(keyMedia, "MEDIA1")
	f.media.answer("MEDIA1", fetchAnswer{err: ErrNotConnected})
	f.await("the job to end", func() bool { return f.view(keyMedia, "MEDIA1").state == 0 })
	requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), http.StatusConflict, codeMediaNotRequested)
	if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
		t.Fatalf("POST after the lost connection = %d", w.Code)
	}
	f.fetches(2)
	if got := f.metric(`wawarden_media_fetches_total{result="not_connected"}`); got != "1" {
		t.Fatalf("fetches{result=not_connected} = %q", got)
	}
}

func TestAWaiterWakesWhenTheJobSettlesOrIsErased(t *testing.T) {
	for _, c := range []struct {
		name   string
		settle func(f *mediaFixture)
	}{
		{"ready", func(f *mediaFixture) { f.media.answer("MEDIA1", fetchAnswer{spool: newFakeSpool(smallSize, true)}) }},
		{"failed", func(f *mediaFixture) { f.media.answer("MEDIA1", fetchAnswer{err: errMediaExpired}) }},
		{"erased while fetching", func(f *mediaFixture) { f.m.drop(f.key(keyMedia, "MEDIA1")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", "document", smallSize)
			if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
				t.Fatalf("POST = %d", w.Code)
			}
			job := f.view(keyMedia, "MEDIA1").job
			woke := make(chan struct{})
			go func() {
				job.await(t.Context())
				close(woke)
			}()
			if closedWithin(woke, 20*time.Millisecond) {
				t.Fatal("the waiter woke while the job was fetching")
			}
			c.settle(f)
			if !closedWithin(woke, 10*time.Second) {
				t.Fatal("the waiter did not wake")
			}
			f.await("the fetch to end", func() bool { fetching, _ := f.slots(); return fetching == 0 })
		})
	}
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.post(keyMedia, "MEDIA1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.view(keyMedia, "MEDIA1").job.await(ctx)
}

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestReadyJobsLiveTheirTTL(t *testing.T) {
	for _, c := range []struct {
		name     string
		inMemory bool
		life     time.Duration
	}{{"in memory", true, mediaRAMLife}, {"on disk", false, mediaDiskLife}} {
		t.Run(c.name, func(t *testing.T) {
			f := newMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", "document", smallSize)
			spool := newFakeSpool(smallSize, c.inMemory)
			v := f.ready(keyMedia, "MEDIA1", spool)
			if want := testNow.Add(c.life); !v.expires.Equal(want) {
				t.Fatalf("the job expires at %v, want %v", v.expires, want)
			}
			reader := jobReader{m: f.m, job: v.job}
			f.clock.advance(c.life - time.Second)
			if w := f.request(http.MethodHead, keyMedia, f.mref(keyMedia, "MEDIA1"), nil); w.Code != http.StatusOK {
				t.Fatalf("HEAD before the TTL = %d", w.Code)
			}
			f.clock.advance(time.Second)
			if _, err := reader.ReadAt(make([]byte, 1), 0); !errors.Is(err, errDeliveryExpired) {
				t.Fatalf("ReadAt at the TTL = %v, want the expiry", err)
			}
			requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), http.StatusConflict, codeMediaNotRequested)
			if erasures, _ := spool.state(); erasures != 1 {
				t.Fatalf("the spool was erased %d times at its TTL", erasures)
			}
			if _, staged := f.slots(); staged != 0 {
				t.Fatalf("%d staged slots after the TTL", staged)
			}
			if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
				t.Fatalf("POST after the TTL = %d", w.Code)
			}
			f.fetches(2)
		})
	}
}

func TestAdmissionLimits(t *testing.T) {
	t.Run("two live jobs per client", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{})
		for _, id := range []string{"MEDIA1", "MEDIA2", "MEDIA3"} {
			f.attach(id, "document", smallSize)
		}
		f.ready(keyMedia, "MEDIA1", newFakeSpool(smallSize, true))
		f.post(keyMedia, "MEDIA2")
		w := f.post(keyMedia, "MEDIA3")
		requireWriterError(t, w, http.StatusTooManyRequests, codeRateLimited)
		if w.Header().Get("Retry-After") != "" || f.metric(`wawarden_media_fetches_total{result="rate_limited"}`) != "1" {
			t.Fatalf("the third live job answered Retry-After %q", w.Header().Get("Retry-After"))
		}
		if w := f.post(keyMedia2, "MEDIA3"); w.Code != http.StatusAccepted {
			t.Fatalf("another client's job = %d", w.Code)
		}
	})
	t.Run("two fetch slots", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{})
		f.attach("MEDIA1", "document", smallSize)
		f.post(keyMedia, "MEDIA1")
		f.post(keyMedia2, "MEDIA1")
		w := f.post(keyMedia3, "MEDIA1")
		requireWriterError(t, w, http.StatusServiceUnavailable, codeBusy)
		if w.Header().Get("Retry-After") != "1" || f.metric(`wawarden_media_fetches_total{result="busy"}`) != "1" {
			t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
		}
		f.media.answer("MEDIA1", fetchAnswer{spool: newFakeSpool(smallSize, true)})
		f.await("a fetch slot", func() bool { fetching, _ := f.slots(); return fetching == 1 })
		if w := f.post(keyMedia3, "MEDIA1"); w.Code != http.StatusAccepted {
			t.Fatalf("POST once a slot is free = %d", w.Code)
		}
	})
	t.Run("one disk fetch", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{})
		f.attach("DISK1", "document", diskSize)
		f.attach("DISK2", "document", diskSize)
		f.post(keyMedia, "DISK1")
		requireWriterError(t, f.post(keyMedia2, "DISK2"), http.StatusServiceUnavailable, codeBusy)
		f.media.answer("DISK1", fetchAnswer{spool: newFakeSpool(smallSize, false)})
		f.settled(keyMedia, "DISK1")
		requireWriterError(t, f.post(keyMedia2, "DISK2"), http.StatusServiceUnavailable, codeBusy)
		f.attach("SMALL1", "document", smallSize)
		if w := f.post(keyMedia2, "SMALL1"); w.Code != http.StatusAccepted {
			t.Fatalf("a RAM job beside the disk job = %d", w.Code)
		}
	})
	t.Run("four staged objects", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{})
		for _, id := range []string{"MEDIA1", "MEDIA2", "MEDIA3"} {
			f.attach(id, "document", smallSize)
		}
		for _, key := range []string{keyMedia, keyMedia2} {
			for _, id := range []string{"MEDIA1", "MEDIA2"} {
				f.ready(key, id, newFakeSpool(smallSize, true))
			}
		}
		requireWriterError(t, f.post(keyMedia3, "MEDIA3"), http.StatusServiceUnavailable, codeBusy)
		if w := f.get(keyMedia, "MEDIA1", nil); w.Code != http.StatusOK {
			t.Fatalf("GET = %d", w.Code)
		}
		if w := f.post(keyMedia3, "MEDIA3"); w.Code != http.StatusAccepted {
			t.Fatalf("POST once a staged object is erased = %d", w.Code)
		}
	})
	t.Run("the effective cap", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{max: 4 << 20})
		f.attach("IMAGE1", "image", 4<<20+1)
		f.attach("STICKER1", "sticker", 1<<20+1)
		f.attach("STICKER2", "sticker", 1<<20)
		requireWriterError(t, f.post(keyMedia, "IMAGE1"), http.StatusUnprocessableEntity, codeMediaTooLarge)
		requireWriterError(t, f.post(keyMedia, "STICKER1"), http.StatusUnprocessableEntity, codeMediaTooLarge)
		if w := f.post(keyMedia, "STICKER2"); w.Code != http.StatusAccepted {
			t.Fatalf("a sticker at its ceiling = %d", w.Code)
		}
		if f.metric(`wawarden_media_fetches_total{result="too_large"}`) != "2" || len(f.media.prechecked) != 1 {
			t.Fatalf("oversize requests were charged or prechecked: %v", f.media.prechecked)
		}
	})
}

func requireWholeClientBurst(t *testing.T, f *mediaFixture, client string) {
	t.Helper()
	for range mediaClientBurst {
		if _, ok := f.m.perClient.Allow(client); !ok {
			t.Fatal("a refused admission charged the client")
		}
	}
}

func TestBudgetsPeekAtAllBeforeCharging(t *testing.T) {
	t.Run("per client", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{perMinute: 6})
		for _, id := range []string{"MEDIA1", "MEDIA2", "MEDIA3"} {
			f.attach(id, "document", smallSize)
			if id == "MEDIA3" {
				break
			}
			f.post(keyMedia, id)
			f.media.answer(id, fetchAnswer{err: ErrMediaFailed})
			f.settled(keyMedia, id)
		}
		w := f.post(keyMedia, "MEDIA3")
		requireWriterError(t, w, http.StatusTooManyRequests, codeRateLimited)
		if got := w.Header().Get("Retry-After"); got != "10" {
			t.Fatalf("Retry-After = %q, want 10 after a burst of 2 at 6 a minute", got)
		}
		if _, ok := f.m.count.Peek(598); !ok {
			t.Fatal("a refused admission charged the global count")
		}
		if _, ok := f.m.bytes.Peek(64<<30 - 2*smallSize); !ok {
			t.Fatal("a refused admission charged the global bytes")
		}
		f.clock.advance(10 * time.Second)
		if w := f.post(keyMedia, "MEDIA3"); w.Code != http.StatusAccepted {
			t.Fatalf("POST after the wait = %d", w.Code)
		}
	})
	t.Run("global count", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{perHour: 1})
		f.attach("MEDIA1", "document", smallSize)
		f.post(keyMedia, "MEDIA1")
		w := f.post(keyMedia2, "MEDIA1")
		requireWriterError(t, w, http.StatusTooManyRequests, codeRateLimited)
		if w.Header().Get("Retry-After") != "3600" || f.metric(`wawarden_media_fetches_total{result="global_limited"}`) != "1" {
			t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
		}
		requireWholeClientBurst(t, f, "client-media2")
		if _, ok := f.m.bytes.Peek(64<<30 - smallSize); !ok {
			t.Fatal("a refused admission charged the global bytes")
		}
	})
	t.Run("global bytes, debited after the fetch", func(t *testing.T) {
		f := newMediaFixture(t, mediaOptions{bytesPerHour: 3 * smallSize})
		for _, id := range []string{"MEDIA1", "MEDIA2"} {
			f.attach(id, "document", smallSize)
		}
		spool := newFakeSpool(smallSize, true)
		spool.fetched = 2*smallSize + 1
		f.ready(keyMedia, "MEDIA1", spool)
		if _, ok := f.m.bytes.Peek(smallSize); ok {
			t.Fatal("the extra fetched bytes were not debited")
		}
		w := f.post(keyMedia2, "MEDIA2")
		requireWriterError(t, w, http.StatusTooManyRequests, codeRateLimited)
		if _, ok := f.m.count.Peek(599); !ok {
			t.Fatal("a refused admission charged the global count")
		}
		requireWholeClientBurst(t, f, "client-media2")
		if got := f.metric("wawarden_media_fetched_bytes_total"); got != strconv.Itoa(2*smallSize+1) {
			t.Fatalf("fetched bytes = %q", got)
		}
	})
}

func TestPrecheckRefusalsChargeNothing(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	for _, c := range []struct {
		err        error
		status     int
		code       string
		retryAfter string
		result     string
	}{
		{Busy(45 * time.Second), http.StatusServiceUnavailable, codeBusy, "45", "busy"},
		{ErrNotConnected, http.StatusServiceUnavailable, codeNotConnected, "5", "not_connected"},
		{errors.New("synthetic unclassified"), http.StatusServiceUnavailable, codeBusy, "1", "busy"},
	} {
		f.media.refuse(c.err)
		w := f.post(keyMedia, "MEDIA1")
		requireWriterError(t, w, c.status, c.code)
		if w.Header().Get("Retry-After") != c.retryAfter {
			t.Fatalf("Retry-After = %q, want %q", w.Header().Get("Retry-After"), c.retryAfter)
		}
		if f.metric(`wawarden_media_fetches_total{result="`+c.result+`"}`) == "0" {
			t.Fatalf("the %s refusal was not counted", c.code)
		}
	}
	if len(f.media.fetched()) != 0 {
		t.Fatal("a refused precheck fetched")
	}
	if _, ok := f.m.count.Peek(600); !ok {
		t.Fatal("a refused precheck charged the global count")
	}
	f.media.refuse(nil)
	if w := f.post(keyMedia, "MEDIA1"); w.Code != http.StatusAccepted {
		t.Fatalf("POST once prechecked = %d", w.Code)
	}
}

func TestTheSweepDropsIneligibleJobs(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	for _, id := range []string{"MEDIA1", "MEDIA2", "MEDIA3", "MEDIA4"} {
		f.attach(id, "document", smallSize)
	}
	f.post(keyMedia3, "MEDIA2")
	f.media.answer("MEDIA2", fetchAnswer{err: ErrMediaFailed})
	f.settled(keyMedia3, "MEDIA2")
	revokedSpool := newFakeSpool(4*deliveryChunk, true)
	f.ready(keyMedia, "MEDIA1", revokedSpool)
	keptSpool := newFakeSpool(smallSize, true)
	f.ready(keyMedia, "MEDIA2", keptSpool)
	f.post(keyMedia2, "MEDIA3")
	f.ready(keyMedia3, "MEDIA4", newFakeSpool(smallSize, true))

	entered, resume := make(chan struct{}), make(chan struct{})
	revokedSpool.hook = func(call int) {
		if call == 2 {
			close(entered)
			<-resume
		}
	}
	delivered := make(chan *deadlineWriter)
	go func() { delivered <- f.get(keyMedia, "MEDIA1", nil) }()
	<-entered
	f.withdraw("MEDIA1", true)
	f.withdraw("MEDIA3", false)
	f.revoke("client-media3")
	f.ticks <- testNow
	f.ticks <- testNow
	close(resume)
	w := <-delivered
	if w.Code != http.StatusOK || w.Body.Len() != deliveryChunk {
		t.Fatalf("the delivery in flight = %d with %d bytes, want it cut after one chunk", w.Code, w.Body.Len())
	}
	rows := f.audit.recorded()
	if last := rows[len(rows)-1]; last.Action != actionMediaSent || last.Reason != reasonRevoked || last.Bytes != deliveryChunk {
		t.Fatalf("the completion row = %+v", last)
	}
	for _, c := range []struct{ key, id string }{{keyMedia, "MEDIA1"}, {keyMedia2, "MEDIA3"}, {keyMedia3, "MEDIA4"}} {
		if v := f.view(c.key, c.id); v.state != 0 {
			t.Fatalf("the sweep kept %s of %s in state %d", c.id, c.key, v.state)
		}
	}
	if v := f.view(keyMedia, "MEDIA2"); v.state != mediaReady {
		t.Fatalf("the sweep dropped an eligible job, leaving state %d", v.state)
	}
	if v := f.view(keyMedia3, "MEDIA2"); v.state != mediaFailed {
		t.Fatalf("the sweep inspected a tombstone, which holds no bytes, leaving state %d", v.state)
	}
	if erasures, _ := revokedSpool.state(); erasures != 1 {
		t.Fatalf("the dropped spool was erased %d times", erasures)
	}
	f.await("the dropped fetch to end", func() bool { fetching, _ := f.slots(); return fetching == 0 })

	f.mu.Lock()
	f.liveErr = errors.New("synthetic: the client table is busy")
	f.mu.Unlock()
	f.archive.mu.Lock()
	f.archive.err = scoped.ErrBusy
	f.archive.mu.Unlock()
	f.revoke("client-media")
	f.ticks <- testNow
	f.ticks <- testNow
	if v := f.view(keyMedia, "MEDIA2"); v.state != mediaReady {
		t.Fatal("a sweep that could not read dropped the job")
	}
	f.clock.advance(mediaRAMLife)
	f.ticks <- testNow
	f.ticks <- testNow
	if erasures, _ := keptSpool.state(); erasures != 1 || f.view(keyMedia, "MEDIA2").state != 0 {
		t.Fatalf("the sweep left an expired job, with %d erasures", erasures)
	}
}

func TestAPanicInOneSweepLeavesTheNextOnesRunning(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	logs := installPanicReporter(t, f.reg)
	f.attach("MEDIA1", "document", smallSize)
	spool := newFakeSpool(smallSize, true)
	f.ready(keyMedia, "MEDIA1", spool)
	f.mu.Lock()
	f.livePanics = true
	f.mu.Unlock()
	f.ticks <- testNow
	f.ticks <- testNow
	f.await("the sweep's panic to be counted", func() bool { return f.metric(`wawarden_panics_total{name="api.media.sweep"}`) != "" })
	if strings.Contains(logs.String(), panicCanary) {
		t.Fatal("the panic's value was logged")
	}
	f.mu.Lock()
	f.livePanics = false
	f.mu.Unlock()
	f.clock.advance(mediaRAMLife)
	f.ticks <- testNow
	f.ticks <- testNow
	if erasures, _ := spool.state(); erasures != 1 {
		t.Fatalf("the sweep after a panic left the expired job, with %d erasures", erasures)
	}
}

func TestPerJobDeliveryCaps(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", 1000)
	f.ready(keyMedia, "MEDIA1", newFakeSpool(1000, true))
	ranged := http.Header{"Range": {"bytes=0-998"}}
	for range 4 {
		if w := f.get(keyMedia, "MEDIA1", ranged); w.Code != http.StatusPartialContent {
			t.Fatalf("a delivery within the share = %d", w.Code)
		}
	}
	if w := f.request(http.MethodHead, keyMedia, f.mref(keyMedia, "MEDIA1"), nil); w.Code != http.StatusOK {
		t.Fatalf("HEAD past the share = %d, want no reservation", w.Code)
	}
	w := f.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-4"}})
	requireWriterError(t, w, http.StatusTooManyRequests, codeRateLimited)
	if f.metric(`wawarden_media_deliveries_total{transport="rest",result="over_cap"}`) != "1" {
		t.Fatal("the refused delivery was not counted")
	}
	if w := f.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-3"}}); w.Code != http.StatusPartialContent {
		t.Fatalf("a delivery that fits the share's rest = %d", w.Code)
	}

	g := newMediaFixture(t, mediaOptions{})
	g.attach("MEDIA1", "document", smallSize)
	spool := newFakeSpool(4*deliveryChunk, true)
	v := g.ready(keyMedia, "MEDIA1", spool)
	entered, resume := make(chan struct{}, 2), make(chan struct{})
	spool.hook = func(call int) {
		if call <= 2 {
			entered <- struct{}{}
			<-resume
		}
	}
	results := make(chan *deadlineWriter, 2)
	for range 2 {
		go func() { results <- g.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-9"}}) }()
	}
	<-entered
	<-entered
	requireWriterError(t, g.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-9"}}), http.StatusTooManyRequests, codeRateLimited)
	close(resume)
	for range 2 {
		if w := <-results; w.Code != http.StatusPartialContent {
			t.Fatalf("a concurrent delivery = %d", w.Code)
		}
	}
	if w := g.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-9"}}); w.Code != http.StatusPartialContent {
		t.Fatalf("a delivery once the others finished = %d", w.Code)
	}
	g.m.mu.Lock()
	running, reserved, sent := v.job.running, v.job.reserved, v.job.delivered
	g.m.mu.Unlock()
	if running != 0 || reserved != 0 || sent != 30 {
		t.Fatalf("after the deliveries: %d running, %d reserved, %d delivered", running, reserved, sent)
	}
}

type eraseTrigger struct {
	name    string
	reopens bool
	cause   error
	trigger func(f *mediaFixture)
}

var eraseTriggers = []eraseTrigger{
	{name: "a whole-body delivery", reopens: true, cause: errDeliveryCompleted, trigger: func(f *mediaFixture) {
		if w := f.get(keyMedia, "MEDIA1", nil); w.Code != http.StatusOK {
			f.t.Fatalf("GET = %d", w.Code)
		}
	}},
	{name: "the TTL", reopens: true, cause: errDeliveryExpired, trigger: func(f *mediaFixture) {
		f.clock.advance(mediaRAMLife)
		f.ticks <- testNow
		f.ticks <- testNow
	}},
	{name: "the sweep's drop", reopens: false, cause: errDeliveryDropped, trigger: func(f *mediaFixture) {
		f.withdraw("MEDIA1", true)
		f.ticks <- testNow
		f.ticks <- testNow
	}},
	{name: "a client revoke", reopens: false, cause: errDeliveryDropped, trigger: func(f *mediaFixture) {
		f.revoke("client-media")
		f.ticks <- testNow
		f.ticks <- testNow
	}},
	{name: "shutdown", reopens: false, cause: errDeliveryDropped, trigger: func(f *mediaFixture) {
		f.stop()
		<-f.stopped
	}},
}

func TestEraseUnderAHoldDefersTheClearToTheLastRelease(t *testing.T) {
	for _, c := range eraseTriggers {
		t.Run(c.name, func(t *testing.T) {
			f := newMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", "document", smallSize)
			spool := newFakeSpool(smallSize, true)
			f.ready(keyMedia, "MEDIA1", spool)
			v, first := f.m.lookup(f.key(keyMedia, "MEDIA1"), true)
			_, second := f.m.lookup(f.key(keyMedia, "MEDIA1"), true)
			if first == nil || second == nil || f.holds(v.job) != 2 {
				t.Fatal("a lookup of a ready in-memory job took no hold")
			}
			c.trigger(f)
			if got, h := f.m.lookup(f.key(keyMedia, "MEDIA1"), true); got.state != 0 || h != nil {
				t.Fatalf("a lookup after the erase found state %d", got.state)
			}
			if _, err := (jobReader{m: f.m, job: v.job}).ReadAt(make([]byte, 1), 0); !errors.Is(err, c.cause) {
				t.Fatalf("ReadAt after the erase = %v, want %v", err, c.cause)
			}
			if erasures, intact := spool.state(); erasures != 0 || !intact {
				t.Fatalf("the spool was cleared under a hold: %d erasures, intact %v", erasures, intact)
			}
			if _, staged := f.slots(); staged != 1 {
				t.Fatalf("%d staged slots while the hold is out, want the erased job's", staged)
			}
			first.release()
			first.release()
			if erasures, intact := spool.state(); erasures != 0 || !intact {
				t.Fatal("a release that was not the last cleared the spool")
			}
			second.release()
			if erasures, intact := spool.state(); erasures != 1 || intact {
				t.Fatalf("the last release left %d erasures", erasures)
			}
			if _, staged := f.slots(); staged != 0 || f.holds(v.job) != 0 {
				t.Fatalf("%d staged slots and %d holds after the last release", staged, f.holds(v.job))
			}
			w := f.post(keyMedia, "MEDIA1")
			switch {
			case c.reopens && w.Code != http.StatusAccepted:
				t.Fatalf("a POST after the erase = %d, want a new job", w.Code)
			case c.reopens:
				f.fetches(2)
			case w.Code == http.StatusAccepted:
				t.Fatalf("a POST after %s started a job", c.name)
			}
		})
	}
}

func TestADiskSpoolIsErasedAtOnce(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("DISK1", "document", diskSize)
	spool := newFakeSpool(smallSize, false)
	f.ready(keyMedia, "DISK1", spool)
	if _, h := f.m.lookup(f.key(keyMedia, "DISK1"), true); h != nil {
		t.Fatal("a lookup took a hold on a disk spool")
	}
	if w := f.get(keyMedia, "DISK1", nil); w.Code != http.StatusOK {
		t.Fatalf("GET = %d", w.Code)
	}
	if erasures, _ := spool.state(); erasures != 1 {
		t.Fatalf("the disk spool was erased %d times", erasures)
	}
	if _, staged := f.slots(); staged != 0 {
		t.Fatalf("%d staged slots after a disk erase", staged)
	}
}

func TestAHoldReleasedOnEveryPathLeavesNoCount(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	spool := newFakeSpool(1000, true)
	v := f.ready(keyMedia, "MEDIA1", spool)
	key := f.key(keyMedia, "MEDIA1")
	busy := func() error {
		_, h := f.m.lookup(key, true)
		defer h.release()
		return errBusy
	}
	overCap := func() error {
		w, h := f.m.lookup(key, true)
		defer h.release()
		_, err := f.m.startDelivery(w.job, transportREST, 4*1000+1)
		return err
	}
	failed := func() error {
		w, h := f.m.lookup(key, true)
		defer h.release()
		_, err := dto.NewAttachment(w.kind, "not-a-variant", w.servedType, w.size, w.tag, jobReader{m: f.m, job: w.job}, w.expires, nil)
		return err
	}
	for name, path := range map[string]func() error{"busy": busy, "over a cap": overCap, "an error": failed} {
		if err := path(); err == nil {
			t.Fatalf("the %s path succeeded", name)
		}
		if got := f.holds(v.job); got != 0 {
			t.Fatalf("the %s path left %d holds", name, got)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the publish did not panic")
			}
		}()
		_, h := f.m.lookup(key, true)
		defer h.release()
		f.m.drop(key)
		panic(secretPanic{text: panicCanary})
	}()
	if erasures, _ := spool.state(); erasures != 1 || f.holds(v.job) != 0 {
		t.Fatalf("a panic between the lookup and the publish left %d holds and %d erasures", f.holds(v.job), erasures)
	}
	if _, staged := f.slots(); staged != 0 {
		t.Fatalf("%d staged slots after the panic", staged)
	}
}

func TestAnOrphanedHoldDropsTheSpoolWithoutClearingIt(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	spool := newFakeSpool(smallSize, true)
	f.ready(keyMedia, "MEDIA1", spool)
	key := f.key(keyMedia, "MEDIA1")
	_, transient := f.m.lookup(key, true)
	_, orphaned := f.m.lookup(key, true)
	f.m.drop(key)
	orphaned.orphan()
	transient.release()
	if erasures, intact := spool.state(); erasures != 0 || !intact {
		t.Fatalf("an orphaned spool was cleared: %d erasures", erasures)
	}
	if _, staged := f.slots(); staged != 0 {
		t.Fatalf("%d staged slots after the orphan's drop", staged)
	}
}

func TestHoldsAndErasesInterleaveWithoutLeakingSlots(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	for round := range 50 {
		id := "MEDIA" + strconv.Itoa(round)
		f.attach(id, "document", smallSize)
		spool := newFakeSpool(64, true)
		v := f.ready(keyMedia, id, spool)
		key := f.key(keyMedia, id)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, h := f.m.lookup(key, true)
				if i == 4 {
					f.m.drop(key)
				}
				h.release()
			}()
		}
		wg.Wait()
		if erasures, _ := spool.state(); erasures != 1 || f.holds(v.job) != 0 {
			t.Fatalf("round %d: %d erasures and %d holds", round, erasures, f.holds(v.job))
		}
		if fetching, staged := f.slots(); fetching != 0 || staged != 0 {
			t.Fatalf("round %d: %d fetch and %d staged slots leaked", round, fetching, staged)
		}
		f.clock.advance(10 * time.Second)
	}
}

func TestShutdownEndsFetchesAndErasesJobs(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.attach("MEDIA2", "document", smallSize)
	spool := newFakeSpool(smallSize, true)
	f.ready(keyMedia, "MEDIA1", spool)
	f.post(keyMedia, "MEDIA2")
	f.stop()
	<-f.stopped
	if erasures, _ := spool.state(); erasures != 1 {
		t.Fatalf("shutdown left the ready spool with %d erasures", erasures)
	}
	if fetching, staged := f.slots(); fetching != 0 || staged != 0 {
		t.Fatalf("shutdown left %d fetch and %d staged slots", fetching, staged)
	}
	requireWriterError(t, f.post(keyMedia2, "MEDIA1"), http.StatusServiceUnavailable, codeBusy)
	requireWriterError(t, f.get(keyMedia, "MEDIA1", nil), http.StatusConflict, codeMediaNotRequested)
	if got := f.metric("wawarden_media_jobs"); got != "0" {
		t.Fatalf("wawarden_media_jobs = %q after shutdown", got)
	}
}

func TestMediaAuditRowsNameTheMessageAndItsBytes(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.get(keyMedia, "MEDIA1", nil)
	f.ready(keyMedia, "MEDIA1", newFakeSpool(smallSize, true))
	f.post(keyMedia, "MEDIA1")
	f.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-99"}})
	f.get(keyMedia, "MEDIA1", nil)
	f.request(http.MethodPost, keyMedia, "not-a-sealed-ref", nil)
	row := func(action, reason string, bytes int64) ReadEvent {
		return ReadEvent{At: testNow, Client: "client-media", Action: action, Chat: groupChat, OK: reason == outcomeOK, Reason: reason, Peer: "192.0.2.1",
			Message: syntheticMediaRef, Bytes: bytes}
	}
	want := []ReadEvent{
		row(actionMedia, codeMediaNotRequested, 0),
		row(actionMediaFetch, outcomeOK, smallSize),
		row(actionMediaFetch, outcomeOK, 0),
		row(actionMedia, outcomeOK, 100),
		row(actionMediaSent, outcomeOK, 100),
		row(actionMedia, outcomeOK, smallSize),
		row(actionMediaSent, outcomeOK, smallSize),
		{At: testNow, Client: "client-media", Action: actionMediaFetch, Reason: codeNotFound, Peer: "192.0.2.1"},
	}
	if got := f.audit.recorded(); !slices.Equal(got, want) {
		t.Fatalf("audit = %+v\nwant %+v", got, want)
	}
}

var syntheticMediaRef = cursor.Ref{Chat: groupChat.JID(), ID: "MEDIA1", Sender: aliceChat.JID()}

func TestMediaMetrics(t *testing.T) {
	f := newMediaFixture(t, mediaOptions{})
	for _, result := range fetchResults {
		if got := f.metric(`wawarden_media_fetches_total{result="` + result + `"}`); got != "0" {
			t.Fatalf("fetches{result=%q} = %q at start, want 0", result, got)
		}
	}
	f.attach("MEDIA1", "document", smallSize)
	f.attach("MEDIA2", "document", smallSize)
	f.ready(keyMedia, "MEDIA1", newFakeSpool(smallSize, true))
	f.post(keyMedia, "MEDIA2")
	if got := f.metric("wawarden_media_jobs"); got != "2" {
		t.Fatalf("wawarden_media_jobs = %q, want 2", got)
	}
	f.get(keyMedia, "MEDIA1", http.Header{"Range": {"bytes=0-99"}})
	f.get(keyMedia, "MEDIA1", nil)
	for series, want := range map[string]string{
		`wawarden_media_fetches_total{result="ok"}`:                             "1",
		"wawarden_media_fetched_bytes_total":                                    strconv.Itoa(smallSize),
		`wawarden_media_deliveries_total{transport="rest",result="ok"}`:         "2",
		`wawarden_media_deliveries_total{transport="rest",result="incomplete"}`: "0",
		`wawarden_media_delivered_bytes_total{transport="rest"}`:                strconv.Itoa(smallSize + 100),
		"wawarden_media_jobs": "1",
	} {
		if got := f.metric(series); got != want {
			t.Fatalf("%s = %q, want %q", series, got, want)
		}
	}
}
