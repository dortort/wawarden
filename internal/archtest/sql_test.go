package archtest

import (
	"go/ast"
	"regexp"
	"strings"
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
	report := func(at ast.Node, s string) {
		if inList.MatchString(s) {
			out = append(out, f.at(at, "SQL IN list outside %s: build IN lists only there", inBuilder))
		}
	}
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			operands := addOperands(e)
			if len(operands) < 2 {
				return true
			}
			var run strings.Builder
			var start ast.Node
			flush := func() {
				if start != nil {
					report(start, run.String())
				}
				run.Reset()
				start = nil
			}
			for _, operand := range operands {
				if s, ok := stringLit(operand); ok {
					if start == nil {
						start = operand
					}
					run.WriteString(s)
					continue
				}
				flush()
				ast.Inspect(operand, visit)
			}
			flush()
			return false
		case *ast.BasicLit:
			if s, ok := stringLit(e); ok {
				report(e, s)
			}
		}
		return true
	}
	ast.Inspect(f.file, visit)
	return out
}
