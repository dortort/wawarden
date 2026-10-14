package archtest

import "go/ast"

const buildinfoDir = "internal/buildinfo"

var buildVersionRule = rule{
	name:  "build-version",
	check: checkBuildVersion,
	cases: []snippet{
		{name: "the linked version read directly", rel: "internal/api/x.go", want: 2, src: `package api

import (
	"github.com/dortort/wawarden/internal/buildinfo"
	bi "github.com/dortort/wawarden/internal/buildinfo"
)

var a, b = buildinfo.Version, bi.Version
`},
		{name: "the linked version read in a test", rel: "cmd/wawarden/x_test.go", want: 1, src: `package main

import "github.com/dortort/wawarden/internal/buildinfo"

var _ = buildinfo.Version
`},
		{name: "the version read through Read, and other Version names", rel: "internal/app/x.go", src: `package app

import (
	"runtime/debug"

	"github.com/dortort/wawarden/internal/buildinfo"
)

type t struct{ Version string }

var (
	_ = buildinfo.Read().Version
	_ = buildinfo.Info{}.Version
	_ = t{}.Version
	_ = debug.BuildInfo{}.Main.Version
)
`},
		{name: "the package itself", rel: "internal/buildinfo/x.go", src: `package buildinfo

var _ = Version
`},
	},
}

func checkBuildVersion(f *sourceFile) []string {
	if f.dir == buildinfoDir {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if s, p := f.ref(sel); s != nil && p == module+"/"+buildinfoDir && s.Sel.Name == "Version" {
				out = append(out, f.at(sel, "buildinfo.Version is only the version set at link time; read buildinfo.Read().Version, which also reports the module version of a go install build"))
			}
		}
		return true
	})
	return out
}
