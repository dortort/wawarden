package archtest

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	apiDir      = "internal/api"
	apiAdminDir = "internal/api/admin"
	appDir      = "internal/app"
	policyDir   = "internal/policy"
	sealDir     = policyDir + "/internal/seal"
	sealPath    = module + "/" + sealDir
)

var bannedImports = set("net/http/pprof", "expvar", "net/http/cgi", "net/http/fcgi", "plugin", "unsafe", "C")

var apiDenied = []string{"database/sql", "html/template", "text/template", "modernc.org/sqlite", "go.mau.fi/whatsmeow", module + "/internal/store/ingest"}

var policyAllowed = set("bytes", "cmp", "crypto/sha256", "crypto/subtle", "encoding/base64", "encoding/binary", "encoding/hex",
	"errors", "hash/crc32", "iter", "maps", "slices", "sort", "strconv", "strings", "time", "unicode", "unicode/utf8", "unique")

var importableModules []string

var indirectOnlyModules []string

var bannedImportRule = rule{
	name:  "banned-imports",
	check: checkBannedImports,
	cases: []snippet{
		{name: "every banned import in a test", rel: "internal/app/x_test.go", want: 7, src: `package app

import (
	"C"
	_ "expvar"
	"net/http/cgi"
	"net/http/fcgi"
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
	"net/http/httputil"
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

var reflectAllowed = map[string]string{
	"internal/api/dto": "compares a response's declaring package with its own",
}

var unsafePointerMethods = set("NewAt", "SetPointer", "UnsafeAddr", "UnsafePointer")

var reflectionRule = rule{
	name:  "reflection",
	check: checkReflection,
	cases: []snippet{
		{name: "reflect outside the reviewed packages", rel: "internal/listeners/x.go", want: 1, src: `package listeners

import (
	"net/http"
	r "reflect"
)

func f() *http.Server { return r.New(r.TypeFor[http.Server]()).Interface().(*http.Server) }
`},
		{name: "unsafe pointers through any receiver", rel: "internal/app/x_test.go", want: 7, src: `package app

import r "reflect"

type wrapped struct{ r.Value }

func f(g *struct{ ok bool }, v r.Value) {
	r.NewAt(r.TypeFor[struct{ OK bool }](), r.ValueOf(g).UnsafePointer()).Elem().Field(0).SetBool(true)
	(*struct{ OK bool })(wrapped{r.ValueOf(g)}.UnsafePointer()).OK = true
	_ = v.UnsafeAddr()
	v.SetPointer(nil)
	_ = r.Value.UnsafePointer
	_ = r.NewAt
}
`},
		{name: "unsafe pointers in a reviewed package", rel: "internal/api/dto/x.go", want: 1, src: `package dto

import "reflect"

func f(v reflect.Value) { _ = v.UnsafePointer() }
`},
		{name: "type inspection in a reviewed package", rel: "internal/api/dto/x.go", src: `package dto

import "reflect"

var pkgPath = reflect.TypeFor[int]().PkgPath()

func f(r any) bool { return reflect.TypeOf(r).Kind() == reflect.Pointer }
`},
		{name: "reflection in a test", rel: "internal/policy/x_test.go", src: `package policy

import "reflect"

func f(a, b any) bool {
	return reflect.DeepEqual(a, b) && reflect.ValueOf(a).IsZero() && reflect.ValueOf(b).Pointer() != 0
}
`},
		{name: "another package named reflect", rel: "internal/app/x.go", src: `package app

import "example.com/reflect"

var _ = reflect.TypeOf
`},
	},
}

func checkReflection(f *sourceFile) []string {
	var out []string
	if _, reviewed := reflectAllowed[f.dir]; !f.test && !reviewed {
		for _, imp := range f.imports {
			if imp.path == "reflect" {
				out = append(out, f.at(imp.node, "reflect outside the reviewed packages can build zero values and reach unexported state"))
			}
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && unsafePointerMethods[sel.Sel.Name] {
			out = append(out, f.at(sel, "%s reads or writes memory through an unsafe.Pointer without importing unsafe", sel.Sel.Name))
		}
		return true
	})
	return out
}

var fenceRule = rule{
	name:  "import-fences",
	check: checkFences,
	cases: []snippet{
		{name: "api imports past its fence", rel: "internal/api/x.go", want: 11, src: `package api

import (
	"database/sql"
	"database/sql/driver"
	"html/template"
	"mime/multipart"
	"text/template/parse"
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
		{name: "api/admin imports the admin store and mime", rel: "internal/api/admin/x.go", src: `package admin

import (
	"mime"
	"github.com/dortort/wawarden/internal/store/admin"
)
`},
		{name: "outside api", rel: "internal/apiary/x.go", src: `package apiary

import (
	"database/sql"
	"github.com/dortort/wawarden/internal/store/ingest"
	"html/template"
	"text/template"
)
`},
		{name: "an api test", rel: "internal/api/x_test.go", src: `package api

import "database/sql"
`},
		{name: "multipart in a helper outside api", rel: "internal/httpx/x.go", want: 1, src: `package httpx

import "mime/multipart"
`},
		{name: "multipart in a test outside api", rel: "internal/httpx/x_test.go", src: `package httpx

import "mime/multipart"
`},
		{name: "policy imports I/O and non-standard packages", rel: "internal/policy/x.go", want: 19, src: `package policy

import (
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"example.com/lib"
	"fmt"
	"github.com/dortort/wawarden/internal/token"
	"io/fs"
	"io/ioutil"
	"log"
	"log/syslog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"syscall"
)
`},
		{name: "policy imports pure standard packages", rel: "internal/policy/x.go", src: `package policy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"maps"
	"slices"
	"time"
	"unique"
)
`},
		{name: "policy imports its seal package", rel: "internal/policy/x.go", src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"
`},
		{name: "the seal package imports I/O, package policy and itself", rel: "internal/policy/internal/seal/x.go", want: 3, src: `package seal

import (
	"fmt"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/policy/internal/seal"
)
`},
		{name: "a policy test imports the module", rel: "internal/policy/x_test.go", src: `package policy

import (
	"os"
	"github.com/dortort/wawarden/internal/token"
)
`},
		{name: "listeners opened outside the app", rel: "cmd/wawarden/x.go", want: 2, src: `package main

import (
	"github.com/dortort/wawarden/internal/listeners"
	"github.com/dortort/wawarden/internal/listeners/listenertest"
)
`},
		{name: "the app opens the listeners", rel: "internal/app/x.go", src: `package app

import "github.com/dortort/wawarden/internal/listeners"
`},
		{name: "a test inspects the listeners", rel: "internal/apiary/x_test.go", src: `package apiary

import "github.com/dortort/wawarden/internal/listeners/listenertest"
`},
	},
}

func checkFences(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if within(imp.path, module+"/"+listenersDir) && f.dir != appDir && !within(f.dir, listenersDir) {
			out = append(out, f.at(imp.node, "only %s may import %q, so the app's inventory names every listener", appDir, imp.path))
		}
		if within(imp.path, "mime/multipart") {
			out = append(out, f.at(imp.node, "%q parses multipart bodies, which nothing in the module reads: only %s serves requests, and it reads JSON bodies only", imp.path, apiDir))
		}
		switch {
		case within(f.dir, apiDir):
			if slices.ContainsFunc(apiDenied, func(denied string) bool { return within(imp.path, denied) }) ||
				within(imp.path, module+"/internal/store/admin") && !within(f.dir, apiAdminDir) {
				out = append(out, f.at(imp.node, "%s may not import %q", apiDir, imp.path))
			}
		case within(f.dir, policyDir):
			if !policyAllowed[imp.path] && (imp.path != sealPath || f.dir != policyDir) {
				out = append(out, f.at(imp.node, "%s may import only the reviewed pure standard-library packages, and package policy also its seal package, not %q", policyDir, imp.path))
			}
		}
	}
	return out
}

var standardLibraryOnly = []string{"internal/keys", "internal/logx", "internal/sanitize"}

var standardLibraryOnlyRule = rule{
	name:  "standard-library-only",
	check: checkStandardLibraryOnly,
	cases: []snippet{
		{name: "module and third-party imports in a confined package", rel: "internal/logx/x.go", want: 3, src: `package logx

import (
	"log/slog"

	"example.com/redact"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/policy"
)
`},
		{name: "a module import in a confined package's subpackage", rel: "internal/keys/sub/x.go", want: 1, src: `package sub

import "github.com/dortort/wawarden/internal/token"
`},
		{name: "standard-library imports in a confined package", rel: "internal/sanitize/x.go", src: `package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)
`},
		{name: "module imports in a confined package's test", rel: "internal/logx/x_test.go", src: `package logx

import (
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)
`},
		{name: "module imports outside the confined packages", rel: "internal/logxtra/x.go", src: `package logxtra

import "github.com/dortort/wawarden/internal/keys"
`},
	},
}

func checkStandardLibraryOnly(f *sourceFile) []string {
	i := slices.IndexFunc(standardLibraryOnly, func(dir string) bool { return within(f.dir, dir) })
	if f.test || i < 0 {
		return nil
	}
	std, err := standardLibrary()
	if err != nil {
		return []string{f.at(f.file, "go list std: %v", err)}
	}
	var out []string
	for _, imp := range f.imports {
		if !std[imp.path] {
			out = append(out, f.at(imp.node, "%s may import only the standard library, not %q", standardLibraryOnly[i], imp.path))
		}
	}
	return out
}

var standardLibrary = sync.OnceValues(func() (map[string]bool, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "std").Output()
	if err != nil {
		return nil, err
	}
	return set(strings.Fields(string(out))...), nil
})

var thirdPartyRule = rule{
	name:  "third-party-imports",
	check: thirdPartyImports(importableModules),
	cases: []snippet{
		{name: "third-party imports", rel: "internal/app/x_test.go", want: 4, src: `package app

import (
	"golang.org/x/tools/go/packages"
	"github.com/dortort/wawarden-fork/internal/policy"
	"evil/pkg"
	"fmtx"
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
		std, err := standardLibrary()
		if err != nil {
			return []string{f.at(f.file, "go list std: %v", err)}
		}
		var out []string
		for _, imp := range f.imports {
			if std[imp.path] || within(imp.path, module) || slices.ContainsFunc(allowed, func(m string) bool { return within(imp.path, m) }) {
				continue
			}
			out = append(out, f.at(imp.node, "%q is outside the standard library and this module, and not on the reviewed list of importable modules", imp.path))
		}
		return out
	}
}

var goModDirectives = set("module", "go", "toolchain")

func goModProblems(gomod string, importable, indirectOnly []string) []string {
	var out []string
	var block string
	for i, line := range strings.Split(gomod, "\n") {
		code, comment, _ := strings.Cut(line, "//")
		fields := strings.Fields(code)
		if len(fields) == 0 {
			continue
		}
		directive, args := block, fields
		switch {
		case block != "" && fields[0] == ")":
			block = ""
			continue
		case block == "":
			directive, args = fields[0], fields[1:]
			if len(args) == 1 && args[0] == "(" {
				block = directive
				continue
			}
		}
		listed := func(modules []string) bool {
			return len(args) > 0 && slices.ContainsFunc(modules, func(m string) bool { return within(args[0], m) })
		}
		note := strings.Fields(comment)
		indirect := len(note) == 1 && note[0] == "indirect" || len(note) > 1 && note[0] == "indirect;"
		if goModDirectives[directive] || directive == "require" && (listed(importable) || indirect && listed(indirectOnly)) {
			continue
		}
		out = append(out, fmt.Sprintf("go.mod:%d: %s %s: only module, go, toolchain, requirements on reviewed importable modules and indirect requirements on reviewed indirect-only modules are allowed", i+1, directive, strings.Join(args, " ")))
	}
	return out
}

func TestGoModDirectives(t *testing.T) {
	importable := []string{"example.com/allowed"}
	indirectOnly := []string{"example.com/transitive"}
	for _, tt := range []struct {
		name  string
		gomod string
		want  int
	}{
		{name: "the module's own directives", gomod: "module " + module + "\n\ngo 1.26.0 // comment\n\ntoolchain go1.27.1\n"},
		{name: "reviewed requirements", gomod: "require example.com/allowed v1.0.0\n\nrequire (\n\texample.com/allowed/sub v1.0.0 // indirect\n\texample.com/transitive v1.0.0 // indirect\n" +
			"\texample.com/transitive/sub v1.0.0 // indirect; for a test\n)\n\nrequire example.com/transitive/other v1.0.0 //indirect\n"},
		{name: "unreviewed requirements", want: 4, gomod: "require example.com/allowedx v1.0.0\n\nrequire (\n\t" + module + "/internal/evil v0.0.0\n\texample.com/other v1.0.0 // indirect\n" +
			"\texample.com/transitivex v1.0.0 // indirect\n)\n"},
		{name: "indirect-only modules required directly", want: 4, gomod: "require example.com/transitive v1.0.0\n\nrequire (\n\texample.com/transitive/sub v1.0.0 // indirectly\n" +
			"\texample.com/transitive v1.0.0 // indirect;\n\texample.com/transitive v1.0.0 // not indirect\n)\n"},
		{name: "replacements and other directives", want: 7, gomod: "replace " + module + "/internal/evil => ../evil\n\nreplace (\n\texample.com/allowed => ./x\n)\n\n" +
			"exclude example.com/allowed v1.0.0\n\nretract v0.1.0\n\ntool example.com/allowed/cmd\n\ngodebug default=go1.20\n\nignore ./x\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := goModProblems(tt.gomod, importable, indirectOnly); len(got) != tt.want {
				t.Fatalf("%d problems, want %d: %q", len(got), tt.want, got)
			}
		})
	}
}

func TestModuleDefinition(t *testing.T) {
	root := moduleRoot(t)
	data, err := fs.ReadFile(os.DirFS(root), "go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, problem := range goModProblems(string(data), importableModules, indirectOnlyModules) {
		t.Error(problem)
	}
	switch _, err := os.Lstat(filepath.Join(root, "go.work")); {
	case err == nil:
		t.Error("go.work at the module root: a workspace builds in modules this walk does not read")
	case !errors.Is(err, fs.ErrNotExist):
		t.Errorf("stat go.work: %v", err)
	}
}

func TestNoVendorDirectory(t *testing.T) {
	switch _, err := os.Lstat(filepath.Join(moduleRoot(t), "vendor")); {
	case err == nil:
		t.Error("vendor at the module root: modules are pinned by go.sum and never vendored")
	case !errors.Is(err, fs.ErrNotExist):
		t.Errorf("stat vendor: %v", err)
	}
}

func TestWorkflowsUseReadOnlyModules(t *testing.T) {
	root := os.DirFS(moduleRoot(t))
	workflows, err := fs.Glob(root, ".github/workflows/*")
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	checked := 0
	for _, name := range workflows {
		data, err := fs.ReadFile(root, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		config := string(data)
		if !strings.Contains(config, "actions/setup-go@") {
			continue
		}
		checked++
		for _, problem := range workflowModuleProblems(config) {
			t.Errorf("%s: %s", name, problem)
		}
	}
	if checked == 0 {
		t.Fatal("no workflow sets up Go, so this check passes vacuously")
	}
}

var moduleFlag = regexp.MustCompile(`(?:^|\W)-mod\b(\S*)`)

func workflowModuleProblems(config string) []string {
	var out []string
	if got, _ := yamlValue(config, "env", "GOFLAGS"); got != "-mod=readonly" {
		out = append(out, fmt.Sprintf("env.GOFLAGS is %q, want -mod=readonly, so that no job builds from a vendor directory", got))
	}
	lines := strings.Split(config, "\n")
	for i, line := range lines {
		if strings.Contains(line, "GOFLAGS") && strings.TrimSpace(line) != "GOFLAGS: -mod=readonly" {
			out = append(out, fmt.Sprintf("line %d: %q may set GOFLAGS to something other than -mod=readonly", i+1, strings.TrimSpace(line)))
		}
	}
	for _, m := range moduleFlag.FindAllStringSubmatchIndex(config, -1) {
		if config[m[2]:m[3]] != "=readonly" {
			i := strings.Count(config[:m[2]], "\n")
			out = append(out, fmt.Sprintf("line %d: %q may set a module mode other than -mod=readonly", i+1, strings.TrimSpace(lines[i])))
		}
	}
	return out
}

func TestWorkflowModuleProblems(t *testing.T) {
	const prefix = "env:\n  GOFLAGS: -mod=readonly\njobs:\n  build:\n"
	for _, tt := range []struct {
		name   string
		config string
		want   int
	}{
		{name: "read-only modules", config: prefix + "    env:\n      GOFLAGS: -mod=readonly\n    steps:\n      - name: go-mod-verify\n" +
			"        run: go build -mod=readonly -modfile=go.mod -modcacherw ./...\n      - uses: github/codeql-action/init@v4\n        with:\n          build-mode: manual\n"},
		{name: "no workflow-wide GOFLAGS", want: 1, config: "jobs:\n  build:\n    env:\n      GOFLAGS: -mod=readonly\n"},
		{name: "workflow-wide vendor mode", want: 3, config: "env:\n  GOFLAGS: -mod=vendor\n"},
		{name: "space-separated flag", want: 1, config: prefix + "    steps:\n      - run: go build -mod vendor ./...\n"},
		{name: "flag with a value", want: 1, config: prefix + "    steps:\n      - run: go build -mod=vendor ./...\n"},
		{name: "double-dash flag", want: 1, config: prefix + "    steps:\n      - run: go list --mod=mod -m all\n"},
		{name: "value on the next line", want: 1, config: prefix + "    steps:\n      - run: |\n          go build -mod\n            vendor ./...\n"},
		{name: "shell-quoted value", want: 1, config: prefix + "    steps:\n      - run: go build -mod'='vendor ./...\n"},
		{name: "job-level GOFLAGS", want: 2, config: prefix + "    env:\n      GOFLAGS: -mod=vendor\n"},
		{name: "extra GOFLAGS", want: 1, config: prefix + "    env:\n      GOFLAGS: -mod=readonly -modcacherw\n"},
		{name: "go env -w GOFLAGS", want: 2, config: prefix + "    steps:\n      - run: go env -w GOFLAGS=-mod=vendor\n"},
		{name: "GOFLAGS through GITHUB_ENV", want: 1, config: prefix + "    steps:\n      - run: echo GOFLAGS= >> \"$GITHUB_ENV\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := workflowModuleProblems(tt.config); len(got) != tt.want {
				t.Fatalf("%d problems, want %d: %q", len(got), tt.want, got)
			}
		})
	}
}

var hiddenPackageRule = rule{
	name:  "hidden-packages",
	check: checkHiddenPackages,
	cases: []snippet{
		{name: "module packages in skipped directories", rel: "internal/app/x.go", want: 5, src: `package app

import (
	_ "github.com/dortort/wawarden/internal/app/testdata/side"
	_ "github.com/dortort/wawarden/internal/app/_hidden"
	dot "github.com/dortort/wawarden/internal/.dot"
	_ "github.com/dortort/wawarden/vendor/example.com/v"
	_ "github.com/dortort/wawarden/_x/testdata"
)
`},
		{name: "a test imports a fixture", rel: "internal/app/x_test.go", want: 1, src: `package app

import _ "github.com/dortort/wawarden/internal/archtest/testdata/control"
`},
		{name: "walked packages and other modules", rel: "internal/app/x.go", src: `package app

import (
	_ "embed"
	_ "example.com/testdata/_x/.y"
	_ "github.com/dortort/wawarden"
	_ "github.com/dortort/wawarden/internal/a_b"
	_ "github.com/dortort/wawarden/internal/testdatax"
	_ "github.com/dortort/wawarden/internal/x.y"
)
`},
	},
}

func checkHiddenPackages(f *sourceFile) []string {
	var out []string
	for _, imp := range f.imports {
		rest, ok := strings.CutPrefix(imp.path, module+"/")
		if !ok {
			continue
		}
		if slices.ContainsFunc(strings.Split(rest, "/"), func(elem string) bool {
			return elem == "testdata" || elem == "vendor" || strings.HasPrefix(elem, "_") || strings.HasPrefix(elem, ".")
		}) {
			out = append(out, f.at(imp.node, "%q is in a directory that the architecture tests and the linters skip, yet it would be built in", imp.path))
		}
	}
	return out
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
