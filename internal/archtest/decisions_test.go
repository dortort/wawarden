package archtest

import "go/ast"

var grantDecisions = set("DecideRead", "DecideWrite", "DecideAdmin")

var grantDecisionRule = rule{
	name:  "grant-decisions",
	check: checkGrantDecisions,
	cases: []snippet{
		{name: "a read method that calls a sibling under a grant it decides", rel: scopedDir + "/leak.go", want: 1, src: grantWideningRead},
		{name: "decisions in the scoped store", rel: scopedDir + "/x.go", want: 2, src: `package scoped

import (
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func write(c *policy.Client) (policy.WriteGrant, bool) { return policy.DecideWrite(c, time.Now()) }

func admin(cred policy.AdminCredential, presented string) bool {
	_, ok := policy.DecideAdmin(cred, presented)
	return ok
}
`},
		{name: "a decision through an aliased import", rel: appDir + "/x.go", want: 1, src: `package app

import (
	"time"

	p "github.com/dortort/wawarden/internal/policy"
)

func grant(c *p.Client) p.ReadGrant {
	g, _ := p.DecideRead(c, time.Now())
	return g
}
`},
		{name: "decisions as values", rel: appDir + "/x.go", want: 3, src: `package app

import "github.com/dortort/wawarden/internal/policy"

var decide = policy.DecideRead

var deciders = []any{(policy.DecideWrite), func() any { return policy.DecideAdmin }}
`},
		{name: "a decision in a subpackage of the API", rel: apiDir + "/dto/x.go", want: 1, src: `package dto

import "github.com/dortort/wawarden/internal/policy"

var _ = policy.DecideRead
`},
		{name: "decisions in the API", rel: apiDir + "/x.go", src: `package api

import (
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

var decide = policy.DecideRead

func decided(c *policy.Client, cred policy.AdminCredential, presented string) bool {
	_, r := decide(c, time.Now())
	_, w := policy.DecideWrite(c, time.Now())
	_, a := policy.DecideAdmin(cred, presented)
	return r && w && a
}
`},
		{name: "package policy's own files", rel: policyDir + "/x.go", src: `package policy

import "time"

func decided(c *Client, cred AdminCredential, presented string) bool {
	_, r := DecideRead(c, time.Now())
	_, w := DecideWrite(c, time.Now())
	_, a := DecideAdmin(cred, presented)
	return r && w && a
}
`},
		{name: "a test outside the API", rel: scopedDir + "/x_test.go", src: `package scoped

import (
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func grant(c *policy.Client) policy.ReadGrant {
	g, _ := policy.DecideRead(c, time.Now())
	return g
}
`},
		{name: "another package named policy and other policy functions", rel: appDir + "/x.go", src: `package app

import (
	"github.com/dortort/wawarden/internal/policy"
	other "example.com/policy"
)

var decide = other.DecideRead

var normalize = policy.Normalize
`},
	},
}

func checkGrantDecisions(f *sourceFile) []string {
	if f.test || f.dir == apiDir {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if s, p := f.ref(sel); s != nil && p == policyPath && grantDecisions[sel.Sel.Name] {
			out = append(out, f.at(sel, "policy.%s outside %s decides a grant: only the API decides grants, for the request it serves", sel.Sel.Name, apiDir))
		}
		return true
	})
	return out
}
