package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/ratelimit"
	"github.com/dortort/wawarden/internal/store/scoped"
)

var updateMCPTools = flag.Bool("update-mcp-tools", false, "rewrite the golden tools/list files under testdata/mcp")

const (
	mcpEndpointURL = "http://wawarden.invalid/mcp"
	legacyProtocol = "2025-11-25"
	argCanary      = "canary-6f2a91-argument"
)

var (
	mcpExpiry    = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	sealedValues = regexp.MustCompile(`m1_[A-Za-z0-9_-]+|"next":"[A-Za-z0-9_-]*"`)
	toolNames    = []string{toolGetChanges, toolGetChat, toolGetMessages, toolListChats, toolSearchMessages}
)

type protocol struct {
	name string
	opts *mcp.ClientSessionOptions
}

var protocols = []protocol{{name: "2026-07-28"}, {name: "legacy", opts: &mcp.ClientSessionOptions{ProtocolVersion: legacyProtocol}}}

func newMCPFixture(t *testing.T) *readFixture {
	t.Helper()
	f := newReadFixture(t)
	for _, c := range f.clients {
		c.ExpiresAt = mcpExpiry
	}
	return f
}

type handlerTransport struct {
	handler http.Handler
	key     string
	mu      *sync.Mutex
	methods *[]string
}

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	if h.methods != nil && r.Body != nil {
		var body bytes.Buffer
		if _, err := body.ReadFrom(r.Body); err != nil {
			return nil, err
		}
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body.Bytes(), &msg)
		h.mu.Lock()
		*h.methods = append(*h.methods, r.Method+" "+msg.Method)
		h.mu.Unlock()
		r.Body = readCloser{bytes.NewReader(body.Bytes())}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	return rec.Result(), nil
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

func connectAt(t *testing.T, h http.Handler, endpoint, key string, p protocol) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "wawarden-test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: handlerTransport{handler: h, key: key}}, DisableStandaloneSSE: true}
	cs, err := client.Connect(t.Context(), transport, p.opts)
	if err != nil {
		t.Fatalf("connect over %s: %v", p.name, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func connect(t *testing.T, h http.Handler, key string, p protocol) *mcp.ClientSession {
	t.Helper()
	return connectAt(t, h, mcpEndpointURL, key, p)
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func structured(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	return b
}

func normalised(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return sealedValues.ReplaceAllString(string(out), "SEALED")
}

func requireToolError(t *testing.T, res *mcp.CallToolResult, code string) {
	t.Helper()
	text, ok := res.Content[0].(*mcp.TextContent)
	if !res.IsError || len(res.Content) != 1 || !ok || text.Text != code {
		t.Fatalf("result = %+v %+v, want the error %s", res, res.Content[0], code)
	}
	var shape struct {
		Session *sessionView `json:"session"`
	}
	if err := json.Unmarshal(structured(t, res), &shape); err != nil || shape.Session == nil || shape.Session.State != "connected" {
		t.Fatalf("structured content %s carries no session", structured(t, res))
	}
}

func rawMCP(t *testing.T, h http.Handler, key, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Bearer "+key)
	for name, values := range header {
		r.Header[name] = values
	}
	return serve(h, r)
}

func legacyCall(method, params string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
}

var legacyHeader = http.Header{"Mcp-Protocol-Version": {legacyProtocol}}

func TestMCPToolsAnswerLikeRESTOnBothProtocols(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			f := newMCPFixture(t)
			cs := connect(t, f.handler, keyGroup, p)
			caps := cs.InitializeResult().Capabilities
			if caps.Tools == nil || caps.Tools.ListChanged || caps.Logging != nil || caps.Prompts != nil || caps.Resources != nil || caps.Completions != nil {
				t.Fatalf("capabilities = %+v, want tools only, without list changes", caps)
			}
			tools, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			var names []string
			for _, tool := range tools.Tools {
				names = append(names, tool.Name)
			}
			if !slices.Equal(names, toolNames) {
				t.Fatalf("tools = %q, want %q", names, toolNames)
			}
			for _, tt := range []struct {
				tool   string
				args   map[string]any
				target string
				wrap   string
			}{
				{tool: toolListChats, args: map[string]any{}, target: "/v1/chats"},
				{tool: toolGetChat, args: map[string]any{"chat": groupRef}, target: "/v1/chats/" + groupRef, wrap: "chat"},
				{tool: toolGetMessages, args: map[string]any{"chat": groupRef, "limit": 3}, target: "/v1/chats/" + groupRef + "/messages?limit=3"},
				{tool: toolSearchMessages, args: map[string]any{"query": "synthetic", "limit": 3}, target: "/v1/search?q=synthetic&limit=3"},
				{tool: toolSearchMessages, args: map[string]any{"query": "synthetic", "chat": groupRef}, target: "/v1/search?q=synthetic&chat=" + groupRef},
				{tool: toolGetChanges, args: map[string]any{"limit": 2, "since": "2026-01-01T00:00:00Z"}, target: "/v1/changes?limit=2&since=2026-01-01T00:00:00Z"},
			} {
				res := call(t, cs, tt.tool, tt.args)
				body := f.get(t, keyGroup, tt.target).Body.Bytes()
				if tt.wrap != "" {
					body = []byte(`{"` + tt.wrap + `":` + string(body) + `,"session":{"state":"connected"}}`)
				}
				got, text := structured(t, res), res.Content[0].(*mcp.TextContent).Text
				if res.IsError || normalised(t, got) != normalised(t, body) || sealedValues.ReplaceAllString(text, "SEALED") != sealedValues.ReplaceAllString(string(body), "SEALED") {
					t.Fatalf("%s %v = %s (%s), REST answers %s", tt.tool, tt.args, got, text, body)
				}
			}
			var first pageView
			if err := json.Unmarshal(structured(t, call(t, cs, toolGetMessages, map[string]any{"chat": groupRef, "limit": 4})), &first); err != nil || first.Next == nil {
				t.Fatalf("first page = %+v, %v", first, err)
			}
			var second pageView
			if err := json.Unmarshal(structured(t, call(t, cs, toolGetMessages, map[string]any{"chat": groupRef, "limit": 4, "cursor": *first.Next})), &second); err != nil {
				t.Fatalf("second page: %v", err)
			}
			if len(second.Messages) != 4 || second.Messages[0].TS >= first.Messages[3].TS {
				t.Fatalf("the cursor did not continue the walk: %+v after %+v", second.Messages, first.Messages)
			}
		})
	}
}

func TestMCPToolErrorsCarryFixedCodes(t *testing.T) {
	long := strings.Repeat("x", maxQueryChars) + argCanary
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			f := newMCPFixture(t)
			f.clients[keyRevoked] = &policy.Client{ID: "client-revoked", ReadAll: true, Revoked: true, ExpiresAt: mcpExpiry}
			cs := connect(t, f.handler, keyGroup, p)
			for _, tt := range []struct {
				tool string
				args map[string]any
				code string
			}{
				{toolGetChat, map[string]any{"chat": aliceRef}, codeNotFound},
				{toolGetChat, map[string]any{"chat": noRef}, codeNotFound},
				{toolGetMessages, map[string]any{"chat": aliceRef, "cursor": "x"}, codeNotFound},
				{toolGetMessages, map[string]any{"chat": groupRef, "cursor": "x"}, codeInvalidCursor},
				{toolSearchMessages, map[string]any{"query": "synthetic", "chat": aliceRef}, codeNotFound},
				{toolGetChanges, map[string]any{"chat": noRef}, codeNotFound},
				{toolGetChanges, map[string]any{"since": "x"}, codeInvalidCursor},
				{toolSearchMessages, map[string]any{"query": "ab cd"}, codeInvalidQuery},
				{toolListChats, map[string]any{"limit": 0}, codeInvalidArguments},
				{toolListChats, map[string]any{"limit": scoped.MaxLimit + 1}, codeInvalidArguments},
				{toolListChats, map[string]any{"limit": argCanary}, codeInvalidArguments},
				{toolListChats, map[string]any{"limit": 2.5}, codeInvalidArguments},
				{toolListChats, map[string]any{"cursor": ""}, codeInvalidArguments},
				{toolListChats, map[string]any{argCanary: 1}, codeInvalidArguments},
				{toolGetChat, map[string]any{}, codeInvalidArguments},
				{toolGetChat, map[string]any{"chat": nil}, codeInvalidArguments},
				{toolGetMessages, map[string]any{"chat": strings.Repeat("a", maxRefChars+1)}, codeInvalidArguments},
				{toolSearchMessages, map[string]any{"query": long}, codeInvalidArguments},
				{toolSearchMessages, map[string]any{"query": "ab"}, codeInvalidArguments},
			} {
				res := call(t, cs, tt.tool, tt.args)
				requireToolError(t, res, tt.code)
				if wire, _ := json.Marshal(res); bytes.Contains(wire, []byte(argCanary)) {
					t.Fatalf("%s %v echoed the argument: %s", tt.tool, tt.args, wire)
				}
			}
			requireToolError(t, call(t, connect(t, f.handler, keyRevoked, p), toolListChats, nil), codeNotFound)
			for err, code := range map[error]string{scoped.ErrBusy: codeBusy, errors.New("synthetic store failure " + argCanary): codeInternal} {
				f.archive.mu.Lock()
				f.archive.err = err
				f.archive.mu.Unlock()
				res := call(t, cs, toolListChats, nil)
				requireToolError(t, res, code)
				if wire, _ := json.Marshal(res); bytes.Contains(wire, []byte(argCanary)) {
					t.Fatalf("a store error reached the wire: %s", wire)
				}
			}
		})
	}
}

func TestMCPDeniedAndMissingAreByteIdentical(t *testing.T) {
	f := newMCPFixture(t)
	for _, pair := range [][2]string{
		{`{"name":"get_chat","arguments":{"chat":"` + aliceRef + `"}}`, `{"name":"get_chat","arguments":{"chat":"` + noRef + `"}}`},
		{`{"name":"get_messages","arguments":{"chat":"` + aliceRef + `"}}`, `{"name":"get_messages","arguments":{"chat":"` + noRef + `"}}`},
		{`{"name":"get_messages","arguments":{"chat":"` + aliceRef + `","cursor":"x"}}`, `{"name":"get_messages","arguments":{"chat":"` + noRef + `","cursor":"x"}}`},
		{`{"name":"search_messages","arguments":{"query":"synthetic","chat":"` + aliceRef + `"}}`, `{"name":"search_messages","arguments":{"query":"synthetic","chat":"` + noRef + `"}}`},
		{`{"name":"get_changes","arguments":{"chat":"` + aliceRef + `","since":"x"}}`, `{"name":"get_changes","arguments":{"chat":"` + noRef + `","since":"x"}}`},
	} {
		f.archive.trace()
		denied := rawMCP(t, f.handler, keyGroup, legacyCall("tools/call", pair[0]), legacyHeader)
		deniedPath := f.archive.trace()
		missing := rawMCP(t, f.handler, keyGroup, legacyCall("tools/call", pair[1]), legacyHeader)
		requireSameResponse(t, denied, missing)
		if missingPath := f.archive.trace(); !slices.Equal(deniedPath, missingPath) {
			t.Fatalf("%s took %q, %s took %q", pair[0], deniedPath, pair[1], missingPath)
		}
		if !strings.Contains(denied.Body.String(), `"isError":true`) || !strings.Contains(denied.Body.String(), `"text":"not_found"`) {
			t.Fatalf("%s = %s, want not_found", pair[0], denied.Body.String())
		}
	}
}

type faultyArchive struct {
	*fakeArchive
	panics bool
}

func (a faultyArchive) Chats(_ policy.ReadGrant, ctx context.Context, _ scoped.ChatPosition, _ int) (scoped.ChatPage, error) {
	if a.panics {
		panic(secretPanic{text: panicCanary})
	}
	<-ctx.Done()
	return scoped.ChatPage{}, ctx.Err()
}

func probeReads(f *readFixture, archive ReadArchive) *reads {
	return &reads{archive: archive, cursors: f.cursors, refs: f.refs, searches: ratelimit.New(DefaultSearchesPerMinute, searchBurst, fixedNow), session: func() string { return f.state }}
}

func probeEndpoint(t *testing.T, f *readFixture, archive ReadArchive, deadline time.Duration) string {
	t.Helper()
	f.handler.(*pipeline).router.mcp("POST /probe/mcp", newMCPEndpoint(probeReads(f, archive), fixedNow, deadline))
	return "http://wawarden.invalid/probe/mcp"
}

func TestMCPEndpointRefusesARequestWithoutThePipelinesClient(t *testing.T) {
	f := newMCPFixture(t)
	endpoint := newMCPEndpoint(probeReads(f, f.archive), fixedNow, mcpCallDeadline)
	rec := rawMCP(t, endpoint, keyGroup, legacyCall("tools/list", "{}"), legacyHeader)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), toolListChats) {
		t.Fatalf("an endpoint reached without the pipeline = %d %s, want 401", rec.Code, rec.Body.String())
	}
}

func TestMCPToolPanicsBecomeInternalErrors(t *testing.T) {
	f := newMCPFixture(t)
	reg := metrics.NewRegistry()
	logs := installPanicReporter(t, reg)
	cs := connectAt(t, f.handler, probeEndpoint(t, f, faultyArchive{fakeArchive: f.archive, panics: true}, mcpCallDeadline), keyGroup, protocols[0])
	res := call(t, cs, toolListChats, nil)
	requireToolError(t, res, codeInternal)
	if wire, _ := json.Marshal(res); bytes.Contains(wire, []byte(panicCanary)) || strings.Contains(logs.String(), panicCanary) {
		t.Fatalf("the panic value leaked: %s %s", wire, logs.String())
	}
	if got := metricValue(t, reg, `wawarden_panics_total{name="api.mcp"}`); got != "1" {
		t.Fatalf("wawarden_panics_total{name=\"api.mcp\"} = %q, want 1", got)
	}
	if res := call(t, cs, toolGetChat, map[string]any{"chat": groupRef}); res.IsError {
		t.Fatalf("the endpoint stopped answering after a panic: %+v", res)
	}
}

func TestMCPToolCallsHaveADeadline(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			f := newMCPFixture(t)
			cs := connectAt(t, f.handler, probeEndpoint(t, f, faultyArchive{fakeArchive: f.archive}, 50*time.Millisecond), keyGroup, p)
			start := time.Now()
			requireToolError(t, call(t, cs, toolListChats, nil), codeBusy)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("a stalled read answered after %s", elapsed)
			}
		})
	}
}

func TestMCPBodiesAreCheckedBeforeTheLibrary(t *testing.T) {
	f := newMCPFixture(t)
	valid := legacyCall("tools/list", "{}")
	deep := legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"x","_":`+strings.Repeat(`{"a":`, 14)+"1"+strings.Repeat("}", 14)+"}}")
	for name, body := range map[string]string{
		"duplicate envelope keys": `{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/list","params":{}}`,
		"duplicate arguments":     legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"`+groupRef+`","chat":"`+aliceRef+`"}}`),
		"trailing message":        valid + valid,
		"trailing token":          valid + " 1",
		"batch":                   "[" + valid + "]",
		"string":                  `"x"`,
		"null":                    "null",
		"byte order mark":         byteOrderMark + valid,
		"invalid UTF-8":           legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"`+"\xff"+`"}}`),
		"nesting past 16":         deep,
	} {
		rec := rawMCP(t, f.handler, keyGroup, body, legacyHeader)
		requireError(t, rec, http.StatusBadRequest, codeInvalidBody)
		requireSecurityHeaders(t, rec.Header())
		_ = name
	}
	requireError(t, rawMCP(t, f.handler, keyGroup, valid, http.Header{"Content-Type": {"text/plain"}}), http.StatusUnsupportedMediaType, codeUnsupportedMediaType)

	padded := legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"`+strings.Repeat("a", maxMCPBodyBytes-200)+`"}}`)
	if rec := rawMCP(t, f.handler, keyGroup, padded, legacyHeader); !strings.Contains(rec.Body.String(), `"text":"invalid_arguments"`) {
		t.Fatalf("a body under the limit = %d %s", rec.Code, rec.Body.String())
	}
	oversized := legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"`+strings.Repeat("a", maxMCPBodyBytes)+`"}}`)
	rec := rawMCP(t, f.handler, keyGroup, oversized, legacyHeader)
	requireError(t, rec, http.StatusRequestEntityTooLarge, codeBodyTooLarge)
	if rec.Header().Get("Connection") != "close" {
		t.Fatalf("Connection = %q, want close: an announced oversized body is never read", rec.Header().Get("Connection"))
	}
	chunked := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(oversized))
	chunked.ContentLength = -1
	chunked.Header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Authorization": {"Bearer " + keyGroup}}
	requireError(t, serve(f.handler, chunked), http.StatusRequestEntityTooLarge, codeBodyTooLarge)
}

func TestMCPRefusalsBeforeTheEndpointAreUniform(t *testing.T) {
	f := newMCPFixture(t)
	valid := legacyCall("tools/list", "{}")
	rec := rawMCP(t, f.handler, "synthetic-unknown-key", valid, legacyHeader)
	requireError(t, rec, http.StatusUnauthorized, codeUnauthorized)
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	requireError(t, rawMCP(t, f.handler, keyGroup, valid, http.Header{"Origin": {"https://example.test"}}), http.StatusForbidden, codeForbidden)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := serve(f.handler, newRequest(t, method, "/mcp", bearer(keyGroup)))
		requireError(t, rec, http.StatusMethodNotAllowed, codeMethodNotAllowed)
		if rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s /mcp Allow = %q, want POST", method, rec.Header().Get("Allow"))
		}
	}
	requireError(t, serve(f.handler, newRequest(t, http.MethodOptions, "/mcp", bearer(keyGroup))), http.StatusMethodNotAllowed, codeMethodNotAllowed)
}

func TestMCPAnswersOnlyTheAllowedMethods(t *testing.T) {
	f := newMCPFixture(t)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"x","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`
	for _, method := range []string{"resources/list", "resources/templates/list", "prompts/list", "logging/setLevel", "completion/complete", "subscriptions/listen"} {
		legacy := rawMCP(t, f.handler, keyGroup, legacyCall(method, `{"level":"debug"}`), legacyHeader)
		current := rawMCP(t, f.handler, keyGroup, legacyCall(method, `{"level":"debug",`+meta+`}`), http.Header{"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {method}})
		for i, rec := range []*httptest.ResponseRecorder{legacy, current} {
			framing := "application/json"
			if i == 0 && method == "subscriptions/listen" {
				framing = "text/event-stream"
			}
			if !strings.Contains(rec.Body.String(), `"code":-32601`) || rec.Header().Get("Content-Type") != framing || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s = %d %v %s, want method not found", method, rec.Code, rec.Header(), rec.Body.String())
			}
		}
	}
	cs := connect(t, f.handler, keyGroup, protocols[1])
	if err := cs.Ping(t.Context(), nil); err != nil {
		t.Fatalf("ping: %v", err)
	}
	for _, p := range protocols {
		cs := connect(t, f.handler, keyGroup, p)
		var refusals []string
		for _, name := range []string{"send_message", "nope", ""} {
			_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
			if err == nil || strings.Contains(err.Error(), "send_message") || strings.Contains(err.Error(), "nope") {
				t.Fatalf("%s: calling %q = %v, want the fixed unknown tool error", p.name, name, err)
			}
			refusals = append(refusals, err.Error())
		}
		if refusals[0] != refusals[1] {
			t.Fatalf("%s: a hidden tool and an unknown one differ: %q", p.name, refusals)
		}
	}
}

func TestMCPAuditsEveryPost(t *testing.T) {
	f := newMCPFixture(t)
	for _, body := range []string{
		legacyCall("tools/list", "{}"),
		legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"`+groupRef+`"}}`),
		legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":"`+aliceRef+`"}}`),
		legacyCall("tools/call", `{"name":"get_messages","arguments":{"chat":"`+groupRef+`","limit":1}}`),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		legacyCall("prompts/list", "{}"),
		legacyCall("tools/call", `{"name":"nope","arguments":{}}`),
		legacyCall("tools/call", `{"name":5}`),
		`{"jsonrpc":"2.0","id":1,"id":1}`,
	} {
		rawMCP(t, f.handler, keyGroup, body, legacyHeader)
	}
	type row struct {
		action string
		chat   policy.CanonicalChat
		ok     bool
		reason string
	}
	var got []row
	for _, e := range f.audit.recorded() {
		if e.Client != "client-group" {
			t.Fatalf("row for %q", e.Client)
		}
		got = append(got, row{e.Action, e.Chat, e.OK, e.Reason})
	}
	want := []row{
		{actionMCP, policy.CanonicalChat{}, true, outcomeOK},
		{"mcp.get_chat", groupChat, true, outcomeOK},
		{"mcp.get_chat", policy.CanonicalChat{}, false, codeNotFound},
		{"mcp.get_messages", groupChat, true, outcomeOK},
		{actionMCP, policy.CanonicalChat{}, true, outcomeOK},
		{actionMCP, policy.CanonicalChat{}, false, codeMethodNotFound},
		{actionMCP, policy.CanonicalChat{}, false, codeUnknownTool},
		{actionMCP, policy.CanonicalChat{}, false, codeBadRequest},
		{actionMCP, policy.CanonicalChat{}, false, codeInvalidBody},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit rows\n got %+v\nwant %+v", got, want)
	}
}

func TestMCPSharesTheReadAndSearchBudgetsWithREST(t *testing.T) {
	f := newMCPFixture(t)
	deps := testClientDeps(t, f.clients, f.clock.now)
	deps.Archive, deps.Audit, deps.Limits = f.archive, f.audit, ReadLimits{ReadsPerMinute: 3, SearchesPerMinute: 1}
	h := NewClientHandler(deps)
	if rec := serve(h, newRequest(t, http.MethodGet, "/v1/search?q=synthetic", bearer(keyGroup))); rec.Code != http.StatusOK {
		t.Fatalf("REST search = %d", rec.Code)
	}
	search := rawMCP(t, h, keyGroup, legacyCall("tools/call", `{"name":"search_messages","arguments":{"query":"synthetic"}}`), legacyHeader)
	if !strings.Contains(search.Body.String(), `"text":"rate_limited"`) {
		t.Fatalf("a search after the REST one = %s, want rate_limited", search.Body.String())
	}
	if rec := rawMCP(t, h, keyGroup, legacyCall("tools/list", "{}"), legacyHeader); rec.Code != http.StatusOK {
		t.Fatalf("third read = %d", rec.Code)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		rawMCP(t, h, keyGroup, legacyCall("tools/list", "{}"), legacyHeader),
		serve(h, newRequest(t, http.MethodGet, "/v1/chats", bearer(keyGroup))),
	} {
		requireError(t, rec, http.StatusTooManyRequests, codeRateLimited)
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("a refused read carries no Retry-After")
		}
	}
}

func TestMCPAnswersBehindALoopbackProxyWithAnyHost(t *testing.T) {
	f := newMCPFixture(t)
	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp", strings.NewReader(legacyCall("tools/list", "{}")))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	r.Host = "wa.example.test"
	r.Header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Authorization": {"Bearer " + keyGroup}, "Mcp-Protocol-Version": {legacyProtocol}}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("POST /mcp with a forwarded host = %d %v", resp.StatusCode, resp.Header)
	}
}

func TestMCPVerifierTakesThePipelinesClient(t *testing.T) {
	if _, err := pipelineClient(t.Context(), "ignored", nil); !errors.Is(err, auth.ErrInvalidToken) || err.Error() != auth.ErrInvalidToken.Error() {
		t.Fatalf("no client = %v, want the bare invalid token error", err)
	}
	c := &policy.Client{ID: "client-x", ExpiresAt: mcpExpiry}
	info, err := pipelineClient(withClient(t.Context(), c), "ignored", nil)
	if err != nil || info.UserID != c.ID || !info.Expiration.Equal(c.ExpiresAt) || len(info.Scopes) != 0 {
		t.Fatalf("client = %+v, %v", info, err)
	}
}

func TestMCPToolListMatchesTheGoldenFiles(t *testing.T) {
	for _, name := range []string{"MCPGODEBUG", "JSONSCHEMAGODEBUG"} {
		if _, set := os.LookupEnv(name); set {
			t.Fatalf("%s is set: the golden tool list is defined with the libraries' defaults", name)
		}
	}
	f := newMCPFixture(t)
	validName := regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	for class, key := range map[string]string{"read_chats": keyGroup, "all_chats": keyAll} {
		rec := rawMCP(t, f.handler, key, legacyCall("tools/list", "{}"), legacyHeader)
		var reply struct {
			Result struct {
				Tools []map[string]any `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Result.Tools) != len(toolNames) {
			t.Fatalf("tools/list = %d %s", rec.Code, rec.Body.String())
		}
		for _, tool := range reply.Result.Tools {
			if name, _ := tool["name"].(string); !validName.MatchString(name) {
				t.Fatalf("tool name %q is not [a-z0-9_]", name)
			}
		}
		got, err := json.MarshalIndent(reply.Result.Tools, "", "  ")
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got = append(got, '\n')
		for _, leak := range []string{groupRef, aliceRef, "Synthetic Group", "Alice", "client-"} {
			if bytes.Contains(got, []byte(leak)) {
				t.Fatalf("the %s tool list carries %q", class, leak)
			}
		}
		path := filepath.Join("testdata", "mcp", "tools_"+class+".json")
		if *updateMCPTools {
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		want, err := fs.ReadFile(os.DirFS(filepath.Dir(path)), filepath.Base(path))
		if err != nil {
			t.Fatalf("read %s: %v (regenerate with -update-mcp-tools)", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("the %s tool list differs from %s (regenerate with -update-mcp-tools after review):\n%s", class, path, got)
		}
	}
}
