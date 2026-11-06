package admin_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/hooks"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
)

func hookSpec() hooks.Spec {
	return hooks.Spec{
		URL: "https://hooks.example.com/in", Method: "put", Headers: map[string]string{"authorization": "Bearer synthetic"},
		Payload: `{"text":"{{message.text}}"}`, Filter: hooks.Filter{Direction: hooks.DirectionInbound, Kinds: []string{"text"}},
	}
}

func TestAWebhookStartsAfterTheNewestMessageAndKeepsItsSettings(t *testing.T) {
	f := open(t)
	f.seed(t, groupG, phoneA)
	c, _ := f.create(t, policy.ClientSpec{Name: "agent", Read: []string{groupG}, AllowWebhooks: true})
	if !c.AllowWebhooks {
		t.Fatalf("created %+v, want allow_webhooks", c)
	}
	authed, ok, _ := f.clients.Lookup(t.Context(), c.ID)
	if !ok || authed == nil || !authed.AllowWebhooks {
		t.Fatalf("Lookup = %+v, %v", authed, ok)
	}
	w := f.store.Webhooks()
	view, secret, err := w.Create(t.Context(), c.ID, hookSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(secret) != 32 || !admin.WellFormedWebhookID(view.ID) || view.Client != c.ID || !view.CreatedAt.Equal(start) {
		t.Fatalf("Create = %+v, %d secret bytes", view, len(secret))
	}
	if view.Spec.Method != hooks.MethodPut || view.Spec.Headers["Authorization"] != "Bearer synthetic" || view.Spec.Filter.Direction != hooks.DirectionInbound ||
		view.Spec.Payload != `{"text":"{{message.text}}"}` || view.Spec.ContentType != hooks.ContentJSON {
		t.Fatalf("stored spec %+v", view.Spec)
	}
	all, err := w.Hooks(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatalf("Hooks = %+v, %v", all, err)
	}
	top, err := w.Top(t.Context())
	if err != nil || all[0].High != top || all[0].High != 2 || string(all[0].Secret) != string(secret) {
		t.Fatalf("the webhook starts at %d with top %d (%v), want after the 2 seeded messages", all[0].High, top, err)
	}

	f.clock.advance(time.Minute)
	if err := w.Advance(t.Context(), view.ID, hooks.Progress{High: 9, Delivered: 2, LastDelivery: f.clock.Now()}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := w.Advance(t.Context(), view.ID, hooks.Progress{High: 3, Failed: 1, LastFailure: f.clock.Now(), Failure: hooks.ReasonStatus}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	got, err := w.Get(t.Context(), c.ID, view.ID)
	if err != nil || got.Delivered != 2 || got.Failed != 1 || got.Failure != hooks.ReasonStatus || !got.LastDelivery.Equal(f.clock.Now()) || !got.LastFailure.Equal(f.clock.Now()) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	all, _ = w.Hooks(t.Context())
	if all[0].High != 9 {
		t.Fatalf("an older position moved the webhook back to %d", all[0].High)
	}
}

func TestWebhooksBelongToTheirClient(t *testing.T) {
	f := open(t)
	a, _ := f.create(t, policy.ClientSpec{Name: "a", AllChats: true, AllowWebhooks: true})
	b, _ := f.create(t, policy.ClientSpec{Name: "b", AllChats: true, AllowWebhooks: true})
	w := f.store.Webhooks()
	view, _, err := w.Create(t.Context(), a.ID, hookSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Get(t.Context(), b.ID, view.ID); !errors.Is(err, admin.ErrWebhookNotFound) {
		t.Fatalf("Get by another client = %v", err)
	}
	if err := w.Delete(t.Context(), b.ID, view.ID); !errors.Is(err, admin.ErrWebhookNotFound) {
		t.Fatalf("Delete by another client = %v", err)
	}
	if list, err := w.List(t.Context(), b.ID); err != nil || len(list) != 0 {
		t.Fatalf("List of another client = %+v, %v", list, err)
	}
	if list, err := w.List(t.Context(), a.ID); err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := w.Delete(t.Context(), a.ID, view.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := w.Delete(t.Context(), a.ID, view.ID); !errors.Is(err, admin.ErrWebhookNotFound) {
		t.Fatalf("a second Delete = %v", err)
	}
	if err := w.Advance(t.Context(), view.ID, hooks.Progress{High: 1}); err != nil {
		t.Fatalf("Advance of a deleted webhook: %v", err)
	}
	if all, _ := w.Hooks(t.Context()); len(all) != 0 {
		t.Fatalf("Advance brought a deleted webhook back: %+v", all)
	}
	for _, id := range []string{"", "wh_", "wh_0123456789ABCDEF", strings.Repeat("a", 19)} {
		if _, err := w.Get(t.Context(), a.ID, id); !errors.Is(err, admin.ErrWebhookNotFound) {
			t.Fatalf("Get(%q) = %v", id, err)
		}
	}
}

func TestAClientHoldsAtMostTenWebhooksAndOnlyValidOnes(t *testing.T) {
	f := open(t)
	c, _ := f.create(t, policy.ClientSpec{Name: "agent", AllChats: true, AllowWebhooks: true})
	w := f.store.Webhooks()
	bad := hookSpec()
	bad.URL = "http://hooks.example.com/in"
	if _, _, err := w.Create(t.Context(), c.ID, bad); !errors.Is(err, hooks.ErrURLInvalid) {
		t.Fatalf("Create with plain HTTP = %v", err)
	}
	for i := range hooks.MaxPerClient {
		if _, _, err := w.Create(t.Context(), c.ID, hookSpec()); err != nil {
			t.Fatalf("Create %d: %v", i+1, err)
		}
	}
	if _, _, err := w.Create(t.Context(), c.ID, hookSpec()); !errors.Is(err, hooks.ErrTooMany) {
		t.Fatalf("Create beyond the limit = %v", err)
	}
	if _, _, err := w.Create(t.Context(), "not-an-id", hookSpec()); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("Create for a malformed client = %v", err)
	}
}

func TestTheClientFlagDefaultsOff(t *testing.T) {
	f := open(t)
	c, _ := f.create(t, policy.ClientSpec{Name: "agent", AllChats: true})
	authed, ok, _ := f.clients.Lookup(t.Context(), c.ID)
	if c.AllowWebhooks || !ok || authed.AllowWebhooks {
		t.Fatalf("a client created without the flag allows webhooks: %+v, %+v", c, authed)
	}
	if _, ok, _ := f.clients.Lookup(t.Context(), "zzzzzzzz"); ok {
		t.Fatal("Lookup found an unknown client")
	}
	list, err := f.clients.List(t.Context())
	if err != nil || len(list) != 1 || list[0].AllowWebhooks {
		t.Fatalf("List = %+v, %v", list, err)
	}
}
