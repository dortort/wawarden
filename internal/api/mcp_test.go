package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/cursor"
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
	writeNames   = []string{toolGetChanges, toolGetChat, toolGetMessages, toolListChats, toolSearchMessages, toolSendMessage}
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

type recordingTransport struct {
	handlerTransport
	mu     sync.Mutex
	bodies [][]byte
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.handlerTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	resp.Body = readCloser{bytes.NewReader(body)}
	return resp, nil
}

func (r *recordingTransport) last() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return nil
	}
	return r.bodies[len(r.bodies)-1]
}

func connectThrough(t *testing.T, rt http.RoundTripper, p protocol) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "wawarden-test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: mcpEndpointURL, HTTPClient: &http.Client{Transport: rt}, DisableStandaloneSSE: true}
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
			if caps, err := json.Marshal(cs.InitializeResult().Capabilities); err != nil || string(caps) != `{"tools":{}}` {
				t.Fatalf("capabilities = %s, %v, want tools only, without list changes", caps, err)
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

func probeWrites(f *readFixture, archive ReadArchive) *writes {
	return &writes{reads: probeReads(f, archive), sender: f.sender, rejected: func(string) {}, now: fixedNow}
}

func probeEndpoint(t *testing.T, f *readFixture, archive ReadArchive, deadline time.Duration) string {
	t.Helper()
	f.handler.(*pipeline).router.mcp("POST /probe/mcp", newMCPEndpoint(probeReads(f, archive), probeWrites(f, archive), fixedNow, deadline, defaultInlineLimits()))
	return "http://wawarden.invalid/probe/mcp"
}

func TestMCPEndpointRefusesARequestWithoutThePipelinesClient(t *testing.T) {
	f := newMCPFixture(t)
	endpoint := newMCPEndpoint(probeReads(f, f.archive), probeWrites(f, f.archive), fixedNow, mcpCallDeadline, defaultInlineLimits())
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

type panickingArchive struct{}

func (panickingArchive) Chats(policy.ReadGrant, context.Context, scoped.ChatPosition, int) (scoped.ChatPage, error) {
	panic(secretPanic{text: panicCanary})
}

func (panickingArchive) Chat(policy.ReadGrant, context.Context, string) (scoped.Chat, bool, error) {
	panic(secretPanic{text: panicCanary})
}

func (panickingArchive) Messages(policy.ReadGrant, context.Context, string, scoped.MessagePosition, scoped.Direction, int) (scoped.MessagePage, bool, error) {
	panic(secretPanic{text: panicCanary})
}

func (panickingArchive) Message(policy.ReadGrant, context.Context, policy.CanonicalChat, string, policy.CanonicalChat) (scoped.Message, bool, error) {
	panic(secretPanic{text: panicCanary})
}

func (panickingArchive) Changes(policy.ReadGrant, context.Context, string, scoped.ChangePosition, int) (scoped.ChangePage, bool, error) {
	panic(secretPanic{text: panicCanary})
}

func (panickingArchive) Search(policy.ReadGrant, context.Context, scoped.Query, string, scoped.SearchPosition, int) (scoped.SearchPage, bool, error) {
	panic(secretPanic{text: panicCanary})
}

func (panickingArchive) Attachment(policy.ReadGrant, context.Context, policy.CanonicalChat, string, policy.CanonicalChat, time.Time) (scoped.Attachment, bool, error) {
	panic(secretPanic{text: panicCanary})
}

func sampleArguments(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal the input schema of %s: %v", tool.Name, err)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode the input schema of %s: %v", tool.Name, err)
	}
	samples := map[string]any{"chat": groupRef, "query": "synthetic", "text": "synthetic hello", "idempotency_key": "sample-key-0001", "message": "m1_synthetic-sample"}
	args := map[string]any{}
	for _, name := range schema.Required {
		v, ok := samples[name]
		if !ok {
			t.Fatalf("no sample value for the required parameter %s of %s: add one", name, tool.Name)
		}
		args[name] = v
	}
	return args
}

func TestEveryListedToolAnswersThroughItsHelper(t *testing.T) {
	for _, p := range protocols {
		for _, class := range []struct {
			key   string
			names []string
		}{{keyGroup, toolNames}, {keyAlice, writeNames}, {keyGroupMedia, mediaNames}, {keyAliceMedia, mediaWriteNames}} {
			t.Run(p.name+"/"+class.key, func(t *testing.T) {
				listedToolsAnswerThroughTheirHelper(t, p, class.key, class.names)
			})
		}
	}
}

func listedToolsAnswerThroughTheirHelper(t *testing.T, p protocol, key string, names []string) {
	f := newMCPGrantFixture(t, true)
	f.clients[keyRevoked] = &policy.Client{ID: "client-revoked", ReadAll: true, Revoked: true, AllowMedia: true, ExpiresAt: mcpExpiry}
	logs := installPanicReporter(t, metrics.NewRegistry())
	probe := probeReads(f, panickingArchive{})
	probe.media = newMediaRegistry(*testMediaDeps(t, &fakeMedia{}), panickingArchive{}, metrics.NewRegistry(), fixedNow)
	f.handler.(*pipeline).router.mcp("POST /probe/mcp", newMCPEndpoint(probe, probeWrites(f, panickingArchive{}), fixedNow, mcpCallDeadline, defaultInlineLimits()))
	panicking := connectAt(t, f.handler, probeMCPURL, key, p)
	reader, revoked := connect(t, f.handler, key, p), connect(t, f.handler, keyRevoked, p)
	mref, err := f.refs.SealRef(f.clients[key].ID, cursor.Ref{Chat: groupChat.JID(), ID: "SYNTHETIC1", Sender: aliceChat.JID()})
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	tools, err := reader.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != len(names) {
		t.Fatalf("list tools = %v, %v", tools, err)
	}
	f.archive.mu.Lock()
	f.archive.err = errors.New("synthetic store failure " + argCanary)
	f.archive.mu.Unlock()
	for i, tool := range tools.Tools {
		if tool.Name != names[i] || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != (tool.Name != toolSendMessage) || tool.OutputSchema == nil {
			t.Errorf("%s is listed without the hint and output schema its helper gives it", tool.Name)
		}
		args, refused := sampleArguments(t, tool), requireToolError
		if tool.Name == toolGetAttachment {
			args["message"], refused = mref, requireAttachmentError
			if a := tool.Annotations; !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
				t.Errorf("%s is listed without the idempotent and open-world hints of mediaTool", tool.Name)
			}
		}
		if tool.Name == toolSendMessage || tool.Name == toolGetAttachment {
			if _, err := revoked.CallTool(t.Context(), &mcp.CallToolParams{Name: tool.Name, Arguments: args}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("a revoked client called %s: %v", tool.Name, err)
			}
		} else {
			refused(t, call(t, revoked, tool.Name, args), codeNotFound)
		}
		refused(t, call(t, panicking, tool.Name, args), codeInternal)
		res := call(t, reader, tool.Name, args)
		refused(t, res, codeInternal)
		if wire, _ := json.Marshal(res); bytes.Contains(wire, []byte(argCanary)) {
			t.Fatalf("%s put the store error on the wire: %s", tool.Name, wire)
		}
	}
	if strings.Contains(logs.String(), panicCanary) {
		t.Fatalf("the panic value leaked: %s", logs.String())
	}
}

func TestOnlyToolsRegisteredThroughTheHelpersAreCallable(t *testing.T) {
	f := newMCPFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "wawarden-test", Version: "0"}, nil)
	k := &toolkit{reads: probeReads(f, f.archive), writes: probeWrites(f, f.archive), now: fixedNow, deadline: mcpCallDeadline}
	registerTools(server, k)
	if names := slices.Sorted(slices.Values(k.names)); !slices.Equal(names, toolNames) {
		t.Fatalf("readTool recorded %q, want %q", names, toolNames)
	}
	registerWriteTools(server, k)
	if names := slices.Sorted(slices.Values(k.names)); !slices.Equal(names, writeNames) {
		t.Fatalf("readTool and writeTool recorded %q, want %q", names, writeNames)
	}
	registerMediaTools(server, k)
	if names := slices.Sorted(slices.Values(k.names)); !slices.Equal(names, mediaWriteNames) {
		t.Fatalf("readTool, writeTool and mediaTool recorded %q, want %q", names, mediaWriteNames)
	}
	server.AddTool(&mcp.Tool{Name: "unrecorded", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: argCanary}}}, nil
	})
	server.AddReceivingMiddleware(allowedMethods(k.names))
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverEnd, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "wawarden-test", Version: "0"}, nil).Connect(t.Context(), clientEnd, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "unrecorded"}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("a tool registered outside readTool answered %+v, %v", res, err)
	}
	requireToolError(t, call(t, cs, toolListChats, nil), codeNotFound)
	requireToolError(t, call(t, cs, toolSendMessage, map[string]any{"chat": aliceRef, "text": "synthetic", "idempotency_key": "no-client-0001"}), codeNotFound)
	requireAttachmentError(t, call(t, cs, toolGetAttachment, map[string]any{"message": "m1_synthetic-sample"}), codeNotFound)
	if calls := f.sender.requests(); len(calls) != 0 {
		t.Fatalf("a call without a write grant reached the sender: %+v", calls)
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
		t.Run(name, func(t *testing.T) {
			rec := rawMCP(t, f.handler, keyGroup, body, legacyHeader)
			requireError(t, rec, http.StatusBadRequest, codeInvalidBody)
			requireSecurityHeaders(t, rec.Header())
		})
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
	probe := rawMCP(t, h, keyAlice, legacyCall("tools/call", `{"name":"search_messages","arguments":{"query":"ab"}}`), legacyHeader)
	if !strings.Contains(probe.Body.String(), `"text":"invalid_arguments"`) {
		t.Fatalf("a search with invalid arguments = %s, want invalid_arguments", probe.Body.String())
	}
	requireError(t, serve(h, newRequest(t, http.MethodGet, "/v1/search?q=synthetic", bearer(keyAlice))), http.StatusTooManyRequests, codeRateLimited)
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

func TestMCPLibraryWritesNoLogLines(t *testing.T) {
	logs := &syncBuffer{}
	defaultLogger := slog.Default()
	defaultWriter, defaultFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() {
		slog.SetDefault(defaultLogger)
		log.SetOutput(defaultWriter)
		log.SetFlags(defaultFlags)
	}()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	stderr := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		stderr <- b
	}()
	defaultStderr := os.Stderr
	os.Stderr = w
	defer func() {
		os.Stderr = defaultStderr
		_ = w.Close()
	}()

	f := newMCPFixture(t)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"x","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`
	current := http.Header{"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"initialize"}}
	for _, c := range []struct {
		body   string
		header http.Header
	}{
		{legacyCall("initialize", `{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"x","version":"1"},`+meta+`}`), current},
		{legacyCall("tools/call", `{"name":5}`), legacyHeader},
		{legacyCall("tools/call", `{"name":"get_chat","arguments":{"chat":5}}`), legacyHeader},
		{legacyCall("no/such/method", "{}"), legacyHeader},
		{legacyCall("resources/list", "{}"), legacyHeader},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, legacyHeader},
	} {
		rawMCP(t, f.handler, keyGroup, c.body, c.header)
	}
	for _, p := range protocols {
		call(t, connect(t, f.handler, keyGroup, p), toolListChats, nil)
	}

	os.Stderr = defaultStderr
	if err := w.Close(); err != nil {
		t.Fatalf("close the pipe: %v", err)
	}
	if written := <-stderr; len(written) > 0 || logs.String() != "" {
		t.Fatalf("the library logged %q to the default logger and %q to standard error", logs.String(), written)
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
	type listing struct {
		key   string
		names []string
	}
	for _, p := range protocols {
		for class, c := range map[string]listing{"read_chats": {keyGroup, toolNames}, "all_chats": {keyAll, toolNames}, "write_chats": {keyAlice, writeNames}} {
			t.Run(p.name+"/"+class, func(t *testing.T) {
				toolListMatchesTheGoldenFile(t, f.handler, p, class, c.key, c.names, validName)
			})
		}
		for _, downloads := range []bool{false, true} {
			g := newMCPGrantFixture(t, downloads)
			classes := map[string]listing{
				"read_chats": {keyGroup, toolNames}, "all_chats": {keyAll, toolNames}, "write_chats": {keyAlice, writeNames},
			}
			media := map[string]listing{"read_chats": {keyGroupMedia, toolNames}, "all_chats": {keyAllMedia, toolNames}, "write_chats": {keyAliceMedia, writeNames}}
			if downloads {
				media = map[string]listing{
					"read_chats_media": {keyGroupMedia, mediaNames}, "all_chats_media": {keyAllMedia, mediaNames}, "write_chats_media": {keyAliceMedia, mediaWriteNames},
				}
			}
			for name, golden := range map[string]map[string]listing{"without allow_media": classes, "with allow_media": media} {
				for class, c := range golden {
					t.Run(fmt.Sprintf("%s/downloads %v/%s/%s", p.name, downloads, name, class), func(t *testing.T) {
						toolListMatchesTheGoldenFile(t, g.handler, p, class, c.key, c.names, validName)
					})
				}
			}
		}
	}
}

func toolListMatchesTheGoldenFile(t *testing.T, h http.Handler, p protocol, class, key string, names []string, validName *regexp.Regexp) {
	rec := &recordingTransport{handlerTransport: handlerTransport{handler: h, key: key}}
	cs := connectThrough(t, rec, p)
	if _, err := cs.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var reply struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	body := rec.last()
	if err := json.Unmarshal(body, &reply); err != nil || len(reply.Result.Tools) != len(names) {
		t.Fatalf("tools/list = %s", body)
	}
	for i, tool := range reply.Result.Tools {
		if tool["name"] != names[i] {
			t.Fatalf("tool %d is %v, want %s", i, tool["name"], names[i])
		}
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

const (
	keyGroupMedia = "synthetic-group-media-reader"
	keyAllMedia   = "synthetic-all-media-reader"
	keyAliceMedia = "synthetic-alice-media-writer"
	probeMCPURL   = "http://wawarden.invalid/probe/mcp"
)

var (
	mediaNames      = []string{toolGetAttachment, toolGetChanges, toolGetChat, toolGetMessages, toolListChats, toolSearchMessages}
	mediaWriteNames = []string{toolGetAttachment, toolGetChanges, toolGetChat, toolGetMessages, toolListChats, toolSearchMessages, toolSendMessage}
	beforeAudio     = protocol{name: protocolNoAudio, opts: &mcp.ClientSessionOptions{ProtocolVersion: protocolNoAudio}}
)

func testMediaDeps(t *testing.T, service MediaService) *MediaDeps {
	t.Helper()
	root, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() {
		stop()
		<-stopped
	})
	return &MediaDeps{
		Service: service, MaxBytes: 100 << 20, PerMinute: 30, GlobalPerHour: 600, GlobalBytesPerHour: 64 << 30, Root: root,
		ClientLive: func(context.Context, string) (bool, error) { return true, nil }, Stopped: func() { close(stopped) }, sweeps: make(chan time.Time),
	}
}

func withMedia(c *policy.Client) *policy.Client {
	m := *c
	m.ID, m.AllowMedia = c.ID+"-media", true
	return &m
}

func newMCPGrantFixture(t *testing.T, downloads bool) *readFixture {
	t.Helper()
	f := newMCPFixture(t)
	f.clients[keyGroupMedia], f.clients[keyAllMedia], f.clients[keyAliceMedia] = withMedia(f.clients[keyGroup]), withMedia(f.clients[keyAll]), withMedia(f.clients[keyAlice])
	deps := testClientDeps(t, f.clients, f.clock.now)
	deps.Archive, deps.Audit, deps.Cursors, deps.Refs, deps.Sender = f.archive, f.audit, f.cursors, f.refs, f.sender
	if downloads {
		deps.Media = testMediaDeps(t, &fakeMedia{})
	}
	f.handler = NewClientHandler(deps)
	return f
}

func newMCPMediaFixture(t *testing.T, o mediaOptions) *mediaFixture {
	t.Helper()
	f := newMediaFixture(t, o)
	for _, c := range f.clients {
		c.ExpiresAt = mcpExpiry
	}
	return f
}

type mcpWire struct {
	mu         sync.Mutex
	header     http.Header
	code       int
	body       bytes.Buffer
	events     []string
	deadlines  []time.Time
	onDeadline func()
	onWrite    func()
	failWrites bool
	discard    bool
	served     chan struct{}
}

func newMCPWire() *mcpWire { return &mcpWire{header: http.Header{}, served: make(chan struct{})} }

func (w *mcpWire) Header() http.Header { return w.header }

func (w *mcpWire) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = code
	}
	w.events = append(w.events, "header")
}

func (w *mcpWire) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	w.events = append(w.events, "write")
	hook, fail, discard := w.onWrite, w.failWrites, w.discard
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	switch {
	case fail:
		return 0, errors.New("synthetic: the connection was reset")
	case discard:
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *mcpWire) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, t)
	w.events = append(w.events, "deadline")
	hook := w.onDeadline
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (w *mcpWire) trace() ([]string, []time.Time, []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events), slices.Clone(w.deadlines), bytes.Clone(w.body.Bytes())
}

type wireTransport struct {
	handler http.Handler
	key     string
	mu      sync.Mutex
	prepare func(w *mcpWire)
	calls   []*mcpWire
}

func (t *wireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Body = readCloser{bytes.NewReader(body)}
	r.Header.Set("Authorization", "Bearer "+t.key)
	var msg struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &msg)
	w := newMCPWire()
	if msg.Method == "tools/call" {
		t.mu.Lock()
		prepare := t.prepare
		t.calls = append(t.calls, w)
		t.mu.Unlock()
		if prepare != nil {
			prepare(w)
		}
	}
	t.handler.ServeHTTP(w, r)
	close(w.served)
	w.mu.Lock()
	defer w.mu.Unlock()
	return &http.Response{StatusCode: w.code, Header: w.header.Clone(), Body: readCloser{bytes.NewReader(bytes.Clone(w.body.Bytes()))}, Request: r}, nil
}

func (t *wireTransport) last() *mcpWire {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls[len(t.calls)-1]
}

func connectWire(t *testing.T, rt http.RoundTripper, endpoint string, p protocol) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "wawarden-test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: rt}, DisableStandaloneSSE: true}
	cs, err := client.Connect(t.Context(), transport, p.opts)
	if err != nil {
		t.Fatalf("connect over %s: %v", p.name, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func (f *mediaFixture) session(key string, p protocol) (*mcp.ClientSession, *wireTransport) {
	rt := &wireTransport{handler: f.handler, key: key}
	return connectWire(f.t, rt, mcpEndpointURL, p), rt
}

func (f *mediaFixture) probe(settles <-chan time.Time) {
	e := newMCPEndpoint(f.reads, &writes{reads: f.reads, sender: &fakeSender{}, rejected: func(string) {}, now: f.clock.now}, f.clock.now, mcpCallDeadline, defaultInlineLimits())
	e.settles = settles
	f.handler.(*pipeline).router.mcp("POST /probe/mcp", e)
}

func (f *mediaFixture) attachmentArgs(key, id string) map[string]any {
	return map[string]any{"message": f.mref(key, id)}
}

func (f *mediaFixture) permits() int { return len(f.m.permit) }

type attachmentView struct {
	info  dto.AttachmentInfo
	block mcp.Content
}

func attachmentOf(t *testing.T, res *mcp.CallToolResult) attachmentView {
	t.Helper()
	if res.IsError {
		t.Fatalf("get_attachment refused: %+v", res.Content[0])
	}
	var v attachmentView
	if err := json.Unmarshal(structured(t, res), &v.info); err != nil {
		t.Fatalf("decode the structured content: %v", err)
	}
	n := len(res.Content)
	preface, ok1 := res.Content[0].(*mcp.TextContent)
	status, ok2 := res.Content[1].(*mcp.TextContent)
	closing, ok3 := res.Content[n-1].(*mcp.TextContent)
	if n < 3 || n > 4 || !ok1 || !ok2 || !ok3 || preface.Text != attachmentPreface || closing.Text != attachmentClosing || normalised(t, []byte(status.Text)) != normalised(t, structured(t, res)) {
		t.Fatalf("the result is not bracketed by the constant texts around the status: %+v", res.Content)
	}
	if n == 4 {
		v.block = res.Content[2]
	}
	return v
}

func blockBytes(t *testing.T, block mcp.Content) (kind, mime string, data []byte, meta mcp.Meta) {
	t.Helper()
	switch b := block.(type) {
	case *mcp.ImageContent:
		return "image", b.MIMEType, b.Data, b.Meta
	case *mcp.AudioContent:
		return "audio", b.MIMEType, b.Data, b.Meta
	case *mcp.EmbeddedResource:
		if b.Resource.URI != attachmentURI {
			t.Fatalf("a blob carries the URI %q, want the constant %q", b.Resource.URI, attachmentURI)
		}
		return "blob", b.Resource.MIMEType, b.Resource.Blob, b.Meta
	}
	t.Fatalf("the result carries no media block: %T", block)
	return "", "", nil, nil
}

func requireAttachmentError(t *testing.T, res *mcp.CallToolResult, code string) {
	t.Helper()
	text, ok := res.Content[0].(*mcp.TextContent)
	if !res.IsError || len(res.Content) != 1 || !ok || text.Text != code {
		t.Fatalf("result = %+v, want the error %s", res, code)
	}
	var shape dto.AttachmentInfo
	if err := json.Unmarshal(structured(t, res), &shape); err != nil || shape.RestRoute != mediaRoute || !shape.Untrusted || shape.Notice != mediaNotice || shape.Kind != "" || shape.Bytes != 0 {
		t.Fatalf("structured content %s is not the empty attachment shape", structured(t, res))
	}
}

type formatSpool struct {
	*fakeSpool
	format string
}

func (s formatSpool) Format() string { return s.format }

func mediaSpool(n int, served string, normalised bool) *fakeSpool {
	s := newFakeSpool(n, true)
	s.served, s.normalised = served, normalised
	return s
}

func TestAHiddenGetAttachmentAnswersLikeAnUnknownTool(t *testing.T) {
	for _, c := range []struct {
		name      string
		downloads bool
		key       string
	}{{"a client without allow_media", true, keyGroup}, {"downloads off", false, keyGroupMedia}} {
		for _, p := range protocols {
			t.Run(c.name+"/"+p.name, func(t *testing.T) {
				f := newMCPGrantFixture(t, c.downloads)
				cs := connect(t, f.handler, c.key, p)
				var refusals []string
				for _, name := range []string{toolGetAttachment, "nope"} {
					_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"message": argCanary}})
					if err == nil || strings.Contains(err.Error(), name) || strings.Contains(err.Error(), argCanary) {
						t.Fatalf("calling %q = %v, want the fixed unknown tool error", name, err)
					}
					refusals = append(refusals, err.Error())
				}
				if refusals[0] != refusals[1] {
					t.Fatalf("a hidden get_attachment and an unknown tool differ: %q", refusals)
				}
				rows := f.audit.recorded()
				for _, e := range rows[len(rows)-2:] {
					if e.Action != actionMCP || e.Reason != codeUnknownTool || e.OK || e.Chat != (policy.CanonicalChat{}) || e.Bytes != 0 {
						t.Fatalf("a hidden call was audited as %+v", e)
					}
				}
			})
		}
	}
}

func TestGetAttachmentAnswersEachFormWithItsBlock(t *testing.T) {
	for _, c := range []struct {
		name, kind, served string
		normalised         bool
		p                  protocol
		want               string
	}{
		{"a normalised image", "image", "image/jpeg", true, protocols[0], "image"},
		{"a normalised sticker", "sticker", "image/png", true, protocols[1], "image"},
		{"a document normalised as an image", "document", "image/jpeg", true, protocols[0], "image"},
		{"a voice note", "audio", "audio/ogg", false, protocols[0], "audio"},
		{"audio on the 2025-11-25 path", "audio", "audio/mp4", false, protocols[1], "audio"},
		{"audio before audio blocks existed", "audio", "audio/mpeg", false, beforeAudio, "blob"},
		{"audio served as bytes", "audio", "application/octet-stream", false, protocols[0], "blob"},
		{"a video", "video", "video/mp4", false, protocols[0], "blob"},
		{"a PDF", "document", "application/pdf", false, protocols[0], "blob"},
		{"a document without magic", "document", "application/octet-stream", false, protocols[1], "blob"},
		{"a Lottie sticker", "sticker", "application/octet-stream", false, protocols[0], "blob"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMCPMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", c.kind, smallSize)
			spool := mediaSpool(smallSize, c.served, c.normalised)
			f.ready(keyMedia, "MEDIA1", spool)
			cs, rt := f.session(keyMedia, c.p)
			v := attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "MEDIA1")))
			kind, mime, data, meta := blockBytes(t, v.block)
			if kind != c.want || mime != c.served || !bytes.Equal(data, spool.orig) {
				t.Fatalf("block = %s %s with %d bytes, want %s %s with the spool's %d", kind, mime, len(data), c.want, c.served, len(spool.orig))
			}
			if len(meta) != 1 || meta[untrustedMeta] != true {
				t.Fatalf("the block's _meta = %v, want only %s: true", meta, untrustedMeta)
			}
			i := v.info
			if i.Delivery != deliveryInline || i.Reason != nil || i.Offset != 0 || i.Length != smallSize || i.Bytes != smallSize || i.State != dto.MediaReady ||
				i.ServedType != c.served || i.Normalised != c.normalised || i.Kind != c.kind || i.Parts != 1 || i.RestRoute != mediaRoute || !i.Untrusted || i.Notice != mediaNotice {
				t.Fatalf("status = %+v", i)
			}
			events, deadlines, body := rt.last().trace()
			if len(deadlines) != 1 || !deadlines[0].Equal(f.clock.now().Add(deliveryWindow)) || slices.Index(events, "deadline") > slices.Index(events, "header") {
				t.Fatalf("the write deadline was not renewed to +30 s before the result was written: %v %v", events, deadlines)
			}
			if bytes.Contains(body, []byte(f.mref(keyMedia, "MEDIA1"))) {
				t.Fatal("the result echoes the message argument")
			}
			if f.view(keyMedia, "MEDIA1").state != 0 {
				t.Fatal("an inline result did not complete the job")
			}
			f.await("the spool to be cleared", func() bool { erasures, _ := spool.state(); return erasures == 1 })
			if fetching, staged := f.slots(); fetching != 0 || staged != 0 || f.permits() != 0 {
				t.Fatalf("%d fetching, %d staged, %d permits after the delivery", fetching, staged, f.permits())
			}
		})
	}
}

func TestGetAttachmentAuditsTheMessageAndTheInlineBytes(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.attach("MEDIA2", "video", 9<<20)
	f.ready(keyMedia, "MEDIA1", mediaSpool(smallSize, "application/pdf", false))
	cs, _ := f.session(keyMedia, protocols[0])
	call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "MEDIA1"))
	call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "MEDIA2"))
	requireAttachmentError(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "MISSING1")), codeNotFound)
	var got []ReadEvent
	for _, e := range f.audit.recorded() {
		if e.Action == "mcp."+toolGetAttachment {
			got = append(got, e)
		}
	}
	message := func(id string) cursor.Ref { return cursor.Ref{Chat: groupChat.JID(), ID: id, Sender: aliceChat.JID()} }
	want := []ReadEvent{
		{Action: "mcp." + toolGetAttachment, Chat: groupChat, OK: true, Reason: deliveryInline, Message: message("MEDIA1"), Bytes: smallSize},
		{Action: "mcp." + toolGetAttachment, Chat: groupChat, OK: true, Reason: reasonTooLargeForInline, Message: message("MEDIA2")},
		{Action: "mcp." + toolGetAttachment, OK: false, Reason: codeNotFound},
	}
	if len(got) != len(want) {
		t.Fatalf("audit rows %+v", got)
	}
	for i, e := range got {
		w := want[i]
		if e.Client != "client-media" || e.Chat != w.Chat || e.OK != w.OK || e.Reason != w.Reason || e.Message != w.Message || e.Bytes != w.Bytes {
			t.Fatalf("row %d = %+v, want %+v", i, e, w)
		}
	}
}

func TestGetAttachmentOverItsInlineLimitFetchesNothing(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{max: 12 << 20})
	f.attach("VIDEO1", "video", 9<<20)
	f.attach("STICKER1", "sticker", 2<<20)
	f.attach("IMAGE1", "image", 13<<20)
	cs, _ := f.session(keyMedia, protocols[0])
	for _, c := range []struct {
		id    string
		parts int
	}{{"VIDEO1", 2}, {"STICKER1", 1}, {"IMAGE1", 2}} {
		v := attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, c.id)))
		if v.block != nil || v.info.Delivery != deliveryPart || v.info.Reason == nil || *v.info.Reason != reasonTooLargeForInline || v.info.State != "" ||
			v.info.Parts != c.parts || v.info.Bytes != 0 || v.info.ExpiresAt != nil {
			t.Fatalf("%s: status = %+v", c.id, v.info)
		}
	}
	if fetched := f.media.fetched(); len(fetched) != 0 || len(f.media.prechecked) != 0 {
		t.Fatalf("a call over its inline limit reached the media service: %v", fetched)
	}
	if got := f.metric(`wawarden_media_fetches_total{result="too_large"}`); got != "0" {
		t.Fatalf("too_large fetches = %s, want none counted", got)
	}
	if w := f.post(keyMedia, "VIDEO1"); w.Code != http.StatusAccepted {
		t.Fatalf("the REST route refused a size the cap allows: %d", w.Code)
	}
}

func TestGetAttachmentWaitsOnAnInMemoryFetch(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	spool := mediaSpool(smallSize, "application/pdf", false)
	cs, _ := f.session(keyMedia, protocols[0])
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolGetAttachment, Arguments: f.attachmentArgs(keyMedia, "MEDIA1")})
		done <- res
	}()
	f.fetches(1)
	f.media.answer("MEDIA1", fetchAnswer{spool: spool})
	v := attachmentOf(t, <-done)
	if _, _, data, _ := blockBytes(t, v.block); !bytes.Equal(data, spool.orig) {
		t.Fatal("the waiting call did not return the fetched bytes")
	}
	if got := f.metric(`wawarden_media_deliveries_total{transport="mcp",result="ok"}`); got != "1" {
		t.Fatalf("mcp deliveries = %s, want 1", got)
	}
	if got := f.metric(`wawarden_media_delivered_bytes_total{transport="mcp"}`); got != strconv.Itoa(smallSize) {
		t.Fatalf("mcp delivered bytes = %s", got)
	}
}

func TestGetAttachmentReportsADiskJobWithoutWaiting(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	f.attach("DISK1", "document", diskSize)
	if w := f.post(keyMedia, "DISK1"); w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d", w.Code)
	}
	cs, _ := f.session(keyMedia, protocols[0])
	v := attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "DISK1")))
	if v.block != nil || v.info.State != dto.MediaFetching || v.info.Delivery != deliveryMetadata || v.info.Reason == nil || *v.info.Reason != reasonFetching ||
		v.info.RetryAfterSeconds == nil || *v.info.RetryAfterSeconds != 2 {
		t.Fatalf("a disk fetch answered %+v", v.info)
	}
	spool := newFakeSpool(smallSize, false)
	f.media.answer("DISK1", fetchAnswer{spool: spool})
	f.settled(keyMedia, "DISK1")
	v = attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "DISK1")))
	if v.block != nil || v.info.State != dto.MediaReady || v.info.Delivery != deliveryPart || v.info.Reason != nil || v.info.Bytes != smallSize {
		t.Fatalf("a ready disk job answered %+v", v.info)
	}
	if view := f.view(keyMedia, "DISK1"); view.state != mediaReady || f.holds(view.job) != 0 || f.permits() != 0 {
		t.Fatal("a metadata answer kept a hold or the permit, or ended the job")
	}
}

func TestGetAttachmentNeverSendsAnImageBlockForBytesTheNormaliserDidNotProduce(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	f.attach("RAW1", "image", smallSize)
	f.attach("BIG1", "image", 4<<20)
	f.ready(keyMedia, "RAW1", mediaSpool(smallSize, "image/jpeg", false))
	f.ready(keyMedia, "BIG1", mediaSpool(4<<20, "image/png", true))
	cs, _ := f.session(keyMedia, protocols[0])
	for id, reason := range map[string]*string{"RAW1": nil, "BIG1": new(reasonTooLargeForInline)} {
		v := attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, id)))
		if v.block != nil || v.info.Delivery != deliveryPart || (v.info.Reason == nil) != (reason == nil) || v.info.Parts != 1 {
			t.Fatalf("%s answered %+v with %T", id, v.info, v.block)
		}
		if view := f.view(keyMedia, id); view.state != mediaReady || f.holds(view.job) != 0 {
			t.Fatalf("%s: a metadata answer ended the job or kept its hold", id)
		}
	}
}

type pausedCall struct {
	published chan struct{}
	resume    chan struct{}
	resumed   sync.Once
	done      chan *mcp.CallToolResult
	errs      chan error
}

func (c *pausedCall) unblock() { c.resumed.Do(func() { close(c.resume) }) }

func (f *mediaFixture) pausedCall(ctx context.Context, cs *mcp.ClientSession, rt *wireTransport, key, id string) *pausedCall {
	c := &pausedCall{published: make(chan struct{}), resume: make(chan struct{}), done: make(chan *mcp.CallToolResult, 1), errs: make(chan error, 1)}
	rt.mu.Lock()
	rt.prepare = func(w *mcpWire) {
		w.onDeadline = func() {
			close(c.published)
			<-c.resume
		}
	}
	rt.mu.Unlock()
	args := f.attachmentArgs(key, id)
	f.t.Cleanup(c.unblock)
	go func() {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: toolGetAttachment, Arguments: args})
		c.done <- res
		c.errs <- err
	}()
	select {
	case <-c.published:
	case res := <-c.done:
		f.t.Fatalf("the call returned without publishing a result: %+v, %v", res, <-c.errs)
	}
	rt.mu.Lock()
	rt.prepare = nil
	rt.mu.Unlock()
	return c
}

func (c *pausedCall) finish(t *testing.T) *mcp.CallToolResult {
	t.Helper()
	c.unblock()
	res, err := <-c.done, <-c.errs
	if err != nil {
		t.Fatalf("the paused call failed: %v", err)
	}
	return res
}

func TestADeliveryHoldOutlivesEveryEraseUntilTheResultIsWritten(t *testing.T) {
	for _, trigger := range eraseTriggers {
		t.Run(trigger.name, func(t *testing.T) {
			f := newMCPMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", "document", smallSize)
			spool := mediaSpool(smallSize, "application/pdf", false)
			job := f.ready(keyMedia, "MEDIA1", spool).job
			cs, rt := f.session(keyMedia, protocols[0])
			paused := f.pausedCall(t.Context(), cs, rt, keyMedia, "MEDIA1")
			if f.holds(job) != 1 || f.permits() != 1 {
				t.Fatalf("published with %d holds and %d permits, want 1 and 1", f.holds(job), f.permits())
			}
			trigger.trigger(f)
			if v, _ := f.m.lookup(f.key(keyMedia, "MEDIA1"), false); v.state != 0 {
				t.Fatal("the trigger did not erase the job")
			}
			if erasures, intact := spool.state(); erasures != 0 || !intact {
				t.Fatalf("the spool was cleared while a published result referenced it: %d erasures, intact %v", erasures, intact)
			}
			cleared := make(chan bool, 1)
			rt.last().mu.Lock()
			rt.last().onWrite = func() {
				erasures, _ := spool.state()
				cleared <- erasures != 0 || f.permits() != 1 || f.holds(job) != 1
			}
			rt.last().mu.Unlock()
			v := attachmentOf(t, paused.finish(t))
			if early := <-cleared; early {
				t.Fatal("the hold, the permit or the spool was released before the library wrote the result")
			}
			if _, _, data, _ := blockBytes(t, v.block); !bytes.Equal(data, spool.orig) {
				t.Fatal("the delivered bytes differ from the source")
			}
			f.await("the release after the write", func() bool {
				erasures, _ := spool.state()
				return erasures == 1 && f.holds(job) == 0 && f.permits() == 0
			})
			if _, staged := f.slots(); staged != 0 {
				t.Fatalf("%d staged slots after the release", staged)
			}
		})
	}
}

func TestASecondInlineCallWhileOneIsPublishedAnswersBusy(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	job := f.ready(keyMedia, "MEDIA1", mediaSpool(smallSize, "application/pdf", false)).job
	cs, rt := f.session(keyMedia, protocols[0])
	paused := f.pausedCall(t.Context(), cs, rt, keyMedia, "MEDIA1")
	other, _ := f.session(keyMedia, protocols[1])
	res := call(t, other, toolGetAttachment, f.attachmentArgs(keyMedia, "MEDIA1"))
	requireAttachmentError(t, res, codeBusy)
	if f.holds(job) != 1 || f.permits() != 1 {
		t.Fatalf("the busy call left %d holds and %d permits, want the first call's 1 and 1", f.holds(job), f.permits())
	}
	attachmentOf(t, paused.finish(t))
	f.await("the release", func() bool { return f.holds(job) == 0 && f.permits() == 0 })
}

func TestAnOrphanedResultIsReleasedAfterTheHandlerAndTheSettle(t *testing.T) {
	for _, erased := range []bool{false, true} {
		t.Run(fmt.Sprintf("erased %v", erased), func(t *testing.T) {
			f := newMCPMediaFixture(t, mediaOptions{})
			settles := make(chan time.Time)
			f.probe(settles)
			f.attach("MEDIA1", "document", smallSize)
			spool := mediaSpool(smallSize, "application/pdf", false)
			job := f.ready(keyMedia, "MEDIA1", spool).job
			rt := &wireTransport{handler: f.handler, key: keyMedia}
			cs := connectWire(t, rt, probeMCPURL, protocols[0])
			ctx, cancel := context.WithCancel(t.Context())
			paused := f.pausedCall(ctx, cs, rt, keyMedia, "MEDIA1")
			cancel()
			f.awaitTheLibraryLettingGoOfTheCall()
			_, transient := f.m.lookup(f.key(keyMedia, "MEDIA1"), true)
			if erased {
				f.clock.advance(mediaRAMLife)
				f.ticks <- testNow
				f.ticks <- testNow
			}
			paused.unblock()
			<-paused.done
			if err := <-paused.errs; err == nil {
				t.Fatal("the cancelled call returned a result")
			}
			<-rt.last().served
			if events, _, _ := rt.last().trace(); slices.Contains(events, "write") {
				t.Fatalf("the library wrote the result of a call whose client had left: %v", events)
			}
			for range 20 {
				if f.holds(job) != 2 || f.permits() != 1 {
					t.Fatalf("before the settle: %d holds and %d permits, want 2 and 1", f.holds(job), f.permits())
				}
				time.Sleep(time.Millisecond)
			}
			settles <- time.Time{}
			f.await("the orphan release", func() bool { return f.holds(job) == 1 && f.permits() == 0 })
			transient.release()
			erasures, intact := spool.state()
			if erasures != 0 || !intact {
				t.Fatalf("a spool the library may still read was cleared: %d erasures, intact %v", erasures, intact)
			}
			if got := f.metric(`wawarden_media_deliveries_total{transport="mcp",result="ok"}`); got != "0" {
				t.Fatalf("an orphaned result counted %s deliveries", got)
			}
			f.m.mu.Lock()
			delivered := job.delivered
			f.m.mu.Unlock()
			_, staged := f.slots()
			switch {
			case delivered != 0:
				t.Fatalf("the job counted %d delivered bytes", delivered)
			case erased && staged != 0:
				t.Fatalf("%d staged slots after the dropped spool", staged)
			case !erased && f.view(keyMedia, "MEDIA1").state != mediaReady:
				t.Fatal("an orphaned delivery ended the job")
			}
		})
	}
}

// The library's stateless serve waits for the handler, so only the call's stack shows it has let go of the response.
func (f *mediaFixture) awaitTheLibraryLettingGoOfTheCall() {
	f.t.Helper()
	buf := make([]byte, 4<<20)
	f.await("the library to let go of the cancelled call", func() bool {
		for g := range bytes.SplitSeq(buf[:runtime.Stack(buf, true)], []byte("\n\n")) {
			if bytes.Contains(g, []byte("mcp.(*ClientSession).CallTool(")) && bytes.Contains(g, []byte("mcp.(*ServerSession).Close(")) {
				return true
			}
		}
		return false
	})
}

func TestACallWhoseClientLeftDuringAnInMemoryFetchPublishesNothing(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	settles := make(chan time.Time)
	f.probe(settles)
	t.Cleanup(func() { close(settles) })
	f.attach("MEDIA1", "document", smallSize)
	spool := mediaSpool(smallSize, "application/pdf", false)
	rt := &wireTransport{handler: f.handler, key: keyMedia}
	cs := connectWire(t, rt, probeMCPURL, protocols[0])
	ctx, cancel := context.WithCancel(t.Context())
	errs := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: toolGetAttachment, Arguments: f.attachmentArgs(keyMedia, "MEDIA1")})
		errs <- err
	}()
	f.fetches(1)
	cancel()
	f.awaitTheLibraryLettingGoOfTheCall()
	f.media.answer("MEDIA1", fetchAnswer{spool: spool})
	if err := <-errs; err == nil {
		t.Fatal("the cancelled call returned a result")
	}
	<-rt.last().served
	v := f.view(keyMedia, "MEDIA1")
	if v.state != mediaReady {
		t.Fatalf("the job settled in state %d, want ready", v.state)
	}
	if f.holds(v.job) != 0 || f.permits() != 0 {
		t.Fatalf("after the request: %d holds and %d permits, want both released without the settle", f.holds(v.job), f.permits())
	}
	if erasures, intact := spool.state(); erasures != 0 || !intact {
		t.Fatalf("the spool of a ready job was cleared: %d erasures, intact %v", erasures, intact)
	}
	if got := f.metric(`wawarden_media_deliveries_total{transport="mcp",result="ok"}`); got != "0" {
		t.Fatalf("a call whose client had left counted %s deliveries", got)
	}
	var got []ReadEvent
	for _, e := range f.audit.recorded() {
		if e.Action == "mcp."+toolGetAttachment {
			got = append(got, e)
		}
	}
	message := cursor.Ref{Chat: groupChat.JID(), ID: "MEDIA1", Sender: aliceChat.JID()}
	if len(got) != 1 || got[0].Chat != groupChat || !got[0].OK || got[0].Reason != deliveryMetadata || got[0].Message != message || got[0].Bytes != 0 {
		t.Fatalf("audit rows %+v, want one metadata row with no bytes", got)
	}
}

func TestAnOrphanedHoldWaitsForItsHandlerToReturn(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	f.attach("MEDIA1", "document", smallSize)
	f.ready(keyMedia, "MEDIA1", mediaSpool(smallSize, "application/pdf", false))
	v, held := f.m.lookup(f.key(keyMedia, "MEDIA1"), true)
	a, err := f.m.inline(v, held, defaultInlineLimits())
	if err != nil || a.hold == nil {
		t.Fatalf("no inline answer: %v", err)
	}
	settles := make(chan time.Time)
	x := &exchange{returned: make(chan struct{})}
	(&mcpEndpoint{settles: settles}).orphan(x, a.hold)
	select {
	case settles <- time.Time{}:
		t.Fatal("the settle began while the handler was still running")
	case <-time.After(50 * time.Millisecond):
	}
	if f.holds(v.job) != 1 || f.permits() != 1 {
		t.Fatalf("while the handler runs: %d holds and %d permits, want 1 and 1", f.holds(v.job), f.permits())
	}
	x.handlerReturned()
	settles <- time.Time{}
	f.await("the orphan release", func() bool { return f.holds(v.job) == 0 && f.permits() == 0 })
}

func TestAHandlerThatFindsTheExchangeClosedPublishesNothing(t *testing.T) {
	ended, end := context.WithCancel(context.Background())
	end()
	for name, x := range map[string]*exchange{
		"closed":                   {returned: make(chan struct{}), live: context.Background(), closed: true, extend: func() error { return nil }},
		"the deadline not renewed": {returned: make(chan struct{}), live: context.Background(), extend: func() error { return http.ErrNotSupported }},
		"the request ended":        {returned: make(chan struct{}), live: ended, extend: func() error { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", "document", smallSize)
			job := f.ready(keyMedia, "MEDIA1", mediaSpool(smallSize, "application/pdf", false)).job
			k := &toolkit{reads: f.reads, now: f.clock.now, media: defaultInlineLimits()}
			resolved, err := inputSchema[getAttachmentArgs]().Resolve(nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			raw, _ := json.Marshal(f.attachmentArgs(keyMedia, "MEDIA1"))
			ctx := context.WithValue(withClient(t.Context(), f.clients[keyMedia]), exchangeKey{}, x)
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: toolGetAttachment, Arguments: raw}}
			res, chat, reason, n := runMediaTool(ctx, k, resolved, req, func(ctx context.Context, g policy.ReadGrant, in getAttachmentArgs) (mediaAnswer, policy.CanonicalChat, error) {
				return f.reads.attachmentCall(ctx, g, in.Message, k.media)
			})
			v := attachmentOf(t, res)
			if v.block != nil || v.info.Delivery != deliveryMetadata || v.info.Length != 0 || chat != groupChat || reason != deliveryMetadata || n != 0 {
				t.Fatalf("answered %+v (%s, %d bytes) with %T", v.info, reason, n, v.block)
			}
			if f.holds(job) != 0 || f.permits() != 0 || x.hold != nil || f.view(keyMedia, "MEDIA1").state != mediaReady {
				t.Fatal("the handler kept its hold or the permit, published a hold, or ended the job")
			}
		})
	}
}

func TestAnUnwrittenOrUnauditedResultIsReleasedWithoutCompletingTheJob(t *testing.T) {
	for name, spoil := range map[string]func(f *mediaFixture, rt *wireTransport){
		"a failed write": func(_ *mediaFixture, rt *wireTransport) {
			rt.prepare = func(w *mcpWire) { w.failWrites = true }
		},
		"a failed audit row": func(f *mediaFixture, _ *wireTransport) {
			f.audit.mu.Lock()
			f.audit.fail = true
			f.audit.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPMediaFixture(t, mediaOptions{})
			f.attach("MEDIA1", "document", smallSize)
			spool := mediaSpool(smallSize, "application/pdf", false)
			job := f.ready(keyMedia, "MEDIA1", spool).job
			cs, rt := f.session(keyMedia, protocols[0])
			spoil(f, rt)
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolGetAttachment, Arguments: f.attachmentArgs(keyMedia, "MEDIA1")})
			if err == nil && !res.IsError {
				t.Fatal("the client received the result")
			}
			f.await("the release", func() bool { return f.holds(job) == 0 && f.permits() == 0 })
			if f.view(keyMedia, "MEDIA1").state != mediaReady {
				t.Fatal("a result the client never received completed the job")
			}
			if erasures, _ := spool.state(); erasures != 0 {
				t.Fatal("the spool of a live job was cleared")
			}
			if ok, incomplete := f.metric(`wawarden_media_deliveries_total{transport="mcp",result="ok"}`), f.metric(`wawarden_media_deliveries_total{transport="mcp",result="incomplete"}`); ok != "0" || incomplete != "1" {
				t.Fatalf("deliveries ok %s, incomplete %s, want 0 and 1", ok, incomplete)
			}
		})
	}
}

func TestAPanicAfterTheHoldIsTakenReleasesIt(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	logs := installPanicReporter(t, metrics.NewRegistry())
	f.attach("MEDIA1", "document", smallSize)
	spool := mediaSpool(smallSize, "application/pdf", false)
	job := f.ready(keyMedia, "MEDIA1", spool).job
	spool.mu.Lock()
	spool.hook = func(int) { panic(secretPanic{text: panicCanary}) }
	spool.mu.Unlock()
	cs, _ := f.session(keyMedia, protocols[0])
	requireAttachmentError(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "MEDIA1")), codeInternal)
	if f.holds(job) != 0 || f.permits() != 0 {
		t.Fatalf("a panic left %d holds and %d permits", f.holds(job), f.permits())
	}
	if strings.Contains(logs.String(), panicCanary) {
		t.Fatal("the panic value leaked")
	}
	f.m.drop(f.key(keyMedia, "MEDIA1"))
	if erasures, _ := spool.state(); erasures != 1 {
		t.Fatalf("the dropped spool was erased %d times", erasures)
	}
	if _, staged := f.slots(); staged != 0 {
		t.Fatalf("%d staged slots after the drop", staged)
	}
}

func (f *mediaFixture) readyWith(key, id string, spool MediaSpool) {
	f.t.Helper()
	if w := f.post(key, id); w.Code != http.StatusAccepted {
		f.t.Fatalf("POST = %d %s, want 202", w.Code, w.Body.String())
	}
	f.media.answer(id, fetchAnswer{spool: spool})
	if v := f.settled(key, id); v.state != mediaReady {
		f.t.Fatalf("the job settled in state %d, want ready", v.state)
	}
}

func TestBothTransportsServeTheJobsOneSpool(t *testing.T) {
	t.Run("a POST then a call: the normaliser's image", func(t *testing.T) {
		f := newMCPMediaFixture(t, mediaOptions{})
		f.attach("DOC1", "document", smallSize)
		normalisedJPEG := mediaSpool(smallSize, "image/jpeg", true)
		f.readyWith(keyMedia, "DOC1", formatSpool{fakeSpool: normalisedJPEG, format: "image"})
		cs, _ := f.session(keyMedia, protocols[0])
		v := attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "DOC1")))
		if kind, _, data, _ := blockBytes(t, v.block); kind != "image" || !bytes.Equal(data, normalisedJPEG.orig) || !v.info.Normalised || v.info.Format != "image" {
			t.Fatalf("a document normalised as an image answered %s %+v", kind, v.info)
		}
	})
	t.Run("a call then a GET: the same bytes", func(t *testing.T) {
		f := newMCPMediaFixture(t, mediaOptions{})
		f.attach("DOC1", "document", smallSize)
		normalisedJPEG := mediaSpool(smallSize, "image/jpeg", true)
		f.media.answer("DOC1", fetchAnswer{spool: formatSpool{fakeSpool: normalisedJPEG, format: "image"}})
		cs, rt := f.session(keyMedia, protocols[0])
		paused := f.pausedCall(t.Context(), cs, rt, keyMedia, "DOC1")
		rest := f.get(keyMedia, "DOC1", nil)
		v := attachmentOf(t, paused.finish(t))
		_, _, data, _ := blockBytes(t, v.block)
		if rest.Code != http.StatusOK || rest.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rest.Body.Bytes(), normalisedJPEG.orig) || !bytes.Equal(data, rest.Body.Bytes()) {
			t.Fatalf("GET = %d %s, and the call's bytes differ from it", rest.Code, rest.Header().Get("Content-Type"))
		}
	})
	refused := func(t *testing.T, f *mediaFixture) []byte {
		t.Helper()
		raw := mediaSpool(smallSize, "application/octet-stream", false)
		f.media.answer("DOC1", fetchAnswer{spool: formatSpool{fakeSpool: raw, format: "image"}})
		return raw.orig
	}
	requireRefusedImageBlob := func(t *testing.T, v attachmentView, want []byte) {
		t.Helper()
		kind, mime, data, _ := blockBytes(t, v.block)
		if kind != "blob" || mime != "application/octet-stream" || v.info.Normalised || v.info.Format != "image" || !bytes.Equal(data, want) {
			t.Fatalf("a JPEG document the normaliser refused answered %s %s %+v", kind, mime, v.info)
		}
	}
	t.Run("a refused JPEG document, the call first", func(t *testing.T) {
		f := newMCPMediaFixture(t, mediaOptions{})
		f.attach("DOC1", "document", smallSize)
		want := refused(t, f)
		cs, rt := f.session(keyMedia, protocols[0])
		paused := f.pausedCall(t.Context(), cs, rt, keyMedia, "DOC1")
		rest := f.get(keyMedia, "DOC1", nil)
		requireRefusedImageBlob(t, attachmentOf(t, paused.finish(t)), rest.Body.Bytes())
		if rest.Code != http.StatusOK || !bytes.Equal(rest.Body.Bytes(), want) {
			t.Fatalf("GET = %d with other bytes", rest.Code)
		}
	})
	t.Run("a refused JPEG document, the routes first", func(t *testing.T) {
		f := newMCPMediaFixture(t, mediaOptions{})
		f.attach("DOC1", "document", smallSize)
		if w := f.post(keyMedia, "DOC1"); w.Code != http.StatusAccepted {
			t.Fatalf("POST = %d", w.Code)
		}
		want := refused(t, f)
		f.settled(keyMedia, "DOC1")
		var rest []byte
		for _, r := range []string{"bytes=0-999", "bytes=1000-"} {
			w := f.get(keyMedia, "DOC1", http.Header{"Range": {r}})
			if w.Code != http.StatusPartialContent {
				t.Fatalf("GET %s = %d", r, w.Code)
			}
			rest = append(rest, w.Body.Bytes()...)
		}
		cs, _ := f.session(keyMedia, protocols[1])
		requireRefusedImageBlob(t, attachmentOf(t, call(t, cs, toolGetAttachment, f.attachmentArgs(keyMedia, "DOC1"))), rest)
		if !bytes.Equal(rest, want) {
			t.Fatal("the ranges differ from the spool")
		}
	})
}

func TestTheMarshalOfAnInlineBlobStaysWithinItsMemoryBound(t *testing.T) {
	f := newMCPMediaFixture(t, mediaOptions{})
	previous := debug.SetGCPercent(1)
	t.Cleanup(func() { debug.SetGCPercent(previous) })
	const calls = 5
	growths := make([]uint64, 0, calls)
	for i := range calls {
		id := fmt.Sprintf("BIG%d", i)
		f.clock.advance(time.Minute)
		f.attach(id, "video", mcpBlobCap)
		f.ready(keyMedia, id, mediaSpool(mcpBlobCap, "video/mp4", false))
		growths = append(growths, f.marshalGrowth(id))
	}
	// A concurrent collection can leave a buffer the library has dropped in the sampled heap, so the least growth is the measure.
	if least := slices.Min(growths); least > 6*mcpBlobCap {
		t.Fatalf("the library's marshal of an 8 MiB blob grew the heap by at least %d bytes (%v), over 6 times the blob", least, growths)
	}
	if got := f.metric(`wawarden_media_delivered_bytes_total{transport="mcp"}`); got != strconv.Itoa(calls*mcpBlobCap) {
		t.Fatalf("delivered %s bytes", got)
	}
}

func (f *mediaFixture) marshalGrowth(id string) uint64 {
	f.t.Helper()
	var stopped atomic.Bool
	peaks := make(chan uint64, 1)
	var base uint64
	w := newMCPWire()
	w.discard = true
	w.onDeadline = func() {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		base = ms.HeapAlloc
		go func() {
			var sample runtime.MemStats
			peak := base
			for !stopped.Load() {
				runtime.ReadMemStats(&sample)
				peak = max(peak, sample.HeapAlloc)
			}
			peaks <- peak
		}()
	}
	body := legacyCall("tools/call", `{"name":"get_attachment","arguments":{"message":"`+f.mref(keyMedia, id)+`"}}`)
	r := httptest.NewRequestWithContext(f.t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Authorization": {"Bearer " + keyMedia}, "Mcp-Protocol-Version": {legacyProtocol}}
	f.handler.ServeHTTP(w, r)
	stopped.Store(true)
	if events, _, _ := w.trace(); w.code != http.StatusOK || !slices.Contains(events, "write") || base == 0 {
		f.t.Fatalf("the call answered %d %v", w.code, events)
	}
	return <-peaks - base
}
