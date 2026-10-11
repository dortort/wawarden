package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func TestBackfillAccepts202WithTheRequestID(t *testing.T) {
	f := newAdminFixture(t)
	f.service.backfillID = "3EB0SYNTHETIC"
	f.service.chats = []ChatEntry{{Chat: groupChat, Ref: groupRef}, {Chat: aliceChat, Ref: groupRef[:31] + "2"}}
	for _, tt := range []struct {
		body  string
		chat  policy.CanonicalChat
		count int
	}{
		{body: `{"chat":"120363000000000001@g.us"}`, chat: groupChat, count: 50},
		{body: `{"chat":"` + groupRef + `","count":1}`, chat: groupChat, count: 1},
		{body: `{"chat":"15550100001@s.whatsapp.net","count":50}`, chat: aliceChat, count: 50},
	} {
		rec := adminRequest(t, f, http.MethodPost, "/admin/v1/backfill", "application/json", tt.body)
		if rec.Code != http.StatusAccepted || rec.Body.String() != `{"request_id":"3EB0SYNTHETIC"}` {
			t.Fatalf("%s = %d %s", tt.body, rec.Code, rec.Body.String())
		}
		f.service.mu.Lock()
		chat, count, granted, budget := f.service.backfillChat, f.service.backfillCount, f.service.backfillGrant, f.service.backfillBudget
		f.service.mu.Unlock()
		if chat != tt.chat || count != tt.count || !granted || budget <= 20*time.Second || budget > sendDeadline {
			t.Fatalf("%s reached the service as %v, %d, granted %v, %v left", tt.body, chat, count, granted, budget)
		}
	}
	for _, m := range f.events.recorded() {
		if m != [2]string{actionBackfill, outcomeOK} {
			t.Fatalf("admin_mutation = %v", m)
		}
	}
}

func TestBackfillRefusalsAreFixed(t *testing.T) {
	for _, tt := range []struct {
		name       string
		body       string
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{name: "no count", body: `{"chat":"120363000000000001@g.us","count":0}`, status: 400, code: codeInvalidBody},
		{name: "count over 50", body: `{"chat":"120363000000000001@g.us","count":51}`, status: 400, code: codeInvalidBody},
		{name: "a chat that is neither", body: `{"chat":"Synthetic Group"}`, status: 400, code: codeInvalidBody},
		{name: "an upper-case reference", body: `{"chat":"ABCDEF00000000000000000000000000"}`, status: 400, code: codeInvalidBody},
		{name: "an unknown reference", body: `{"chat":"` + noRef + `"}`, status: 404, code: codeNotFound},
		{name: "an unknown field", body: `{"chat":"120363000000000001@g.us","since":1}`, status: 400, code: codeInvalidBody},
		{name: "the quota", body: `{"chat":"120363000000000001@g.us"}`, err: BackfillQuota(4*time.Minute + 10*time.Second), status: 429, code: codeBackfillQuota, retryAfter: "250"},
		{name: "the daily quota", body: `{"chat":"120363000000000001@g.us"}`, err: BackfillQuota(20 * time.Hour), status: 429, code: codeBackfillQuota, retryAfter: "72000"},
		{name: "no anchor", body: `{"chat":"120363000000000001@g.us"}`, err: ErrBackfillNoAnchor, status: 422, code: codeBackfillNoAnchor},
		{name: "not connected", body: `{"chat":"120363000000000001@g.us"}`, err: ErrNotConnected, status: 503, code: codeNotConnected, retryAfter: "5"},
		{name: "a send in progress", body: `{"chat":"120363000000000001@g.us"}`, err: RateLimited(time.Second), status: 429, code: codeRateLimited, retryAfter: "1"},
		{name: "rejected", body: `{"chat":"120363000000000001@g.us"}`, err: ErrSendRejected, status: 502, code: codeSendRejected},
		{name: "unknown", body: `{"chat":"120363000000000001@g.us"}`, err: ErrSendOutcomeUnknown, status: 504, code: codeSendOutcomeUnknown},
		{name: "a failure", body: `{"chat":"120363000000000001@g.us"}`, err: errors.New("synthetic failure naming 120363000000000001@g.us"), status: 500, code: codeInternal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdminFixture(t)
			f.service.chats = []ChatEntry{{Chat: groupChat, Ref: groupRef}}
			f.service.backfillErr = tt.err
			rec := adminRequest(t, f, http.MethodPost, "/admin/v1/backfill", "application/json", tt.body)
			requireError(t, rec, tt.status, tt.code)
			if got := rec.Header().Get("Retry-After"); got != tt.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.retryAfter)
			}
			if got := f.events.recorded(); len(got) != 1 || got[0] != [2]string{actionBackfill, tt.code} {
				t.Fatalf("admin_mutation = %v", got)
			}
		})
	}
}
