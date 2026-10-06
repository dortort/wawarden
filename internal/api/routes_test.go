package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAdmin struct {
	mu           sync.Mutex
	status       AdminStatus
	statusErr    error
	code         string
	pairErr      error
	reconnectErr error
	panics       bool
	calls        []string
	pairBudget   time.Duration
}

func (f *fakeAdmin) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAdmin) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeAdmin) Status(context.Context) (AdminStatus, error) {
	f.record("status")
	return f.status, f.statusErr
}

func (f *fakeAdmin) Pair(ctx context.Context) (string, error) {
	f.record("pair")
	if deadline, ok := ctx.Deadline(); ok {
		f.mu.Lock()
		f.pairBudget = time.Until(deadline)
		f.mu.Unlock()
	}
	if f.panics {
		panic(secretPanic{text: panicCanary})
	}
	return f.code, f.pairErr
}

func (f *fakeAdmin) Reconnect(context.Context) error {
	f.record("reconnect")
	if f.panics {
		panic(secretPanic{text: panicCanary})
	}
	return f.reconnectErr
}

type fakeEvents struct {
	mu        sync.Mutex
	mutations [][2]string
	failures  int
}

func (e *fakeEvents) AdminMutation(action, outcome string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mutations = append(e.mutations, [2]string{action, outcome})
}

func (e *fakeEvents) AdminAuthFailure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures++
}

func (e *fakeEvents) recorded() [][2]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.mutations)
}

func (e *fakeEvents) authFailures() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failures
}

func adminRequest(t *testing.T, f *adminFixture, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.adminSecret)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return serve(f.handler, r)
}

func TestStatusAnswersAFixedShape(t *testing.T) {
	f := newAdminFixture(t)
	f.service.status = AdminStatus{
		State: "disconnected", Reason: "replaced", Paired: true,
		Chats: 3, Messages: 120, BlobsPending: 1, BlobsQuarantined: 2, InboxBacklog: 4, InboxQuarantined: 5,
		LastIngest: time.Date(2026, 10, 5, 9, 30, 15, 123456789, time.FixedZone("synthetic", 3600)), Version: "v0.2.0",
	}
	rec := adminRequest(t, f, http.MethodGet, "/admin/v1/status", "", "")
	want := `{"state":"disconnected","reason":"replaced","paired":true,"counts":{"chats":3,"messages":120,"history_blobs_pending":1,` +
		`"history_blobs_quarantined":2,"inbox_backlog":4,"inbox_quarantined":5},"last_ingest_at":"2026-10-05T08:30:15Z","version":"v0.2.0"}`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("status = %d %s, want 200 %s", rec.Code, rec.Body, want)
	}
	requireSecurityHeaders(t, rec.Header())

	f.service.status = AdminStatus{State: "unpaired", Version: "dev"}
	rec = adminRequest(t, f, http.MethodGet, "/admin/v1/status", "", "")
	if !strings.Contains(rec.Body.String(), `"last_ingest_at":null`) {
		t.Fatalf("status before any ingest = %s, want last_ingest_at null", rec.Body)
	}
	if got := f.events.recorded(); len(got) != 0 {
		t.Fatalf("status emitted admin_mutation events %v: it changes nothing", got)
	}
	requireError(t, adminRequest(t, f, http.MethodPost, "/admin/v1/status", jsonType, "{}"), http.StatusMethodNotAllowed, codeMethodNotAllowed)
	for _, path := range []string{"/admin/v1/pair", "/admin/v1/reconnect"} {
		requireError(t, adminRequest(t, f, http.MethodGet, path, "", ""), http.StatusMethodNotAllowed, codeMethodNotAllowed)
	}
}

func TestStatusFailuresAreFixed(t *testing.T) {
	f := newAdminFixture(t)
	f.service.statusErr = ErrEngineUnavailable
	requireError(t, adminRequest(t, f, http.MethodGet, "/admin/v1/status", "", ""), http.StatusServiceUnavailable, "engine_unavailable")
	f.service.statusErr = errors.New("synthetic failure naming 15550100001@s.whatsapp.net")
	requireError(t, adminRequest(t, f, http.MethodGet, "/admin/v1/status", "", ""), http.StatusInternalServerError, codeInternal)
}

func TestMutationsAnswerWithFixedCodesAndEmitTheirOutcome(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "already paired", err: ErrAlreadyPaired, status: http.StatusConflict, code: "already_paired"},
		{name: "already connected", err: ErrAlreadyConnected, status: http.StatusConflict, code: "already_connected"},
		{name: "not paired", err: ErrNotPaired, status: http.StatusConflict, code: "not_paired"},
		{name: "owner phone missing", err: ErrOwnerPhoneMissing, status: http.StatusConflict, code: "owner_phone_missing"},
		{name: "owner mismatch", err: ErrOwnerMismatch, status: http.StatusConflict, code: "owner_mismatch"},
		{name: "rate limited", err: ErrRateLimited, status: http.StatusTooManyRequests, code: "rate_limited"},
		{name: "pair failed", err: ErrPairFailed, status: http.StatusBadGateway, code: "pair_failed"},
		{name: "engine unavailable", err: ErrEngineUnavailable, status: http.StatusServiceUnavailable, code: "engine_unavailable"},
		{name: "wrapped", err: errors.Join(errors.New("synthetic"), ErrNotPaired), status: http.StatusConflict, code: "not_paired"},
		{name: "unknown", err: errors.New("synthetic failure naming 15550100001@s.whatsapp.net"), status: http.StatusInternalServerError, code: codeInternal},
	}
	for _, action := range []string{actionPair, actionReconnect} {
		for _, tt := range tests {
			t.Run(action+" "+tt.name, func(t *testing.T) {
				f := newAdminFixture(t)
				f.service.pairErr, f.service.reconnectErr = tt.err, tt.err
				rec := adminRequest(t, f, http.MethodPost, "/admin/v1/"+action, jsonType, "{}")
				requireError(t, rec, tt.status, tt.code)
				requireSecurityHeaders(t, rec.Header())
				if got := f.events.recorded(); !slices.Equal(got, [][2]string{{action, tt.code}}) {
					t.Fatalf("admin_mutation events %v, want one %s with outcome %s", got, action, tt.code)
				}
			})
		}
	}
}

func TestPanickingMutationsEmitAnInternalErrorOutcome(t *testing.T) {
	for _, action := range []string{actionPair, actionReconnect} {
		t.Run(action, func(t *testing.T) {
			f := newAdminFixture(t)
			logs := installPanicReporter(t, f.reg)
			f.service.panics = true
			rec := adminRequest(t, f, http.MethodPost, "/admin/v1/"+action, jsonType, "{}")
			requireError(t, rec, http.StatusInternalServerError, codeInternal)
			if got := f.events.recorded(); !slices.Equal(got, [][2]string{{action, codeInternal}}) {
				t.Fatalf("admin_mutation events %v, want one %s with outcome %s", got, action, codeInternal)
			}
			if got := metricValue(t, f.reg, `wawarden_panics_total{name="api.admin"}`); got != "1" {
				t.Fatalf("wawarden_panics_total{name=\"api.admin\"} = %q, want 1", got)
			}
			if out := logs.String(); strings.Contains(out, panicCanary) || strings.Contains(fmt.Sprint(f.events.recorded()), panicCanary) {
				t.Fatalf("the panic value leaked:\n%s", out)
			}
		})
	}
}

func TestPairReturnsTheCodeToTheCallerOnly(t *testing.T) {
	const canary = "CANA-RY42"
	f := newAdminFixture(t)
	logs := installPanicReporter(t, f.reg)
	f.service.code = canary
	rec := adminRequest(t, f, http.MethodPost, "/admin/v1/pair", "application/json; charset=utf-8", " {} ")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"code":"`+canary+`"}` {
		t.Fatalf("pair = %d %s", rec.Code, rec.Body)
	}
	if got := f.events.recorded(); !slices.Equal(got, [][2]string{{actionPair, outcomeOK}}) {
		t.Fatalf("admin_mutation events %v", got)
	}
	f.service.mu.Lock()
	budget := f.service.pairBudget
	f.service.mu.Unlock()
	if budget <= 0 || budget > pairTimeout {
		t.Fatalf("Pair ran with %v left, want a deadline of at most %v so the answer beats the write timeout", budget, pairTimeout)
	}
	for _, out := range []string{exposition(t, f.reg), logs.String(), fmt.Sprint(f.events.recorded()), fmt.Sprint(rec.Header())} {
		if strings.Contains(out, canary) || strings.Contains(out, strings.ReplaceAll(canary, "-", "")) {
			t.Fatalf("the pairing code reached an output other than the response body: %q", out)
		}
	}
}

func TestMutationsTakeAnEmptyObjectOnly(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		status                  int
		code                    string
	}{
		{name: "no content type", body: "{}", status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType},
		{name: "form", contentType: "application/x-www-form-urlencoded", body: "a=1", status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType},
		{name: "empty body", contentType: jsonType, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "a field", contentType: jsonType, body: `{"phone":"+15550100001"}`, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "an array", contentType: jsonType, body: `[]`, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "trailing data", contentType: jsonType, body: `{}{}`, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "too large", contentType: jsonType, body: "{" + strings.Repeat(" ", maxBodyBytes) + "}", status: http.StatusRequestEntityTooLarge, code: codeBodyTooLarge},
	}
	for _, action := range []string{actionPair, actionReconnect} {
		for _, tt := range tests {
			t.Run(action+" "+tt.name, func(t *testing.T) {
				f := newAdminFixture(t)
				rec := adminRequest(t, f, http.MethodPost, "/admin/v1/"+action, tt.contentType, tt.body)
				requireError(t, rec, tt.status, tt.code)
				if calls := f.service.called(); len(calls) != 0 {
					t.Fatalf("the service was called %v for a refused body", calls)
				}
				if got := f.events.recorded(); !slices.Equal(got, [][2]string{{action, tt.code}}) {
					t.Fatalf("admin_mutation events %v, want one %s with outcome %s", got, action, tt.code)
				}
			})
		}
	}
}

func TestReconnectAccepts(t *testing.T) {
	f := newAdminFixture(t)
	rec := adminRequest(t, f, http.MethodPost, "/admin/v1/reconnect", jsonType, "{}")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"accepted"}` {
		t.Fatalf("reconnect = %d %s", rec.Code, rec.Body)
	}
	if got := f.events.recorded(); !slices.Equal(got, [][2]string{{actionReconnect, outcomeOK}}) {
		t.Fatalf("admin_mutation events %v", got)
	}
	if calls := f.service.called(); !slices.Equal(calls, []string{"reconnect"}) {
		t.Fatalf("service calls %v", calls)
	}
}

func TestAdminRoutesNeedTheAdminToken(t *testing.T) {
	f := newAdminFixture(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/admin/v1/status"}, {http.MethodPost, "/admin/v1/pair"}, {http.MethodPost, "/admin/v1/reconnect"},
	} {
		r := newRequest(t, route.method, route.path, bearer("wwadm_synthetic"))
		r.Header.Set("Content-Type", jsonType)
		requireError(t, serve(f.handler, r), http.StatusUnauthorized, codeUnauthorized)
	}
	if calls := f.service.called(); len(calls) != 0 {
		t.Fatalf("the service was called %v without the admin token", calls)
	}
	if got := f.events.recorded(); len(got) != 0 || f.events.authFailures() != 3 {
		t.Fatalf("events %v and %d authentication failures, want no mutation and three failures", got, f.events.authFailures())
	}
}

func TestStatusCarriesNoIdentifier(t *testing.T) {
	f := newAdminFixture(t)
	f.service.status = AdminStatus{State: "connected", Paired: true, Version: "dev"}
	var body map[string]any
	if err := json.Unmarshal(adminRequest(t, f, http.MethodGet, "/admin/v1/status", "", "").Body.Bytes(), &body); err != nil {
		t.Fatalf("status is not JSON: %v", err)
	}
	if keys := slices.Sorted(maps.Keys(body)); !slices.Equal(keys, []string{"counts", "last_ingest_at", "paired", "reason", "state", "version"}) {
		t.Fatalf("status keys %q", keys)
	}
}
