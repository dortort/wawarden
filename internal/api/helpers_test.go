package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func fixedNow() time.Time { return testNow }

func liveClient(id string) *policy.Client {
	return &policy.Client{ID: id, ExpiresAt: testNow.Add(24 * time.Hour)}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: testNow} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func installPanicReporter(t *testing.T, reg *metrics.Registry) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	safego.Install(slog.New(slog.NewJSONHandler(logs, nil)), reg)
	return logs
}

func exposition(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func metricValue(t *testing.T, reg *metrics.Registry, series string) string {
	t.Helper()
	for line := range strings.SplitSeq(exposition(t, reg), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

func requireSecurityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v, want Cache-Control: no-store and X-Content-Type-Options: nosniff", h)
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("response carries %s", k)
		}
	}
}

type tripwireBody struct{ t *testing.T }

func (b tripwireBody) Read([]byte) (int, error) {
	b.t.Error("the request body was read")
	return 0, io.ErrUnexpectedEOF
}

func (tripwireBody) Close() error { return nil }

func newRequest(t *testing.T, method, target string, header http.Header) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	r.Body = tripwireBody{t: t}
	for k, values := range header {
		for _, v := range values {
			r.Header.Add(k, v)
		}
	}
	return r
}

var formContentTypes = []string{"application/x-www-form-urlencoded", "multipart/form-data; boundary=synthetic-boundary"}

func newFormRequest(t *testing.T, target, contentType string, header http.Header) *http.Request {
	t.Helper()
	r := newRequest(t, http.MethodPost, target, header)
	r.Header.Set("Content-Type", contentType)
	r.ContentLength = 64
	return r
}

func requireUnreadFormRefused(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	requireError(t, rec, http.StatusUnauthorized, codeUnauthorized)
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if got := rec.Header().Get("Connection"); got != "close" {
		t.Fatalf("Connection = %q, want close: a refused request's announced body is never read", got)
	}
	requireSecurityHeaders(t, rec.Header())
}

func bearer(credential string) http.Header {
	return http.Header{"Authorization": {"Bearer " + credential}}
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	want := `{"error":"` + code + `"}`
	if rec.Code != status || rec.Body.String() != want || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response = %d %q (%q), want %d %q as JSON", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"), status, want)
	}
}

func requireSameResponse(t *testing.T, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != want.Code || !reflect.DeepEqual(got.Header(), want.Header()) || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
		t.Fatalf("response differs from the reference\n got: %d %v %q\nwant: %d %v %q",
			got.Code, got.Header(), got.Body.String(), want.Code, want.Header(), want.Body.String())
	}
}
