package archtest

import (
	"slices"
	"strings"
	"testing"
)

const (
	apiDir      = "internal/api"
	apiAdminDir = "internal/api/admin"
	policyDir   = "internal/policy"
)

var bannedImports = set("net/http/pprof", "expvar", "plugin", "unsafe", "C")

var apiDenied = []string{"database/sql", "modernc.org/sqlite", "go.mau.fi/whatsmeow", module + "/internal/store/ingest"}

var policyDenied = set("net", "net/http", "os", "os/exec", "database/sql", "io/fs")

var allowedModules []string

var bannedImportRule = rule{
	name:  "banned-imports",
	check: checkBannedImports,
	cases: []snippet{
		{name: "every banned import in a test", rel: "internal/app/x_test.go", want: 5, src: `package app

import (
	"C"
	_ "expvar"
	_ "net/http/pprof"
	"plugin"
	"unsafe"
)
`},
		{name: "a banned import under an alias", rel: "cmd/wawarden/x.go", want: 1, src: `package main

import p "plugin"
`},
		{name: "neighbouring packages", rel: "internal/app/x.go", src: `package app

import (
	"net/http"
	"runtime/pprof"
	"unicode/utf8"
)
`},
	},
}

func checkBannedImports(f *sourceFile) []string {
	var out []string
	for _, imp := range f.imports {
		if bannedImports[imp.path] {
			out = append(out, f.at(imp.node, "import of %q is banned", imp.path))
		}
	}
	return out
}

var fenceRule = rule{
	name:  "import-fences",
	check: checkFences,
	cases: []snippet{
		{name: "api imports past its fence", rel: "internal/api/x.go", want: 8, src: `package api

import (
	"database/sql"
	"database/sql/driver"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)
`},
		{name: "an api subpackage imports past the fence", rel: "internal/api/dto/x.go", want: 2, src: `package dto

import (
	"database/sql"
	"github.com/dortort/wawarden/internal/store/admin/sub"
)
`},
		{name: "api/admin imports the admin store", rel: "internal/api/admin/x.go", src: `package admin

import "github.com/dortort/wawarden/internal/store/admin"
`},
		{name: "outside api", rel: "internal/apiary/x.go", src: `package apiary

import (
	"database/sql"
	"github.com/dortort/wawarden/internal/store/ingest"
)
`},
		{name: "an api test", rel: "internal/api/x_test.go", src: `package api

import "database/sql"
`},
		{name: "policy imports I/O and non-standard packages", rel: "internal/policy/x.go", want: 8, src: `package policy

import (
	"database/sql"
	"example.com/lib"
	"github.com/dortort/wawarden/internal/token"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
)
`},
		{name: "policy imports pure standard packages", rel: "internal/policy/x.go", src: `package policy

import (
	"crypto/sha256"
	"crypto/subtle"
	"maps"
	"net/netip"
	"time"
)
`},
		{name: "a policy test imports the module", rel: "internal/policy/x_test.go", src: `package policy

import (
	"os"
	"github.com/dortort/wawarden/internal/token"
)
`},
	},
}

func checkFences(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		switch {
		case within(f.dir, apiDir):
			if slices.ContainsFunc(apiDenied, func(denied string) bool { return within(imp.path, denied) }) ||
				within(imp.path, module+"/internal/store/admin") && !within(f.dir, apiAdminDir) {
				out = append(out, f.at(imp.node, "%s may not import %q", apiDir, imp.path))
			}
		case within(f.dir, policyDir):
			if !standard(imp.path) || policyDenied[imp.path] {
				out = append(out, f.at(imp.node, "%s may import only pure standard-library packages, not %q", policyDir, imp.path))
			}
		}
	}
	return out
}

func standard(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

var thirdPartyRule = rule{
	name:  "third-party-imports",
	check: thirdPartyImports(allowedModules),
	cases: []snippet{
		{name: "third-party imports", rel: "internal/app/x_test.go", want: 2, src: `package app

import (
	"golang.org/x/tools/go/packages"
	"github.com/dortort/wawarden-fork/internal/policy"
)
`},
		{name: "standard library and this module", rel: "internal/app/x.go", src: `package app

import (
	"fmt"
	"net/http"
	"github.com/dortort/wawarden"
	"github.com/dortort/wawarden/internal/policy"
)
`},
	},
}

func thirdPartyImports(allowed []string) func(*sourceFile) []string {
	return func(f *sourceFile) []string {
		var out []string
		for _, imp := range f.imports {
			if standard(imp.path) || within(imp.path, module) || slices.ContainsFunc(allowed, func(m string) bool { return within(imp.path, m) }) {
				continue
			}
			out = append(out, f.at(imp.node, "%q is outside the standard library and this module, and not on the reviewed allow-list", imp.path))
		}
		return out
	}
}

func TestThirdPartyAllowList(t *testing.T) {
	f, err := parseSource("internal/app/x.go", []byte(`package app

import (
	"example.com/allowed"
	"example.com/allowed/sub"
	"example.com/allowedx"
	"example.com/other"
)
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := thirdPartyImports([]string{"example.com/allowed"})(f); len(got) != 2 {
		t.Fatalf("findings %q, want exactly example.com/allowedx and example.com/other", got)
	}
}
