package wa

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newAdapterLogger(t *testing.T, debug time.Duration) (logger, *logBuffer, *logBuffer, *fakeClock) {
	t.Helper()
	level := slog.LevelInfo
	if debug > 0 {
		level = slog.LevelDebug
	}
	out, logs := newLogger(level)
	alerts, alertLogs := newLogger(slog.LevelWarn)
	clock := &fakeClock{now: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)}
	return logger{out: out, module: "whatsmeow", gate: newDebugGate(debug, clock.Now, alerts)}, logs, alertLogs, clock
}

func TestLibraryLogsReachTheScrubbingWriter(t *testing.T) {
	l, logs, _, _ := newAdapterLogger(t, 0)
	sub := l.Sub("Client").Sub("Recv")
	sub.Errorf("Failed to decrypt message from %s in %s: %v", "15550100001:5@s.whatsapp.net", "120363000000000001@g.us", errors.New("synthetic"))
	sub.Warnf("Got LID %s for +15550100002, signal address 15550100003.0:4 and session 15550100004:7", "100000000000001@lid")
	sub.Infof("Successfully paired %s", "15550100009:12@s.whatsapp.net")
	sub.Warnf("Node handling took %s for %s", "6s", `<message from="15550100001@s.whatsapp.net" id="3EB0A1"><enc v="2"/></message>`)
	sub.Debugf("<message from=\"15550100001@s.whatsapp.net\" id=\"3EB0A1\"><enc/></message>")
	got := logs.String()
	for _, leak := range []string{"15550100001", "15550100002", "15550100003", "15550100004", "15550100009", "120363000000000001", "100000000000001"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the identifier %s reached the output:\n%s", leak, got)
		}
	}
	lines := logs.events("whatsmeow_log")
	if len(lines) != 3 {
		t.Fatalf("whatsmeow_log events %v, want the error, the warning and the info, and no debug line", lines)
	}
	if dropped := logs.events("log_dropped"); len(dropped) != 1 || dropped[0]["reason"] != "xml" {
		t.Fatalf("log_dropped events %v, want the line with a protocol node dropped", dropped)
	}
	for i, level := range []string{"ERROR", "WARN", "INFO"} {
		if lines[i]["level"] != level || lines[i]["module"] != "whatsmeow/Client/Recv" {
			t.Fatalf("line %d = %v", i, lines[i])
		}
	}
	if d := lines[0]["detail"].(string); !strings.Contains(d, "jid:") || !strings.Contains(d, "synthetic") {
		t.Fatalf("detail %q, want pseudonyms and the error text", d)
	}
	if d := lines[1]["detail"].(string); strings.Count(d, maskedNumber) != 3 || !strings.Contains(d, "jid:") {
		t.Fatalf("detail %q, want the bare numbers masked and the identifier pseudonymised", d)
	}
}

func TestLongLibraryLinesAreCutBetweenWords(t *testing.T) {
	l, logs, _, _ := newAdapterLogger(t, 0)
	body := strings.Repeat("synthetic response body ", 200)
	l.Errorf("unexpected response with status %d: %s and %s", 503, body, "15550100001@s.whatsapp.net")
	l.Warnf("%s", strings.Repeat("x", 4096))
	lines := logs.events("whatsmeow_log")
	if len(lines) != 2 {
		t.Fatalf("events %v", lines)
	}
	first := lines[0]["detail"].(string)
	if len(first) > maxDetailBytes+len(" "+truncated) || !strings.HasSuffix(first, " "+truncated) || strings.Contains(first, "15550100001") {
		t.Fatalf("detail of %d bytes %q", len(first), first[max(0, len(first)-64):])
	}
	if lines[1]["detail"] != truncated {
		t.Fatalf("a single overlong word = %q, want it dropped whole", lines[1]["detail"])
	}
	for _, s := range []string{"15550100001@s.whatsapp.net", "15550100001:4@s.whatsapp.net"} {
		cut := clip(strings.Repeat("a ", maxDetailBytes/2-4) + s)
		if strings.Contains(cut, "1555") && !strings.Contains(cut, s) {
			t.Fatalf("clip split an identifier: %q", cut[len(cut)-40:])
		}
	}
}

func TestMaskNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"no numbers here":                   "no numbers here",
		"short 12345 run":                   "short 12345 run",
		"phone 15550100001 alone":           "phone " + maskedNumber + " alone",
		"address 15550100001.0:4 alone":     "address " + maskedNumber + " alone",
		"jid 15550100001:4@s.whatsapp.net":  "jid 15550100001:4@s.whatsapp.net",
		"+15550100001, and 100000000000001": "+" + maskedNumber + ", and " + maskedNumber,
		"took 1.5s":                         "took 1.5s",
	} {
		if got := maskNumbers(in); got != want {
			t.Errorf("maskNumbers(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsafeDebugLogsABannerAndRevertsByItself(t *testing.T) {
	l, logs, alerts, clock := newAdapterLogger(t, 2*time.Minute)
	if b := alerts.events("unsafe_debug"); len(b) != 1 || b[0]["level"] != "WARN" || b[0]["minutes"] != float64(2) {
		t.Fatalf("banner events %v", b)
	}
	l.Debugf("Sending %s", "15550100001@s.whatsapp.net")
	clock.advance(time.Minute)
	l.Sub("Socket").Debugf("Frame of %d bytes", 42)
	clock.advance(time.Minute)
	l.Debugf("after the window")
	l.Debugf("still after the window")
	debug := logs.events("whatsmeow_log")
	if len(debug) != 2 || debug[0]["level"] != "DEBUG" || strings.Contains(logs.String(), "15550100001") {
		t.Fatalf("debug lines %v", debug)
	}
	if ended := alerts.events("unsafe_debug_ended"); len(ended) != 1 {
		t.Fatalf("end events %v, want exactly one", ended)
	}
	off, offLogs, offAlerts, _ := newAdapterLogger(t, 0)
	off.Debugf("never")
	if offLogs.String() != "" || offAlerts.String() != "" {
		t.Fatalf("without unsafe debug the logger wrote %q and %q", offLogs.String(), offAlerts.String())
	}
}

func TestLibraryLinesBelowTheLevelAreNotFormatted(t *testing.T) {
	out, logs := newLogger(slog.LevelError)
	alerts, _ := newLogger(slog.LevelWarn)
	l := logger{out: out, module: "whatsmeow", gate: newDebugGate(0, time.Now, alerts)}
	l.Infof("%v", panicking{})
	l.Warnf("%v", panicking{})
	if logs.String() != "" {
		t.Fatalf("lines below the level were written: %q", logs.String())
	}
}

type panicking struct{}

func (panicking) String() string { panic("formatted a line below the log level") }
