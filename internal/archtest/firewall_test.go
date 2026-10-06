package archtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"maps"
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

var routerMembers = set(muxField, "routes", "now", "credential", "read", "write", "admin", "mcp", muxRegister, "ServeHTTP")

func anchorProblems(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	api := declarations(t, root, apiDir)
	members := map[string]bool{}
	for name := range api {
		if member, ok := strings.CutPrefix(name, "router."); ok {
			members[member] = true
		}
	}
	if !maps.Equal(members, routerMembers) {
		out = append(out, fmt.Sprintf("package api's router declares %q, but the mux-ownership rule was written for %q: review the new members and update the rule and this list together",
			slices.Sorted(maps.Keys(members)), slices.Sorted(maps.Keys(routerMembers))))
	}
	if !api[muxDecide] {
		out = append(out, fmt.Sprintf("package api no longer declares %s, so the mux-ownership rule guards nothing by that name: update it", muxDecide))
	}
	for _, helper := range []string{mcpToolHelper, mcpEndpointFunc} {
		if !api[helper] {
			out = append(out, fmt.Sprintf("package api no longer declares %s, the only function where the mcp-registration rule allows its registrations: update it", helper))
		}
	}
	handlers := map[string]bool{}
	for name := range declarations(t, root, listenersDir) {
		if typ, ok := strings.CutSuffix(name, ".Handler"); ok && ast.IsExported(typ) {
			handlers[typ] = true
		}
	}
	if !maps.Equal(handlers, set(listenerSpec)) {
		out = append(out, fmt.Sprintf("package listeners declares the exported types %q with a Handler field, but the handler-ownership rule guards only %s: update it",
			slices.Sorted(maps.Keys(handlers)), listenerSpec))
	}
	if handles := rawHandles(t, root); !maps.Equal(handles, set("DB."+rawHandleMethod)) {
		out = append(out, fmt.Sprintf("package db hands out a *sql.DB through %q, but the raw-database-handle rule guards only DB.%s: update it",
			slices.Sorted(maps.Keys(handles)), rawHandleMethod))
	}
	return out
}

func rawHandles(t *testing.T, root string) map[string]bool {
	t.Helper()
	files, err := moduleFiles(os.DirFS(filepath.Join(root, dbDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", dbDir, err)
	}
	handles := map[string]bool{}
	for _, f := range files {
		if f.test || f.dir != "." {
			continue
		}
		for _, decl := range f.file.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || !d.Name.IsExported() || d.Type.Results == nil {
				continue
			}
			for _, r := range d.Type.Results.List {
				if star, ok := r.Type.(*ast.StarExpr); ok && f.isType(star.X, "database/sql", "DB") {
					handles[receiverPrefix(d)+d.Name.Name] = true
				}
			}
		}
	}
	return handles
}

func TestRuleAnchors(t *testing.T) {
	if problems := anchorProblems(t, moduleRoot(t)); len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
	for _, tt := range []struct {
		name string
		api  string
		lis  string
		db   string
		want int
	}{
		{name: "the names the rules guard", api: "func (rt *router) register() {}\nfunc decided() {}\nfunc readTool() {}\nfunc newMCPEndpoint() {}\n", lis: "type Spec struct{ Handler http.Handler }\n", db: "func (d *DB) RawHandle() *sql.DB { return nil }\n"},
		{name: "renamed", api: "func (rt *router) add() {}\nfunc guard() {}\nfunc addTool() {}\nfunc (rt *router) readTool() {}\n", lis: "type Endpoint struct{ Handler http.Handler }\n", db: "func (d *DB) SQLHandle() *sql.DB { return nil }\n", want: 6},
		{name: "added beside them", api: "func (rt *router) register() {}\nfunc (rt *router) open() {}\nfunc decided() {}\nfunc readTool() {}\nfunc newMCPEndpoint() {}\n", lis: "type Spec struct{ Handler http.Handler }\ntype Wrapped struct{ Handler http.Handler }\n", db: "func (d *DB) RawHandle() *sql.DB { return nil }\nfunc (d *DB) Conn() *sql.DB { return nil }\n", want: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for dir, src := range map[string]string{
				apiDir:       "package api\n\ntype router struct{ mux, routes, now, credential int }\n\nfunc (rt *router) read()      {}\nfunc (rt *router) write()     {}\nfunc (rt *router) admin()     {}\nfunc (rt *router) mcp()       {}\nfunc (rt *router) ServeHTTP() {}\n" + tt.api,
				listenersDir: "package listeners\n\nimport \"net/http\"\n\n" + tt.lis,
				dbDir:        "package db\n\nimport \"database/sql\"\n\ntype DB struct{}\n\n" + tt.db,
			} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, dir, "x.go"), []byte(src), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if got := anchorProblems(t, root); len(got) != tt.want {
				t.Fatalf("%d problems, want %d: %q", len(got), tt.want, got)
			}
		})
	}
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
		{name: "digestcompare", fragment: "invalid operation: a == b (struct containing [0]func() cannot be compared)"},
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
