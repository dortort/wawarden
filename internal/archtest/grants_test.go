package archtest

import (
	"go/ast"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	policyPath = module + "/" + policyDir
	grantFile  = "internal/policy/decide.go"
	chatFile   = "internal/policy/normalize.go"
	chatType   = "CanonicalChat"
)

var (
	grantTypes  = []string{"ReadGrant", "WriteGrant", "AdminGrant"}
	sealedTypes = append([]string{chatType}, grantTypes...)
)

var grantSetFields = set("chats")

var grantRule = rule{
	name:  "grant-forging",
	check: checkGrants,
	cases: []snippet{
		{name: "grants and chats outside policy", rel: "internal/api/x.go", want: 7, src: `package api

import p "github.com/dortort/wawarden/internal/policy"

var (
	_ = p.ReadGrant{}
	_ = p.WriteGrant{}
	_ = &p.AdminGrant{}
	_ = []p.ReadGrant{{}}
	_ = map[string]*p.WriteGrant{"a": {}}
	_ = p.CanonicalChat{jid: "x"}
	_ = map[p.CanonicalChat]bool{{ok: true}: true}
	_ = p.CanonicalChat{}
)
`},
		{name: "aliases and conversions outside policy", rel: "internal/api/x.go", want: 5, src: `package api

import "github.com/dortort/wawarden/internal/policy"

type rg = policy.ReadGrant

type ag policy.AdminGrant

type cc = policy.CanonicalChat

func f(g policy.ReadGrant, c policy.CanonicalChat, x struct{ ok bool }) {
	_ = policy.ReadGrant(g)
	_ = policy.CanonicalChat(c)
	x.ok = true
}
`},
		{name: "grants and chats elsewhere in policy", rel: "internal/policy/grant.go", want: 3, src: `package policy

var (
	_ = ReadGrant{ok: true}
	_ = []CanonicalChat{{jid: "x", ok: true}}
	_ = AdminGrant{}
	_ = CanonicalChat{}
)
`},
		{name: "aliases, conversions and ok assignments elsewhere in policy", rel: "internal/policy/grant.go", want: 9, src: `package policy

import "time"

type rg = ReadGrant

type wg WriteGrant

type ag = *AdminGrant

func f(c *Client, gs []ReadGrant, x struct{ ok bool }) {
	g, _ := DecideRead(c, time.Time{})
	g.ok = true
	gs[0].ok = !gs[0].ok
	(&g).ok = true
	x.ok = true
	_ = rg{ok: true}
	_ = ReadGrant(x)
	_ = (*AdminGrant)(nil)
}
`},
		{name: "generic forms and ok pointers elsewhere in policy", rel: "internal/policy/grant.go", want: 7, src: `package policy

type only interface{ ReadGrant }

type either interface {
	~WriteGrant | int
}

type shadow struct {
	client string
	all    bool
	chats  map[CanonicalChat]struct{}
	ok     bool
}

type box[K comparable, T *AdminGrant] struct{ v map[K]T }

func mint[T only]() T { return T{ok: true, all: true} }

func conv[T ReadGrant](s shadow) T { return T(s) }

func nested[T interface{ ~[]ReadGrant }]() T { return T{{ok: true}} }

func ptr(g *ReadGrant, flags []bool) {
	p := &g.ok
	*p = true
	_ = &(g.ok)
	_ = &flags[0]
}
`},
		{name: "chat aliases, conversions, generics and ok assignments elsewhere in policy", rel: "internal/policy/chat.go", want: 6, src: `package policy

type cc = CanonicalChat

type fake struct {
	jid string
	ok  bool
}

var (
	_ = cc{ok: true}
	_ = CanonicalChat(fake{"x", true})
	_ = (*CanonicalChat)(&CanonicalChat{})
)

func chat[T interface{ ~struct{ jid string; ok bool } | CanonicalChat }]() T { return T{ok: true} }

func set(c *CanonicalChat, flags []bool) {
	for _, c.ok = range flags {
	}
	c.ok, _ = true, 0
}
`},
		{name: "field changes elsewhere in policy", rel: "internal/policy/grant.go", want: 10, src: `package policy

import m "maps"

func (g *ReadGrant) widen(c CanonicalChat, more map[CanonicalChat]struct{}, counter *struct{ n int }) {
	g.all = true
	g.chats[c] = struct{}{}
	(g.client) = "x"
	p := &g.all
	*p = true
	m.Copy(g.chats, more)
	delete(g.chats, c)
	clear(g.chats)
	for g.client = range map[string]bool{} {
	}
	counter.n++
}

func (g WriteGrant) add(c CanonicalChat) { g.chats[c] = struct{}{} }
`},
		{name: "grant sets aliased elsewhere in policy", rel: "internal/policy/grant.go", want: 9, src: `package policy

import "maps"

func (g ReadGrant) widen(c CanonicalChat, more map[CanonicalChat]struct{}) {
	chats := g.chats
	chats[c] = struct{}{}
	maps.Copy(chats, more)
}

func (g *WriteGrant) widen(c *Client, out chan<- map[CanonicalChat]struct{}) map[CanonicalChat]struct{} {
	set := g.chats
	for chat := range c.Read {
		set[chat] = struct{}{}
	}
	var alias = (g.chats)
	keep(g.chats, alias)
	out <- g.chats
	_ = []map[CanonicalChat]struct{}{g.chats}
	_ = func() map[CanonicalChat]struct{} { return g.chats }
	_ = maps.Keys(g.chats)
	return g.chats
}

func keep(...map[CanonicalChat]struct{}) {}
`},
		{name: "grant sets read elsewhere in policy", rel: "internal/policy/grant.go", src: `package policy

import "maps"

func (g ReadGrant) read(c CanonicalChat, more map[CanonicalChat]struct{}) (int, map[CanonicalChat]struct{}, bool) {
	_, ok := (g.chats)[c]
	for chat := range g.chats {
		ok = ok || chat == c
	}
	out := maps.Clone(g.chats)
	maps.Copy(out, more)
	return len(g.chats), out, ok && g.Allows(c)
}
`},
		{name: "field changes in decide.go and local changes elsewhere in policy", rel: grantFile, src: `package policy

import "maps"

func f(c *Client, more map[CanonicalChat]struct{}) ReadGrant {
	g := ReadGrant{client: c.ID}
	g.chats = maps.Clone(c.Read)
	g.all = c.ReadAll
	maps.Copy(g.chats, more)
	delete(g.chats, CanonicalChat{})
	alias := g.chats
	maps.Copy(alias, more)
	return g
}
`},
		{name: "local maps and package variables elsewhere in policy", rel: "internal/policy/admin.go", src: `package policy

import (
	"maps"
	"time"
)

func f(set map[CanonicalChat]struct{}, more map[CanonicalChat]struct{}) {
	out := make(map[CanonicalChat]struct{}, len(set))
	for chat := range set {
		out[chat] = struct{}{}
	}
	maps.Copy(out, more)
	delete(out, CanonicalChat{})
	time.Local = time.UTC
	n := 0
	n++
	_ = &out
}
`},
		{name: "generic forms outside policy", rel: "internal/api/x.go", want: 3, src: `package api

import "github.com/dortort/wawarden/internal/policy"

type only interface {
	policy.ReadGrant | policy.WriteGrant
}

func mint[T only, U policy.AdminGrant]() {}

func slice[S ~[]E, E interface{ *policy.WriteGrant }]() {}
`},
		{name: "interfaces and type parameters that name no grant", rel: "internal/api/x.go", src: `package api

import "github.com/dortort/wawarden/internal/policy"

type granted interface {
	Valid() bool
	Grant() policy.ReadGrant
}

type holder struct{ g policy.ReadGrant }

type handler[G any] func(G) error

func decide[G interface{ Valid() bool }](g G) bool { return g.Valid() }

func f(h func(policy.ReadGrant), g *policy.ReadGrant) {
	_ = &g
	_ = &holder{}
}
`},
		{name: "chats, aliases, generics and ok flags in decide.go", rel: grantFile, want: 9, src: `package policy

type rg = ReadGrant

type cc = CanonicalChat

var _ = CanonicalChat{jid: "x", ok: true}

func f(c CanonicalChat, g ReadGrant, x struct{ jid string; ok bool }, flags []bool) {
	c.ok = true
	g.ok = true
	_ = &c.ok
	_ = CanonicalChat(x)
	for _, g.ok = range flags {
	}
}

func mint[T ReadGrant | WriteGrant]() (t T) { return t }
`},
		{name: "grants in decide.go", rel: grantFile, src: `package policy

func f(chats map[CanonicalChat]struct{}, flags []bool) {
	_ = ReadGrant{}
	_ = WriteGrant{ok: true}
	_ = AdminGrant{ok: true}
	_ = CanonicalChat{}
	var g ReadGrant
	_ = ReadGrant(g)
	_ = (*WriteGrant)(nil)
	for chat := range chats {
		_ = chat.ok && g.ok
	}
	for _, ok := range flags {
		_ = ok
	}
}
`},
		{name: "chats built by literals in normalize.go", rel: chatFile, src: `package policy

func Normalize(s string) (CanonicalChat, bool) {
	if s == "" {
		return CanonicalChat{}, false
	}
	return CanonicalChat{jid: s, ok: true}, true
}

var _ = []CanonicalChat{{jid: "x", ok: true}}
`},
		{name: "aliases, conversions, field changes, generics and grants in normalize.go", rel: chatFile, want: 7, src: `package policy

type cc = CanonicalChat

type fake struct {
	jid string
	ok  bool
}

func Normalize(s string) (CanonicalChat, bool) {
	var c CanonicalChat
	c.jid = s
	c.ok = true
	_ = &c.ok
	_ = CanonicalChat(fake{s, true})
	_ = ReadGrant{ok: true}
	return c, true
}

func chat[T interface{ CanonicalChat }]() T { return T{ok: true} }
`},
		{name: "chat literals in a file named like normalize.go", rel: "internal/policy/normalizer.go", want: 2, src: `package policy

var (
	_ = CanonicalChat{jid: "x", ok: true}
	_ = map[CanonicalChat]bool{{ok: true}: true}
)
`},
		{name: "chat literals in another package's normalize.go", rel: "internal/api/normalize.go", want: 1, src: `package api

import "github.com/dortort/wawarden/internal/policy"

var _ = policy.CanonicalChat{ok: true}
`},
		{name: "grants in a test", rel: "internal/api/x_test.go", src: `package api

import "github.com/dortort/wawarden/internal/policy"

type rg = policy.ReadGrant

var _ = policy.ReadGrant{}
`},
		{name: "values with an ok flag built by literals in policy", rel: "internal/policy/admin.go", src: `package policy

func f(sum [32]byte) (AdminCredential, bool) {
	ok := true
	c := AdminCredential{sum: sum, ok: ok}
	return c, c.ok
}
`},
		{name: "another package's grant types", rel: "internal/api/x.go", src: `package api

import "example.com/policy"

var _ = policy.ReadGrant{ok: true}

type ReadGrant struct{ ok bool }

type alias = policy.ReadGrant

var _ = ReadGrant{ok: true}
`},
	},
}

func checkGrants(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		if name, ok := f.policyType(typ, grantTypes); ok && f.rel != grantFile {
			out = append(out, f.at(lit, "policy.%s composite literal outside %s: grants are minted only by the Decide functions", name, grantFile))
		}
		if f.isType(typ, policyPath, chatType) && len(lit.Elts) > 0 && f.rel != chatFile {
			out = append(out, f.at(lit, "policy.CanonicalChat composite literal with fields outside %s: a valid chat is built only by Normalize, so elsewhere only the zero value may be built", chatFile))
		}
	})
	constraints := func(params *ast.FieldList) {
		if params == nil {
			return
		}
		for _, p := range params.List {
			if name, ok := f.mentionedType(p.Type, sealedTypes); ok {
				out = append(out, f.at(p, "a type parameter constrained by policy.%s can build or convert to one", name))
			}
		}
	}
	settled := map[ast.Expr]bool{}
	fieldWrites := func(exprs ...ast.Expr) {
		for _, e := range exprs {
			settled[ast.Unparen(e)] = true
			if msg := f.fieldWrite(e); msg != "" {
				out = append(out, f.at(e, "%s", msg))
			}
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IndexExpr:
			settled[ast.Unparen(n.X)] = true
		case *ast.SelectorExpr:
			if f.setAlias(n) && !settled[n] {
				out = append(out, f.at(n, "a grant's %s set used outside %s other than by indexing it, ranging over it, or passing it to len or maps.Clone: an alias of it could widen an existing grant", n.Sel.Name, grantFile))
			}
		case *ast.TypeSpec:
			constraints(n.TypeParams)
			if name, ok := f.policyType(n.Type, sealedTypes); ok {
				out = append(out, f.at(n, "type %s is declared from policy.%s, so its literals or conversions would forge one", n.Name.Name, name))
			}
		case *ast.FuncType:
			constraints(n.TypeParams)
		case *ast.InterfaceType:
			for _, elem := range n.Methods.List {
				if len(elem.Names) > 0 {
					continue
				}
				if name, ok := f.mentionedType(elem.Type, sealedTypes); ok {
					out = append(out, f.at(elem, "an interface element names policy.%s, so a type parameter it constrains can build or convert to one", name))
				}
			}
		case *ast.CallExpr:
			if f.mutatesMap(n) {
				fieldWrites(n.Args[0])
			}
			if f.readsMap(n) {
				for _, arg := range n.Args {
					settled[ast.Unparen(arg)] = true
				}
			}
			name, ok := f.policyType(n.Fun, sealedTypes)
			switch {
			case !ok:
			case name == chatType:
				out = append(out, f.at(n, "conversion to policy.CanonicalChat: a valid chat is built only by Normalize, with a literal, so none may be converted into one"))
			case f.rel != grantFile:
				out = append(out, f.at(n, "conversion to policy.%s outside %s: grants are minted only by the Decide functions", name, grantFile))
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				fieldWrites(n.X)
			}
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				fieldWrites(n.Lhs...)
			}
		case *ast.IncDecStmt:
			fieldWrites(n.X)
		case *ast.RangeStmt:
			settled[ast.Unparen(n.X)] = true
			if n.Tok == token.ASSIGN {
				fieldWrites(n.Key, n.Value)
			}
		}
		return true
	})
	return out
}

var mapMutators = map[string]map[string]bool{"maps": set("Copy", "DeleteFunc", "Insert")}

func (f *sourceFile) fieldWrite(e ast.Expr) string {
	if f.dir != policyDir {
		return ""
	}
	if sel, ok := ast.Unparen(e).(*ast.SelectorExpr); ok && sel.Sel.Name == "ok" {
		return "an ok flag set by assignment or through its address: build values that carry an ok flag whole, with a literal"
	}
	if index, ok := ast.Unparen(e).(*ast.IndexExpr); ok {
		e = index.X
	}
	if sel, ok := ast.Unparen(e).(*ast.SelectorExpr); ok && f.rel != grantFile {
		if s, _ := f.ref(sel); s == nil {
			return "a field changed outside " + grantFile + ": package policy builds values whole, and only the Decide functions set a grant's fields"
		}
	}
	return ""
}

func (f *sourceFile) setAlias(sel *ast.SelectorExpr) bool {
	if f.dir != policyDir || f.rel == grantFile || !grantSetFields[sel.Sel.Name] {
		return false
	}
	s, _ := f.ref(sel)
	return s == nil
}

func (f *sourceFile) readsMap(call *ast.CallExpr) bool {
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		return id.Name == "len"
	}
	sel, p := f.ref(call.Fun)
	return sel != nil && p == "maps" && sel.Sel.Name == "Clone"
}

func (f *sourceFile) mutatesMap(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		return id.Name == "delete" || id.Name == "clear"
	}
	sel, p := f.ref(call.Fun)
	return sel != nil && mapMutators[p][sel.Sel.Name]
}

func (f *sourceFile) policyType(e ast.Expr, names []string) (string, bool) {
	for _, name := range names {
		if f.isType(e, policyPath, name) {
			return name, true
		}
	}
	return "", false
}

func (f *sourceFile) mentionedType(e ast.Expr, names []string) (string, bool) {
	var found string
	ast.Inspect(e, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch n := n.(type) {
		case *ast.Field:
			found, _ = f.mentionedType(n.Type, names)
			return false
		case *ast.InterfaceType:
			return false
		case ast.Expr:
			found, _ = f.policyType(n, names)
			_, selector := n.(*ast.SelectorExpr)
			return found == "" && !selector
		}
		return true
	})
	return found, found != ""
}

var valueTypes = set("bool", "string", "byte", "rune", "int", "int8", "int16", "int32", "int64",
	"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128")

func (f *sourceFile) valueType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return valueTypes[t.Name]
	case *ast.ArrayType:
		return t.Len != nil && f.valueType(t.Elt)
	case *ast.IndexExpr:
		sel, p := f.ref(t.X)
		return sel != nil && p == "unique" && sel.Sel.Name == "Handle"
	}
	return false
}

func TestChatFileDeclaresNormalize(t *testing.T) {
	files, err := moduleFiles(os.DirFS(filepath.Join(moduleRoot(t), policyDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", policyDir, err)
	}
	var found []string
	for _, f := range files {
		if f.test || f.dir != "." {
			continue
		}
		for _, decl := range f.file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "Normalize" {
				found = append(found, policyDir+"/"+f.rel)
			}
		}
	}
	if !slices.Equal(found, []string{chatFile}) {
		t.Fatalf("package policy declares Normalize in %q, but the grant-forging rule lets only %s build a valid chat: they must match", found, chatFile)
	}
}

func TestGrantSetFields(t *testing.T) {
	files, err := moduleFiles(os.DirFS(filepath.Join(moduleRoot(t), policyDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", policyDir, err)
	}
	declared, fields := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		if f.test || f.dir != "." {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !slices.Contains(sealedTypes, ts.Name.Name) {
				return true
			}
			declared[ts.Name.Name] = true
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("policy.%s is no longer a struct, so the grant-forging rule cannot read its fields: update it", ts.Name.Name)
			}
			for _, field := range st.Fields.List {
				if len(field.Names) == 0 {
					t.Fatalf("policy.%s embeds a field: name it, so the grant-forging rule can confine it", ts.Name.Name)
				}
				for _, name := range field.Names {
					if !f.valueType(field.Type) {
						fields[name.Name] = true
					}
				}
			}
			return false
		})
	}
	if len(declared) != len(sealedTypes) {
		t.Fatalf("package policy declares %q of %q, so the grant-forging rule guards a type that is gone: update it",
			slices.Sorted(maps.Keys(declared)), sealedTypes)
	}
	if !maps.Equal(fields, grantSetFields) {
		t.Fatalf("the grant types hold %q by reference, but the grant-forging rule confines aliases of %q: they must match",
			slices.Sorted(maps.Keys(fields)), slices.Sorted(maps.Keys(grantSetFields)))
	}
}
