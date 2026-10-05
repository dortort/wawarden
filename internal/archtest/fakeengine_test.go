package archtest

import (
	"go/ast"
	"go/build/constraint"
	"slices"
)

const (
	fakeDir     = "internal/engine/fake"
	devTag      = "dev"
	maxBuildTag = 16
)

var fakeAllowedImports = set("bytes", "compress/zlib", "context", "encoding/json", "errors", "io", "slices", "sync", "time",
	module+"/internal/engine", module+"/internal/policy", module+"/internal/safego")

var devOnlyRule = rule{
	name:  "dev-only-fake-engine",
	check: checkDevOnly,
	cases: []snippet{
		{name: "an untagged fake file", rel: "internal/engine/fake/x.go", want: 1, src: "package fake\n"},
		{name: "a fake file in a subpackage", rel: "internal/engine/fake/sub/x.go", want: 1, src: "package sub\n"},
		{name: "a fake file built with dev or linux", rel: "internal/engine/fake/x.go", want: 1, src: "//go:build dev || linux\n\npackage fake\n"},
		{name: "a fake file built without dev", rel: "internal/engine/fake/x.go", want: 1, src: "//go:build !dev\n\npackage fake\n"},
		{name: "a fake test built with dev or anywhere but windows", rel: "internal/engine/fake/x_test.go", want: 1, src: "//go:build dev || !windows\n\npackage fake\n"},
		{name: "a fake file with only a legacy build line", rel: "internal/engine/fake/x.go", want: 1, src: "// +build dev\n\npackage fake\n"},
		{name: "a build line after a block comment on its line", rel: "internal/engine/fake/x.go", want: 1, src: "/* x */ //go:build dev\n\npackage fake\n"},
		{name: "a build line inside a block comment", rel: "internal/engine/fake/x.go", want: 1, src: "/*\n//go:build dev\n*/\n\npackage fake\n"},
		{name: "two build lines", rel: "internal/engine/fake/x.go", want: 1, src: "//go:build dev\n//go:build dev\n\npackage fake\n"},
		{name: "a build line after the package clause", rel: "internal/engine/fake/x.go", want: 1, src: "package fake\n\n//go:build dev\n"},
		{name: "an untagged importer", rel: "internal/app/x.go", want: 1, src: `package app

import (
	"github.com/dortort/wawarden/internal/engine/fake"
	sub "github.com/dortort/wawarden/internal/engine/fake/sub"
)

var _, _ = fake.New, sub.X
`},
		{name: "an importer built with dev or a race detector", rel: "cmd/wawarden/x_test.go", want: 1, src: `//go:build dev || race

package main

import _ "github.com/dortort/wawarden/internal/engine/fake"
`},
		{name: "fake files built only with dev", rel: "internal/engine/fake/x.go", src: "//go:build dev && (linux || darwin)\n\npackage fake\n"},
		{name: "a fake test built only with dev", rel: "internal/engine/fake/x_test.go", src: "// Leading comment.\n\n//go:build dev\n\npackage fake\n"},
		{name: "an importer built only with dev", rel: "internal/app/x.go", src: `//go:build dev

package app

import "github.com/dortort/wawarden/internal/engine/fake"

var _ = fake.New
`},
		{name: "a release file naming the package in a string, and a neighbouring directory", rel: "internal/engine/fakes/x.go", src: `package fakes

import _ "github.com/dortort/wawarden/internal/engine/fakes/sub"

const path = "github.com/dortort/wawarden/internal/engine/fake"
`},
	},
}

var fakeImportRule = rule{
	name:  "fake-engine-imports",
	check: checkFakeImports,
	cases: []snippet{
		{name: "network, process and adapter imports in the fake", rel: "internal/engine/fake/x.go", want: 6, src: `//go:build dev

package fake

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"

	"github.com/dortort/wawarden/internal/engine/wa"
)
`},
		{name: "the allowed imports", rel: "internal/engine/fake/x.go", src: `//go:build dev

package fake

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)
`},
		{name: "a fake test", rel: "internal/engine/fake/x_test.go", src: `//go:build dev

package fake

import "os"
`},
	},
}

func checkDevOnly(f *sourceFile) []string {
	if within(f.dir, fakeDir) {
		if devOnly(f) {
			return nil
		}
		return []string{f.at(f.file.Name, "every file of %s must carry a //go:build line that is false whenever the %s tag is not set", fakeDir, devTag)}
	}
	for _, imp := range f.imports {
		if within(imp.path, module+"/"+fakeDir) && !devOnly(f) {
			return []string{f.at(imp.node, "a file that imports %q must carry a //go:build line that is false whenever the %s tag is not set", imp.path, devTag)}
		}
	}
	return nil
}

func checkFakeImports(f *sourceFile) []string {
	if f.test || !within(f.dir, fakeDir) {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if !fakeAllowedImports[imp.path] {
			out = append(out, f.at(imp.node, "%s may import only packages that cannot reach the network or another program, not %q", fakeDir, imp.path))
		}
	}
	return out
}

func devOnly(f *sourceFile) bool {
	var lines []*ast.Comment
	var header []*ast.Comment
	for _, g := range f.file.Comments {
		if g.Pos() >= f.file.Package {
			break
		}
		header = append(header, g.List...)
	}
	for _, c := range header {
		if !constraint.IsGoBuild(c.Text) {
			continue
		}
		line := f.fset.Position(c.Pos()).Line
		if slices.ContainsFunc(header, func(o *ast.Comment) bool { return o.Pos() < c.Pos() && f.fset.Position(o.End()).Line == line }) {
			return false
		}
		lines = append(lines, c)
	}
	if len(lines) != 1 {
		return false
	}
	expr, err := constraint.Parse(lines[0].Text)
	if err != nil {
		return false
	}
	var tags []string
	collectTags(expr, &tags)
	if len(tags) > maxBuildTag {
		return false
	}
	for assignment := range 1 << len(tags) {
		if expr.Eval(func(tag string) bool {
			i := slices.Index(tags, tag)
			return i >= 0 && assignment&(1<<i) != 0
		}) {
			return false
		}
	}
	return true
}

func collectTags(x constraint.Expr, tags *[]string) {
	switch x := x.(type) {
	case *constraint.TagExpr:
		if x.Tag != devTag && !slices.Contains(*tags, x.Tag) {
			*tags = append(*tags, x.Tag)
		}
	case *constraint.NotExpr:
		collectTags(x.X, tags)
	case *constraint.AndExpr:
		collectTags(x.X, tags)
		collectTags(x.Y, tags)
	case *constraint.OrExpr:
		collectTags(x.X, tags)
		collectTags(x.Y, tags)
	}
}
