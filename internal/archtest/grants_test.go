package archtest

import (
	"go/ast"
	"go/token"
)

const (
	policyPath = module + "/" + policyDir
	grantFile  = "internal/policy/decide.go"
	chatType   = "CanonicalChat"
)

var (
	grantTypes  = []string{"ReadGrant", "WriteGrant", "AdminGrant"}
	sealedTypes = append([]string{chatType}, grantTypes...)
)

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
		{name: "field changes in decide.go and local changes elsewhere in policy", rel: grantFile, src: `package policy

import "maps"

func f(c *Client, more map[CanonicalChat]struct{}) ReadGrant {
	g := ReadGrant{client: c.ID}
	g.chats = maps.Clone(c.Read)
	g.all = c.ReadAll
	maps.Copy(g.chats, more)
	delete(g.chats, CanonicalChat{})
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
		if f.isType(typ, policyPath, chatType) && len(lit.Elts) > 0 {
			out = append(out, f.at(lit, "policy.CanonicalChat composite literal with fields: a valid chat has no constructor, so only the zero value may be built"))
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
	fieldWrites := func(exprs ...ast.Expr) {
		for _, e := range exprs {
			if msg := f.fieldWrite(e); msg != "" {
				out = append(out, f.at(e, "%s", msg))
			}
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
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
			name, ok := f.policyType(n.Fun, sealedTypes)
			switch {
			case !ok:
			case name == chatType:
				out = append(out, f.at(n, "conversion to policy.CanonicalChat: a valid chat has no constructor, so none may be converted into one"))
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
