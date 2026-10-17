package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

var updateSurface = flag.Bool("update-v1-surface", false, "rewrite testdata/"+surfaceGolden+" from the source after review: a line that disappears or changes breaks the v1 compatibility promise")

const (
	surfaceGolden     = "v1-surface.txt"
	surfaceRegenerate = "go test ./internal/archtest -run TestV1SurfaceMatchesTheGolden -update-v1-surface"
	metricsDir        = "internal/metrics"
	mcpGoldenDir      = apiDir + "/testdata/mcp"
)

var surfaceFloors = map[string]int{
	"command": 16, "default": 1, "emf-metric": 8, "env": 31, "event": 16, "exit-code": 6, "flag": 56, "mcp-tool": 35, "metric": 21, "route": 23,
}

type constDecl struct {
	file *sourceFile
	expr ast.Expr
}

type surfaceScan struct {
	files    []*sourceFile
	consts   map[string]map[string]constDecl
	entries  map[string]bool
	problems map[string]bool
}

func newSurfaceScan(files []*sourceFile) *surfaceScan {
	s := &surfaceScan{consts: map[string]map[string]constDecl{}, entries: map[string]bool{}, problems: map[string]bool{}}
	for _, f := range files {
		if f.test || devOnly(f) {
			continue
		}
		s.files = append(s.files, f)
		eachConst(f, func(name *ast.Ident, value ast.Expr) {
			if s.consts[f.pkgPath()] == nil {
				s.consts[f.pkgPath()] = map[string]constDecl{}
			}
			s.consts[f.pkgPath()][name.Name] = constDecl{file: f, expr: value}
		})
	}
	return s
}

func eachConst(f *sourceFile, visit func(name *ast.Ident, value ast.Expr)) {
	for _, decl := range f.file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					visit(name, vs.Values[i])
				}
			}
		}
	}
}

func (s *surfaceScan) add(entry string) { s.entries[entry] = true }

func (s *surfaceScan) fail(f *sourceFile, n ast.Node, format string, args ...any) {
	s.problems[f.at(n, format, args...)] = true
}

func (s *surfaceScan) str(f *sourceFile, e ast.Expr) (string, bool) {
	return s.resolve(f, e, 0)
}

func (s *surfaceScan) resolve(f *sourceFile, e ast.Expr, depth int) (string, bool) {
	if e == nil || depth > 8 {
		return "", false
	}
	var b strings.Builder
	for _, operand := range addOperands(e) {
		if v, ok := stringLit(operand); ok {
			b.WriteString(v)
			continue
		}
		var d constDecl
		var found bool
		switch x := operand.(type) {
		case *ast.Ident:
			d, found = s.consts[f.pkgPath()][x.Name]
		case *ast.SelectorExpr:
			if sel, p := f.ref(x); sel != nil {
				d, found = s.consts[p][sel.Sel.Name]
			}
		}
		if !found {
			return "", false
		}
		v, ok := s.resolve(d.file, d.expr, depth+1)
		if !ok {
			return "", false
		}
		b.WriteString(v)
	}
	return b.String(), true
}

func (s *surfaceScan) scan(fsys fs.FS) {
	s.routes()
	s.health()
	s.mcpTools(fsys)
	s.envNames()
	s.cli()
	s.exitCodes()
	s.events()
	s.metrics()
	s.emfMetrics()
}

func surfaceList(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(slices.Sorted(slices.Values(items)), ",")
}

func identNamed(e ast.Expr, name string) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && id.Name == name
}

func (s *surfaceScan) routes() {
	classes := map[string]string{}
	for _, f := range s.files {
		if f.dir != apiDir {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !identNamed(lit.Type, "route") {
					return true
				}
				if value := keyedValue(lit, "class"); value != nil {
					if c, ok := s.str(f, value); ok {
						classes[fn.Name.Name] = c
					} else {
						s.fail(f, value, "a route's class that is not a constant")
					}
				}
				return true
			})
		}
	}
	if len(classes) == 0 {
		s.problems[apiDir+": no method builds a route with a class, so no route registration can be found"] = true
		return
	}
	for _, f := range s.files {
		if f.dir != apiDir {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			c, ok := classes[sel.Sel.Name]
			if !ok || len(call.Args) != 2 {
				return true
			}
			if pattern, ok := s.str(f, call.Args[0]); ok {
				s.add(fmt.Sprintf("route %s class=%s", pattern, c))
			} else {
				s.fail(f, call, "a %s route registered with a pattern that is not a constant", c)
			}
			return true
		})
	}
}

func (s *surfaceScan) health() {
	for _, f := range s.files {
		if f.dir != apiDir {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "NewHealthHandler" || fn.Body == nil {
				continue
			}
			var paths, methods []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				b, ok := n.(*ast.BinaryExpr)
				if !ok || (b.Op != token.EQL && b.Op != token.NEQ) {
					return true
				}
				for _, side := range [][2]ast.Expr{{b.X, b.Y}, {b.Y, b.X}} {
					field, ok := ast.Unparen(side[0]).(*ast.SelectorExpr)
					if !ok {
						continue
					}
					switch field.Sel.Name {
					case "Path":
						if p, ok := s.str(f, side[1]); ok {
							paths = append(paths, p)
						}
					case "Method":
						if sel, p := f.ref(side[1]); sel != nil && p == "net/http" && strings.HasPrefix(sel.Sel.Name, "Method") {
							methods = append(methods, strings.ToUpper(strings.TrimPrefix(sel.Sel.Name, "Method")))
						}
					}
				}
				return true
			})
			if len(paths) != 1 || len(methods) != 1 {
				s.fail(f, fn.Name, "NewHealthHandler compares the request with %d paths and %d methods, want one of each", len(paths), len(methods))
				return
			}
			s.add(fmt.Sprintf("route %s %s class=health", methods[0], paths[0]))
			return
		}
	}
	s.problems[apiDir+": no NewHealthHandler, so the health route cannot be read"] = true
}

func (s *surfaceScan) mcpTools(fsys fs.FS) {
	names, err := fs.Glob(fsys, mcpGoldenDir+"/tools_*.json")
	if err != nil || len(names) == 0 {
		s.problems[fmt.Sprintf("%s holds no tools_*.json golden file (%v)", mcpGoldenDir, err)] = true
		return
	}
	for _, name := range names {
		class := strings.TrimSuffix(strings.TrimPrefix(path.Base(name), "tools_"), ".json")
		var tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"inputSchema"`
		}
		data, err := fs.ReadFile(fsys, name)
		if err == nil {
			err = json.Unmarshal(data, &tools)
		}
		if err != nil || len(tools) == 0 {
			s.problems[fmt.Sprintf("%s: no tool list can be read (%v)", name, err)] = true
			continue
		}
		for _, tool := range tools {
			if tool.Name == "" {
				s.problems[name+": a tool without a name"] = true
				continue
			}
			s.add(fmt.Sprintf("mcp-tool %s %s properties=%s required=%s", class, tool.Name,
				surfaceList(slices.Collect(maps.Keys(tool.InputSchema.Properties))), surfaceList(tool.InputSchema.Required)))
		}
	}
}

func (s *surfaceScan) envNames() {
	found := false
	list := func(f *sourceFile, elts []ast.Expr) {
		for _, e := range elts {
			if v, ok := s.str(f, e); ok {
				s.add("env " + v)
			} else {
				s.fail(f, e, "an accepted variable whose name is not a constant")
			}
		}
	}
	for _, f := range s.files {
		if f.dir != configDir {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if name.Name != "known" || i >= len(x.Values) {
						continue
					}
					found = true
					if lit, ok := x.Values[i].(*ast.CompositeLit); ok {
						list(f, lit.Elts)
					} else {
						s.fail(f, name, "known is not a list literal")
					}
				}
			case *ast.CallExpr:
				if identNamed(x.Fun, "append") && len(x.Args) > 0 && identNamed(x.Args[0], "known") {
					if x.Ellipsis.IsValid() {
						s.fail(f, x, "known grows by a list the scan cannot read")
					} else {
						list(f, x.Args[1:])
					}
				}
			}
			return true
		})
	}
	if !found {
		s.problems[configDir+": no list named known, so the accepted variables cannot be read"] = true
	}
}

func (s *surfaceScan) exitCodes() {
	for _, f := range s.files {
		if f.dir != cmdDir {
			continue
		}
		eachConst(f, func(name *ast.Ident, value ast.Expr) {
			if !strings.HasPrefix(name.Name, "exit") {
				return
			}
			lit, ok := ast.Unparen(value).(*ast.BasicLit)
			code, err := 0, errors.New("not an integer literal")
			if ok && lit.Kind == token.INT {
				code, err = strconv.Atoi(lit.Value)
			}
			if err != nil {
				s.fail(f, name, "the exit code %s: %v", name.Name, err)
				return
			}
			s.add(fmt.Sprintf("exit-code %d %s", code, name.Name))
		})
	}
}

func (s *surfaceScan) events() {
	for _, f := range s.files {
		if f.dir != notifyDir {
			continue
		}
		eachConst(f, func(name *ast.Ident, value ast.Expr) {
			if !strings.HasPrefix(name.Name, "Event") {
				return
			}
			if v, ok := s.str(f, value); ok {
				s.add("event " + v)
			} else {
				s.fail(f, name, "the event %s is not a string constant", name.Name)
			}
		})
	}
}

var registryKinds = map[string]string{"Counter": "counter", "CounterVec": "counter", "Gauge": "gauge", "GaugeVec": "gauge"}

func (s *surfaceScan) metrics() {
	for _, f := range s.files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			kind, ok := registryKinds[sel.Sel.Name]
			vec := strings.HasSuffix(sel.Sel.Name, "Vec")
			if !ok || len(call.Args) < 2 || (!vec && len(call.Args) != 2) {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && f.names[x.Name] != "" {
				return true
			}
			name, ok := s.str(f, call.Args[0])
			var labels []string
			for _, l := range call.Args[2:] {
				v, isConst := s.str(f, l)
				ok = ok && isConst
				labels = append(labels, v)
			}
			if !ok || call.Ellipsis.IsValid() {
				s.fail(f, call, "a metric registered with a name or labels that are not constants")
				return true
			}
			entry := fmt.Sprintf("metric %s %s", name, kind)
			if vec {
				entry += " labels=" + surfaceList(labels)
			}
			s.add(entry)
			return true
		})
	}
}

func (s *surfaceScan) emfMetrics() {
	var namespace string
	var names []string
	for _, f := range s.files {
		if f.dir != metricsDir {
			continue
		}
		eachConst(f, func(name *ast.Ident, value ast.Expr) {
			if name.Name == "EMFNamespace" {
				namespace, _ = s.str(f, value)
			}
		})
		f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
			if !identNamed(typ, "emfMetric") {
				return
			}
			if v, ok := s.str(f, keyedValue(lit, "name")); ok {
				names = append(names, v)
			} else {
				s.fail(f, lit, "an EMF metric whose name is not a keyed constant")
			}
		})
	}
	if namespace == "" || len(names) == 0 {
		s.problems[metricsDir+": no EMFNamespace constant or no emfMetric literal, so the EMF metric names cannot be read"] = true
		return
	}
	for _, name := range names {
		s.add("emf-metric " + namespace + "/" + name)
	}
}

type flagMethod struct {
	kind       string
	args, name int
}

var flagMethods = map[string]flagMethod{
	"String": {"string", 3, 0}, "StringVar": {"string", 4, 1},
	"Bool": {"bool", 3, 0}, "BoolVar": {"bool", 4, 1}, "BoolFunc": {"bool", 3, 0},
	"Int": {"int", 3, 0}, "IntVar": {"int", 4, 1}, "Int64": {"int64", 3, 0}, "Int64Var": {"int64", 4, 1},
	"Uint": {"uint", 3, 0}, "UintVar": {"uint", 4, 1}, "Uint64": {"uint64", 3, 0}, "Uint64Var": {"uint64", 4, 1},
	"Float64": {"float64", 3, 0}, "Float64Var": {"float64", 4, 1}, "Duration": {"duration", 3, 0}, "DurationVar": {"duration", 4, 1},
	"Func": {"value", 3, 0}, "Var": {"value", 3, 1}, "TextVar": {"value", 4, 1},
}

func flagDefinition(f *sourceFile, call *ast.CallExpr) (flagMethod, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return flagMethod{}, false
	}
	m, ok := flagMethods[sel.Sel.Name]
	if !ok || len(call.Args) != m.args {
		return flagMethod{}, false
	}
	if x, ok := sel.X.(*ast.Ident); ok {
		if p, imported := f.names[x.Name]; imported && p != "flag" {
			return flagMethod{}, false
		}
	}
	return m, true
}

func flagSetConstructor(f *sourceFile, call *ast.CallExpr) bool {
	sel, p := f.ref(call.Fun)
	return sel != nil && p == "flag" && sel.Sel.Name == "NewFlagSet"
}

func firstArg(e ast.Expr, aliases map[string]bool) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.IndexExpr:
		lit, ok := x.Index.(*ast.BasicLit)
		return identNamed(x.X, "args") && ok && lit.Kind == token.INT && lit.Value == "0"
	case *ast.Ident:
		return aliases[x.Name]
	}
	return false
}

func argAliases(body *ast.BlockStmt) map[string]bool {
	aliases := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			if id, ok := as.Lhs[i].(*ast.Ident); ok && firstArg(rhs, nil) {
				aliases[id.Name] = true
			}
		}
		return true
	})
	return aliases
}

func (s *surfaceScan) guard(f *sourceFile, cond ast.Expr, aliases map[string]bool) (string, bool) {
	var word string
	var found bool
	ast.Inspect(cond, func(n ast.Node) bool {
		b, ok := n.(*ast.BinaryExpr)
		if !ok || b.Op != token.NEQ {
			return true
		}
		for _, side := range [][2]ast.Expr{{b.X, b.Y}, {b.Y, b.X}} {
			if v, ok := s.str(f, side[1]); ok && firstArg(side[0], aliases) {
				word, found = v, true
			}
		}
		return true
	})
	return word, found
}

type cliFunc struct {
	file *sourceFile
	decl *ast.FuncDecl
}

type cliScan struct {
	*surfaceScan
	funcs    map[string]cliFunc
	children map[string]map[string]bool
	flags    map[string]map[string]bool
	defaults map[string]string
	reached  map[ast.Node]bool
	walked   map[string]bool
}

type cliWalk struct {
	*cliScan
	fn            cliFunc
	aliases       map[string]bool
	targets       map[string]string
	clauseReturns map[*ast.ReturnStmt]bool
}

func (s *surfaceScan) cli() {
	c := &cliScan{surfaceScan: s, funcs: map[string]cliFunc{}, children: map[string]map[string]bool{}, flags: map[string]map[string]bool{},
		defaults: map[string]string{}, reached: map[ast.Node]bool{}, walked: map[string]bool{}}
	for _, f := range s.files {
		if f.dir != cmdDir {
			continue
		}
		for _, decl := range f.file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
				c.funcs[fn.Name.Name] = cliFunc{file: f, decl: fn}
			}
		}
	}
	entry, ok := c.funcs["main"]
	if !ok {
		s.problems[cmdDir+": no func main, so the subcommands cannot be read"] = true
		return
	}
	root := path.Base(cmdDir)
	c.walk(entry, root)
	c.unreached()
	c.record(root)
}

func (c *cliScan) walk(fn cliFunc, at string) {
	key := fn.decl.Name.Name + " " + at
	if c.walked[key] {
		return
	}
	c.walked[key] = true
	w := &cliWalk{cliScan: c, fn: fn, aliases: argAliases(fn.decl.Body), targets: map[string]string{}, clauseReturns: map[*ast.ReturnStmt]bool{}}
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || !firstArg(sw.Tag, w.aliases) {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc := stmt.(*ast.CaseClause)
			ast.Inspect(cc, func(m ast.Node) bool {
				if ret, ok := m.(*ast.ReturnStmt); ok {
					w.clauseReturns[ret] = true
				}
				return true
			})
			if len(cc.List) == 0 || len(cc.Body) == 0 {
				continue
			}
			ret, ok := cc.Body[len(cc.Body)-1].(*ast.ReturnStmt)
			if callee, isCall := w.callee(ret); ok && isCall {
				word, _ := c.str(fn.file, cc.List[0])
				if len(cc.List) > 1 {
					word = ""
				}
				w.targets[callee] = word
			}
		}
		return true
	})
	w.block(fn.decl.Body.List, at)
}

func (w *cliWalk) callee(ret *ast.ReturnStmt) (string, bool) {
	if ret == nil || len(ret.Results) != 1 {
		return "", false
	}
	call, ok := ast.Unparen(ret.Results[0]).(*ast.CallExpr)
	if !ok {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	_, ok = w.funcs[id.Name]
	return id.Name, ok
}

func (w *cliWalk) block(stmts []ast.Stmt, at string) {
	for _, stmt := range stmts {
		switch x := stmt.(type) {
		case *ast.BlockStmt:
			w.block(x.List, at)
		case *ast.IfStmt:
			if word, ok := w.guard(w.fn.file, x.Cond, w.aliases); ok {
				w.reached[x] = true
				at = w.child(at, word)
				continue
			}
			w.node(x.Init, at)
			w.node(x.Cond, at)
			w.block(x.Body.List, at)
			if x.Else != nil {
				w.block([]ast.Stmt{x.Else}, at)
			}
		case *ast.SwitchStmt:
			if firstArg(x.Tag, w.aliases) {
				w.dispatch(x, at)
				continue
			}
			w.node(x.Init, at)
			w.node(x.Tag, at)
			for _, stmt := range x.Body.List {
				cc := stmt.(*ast.CaseClause)
				for _, e := range cc.List {
					w.node(e, at)
				}
				w.block(cc.Body, at)
			}
		case *ast.ReturnStmt:
			if callee, ok := w.callee(x); ok && !w.clauseReturns[x] {
				if word, isTarget := w.targets[callee]; isTarget {
					w.setDefault(x, at, word)
					continue
				}
			}
			w.node(x, at)
		default:
			w.node(x, at)
		}
	}
}

func (w *cliWalk) dispatch(sw *ast.SwitchStmt, at string) {
	w.reached[sw] = true
	w.node(sw.Init, at)
	for _, stmt := range sw.Body.List {
		cc := stmt.(*ast.CaseClause)
		if len(cc.List) == 0 {
			w.block(cc.Body, at)
			continue
		}
		for _, e := range cc.List {
			if word, ok := w.str(w.fn.file, e); ok {
				w.block(cc.Body, w.child(at, word))
			} else {
				w.fail(w.fn.file, e, "a subcommand that is not a constant")
			}
		}
	}
}

func (w *cliWalk) setDefault(ret *ast.ReturnStmt, at, word string) {
	if prior, ok := w.defaults[at]; word == "" || (ok && prior != word) {
		w.fail(w.fn.file, ret, "%s falls back to a command the scan cannot name", at)
		return
	}
	w.defaults[at] = word
}

func (w *cliWalk) child(at, word string) string {
	if w.children[at] == nil {
		w.children[at] = map[string]bool{}
	}
	w.children[at][word] = true
	return at + " " + word
}

func (w *cliWalk) node(n ast.Node, at string) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		if flagSetConstructor(w.fn.file, call) {
			w.reached[call] = true
		}
		if m, ok := flagDefinition(w.fn.file, call); ok {
			w.reached[call] = true
			if name, ok := w.str(w.fn.file, call.Args[m.name]); ok {
				if w.flags[at] == nil {
					w.flags[at] = map[string]bool{}
				}
				w.flags[at]["--"+name+" "+m.kind] = true
			} else {
				w.fail(w.fn.file, call, "a flag whose name is not a constant")
			}
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			if fn, ok := w.funcs[id.Name]; ok {
				w.walk(fn, at)
			}
		}
		return true
	})
}

func (c *cliScan) unreached() {
	for _, f := range c.files {
		if f.dir != cmdDir {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && !c.reached[call] {
				if _, isFlag := flagDefinition(f, call); isFlag || flagSetConstructor(f, call) {
					c.fail(f, call, "a flag or flag set that the scan does not reach from main through the subcommand dispatch")
				}
			}
			return true
		})
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			aliases := argAliases(fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SwitchStmt:
					if firstArg(x.Tag, aliases) && !c.reached[x] {
						c.fail(f, x, "a subcommand dispatch that the scan does not reach from main")
					}
				case *ast.IfStmt:
					if _, ok := c.guard(f, x.Cond, aliases); ok && !c.reached[x] {
						c.fail(f, x, "a subcommand check that the scan does not reach from main")
					}
				}
				return true
			})
		}
	}
}

func (c *cliScan) record(root string) {
	paths := []string{root}
	for i := 0; i < len(paths); i++ {
		for word := range c.children[paths[i]] {
			paths = append(paths, paths[i]+" "+word)
		}
	}
	for _, p := range paths {
		if len(c.children[p]) > 0 {
			continue
		}
		c.add("command " + p)
		for at := p; ; {
			for fl := range c.flags[at] {
				c.add("flag " + p + " " + fl)
			}
			i := strings.LastIndex(at, " ")
			if i < 0 {
				break
			}
			at = at[:i]
		}
	}
	for at, word := range c.defaults {
		if !c.children[at][word] {
			c.problems[fmt.Sprintf("%s falls back to %q, which is not one of its subcommands", at, word)] = true
			continue
		}
		c.add("default " + at + " " + word)
	}
}

func checkSurfaceGolden(file string, got []byte, update bool) error {
	if update {
		if err := os.WriteFile(file, got, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", file, err)
		}
	}
	want, err := fs.ReadFile(os.DirFS(filepath.Dir(file)), filepath.Base(file))
	if err != nil {
		return fmt.Errorf("read %s: %w (create it with %s)", file, err, surfaceRegenerate)
	}
	if bytes.Equal(got, want) {
		return nil
	}
	gotLines, wantLines := set(strings.Split(string(got), "\n")...), set(strings.Split(string(want), "\n")...)
	var b strings.Builder
	b.WriteString("the v1 surface differs from " + file + ":")
	for _, l := range slices.Sorted(maps.Keys(wantLines)) {
		if !gotLines[l] {
			b.WriteString("\n- " + l)
		}
	}
	for _, l := range slices.Sorted(maps.Keys(gotLines)) {
		if !wantLines[l] {
			b.WriteString("\n+ " + l)
		}
	}
	if maps.Equal(gotLines, wantLines) {
		b.WriteString("\nthe file holds the same lines out of order, twice or without the final newline")
	}
	b.WriteString("\na line that disappears or changes breaks the v1 compatibility promise; after review, regenerate the file with " + surfaceRegenerate)
	return errors.New(b.String())
}

func TestV1SurfaceMatchesTheGolden(t *testing.T) {
	root := moduleRoot(t)
	fsys := os.DirFS(root)
	files, err := moduleFiles(fsys)
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	s := newSurfaceScan(files)
	s.scan(fsys)
	for _, p := range slices.Sorted(maps.Keys(s.problems)) {
		t.Error(p)
	}
	entries := slices.Sorted(maps.Keys(s.entries))
	if err := surfaceVerdict(entries, surfaceFloors, filepath.Join("testdata", surfaceGolden), *updateSurface && !t.Failed()); err != nil {
		t.Fatal(err)
	}
}

func surfaceVerdict(entries []string, floors map[string]int, file string, update bool) error {
	counts := map[string]int{}
	for _, e := range entries {
		section, _, _ := strings.Cut(e, " ")
		counts[section]++
	}
	var errs []error
	for _, section := range slices.Sorted(maps.Keys(floors)) {
		if counts[section] < floors[section] {
			errs = append(errs, fmt.Errorf("the scan found %d %s entries, fewer than the %d of the v1 surface: removing one breaks the v1 compatibility promise, and a scan that misses entries would let a regeneration drop them unnoticed", counts[section], section, floors[section]))
		}
	}
	for _, section := range slices.Sorted(maps.Keys(counts)) {
		if _, ok := floors[section]; !ok {
			errs = append(errs, fmt.Errorf("the section %q has no floor in surfaceFloors", section))
		}
	}
	got := []byte(strings.Join(entries, "\n") + "\n")
	if len(errs) > 0 {
		return errors.Join(append(errs, checkSurfaceGolden(file, got, false))...)
	}
	return checkSurfaceGolden(file, got, update)
}

func scanSnippets(t *testing.T, srcs map[string]string) *surfaceScan {
	t.Helper()
	var files []*sourceFile
	for _, rel := range slices.Sorted(maps.Keys(srcs)) {
		f, err := parseSource(rel, []byte(srcs[rel]))
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		files = append(files, f)
	}
	return newSurfaceScan(files)
}

func TestSurfaceScans(t *testing.T) {
	routes := func(s *surfaceScan) { s.routes(); s.health() }
	for _, c := range []struct {
		name     string
		srcs     map[string]string
		scan     func(*surfaceScan)
		want     []string
		problems int
	}{
		{name: "routes, their classes and the health route", scan: routes, problems: 1, srcs: map[string]string{
			apiDir + "/register.go": `package api

import "net/http"

type class string

const (
	classRead  class = "read"
	classAdmin class = "admin"
	mcpPattern       = "POST /mcp"
	prefix           = "/v1"
)

type route struct {
	pattern string
	class   class
}

type router struct{ routes []route }

func (rt *router) register(r route) { rt.routes = append(rt.routes, r) }

func (rt *router) read(pattern string, h func()) { rt.register(route{pattern: pattern, class: classRead}) }

func (rt *router) admin(pattern string, h func()) { rt.register(route{pattern: pattern, class: classAdmin}) }

func registerAll(rt *router, dynamic string) {
	rt.read("GET "+prefix+"/me", nil)
	rt.admin(mcpPattern, nil)
	rt.read(dynamic, nil)
	rt.register(route{pattern: "GET /unclassed"})
}

func NewHealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/healthz":
		case r.Method != http.MethodGet:
		}
	})
}
`,
			apiDir + "/register_test.go": `package api

func registerMore(rt *router) { rt.read("GET /test-only", nil) }
`,
		}, want: []string{"route GET /healthz class=health", "route GET /v1/me class=read", "route POST /mcp class=admin"}},
		{name: "no route helper and no health handler", scan: routes, problems: 2, srcs: map[string]string{
			apiDir + "/api.go": "package api\n\nfunc f() {}\n",
		}},
		{name: "a health handler with two paths", scan: routes, problems: 2, srcs: map[string]string{
			apiDir + "/api.go": `package api

import "net/http"

func NewHealthHandler(r *http.Request) bool {
	return r.URL.Path == "/healthz" || r.URL.Path == "/livez" || r.Method == http.MethodGet
}
`,
		}},
		{name: "accepted variables in release files", scan: (*surfaceScan).envNames, problems: 1, srcs: map[string]string{
			configDir + "/config.go": `package config

import "os"

const (
	envA = "WAWARDEN_A"
	envC = prefix + "C"
	prefix = "WAWARDEN_"
)

var known = []string{envA, "WAWARDEN_B"}

func init() {
	known = append(known, envC)
	known = append(known, os.Getenv("NAME"))
}
`,
			configDir + "/dev.go": `//go:build dev

package config

func init() { known = append(known, "WAWARDEN_DEV_X") }
`,
			configDir + "/config_test.go": `package config

func init() { known = append(known, "WAWARDEN_TEST_X") }
`,
		}, want: []string{"env WAWARDEN_A", "env WAWARDEN_B", "env WAWARDEN_C"}},
		{name: "no list of accepted variables", scan: (*surfaceScan).envNames, problems: 1, srcs: map[string]string{
			configDir + "/config.go": "package config\n\nvar names = []string{\"WAWARDEN_A\"}\n",
		}},
		{name: "subcommands, their flags and the default", scan: (*surfaceScan).cli, srcs: map[string]string{
			keysDir + "/keys.go": "package keys\n\nconst Subcommand = \"keys\"\n",
			cmdDir + "/main.go": `package main

import (
	"flag"
	"os"
	"strings"

	"github.com/dortort/wawarden/internal/keys"
)

const (
	flagToken = "token"
	commandUp = "up"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return serve(args)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "admin":
		return admin(args[1:])
	case keys.Subcommand:
		return rotate(args[1:])
	}
	return usageError()
}

func usageError() int { return 2 }

func serve(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.Bool("allow-root", false, "")
	return flags.NArg()
}

func shared(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.String(flagToken, "", "")
	return flags
}

func admin(args []string) int {
	if len(args) == 0 {
		return usageError()
	}
	command, rest := args[0], args[1:]
	flags := shared("admin " + command)
	switch command {
	case commandUp, "down":
		flags.Int("count", 0, "")
	case "list":
	default:
		return usageError()
	}
	return len(rest)
}

func rotate(args []string) int {
	if len(args) == 0 || args[0] != "now" {
		return usageError()
	}
	var days int
	flags := flag.NewFlagSet("keys now", flag.ContinueOnError)
	flags.IntVar(&days, "days", 0, "")
	return days
}
`,
		}, want: []string{
			"command wawarden admin down", "command wawarden admin list", "command wawarden admin up", "command wawarden keys now", "command wawarden serve",
			"default wawarden serve",
			"flag wawarden admin down --count int", "flag wawarden admin down --token string", "flag wawarden admin list --token string",
			"flag wawarden admin up --count int", "flag wawarden admin up --token string", "flag wawarden keys now --days int", "flag wawarden serve --allow-root bool",
		}},
		{name: "flags and subcommands the scan cannot place", scan: (*surfaceScan).cli, problems: 5, srcs: map[string]string{
			cmdDir + "/main.go": `package main

import (
	"flag"
	"os"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	flags := flag.NewFlagSet("wawarden", flag.ContinueOnError)
	name := os.Args[0]
	flags.String(name, "", "")
	return len(args)
}

func orphan(args []string) int {
	if args[0] != "x" {
		return 2
	}
	switch args[0] {
	case "y":
		flags := flag.NewFlagSet("y", flag.ContinueOnError)
		flags.Bool("z", false, "")
	}
	return 0
}
`,
		}, want: []string{"command wawarden"}},
		{name: "a default the scan cannot name", scan: (*surfaceScan).cli, problems: 1, srcs: map[string]string{
			cmdDir + "/main.go": `package main

import "os"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		return both(args)
	}
	switch args[0] {
	case "a", "b":
		return both(args[1:])
	}
	return 2
}

func both(args []string) int { return len(args) }
`,
		}, want: []string{"command wawarden a", "command wawarden b"}},
		{name: "no main", scan: (*surfaceScan).cli, problems: 1, srcs: map[string]string{
			cmdDir + "/run.go": "package main\n\nfunc run() {}\n",
		}},
		{name: "exit codes", scan: (*surfaceScan).exitCodes, problems: 1, srcs: map[string]string{
			cmdDir + "/exit.go": `package main

const (
	exitFailed = 1
	exitUsage  = 2
	exitOdd    = "x"
	other      = 3
)
`,
			cmdDir + "/exit_test.go": "package main\n\nconst exitTest = 9\n",
		}, want: []string{"exit-code 1 exitFailed", "exit-code 2 exitUsage"}},
		{name: "notification events", scan: (*surfaceScan).events, problems: 1, srcs: map[string]string{
			notifyDir + "/notify.go": `package notify

const (
	EventA   = "a"
	EventB   = prefix + "b"
	EventBad = len("x")
	prefix   = "p_"
	other    = "c"
)
`,
		}, want: []string{"event a", "event p_b"}},
		{name: "metric families", scan: (*surfaceScan).metrics, problems: 2, srcs: map[string]string{
			"internal/app/app.go": `package app

import "github.com/dortort/wawarden/internal/metrics"

const label = "reason"

func f(reg *metrics.Registry, name string, labels []string) {
	reg.Counter("wawarden_a_total", "help")
	reg.GaugeVec("wawarden_b", "help", "y", "x")
	reg.CounterVec("wawarden_c_total", "help", label)
	reg.Counter(name, "help")
	reg.GaugeVec("wawarden_d", "help", labels...)
}
`,
			"internal/app/app_test.go": `package app

import "github.com/dortort/wawarden/internal/metrics"

func g(reg *metrics.Registry) { reg.Counter("wawarden_test_total", "help") }
`,
		}, want: []string{"metric wawarden_a_total counter", "metric wawarden_b gauge labels=x,y", "metric wawarden_c_total counter labels=reason"}},
		{name: "EMF metric names", scan: (*surfaceScan).emfMetrics, problems: 1, srcs: map[string]string{
			metricsDir + "/emf.go": `package metrics

const EMFNamespace = "WaWarden"

type emfMetric struct{ name, family string }

var emfMetrics = []emfMetric{
	{name: "Paired", family: "wawarden_paired"},
	{"Positional", "wawarden_positional"},
}
`,
		}, want: []string{"emf-metric WaWarden/Paired"}},
		{name: "no EMF namespace", scan: (*surfaceScan).emfMetrics, problems: 1, srcs: map[string]string{
			metricsDir + "/emf.go": "package metrics\n\ntype emfMetric struct{ name string }\n\nvar emfMetrics = []emfMetric{{name: \"Paired\"}}\n",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := scanSnippets(t, c.srcs)
			c.scan(s)
			if got := slices.Sorted(maps.Keys(s.entries)); !slices.Equal(got, c.want) {
				t.Errorf("entries %q, want %q", got, c.want)
			}
			if got := slices.Sorted(maps.Keys(s.problems)); len(got) != c.problems {
				t.Errorf("%d problems, want %d: %q", len(got), c.problems, got)
			}
		})
	}
}

func TestSurfaceMCPTools(t *testing.T) {
	s := newSurfaceScan(nil)
	s.mcpTools(fstest.MapFS{
		mcpGoldenDir + "/tools_some_class.json": {Data: []byte(`[{"name":"b_tool","inputSchema":{"type":"object","properties":{"z":{},"a":{}},"required":["z","a"]}},{"name":"a_tool","inputSchema":{"type":"object"}}]`)},
		mcpGoldenDir + "/other.json":            {Data: []byte(`[{"name":"ignored"}]`)},
	})
	want := []string{"mcp-tool some_class a_tool properties=- required=-", "mcp-tool some_class b_tool properties=a,z required=a,z"}
	if got := slices.Sorted(maps.Keys(s.entries)); !slices.Equal(got, want) || len(s.problems) > 0 {
		t.Fatalf("entries %q and problems %q, want %q and none", got, slices.Sorted(maps.Keys(s.problems)), want)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"no golden file":      {},
		"not a tool list":     {mcpGoldenDir + "/tools_x.json": {Data: []byte(`{}`)}},
		"an empty tool list":  {mcpGoldenDir + "/tools_x.json": {Data: []byte(`[]`)}},
		"a tool without name": {mcpGoldenDir + "/tools_x.json": {Data: []byte(`[{"inputSchema":{}}]`)}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSurfaceScan(nil)
			s.mcpTools(fsys)
			if len(s.entries) > 0 || len(s.problems) != 1 {
				t.Fatalf("entries %q and problems %q, want none and one", slices.Sorted(maps.Keys(s.entries)), slices.Sorted(maps.Keys(s.problems)))
			}
		})
	}
}

func TestSurfaceGoldenComparison(t *testing.T) {
	golden := "route GET /a class=read\nroute GET /b class=read\n"
	for _, c := range []struct {
		name, got string
		fragments []string
	}{
		{name: "equal", got: golden},
		{name: "an entry disappears", got: "route GET /a class=read\n", fragments: []string{"\n- route GET /b class=read"}},
		{name: "an entry changes", got: "route GET /a class=read\nroute GET /b class=write\n", fragments: []string{"\n- route GET /b class=read", "\n+ route GET /b class=write"}},
		{name: "an entry is added", got: golden + "route GET /c class=read\n", fragments: []string{"\n+ route GET /c class=read"}},
		{name: "the same entries out of order", got: "route GET /b class=read\nroute GET /a class=read\n", fragments: []string{"out of order"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), surfaceGolden)
			if err := os.WriteFile(file, []byte(golden), 0o600); err != nil {
				t.Fatal(err)
			}
			err := checkSurfaceGolden(file, []byte(c.got), false)
			if c.fragments == nil {
				if err != nil {
					t.Fatalf("an equal surface failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a different surface passed")
			}
			for _, fragment := range append(c.fragments, surfaceRegenerate) {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("%q lacks %q", err, fragment)
				}
			}
		})
	}
	t.Run("regenerated", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), surfaceGolden)
		for _, got := range [][]byte{[]byte(golden), []byte(golden + "route GET /c class=read\n")} {
			if err := checkSurfaceGolden(file, got, true); err != nil {
				t.Fatalf("regeneration failed: %v", err)
			}
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("the regenerated file: %v, %v, want mode 0600", info, err)
			}
			if data, err := fs.ReadFile(os.DirFS(filepath.Dir(file)), surfaceGolden); err != nil || !bytes.Equal(data, got) {
				t.Fatalf("the regenerated file holds %q (%v), want %q", data, err, got)
			}
			if err := checkSurfaceGolden(file, got, false); err != nil {
				t.Fatalf("the regenerated surface failed: %v", err)
			}
		}
	})
	t.Run("below a floor", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), surfaceGolden)
		if err := os.WriteFile(file, []byte(golden), 0o600); err != nil {
			t.Fatal(err)
		}
		err := surfaceVerdict([]string{"route GET /a class=read"}, map[string]int{"route": 2}, file, true)
		if err == nil || !strings.Contains(err.Error(), "the scan found 1 route entries, fewer than the 2") {
			t.Fatalf("a scan below its floor gave %v", err)
		}
		if data, err := fs.ReadFile(os.DirFS(filepath.Dir(file)), surfaceGolden); err != nil || string(data) != golden {
			t.Fatalf("a scan below its floor left %q (%v), want the golden untouched", data, err)
		}
		if err := surfaceVerdict([]string{"route GET /a class=read", "route GET /c class=read"}, map[string]int{"route": 2}, file, true); err != nil {
			t.Fatalf("a scan at its floor was not regenerated: %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		err := checkSurfaceGolden(filepath.Join(t.TempDir(), surfaceGolden), []byte(golden), false)
		if err == nil || !strings.Contains(err.Error(), surfaceRegenerate) {
			t.Fatalf("a missing golden file gave %v", err)
		}
	})
}
