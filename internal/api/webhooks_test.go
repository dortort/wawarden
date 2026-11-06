package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/hooks"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	keyGroupHooks = "synthetic-group-webhook-holder"
	keyAliceHooks = "synthetic-alice-webhook-holder"
	syntheticHook = "wh_0123456789abcdef"
)

var (
	hookNames      = []string{toolCreateWebhook, toolDeleteWebhook, toolGetChanges, toolGetChat, toolGetMessages, toolListChats, toolListWebhooks, toolSearchMessages}
	hookWriteNames = []string{toolCreateWebhook, toolDeleteWebhook, toolGetChanges, toolGetChat, toolGetMessages, toolListChats, toolListWebhooks, toolSearchMessages, toolSendMessage}
)

type fakeWebhooks struct {
	mu    sync.Mutex
	hooks map[string]WebhookView
	owner map[string]string
	specs []hooks.Spec
	err   error
}

func newFakeWebhooks() *fakeWebhooks {
	return &fakeWebhooks{hooks: map[string]WebhookView{}, owner: map[string]string{}}
}

func (f *fakeWebhooks) Create(_ context.Context, client string, spec hooks.Spec) (WebhookView, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.err != nil {
		return WebhookView{}, "", f.err
	}
	spec, err := hooks.Normalize(spec)
	if err != nil {
		return WebhookView{}, "", err
	}
	v := WebhookView{ID: syntheticHook, Spec: spec, CreatedAt: testNow}
	f.hooks[v.ID], f.owner[v.ID] = v, client
	return v, strings.Repeat("ab", 32), nil
}

func (f *fakeWebhooks) List(_ context.Context, client string) ([]WebhookView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []WebhookView
	for id, v := range f.hooks {
		if f.owner[id] == client {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeWebhooks) Get(_ context.Context, client, id string) (WebhookView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.hooks[id]
	if !ok || f.owner[id] != client {
		return WebhookView{}, ErrWebhookNotFound
	}
	return v, nil
}

func (f *fakeWebhooks) Delete(_ context.Context, client, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.hooks[id]; !ok || f.owner[id] != client {
		return ErrWebhookNotFound
	}
	delete(f.hooks, id)
	return nil
}

func withWebhooks(c *policy.Client) *policy.Client {
	m := *c
	m.ID, m.AllowWebhooks = c.ID+"-hooks", true
	return &m
}

type hookFixture struct {
	*readFixture
	service *fakeWebhooks
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	f := newMCPFixture(t)
	f.clients[keyGroupHooks], f.clients[keyAliceHooks] = withWebhooks(f.clients[keyGroup]), withWebhooks(f.clients[keyAlice])
	svc := newFakeWebhooks()
	deps := testClientDeps(t, f.clients, f.clock.now)
	deps.Archive, deps.Audit, deps.Cursors, deps.Refs, deps.Sender, deps.Webhooks = f.archive, f.audit, f.cursors, f.refs, f.sender, svc
	f.handler = NewClientHandler(deps)
	return &hookFixture{readFixture: f, service: svc}
}

func (f *hookFixture) send(t *testing.T, key, method, target, body string) *http.Request {
	t.Helper()
	r := newRequest(t, method, target, bearer(key))
	if body != "" {
		r.Body = readCloser{bytes.NewReader([]byte(body))}
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", jsonType)
	}
	return r
}

func TestWebhookRoutesCreateListGetAndDelete(t *testing.T) {
	f := newHookFixture(t)
	body := `{"url":"https://hooks.example.com/in?x=1","method":"put","content_type":"application/json",` +
		`"headers":[{"name":"authorization","value":"Bearer synthetic"}],"payload":"{\"text\":\"{{message.text}}\",\"msg\":{{message.kind}}}",` +
		`"filter":{"chats":["` + groupRef + `"],"direction":"inbound","kinds":["text"],"contains":"Invoice"}}`
	rec := serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a template that is not JSON = %d %s, want 400", rec.Code, rec.Body)
	}
	body = strings.Replace(body, `{{message.kind}}`, `\"{{message.kind}}\"`, 1)
	rec = serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", rec.Code, rec.Body)
	}
	var created struct {
		Webhook map[string]any `json:"webhook"`
		Secret  string         `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Secret != strings.Repeat("ab", 32) {
		t.Fatalf("create answer %s", rec.Body)
	}
	if created.Webhook["method"] != "PUT" || fmt.Sprint(created.Webhook["header_names"]) != "[Authorization]" || strings.Contains(rec.Body.String(), "Bearer synthetic") {
		t.Fatalf("create answer %s: want the normalised method, the header's name and never its value", rec.Body)
	}
	got := f.service.specs[len(f.service.specs)-1]
	if got.Headers["authorization"] != "Bearer synthetic" || got.Filter.Direction != "inbound" || got.Filter.Contains != "Invoice" || got.Method != "put" {
		t.Fatalf("the service got %+v", got)
	}

	rec = serve(f.handler, f.send(t, keyGroupHooks, http.MethodGet, "/v1/webhooks", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"`+syntheticHook+`"`) || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	rec = serve(f.handler, f.send(t, keyGroupHooks, http.MethodGet, "/v1/webhooks/"+syntheticHook, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"url":"https://hooks.example.com/in?x=1"`) {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	rec = serve(f.handler, f.send(t, keyAliceHooks, http.MethodGet, "/v1/webhooks/"+syntheticHook, ""))
	requireError(t, rec, http.StatusNotFound, codeNotFound)
	rec = serve(f.handler, f.send(t, keyAliceHooks, http.MethodDelete, "/v1/webhooks/"+syntheticHook, ""))
	requireError(t, rec, http.StatusNotFound, codeNotFound)
	rec = serve(f.handler, f.send(t, keyGroupHooks, http.MethodDelete, "/v1/webhooks/"+syntheticHook, ""))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"id":"`+syntheticHook+`","deleted":true}` {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	rec = serve(f.handler, f.send(t, keyGroupHooks, http.MethodDelete, "/v1/webhooks/"+syntheticHook, ""))
	requireError(t, rec, http.StatusNotFound, codeNotFound)
}

func TestWebhookRoutesAreHiddenFromAClientWithoutTheFlag(t *testing.T) {
	f := newHookFixture(t)
	for _, r := range []*http.Request{
		f.send(t, keyGroup, http.MethodPost, "/v1/webhooks", `{"url":"https://hooks.example.com/in"}`),
		f.send(t, keyGroup, http.MethodGet, "/v1/webhooks", ""),
		f.send(t, keyGroup, http.MethodGet, "/v1/webhooks/"+syntheticHook, ""),
		f.send(t, keyGroup, http.MethodDelete, "/v1/webhooks/"+syntheticHook, ""),
		f.send(t, "no-such-key", http.MethodGet, "/v1/webhooks", ""),
	} {
		rec := serve(f.handler, r)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d %s, want a refusal", r.Method, r.URL, rec.Code, rec.Body)
		}
	}
	if len(f.service.specs) != 0 {
		t.Fatalf("the service was called %d times for a client without allow_webhooks", len(f.service.specs))
	}
}

func TestWebhookRoutesAnswerTheRefusalCode(t *testing.T) {
	f := newHookFixture(t)
	for _, tt := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"url":"http://hooks.example.com/in"}`, http.StatusBadRequest, "webhook_url_invalid"},
		{`{"url":"https://hooks.example.com/in","method":"GET"}`, http.StatusBadRequest, "webhook_method_invalid"},
		{`{"url":"https://hooks.example.com/in","content_type":"text/html"}`, http.StatusBadRequest, "webhook_content_type_invalid"},
		{`{"url":"https://hooks.example.com/in","headers":[{"name":"Host","value":"x"}]}`, http.StatusBadRequest, "webhook_header_invalid"},
		{`{"url":"https://hooks.example.com/in","headers":[{"name":"X-A","value":"1"},{"name":"X-A","value":"2"}]}`, http.StatusBadRequest, "webhook_header_invalid"},
		{`{"url":"https://hooks.example.com/in","payload":"{{message.secret}}","content_type":"text/plain"}`, http.StatusBadRequest, "webhook_payload_invalid"},
		{`{"url":"https://hooks.example.com/in","filter":{"kinds":["sticker"]}}`, http.StatusBadRequest, "webhook_filter_invalid"},
		{`{"url":"https://hooks.example.com/in","filter":{"direction":"sideways"}}`, http.StatusBadRequest, "webhook_filter_invalid"},
		{`{"url":"https://hooks.example.com/in","unknown":true}`, http.StatusBadRequest, codeInvalidBody},
	} {
		rec := serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", tt.body))
		requireError(t, rec, tt.status, tt.code)
	}
	f.service.err = hooks.ErrTooMany
	rec := serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", `{"url":"https://hooks.example.com/in"}`))
	requireError(t, rec, http.StatusConflict, codeWebhookLimit)
	f.service.err = errors.New("synthetic store failure")
	rec = serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", `{"url":"https://hooks.example.com/in"}`))
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
}

func TestWebhookRoutesAreAudited(t *testing.T) {
	f := newHookFixture(t)
	serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", `{"url":"https://hooks.example.com/in"}`))
	serve(f.handler, f.send(t, keyGroupHooks, http.MethodPost, "/v1/webhooks", `{"url":"ftp://x"}`))
	serve(f.handler, f.send(t, keyGroupHooks, http.MethodGet, "/v1/webhooks", ""))
	serve(f.handler, f.send(t, keyGroupHooks, http.MethodDelete, "/v1/webhooks/"+syntheticHook, ""))
	var got []string
	for _, e := range f.audit.recorded() {
		got = append(got, e.Action+"/"+e.Reason)
	}
	want := []string{actionWebhookCreate + "/ok", actionWebhookCreate + "/webhook_url_invalid", actionWebhooks + "/ok", actionWebhookDelete + "/ok"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("audited %q, want %q", got, want)
	}
}

func TestWebhookToolsCreateListAndDelete(t *testing.T) {
	f := newHookFixture(t)
	for _, p := range protocols {
		cs := connect(t, f.handler, keyGroupHooks, p)
		res := call(t, cs, toolCreateWebhook, map[string]any{
			"url": "https://hooks.example.com/in", "content_type": "text/plain", "payload": "New message in {{message.chat}}: {{message.text}}",
			"headers": []map[string]any{{"name": "X-Api-Key", "value": "synthetic"}}, "filter": map[string]any{"direction": "inbound"},
		})
		if res.IsError || !strings.Contains(string(structured(t, res)), `"secret":"`) {
			t.Fatalf("%s: create_webhook = %+v", p.name, res)
		}
		res = call(t, cs, toolListWebhooks, nil)
		if res.IsError || !strings.Contains(string(structured(t, res)), `"header_names":["X-Api-Key"]`) || strings.Contains(string(structured(t, res)), "synthetic\"") {
			t.Fatalf("%s: list_webhooks = %s", p.name, structured(t, res))
		}
		res = call(t, cs, toolCreateWebhook, map[string]any{"url": "https://hooks.example.com/in", "method": "GET"})
		if !res.IsError {
			t.Fatalf("%s: create_webhook with GET = %+v, want an error", p.name, res)
		}
		res = call(t, cs, toolDeleteWebhook, map[string]any{"webhook": syntheticHook})
		if res.IsError || string(structured(t, res)) != `{"deleted":true,"id":"`+syntheticHook+`"}` {
			t.Fatalf("%s: delete_webhook = %+v", p.name, res)
		}
		res = call(t, cs, toolDeleteWebhook, map[string]any{"webhook": syntheticHook})
		if !res.IsError {
			t.Fatalf("%s: a second delete_webhook = %+v, want not_found", p.name, res)
		}
	}
}

func TestWebhookToolListsMatchTheGoldenFiles(t *testing.T) {
	f := newHookFixture(t)
	validName := regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	for _, p := range protocols {
		for class, c := range map[string]struct {
			key   string
			names []string
		}{
			"read_chats_webhooks": {keyGroupHooks, hookNames}, "write_chats_webhooks": {keyAliceHooks, hookWriteNames},
			"read_chats": {keyGroup, toolNames}, "write_chats": {keyAlice, writeNames},
		} {
			t.Run(p.name+"/"+class, func(t *testing.T) {
				toolListMatchesTheGoldenFile(t, f.handler, p, class, c.key, c.names, validName)
			})
		}
	}
}

func TestWebhookFeedReadsUnderTheClientsOwnGrant(t *testing.T) {
	f := newReadFixture(t)
	for i := range f.archive.messages {
		f.archive.messages[i].Live = f.archive.messages[i].Position.Seq%2 == 0
	}
	feed := NewWebhookFeed(FeedDeps{Archive: f.archive, Refs: f.refs, Now: f.clock.now})
	holder := withWebhooks(readClient("client-group", groupChat))
	page, ok, err := feed.Next(t.Context(), holder, 0, 4)
	if err != nil || !ok || len(page.Items) != 4 || !page.More || page.Next != 4 {
		t.Fatalf("Next = %+v, %v, %v", page, ok, err)
	}
	for _, item := range page.Items {
		if item.Message.Chat != groupRef || item.Live != (item.Seq%2 == 0) || !strings.HasPrefix(item.Message.Ref, "m1_") || item.Seq != item.Change {
			t.Fatalf("item %+v", item)
		}
	}
	page, ok, err = feed.Next(t.Context(), holder, 4, 50)
	if err != nil || !ok || len(page.Items) != 6 || page.More {
		t.Fatalf("Next after 4 = %d items, %v, %v", len(page.Items), ok, err)
	}
	for name, c := range map[string]*policy.Client{
		"without the flag": readClient("client-group", groupChat),
		"revoked":          func() *policy.Client { c := withWebhooks(readClient("x", groupChat)); c.Revoked = true; return c }(),
		"expired": func() *policy.Client {
			c := withWebhooks(readClient("x", groupChat))
			c.ExpiresAt = testNow.Add(-time.Second)
			return c
		}(),
		"nil": nil,
	} {
		if page, ok, err := feed.Next(t.Context(), c, 0, 50); ok || err != nil || len(page.Items) != 0 {
			t.Fatalf("Next for a client %s = %+v, %v, %v, want nothing", name, page, ok, err)
		}
	}
}
