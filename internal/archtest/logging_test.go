package archtest

import "go/ast"

const logxDir = "internal/logx"

var slogHandlerConstructors = set("NewJSONHandler", "NewTextHandler")

var logHandlerRule = rule{
	name:  "log-handlers",
	check: checkLogHandlers,
	cases: []snippet{
		{name: "handlers built outside internal/logx", rel: "internal/app/x.go", want: 4, src: `package app

import (
	"io"
	"log/slog"
	s "log/slog"
)

func f(w io.Writer) []*slog.Logger {
	build := slog.NewJSONHandler
	return []*slog.Logger{
		slog.New(slog.NewJSONHandler(w, nil)),
		slog.New(s.NewTextHandler(w, &slog.HandlerOptions{})),
		slog.New(build(w, nil)),
		slog.New(slog.NewTextHandler(w, nil)),
	}
}
`},
		{name: "handlers built in internal/logx", rel: "internal/logx/x.go", src: `package logx

import (
	"io"
	"log/slog"
)

func New(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }
`},
		{name: "handlers built in a test", rel: "internal/api/x_test.go", src: `package api

import (
	"io"
	"log/slog"
)

func f(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }
`},
		{name: "loggers that wrap an existing handler, and another package's constructors", rel: "internal/listeners/x.go", src: `package listeners

import (
	"log"
	"log/slog"

	other "example.com/slog"
)

func f(logger *slog.Logger) *log.Logger {
	_ = other.NewJSONHandler(nil, nil)
	return slog.NewLogLogger(logger.With("k", "v").Handler(), slog.LevelWarn)
}
`},
	},
}

func checkLogHandlers(f *sourceFile) []string {
	if f.test || within(f.dir, logxDir) {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		expr, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel, p := f.ref(expr); sel != nil && p == "log/slog" && slogHandlerConstructors[sel.Sel.Name] {
			out = append(out, f.at(sel, "slog.%s outside %s builds a logger that bypasses its scrubbing writer: use logx.New", sel.Sel.Name, logxDir))
		}
		return true
	})
	return out
}
