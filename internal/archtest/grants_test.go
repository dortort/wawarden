package archtest

import "go/ast"

const (
	policyPath = module + "/" + policyDir
	grantFile  = "internal/policy/decide.go"
)

var grantTypes = []string{"ReadGrant", "WriteGrant", "AdminGrant"}

var grantRule = rule{
	name:  "grant-forging",
	check: checkGrantLiterals,
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
		{name: "grants and chats elsewhere in policy", rel: "internal/policy/grant.go", want: 3, src: `package policy

var (
	_ = ReadGrant{ok: true}
	_ = []CanonicalChat{{jid: "x", ok: true}}
	_ = AdminGrant{}
	_ = CanonicalChat{}
)
`},
		{name: "a chat with fields in decide.go", rel: grantFile, want: 1, src: `package policy

var _ = CanonicalChat{jid: "x", ok: true}
`},
		{name: "grants in decide.go", rel: grantFile, src: `package policy

func f() {
	_ = ReadGrant{}
	_ = WriteGrant{ok: true}
	_ = AdminGrant{ok: true}
	_ = CanonicalChat{}
}
`},
		{name: "grants in a test", rel: "internal/api/x_test.go", src: `package api

import "github.com/dortort/wawarden/internal/policy"

var _ = policy.ReadGrant{}
`},
		{name: "another package's grant types", rel: "internal/api/x.go", src: `package api

import "example.com/policy"

var _ = policy.ReadGrant{ok: true}

type ReadGrant struct{ ok bool }

var _ = ReadGrant{ok: true}
`},
	},
}

func checkGrantLiterals(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		for _, name := range grantTypes {
			if f.isType(typ, policyPath, name) && f.rel != grantFile {
				out = append(out, f.at(lit, "policy.%s composite literal outside %s: grants are minted only by the Decide functions", name, grantFile))
			}
		}
		if f.isType(typ, policyPath, "CanonicalChat") && len(lit.Elts) > 0 {
			out = append(out, f.at(lit, "policy.CanonicalChat composite literal with fields: no constructor exists in M0"))
		}
	})
	return out
}
