package archtest

import (
	"go/ast"
	"path"
	"regexp"
)

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

const standardStreamsFile = "cmd/wawarden/main.go"

var processOutput = map[string]map[string]bool{
	"log/slog": set("Default", "SetDefault", "Debug", "DebugContext", "Info", "InfoContext", "Warn", "WarnContext",
		"Error", "ErrorContext", "Log", "LogAttrs"),
	"log": set("Default", "SetOutput", "Writer", "Output", "Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln",
		"Panic", "Panicf", "Panicln"),
	"fmt":     set("Print", "Printf", "Println"),
	"os":      set("Stdout", "Stderr", "NewFile"),
	"syscall": set("Stdout", "Stderr", "Write"),
}

var streamPath = regexp.MustCompile(`/dev/(?:stdout|stderr|fd/)|/proc/[^/\s]+/fd/`)

var logOutputRule = rule{
	name:  "log-output",
	check: checkLogOutput,
	cases: []snippet{
		{name: "the default loggers and the standard streams", rel: "internal/app/x.go", want: 27, src: `package app

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	s "log/slog"
	"os"
	"syscall"
)

func f(ctx context.Context) {
	slog.Info("chat", "id", "15550100001@s.whatsapp.net")
	slog.Default().Error("x")
	slog.SetDefault(slog.New(slog.DiscardHandler))
	slog.WarnContext(ctx, "x")
	s.Debug("x")
	slog.LogAttrs(ctx, slog.LevelInfo, "x")
	log.Printf("%s", "x")
	log.Println("x")
	log.SetOutput(os.Stdout)
	_ = log.Default()
	_ = log.Writer()
	_ = log.Output(1, "x")
	fmt.Println("x")
	fmt.Printf("%s", "x")
	fmt.Print("x")
	_, _ = os.Stdout.WriteString("x")
	_, _ = fmt.Fprintln(os.Stderr, "x")
	_, _ = syscall.Write(syscall.Stderr, nil)
	_ = os.NewFile(2, "")
	_, _ = os.OpenFile("/dev/stdout", os.O_WRONLY, 0)
	_, _ = os.OpenFile("/dev/fd/"+"2", os.O_WRONLY, 0)
	_, _ = os.OpenFile("/proc/self/fd/1", os.O_WRONLY, 0)
	println("x")
	print("x")
	p := fmt.Println
	_, _ = p("x")
}
`},
		{name: "loggers and writers handed in", rel: "internal/app/x.go", src: `package app

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"

	other "example.com/log"
)

func f(w io.Writer, logger *slog.Logger, l *log.Logger) string {
	_, _ = os.ReadDir("/proc/self/fd")
	_, _ = os.ReadDir("/dev/fd")
	_, _ = os.Open("/dev/null")
	logger.Info("x")
	logger.Error("x", slog.String("event", "x"))
	l.Println("x")
	_, _ = fmt.Fprintln(w, "x")
	_ = log.New(w, "", 0)
	_ = slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	other.Printf("x")
	return fmt.Sprintf("%d", 1)
}
`},
		{name: "the standard streams in main", rel: "cmd/wawarden/main.go", src: `package main

import "os"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
`},
		{name: "the default loggers in main and the standard streams elsewhere in its package", rel: "cmd/wawarden/run.go", want: 3, src: `package main

import (
	"fmt"
	"log/slog"
	"os"
)

func run() int {
	slog.Info("x")
	fmt.Println("x")
	_, _ = os.Stdout.WriteString("x")
	return 0
}
`},
		{name: "the default loggers in main", rel: "cmd/wawarden/main.go", want: 2, src: `package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	log.Println("x")
	fmt.Println("x")
	os.Exit(0)
}
`},
		{name: "the default loggers and the standard streams in a test", rel: "internal/app/x_test.go", src: `package app

import (
	"fmt"
	"log/slog"
	"os"
)

func f() {
	slog.Info("x")
	fmt.Println("x")
	println("x")
	_, _ = os.Stderr.WriteString("x")
}
`},
	},
}

func checkLogOutput(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			sel, p := f.ref(x)
			if sel == nil || !processOutput[p][sel.Sel.Name] || (p == "os" && f.rel == standardStreamsFile) {
				return true
			}
			out = append(out, f.at(sel, "%s.%s reaches the process's output past the scrubbing writer: log through logx.New and print to the writers run receives", path.Base(p), sel.Sel.Name))
		case *ast.CallExpr:
			if id, ok := ast.Unparen(x.Fun).(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
				out = append(out, f.at(id, "the built-in %s reaches standard error past the scrubbing writer", id.Name))
			}
		}
		return true
	})
	literalRuns(f.file, func(at ast.Node, s string) {
		if m := streamPath.FindString(s); m != "" {
			out = append(out, f.at(at, "%q names a descriptor of the process, which reaches its output past the scrubbing writer", m))
		}
	})
	return out
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
