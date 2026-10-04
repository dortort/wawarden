package archtest

import (
	"encoding/json"
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

func declarations(t *testing.T, root, dir string) map[string]bool {
	t.Helper()
	files, err := moduleFiles(os.DirFS(filepath.Join(root, dir)))
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
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
	declared := declarations(t, root, apiDir)
	for _, name := range []string{"newRouter", "router", "router.mux", "router.read", "router.write", "router.admin"} {
		if !declared[name] {
			t.Fatalf("package api no longer declares %s, so the unexported mux fixtures prove nothing: update them", name)
		}
	}

	fixtures := []struct {
		name     string
		policy   bool
		fragment string
	}{
		{name: "control"},
		{name: "grantliteral", fragment: "cannot refer to unexported field ok in struct literal of type policy.ReadGrant"},
		{name: "grantfield", fragment: "g.ok undefined (cannot refer to unexported field ok)"},
		{name: "credentialcompare", fragment: "invalid operation: a == b (struct containing [0]func() cannot be compared)"},
		{name: "unexportedmux", fragment: "undefined: api.newRouter"},
		{name: "unexportedrouter", fragment: "undefined: api.router"},
		{name: "foreignresponse", fragment: "leak does not implement dto.Response (unexported method encode)"},
		{name: "sealimport", fragment: "use of internal package " + sealPath + " not allowed"},
		{name: "policycontrol", policy: true},
		{name: "chatliteral", policy: true, fragment: "cannot refer to unexported field ok in struct literal of type CanonicalChat"},
		{name: "chatslice", policy: true, fragment: "cannot refer to unexported field ok in struct literal of type CanonicalChat"},
		{name: "grantcontainer", policy: true, fragment: "cannot refer to unexported field ok in struct literal of type WriteGrant"},
		{name: "grantconstraint", policy: true, fragment: "AdminGrant does not satisfy ~struct{ok bool}"},
		{name: "chatlookalike", policy: true, fragment: "cannot convert lookalike{…} (value of struct type lookalike) to type CanonicalChat"},
		{name: "grantpointer", policy: true, fragment: "cannot convert &g (value of type *AdminGrant) to type *okbit"},
		{name: "grantwrite", policy: true, fragment: "g.ok undefined (cannot refer to unexported field ok)"},
		{name: "chatwrite", policy: true, fragment: "c.jid undefined (type *CanonicalChat has no field or method jid"},
		{name: "grantaddress", policy: true, fragment: "w.ok undefined (cannot refer to unexported field ok)"},
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
			if fx.policy {
				file := overlay(t, filepath.Join(testdata, fx.name), filepath.Join(root, policyDir))
				cmd = exec.CommandContext(t.Context(), "go", "build", "-overlay="+file, "./"+policyDir) //nolint:gosec // G204: the overlay is a file this test wrote
				cmd.Dir = root
			}
			out, err := cmd.CombinedOutput()
			if fx.fragment == "" {
				if err != nil {
					t.Fatalf("a control fixture must compile, or the harness proves nothing: %v\n%s", err, out)
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

func overlay(t *testing.T, fixture, pkg string) string {
	t.Helper()
	sources, err := filepath.Glob(filepath.Join(fixture, "*.go"))
	if err != nil || len(sources) == 0 {
		t.Fatalf("no Go files in %s: %v", fixture, err)
	}
	replace := map[string]string{}
	for _, src := range sources {
		replace[filepath.Join(pkg, "fixture_"+filepath.Base(src))] = src
	}
	data, err := json.Marshal(map[string]map[string]string{"Replace": replace})
	if err != nil {
		t.Fatalf("encode the overlay: %v", err)
	}
	file := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatalf("write the overlay: %v", err)
	}
	return file
}
