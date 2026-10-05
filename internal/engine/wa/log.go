package wa

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	maxDetailBytes = 1024
	minMaskedRun   = 6
	truncated      = "[truncated]"
	maskedNumber   = "[number]"
)

type debugGate struct {
	until  time.Time
	now    func() time.Time
	alerts *slog.Logger
	ended  atomic.Bool
}

func newDebugGate(d time.Duration, now func() time.Time, alerts *slog.Logger) *debugGate {
	g := &debugGate{now: now, alerts: alerts}
	if d > 0 {
		g.until = now().Add(d)
		alerts.Warn("protocol library debug logging is on: it can record what the log writer does not recognise, such as push names and bare phone numbers",
			slog.String("event", "unsafe_debug"), slog.Int64("minutes", int64(d/time.Minute)), slog.Time("until", g.until.UTC()))
	}
	return g
}

func (g *debugGate) on() bool {
	if g.until.IsZero() {
		return false
	}
	if g.now().Before(g.until) {
		return true
	}
	if !g.ended.Swap(true) {
		g.alerts.Warn("protocol library debug logging ended", slog.String("event", "unsafe_debug_ended"))
	}
	return false
}

type logger struct {
	out    *slog.Logger
	module string
	gate   *debugGate
}

var _ waLog.Logger = logger{}

func (l logger) Errorf(msg string, args ...any) { l.write(slog.LevelError, msg, args) }
func (l logger) Warnf(msg string, args ...any)  { l.write(slog.LevelWarn, msg, args) }
func (l logger) Infof(msg string, args ...any)  { l.write(slog.LevelInfo, msg, args) }

func (l logger) Debugf(msg string, args ...any) {
	if l.gate.on() {
		l.write(slog.LevelDebug, msg, args)
	}
}

func (l logger) Sub(module string) waLog.Logger {
	return logger{out: l.out, module: l.module + "/" + module, gate: l.gate}
}

func (l logger) write(level slog.Level, format string, args []any) {
	ctx := context.Background()
	if !l.out.Enabled(ctx, level) {
		return
	}
	detail := clip(maskNumbers(fmt.Sprintf(format, args...)))
	l.out.LogAttrs(ctx, level, "protocol library log", slog.String("event", "whatsmeow_log"), slog.String("module", l.module), slog.String("detail", detail))
}

func maskNumbers(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		end, digits := i, 0
		for end < len(s) && (isDigit(s[end]) || s[end] == '.' || s[end] == ':') {
			if isDigit(s[end]) {
				digits++
			}
			end++
		}
		if digits >= minMaskedRun && (end == len(s) || s[end] != '@') {
			b.WriteString(s[last:i])
			b.WriteString(maskedNumber)
			last = end
		}
		i = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func clip(s string) string {
	if len(s) <= maxDetailBytes {
		return s
	}
	cut := strings.LastIndexAny(s[:maxDetailBytes+1], " \t\n\r")
	if cut < 0 {
		return truncated
	}
	return s[:cut] + " " + truncated
}
