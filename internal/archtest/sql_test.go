package archtest

import (
	"go/ast"
	"regexp"
)

const inBuilder = "internal/store/scoped/inbuilder.go"

var inList = regexp.MustCompile(`(?i)\bIN\s*\(`)

var inListRule = rule{
	name:  "sql-in-lists",
	check: checkInLists,
	cases: []snippet{
		{name: "literals", rel: "internal/store/x.go", want: 3, src: "package store\n\n" +
			"const a = \"SELECT id FROM t WHERE id IN (?)\"\n\n" +
			"var b = `select id from t where id in(?)`\n\n" +
			"func f() { _ = []string{\"x\", \"chat IN\\t(\"} }\n"},
		{name: "concatenations", rel: "internal/store/x.go", want: 4, src: `package store

func f(x string) {
	_ = "WHERE id IN" + " (" + "?)"
	_ = x + "id in" + (" " + "(?)")
	_ = "id I" + x + "N (" + "?)" + "AND chat in" + "("
	_ = ("WHERE id" + " ") + "IN" + ("(" + x)
}
`},
		{name: "IN lists assembled from constants of the file", rel: "internal/store/x.go", want: 2, src: `package store

import "context"

type querier interface {
	QueryContext(context.Context, string, ...any) (any, error)
}

const (
	op   = " IN"
	open = " ("
	list = "WHERE id" + op + open + "?)"
)

func f(ctx context.Context, q querier) {
	_, _ = q.QueryContext(ctx, "SELECT id FROM t WHERE id"+op+open+"?)")
	_, _ = q.QueryContext(ctx, "SELECT id FROM t "+list)
}
`},
		{name: "the IN builder", rel: inBuilder, src: `package scoped

const q = "WHERE chat_jid IN ("
`},
		{name: "a test", rel: "internal/store/x_test.go", src: `package store

const q = "WHERE chat_jid IN ("
`},
		{name: "near misses", rel: "internal/store/x.go", src: `package store

func f(x string) {
	_ = "JOIN (SELECT 1)"
	_ = "WITHIN (" + "LOGIN("
	_ = "IN"
	_ = "IN" + x + "("
	_ = 'I'
}
`},
	},
}

func checkInLists(f *sourceFile) []string {
	if f.test || f.rel == inBuilder {
		return nil
	}
	var out []string
	literalRuns(f.file, func(at ast.Node, s string) {
		if inList.MatchString(s) {
			out = append(out, f.at(at, "SQL IN list outside %s: build IN lists only there", inBuilder))
		}
	})
	assembledSQL(f.file, inList, func(at ast.Expr, text, _ string) {
		out = append(out, f.at(at, "%q, assembled from constants of this file, holds an SQL IN list outside %s: build IN lists only there", text, inBuilder))
	})
	return out
}
