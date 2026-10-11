package app

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

type countingClient struct {
	*stubClient
	acked bool
	sends *atomic.Int32
}

func (c countingClient) Send(ctx context.Context, req engine.SendRequest) (engine.Sent, error) {
	c.sends.Add(1)
	if !c.acked {
		return engine.Sent{}, engine.ErrOutcomeUnknown
	}
	return c.stubClient.Send(ctx, req)
}

type messageRef struct {
	Ref  string `json:"mref"`
	Text string `json:"text"`
}

func TestARetryAfterTheReplyTargetsSenderIsReKeyedReplays(t *testing.T) {
	for _, tt := range []struct {
		name   string
		acked  bool
		status int
	}{
		{name: "acknowledged", acked: true, status: http.StatusCreated},
		{name: "outcome unknown", status: http.StatusGatewayTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const (
				groupJID = "120363000000000031@g.us"
				bobPN    = "15550100032@s.whatsapp.net"
				bobLID   = "100000000000032@lid"
				carolPN  = "15550100033@s.whatsapp.net"
			)
			adminToken := token.NewAdmin()
			cfg := testConfig(t, adminToken)
			stub, sends := newStubClient(), &atomic.Int32{}
			a, err := newAppWith(t.Context(), cfg, logx.NewWriter(&syncBuffer{}), testMaster(t), nil,
				fixed(engineParts{client: countingClient{stubClient: stub, acked: tt.acked, sends: sends}, versions: stubVersions{}, decoder: stubDecoder{}}))
			if err != nil {
				t.Fatalf("newAppWith: %v", err)
			}
			stop := run(t, a)
			<-stub.connected
			if !stub.emit(engine.Connected{}) {
				t.Fatal("the engine handler is missing")
			}
			waitUntil(t, "ready", a.engine.Ready)
			group, _ := policy.Normalize(groupJID)
			pn, _ := policy.Normalize(bobPN)
			lid, _ := policy.Normalize(bobLID)
			carol, _ := policy.Normalize(carolPN)
			archived := func(id string, sender policy.CanonicalChat) func() bool {
				return func() bool {
					var ok bool
					_ = a.archive.Read(t.Context(), "test.archived", func(r *ingest.Reader) error {
						var err error
						_, ok, err = r.ResolveInChat(group, id, sender)
						return err
					})
					return ok
				}
			}
			stub.emit(engine.Group{Chat: groupJID, Subject: "Synthetic group", Timestamp: time.Now(),
				Members: []engine.Participant{{User: "15550100009@s.whatsapp.net", Admin: true}, {User: bobPN}, {User: carolPN}}})
			stub.emit(engine.Message{Chat: groupJID, ID: "M1", Sender: bobPN, Timestamp: time.Now(), Kind: engine.KindText, Text: "synthetic question"})
			waitUntil(t, "M1 archived", archived("M1", pn))

			admin, listener := addr(t, a, "admin"), addr(t, a, "client")
			created := post(t, admin, "/admin/v1/clients", bearer(adminToken), `{"name":"agent","read_chats":["`+groupJID+`"],"write_chats":["`+groupJID+`"]}`)
			var credential struct {
				Credential string `json:"credential"`
			}
			if created.status != http.StatusOK || json.Unmarshal([]byte(created.body), &credential) != nil {
				t.Fatalf("create = %d %s", created.status, created.body)
			}
			var chats struct {
				Chats []struct {
					ID string `json:"id"`
				} `json:"chats"`
			}
			if r := mustDo(t, http.MethodGet, listener, "/v1/chats", bearer(credential.Credential)); json.Unmarshal([]byte(r.body), &chats) != nil || len(chats.Chats) != 1 {
				t.Fatalf("chats = %d %s", r.status, r.body)
			}
			ref := chats.Chats[0].ID
			var page struct {
				Messages []messageRef `json:"messages"`
			}
			if r := mustDo(t, http.MethodGet, listener, "/v1/chats/"+ref+"/messages", bearer(credential.Credential)); json.Unmarshal([]byte(r.body), &page) != nil || len(page.Messages) != 1 {
				t.Fatalf("messages = %d %s", r.status, r.body)
			}
			header := bearer(credential.Credential)
			header.Set("Idempotency-Key", "rekey-retry-key-0001")
			body := `{"text":"synthetic answer","reply_to":"` + page.Messages[0].Ref + `"}`
			first := post(t, listener, "/v1/chats/"+ref+"/messages", header, body)
			if first.status != tt.status {
				t.Fatalf("the first send = %d %s, want %d", first.status, first.body, tt.status)
			}

			stub.emit(engine.Message{Chat: groupJID, ID: "M2", Sender: bobLID, SenderAlt: bobPN, Addressing: "lid", Timestamp: time.Now(), Kind: engine.KindText, Text: "synthetic follow-up"})
			waitUntil(t, "the reply target re-keyed", archived("M1", lid))

			retry := post(t, listener, "/v1/chats/"+ref+"/messages", header, body)
			if retry.status != tt.status || retry.header.Get("Idempotent-Replayed") != "true" {
				t.Fatalf("the retry after the re-key = %d %s %v, want the stored %d replayed", retry.status, retry.body, retry.header, tt.status)
			}
			if tt.acked && !strings.Contains(retry.body, `"replayed":true`) {
				t.Fatalf("the retry after the re-key = %s, want a replayed body", retry.body)
			}
			if n := sends.Load(); n != 1 {
				t.Fatalf("the engine sent %d times, want once", n)
			}

			stub.emit(engine.Message{Chat: groupJID, ID: "M1", Sender: carolPN, Timestamp: time.Now(), Kind: engine.KindText, Text: "synthetic other question"})
			waitUntil(t, "the second M1 archived", archived("M1", carol))
			if r := mustDo(t, http.MethodGet, listener, "/v1/chats/"+ref+"/messages", bearer(credential.Credential)); json.Unmarshal([]byte(r.body), &page) != nil {
				t.Fatalf("messages = %d %s", r.status, r.body)
			}
			i := slices.IndexFunc(page.Messages, func(m messageRef) bool { return m.Text == "synthetic other question" })
			if i < 0 {
				t.Fatal("the second M1 is missing from the page")
			}
			other := post(t, listener, "/v1/chats/"+ref+"/messages", header, `{"text":"synthetic answer","reply_to":"`+page.Messages[i].Ref+`"}`)
			if other.status != http.StatusUnprocessableEntity || other.body != `{"error":"idempotency_key_reused"}` {
				t.Fatalf("the key reused for another target with the same message id = %d %s", other.status, other.body)
			}
			if err := stop(); err != nil {
				t.Fatalf("Run: %v", err)
			}
		})
	}
}
