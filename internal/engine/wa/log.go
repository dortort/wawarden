package wa

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	signallog "go.mau.fi/libsignal/logger"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/dortort/wawarden/internal/logx"
)

const (
	maxDetailBytes = 1024
	minMaskedRun   = 6
	truncated      = "[truncated]"
	maskedNumber   = "[number]"
	scrubbedURL    = "[url]"
	scrubbedPath   = "[path]"
	scrubbedParam  = "[param]"
	noSlashPath    = "media download path does not start with slash: "
	logStop        = "\\s\"'<>`"
)

var (
	logQuotedURL  = regexp.MustCompile(`"((?i:https?|wss?)://(?:[^"\\]|\\.)*)"`)
	logBareURL    = regexp.MustCompile("\\b((?i:https?|wss?)://[^" + logStop + "]*)")
	logCDNPath    = regexp.MustCompile("(/(?:o1/|m1/)?v/t[0-9][^" + logStop + "]*)")
	logQueryPath  = regexp.MustCompile("(?:^|[^\\w.~%+/@-])(/[^" + logStop + "]*[?&][^" + logStop + "]*)")
	logMediaParam = regexp.MustCompile("\\b(?:hash|mms-type|oh|oe|_nc_\\w+|__wa-mms)=([^&" + logStop + "]+)")
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
	debug  *slog.Logger
	module string
	gate   *debugGate
}

var _ waLog.Logger = logger{}

func newLibraryLogger(w *logx.Writer, level slog.Level, module string, gate *debugGate) logger {
	return logger{out: logx.New(w, level), debug: logx.New(w, slog.LevelDebug), module: module, gate: gate}
}

func (l logger) Errorf(msg string, args ...any) { l.write(l.out, slog.LevelError, msg, args) }
func (l logger) Warnf(msg string, args ...any)  { l.write(l.out, slog.LevelWarn, msg, args) }
func (l logger) Infof(msg string, args ...any)  { l.write(l.out, slog.LevelInfo, msg, args) }

func (l logger) Debugf(msg string, args ...any) {
	if l.gate.on() {
		l.write(l.debug, slog.LevelDebug, msg, args)
	}
}

func (l logger) Sub(module string) waLog.Logger {
	l.module += "/" + module
	return l
}

func (l logger) write(out *slog.Logger, level slog.Level, format string, args []any) {
	ctx := context.Background()
	if !out.Enabled(ctx, level) {
		return
	}
	detail := clip(maskNumbers(logx.RedactTokens(scrubMedia(fmt.Sprintf(format, args...)))))
	out.LogAttrs(ctx, level, "protocol library log", slog.String("event", "whatsmeow_log"), slog.String("module", l.module), slog.String("detail", detail))
}

var (
	signalOut   atomic.Pointer[logger]
	signalSetup = sync.OnceFunc(func() {
		var l signallog.Loggable = signalLogger{}
		signallog.Setup(&l)
	})
)

// Without a logger of its own, the signal library prints its lines to standard output.
func routeSignalLogs(l logger) {
	signalOut.Store(&l)
	signalSetup()
}

type signalLogger struct{}

func (signalLogger) Debug(string, string)       {}
func (signalLogger) Info(string, string)        {}
func (signalLogger) Configure(string)           {}
func (signalLogger) Warning(caller, msg string) { signalLine(slog.LevelWarn, caller, msg) }
func (signalLogger) Error(caller, msg string)   { signalLine(slog.LevelError, caller, msg) }

func signalLine(level slog.Level, caller, msg string) {
	if l := signalOut.Load(); l != nil {
		l.write(l.out, level, "%s: %s", []any{caller, msg})
	}
}

func scrubMedia(s string) string {
	if i := strings.Index(s, noSlashPath); i >= 0 {
		s = s[:i+len(noSlashPath)] + scrubbedPath
	}
	if !strings.ContainsAny(s, "/=") {
		return s
	}
	s = redactGroup(logQuotedURL, s, scrubbedURL)
	s = redactGroup(logBareURL, s, scrubbedURL)
	s = redactGroup(logCDNPath, s, scrubbedPath)
	s = redactGroup(logQueryPath, s, scrubbedPath)
	return redactGroup(logMediaParam, s, scrubbedParam)
}

func redactGroup(re *regexp.Regexp, s, placeholder string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		start := m[2]
		end := start + len(strings.TrimRight(s[start:m[3]], ".,;:!?)"))
		b.WriteString(s[last:start])
		b.WriteString(placeholder)
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
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
