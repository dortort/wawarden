package archtest

import "go/ast"

const policyPath = module + "/" + policyDir

var grantFreeMethods = set("Query.Expression")

var scopedGrantRule = rule{
	name:  "scoped-grant-first",
	check: checkScopedGrantFirst,
	cases: []snippet{
		{name: "exported methods without a grant first", rel: scopedDir + "/x.go", want: 5, src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
)

type Reader struct{}

type Rows[T any] struct{}

func (r *Reader) Chats(ctx context.Context, g policy.ReadGrant) {}
func (Reader) Count() int                                         { return 0 }
func (r *Reader) Pointer(g *policy.ReadGrant)                     {}
func (r *Reader) Write(g policy.WriteGrant)                       {}
func (r Rows[T]) All(ctx context.Context) []T                     { return nil }
`},
		{name: "a grant type of another package", rel: scopedDir + "/x.go", want: 1, src: `package scoped

import policy "example.com/policy"

type Reader struct{}

func (r *Reader) Chats(g policy.ReadGrant) {}
`},
		{name: "reads with a grant first, unexported methods and types, functions and the pure query method", rel: scopedDir + "/x.go", src: `package scoped

import (
	"context"

	p "github.com/dortort/wawarden/internal/policy"
)

type Reader struct{}

type Query struct{}

type querier struct{}

func (q querier) QueryContext(ctx context.Context) {}
func (r *Reader) Chats(g p.ReadGrant, ctx context.Context) {}
func (r *Reader) Search(g, h p.ReadGrant)                  {}
func (r *Reader) read(ctx context.Context)                 {}
func (q Query) Expression() string                         { return "" }
func New() *Reader                                         { return nil }
`},
		{name: "a test of the scoped package", rel: scopedDir + "/x_test.go", src: `package scoped

func (r *Reader) Exec() {}
`},
		{name: "another store package", rel: "internal/store/admin/x.go", src: `package admin

type Reader struct{}

func (r *Reader) Counters() {}
`},
	},
}

func checkScopedGrantFirst(f *sourceFile) []string {
	if f.test || !within(f.dir, scopedDir) {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() || !ast.IsExported(receiverName(fn)) || grantFreeMethods[receiverName(fn)+"."+fn.Name.Name] {
			continue
		}
		params := fn.Type.Params.List
		if len(params) == 0 || isPointer(params[0].Type) || !f.isType(params[0].Type, policyPath, "ReadGrant") {
			out = append(out, f.at(fn, "%s.%s does not take a policy.ReadGrant as its first parameter: every read of %s takes the caller's grant first", receiverName(fn), fn.Name.Name, scopedDir))
		}
	}
	return out
}

func receiverName(fn *ast.FuncDecl) string {
	e := ast.Unparen(fn.Recv.List[0].Type)
	if star, ok := e.(*ast.StarExpr); ok {
		e = ast.Unparen(star.X)
	}
	switch x := e.(type) {
	case *ast.IndexExpr:
		e = x.X
	case *ast.IndexListExpr:
		e = x.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func isPointer(e ast.Expr) bool {
	_, ok := ast.Unparen(e).(*ast.StarExpr)
	return ok
}
