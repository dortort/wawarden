// Package safego starts goroutines and recovers panics without exposing panic values.
package safego

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"github.com/dortort/wawarden/internal/metrics"
)

type reporter struct {
	logger *slog.Logger
	panics *metrics.CounterVec
}

var installed atomic.Pointer[reporter]

func Install(logger *slog.Logger, reg *metrics.Registry) {
	if logger == nil || reg == nil {
		panic("safego: Install needs a logger and a registry")
	}
	installed.Store(&reporter{
		logger: logger,
		panics: reg.CounterVec("wawarden_panics_total", "Panics recovered, by goroutine or handler name.", "name"),
	})
}

func Go(name string, fn func()) {
	go func() {
		// Not Recover: no net/http frame above a goroutine would catch its ErrAbortHandler re-panic.
		defer func() { report(name, recover()) }()
		fn()
	}()
}

func Recover(name string) {
	v := recover()
	if v == http.ErrAbortHandler {
		panic(v)
	}
	report(name, v)
}

func report(name string, v any) {
	if v == nil {
		return
	}
	stack := debug.Stack()
	logger := slog.Default()
	if r := installed.Load(); r != nil {
		r.panics.With(name).Inc()
		logger = r.logger
	}
	logger.Error("panic recovered",
		slog.String("event", "panic"),
		slog.String("name", name),
		slog.String("panic_type", fmt.Sprintf("%T", v)),
		slog.String("stack", string(stack)),
	)
}
