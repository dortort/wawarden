package archtest

import (
	"go/ast"
	"go/constant"
	"go/token"
)

const mainFile = "cmd/wawarden/main.go"

var preludeRule = rule{
	name:  "main-prelude",
	check: checkMainPrelude,
	cases: []snippet{
		{name: "the shipped prelude", rel: mainFile, src: `package main

import (
	"os"
	"runtime/debug"
	"syscall"
)

func main() {
	syscall.Umask(0o077)
	debug.SetTraceback("single")
	os.Exit(0)
}
`},
		{name: "aliased imports and an old-style octal mask", rel: mainFile, src: `package main

import (
	rd "runtime/debug"
	sc "syscall"
)

func main() {
	sc.Umask(077)
	rd.SetTraceback("single")
}
`},
		{name: "another file of the command", rel: "cmd/wawarden/run.go", src: `package main

func run() {}
`},
		{name: "a test of the command", rel: "cmd/wawarden/main_test.go", src: `package main

func main() {}
`},
		{name: "no umask", rel: mainFile, want: 1, src: `package main

import "runtime/debug"

func main() {
	debug.SetTraceback("single")
}
`},
		{name: "umask after other work", rel: mainFile, want: 1, src: `package main

import (
	"os"
	"runtime/debug"
	"syscall"
)

func main() {
	_ = os.MkdirAll("/data", 0o755)
	syscall.Umask(0o077)
	debug.SetTraceback("single")
}
`},
		{name: "a permissive mask", rel: mainFile, want: 1, src: `package main

import (
	"runtime/debug"
	"syscall"
)

func main() {
	syscall.Umask(0o022)
	debug.SetTraceback("single")
}
`},
		{name: "another package's Umask", rel: mainFile, want: 1, src: `package main

import (
	"runtime/debug"
	syscall "example.com/sys"
)

func main() {
	syscall.Umask(0o077)
	debug.SetTraceback("single")
}
`},
		{name: "a wider traceback", rel: mainFile, want: 1, src: `package main

import (
	"runtime/debug"
	"syscall"
)

func main() {
	syscall.Umask(0o077)
	debug.SetTraceback("all")
}
`},
		{name: "the umask deferred", rel: mainFile, want: 1, src: `package main

import (
	"runtime/debug"
	"syscall"
)

func main() {
	defer syscall.Umask(0o077)
	debug.SetTraceback("single")
}
`},
		{name: "no func main", rel: mainFile, want: 1, src: `package main

import "syscall"

type t struct{}

func (t) main() { syscall.Umask(0o077) }
`},
	},
}

func checkMainPrelude(f *sourceFile) []string {
	if f.rel != mainFile {
		return nil
	}
	for _, d := range f.file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "main" || fn.Body == nil {
			continue
		}
		body := fn.Body.List
		if len(body) >= 2 && f.callsWith(body[0], "syscall", "Umask", constant.MakeInt64(0o077)) &&
			f.callsWith(body[1], "runtime/debug", "SetTraceback", constant.MakeString("single")) {
			return nil
		}
		return []string{f.at(fn, `func main must begin with syscall.Umask(0o077) and then debug.SetTraceback("single"), before anything else runs`)}
	}
	return []string{f.at(f.file, "%s declares no func main", mainFile)}
}

func (f *sourceFile) callsWith(s ast.Stmt, pkg, name string, arg constant.Value) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	if sel, p := f.ref(call.Fun); sel == nil || p != pkg || sel.Sel.Name != name {
		return false
	}
	lit, ok := ast.Unparen(call.Args[0]).(*ast.BasicLit)
	if !ok {
		return false
	}
	v := constant.MakeFromLiteral(lit.Value, lit.Kind, 0)
	return v.Kind() == arg.Kind() && constant.Compare(v, token.EQL, arg)
}
