package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/hooks"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

type hookReceiver struct {
	mu     sync.Mutex
	bodies []string
	sigs   []string
	ids    []string
	stamps []string
}

func (r *hookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(body))
	r.sigs = append(r.sigs, req.Header.Get("WaWarden-Signature"))
	r.ids = append(r.ids, req.Header.Get("WaWarden-Event-Id"))
	r.stamps = append(r.stamps, req.Header.Get("WaWarden-Timestamp"))
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *hookReceiver) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func TestAWebhookPostsTheNewMessagesItsClientMayRead(t *testing.T) {
	logs := &strings.Builder{}
	w := logx.NewWriter(logs)
	master := testMaster(t)
	archive, err := ingest.Open(t.Context(), ingest.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1,
		Logger: logx.New(w, slog.LevelDebug), Master: master, AuditOut: w})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	group, _ := policy.Normalize("120363000000000001@g.us")
	other, _ := policy.Normalize("120363000000000002@g.us")
	alice, _ := policy.Normalize("15550100001@s.whatsapp.net")
	insert := func(chat policy.CanonicalChat, id, text string, origin ingest.Origin, fromMe bool) {
		t.Helper()
		if err := archive.Write(t.Context(), "test.insert", func(tx *ingest.Tx) error {
			_, _, err := tx.InsertMessage(ingest.Message{Chat: chat, ID: id, Sender: alice, FromMe: fromMe, Origin: origin, Timestamp: time.Now(),
				Kind: ingest.KindText, Text: text, Ingested: time.Now()})
			return err
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert(group, "BEFORE", "before the webhook", ingest.OriginLive, false)

	client, _, err := archive.Clients().Create(t.Context(), policy.ClientSpec{Name: "agent", Read: []string{group.JID()}, AllowWebhooks: true})
	if err != nil {
		t.Fatalf("create the client: %v", err)
	}
	recv := &hookReceiver{}
	server := httptest.NewTLSServer(recv)
	t.Cleanup(server.Close)
	_, refs, err := sealers(master)
	if err != nil {
		t.Fatalf("sealers: %v", err)
	}
	svc := webhookService{store: archive.Webhooks(), changed: func() {}}
	view, secret, err := svc.Create(t.Context(), client.ID, hooks.Spec{
		URL: server.URL + "/in", Payload: `{"chat":"{{message.chat}}","text":"{{message.text}}","msg":"{{message}}"}`,
		Filter: hooks.Filter{Direction: hooks.DirectionInbound, Contains: "ORDER"},
	})
	if err != nil {
		t.Fatalf("create the webhook: %v", err)
	}
	if len(secret) != 64 {
		t.Fatalf("the secret %q is not 64 hexadecimal digits", secret)
	}
	if _, err := svc.Get(t.Context(), "zzzzzzzz", view.ID); err != api.ErrWebhookNotFound {
		t.Fatalf("Get by another client = %v", err)
	}

	insert(group, "M1", "new order 1", ingest.OriginLive, false)
	insert(other, "M2", "order in an unreadable chat", ingest.OriginLive, false)
	insert(group, "M3", "order from history", ingest.OriginHistory, false)
	insert(group, "M4", "the owner's own order", ingest.OriginLive, true)
	insert(group, "M5", "no match", ingest.OriginLive, false)
	insert(group, "M6", "second Order", ingest.OriginLive, false)

	d, err := hooks.New(hooks.Options{
		Store: archive.Webhooks(), Clients: archive.Clients(), Top: archive.Webhooks().Top, Metrics: metrics.NewRegistry(), Logger: logx.New(w, slog.LevelDebug),
		Feed: api.NewWebhookFeed(api.FeedDeps{Archive: archive.Scoped(), Refs: refs}), HTTP: server.Client(),
	})
	if err != nil {
		t.Fatalf("hooks.New: %v", err)
	}
	d.Start(t.Context())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := svc.Get(t.Context(), client.ID, view.ID); err == nil && v.Delivered == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := d.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := recv.received()
	if len(got) != 2 {
		t.Fatalf("received %q, want the two new, live, inbound, readable matches", got)
	}
	for i, want := range []string{"new order 1", "second Order"} {
		var body struct {
			Chat, Text string
			Msg        map[string]any
		}
		if err := json.Unmarshal([]byte(got[i]), &body); err != nil || body.Text != want || len(body.Chat) != 32 || body.Msg["untrusted"] != true {
			t.Fatalf("delivery %d = %s, %v", i, got[i], err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(recv.ids[i] + "." + recv.stamps[i] + "." + got[i]))
		if recv.sigs[i] != "v1="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("delivery %d does not verify with the secret the client was given", i)
		}
	}
	after, err := svc.Get(t.Context(), client.ID, view.ID)
	if err != nil || after.Delivered != 2 || after.Failed != 0 || after.LastDelivery.IsZero() {
		t.Fatalf("after delivery %+v, %v", after, err)
	}
}
