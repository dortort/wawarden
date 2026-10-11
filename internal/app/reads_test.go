package app

import (
	"net/http"
	"testing"

	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/token"
)

func TestSealersUseTheCursorAndMessageReferenceKeys(t *testing.T) {
	master := testMaster(t)
	cursors, refs, err := sealers(master)
	if err != nil {
		t.Fatalf("sealers: %v", err)
	}
	binding := cursor.Binding{Client: "aaaaaaaa", Endpoint: "chats"}
	sealedCursor, err := cursors.Cursor(binding, cursor.Position{1, 2, 3})
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	sealedRef, err := refs.SealRef("aaaaaaaa", cursor.Ref{Chat: "120363000000000001@g.us", ID: "SYNTHETIC", Sender: "15550100001@s.whatsapp.net"})
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	if text, err := cursors.SealRef("aaaaaaaa", cursor.Ref{Chat: "120363000000000001@g.us", ID: "SYNTHETIC", Sender: "15550100001@s.whatsapp.net"}); err == nil || text != "" {
		t.Fatalf("the cursor sealer sealed a message reference: %q, %v", text, err)
	}
	purposes := map[string][]byte{
		"log redact":  master.LogRedactKey(),
		"chat hmac":   master.ChatHMACKey(),
		"cursor seal": master.CursorSealKey(),
		"message ref": master.MessageRefKey(),
		"audit chain": master.AuditChainKey(),
	}
	for name, key := range purposes {
		s, err := cursor.New(key, master.ID())
		if err != nil {
			t.Fatalf("cursor.New(%s): %v", name, err)
		}
		if _, err := s.OpenCursor(binding, sealedCursor); (err == nil) != (name == "cursor seal") {
			t.Errorf("the %s key opens the cursor: %v", name, err == nil)
		}
		if _, err := s.OpenRef("aaaaaaaa", sealedRef); (err == nil) != (name == "message ref") {
			t.Errorf("the %s key opens the message reference: %v", name, err == nil)
		}
	}
}

func TestLiftedRateCapsAreAnnounced(t *testing.T) {
	cfg := testConfig(t, token.NewAdmin())
	a, logs := open(t, cfg, noClients{})
	_ = a.closeStores()
	if got := logs.find("unsafe_rate_caps"); len(got) != 0 {
		t.Fatalf("unsafe_rate_caps logged with the caps in place: %v", got)
	}
	cfg = testConfig(t, token.NewAdmin())
	cfg.UnsafeRateCaps, cfg.ReadPerMinute, cfg.SearchPerMinute = true, 1200, 90
	a, logs = open(t, cfg, noClients{})
	_ = a.closeStores()
	got := logs.find("unsafe_rate_caps")
	if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["read_per_minute"] != float64(1200) || got[0]["search_per_minute"] != float64(90) {
		t.Fatalf("unsafe_rate_caps events = %v", got)
	}
}

func TestTheClientListenerAppliesTheConfiguredReadBudget(t *testing.T) {
	cfg := testConfig(t, token.NewAdmin())
	cfg.ReadPerMinute = 2
	a, _, stop := start(t, cfg, oneClient{})
	client := addr(t, a, "client")
	for i := range 2 {
		if r := mustDo(t, http.MethodGet, client, "/v1/me", bearer(syntheticClientToken)); r.status != http.StatusOK {
			t.Fatalf("read %d = %d %s", i+1, r.status, r.body)
		}
	}
	r := mustDo(t, http.MethodGet, client, "/v1/me", bearer(syntheticClientToken))
	if r.status != http.StatusTooManyRequests || r.body != `{"error":"rate_limited"}` || r.header.Get("Retry-After") != "30" {
		t.Fatalf("third read = %d %s, Retry-After %q", r.status, r.body, r.header.Get("Retry-After"))
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
