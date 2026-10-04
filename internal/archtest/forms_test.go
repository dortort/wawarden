package archtest

import "go/ast"

var formParsers = set("FormFile", "FormValue", "MultipartReader", "ParseForm", "ParseMultipartForm", "PostFormValue")

var formRule = rule{
	name:  "form-parsing",
	check: checkFormParsing,
	cases: []snippet{
		{name: "form parsing in api", rel: "internal/api/x.go", want: 9, src: `package api

import web "net/http"

type wrapped struct{ *web.Request }

func f(r *web.Request, w wrapped) {
	_ = r.ParseForm()
	_ = r.ParseMultipartForm(1 << 20)
	_ = r.FormValue("a")
	_ = r.PostFormValue("a")
	_, _, _ = r.FormFile("a")
	_, _ = r.MultipartReader()
	value := r.FormValue
	_ = value
	_ = (*web.Request).ParseForm
	_ = w.ParseForm()
}
`},
		{name: "form parsing through an interface in an api subpackage", rel: "internal/api/dto/x.go", want: 1, src: `package dto

type form interface{ FormValue(string) string }

func f(r form) string { return r.FormValue("a") }
`},
		{name: "JSON bodies in api", rel: "internal/api/x.go", src: `package api

import (
	"encoding/json"
	"mime"
	"net/http"
)

func f(r *http.Request, v any) error {
	if _, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil {
		return err
	}
	return json.NewDecoder(r.Body).Decode(v)
}
`},
		{name: "form parsing in a helper outside api", rel: "internal/httpx/x.go", want: 2, src: `package httpx

import "net/http"

func Parse(r *http.Request) error { return r.ParseForm() }

func Value(r *http.Request) string { return r.PostFormValue("a") }
`},
		{name: "form parsing in tests", rel: "internal/api/x_test.go", src: `package api

import "net/http"

func f(r *http.Request) string { return r.FormValue("a") }
`},
		{name: "form parsing in a test outside api", rel: "internal/httpx/x_test.go", src: `package httpx

import "net/http"

func f(r *http.Request) error { return r.ParseForm() }
`},
	},
}

func checkFormParsing(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && formParsers[sel.Sel.Name] {
			out = append(out, f.at(sel, "%s names a net/http Request method that parses a form body: only %s serves requests, and it reads JSON bodies only", sel.Sel.Name, apiDir))
		}
		return true
	})
	return out
}
