package scoped_test

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

var updatePlans = flag.Bool("update-plans", false, "rewrite the golden query plans under testdata/plans")

var boundedSorts = map[string]string{
	"selectDated/three":   "sorts the chats a grant names, at most 1,000",
	"selectChanges/three": "sorts the changes of one window of 20,000 change numbers",
}

var fullScan = regexp.MustCompile(`^SCAN \S+( USING (COVERING )?INDEX \S+)?$`)

func TestEveryReadQueryHasAGoldenPlan(t *testing.T) {
	s := openStore(t)
	variants := []struct {
		name string
		g    policy.ReadGrant
	}{
		{"all", grantAll(t)},
		{"none", grant(t)},
		{"one", grant(t, alice)},
		{"three", grant(t, alice, bob, groupJID)},
	}
	for name, query := range scoped.Queries {
		t.Run(name, func(t *testing.T) {
			var golden strings.Builder
			for _, v := range variants {
				lines, err := s.Scoped().Plan(v.g, t.Context(), query)
				if err != nil {
					t.Fatalf("plan %s: %v", v.name, err)
				}
				golden.WriteString("== " + v.name + "\n" + strings.Join(lines, "\n") + "\n")
				if v.name == "none" {
					continue
				}
				for _, line := range lines {
					switch {
					case fullScan.MatchString(line):
						t.Errorf("%s with %s scopes reads a whole table: %q", name, v.name, line)
					case strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "VIRTUAL TABLE"):
						t.Errorf("%s with %s scopes scans: %q", name, v.name, line)
					case strings.Contains(line, "TEMP B-TREE") && boundedSorts[name+"/"+v.name] == "":
						t.Errorf("%s with %s scopes sorts in a temporary b-tree that no bound covers: %q", name, v.name, line)
					}
				}
			}
			path := filepath.Join("testdata", "plans", name+".txt")
			if *updatePlans {
				if err := os.WriteFile(path, []byte(golden.String()), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := fs.ReadFile(os.DirFS(filepath.Join("testdata", "plans")), name+".txt")
			if err != nil {
				t.Fatalf("read the golden plan: %v (regenerate with -update-plans)", err)
			}
			if golden.String() != string(want) {
				t.Fatalf("the plan of %s changed:\n%s\nwant:\n%s", name, golden.String(), want)
			}
		})
	}
}

func TestEveryQueryOfThePackageIsPlanned(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	constants := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.CONST {
				for _, spec := range d.Specs {
					for _, id := range spec.(*ast.ValueSpec).Names {
						constants[id.Name] = true
					}
				}
			}
		}
	}
	var declared []string
	for _, f := range parsed {
		forwards := fset.File(f.Pos()).Name() == "inbuilder.go"
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, id := range x.Names {
					if i < len(x.Values) && selects(x.Values[i]) {
						declared = append(declared, id.Name)
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || !sqlCalls[sel.Sel.Name] || len(x.Args) < 2 {
					return true
				}
				switch id, ok := ast.Unparen(x.Args[1]).(*ast.Ident); {
				case ok && constants[id.Name]:
					declared = append(declared, id.Name)
				case !ok || !forwards:
					t.Errorf("%s: the SQL text of %s is not a constant of the package, so it has no golden plan and no checked scope marker", fset.Position(x.Args[1].Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	var planned []string
	for name := range scoped.Queries {
		planned = append(planned, name)
	}
	slices.Sort(declared)
	declared = slices.Compact(declared)
	slices.Sort(planned)
	if !slices.Equal(declared, planned) {
		t.Fatalf("queries declared %q, planned %q: add every query to the golden plans", declared, planned)
	}
	golden, err := filepath.Glob(filepath.Join("testdata", "plans", "*.txt"))
	if err != nil || len(golden) != len(planned) {
		t.Fatalf("%d golden plans for %d queries: remove stale files", len(golden), len(planned))
	}
	for name, query := range scoped.Queries {
		marked := strings.Contains(query, "{scope ")
		switch {
		case unscopedScalars[name] != "" && (marked || !scoped.UnscopedScalars[query]):
			t.Errorf("%s is a reviewed unscoped scalar but carries a scope marker or is not allowed as one", name)
		case unscopedScalars[name] == "" && !marked:
			t.Errorf("%s carries no scope marker", name)
		}
	}
	if len(scoped.UnscopedScalars) != len(unscopedScalars) {
		t.Errorf("%d queries run without a scope, want only the %d reviewed", len(scoped.UnscopedScalars), len(unscopedScalars))
	}
}

var unscopedScalars = map[string]string{
	"selectLID":       "maps a phone chat to its LID chat",
	"selectTopChange": "the highest change number, no message content",
	"selectTopSeq":    "the highest message sequence number, no message content",
}

var sqlCalls = map[string]bool{"QueryContext": true, "QueryRowContext": true, "scalar": true}

var selectKeyword = regexp.MustCompile(`(?i)\bSELECT\b`)

func selects(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && selectKeyword.MatchString(lit.Value) {
			found = true
		}
		return !found
	})
	return found
}
