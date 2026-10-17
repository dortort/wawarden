package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	mcpPattern      = "POST /mcp"
	mcpServerName   = "wawarden"
	maxMCPBodyBytes = 64 << 10
	maxMCPDepth     = 16
	mcpCallDeadline = 10 * time.Second
	mcpOrphanSettle = 2 * time.Second
	mcpOrphanPanic  = "api.mcp.orphan"

	actionMCP = "mcp.protocol"

	codeBadRequest       = "bad_request"
	codeMethodNotFound   = "method_not_found"
	codeUnknownTool      = "unknown_tool"
	codeInvalidArguments = "invalid_arguments"
)

var mcpMethods = []string{"initialize", "notifications/initialized", "ping", "server/discover", "tools/list", "tools/call"}

type mcpEndpoint struct {
	handler http.Handler
	now     func() time.Time
	settles <-chan time.Time
}

type mcpClass struct{ write, media bool }

func newMCPEndpoint(s *reads, w *writes, now func() time.Time, deadline time.Duration, media inlineLimits) *mcpEndpoint {
	version := buildinfo.Read().Version
	servers := map[mcpClass]*mcp.Server{}
	for _, class := range []mcpClass{{}, {write: true}, {media: true}, {write: true, media: true}} {
		server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: version}, &mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		})
		kit := &toolkit{reads: s, writes: w, now: now, deadline: deadline, media: media}
		registerTools(server, kit)
		if class.media {
			registerMediaTools(server, kit)
		}
		if class.write {
			registerWriteTools(server, kit)
		}
		server.AddReceivingMiddleware(allowedMethods(kit.names))
		servers[class] = server
	}
	serverFor := func(r *http.Request) *mcp.Server {
		c := clientFrom(r.Context())
		_, write := policy.DecideWrite(c, now())
		g, _ := policy.DecideRead(c, now())
		return servers[mcpClass{write: write, media: s.media != nil && g.Media()}]
	}
	sdk := mcp.NewStreamableHTTPHandler(serverFor, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		MaxRequestBodyBytes:        maxMCPBodyBytes,
		DisableLocalhostProtection: true,
	})
	return &mcpEndpoint{handler: auth.RequireBearerToken(pipelineClient, nil)(sdk), now: now}
}

func (m *mcpEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	note := noteFrom(r.Context())
	if note != nil {
		note.action = actionMCP
	}
	body, refusal := mcpBody(w, r)
	if refusal != nil {
		if note != nil {
			note.reason = refusal.code
		}
		writeError(w, refusal.status, refusal.code)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	x := &exchange{action: actionMCP, reason: codeBadRequest, live: r.Context(), returned: make(chan struct{})}
	out := &mcpWriter{ResponseWriter: w, x: x, note: note}
	rc := http.NewResponseController(out)
	x.extend = func() error { return rc.SetWriteDeadline(m.now().Add(deliveryWindow)) }
	m.handler.ServeHTTP(out, r.WithContext(context.WithValue(r.Context(), exchangeKey{}, x)))
	hold := x.close()
	if out.started {
		hold.release(!out.failed && auditCommitted(w))
		return
	}
	m.orphan(x, hold)
	if r.Context().Err() != nil {
		// The client has left, so the header only commits the audit row of the call's outcome.
		out.WriteHeader(http.StatusOK)
		return
	}
	writeError(out, http.StatusInternalServerError, codeInternal)
}

func (m *mcpEndpoint) orphan(x *exchange, hold *deliveryHold) {
	if hold == nil {
		return
	}
	safego.Go(mcpOrphanPanic, func() {
		defer hold.orphan()
		<-x.returned
		if m.settles != nil {
			<-m.settles
			return
		}
		t := time.NewTimer(mcpOrphanSettle)
		defer t.Stop()
		<-t.C
	})
}

func mcpBody(w http.ResponseWriter, r *http.Request) ([]byte, *codedError) {
	body, err := readJSON(w, r, maxMCPBodyBytes)
	if refusal, ok := errors.AsType[*codedError](err); ok {
		return nil, refusal
	}
	if err != nil || !strictJSON(body, maxMCPDepth, anyKey) {
		return nil, errBadBody
	}
	return body, nil
}

func anyKey(string) bool { return true }

func pipelineClient(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	c := clientFrom(ctx)
	if c == nil {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: c.ID, Expiration: c.ExpiresAt}, nil
}

// The library answers a tool call on its own goroutine, which may still run after the request has ended.
type exchange struct {
	mu       sync.Mutex
	action   string
	chat     policy.CanonicalChat
	reason   string
	message  cursor.Ref
	bytes    int64
	closed   bool
	live     context.Context
	hold     *deliveryHold
	extend   func() error
	returned chan struct{}
	ended    sync.Once
}

type exchangeKey struct{}

func exchangeFrom(ctx context.Context) *exchange {
	x, _ := ctx.Value(exchangeKey{}).(*exchange)
	return x
}

func (x *exchange) settle(action string, chat policy.CanonicalChat, reason string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.action, x.chat, x.reason = action, chat, reason
}

func (x *exchange) settleMedia(action string, chat policy.CanonicalChat, reason string, bytes int64) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.action, x.chat, x.reason, x.bytes = action, chat, reason, bytes
}

func (x *exchange) noteMessage(message cursor.Ref) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.message = message
}

func (x *exchange) publish(ctx context.Context, hold *deliveryHold) bool {
	if x == nil || hold == nil {
		return false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed || ctx.Err() != nil || x.live.Err() != nil || x.extend() != nil {
		return false
	}
	x.hold = hold
	return true
}

func (x *exchange) close() *deliveryHold {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.closed = true
	hold := x.hold
	x.hold = nil
	return hold
}

func (x *exchange) handlerReturned() {
	if x != nil {
		x.ended.Do(func() { close(x.returned) })
	}
}

func (x *exchange) noteOn(n *auditNote, status int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	n.action, n.chat, n.reason = x.action, x.chat, x.reason
	if x.chat != (policy.CanonicalChat{}) {
		n.message, n.bytes = x.message, x.bytes
	}
	if status == http.StatusAccepted {
		n.reason = ""
	}
}

type mcpWriter struct {
	http.ResponseWriter
	x       *exchange
	note    *auditNote
	started bool
	failed  bool
}

func (m *mcpWriter) WriteHeader(status int) {
	if !m.started {
		m.started = true
		if m.note != nil {
			m.x.noteOn(m.note, status)
		}
		m.Header().Set("Cache-Control", "no-store")
	}
	m.ResponseWriter.WriteHeader(status)
}

func (m *mcpWriter) Write(p []byte) (int, error) {
	if !m.started {
		m.WriteHeader(http.StatusOK)
	}
	n, err := m.ResponseWriter.Write(p)
	if err != nil || n < len(p) {
		m.failed = true
	}
	return n, err
}

func (m *mcpWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

func allowedMethods(tools []string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			x := exchangeFrom(ctx)
			if !slices.Contains(mcpMethods, method) {
				x.settle(actionMCP, policy.CanonicalChat{}, codeMethodNotFound)
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
			}
			if call, ok := req.(*mcp.CallToolRequest); ok && (call.Params == nil || !slices.Contains(tools, call.Params.Name)) {
				x.settle(actionMCP, policy.CanonicalChat{}, codeUnknownTool)
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "unknown tool"}
			}
			x.settle(actionMCP, policy.CanonicalChat{}, "")
			return next(ctx, method, req)
		}
	}
}
