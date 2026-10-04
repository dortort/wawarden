package archtest

import (
	"go/ast"
	"go/token"
)

const (
	policyPath = module + "/" + policyDir
	grantFile  = "internal/policy/decide.go"
)

var grantTypes = []string{"ReadGrant", "WriteGrant", "AdminGrant"}

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
		{name: "aliases and conversions outside policy", rel: "internal/api/x.go", want: 3, src: `package api

import "github.com/dortort/wawarden/internal/policy"

type rg = policy.ReadGrant

type ag policy.AdminGrant

func f(g policy.ReadGrant, x struct{ ok bool }) {
	_ = policy.ReadGrant(g)
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
		{name: "a chat with fields and an alias in decide.go", rel: grantFile, want: 2, src: `package policy

type rg = ReadGrant

var _ = CanonicalChat{jid: "x", ok: true}
`},
		{name: "grants in decide.go", rel: grantFile, src: `package policy

func f() {
	_ = ReadGrant{}
	_ = WriteGrant{ok: true}
	_ = AdminGrant{ok: true}
	_ = CanonicalChat{}
	var g ReadGrant
	g.ok = true
	_ = ReadGrant(g)
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
	grantType := func(e ast.Expr) (string, bool) {
		for _, name := range grantTypes {
			if f.isType(e, policyPath, name) {
				return name, true
			}
		}
		return "", false
	}
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		if name, ok := grantType(typ); ok && f.rel != grantFile {
			out = append(out, f.at(lit, "policy.%s composite literal outside %s: grants are minted only by the Decide functions", name, grantFile))
		}
		if f.isType(typ, policyPath, "CanonicalChat") && len(lit.Elts) > 0 {
			out = append(out, f.at(lit, "policy.CanonicalChat composite literal with fields: no constructor exists in M0"))
		}
	})
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.TypeSpec:
			if name, ok := grantType(n.Type); ok {
				out = append(out, f.at(n, "type %s is declared from policy.%s, so its literals or conversions would mint grants", n.Name.Name, name))
			}
		case *ast.CallExpr:
			if name, ok := grantType(n.Fun); ok && f.rel != grantFile {
				out = append(out, f.at(n, "conversion to policy.%s outside %s: grants are minted only by the Decide functions", name, grantFile))
			}
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE || f.dir != policyDir || f.rel == grantFile {
				break
			}
			for _, lhs := range n.Lhs {
				if sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr); ok && sel.Sel.Name == "ok" {
					out = append(out, f.at(lhs, "an ok flag set by assignment outside %s: build values that carry an ok flag whole, with a literal", grantFile))
				}
			}
		}
		return true
	})
	return out
}
