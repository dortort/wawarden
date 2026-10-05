package archtest

import "go/ast"

const execFile = "cmd/wawarden/tokencommand.go"

var processStarters = map[string]map[string]bool{
	"os":      set("StartProcess"),
	"syscall": set("Exec", "ForkExec", "StartProcess"),
}

var execRule = rule{
	name:  "process-execution",
	check: checkExec,
	cases: []snippet{
		{name: "programs started outside the token command file", rel: "cmd/wawarden/run.go", want: 5, src: `package main

import (
	"os"
	run "os/exec"
	"syscall"
)

func f() {
	_ = run.Command("true").Run()
	_, _ = os.StartProcess("/bin/true", nil, nil)
	_ = syscall.Exec("/bin/true", nil, nil)
	_, _ = syscall.ForkExec("/bin/true", nil, nil)
	_, _, _ = syscall.StartProcess("/bin/true", nil, nil)
}
`},
		{name: "os/exec in an internal package", rel: "internal/app/x.go", want: 1, src: `package app

import _ "os/exec"
`},
		{name: "the token command file", rel: execFile, src: `package main

import (
	"os/exec"
	"syscall"
)

func f() { _ = exec.Command("true"); _ = syscall.Kill(0, 0) }
`},
		{name: "a test", rel: "internal/archtest/x_test.go", src: `package archtest

import "os/exec"

var _ = exec.Command
`},
	},
}

func checkExec(f *sourceFile) []string {
	if f.test || f.rel == execFile {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if imp.path == "os/exec" {
			out = append(out, f.at(imp.node, "only %s may run another program: the admin CLI's token command", execFile))
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if s, p := f.ref(sel); s != nil && processStarters[p][s.Sel.Name] {
				out = append(out, f.at(sel, "%s.%s starts a program outside %s", p, s.Sel.Name, execFile))
			}
		}
		return true
	})
	return out
}
