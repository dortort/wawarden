package archtest

import (
	"errors"
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func apiDeclarations(t *testing.T, root string) map[string]bool {
	t.Helper()
	files, err := moduleFiles(os.DirFS(filepath.Join(root, apiDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", apiDir, err)
	}
	declared := map[string]bool{}
	for _, f := range files {
		if f.test || f.dir != "." {
			continue
		}
		for _, decl := range f.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				declared[receiverPrefix(d)+d.Name.Name] = true
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					declared[ts.Name.Name] = true
					if st, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								declared[ts.Name.Name+"."+name.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return declared
}

func receiverPrefix(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	typ := d.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "."
	}
	return ""
}

func TestCompileTimeFirewalls(t *testing.T) {
	root := moduleRoot(t)
	declared := apiDeclarations(t, root)
	for _, name := range []string{"newRouter", "router", "router.mux", "router.read", "router.write", "router.admin"} {
		if !declared[name] {
			t.Fatalf("package api no longer declares %s, so the unexported mux fixtures prove nothing: update them", name)
		}
	}

	fixtures := []struct {
		name     string
		fragment string
	}{
		{name: "control"},
		{name: "grantliteral", fragment: "cannot refer to unexported field ok in struct literal of type policy.ReadGrant"},
		{name: "grantfield", fragment: "g.ok undefined (cannot refer to unexported field ok)"},
		{name: "unexportedmux", fragment: "undefined: api.newRouter"},
		{name: "unexportedrouter", fragment: "undefined: api.router"},
		{name: "foreignresponse", fragment: "leak does not implement dto.Response (unexported method encode)"},
	}
	testdata := filepath.Join(root, "internal", "archtest", "testdata")
	entries, err := os.ReadDir(testdata)
	if err != nil {
		t.Fatalf("read the fixtures: %v", err)
	}
	var onDisk, listed []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, fx := range fixtures {
		listed = append(listed, fx.name)
	}
	slices.Sort(onDisk)
	slices.Sort(listed)
	if !slices.Equal(onDisk, listed) {
		t.Fatalf("fixtures on disk %q, listed %q: every fixture must be built and asserted", onDisk, listed)
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "go", "build", ".")
			cmd.Dir = filepath.Join(testdata, fx.name)
			out, err := cmd.CombinedOutput()
			if fx.fragment == "" {
				if err != nil {
					t.Fatalf("the control fixture must compile, or the harness proves nothing: %v\n%s", err, out)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("go build = %v, want a compile failure\n%s", err, out)
			}
			if !strings.Contains(string(out), fx.fragment) {
				t.Fatalf("go build failed without %q:\n%s", fx.fragment, out)
			}
		})
	}
}
