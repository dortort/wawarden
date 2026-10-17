package archtest

import (
	"go/ast"
	"slices"
)

const (
	mcpPackage      = mcpModule + "/mcp"
	mcpToolsFile    = apiDir + "/tools.go"
	mcpEndpointFile = apiDir + "/mcp.go"
	mcpReadHelper   = "readTool"
	mcpWriteHelper  = "writeTool"
	mcpMediaHelper  = "mediaTool"
	mcpEndpointFunc = "newMCPEndpoint"
	mcpBlockBuilder = "mediaBlock"
)

var mcpToolHelpers = []string{mcpReadHelper, mcpWriteHelper, mcpMediaHelper}

type mcpSite struct{ file, fn string }

var mcpServerSurface = map[string][]mcpSite{
	"AddTool":                  {{mcpToolsFile, mcpReadHelper}, {mcpToolsFile, mcpWriteHelper}, {mcpToolsFile, mcpMediaHelper}},
	"NewServer":                {{mcpEndpointFile, mcpEndpointFunc}},
	"AddReceivingMiddleware":   {{mcpEndpointFile, mcpEndpointFunc}},
	"AddSendingMiddleware":     {{mcpEndpointFile, mcpEndpointFunc}},
	"AddPrompt":                nil,
	"AddResource":              nil,
	"AddResourceTemplate":      nil,
	"AddReceivingCustomMethod": nil,
}

var mcpMediaContent = map[string][]mcpSite{
	"ImageContent":     {{mcpToolsFile, mcpBlockBuilder}},
	"AudioContent":     {{mcpToolsFile, mcpBlockBuilder}},
	"EmbeddedResource": {{mcpToolsFile, mcpBlockBuilder}},
	"ResourceContents": {{mcpToolsFile, mcpBlockBuilder}},
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
		{name: "the tool helpers", rel: mcpToolsFile, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func readTool(server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) {
	register := func() { server.AddTool(tool, h) }
	register()
}

func writeTool(server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) { server.AddTool(tool, h) }
`},
		{name: "the write helper in another file", rel: "internal/api/sends.go", want: 1, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func writeTool(server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) { server.AddTool(tool, h) }
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
		{name: "the media helper beside the other helpers", rel: mcpToolsFile, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func mediaTool[In any](server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) { server.AddTool(tool, h) }

func mediaBlock(data []byte, mime string, audio, image bool) mcp.Content {
	switch {
	case image:
		return &mcp.ImageContent{Data: data, MIMEType: mime}
	case audio:
		return &mcp.AudioContent{Data: data, MIMEType: mime}
	}
	return &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "x", Blob: data}}
}
`},
		{name: "the media helper in another file", rel: apiDir + "/media.go", want: 1, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func mediaTool[In any](server *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) { server.AddTool(tool, h) }
`},
		{name: "media blocks built outside the block builder", rel: mcpToolsFile, want: 5, src: `package api

import sdk "github.com/modelcontextprotocol/go-sdk/mcp"

func attachmentResult(data []byte) *sdk.CallToolResult {
	image := &sdk.ImageContent{Data: data}
	var audio sdk.AudioContent
	resource := sdk.EmbeddedResource{Resource: new(sdk.ResourceContents)}
	_ = []sdk.Content{image, &audio, &resource}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "x"}}}
}

func readTool(raw []byte) any { return sdk.ImageContent{Data: raw} }
`},
		{name: "the block builder moved to another file", rel: apiDir + "/media.go", want: 2, src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func mediaBlock(data []byte) mcp.Content {
	return &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{Blob: data}}
}
`},
		{name: "media blocks in a test and fields of the same names elsewhere", rel: apiDir + "/x_test.go", src: `package api

import "github.com/modelcontextprotocol/go-sdk/mcp"

func check(res *mcp.CallToolResult) bool {
	_, ok := res.Content[0].(*mcp.ImageContent)
	return ok
}
`},
		{name: "a field named like a media block", rel: apiDir + "/x.go", src: `package api

type answer struct{ ImageContent, AudioContent []byte }

func f(a answer) int { return len(a.ImageContent) + len(a.AudioContent) }
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
			if allowed, built := mcpMediaContent[sel.Sel.Name]; built {
				if _, p := f.ref(sel); p == mcpPackage && !slices.Contains(allowed, site) {
					out = append(out, f.at(sel.Sel, "%s outside %s in %s builds an MCP media block that the result builder has not chosen", sel.Sel.Name, allowed[0].fn, allowed[0].file))
				}
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
			case len(allowed) == 0:
				out = append(out, f.at(sel.Sel, "%s adds a capability to the MCP server, which serves tools only", sel.Sel.Name))
			case !slices.Contains(allowed, site):
				out = append(out, f.at(sel.Sel, "%s outside %s in %s reaches the MCP server without the helper every tool and middleware goes through", sel.Sel.Name, allowed[0].fn, allowed[0].file))
			}
			return true
		})
	}
	return out
}
