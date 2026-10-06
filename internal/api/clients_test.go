package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

func mustChat(t *testing.T, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q)", jid)
	}
	return c
}

func sampleView(t *testing.T) ClientView {
	return ClientView{
		ID: "aaaqeaye", Name: "agent", State: "active",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2027, 1, 4, 12, 0, 0, 0, time.UTC),
		ReadCount: 2, WriteCount: 1,
		Read: []ClientChatView{
			{Chat: mustChat(t, "120363000000000001@g.us"), Known: true, Name: "Synthetic Group"},
			{Chat: mustChat(t, "100000000000001@lid")},
		},
		Write: []ClientChatView{{Chat: mustChat(t, "120363000000000001@g.us"), Known: true, Name: "Synthetic Group"}},
	}
}

const sampleClientJSON = `{"id":"aaaqeaye","name":"agent","state":"active","created_at":"2026-10-06T12:00:00Z","expires_at":"2027-01-04T12:00:00Z",` +
	`"revoked_at":null,"all_chats":false,"allow_first_contact":false,` +
	`"read_chats":[{"id":"120363000000000001@g.us","kind":"group","known":true,"name":"Synthetic Group"},{"id":"100000000000001@lid","kind":"lid","known":false,"name":null}],` +
	`"write_chats":[{"id":"120363000000000001@g.us","kind":"group","known":true,"name":"Synthetic Group"}]}`

func TestCreateClientAnswersTheViewAndTheCredentialOnce(t *testing.T) {
	f := newAdminFixture(t)
	_, credential := token.NewClient()
	f.service.view, f.service.credential = sampleView(t), credential
	body := `{"name":"agent","read_chats":["120363000000000001@g.us","15550100001@c.us"],"write_chats":["120363000000000001@g.us"],` +
		`"allow_first_contact":true,"expires_in_days":30}`
	rec := adminRequest(t, f, http.MethodPost, "/admin/v1/clients", jsonType, body)
	if want := `{"client":` + sampleClientJSON + `,"credential":"` + credential + `"}`; rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("create = %d %s, want 200 %s", rec.Code, rec.Body, want)
	}
	if n := strings.Count(rec.Body.String(), credential); n != 1 {
		t.Fatalf("the create response holds the credential %d times", n)
	}
	want := policy.ClientSpec{Name: "agent", Read: []string{"120363000000000001@g.us", "15550100001@c.us"}, Write: []string{"120363000000000001@g.us"}, AllowFirstContact: true, ExpiresInDays: 30}
	if got := f.service.spec; got.Name != want.Name || !slices.Equal(got.Read, want.Read) || !slices.Equal(got.Write, want.Write) || got.AllowFirstContact != want.AllowFirstContact || got.ExpiresInDays != want.ExpiresInDays || got.AllChats {
		t.Fatalf("the service got %+v, want %+v", got, want)
	}
	if got := f.events.recorded(); !slices.Equal(got, [][2]string{{"client_create", "ok"}}) {
		t.Fatalf("events %v, want one client_create ok", got)
	}
	for _, path := range []string{"/admin/v1/clients", "/admin/v1/clients/aaaqeaye"} {
		if rec := adminRequest(t, f, http.MethodGet, path, "", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), credential) || strings.Contains(rec.Body.String(), "credential") {
			t.Fatalf("GET %s = %d %s: only the create response carries the credential", path, rec.Code, rec.Body)
		}
	}
}

func TestCreateClientRefusalsAreFixed(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{err: policy.ErrNameInvalid, status: http.StatusUnprocessableEntity, code: "name_invalid"},
		{err: policy.ErrExpiryOutOfRange, status: http.StatusUnprocessableEntity, code: "expiry_out_of_range"},
		{err: policy.ErrReadScopeMissing, status: http.StatusUnprocessableEntity, code: "read_scope_missing"},
		{err: policy.ErrReadScopeConflict, status: http.StatusUnprocessableEntity, code: "read_scope_conflict"},
		{err: policy.ErrAllChatsWithWrite, status: http.StatusUnprocessableEntity, code: "all_chats_with_write"},
		{err: policy.ErrChatInvalid, status: http.StatusUnprocessableEntity, code: "chat_invalid"},
		{err: policy.ErrTooManyChats, status: http.StatusUnprocessableEntity, code: "too_many_chats"},
		{err: policy.ErrWriteNotReadable, status: http.StatusUnprocessableEntity, code: "write_not_readable"},
		{err: policy.ErrWriteChatUnknown, status: http.StatusUnprocessableEntity, code: "write_chat_unknown"},
		{err: policy.ErrNameTaken, status: http.StatusConflict, code: "name_taken"},
		{err: fmt.Errorf("wrapped: %w", policy.ErrNameTaken), status: http.StatusConflict, code: "name_taken"},
		{err: &policy.SpecError{}, status: http.StatusInternalServerError, code: codeInternal},
		{err: errors.New("synthetic store failure with 15550100001@s.whatsapp.net"), status: http.StatusInternalServerError, code: codeInternal},
	}
	if len(clientRefusals) != 10 {
		t.Fatalf("%d client refusals mapped, want the ten the policy declares", len(clientRefusals))
	}
	for _, tt := range tests {
		t.Run(tt.code+" "+tt.err.Error(), func(t *testing.T) {
			f := newAdminFixture(t)
			f.service.clientErr = tt.err
			requireError(t, adminRequest(t, f, http.MethodPost, "/admin/v1/clients", jsonType, `{"name":"agent","all_chats":true}`), tt.status, tt.code)
			if got := f.events.recorded(); !slices.Equal(got, [][2]string{{"client_create", tt.code}}) {
				t.Fatalf("events %v, want client_create %s", got, tt.code)
			}
		})
	}
}

func TestCreateClientBodies(t *testing.T) {
	chats := func(n int) string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf(`"120363%018d@g.us"`, i)
		}
		return "[" + strings.Join(ids, ",") + "]"
	}
	full := `{"name":"agent","read_chats":` + chats(256) + `,"write_chats":` + chats(256) + `}`
	if len(full) <= maxBodyBytes || len(full) > maxClientBodyBytes {
		t.Fatalf("a create with two full sets is %d bytes, want between the default cap and the create cap", len(full))
	}
	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{name: "two full chat sets", body: full, status: http.StatusOK},
		{name: "unknown field", body: `{"name":"agent","token":"x"}`, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "expiry as text", body: `{"name":"agent","expires_in_days":"90"}`, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "over the create cap", body: `{"name":"` + strings.Repeat("a", maxClientBodyBytes) + `"}`, status: http.StatusRequestEntityTooLarge, code: codeBodyTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdminFixture(t)
			f.service.view = sampleView(t)
			rec := adminRequest(t, f, http.MethodPost, "/admin/v1/clients", jsonType, tt.body)
			if tt.status == http.StatusOK {
				if rec.Code != http.StatusOK || len(f.service.spec.Read) != 256 || len(f.service.spec.Write) != 256 {
					t.Fatalf("create = %d %s with %d and %d chats", rec.Code, rec.Body, len(f.service.spec.Read), len(f.service.spec.Write))
				}
				return
			}
			requireError(t, rec, tt.status, tt.code)
			if calls := f.service.called(); len(calls) != 0 {
				t.Fatalf("the service was called %v for a refused body", calls)
			}
		})
	}
}

func TestClientReadsAndRevoke(t *testing.T) {
	f := newAdminFixture(t)
	f.service.view = sampleView(t)
	rec := adminRequest(t, f, http.MethodGet, "/admin/v1/clients", "", "")
	want := `{"clients":[{"id":"aaaqeaye","name":"agent","state":"active","created_at":"2026-10-06T12:00:00Z","expires_at":"2027-01-04T12:00:00Z",` +
		`"revoked_at":null,"all_chats":false,"allow_first_contact":false,"read_chat_count":2,"write_chat_count":1}]}`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("list = %d %s, want %s", rec.Code, rec.Body, want)
	}
	if rec := adminRequest(t, f, http.MethodGet, "/admin/v1/clients/aaaqeaye", "", ""); rec.Code != http.StatusOK || rec.Body.String() != sampleClientJSON || f.service.clientID != "aaaqeaye" {
		t.Fatalf("show = %d %s for %q", rec.Code, rec.Body, f.service.clientID)
	}
	revoked := sampleView(t)
	revoked.State, revoked.RevokedAt = "revoked", time.Date(2026, 10, 7, 8, 0, 0, 0, time.FixedZone("synthetic", 7200))
	f.service.view = revoked
	rec = adminRequest(t, f, http.MethodPost, "/admin/v1/clients/aaaqeaye/revoke", jsonType, "{}")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"revoked"`) || !strings.Contains(rec.Body.String(), `"revoked_at":"2026-10-07T06:00:00Z"`) {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	requireError(t, adminRequest(t, f, http.MethodPost, "/admin/v1/clients/aaaqeaye/revoke", jsonType, `{"now":true}`), http.StatusBadRequest, codeInvalidBody)
	f.service.clientErr = ErrClientNotFound
	requireError(t, adminRequest(t, f, http.MethodGet, "/admin/v1/clients/zzzzzzzz", "", ""), http.StatusNotFound, codeNotFound)
	requireError(t, adminRequest(t, f, http.MethodPost, "/admin/v1/clients/zzzzzzzz/revoke", jsonType, "{}"), http.StatusNotFound, codeNotFound)
	want2 := [][2]string{{"client_revoke", "ok"}, {"client_revoke", codeInvalidBody}, {"client_revoke", codeNotFound}}
	if got := f.events.recorded(); !slices.Equal(got, want2) {
		t.Fatalf("events %v, want %v: reads emit nothing", got, want2)
	}
	for _, tt := range []struct{ method, path string }{
		{http.MethodDelete, "/admin/v1/clients/aaaqeaye"},
		{http.MethodGet, "/admin/v1/clients/aaaqeaye/revoke"},
		{http.MethodPut, "/admin/v1/clients"},
		{http.MethodPost, "/admin/v1/chats"},
	} {
		requireError(t, adminRequest(t, f, tt.method, tt.path, jsonType, "{}"), http.StatusMethodNotAllowed, codeMethodNotAllowed)
	}
}

func TestChatListing(t *testing.T) {
	f := newAdminFixture(t)
	f.service.chats = []ChatEntry{
		{Chat: mustChat(t, "120363000000000001@g.us"), Ref: strings.Repeat("a", 32), Name: "Synthetic Group"},
		{Chat: mustChat(t, "15550100001@s.whatsapp.net"), Ref: strings.Repeat("b", 32)},
	}
	f.service.truncated = true
	rec := adminRequest(t, f, http.MethodGet, "/admin/v1/chats?match=Synth%C3%A9tic+Group", "", "")
	want := `{"chats":[{"id":"120363000000000001@g.us","kind":"group","ref":"` + strings.Repeat("a", 32) + `","name":"Synthetic Group"},` +
		`{"id":"15550100001@s.whatsapp.net","kind":"phone","ref":"` + strings.Repeat("b", 32) + `","name":null}],"truncated":true}`
	if rec.Code != http.StatusOK || rec.Body.String() != want || f.service.match != "Synthétic Group" || f.service.limit != chatListLimit {
		t.Fatalf("chats = %d %s with match %q and limit %d", rec.Code, rec.Body, f.service.match, f.service.limit)
	}
	if rec := adminRequest(t, f, http.MethodGet, "/admin/v1/chats", "", ""); rec.Code != http.StatusOK || f.service.match != "" {
		t.Fatalf("chats without a match = %d, match %q", rec.Code, f.service.match)
	}
	for _, query := range []string{
		"other=1", "match=a&match=b", "match=%zz", "match=" + strings.Repeat("a", 65), "match=a%0Ab", "match=%FF", "match=a;b",
	} {
		t.Run(query, func(t *testing.T) {
			requireError(t, adminRequest(t, f, http.MethodGet, "/admin/v1/chats?"+query, "", ""), http.StatusBadRequest, codeInvalidQuery)
		})
	}
	if got := f.events.recorded(); len(got) != 0 {
		t.Fatalf("listing chats emitted %v", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body) != 2 {
		t.Fatalf("chats body %s", rec.Body)
	}
}
