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
	"github.com/dortort/wawarden/internal/policy"
)

const (
	mcpPattern      = "POST /mcp"
	mcpServerName   = "wawarden"
	maxMCPBodyBytes = 64 << 10
	maxMCPDepth     = 16
	mcpCallDeadline = 10 * time.Second

	actionMCP = "mcp.protocol"

	codeBadRequest       = "bad_request"
	codeMethodNotFound   = "method_not_found"
	codeUnknownTool      = "unknown_tool"
	codeInvalidArguments = "invalid_arguments"
)

var mcpMethods = []string{"initialize", "notifications/initialized", "ping", "server/discover", "tools/list", "tools/call"}

type mcpEndpoint struct {
	handler http.Handler
}

func newMCPEndpoint(s *reads, now func() time.Time, deadline time.Duration) *mcpEndpoint {
	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: buildinfo.Version}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	k := &toolkit{reads: s, now: now, deadline: deadline}
	registerTools(server, k)
	server.AddReceivingMiddleware(allowedMethods(k.names))
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		MaxRequestBodyBytes:        maxMCPBodyBytes,
		DisableLocalhostProtection: true,
	})
	return &mcpEndpoint{handler: auth.RequireBearerToken(pipelineClient, nil)(sdk)}
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
	x := &exchange{action: actionMCP, reason: codeBadRequest}
	out := &mcpWriter{ResponseWriter: w, x: x, note: note}
	m.handler.ServeHTTP(out, r.WithContext(context.WithValue(r.Context(), exchangeKey{}, x)))
	if !out.started {
		writeError(out, http.StatusInternalServerError, codeInternal)
	}
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
	mu     sync.Mutex
	action string
	chat   policy.CanonicalChat
	reason string
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

func (x *exchange) noteOn(n *auditNote, status int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	n.action, n.chat, n.reason = x.action, x.chat, x.reason
	if status == http.StatusAccepted {
		n.reason = ""
	}
}

type mcpWriter struct {
	http.ResponseWriter
	x       *exchange
	note    *auditNote
	started bool
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
	return m.ResponseWriter.Write(p)
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
