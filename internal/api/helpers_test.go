package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func fixedNow() time.Time { return testNow }

func liveClient(id string) *policy.Client {
	return &policy.Client{ID: id, ExpiresAt: testNow.Add(time.Hour)}
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
