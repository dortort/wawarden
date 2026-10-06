package archtest

import "go/ast"

const (
	mcpPackage      = mcpModule + "/mcp"
	mcpToolsFile    = apiDir + "/tools.go"
	mcpEndpointFile = apiDir + "/mcp.go"
	mcpToolHelper   = "readTool"
	mcpEndpointFunc = "newMCPEndpoint"
)

type mcpSite struct{ file, fn string }

var mcpServerSurface = map[string]mcpSite{
	"AddTool":                  {mcpToolsFile, mcpToolHelper},
	"NewServer":                {mcpEndpointFile, mcpEndpointFunc},
	"AddReceivingMiddleware":   {mcpEndpointFile, mcpEndpointFunc},
	"AddSendingMiddleware":     {mcpEndpointFile, mcpEndpointFunc},
	"AddPrompt":                {},
	"AddResource":              {},
	"AddResourceTemplate":      {},
	"AddReceivingCustomMethod": {},
}

var mcpRegistrationRule = rule{
	name:  "mcp-registration",
	check: checkMCPRegistration,
	cases: []snippet{
		{name: "tools registered outside readTool", rel: "internal/api/x.go", want: 7, src: `package api

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type wrapped struct{ *sdk.Server }

var _ = (*sdk.Server).AddTool

func f(server *sdk.Server, w wrapped, h sdk.ToolHandler) {
	server.AddTool(&sdk.Tool{Name: "x"}, h)
	add := server.AddTool
	add(nil, h)
	_ = (*sdk.Server).AddTool
	sdk.AddTool(server, &sdk.Tool{Name: "y"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return nil, nil, nil
	})
	sdk.AddTool[struct{}, any](server, nil, nil)
	w.AddTool(nil, h)
}
`},
		{name: "the rest of the server's surface outside newMCPEndpoint", rel: "internal/api/x.go", want: 7, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func f(server *mcp.Server, m mcp.Middleware) {
	_ = mcp.NewServer(nil, nil)
	server.AddReceivingMiddleware(m)
	server.AddSendingMiddleware(m)
	server.AddPrompt(nil, nil)
	server.AddResource(nil, nil)
	server.AddResourceTemplate(nil, nil)
	_ = mcp.AddReceivingCustomMethod[*struct{}, *mcp.CallToolResult]
}
`},
		{name: "a raw registration beside readTool in its own file", rel: mcpToolsFile, want: 2, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

type toolkit struct{}

func readTool(server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) { server.AddTool(tool, h) }

func (k *toolkit) readTool(server *mcp.Server, h mcp.ToolHandler) { server.AddTool(nil, h) }

func registerTools(server *mcp.Server, h mcp.ToolHandler) {
	readTool(server, &mcp.Tool{Name: "x"}, h)
	server.AddTool(&mcp.Tool{Name: "y"}, h)
}
`},
		{name: "the helpers moved to other files", rel: "internal/api/x.go", want: 3, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func readTool(server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) { server.AddTool(tool, h) }

func newMCPEndpoint(m mcp.Middleware) *mcp.Server {
	server := mcp.NewServer(nil, nil)
	server.AddReceivingMiddleware(m)
	return server
}
`},
		{name: "readTool and newMCPEndpoint swapped", rel: mcpEndpointFile, want: 1, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func newMCPEndpoint(m mcp.Middleware) *mcp.Server {
	server := mcp.NewServer(nil, nil)
	server.AddReceivingMiddleware(m)
	server.AddTool(nil, nil)
	return server
}
`},
		{name: "the MCP library's registrations outside the api", rel: "internal/app/x.go", want: 2, src: `package app

import sdk "github.com/modelcontextprotocol/go-sdk/mcp"

func f() {
	_ = sdk.NewServer(nil, nil)
	sdk.AddTool[struct{}, any](nil, nil, nil)
}
`},
		{name: "the tool helper", rel: mcpToolsFile, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func readTool(server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) {
	register := func() { server.AddTool(tool, h) }
	register()
}
`},
		{name: "the endpoint constructor", rel: mcpEndpointFile, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func newMCPEndpoint(in, out mcp.Middleware) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "x"}, nil)
	server.AddReceivingMiddleware(in)
	server.AddSendingMiddleware(out)
	return server
}
`},
		{name: "registrations in an api test", rel: "internal/api/x_test.go", src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func f(h mcp.ToolHandler) {
	server := mcp.NewServer(nil, nil)
	server.AddTool(&mcp.Tool{Name: "x"}, h)
}
`},
		{name: "methods and functions of the same names elsewhere", rel: "internal/api/x.go", src: `package api

import (
	other "example.com/mcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func f() {
	other.AddTool(nil)
	_ = other.NewServer
	_ = mcp.NewStreamableHTTPHandler
}
`},
		{name: "an AddTool method of another package's type", rel: "internal/metrics/x.go", src: `package metrics

type registry struct{}

func (registry) AddTool() {}

func f(r registry) { r.AddTool() }
`},
	},
}

func checkMCPRegistration(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		site := mcpSite{file: f.rel}
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			site.fn = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			allowed, guarded := mcpServerSurface[sel.Sel.Name]
			if !guarded {
				return true
			}
			if s, p := f.ref(sel); s != nil && p != mcpPackage || s == nil && f.dir != apiDir {
				return true
			}
			switch {
			case allowed.fn == "":
				out = append(out, f.at(sel.Sel, "%s adds a capability to the MCP server, which serves tools only", sel.Sel.Name))
			case allowed != site:
				out = append(out, f.at(sel.Sel, "%s outside %s in %s reaches the MCP server without the helper every tool and middleware goes through", sel.Sel.Name, allowed.fn, allowed.file))
			}
			return true
		})
	}
	return out
}
