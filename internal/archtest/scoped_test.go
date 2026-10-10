package archtest

import (
	"go/ast"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	policyPath   = module + "/" + policyDir
	dbPath       = module + "/" + dbDir
	scopedReader = scopedDir + "/scoped.go"
	scopedRead   = "read"
)

var (
	grantFreeMethods  = set("Query.Expression")
	scopedReadMethods = set("Reader.Chat", "Reader.Chats", "Reader.Changes", "Reader.Message", "Reader.Messages", "Reader.Search")
)

const grantWideningRead = `package scoped

import (
	"context"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func (r *Reader) Everything(g policy.ReadGrant, ctx context.Context) (ChatPage, error) {
	wide, _ := policy.DecideRead(&policy.Client{ID: "x", ReadAll: true, ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	return r.Chats(wide, ctx, ChatPosition{}, 10)
}
`

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

type scopedSite struct{ file, decl string }

var scopedInternals = map[string][]scopedSite{
	".db":        {{scopedReader, "Reader.read"}},
	".Read":      {{scopedReader, "Reader.read"}},
	".raw":       {{inBuilder, "querier.QueryContext"}, {inBuilder, "querier.scalar"}},
	".scope":     {{inBuilder, "querier.QueryContext"}},
	".all":       {{inBuilder, "scope.expand"}},
	".jids":      {{inBuilder, "scope.expand"}},
	"querier{}":  {{scopedReader, "Reader.read"}},
	"scope{}":    {{inBuilder, "scopeOf"}},
	"db.DB":      {{scopedReader, "Reader"}, {scopedReader, "New"}},
	"db.Querier": {{scopedReader, "Reader.read"}, {inBuilder, "querier"}},
}

var (
	scopedFields = set("db", "Read", "Write", "raw", "scope", "all", "jids")
	scopedTypes  = set("querier", "scope")
)

var scopedReadPathRule = rule{
	name:  "scoped-read-path",
	check: checkScopedReadPath,
	cases: []snippet{
		{name: "reads around the scoped querier", rel: scopedDir + "/x.go", want: 15, src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

type alias = querier

type copied scope

func (r *Reader) Texts(g policy.ReadGrant, ctx context.Context) error {
	return r.db.Read(ctx, "x", func(ctx context.Context, q db.Querier) error {
		_, err := q.QueryContext(ctx, "SELECT text FROM messages")
		return err
	})
}

func (r *Reader) Wide(g policy.ReadGrant, ctx context.Context, raw db.Querier) {
	_, _, _ = chatByRef(ctx, querier{raw: raw, scope: scope{all: true}}, "")
}

func (r *Reader) Inside(g policy.ReadGrant, ctx context.Context) error {
	return r.read(g, ctx, "x", func(ctx context.Context, q querier) error {
		q.scope.all = true
		return q.raw.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(new(int))
	})
}

func f(q querier) {
	_ = new(querier)
	_ = querier(q)
	_ = []scope{{jids: nil}}
	_ = db.Open
}
`},
		{name: "the reader's internals in the wrong function of their own file", rel: scopedReader, want: 4, src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

type Reader struct{ db *db.DB }

func New(d *db.DB) *Reader {
	_ = d.Write(context.Background(), "x", nil)
	return &Reader{db: d}
}

func (r *Reader) read(ctx context.Context, fn func(querier) error) error {
	return r.db.Read(ctx, "x", func(ctx context.Context, q db.Querier) error { return fn(querier{raw: q}) })
}

func (r *Reader) other(ctx context.Context) {
	_ = r.db.Read(ctx, "x", nil)
	_ = querier{}
}
`},
		{name: "the builder's internals in the wrong function of their own file", rel: inBuilder, want: 4, src: `package scoped

import (
	"context"
	"database/sql"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

type scope struct{ all bool }

type querier struct {
	raw   db.Querier
	scope scope
}

func (q querier) QueryContext(ctx context.Context, query string) (*sql.Rows, error) {
	return q.raw.QueryContext(ctx, query)
}

func widen(ctx context.Context, q querier, query string) (*sql.Rows, error) {
	q.scope = scope{all: true}
	return q.raw.QueryContext(ctx, query)
}

func (s scope) wide() bool { return s.all }
`},
		{name: "reads that do not run under their method's grant", rel: scopedDir + "/x.go", want: 9, src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
)

var kept policy.ReadGrant

type Query struct{}

func (r *Reader) Kept(g policy.ReadGrant, ctx context.Context) error {
	return r.read(kept, ctx, "x", nil)
}

func (r *Reader) Shadowed(g policy.ReadGrant, ctx context.Context) error {
	if g := kept; g.Valid() {
		return r.read(g, ctx, "x", nil)
	}
	g = kept
	return nil
}

func (r *Reader) Value(g policy.ReadGrant, ctx context.Context) error {
	read := r.read
	return read(g, ctx, "x", nil)
}

func (r *Reader) Two(g, h policy.ReadGrant, ctx context.Context) error { return r.read(h, ctx, "x", nil) }

func (q Query) Expression() string { _ = (&Reader{}).read(kept, nil, "x", nil); return "" }

func (r *Reader) helper(g policy.ReadGrant, ctx context.Context) error { return r.read(g, ctx, "x", nil) }

var later = func(r *Reader) error { return r.read(kept, nil, "x", nil) }
`},
		{name: "a read method that calls a sibling under a grant it decides", rel: scopedDir + "/leak.go", want: 1, src: grantWideningRead},
		{name: "calls of read methods inside the package", rel: scopedDir + "/x.go", want: 8, src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
)

var kept policy.ReadGrant

func (r *Reader) Own(g policy.ReadGrant, ctx context.Context) (Chat, bool, error) {
	return r.Chat(g, ctx, "x")
}

func (r *Reader) Kept(g policy.ReadGrant, ctx context.Context) error {
	_, err := (*Reader).Chats(r, kept, ctx, ChatPosition{}, 10)
	defer r.Search(kept, ctx, Query{}, "", SearchPosition{}, 1)
	return err
}

func (r *Reader) Local(g policy.ReadGrant, ctx context.Context) error {
	other := r
	return (other.Kept)(kept, ctx)
}

func (r *Reader) helper(ctx context.Context) {
	_, _, _ = r.Messages(kept, ctx, "", MessagePosition{}, Older, 1)
}

var through = func(r interface{ Changes(policy.ReadGrant) }) { r.Changes(kept) }

func message(r *Reader, ctx context.Context) {
	_, _, _ = r.Message(kept, ctx, policy.CanonicalChat{}, "", policy.CanonicalChat{})
}
`},
		{name: "the reader and the builder", rel: scopedReader, src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

type Reader struct {
	db    *db.DB
	trace func(string)
}

func New(d *db.DB) *Reader { return &Reader{db: d} }

func (r *Reader) read(g policy.ReadGrant, ctx context.Context, op string, fn func(context.Context, querier) error) error {
	s, err := scopeOf(g)
	if err != nil {
		return err
	}
	return r.db.Read(ctx, op, func(ctx context.Context, q db.Querier) error {
		return fn(ctx, querier{raw: q, scope: s, trace: r.trace})
	})
}
`},
		{name: "the IN builder", rel: inBuilder, src: `package scoped

import (
	"context"
	"database/sql"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

type scope struct {
	all  bool
	jids []string
}

func scopeOf(g policy.ReadGrant) scope {
	if g.All() {
		return scope{all: true}
	}
	return scope{jids: []string{}}
}

func (s scope) expand(query string) string {
	if s.all || len(s.jids) == 0 {
		return query
	}
	return query
}

type querier struct {
	raw   db.Querier
	scope scope
}

func (q querier) QueryContext(ctx context.Context, query string) (*sql.Rows, error) {
	return q.raw.QueryContext(ctx, q.scope.expand(query))
}

func (q querier) scalar(ctx context.Context, query string, dest any) error {
	return q.raw.QueryRowContext(ctx, query).Scan(dest)
}
`},
		{name: "reads under their method's grant", rel: scopedDir + "/x.go", src: `package scoped

import (
	"context"

	p "github.com/dortort/wawarden/internal/policy"
)

func (r *Reader) Chat(g p.ReadGrant, ctx context.Context, ref string) (bool, error) {
	var found bool
	err := r.read(g, ctx, "x", func(ctx context.Context, q querier) error {
		rows, err := rowsOf[Chat]{q, scanChat}.QueryContext(ctx, selectChat, ref)
		found = len(rows) > 0 && g.Allows(rows[0].Chat) && rows[0].g == nil
		return err
	})
	return found, err
}

func (r *Reader) Two(g, h p.ReadGrant, ctx context.Context) error { return r.read(g, ctx, "x", nil) }
`},
		{name: "the grant's methods and fields named like read methods", rel: scopedDir + "/x.go", src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
)

func (r *Reader) Chats(g policy.ReadGrant, ctx context.Context, pos ChatPosition, limit int) (ChatPage, error) {
	var page ChatPage
	err := r.read(g, ctx, "x", func(ctx context.Context, q querier) error {
		page.Chats = append(page.Chats, Chat{})
		if len(g.Chats()) == 0 {
			page.Chats = page.Chats[:0]
		}
		return nil
	})
	return page, err
}

func count(g policy.ReadGrant) int { return len(g.Chats()) }
`},
		{name: "a test calling read methods", rel: scopedDir + "/x_test.go", src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
)

func everything(r *Reader, g policy.ReadGrant) (ChatPage, error) {
	return r.Chats(g, context.Background(), ChatPosition{}, 10)
}
`},
		{name: "a test of the scoped package", rel: scopedDir + "/x_test.go", src: `package scoped

import (
	"context"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

func (r *Reader) Dump(ctx context.Context) error {
	return r.db.Read(ctx, "x", func(ctx context.Context, q db.Querier) error {
		_, err := querier{raw: q, scope: scope{all: true}}.QueryContext(ctx, "SELECT 1{scope m.chat_jid}")
		return err
	})
}
`},
		{name: "another store package", rel: "internal/store/ingest/x.go", src: `package ingest

import (
	"context"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

type Store struct{ db *db.DB }

func (s *Store) Count(ctx context.Context) error {
	return s.db.Read(ctx, "x", func(ctx context.Context, q db.Querier) error { return nil })
}
`},
	},
}

func checkScopedReadPath(f *sourceFile) []string {
	if f.test || !within(f.dir, scopedDir) {
		return nil
	}
	var out []string
	type span struct {
		node ast.Node
		decl string
	}
	var spans []span
	for _, decl := range f.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil {
				name = receiverName(d) + "." + name
			}
			spans = append(spans, span{d, name})
			if d.Body != nil {
				out = append(out, checkGrantPassed(f, d.Body, d)...)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				out = append(out, checkGrantPassed(f, spec, nil)...)
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					spans = append(spans, span{spec, ""})
					continue
				}
				spans = append(spans, span{ts, ts.Name.Name})
				if id, ok := ast.Unparen(ts.Type).(*ast.Ident); ok && scopedTypes[id.Name] {
					out = append(out, f.at(ts, "%s declares another name for %s, which this rule would not follow", ts.Name.Name, id.Name))
				}
			}
		}
	}
	guard := func(n ast.Node, what string) {
		site := scopedSite{file: f.rel}
		for _, s := range spans {
			if s.node.Pos() <= n.Pos() && n.End() <= s.node.End() {
				site.decl = s.decl
			}
		}
		allowed := scopedInternals[what]
		if slices.Contains(allowed, site) {
			return
		}
		if len(allowed) == 0 {
			out = append(out, f.at(n, "%s reaches the database around the scope that Reader.read takes from the caller's grant: nothing in %s may use it", what, scopedDir))
			return
		}
		where := make([]string, len(allowed))
		for i, s := range allowed {
			where[i] = s.decl + " in " + s.file
		}
		out = append(out, f.at(n, "%s outside %s reaches the database around the scope that Reader.read takes from the caller's grant and only the IN builder applies", what, strings.Join(where, " or ")))
	}
	reads := map[string]bool{}
	for _, m := range append(slices.Collect(maps.Keys(scopedReadMethods)), scopedReadMethodsOf(f)...) {
		_, name, _ := strings.Cut(m, ".")
		reads[name] = true
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if sel, p := f.ref(x); sel != nil {
				if p == dbPath {
					guard(x, "db."+x.Sel.Name)
				}
			} else if scopedFields[x.Sel.Name] {
				guard(x.Sel, "."+x.Sel.Name)
			}
		case *ast.CallExpr:
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok && reads[sel.Sel.Name] && len(x.Args) > 0 {
				out = append(out, f.at(sel.Sel, "this call of %s, a read method of %s, runs its read under a grant that is not the calling method's own, which it may not pass on: build the query inside the calling method instead", sel.Sel.Name, scopedDir))
			}
			id, ok := ast.Unparen(x.Fun).(*ast.Ident)
			switch {
			case ok && scopedTypes[id.Name]:
				guard(x, id.Name+"(…)")
			case ok && id.Name == "new" && len(x.Args) == 1:
				if t, ok := ast.Unparen(x.Args[0]).(*ast.Ident); ok && scopedTypes[t.Name] {
					guard(x, "new("+t.Name+")")
				}
			}
		}
		return true
	})
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		if id, ok := ast.Unparen(typ).(*ast.Ident); ok && scopedTypes[id.Name] {
			guard(lit, id.Name+"{}")
		}
	})
	return out
}

func checkGrantPassed(f *sourceFile, root ast.Node, fn *ast.FuncDecl) []string {
	var grant, recv string
	if fn != nil && fn.Recv != nil && len(fn.Recv.List[0].Names) > 0 {
		recv = fn.Recv.List[0].Names[0].Name
		params := fn.Type.Params.List
		if fn.Name.IsExported() && ast.IsExported(receiverName(fn)) && !grantFreeMethods[receiverName(fn)+"."+fn.Name.Name] &&
			len(params) > 0 && len(params[0].Names) > 0 && params[0].Names[0].Name != "_" && f.isType(params[0].Type, policyPath, "ReadGrant") && !isPointer(params[0].Type) {
			grant = params[0].Names[0].Name
		}
	}
	var out []string
	used := map[ast.Node]bool{}
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != scopedRead {
				return true
			}
			used[sel] = true
			if r, ok := sel.X.(*ast.Ident); ok && grant != "" && r.Name == recv && len(x.Args) > 0 {
				if g, ok := x.Args[0].(*ast.Ident); ok && g.Name == grant {
					used[g] = true
					return true
				}
			}
			out = append(out, f.at(sel.Sel, "read here does not run under the grant its caller takes first: only an exported read method may call it, on its receiver, with that grant"))
		case *ast.SelectorExpr:
			used[x.Sel] = true
			if g, ok := x.X.(*ast.Ident); ok && grant != "" && g.Name == grant {
				used[g] = true
			}
		}
		return true
	})
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == scopedRead && !used[x] {
				out = append(out, f.at(x.Sel, "read used as a value hides the grant it runs under from this rule"))
			}
		case *ast.Ident:
			if grant != "" && x.Name == grant && !used[x] {
				out = append(out, f.at(x, "the grant %s of %s is assigned, shadowed or passed on here: use it only as the first argument of read and as the receiver of its own methods", grant, fn.Name.Name))
			}
		}
		return true
	})
	return out
}

func scopedReadMethodsOf(f *sourceFile) []string {
	var out []string
	for _, decl := range f.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.IsExported() && ast.IsExported(receiverName(fn)) && !grantFreeMethods[receiverName(fn)+"."+fn.Name.Name] {
			out = append(out, receiverName(fn)+"."+fn.Name.Name)
		}
	}
	return out
}

func TestScopedReadMethodsListed(t *testing.T) {
	files, err := moduleFiles(os.DirFS(filepath.Join(moduleRoot(t), scopedDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", scopedDir, err)
	}
	declared := map[string]bool{}
	for _, f := range files {
		if !f.test && f.dir == "." {
			for _, m := range scopedReadMethodsOf(f) {
				declared[m] = true
			}
		}
	}
	if got, want := slices.Sorted(maps.Keys(declared)), slices.Sorted(maps.Keys(scopedReadMethods)); !slices.Equal(got, want) {
		t.Fatalf("package scoped declares the read methods %q, but the scoped-read-path rule refuses calls of %q: they must match", got, want)
	}
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
