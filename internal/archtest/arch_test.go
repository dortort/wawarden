// Package archtest enforces the module's architecture rules on its source files and through compile-time fixtures.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
	goroutineRule, netHTTPRule, dotImportRule, socketRule, muxRule, serverRule,
	inListRule, bannedImportRule, fenceRule, grantRule, thirdPartyRule, hiddenPackageRule, nolintRule, wildcardRule,
	credentialRule, preludeRule,
}

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

func moduleFiles(fsys fs.FS) ([]*sourceFile, error) {
	var files []*sourceFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if _, err := fs.Stat(fsys, path.Join(p, "go.mod")); err == nil || name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(name) != ".go" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
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
		"internal/listeners/listeners.go",
		"internal/policy/decide.go",
		"internal/safego/safego.go",
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
		"internal/a/testdata/fixture.go": {Data: []byte("package fixture\n")},
		"vendor/example.com/v/v.go":      {Data: []byte("package v\n")},
		".hidden/h.go":                   {Data: []byte("package h\n")},
		"_scratch/s.go":                  {Data: []byte("package s\n")},
		"internal/_skip.go":              {Data: []byte("package internal\n")},
		"nested/go.mod":                  {Data: []byte("module example.com/nested\n")},
		"nested/n.go":                    {Data: []byte("package nested\n")},
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
}

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
					if got := r.check(f); len(got) != c.want {
						t.Fatalf("%d findings, want %d: %q", len(got), c.want, got)
					}
				})
			}
			if !violating || !conforming {
				t.Fatal("every rule needs at least one violating and one conforming case")
			}
		})
	}
}
