// Package archtest enforces the module's architecture rules on its source files and through compile-time fixtures.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const module = "github.com/dortort/wawarden"

type importSpec struct {
	node *ast.ImportSpec
	path string
	name string
}

type sourceFile struct {
	rel     string
	dir     string
	test    bool
	fset    *token.FileSet
	file    *ast.File
	imports []importSpec
	names   map[string]string
}

type snippet struct {
	name string
	rel  string
	src  string
	want int
}

type rule struct {
	name  string
	check func(*sourceFile) []string
	cases []snippet
}

var rules = []rule{
	goroutineRule, netHTTPRule, dotImportRule, socketRule, muxRule, serverRule, handlerRule,
	inListRule, bannedImportRule, reflectionRule, fenceRule, chatMethodRule, thirdPartyRule, hiddenPackageRule, nolintRule, generatedRule, wildcardRule,
	credentialRule, preludeRule, secretComparisonRule, formRule, shadowRule, sealRule, keysDirectoryRule, standardLibraryOnlyRule, logHandlerRule, logOutputRule,
}

var majorVersion = regexp.MustCompile(`^v([2-9]|[1-9][0-9]+)$`)

func parseSource(rel string, src []byte) (*sourceFile, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	f := &sourceFile{rel: rel, dir: path.Dir(rel), test: strings.HasSuffix(rel, "_test.go"), fset: fset, file: file, names: map[string]string{}}
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		name := path.Base(p)
		if dir := path.Dir(p); dir != "." && majorVersion.MatchString(name) {
			name = path.Base(dir)
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		f.imports = append(f.imports, importSpec{node: spec, path: p, name: name})
		if name != "_" && name != "." {
			f.names[name] = p
		}
	}
	return f, nil
}

func (f *sourceFile) at(n ast.Node, format string, args ...any) string {
	return fmt.Sprintf("%s: %s", f.fset.Position(n.Pos()), fmt.Sprintf(format, args...))
}

func (f *sourceFile) pkgPath() string {
	if f.dir == "." {
		return module
	}
	return module + "/" + f.dir
}

func (f *sourceFile) ref(e ast.Expr) (*ast.SelectorExpr, string) {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return nil, ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil, ""
	}
	p, ok := f.names[x.Name]
	if !ok {
		return nil, ""
	}
	return sel, p
}

func (f *sourceFile) isType(e ast.Expr, pkg, name string) bool {
	e = ast.Unparen(e)
	if star, ok := e.(*ast.StarExpr); ok {
		e = ast.Unparen(star.X)
	}
	if id, ok := e.(*ast.Ident); ok {
		return f.pkgPath() == pkg && id.Name == name
	}
	sel, p := f.ref(e)
	return sel != nil && p == pkg && sel.Sel.Name == name
}

func (f *sourceFile) compositeLits(visit func(lit *ast.CompositeLit, typ ast.Expr)) {
	elided := map[*ast.CompositeLit]ast.Expr{}
	infer := func(e, typ ast.Expr) {
		if lit, ok := ast.Unparen(e).(*ast.CompositeLit); ok && lit.Type == nil && typ != nil {
			elided[lit] = typ
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typ := lit.Type
		if typ == nil {
			typ = elided[lit]
		}
		if typ != nil {
			visit(lit, typ)
		}
		var key, elem ast.Expr
		switch t := ast.Unparen(typ).(type) {
		case *ast.ArrayType:
			elem = t.Elt
		case *ast.MapType:
			key, elem = t.Key, t.Value
		}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				infer(kv.Key, key)
				e = kv.Value
			}
			infer(e, elem)
		}
		return true
	})
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

func addOperands(e ast.Expr) []ast.Expr {
	e = ast.Unparen(e)
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return append(addOperands(b.X), addOperands(b.Y)...)
	}
	return []ast.Expr{e}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := ast.Unparen(e).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func literalRuns(root ast.Node, visit func(at ast.Node, s string)) {
	var walk func(ast.Node) bool
	walk = func(n ast.Node) bool {
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
					visit(start, run.String())
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
				ast.Inspect(operand, walk)
			}
			flush()
			return false
		case *ast.BasicLit:
			if s, ok := stringLit(e); ok {
				visit(e, s)
			}
		}
		return true
	}
	ast.Inspect(root, walk)
}

func constString(e ast.Expr) (string, bool) {
	if e == nil {
		return "", false
	}
	var b strings.Builder
	for _, operand := range addOperands(e) {
		s, ok := stringLit(operand)
		if !ok {
			return "", false
		}
		b.WriteString(s)
	}
	return b.String(), true
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
	data, err := fs.ReadFile(os.DirFS(dir), "go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			if got := strings.TrimSpace(rest); got != module {
				t.Fatalf("go.mod declares module %q, want %q", got, module)
			}
			return dir
		}
	}
	t.Fatal("go.mod declares no module")
	return ""
}

var foreignSources = set(".c", ".cc", ".cpp", ".cxx", ".m", ".h", ".hh", ".hpp", ".hxx", ".f", ".F", ".for", ".f90",
	".s", ".S", ".sx", ".swig", ".swigcxx", ".syso")

func moduleFiles(fsys fs.FS) ([]*sourceFile, error) {
	var files []*sourceFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		name := d.Name()
		hidden := strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
		switch {
		case d.IsDir() && (hidden || name == "vendor" || name == "testdata"):
			return fs.SkipDir
		case hidden:
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link: the go tool follows it to code this walk does not read", p)
		case d.IsDir():
			if _, err := fs.Stat(fsys, path.Join(p, "go.mod")); err == nil {
				return fmt.Errorf("%s holds a nested module: a workspace or replace directive can build it in, and this walk does not read it", p)
			}
			return nil
		case foreignSources[path.Ext(name)]:
			return fmt.Errorf("%s is assembly, C or an object file: the go tool builds it in, and these rules read only Go", p)
		case path.Ext(name) != ".go":
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f, err := parseSource(p, src)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	return files, err
}

func TestArchitecture(t *testing.T) {
	files, err := moduleFiles(os.DirFS(moduleRoot(t)))
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	walked := map[string]bool{}
	for _, f := range files {
		walked[f.rel] = true
	}
	for _, rel := range []string{
		"cmd/wawarden/main.go",
		"internal/api/register.go",
		"internal/archtest/arch_test.go",
		"internal/keys/keys.go",
		"internal/listeners/listeners.go",
		"internal/logx/logx.go",
		"internal/policy/decide.go",
		"internal/policy/internal/seal/seal.go",
		"internal/safego/safego.go",
		"internal/sanitize/sanitize.go",
	} {
		if !walked[rel] {
			t.Fatalf("the walk missed %s, so every rule would pass on it vacuously", rel)
		}
	}
	for rel := range wildcardAllowances {
		if !walked[rel] {
			t.Errorf("the wildcard allow-list names %s, which the walk did not find", rel)
		}
	}
	for dir := range reflectAllowed {
		if !slices.ContainsFunc(files, func(f *sourceFile) bool {
			return !f.test && f.dir == dir && slices.ContainsFunc(f.imports, func(imp importSpec) bool { return imp.path == "reflect" })
		}) {
			t.Errorf("the reflect allow-list names %s, where no non-test file imports reflect", dir)
		}
	}
	for _, dir := range standardLibraryOnly {
		if !slices.ContainsFunc(files, func(f *sourceFile) bool { return !f.test && f.dir == dir }) {
			t.Errorf("the standard-library-only list names %s, where the walk found no non-test file", dir)
		}
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			for _, f := range files {
				for _, v := range r.check(f) {
					t.Error(v)
				}
			}
		})
	}
}

func TestModuleWalk(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":                         {Data: []byte("module " + module + "\n")},
		"main.go":                        {Data: []byte("package main\n")},
		"internal/a/a.go":                {Data: []byte("package a\n")},
		"internal/a/a_test.go":           {Data: []byte("package a\n")},
		"internal/a/notes.txt":           {Data: []byte("not go\n")},
		"internal/a/README.md":           {Data: []byte("# a\n")},
		".golangci.yml":                  {Data: []byte("version: \"2\"\n")},
		"internal/a/testdata/asm.s":      {Data: []byte("TEXT ·f(SB),$0\n")},
		"internal/a/_x_arm64.s":          {Data: []byte("TEXT ·f(SB),$0\n")},
		"internal/a/testdata/fixture.go": {Data: []byte("package fixture\n")},
		"vendor/example.com/v/v.go":      {Data: []byte("package v\n")},
		".hidden/h.go":                   {Data: []byte("package h\n")},
		"_scratch/s.go":                  {Data: []byte("package s\n")},
		"internal/_skip.go":              {Data: []byte("package internal\n")},
		"internal/a/testdata/go.mod":     {Data: []byte("module example.com/fixture\n")},
		"internal/a/.link":               {Data: []byte("internal/a"), Mode: fs.ModeSymlink},
	}
	files, err := moduleFiles(fsys)
	if err != nil {
		t.Fatalf("moduleFiles: %v", err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.rel)
	}
	slices.Sort(got)
	if want := []string{"internal/a/a.go", "internal/a/a_test.go", "main.go"}; !slices.Equal(got, want) {
		t.Fatalf("walked %q, want %q", got, want)
	}

	for name, extra := range map[string]fstest.MapFS{
		"a nested module":       {"nested/go.mod": {Data: []byte("module " + module + "/nested\n")}, "nested/n.go": {Data: []byte("package nested\n")}},
		"a symlinked directory": {"internal/evil": {Data: []byte("../outside"), Mode: fs.ModeSymlink}},
		"a symlinked Go file":   {"internal/a/b.go": {Data: []byte("a.go"), Mode: fs.ModeSymlink}},
		"Go assembly":           {"internal/a/forge_arm64.s": {Data: []byte("TEXT ·Forge(SB),NOSPLIT,$0-1\n")}},
		"preprocessed assembly": {"internal/a/forge_amd64.S": {Data: []byte("TEXT ·Forge(SB),NOSPLIT,$0-1\n")}},
		"a system object":       {"internal/a/rsrc_windows.syso": {Data: []byte{0x7f, 'E', 'L', 'F'}}},
		"C":                     {"internal/a/z.c": {Data: []byte("int z;\n")}},
		"a C header":            {"internal/a/z.h": {Data: []byte("int z;\n")}},
	} {
		t.Run(name, func(t *testing.T) {
			tree := maps.Clone(fsys)
			maps.Copy(tree, extra)
			if _, err := moduleFiles(tree); err == nil {
				t.Fatal("moduleFiles accepted a tree holding code it cannot read")
			}
		})
	}
}

var milestoneLabel = regexp.MustCompile(`\bM\d+\b`)

func TestRuleCases(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.name] {
			t.Fatalf("two rules are named %q", r.name)
		}
		seen[r.name] = true
		t.Run(r.name, func(t *testing.T) {
			var violating, conforming bool
			for _, c := range r.cases {
				violating = violating || c.want > 0
				conforming = conforming || c.want == 0
				t.Run(c.name, func(t *testing.T) {
					f, err := parseSource(c.rel, []byte(c.src))
					if err != nil {
						t.Fatalf("parse: %v", err)
					}
					got := r.check(f)
					if len(got) != c.want {
						t.Fatalf("%d findings, want %d: %q", len(got), c.want, got)
					}
					for _, finding := range got {
						if milestoneLabel.MatchString(finding) {
							t.Errorf("finding %q names a milestone, which means nothing to a reader of this repository", finding)
						}
					}
				})
			}
			if !violating || !conforming {
				t.Fatal("every rule needs at least one violating and one conforming case")
			}
		})
	}
}
