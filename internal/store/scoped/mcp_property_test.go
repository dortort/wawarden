package scoped_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/store/scoped"
)

type toolAnswer struct {
	body  []byte
	page  httpPage
	error string
	sql   []string
}

func (h *httpHarness) tool(t *rapid.T, token, name string, args map[string]any) toolAnswer {
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("encode the arguments: %v", err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	r.Header = http.Header{"Authorization": {"Bearer " + token}, "Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Mcp-Protocol-Version": {"2025-11-25"}}
	h.mu.Lock()
	h.sql = nil
	h.mu.Unlock()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, rec.Body.Bytes())
	var reply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
			IsError    bool            `json:"isError"`
		} `json:"result"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || len(reply.Result.Content) != 1 {
		t.Fatalf("%s %v = %d %s", name, args, rec.Code, rec.Body.String())
	}
	out := toolAnswer{body: rec.Body.Bytes(), sql: h.sql}
	if reply.Result.IsError {
		out.error = reply.Result.Content[0].Text
		return out
	}
	if err := json.Unmarshal(reply.Result.Structured, &out.page); err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return out
}

func (h *httpHarness) toolPage(t *rapid.T, token, name string, args map[string]any) httpPage {
	a := h.tool(t, token, name, args)
	if a.error != "" {
		t.Fatalf("%s %v = %s", name, args, a.error)
	}
	return a.page
}

func (h *httpHarness) toolWalk(t *rapid.T, token, name string, args map[string]any, key string, limit int) []httpMessage {
	var out []httpMessage
	args = maps.Clone(args)
	args["limit"] = limit
	for range 1000 {
		p := h.toolPage(t, token, name, args)
		out = append(out, p.Messages...)
		if p.Next == nil || key == "since" && !p.More {
			return out
		}
		args[key] = *p.Next
	}
	t.Fatalf("%s %v did not end", name, args)
	return nil
}

func requireToolDeniedEqualsMissing(t *rapid.T, h *httpHarness, token, name string, denied, missing map[string]any) {
	d, m := h.tool(t, token, name, denied), h.tool(t, token, name, missing)
	if d.error != "not_found" || !bytes.Equal(d.body, m.body) {
		t.Fatalf("%s %v = %s, %v = %s", name, denied, d.body, missing, m.body)
	}
	if !slices.Equal(d.sql, m.sql) || len(d.sql) == 0 {
		t.Fatalf("%s %v issued %q, %v issued %q", name, denied, d.sql, missing, m.sql)
	}
}

func TestPropertyMCPToolsMatchTheOracle(t *testing.T) {
	parent := t.TempDir()
	cov := &tally{counts: map[string]int{}}
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp(parent, "archive")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		a := buildArchive(t, dir)
		defer func() { _ = a.store.Close(); _ = os.RemoveAll(dir) }()
		chats, messages := dump(t, a.store)
		h := newHTTPHarness(t, a)
		a.seen["dead canary"] = len(a.dead) > 0
		refOf := map[string]string{}
		for _, c := range chats {
			refOf[c.JID] = c.Ref
		}
		for _, c := range a.clients {
			token := "synthetic-token-" + c.id
			set := scopeOfClient(t, a.store, c)
			h.clients.set(token, clientOf(t, c, set))
			if c.revoked {
				if denied := h.tool(t, token, "list_chats", map[string]any{}); denied.error != "not_found" || len(denied.sql) != 0 {
					t.Fatalf("a revoked client's listing = %s after %q", denied.body, denied.sql)
				}
				continue
			}
			limit := rapid.IntRange(1, 5).Draw(t, "limit")
			checkMCPChats(t, h, token, set, chats, limit)
			cursors := map[string]string{}
			for _, dc := range chats {
				if !set.has(dc.JID) {
					checkMCPDenied(t, h, token, dc.Ref)
					a.seen["mcp denied chat"] = true
					continue
				}
				got := h.toolWalk(t, token, "get_messages", map[string]any{"chat": dc.Ref}, "cursor", limit)
				want := expected(messages, func(m scoped.DumpMessage) bool { return m.Chat == dc.JID })
				if !slices.Equal(texts(got), want) {
					t.Fatalf("%s read %s as %q, want %q", c.id, dc.Ref, texts(got), want)
				}
				for _, m := range got {
					if m.Chat != dc.Ref {
						t.Fatalf("%s read a message of %s under %s", c.id, m.Chat, dc.Ref)
					}
				}
				if first := h.toolPage(t, token, "get_messages", map[string]any{"chat": dc.Ref, "limit": 1}); first.Next != nil {
					cursors[dc.Ref] = *first.Next
					a.seen["mcp message cursor"] = true
				}
			}
			changes := h.toolWalk(t, token, "get_changes", map[string]any{}, "since", limit)
			want := expected(messages, func(m scoped.DumpMessage) bool { return set.has(m.Chat) })
			if got := texts(changes); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
				t.Fatalf("%s changes = %q, want %q", c.id, got, want)
			}
			checkMCPSearch(t, h, a, token, set, messages, refOf)
			checkMCPReplay(t, h, a, token, c, set, cursors, refOf)
		}
		for _, body := range h.bodies {
			unsealed := sealedTokens.ReplaceAll(body, nil)
			for _, d := range a.dead {
				if bytes.Contains(unsealed, []byte(d)) {
					t.Fatalf("a tool result carries the revoked or edited text %s: %s", d, body)
				}
			}
		}
		cov.add(a.seen)
	})
	cov.require(t, "mcp denied chat", "mcp message cursor", "mcp replay narrowed", "mcp search hit", "dead canary")
}

func checkMCPChats(t *rapid.T, h *httpHarness, token string, set scopeSet, chats []scoped.DumpChat, limit int) {
	var got []string
	args := map[string]any{"limit": limit}
	for range 1000 {
		p := h.toolPage(t, token, "list_chats", args)
		for _, c := range p.Chats {
			got = append(got, c.ID)
		}
		if p.Next == nil {
			break
		}
		args["cursor"] = *p.Next
	}
	ordered := slices.Clone(chats)
	slices.SortFunc(ordered, chatOrder)
	var want []string
	for _, c := range ordered {
		if set.has(c.JID) {
			want = append(want, c.Ref)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("list_chats = %q, want %q", got, want)
	}
}

func checkMCPDenied(t *rapid.T, h *httpHarness, token, ref string) {
	for _, c := range []struct {
		tool  string
		extra map[string]any
	}{
		{"get_chat", nil},
		{"get_messages", nil},
		{"get_messages", map[string]any{"cursor": "x"}},
		{"get_changes", nil},
		{"get_changes", map[string]any{"since": "x"}},
		{"search_messages", map[string]any{"query": "text"}},
		{"search_messages", map[string]any{"query": "text", "cursor": "x"}},
	} {
		denied, missing := maps.Clone(c.extra), maps.Clone(c.extra)
		if denied == nil {
			denied, missing = map[string]any{}, map[string]any{}
		}
		denied["chat"], missing["chat"] = ref, missingRef
		requireToolDeniedEqualsMissing(t, h, token, c.tool, denied, missing)
	}
}

func checkMCPSearch(t *rapid.T, h *httpHarness, a *archive, token string, set scopeSet, messages []scoped.DumpMessage, refOf map[string]string) {
	for _, c := range slices.Sorted(maps.Keys(a.live)) {
		var holder *scoped.DumpMessage
		for i := range messages {
			if !messages[i].Revoked && strings.Contains(messages[i].Text, c) {
				holder = &messages[i]
			}
		}
		got := h.toolWalk(t, token, "search_messages", map[string]any{"query": c}, "cursor", scoped.MaxLimit)
		inScope := holder != nil && set.has(holder.Chat)
		switch {
		case inScope && (len(got) != 1 || got[0].Chat != refOf[holder.Chat] || got[0].Text == nil || !strings.Contains(*got[0].Text, c)):
			t.Fatalf("search_messages %s = %+v, want the one message of %s", c, got, holder.Chat)
		case !inScope && len(got) != 0:
			t.Fatalf("search_messages %s = %+v, want nothing", c, got)
		case inScope:
			a.seen["mcp search hit"] = true
		}
	}
}

func checkMCPReplay(t *rapid.T, h *httpHarness, a *archive, token string, c propClient, set scopeSet, cursors map[string]string, refOf map[string]string) {
	narrowed := scopeSet{chats: map[string]bool{}}
	for _, jid := range slices.Sorted(maps.Keys(refOf)) {
		if set.has(jid) && rapid.Bool().Draw(t, "kept after the change") {
			narrowed.chats[jid] = true
		}
	}
	changes := h.toolPage(t, token, "get_changes", map[string]any{"limit": 1})
	search := h.toolPage(t, token, "search_messages", map[string]any{"query": "text", "limit": 1})
	h.clients.set(token, clientOf(t, propClient{id: c.id}, narrowed))
	allowed := map[string]bool{}
	for jid := range narrowed.chats {
		allowed[refOf[jid]] = true
	}
	for ref, next := range cursors {
		replay := h.tool(t, token, "get_messages", map[string]any{"chat": ref, "limit": 1, "cursor": next})
		if allowed[ref] {
			if replay.error != "" {
				t.Fatalf("replay of a cursor for a kept chat = %s", replay.body)
			}
			continue
		}
		missing := h.tool(t, token, "get_messages", map[string]any{"chat": missingRef, "limit": 1, "cursor": next})
		if replay.error != "not_found" || !bytes.Equal(replay.body, missing.body) {
			t.Fatalf("replay of a cursor for a removed chat = %s", replay.body)
		}
		a.seen["mcp replay narrowed"] = true
	}
	for _, p := range []struct {
		tool, query string
		key         string
		next        *string
	}{{"get_changes", "", "since", changes.Next}, {"search_messages", "text", "cursor", search.Next}} {
		if p.next == nil {
			continue
		}
		args := map[string]any{"limit": scoped.MaxLimit, p.key: *p.next}
		if p.query != "" {
			args["query"] = p.query
		}
		for _, m := range h.toolPage(t, token, p.tool, args).Messages {
			if !allowed[m.Chat] {
				t.Fatalf("a replayed cursor returned a message of %s after the grant narrowed", m.Chat)
			}
		}
	}
	h.clients.set(token, clientOf(t, c, set))
}
