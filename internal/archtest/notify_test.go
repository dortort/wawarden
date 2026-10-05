package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	notifyDir  = "internal/notify"
	notifyFile = notifyDir + "/notify.go"
)

var notifyEvents = set("admin_mutation", "admin_auth_failure", "unpaired", "disconnected", "pair_rejected", "logout_failed",
	"quarantine", "rekey_conflict", "ingest_paused", "backup_done", "backup_failed")

var notifyEventRule = rule{
	name:  "notify-events",
	check: checkNotifyEvents,
	cases: []snippet{
		{name: "operational events logged outside internal/notify", rel: "internal/engine/x.go", want: 3, src: `package engine

import "log/slog"

func f(alerts *slog.Logger) {
	alerts.Warn("x", slog.String("event", "unpaired"))
	alerts.Warn("x", "event", "quarantine", "queue", "inbox")
	alerts.LogAttrs(nil, slog.LevelWarn, "x", slog.Any("event", "admin_"+"mutation"))
}
`},
		{name: "other events and the same words elsewhere", rel: "internal/engine/x.go", src: `package engine

import "log/slog"

const StateUnpaired = "unpaired"

func f(logger *slog.Logger, state string) {
	logger.Info("x", slog.String("event", "engine_state"), slog.String("state", "disconnected"))
	logger.Warn("x", "event", state)
	logger.Warn("x", "reason", "unpaired")
}
`},
		{name: "internal/notify and tests", rel: "internal/notify/x.go", src: `package notify

import "log/slog"

func f(logger *slog.Logger) { logger.Warn("x", slog.String("event", "unpaired")) }
`},
		{name: "a test", rel: "internal/app/x_test.go", src: `package app

import "log/slog"

func f(logger *slog.Logger) { logger.Warn("x", "event", "disconnected") }
`},
	},
}

func checkNotifyEvents(f *sourceFile) []string {
	if f.test || within(f.dir, notifyDir) {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for i := 0; i+1 < len(call.Args); i++ {
			key, isKey := constString(call.Args[i])
			name, isName := constString(call.Args[i+1])
			if isKey && isName && key == "event" && notifyEvents[name] {
				out = append(out, f.at(call.Args[i+1], "the operational event %q is written outside %s: call the notifier, so that it keeps one shape and reaches the webhook", name, notifyDir))
			}
		}
		return true
	})
	return out
}

func TestNotifyEventNamesMatchThePackage(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(moduleRoot(t), notifyFile), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", notifyFile, err)
	}
	declared := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Event") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				declared[v] = true
			}
		}
	}
	if got, want := slices.Sorted(maps.Keys(declared)), slices.Sorted(maps.Keys(notifyEvents)); !slices.Equal(got, want) {
		t.Fatalf("%s declares the events %q, the notify-events rule knows %q: keep them equal", notifyFile, got, want)
	}
}
