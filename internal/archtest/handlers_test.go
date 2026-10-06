package archtest

import (
	"go/ast"
	"go/token"
)

var handlerBuilders = map[string]map[string]bool{
	"net/http": set("AllowQuerySemicolons", "CrossOriginProtection", "FileServer", "FileServerFS", "HandlerFunc", "MaxBytesHandler",
		"NewCrossOriginProtection", "NotFoundHandler", "RedirectHandler", "StripPrefix", "TimeoutHandler"),
	"net/http/httputil": set("NewSingleHostReverseProxy", "ReverseProxy"),
	"net/rpc":           set("DefaultServer", "NewServer", "Server"),
	mcpModule + "/mcp": set("NewSSEHandler", "NewStreamableHTTPHandler", "SSEHandler", "SSEServerTransport", "StreamableHTTPHandler",
		"StreamableServerTransport"),
	mcpModule + "/auth": set("ProtectedResourceMetadataHandler", "RequireBearerToken"),
}

var handlerRule = rule{
	name:  "handler-ownership",
	check: checkHandlers,
	cases: []snippet{
		{name: "handlers built or wrapped outside api", rel: "internal/app/x.go", want: 13, src: `package app

import (
	web "net/http"
	"net/http/httputil"
	"net/rpc"
)

type open struct{ next web.Handler }

func (o open) ServeHTTP(w web.ResponseWriter, r *web.Request) { o.next.ServeHTTP(w, r) }

func (o *open) wrap() (web.Handler, error) { return o, nil }

func f(h web.Handler, fn func(web.ResponseWriter, *web.Request)) {
	_ = web.HandlerFunc(fn)
	_ = web.StripPrefix("/x", h)
	_ = web.TimeoutHandler(h, 0, "")
	_ = web.MaxBytesHandler(h, 1)
	_ = web.AllowQuerySemicolons(h)
	_ = web.NotFoundHandler()
	_ = web.FileServerFS(nil)
	_ = web.NewCrossOriginProtection().Handler(h)
	_ = httputil.NewSingleHostReverseProxy(nil)
	_ = rpc.NewServer()
	_ = func() web.Handler { return nil }
}
`},
		{name: "handlers built or swapped in listeners", rel: "internal/listeners/x.go", want: 6, src: `package listeners

import "net/http"

type Spec struct{ Handler http.Handler }

func (s Spec) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.Handler.ServeHTTP(w, r) }

func f(spec Spec, h http.Handler) []*http.Server {
	srv := &http.Server{Handler: spec.Handler}
	_ = &srv.Handler
	return []*http.Server{
		{Handler: http.TimeoutHandler(spec.Handler, 0, "")},
		{Handler: h},
		{ReadTimeout: 1},
	}
}
`},
		{name: "specs that do not carry an api handler unchanged", rel: "internal/app/x.go", want: 5, src: `package app

import (
	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/listeners"
	other "example.com/api"
)

func f(d api.ClientDeps, h interface{ ServeHTTP() }, wrap func(any) any) []listeners.Spec {
	s := listeners.Spec{Name: "client", Handler: api.NewClientHandler(d)}
	s.Handler = nil
	return []listeners.Spec{
		s,
		{Name: "client", Handler: wrap(api.NewClientHandler(d))},
		{Name: "admin", Handler: other.NewAdminHandler(d)},
		{"health", s.Addr, api.NewHealthHandler(nil)},
		{Name: "unset"},
	}
}
`},
		{name: "specs converted or changed through a pointer", rel: "internal/app/x.go", want: 4, src: `package app

import (
	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/listeners"
)

func f(ready func() bool, local struct{ Name string }, p *struct{ Name string }) []listeners.Spec {
	s := listeners.Spec{Name: "health", Handler: api.NewHealthHandler(ready)}
	h := &s.Handler
	*h = api.NewHealthHandler(ready)
	_ = &(s.Handler)
	return []listeners.Spec{s, listeners.Spec(local), *(*listeners.Spec)(p)}
}
`},
		{name: "aliases of the spec", rel: "internal/app/x.go", want: 2, src: `package app

import (
	"net/http"

	"github.com/dortort/wawarden/internal/listeners"
)

type spec = listeners.Spec

type specPointer = *listeners.Spec

type wrapped struct{ http.Handler }

func f(s listeners.Spec, h http.Handler) []spec {
	_ = spec(s)
	return []spec{{Name: "client", Handler: wrapped{h}}}
}
`},
		{name: "MCP handlers built or wrapped outside api", rel: "internal/api/dto/x.go", want: 8, src: `package dto

import (
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func f() {
	_ = mcp.NewStreamableHTTPHandler(nil, nil)
	_ = mcp.NewSSEHandler(nil, nil)
	_ = &mcp.StreamableHTTPHandler{}
	_ = &mcp.SSEHandler{}
	_ = &mcp.SSEServerTransport{}
	_ = &mcp.StreamableServerTransport{}
	_ = auth.RequireBearerToken(nil, nil)
	_ = auth.ProtectedResourceMetadataHandler(nil)
}
`},
		{name: "MCP handlers in api", rel: "internal/api/x.go", src: `package api

import (
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var _ = auth.RequireBearerToken(nil, nil)(mcp.NewStreamableHTTPHandler(nil, nil))
`},
		{name: "a handler built in an api subpackage", rel: "internal/api/dto/x.go", want: 1, src: `package dto

import "net/http"

var _ = http.HandlerFunc(nil)
`},
		{name: "the app hands the api handlers to the listeners", rel: "internal/app/x.go", src: `package app

import (
	"net/http"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/listeners"
)

type serving interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}

func f(d api.ClientDeps, ready func() bool, h http.Handler) []listeners.Spec {
	specs := []listeners.Spec{{Name: "client", Handler: api.NewClientHandler(d)}}
	return append(specs, listeners.Spec{Name: "health", Handler: (api.NewHealthHandler(ready))})
}
`},
		{name: "listeners serve the handler their spec carries", rel: "internal/listeners/x.go", src: `package listeners

import "net/http"

type Spec struct {
	Name    string
	Handler http.Handler
}

func f(spec Spec) *http.Server {
	spec = Spec(spec)
	return &http.Server{Handler: spec.Handler, ReadHeaderTimeout: 1}
}
`},
		{name: "handlers in api", rel: "internal/api/x.go", src: `package api

import "net/http"

type router struct{}

func (rt *router) ServeHTTP(http.ResponseWriter, *http.Request) {}

func NewHealthHandler(ready func() bool) http.Handler {
	return http.StripPrefix("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}
`},
		{name: "handlers and servers in tests", rel: "internal/listeners/x_test.go", src: `package listeners

import "net/http"

func f(h http.Handler) http.Handler {
	_ = Spec{Handler: http.NotFoundHandler()}
	_ = &http.Server{Handler: h}
	return http.HandlerFunc(nil)
}
`},
		{name: "another package's handler builders", rel: "internal/app/x.go", src: `package app

import "example.com/http"

var _ = http.HandlerFunc(nil)

func f() http.Handler { return nil }
`},
	},
}

func checkHandlers(f *sourceFile) []string {
	if f.test || f.dir == apiDir {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Recv != nil && n.Name.Name == "ServeHTTP" {
				out = append(out, f.at(n.Name, "a ServeHTTP method outside %s makes a handler that no policy-classed registration guards", apiDir))
			}
		case *ast.FuncType:
			if n.Results == nil {
				break
			}
			for _, r := range n.Results.List {
				if f.isType(r.Type, "net/http", "Handler") {
					out = append(out, f.at(r, "a function returning an http.Handler outside %s builds or wraps a handler", apiDir))
				}
			}
		case *ast.SelectorExpr:
			if sel, p := f.ref(n); sel != nil && handlerBuilders[p][sel.Sel.Name] {
				out = append(out, f.at(n, "%s.%s outside %s builds or wraps a handler", p, sel.Sel.Name, apiDir))
			}
		case *ast.AssignStmt:
			if within(f.dir, listenersDir) {
				break
			}
			for _, lhs := range n.Lhs {
				if sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr); ok && sel.Sel.Name == "Handler" {
					out = append(out, f.at(lhs, "Handler assigned after its listeners.Spec literal: set it only in the literal, to the result of an %s constructor", apiDir))
				}
			}
		case *ast.UnaryExpr:
			if sel, ok := ast.Unparen(n.X).(*ast.SelectorExpr); ok && n.Op == token.AND && sel.Sel.Name == "Handler" {
				out = append(out, f.at(n, "the address of a Handler field taken outside %s, through which it could be changed after its literal", apiDir))
			}
		case *ast.TypeSpec:
			if n.Assign.IsValid() && f.isType(n.Type, module+"/"+listenersDir, listenerSpec) && !within(f.dir, listenersDir) {
				out = append(out, f.at(n, "type %s is an alias of listeners.Spec, whose literals and conversions this rule then cannot see", n.Name.Name))
			}
		case *ast.CallExpr:
			if f.isType(n.Fun, module+"/"+listenersDir, listenerSpec) && !within(f.dir, listenersDir) {
				out = append(out, f.at(n, "conversion to listeners.Spec: build it with a literal whose Handler is the direct result of an %s constructor", apiDir))
			}
		}
		return true
	})
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		handler := keyedValue(lit, "Handler")
		switch {
		case f.isType(typ, module+"/"+listenersDir, listenerSpec) && !within(f.dir, listenersDir) && !f.apiCall(handler):
			out = append(out, f.at(lit, "a listeners.Spec must set Handler by name to the direct result of an %s constructor, so no listener serves a wrapped or foreign handler", apiDir))
		case f.dir == listenersDir && f.isType(typ, "net/http", "Server") && !specHandler(handler):
			out = append(out, f.at(lit, "an http.Server in %s must serve its spec's Handler unchanged", listenersDir))
		}
	})
	return out
}

func keyedValue(lit *ast.CompositeLit, key string) ast.Expr {
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return kv.Value
			}
		}
	}
	return nil
}

func (f *sourceFile) apiCall(e ast.Expr) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, p := f.ref(call.Fun)
	return sel != nil && p == module+"/"+apiDir
}

func specHandler(e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Handler"
}
