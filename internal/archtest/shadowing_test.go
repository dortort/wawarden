package archtest

import (
	"go/ast"
	"go/token"
	"go/types"
)

var predeclared = set(types.Universe.Names()...)

var shadowRule = rule{
	name:  "shadowing",
	check: checkShadowing,
	cases: []snippet{
		{name: "short variable declarations", rel: "internal/policy/decide.go", want: 4, src: `package policy

import (
	"crypto/subtle"
	"maps"
)

func f(a, b []byte, ch chan int) bool {
	subtle := struct{ ConstantTimeCompare func(x, y []byte) int }{}
	len, ok := 0, true
	switch maps := any(a).(type) {
	default:
		_ = maps
	}
	select {
	case cap := <-ch:
		_ = cap
	}
	return subtle.ConstantTimeCompare != nil && ok && len == 0
}
`},
		{name: "short variable declarations with other names", rel: "internal/policy/decide.go", src: `package policy

import "crypto/subtle"

func f(a, b []byte) bool {
	n, ok := len(a), true
	switch v := any(a).(type) {
	default:
		_ = v
	}
	return ok && n == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}
`},
		{name: "var declarations", rel: "internal/token/token.go", want: 3, src: `package token

import "hash/crc32"

var new = 1

func f() {
	var copy, x = 1, 2
	var (
		crc32 int
	)
	_, _, _ = copy, x, crc32
}
`},
		{name: "var declarations with other names", rel: "internal/token/token.go", src: `package token

import "hash/crc32"

var table = crc32.IEEETable

func f() {
	var sum, n = 0, 1
	_, _ = sum, n
}
`},
		{name: "const declarations", rel: "internal/token/token.go", want: 3, src: `package token

import "encoding/hex"

const true = false

const (
	hex = 1
	max = 2
)
`},
		{name: "const declarations with other names", rel: "internal/token/token.go", src: `package token

import "encoding/hex"

const prefix = "wwadm_"

var digits = hex.EncodedLen(4)
`},
		{name: "type declarations", rel: "internal/policy/grant.go", want: 3, src: `package policy

import "crypto/subtle"

type error struct{}

type subtle int

func f() {
	type any = int
}
`},
		{name: "type declarations with other names", rel: "internal/policy/grant.go", src: `package policy

import "crypto/subtle"

type digest [32]byte

var compare = subtle.ConstantTimeCompare
`},
		{name: "function declarations", rel: "internal/policy/grant.go", want: 2, src: `package policy

import "maps"

func len(m map[CanonicalChat]struct{}) int { return 0 }

func maps() {}
`},
		{name: "function and method declarations with other names", rel: "internal/policy/grant.go", src: `package policy

import (
	"crypto/subtle"
	"maps"
)

func clone(m map[CanonicalChat]struct{}) map[CanonicalChat]struct{} { return maps.Clone(m) }

func (g ReadGrant) len() int { return len(g.chats) }

func (c AdminCredential) subtle() bool { return subtle.ConstantTimeByteEq(1, 1) == 1 }
`},
		{name: "parameters", rel: "internal/policy/admin.go", want: 4, src: `package policy

import (
	"crypto/subtle"
	"maps"
)

type comparer interface{ Compare(subtle []byte) bool }

func f(subtle comparer, len func(map[CanonicalChat]struct{}) int) {
	_ = func(maps int) {}
}
`},
		{name: "parameters with other names", rel: "internal/policy/admin.go", src: `package policy

import "crypto/subtle"

type comparer interface{ Compare(sum []byte) bool }

func f(sum, presented []byte) bool { return subtle.ConstantTimeCompare(sum, presented) == 1 }
`},
		{name: "results", rel: "internal/token/token.go", want: 3, src: `package token

import "encoding/hex"

func f() (len int, hex string) {
	_ = func() (copy int) { return 0 }
	return 0, ""
}
`},
		{name: "results with other names", rel: "internal/token/token.go", src: `package token

import "encoding/hex"

func f(b []byte) (n int, text string) { return len(b), hex.EncodeToString(b) }
`},
		{name: "receivers", rel: "internal/policy/decide.go", want: 2, src: `package policy

import "time"

func (len ReadGrant) Allows(c CanonicalChat) bool { return false }

func (time *Client) expired() bool { return false }
`},
		{name: "receivers with other names", rel: "internal/policy/decide.go", src: `package policy

import "time"

func (g ReadGrant) Allows(c CanonicalChat) bool { return false }

func (c *Client) expired(now time.Time) bool { return !now.Before(c.ExpiresAt) }
`},
		{name: "range variables", rel: "internal/policy/grant.go", want: 3, src: `package policy

import "maps"

func f(m map[CanonicalChat]struct{}, xs []int) {
	for maps, len := range m {
		_, _ = maps, len
	}
	for _, cap := range xs {
		_ = cap
	}
}
`},
		{name: "range variables with other names, and assignments", rel: "internal/policy/grant.go", src: `package policy

import "maps"

func f(m map[CanonicalChat]struct{}, xs []int) map[CanonicalChat]struct{} {
	for chat, v := range m {
		_, _ = chat, v
	}
	var i int
	for i = range xs {
	}
	_ = i
	return maps.Clone(m)
}
`},
		{name: "type parameters", rel: "internal/token/token.go", want: 2, src: `package token

import "maps"

func f[maps any]() {}

type box[len any] struct{}
`},
		{name: "type parameters with other names", rel: "internal/token/token.go", src: `package token

func f[K comparable, V any]() {}

type box[T any] struct{ v T }
`},
		{name: "a policy subpackage", rel: "internal/policy/sub/x.go", want: 1, src: `package sub

func f() { delete := 0; _ = delete }
`},
		{name: "outside policy and token, and in their tests", rel: "internal/api/x.go", src: `package api

import "maps"

func f(maps int) { len := maps; _ = len }
`},
		{name: "a token test", rel: "internal/token/x_test.go", src: `package token

import "hash/crc32"

func f(crc32 int) { copy := crc32; _ = copy }
`},
		{name: "a package whose path starts with token", rel: "internal/tokenizer/x.go", src: `package tokenizer

func f() { len := 0; _ = len }
`},
	},
}

func checkShadowing(f *sourceFile) []string {
	if f.test || !within(f.dir, policyDir) && !within(f.dir, tokenDir) {
		return nil
	}
	var out []string
	declare := func(id *ast.Ident, form string) {
		if p, ok := f.names[id.Name]; ok {
			out = append(out, f.at(id, "%s %s shadows this file's import of %q, so the rules that recognise its calls by name would accept a local look-alike", form, id.Name, p))
		} else if predeclared[id.Name] {
			out = append(out, f.at(id, "%s %s shadows the predeclared identifier, so the rules that recognise it by name would accept a local look-alike", form, id.Name))
		}
	}
	fields := func(list *ast.FieldList, form string) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			for _, id := range field.Names {
				declare(id, form)
			}
		}
	}
	idents := func(form string, exprs ...ast.Expr) {
		for _, e := range exprs {
			if id, ok := e.(*ast.Ident); ok {
				declare(id, form)
			}
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				idents("variable", n.Lhs...)
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				idents("range variable", n.Key, n.Value)
			}
		case *ast.GenDecl:
			for _, spec := range n.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					form := "variable"
					if n.Tok == token.CONST {
						form = "constant"
					}
					for _, id := range spec.Names {
						declare(id, form)
					}
				case *ast.TypeSpec:
					declare(spec.Name, "type")
					fields(spec.TypeParams, "type parameter")
				}
			}
		case *ast.FuncDecl:
			if n.Recv == nil {
				declare(n.Name, "function")
			}
			fields(n.Recv, "receiver")
		case *ast.FuncType:
			fields(n.TypeParams, "type parameter")
			fields(n.Params, "parameter")
			fields(n.Results, "result")
		}
		return true
	})
	return out
}
