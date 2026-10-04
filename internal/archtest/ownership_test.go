package archtest

import (
	"go/ast"
	"go/token"
	"net"
)

const (
	safegoDir    = "internal/safego"
	listenersDir = "internal/listeners"
	muxFile      = "internal/api/register.go"
)

var goroutineRule = rule{
	name:  "goroutines",
	check: checkGoroutines,
	cases: []snippet{
		{name: "go statement", rel: "internal/app/x.go", want: 2, src: `package app

func f() {
	go f()
	go func() {}()
}
`},
		{name: "go statement in a safego subpackage", rel: "internal/safego/sub/x.go", want: 1, src: `package sub

func f() { go f() }
`},
		{name: "go statement in safego", rel: "internal/safego/x.go", src: `package safego

func f() { go f() }
`},
		{name: "go statement in a test", rel: "internal/app/x_test.go", src: `package app

func f() { go f() }
`},
	},
}

func checkGoroutines(f *sourceFile) []string {
	if f.test || f.dir == safegoDir {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			out = append(out, f.at(g, "go statement outside %s: start goroutines with safego.Go", safegoDir))
		}
		return true
	})
	return out
}

var bannedHTTP = set("ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS", "DefaultServeMux", "Handle", "HandleFunc",
	"DefaultClient", "Get", "Post", "PostForm", "Head")

var httpBanProofs = map[string]map[string]bool{
	"internal/api/coverage_test.go": set("DefaultServeMux"),
	"internal/app/app_test.go":      set("DefaultServeMux"),
}

const everyBannedHTTP = `
var (
	_ = web.ListenAndServe
	_ = web.ListenAndServeTLS
	_ = web.Serve
	_ = web.ServeTLS
	_ = web.DefaultServeMux
	_ = web.Handle
	_ = web.HandleFunc
	_ = web.DefaultClient
	_ = web.Get
	_ = web.Post
	_ = web.PostForm
	_ = web.Head
)
`

var netHTTPRule = rule{
	name:  "net-http-identifiers",
	check: checkNetHTTP,
	cases: []snippet{
		{name: "every banned identifier through an alias", rel: "internal/app/x.go", want: 12, src: `package app

import web "net/http"
` + everyBannedHTTP},
		{name: "every banned identifier in a test", rel: "internal/app/x_test.go", want: 12, src: `package app

import web "net/http"
` + everyBannedHTTP},
		{name: "a ban proof uses only its listed identifier", rel: "internal/api/coverage_test.go", want: 1, src: `package api

import "net/http"

var (
	_ = http.DefaultServeMux
	_ = http.DefaultClient
)
`},
		{name: "a listed ban proof", rel: "internal/app/app_test.go", src: `package app

import "net/http"

var _ = http.DefaultServeMux
`},
		{name: "permitted identifiers", rel: "internal/app/x.go", src: `package app

import "net/http"

var (
	_ = http.MethodGet
	_ = http.NewRequestWithContext
	_ = http.StatusNotFound
	_ http.Handler
)
`},
		{name: "another package named http", rel: "internal/app/x.go", src: `package app

import "example.com/http"

var _ = http.Get
`},
		{name: "blank import", rel: "internal/app/x.go", src: `package app

import _ "net/http"
`},
	},
}

func checkNetHTTP(f *sourceFile) []string {
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if s, p := f.ref(sel); s == nil || p != "net/http" || !bannedHTTP[s.Sel.Name] || f.test && httpBanProofs[f.rel][s.Sel.Name] {
			return true
		}
		out = append(out, f.at(sel, "net/http.%s is banned", sel.Sel.Name))
		return true
	})
	return out
}

var dotImportRule = rule{
	name:  "dot-imports",
	check: checkDotImports,
	cases: []snippet{
		{name: "dot imports", rel: "internal/app/x_test.go", want: 2, src: `package app

import (
	. "fmt"
	. "net/http"
)
`},
		{name: "named and blank imports", rel: "internal/app/x.go", src: `package app

import (
	_ "embed"
	web "net/http"
)
`},
	},
}

func checkDotImports(f *sourceFile) []string {
	var out []string
	for _, imp := range f.imports {
		if imp.name == "." {
			out = append(out, f.at(imp.node, "dot import of %q hides which package an identifier comes from", imp.path))
		}
	}
	return out
}

var socketOpeners = map[string]map[string]bool{
	"net": set("Listen", "ListenConfig", "ListenIP", "ListenMulticastUDP", "ListenPacket", "ListenTCP", "ListenUDP",
		"ListenUnix", "ListenUnixgram", "FileListener"),
	"crypto/tls":        set("Listen", "NewListener"),
	"net/http/httptest": set("NewServer", "NewTLSServer", "NewUnstartedServer"),
}

var loopbackNetworks = set("tcp", "tcp4", "udp", "udp4")

const everySocketOpener = `
import (
	"crypto/tls"
	"net"
	"net/http/httptest"
)

var (
	_ = net.Listen
	_ = net.ListenIP
	_ = net.ListenMulticastUDP
	_ = net.ListenPacket
	_ = net.ListenTCP
	_ = net.ListenUDP
	_ = net.ListenUnix
	_ = net.ListenUnixgram
	_ = net.FileListener
	_ net.ListenConfig
	_ = tls.Listen
	_ = tls.NewListener
	_ = httptest.NewServer
	_ = httptest.NewTLSServer
	_ = httptest.NewUnstartedServer
)
`

var socketRule = rule{
	name:  "sockets",
	check: checkSockets,
	cases: []snippet{
		{name: "every opener outside listeners", rel: "internal/app/x.go", want: 15, src: "package app\n" + everySocketOpener},
		{name: "every opener in a listeners subpackage", rel: "internal/listeners/sub/x.go", want: 15, src: "package sub\n" + everySocketOpener},
		{name: "every opener in listeners", rel: "internal/listeners/x.go", src: "package listeners\n" + everySocketOpener},
		{name: "loopback listens in a test", rel: "internal/app/x_test.go", src: `package app

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
)

func f(ctx context.Context) {
	_, _ = (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	_, _ = (&net.ListenConfig{KeepAlive: -1}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
	_, _ = net.Listen("tcp", "127.0.0." + "1:0")
	_, _ = net.ListenPacket("udp", "127.0.0.1:9")
	_, _ = tls.Listen("tcp", "127.0.0.1:0", nil)
	_ = httptest.NewServer(nil)
	_ = httptest.NewUnstartedServer(nil)
}
`},
		{name: "other listens in a test", rel: "internal/listeners/x_test.go", want: 9, src: `package listeners

import (
	"context"
	"crypto/tls"
	"net"
)

func f(ctx context.Context, addr string) {
	_, _ = net.Listen("tcp", "0.0.0.0:0")
	_, _ = net.Listen("tcp", ":0")
	_, _ = net.Listen("tcp6", "127.0.0.1:0")
	_, _ = net.Listen("unix", "127.0.0.1:0")
	_, _ = net.Listen("tcp", addr)
	_, _ = (&net.ListenConfig{}).Listen(ctx, "tcp", "[::1]:0")
	_, _ = net.ListenTCP("tcp", nil)
	var lc net.ListenConfig
	_, _ = lc.Listen(ctx, "tcp", "127.0.0.1:0")
	_ = tls.NewListener(nil, nil)
}
`},
	},
}

func checkSockets(f *sourceFile) []string {
	if !f.test && f.dir == listenersDir {
		return nil
	}
	var out []string
	vetted := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if !f.test {
				break
			}
			if opener, network, address := f.testListen(n); opener != nil {
				vetted[opener] = true
				if !loopbackListen(network, address) {
					out = append(out, f.at(n, "a test may listen only on a constant 127.0.0.1 TCP or UDP address"))
				}
			}
		case *ast.SelectorExpr:
			sel, p := f.ref(n)
			if sel == nil || !socketOpeners[p][sel.Sel.Name] || vetted[sel] || f.test && p == "net/http/httptest" {
				break
			}
			out = append(out, f.at(sel, "%s.%s opens a socket outside %s", p, sel.Sel.Name, listenersDir))
		}
		return true
	})
	return out
}

func (f *sourceFile) testListen(call *ast.CallExpr) (opener *ast.SelectorExpr, network, address ast.Expr) {
	arg := func(i int) ast.Expr {
		if i < len(call.Args) {
			return call.Args[i]
		}
		return nil
	}
	if sel, p := f.ref(call.Fun); sel != nil {
		if p == "net" && (sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenPacket") || p == "crypto/tls" && sel.Sel.Name == "Listen" {
			return sel, arg(0), arg(1)
		}
		return nil, nil, nil
	}
	method, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "Listen" && method.Sel.Name != "ListenPacket" {
		return nil, nil, nil
	}
	addr, ok := ast.Unparen(method.X).(*ast.UnaryExpr)
	if !ok || addr.Op != token.AND {
		return nil, nil, nil
	}
	lit, ok := ast.Unparen(addr.X).(*ast.CompositeLit)
	if !ok {
		return nil, nil, nil
	}
	if sel, p := f.ref(lit.Type); sel != nil && p == "net" && sel.Sel.Name == "ListenConfig" {
		return sel, arg(1), arg(2)
	}
	return nil, nil, nil
}

func loopbackListen(network, address ast.Expr) bool {
	n, ok := constString(network)
	if !ok || !loopbackNetworks[n] {
		return false
	}
	a, ok := constString(address)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(a)
	return err == nil && host == "127.0.0.1"
}

var muxRule = rule{
	name:  "mux-ownership",
	check: checkMux,
	cases: []snippet{
		{name: "mux outside register.go", rel: "internal/api/routes.go", want: 4, src: `package api

import web "net/http"

func f(h web.Handler) {
	mux := web.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("/", h.ServeHTTP)
	var s struct{ Handle func(string, web.Handler) }
	(s.Handle)("/", h)
}
`},
		{name: "mux in register.go", rel: muxFile, src: `package api

import "net/http"

func f(h http.Handler) {
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("/", h.ServeHTTP)
}
`},
		{name: "mux in a test", rel: "internal/app/x_test.go", src: `package app

import "net/http"

func f(h http.Handler) { http.NewServeMux().Handle("/", h) }
`},
		{name: "declaring a Handle method", rel: "internal/app/x.go", src: `package app

type h struct{}

func (h) Handle() {}
`},
	},
}

func checkMux(f *sourceFile) []string {
	if f.test || f.rel == muxFile {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if sel, p := f.ref(n); sel != nil && p == "net/http" && sel.Sel.Name == "NewServeMux" {
				out = append(out, f.at(n, "http.NewServeMux outside %s", muxFile))
			}
		case *ast.CallExpr:
			if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
				out = append(out, f.at(n, "a %s call outside %s registers a route without a policy class", sel.Sel.Name, muxFile))
			}
		}
		return true
	})
	return out
}

var serverRule = rule{
	name:  "server-ownership",
	check: checkServers,
	cases: []snippet{
		{name: "servers outside listeners", rel: "internal/app/x.go", want: 5, src: `package app

import web "net/http"

func f(h web.Handler) {
	_ = &web.Server{Handler: h}
	_ = []web.Server{{Handler: h}}
	_ = new(web.Server)
	_ = new((web.Server))
}
`},
		{name: "servers in a listeners test", rel: "internal/listeners/x_test.go", want: 2, src: `package listeners

import "net/http"

var (
	_ = http.Server{Handler: http.NotFoundHandler()}
	_ http.Server
)
`},
		{name: "servers in listeners without a handler", rel: "internal/listeners/x.go", want: 6, src: `package listeners

import "net/http"

var (
	_ = &http.Server{Addr: "127.0.0.1:0"}
	_ = &http.Server{Handler: nil}
	_ = []*http.Server{{ReadTimeout: 1}}
	_ = map[string]http.Server{"a": {}}
	_ = new(http.Server)
)
`},
		{name: "zero-value servers in listeners", rel: "internal/listeners/x.go", want: 7, src: `package listeners

import (
	"net"
	"net/http"
)

type wrap struct{ http.Server }

type holder struct{ srv http.Server }

type alias = http.Server

type defined http.Server

func f(ln net.Listener) error {
	var srv http.Server
	_ = make([]http.Server, 1)
	_ = http.Server(srv)
	return srv.Serve(ln)
}
`},
		{name: "ListenAndServe on any receiver", rel: "internal/app/x_test.go", want: 3, src: `package app

type srv struct{}

func (srv) ListenAndServe() error            { return nil }
func (srv) ListenAndServeTLS(string, string) error { return nil }

func f(s srv) {
	_ = s.ListenAndServe()
	_ = s.ListenAndServeTLS("", "")
	start := s.ListenAndServe
	_ = start
}
`},
		{name: "servers in listeners with a handler", rel: "internal/listeners/x.go", src: `package listeners

import (
	"net"
	"net/http"
)

type server struct {
	http *http.Server
}

type wrap struct{ *http.Server }

func f(h http.Handler, ln net.Listener) error {
	var later *http.Server
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 1}
	later = srv
	_ = []*http.Server{{Handler: h}}
	_ = server{http: later}
	return srv.Serve(ln)
}
`},
		{name: "server pointers in a test", rel: "internal/app/x_test.go", src: `package app

import "net/http"

func f(srv *http.Server) *http.Server { return srv }
`},
		{name: "another package's Server", rel: "internal/app/x.go", src: `package app

import "example.com/http"

var (
	_ = &http.Server{}
	_ http.Server
)
`},
	},
}

func checkServers(f *sourceFile) []string {
	var out []string
	pointerOrLiteral := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.StarExpr:
			if sel, _ := f.ref(n.X); sel != nil {
				pointerOrLiteral[sel] = true
			}
		case *ast.CompositeLit:
			if sel, _ := f.ref(n.Type); sel != nil {
				pointerOrLiteral[sel] = true
			}
		case *ast.SelectorExpr:
			if n.Sel.Name == "ListenAndServe" || n.Sel.Name == "ListenAndServeTLS" {
				out = append(out, f.at(n, "%s is banned: servers run only on listeners that %s opened", n.Sel.Name, listenersDir))
			}
			if sel, p := f.ref(n); sel != nil && p == "net/http" && sel.Sel.Name == "Server" && !pointerOrLiteral[sel] {
				out = append(out, f.at(n, "http.Server used as a value type: a zero http.Server has a nil Handler, which serves http.DefaultServeMux; hold a *http.Server built by a literal"))
			}
		}
		return true
	})
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		switch {
		case !f.isType(typ, "net/http", "Server"):
		case f.test || f.dir != listenersDir:
			out = append(out, f.at(lit, "http.Server built outside %s", listenersDir))
		case !setsHandler(lit):
			out = append(out, f.at(lit, "http.Server without an explicit non-nil Handler serves http.DefaultServeMux"))
		}
	})
	return out
}

func setsHandler(lit *ast.CompositeLit) bool {
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Handler" {
			v, isIdent := ast.Unparen(kv.Value).(*ast.Ident)
			return !isIdent || v.Name != "nil"
		}
	}
	return false
}
