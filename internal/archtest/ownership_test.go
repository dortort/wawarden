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
		{name: "functions run on goroutines the standard library starts", rel: "internal/app/x.go", want: 9, src: `package app

import (
	stdctx "context"
	"net/http"
	"runtime"
	"sync"
	"time"
)

type group struct{ sync.WaitGroup }

func f(ctx stdctx.Context, wg *sync.WaitGroup, g group, srv *http.Server, fn func()) {
	_ = time.AfterFunc(0, fn)
	_ = stdctx.AfterFunc(ctx, fn)
	runtime.SetFinalizer(srv, nil)
	_ = runtime.AddCleanup(srv, func(int) {}, 0)
	wg.Go(fn)
	start := wg.Go
	g.Go(fn)
	srv.RegisterOnShutdown(fn)
	_ = (*sync.WaitGroup).Go
	_ = start
}
`},
		{name: "another package's Go", rel: "internal/safego/sub/x.go", want: 2, src: `package sub

import (
	"example.com/errgroup"
	"example.com/pool"
)

func f(g *errgroup.Group) {
	g.Go(nil)
	pool.Go(nil)
}
`},
		{name: "safego.Go and functions that start no goroutine", rel: "internal/app/x.go", src: `package app

import (
	"context"
	"os"
	"os/signal"
	"time"

	sg "github.com/dortort/wawarden/internal/safego"
	"example.com/clock"
)

func f(fn func()) {
	sg.Go("x", fn)
	_ = time.NewTimer(0)
	_ = time.After(0)
	_, _ = signal.NotifyContext(context.Background(), os.Interrupt)
	_ = clock.AfterFunc
}
`},
		{name: "go statement in safego", rel: "internal/safego/x.go", src: `package safego

import "time"

func f() {
	go f()
	_ = time.AfterFunc(0, f)
}
`},
		{name: "go statement in a test", rel: "internal/app/x_test.go", src: `package app

import (
	"net/http"
	"sync"
)

func f(wg *sync.WaitGroup, srv *http.Server) {
	go f(wg, srv)
	wg.Go(func() {})
	srv.RegisterOnShutdown(func() {})
}
`},
	},
}

var goroutineStarters = map[string]map[string]bool{
	"context": set("AfterFunc"),
	"runtime": set("AddCleanup", "SetFinalizer"),
	"time":    set("AfterFunc"),
}

var goroutineMethods = set("Go", "RegisterOnShutdown")

func checkGoroutines(f *sourceFile) []string {
	if f.test || f.dir == safegoDir {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			out = append(out, f.at(n, "go statement outside %s: start goroutines with safego.Go", safegoDir))
		case *ast.SelectorExpr:
			sel, p := f.ref(n)
			switch {
			case sel != nil && goroutineStarters[p][sel.Sel.Name]:
				out = append(out, f.at(n, "%s.%s runs its function on a goroutine that safego does not recover", p, sel.Sel.Name))
			case sel != nil && p == module+"/"+safegoDir:
			case goroutineMethods[n.Sel.Name]:
				out = append(out, f.at(n, "%s runs its function on a goroutine that safego does not recover: start goroutines with safego.Go", n.Sel.Name))
			}
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
		"ListenUnix", "ListenUnixgram", "FileListener", "FilePacketConn"),
	"crypto/tls":        set("Listen", "NewListener"),
	"net/http/httptest": set("NewServer", "NewTLSServer", "NewUnstartedServer"),
	"syscall": set("Socket", "Bind", "Listen", "Syscall", "Syscall6", "Syscall9", "RawSyscall", "RawSyscall6",
		"AllThreadsSyscall", "AllThreadsSyscall6"),
}

var loopbackNetworks = set("tcp", "tcp4", "udp", "udp4")

const everySocketOpener = `
import (
	"crypto/tls"
	"net"
	"net/http/httptest"
	"syscall"
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
	_ = net.FilePacketConn
	_ net.ListenConfig
	_ = tls.Listen
	_ = tls.NewListener
	_ = httptest.NewServer
	_ = httptest.NewTLSServer
	_ = httptest.NewUnstartedServer
	_ = syscall.Socket
	_ = syscall.Bind
	_ = syscall.Listen
	_ = syscall.Syscall
	_ = syscall.Syscall6
	_ = syscall.Syscall9
	_ = syscall.RawSyscall
	_ = syscall.RawSyscall6
	_ = syscall.AllThreadsSyscall
	_ = syscall.AllThreadsSyscall6
)
`

var socketRule = rule{
	name:  "sockets",
	check: checkSockets,
	cases: []snippet{
		{name: "every opener outside listeners", rel: "internal/app/x.go", want: 26, src: "package app\n" + everySocketOpener},
		{name: "every opener in a listeners subpackage", rel: "internal/listeners/sub/x.go", want: 26, src: "package sub\n" + everySocketOpener},
		{name: "raw sockets in a test", rel: "internal/listeners/x_test.go", want: 4, src: `package listeners

import sc "syscall"

func f() {
	fd, _ := sc.Socket(sc.AF_INET, sc.SOCK_STREAM, 0)
	_ = sc.Bind(fd, &sc.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	_ = sc.Listen(fd, 1)
	_, _, _ = sc.RawSyscall(sc.SYS_LISTEN, uintptr(fd), 1, 0)
}
`},
		{name: "socket inspection outside listeners", rel: "internal/listeners/listenertest/x.go", src: `package listenertest

import "syscall"

func f(fd int) {
	_, _ = syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	_, _ = syscall.Getsockname(fd)
	var lim syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
	syscall.Umask(0o077)
}
`},
		{name: "every opener in listeners", rel: "internal/listeners/x.go", src: "package listeners\n" + everySocketOpener},
		{name: "loopback and temporary unix listens in a test", rel: "internal/app/x_test.go", src: `package app

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func f(ctx context.Context, t *testing.T) {
	_, _ = (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	_, _ = (&net.ListenConfig{KeepAlive: -1}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
	_, _ = net.Listen("tcp", "127.0.0." + "1:0")
	_, _ = net.ListenPacket("udp", "127.0.0.1:9")
	_, _ = tls.Listen("tcp", "127.0.0.1:0", nil)
	_ = httptest.NewServer(nil)
	_ = httptest.NewUnstartedServer(nil)
	_, _ = (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "s"))
	_, _ = net.Listen("unix", filepath.Join(t.TempDir(), "a", "b"+".sock"))
}
`},
		{name: "other listens in a test", rel: "internal/listeners/x_test.go", want: 15, src: `package listeners

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func f(ctx context.Context, t *testing.T, addr, name string) {
	_, _ = net.Listen("unix", "/tmp/s")
	_, _ = net.Listen("unix", filepath.Join(os.TempDir(), "s"))
	_, _ = net.Listen("unix", filepath.Join(t.TempDir(), name))
	_, _ = net.Listen("unix", filepath.Join(t.TempDir(t), "s"))
	_, _ = net.Listen("unixpacket", filepath.Join(t.TempDir(), "s"))
	_, _ = net.Listen("tcp", filepath.Join(t.TempDir(), "s"))
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
				if !f.unreachableListen(network, address) {
					out = append(out, f.at(n, "a test may listen only on a constant 127.0.0.1 TCP or UDP address, or on a unix socket under t.TempDir()"))
				}
			}
		case *ast.SelectorExpr:
			sel, p := f.ref(n)
			if sel == nil || !socketOpeners[p][sel.Sel.Name] || vetted[sel] || f.test && p == "net/http/httptest" {
				break
			}
			out = append(out, f.at(sel, "%s.%s can open a socket outside %s", p, sel.Sel.Name, listenersDir))
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

func (f *sourceFile) unreachableListen(network, address ast.Expr) bool {
	n, ok := constString(network)
	switch {
	case !ok:
		return false
	case n == "unix":
		return f.underTempDir(address)
	case !loopbackNetworks[n]:
		return false
	}
	a, ok := constString(address)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(a)
	return err == nil && host == "127.0.0.1"
}

func (f *sourceFile) underTempDir(e ast.Expr) bool {
	join, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(join.Args) < 2 {
		return false
	}
	if sel, p := f.ref(join.Fun); sel == nil || p != "path/filepath" || sel.Sel.Name != "Join" {
		return false
	}
	dir, ok := ast.Unparen(join.Args[0]).(*ast.CallExpr)
	if !ok || len(dir.Args) != 0 {
		return false
	}
	method, ok := ast.Unparen(dir.Fun).(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "TempDir" {
		return false
	}
	if sel, _ := f.ref(method); sel != nil {
		return false
	}
	for _, elem := range join.Args[1:] {
		if _, ok := constString(elem); !ok {
			return false
		}
	}
	return true
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
		{name: "mux values and method values outside register.go", rel: "internal/app/x.go", want: 6, src: `package app

import "net/http"

type holder struct{ mux *http.ServeMux }

func f(h http.Handler, hs []holder) {
	var m http.ServeMux
	_ = &http.ServeMux{}
	_ = new(http.ServeMux)
	reg := m.HandleFunc
	reg("/", h.ServeHTTP)
	register := hs[0].mux.Handle
	register("/", h)
}
`},
		{name: "route internals outside register.go", rel: "internal/api/extra.go", want: 6, src: `package api

import "net/http"

func f(rt *router, h http.HandlerFunc) {
	rt.register(route{pattern: "GET /x", class: classRead}, h)
	_ = rt.register
	_ = (*router).register
	_ = decided[int]
	rt.mux.ServeHTTP(nil, nil)
	_ = router{}.mux
}
`},
		{name: "mux in register.go", rel: muxFile, src: `package api

import "net/http"

type router struct{ mux *http.ServeMux }

func (rt *router) register(pattern string, h http.HandlerFunc) { rt.mux.Handle(pattern, h) }

func f(h http.Handler) {
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("/", h.ServeHTTP)
	rt := &router{mux: mux}
	rt.register("/", decided[int](nil, nil))
}
`},
		{name: "routes through the policy helpers elsewhere in api", rel: "internal/api/api.go", src: `package api

func f(p *pipeline, mux int) {
	p.router.admin("GET /metrics", nil)
	p.router.read("GET /r", nil)
	p.router.write("POST /w", nil)
	_ = p.router.routes
	_ = mux
}
`},
		{name: "route internals in an api test", rel: "internal/api/x_test.go", src: `package api

func f(rt *router) {
	rt.register(route{}, nil)
	_ = decided[int]
	_ = rt.mux
}
`},
		{name: "methods named register and fields named mux in other packages", rel: "internal/metrics/x.go", src: `package metrics

type registry struct{ mux int }

func (r *registry) register(name string) int { return r.mux }

func decided() {}

func f(r *registry) {
	_ = r.register("x")
	decided()
}
`},
		{name: "mux in a test", rel: "internal/app/x_test.go", src: `package app

import "net/http"

func f(h http.Handler) {
	var m http.ServeMux
	m.Handle("/", h)
	http.NewServeMux().Handle("/", h)
}
`},
		{name: "declaring a Handle method", rel: "internal/app/x.go", src: `package app

type h struct{}

func (h) Handle() {}
`},
		{name: "a package-level Handle of another package", rel: "internal/app/x.go", src: `package app

import (
	"example.com/http"
	"example.com/win"
)

var (
	_ win.Handle
	_ = http.ServeMux{}
)
`},
	},
}

func checkMux(f *sourceFile) []string {
	if f.test || f.rel == muxFile {
		return nil
	}
	var out []string
	internal := func(id *ast.Ident) {
		out = append(out, f.at(id, "%s outside %s reaches the mux without the grant-minting read, write or admin helpers", id.Name, muxFile))
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && f.dir == apiDir && id.Name == "decided" {
			internal(id)
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if f.dir == apiDir && (sel.Sel.Name == "register" || sel.Sel.Name == "mux") {
			internal(sel.Sel)
		}
		if s, p := f.ref(sel); s != nil {
			if p == "net/http" && (s.Sel.Name == "NewServeMux" || s.Sel.Name == "ServeMux") {
				out = append(out, f.at(sel, "http.%s outside %s", s.Sel.Name, muxFile))
			}
			return true
		}
		if sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc" {
			out = append(out, f.at(sel, "%s outside %s registers a route without a policy class", sel.Sel.Name, muxFile))
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
		{name: "ListenAndServe promoted through embedding", rel: "internal/app/x.go", want: 4, src: `package app

import "net/http"

type embedded struct{ *http.Server }

type nested struct{ embedded }

func f(e embedded, n *nested, anon struct{ *http.Server }) {
	_ = e.ListenAndServe()
	_ = n.ListenAndServeTLS("", "")
	_ = n.embedded.ListenAndServe()
	_ = anon.ListenAndServeTLS("", "")
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
