package logx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

const (
	canaryUser = "15550100042"
	canary     = canaryUser + "@s.whatsapp.net"
)

var (
	testKey   = bytes.Repeat([]byte{0x5a}, keySize)
	fixedTime = time.Date(2026, time.January, 2, 3, 4, 5, 6_000_000, time.UTC)
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func keyedWriter(t *testing.T) (*Writer, *syncBuffer) {
	t.Helper()
	out := &syncBuffer{}
	w := NewWriter(out)
	w.now = func() time.Time { return fixedTime }
	w.SetKey(testKey)
	return w, out
}

func expected(canonical string) string {
	mac := hmac.New(sha256.New, testKey)
	mac.Write([]byte(canonical))
	return "jid:" + hex.EncodeToString(mac.Sum(nil))[:8]
}

func through(t *testing.T, w *Writer, out *syncBuffer, line string) string {
	t.Helper()
	before := len(out.String())
	n, err := w.Write([]byte(line))
	if err != nil || n != len(line) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(line))
	}
	return out.String()[before:]
}

func TestIdentifiersBecomePseudonyms(t *testing.T) {
	phone := expected("15550100001@s.whatsapp.net")
	tests := []struct {
		name, in, want string
		letters        bool
	}{
		{name: "phone user", in: "15550100001@s.whatsapp.net", want: phone},
		{name: "phone user with a device", in: "15550100001:4@s.whatsapp.net", want: phone},
		{name: "phone user with an agent and a device", in: "15550100001.0:4@s.whatsapp.net", want: phone},
		{name: "legacy phone server", in: "15550100001@c.us", want: phone},
		{name: "upper-case server", in: "15550100001@S.WHATSAPP.NET", want: phone},
		{name: "LID user", in: "100000000000001@lid", want: expected("100000000000001@lid")},
		{name: "LID user with a device", in: "100000000000001:7@lid", want: expected("100000000000001@lid")},
		{name: "LID user with an agent and a device", in: "100000000000001.1:7@lid", want: expected("100000000000001@lid")},
		{name: "group", in: "120363000000000001@g.us", want: expected("120363000000000001@g.us")},
		{name: "legacy group", in: "15550100001-1600000000@g.us", want: expected("15550100001-1600000000@g.us")},
		{name: "status broadcast", in: "status@broadcast", want: expected("status@broadcast"), letters: true},
		{name: "broadcast list", in: "1600000000@broadcast", want: expected("1600000000@broadcast")},
		{name: "newsletter", in: "120363000000000002@newsletter", want: expected("120363000000000002@newsletter")},
		{name: "bot", in: "15550100009@bot", want: expected("15550100009@bot")},
		{name: "hosted", in: "15550100008@hosted", want: expected("15550100008@hosted")},
		{name: "hosted LID", in: "100000000000002:3@hosted.lid", want: expected("100000000000002@hosted.lid")},
		{name: "messenger", in: "15550100006@msgr", want: expected("15550100006@msgr")},
		{name: "interop", in: "15550100005@interop", want: expected("15550100005@interop")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frames := []struct{ before, after string }{
				{`{"msg":"`, `"}`},
				{`{"msg":"from=`, `,"x":1}`},
				{`{"msg":"(`, `)."}`},
				{`{"`, `":true}`},
				{`{"msg":"`, `_session"}`},
				{`at the end of a line `, ``},
			}
			if !tt.letters {
				frames = append(frames, struct{ before, after string }{`{"msg":"sender:`, ` sent"}`}, struct{ before, after string }{`{"chat":"+`, `"}`})
			}
			for _, frame := range frames {
				w, out := keyedWriter(t)
				got := through(t, w, out, frame.before+tt.in+frame.after+"\n")
				if want := frame.before + tt.want + frame.after + "\n"; got != want {
					t.Fatalf("Write(%q) wrote %q, want %q", frame.before+tt.in+frame.after, got, want)
				}
			}
		})
	}
}

func TestIdentifiersOneSeparatorApart(t *testing.T) {
	w, out := keyedWriter(t)
	got := through(t, w, out, `{"msg":"15550100001@s.whatsapp.net,120363000000000001@g.us_15550100009@bot:100000000000001@lid"}`+"\n")
	want := `{"msg":"` + expected("15550100001@s.whatsapp.net") + `,` + expected("120363000000000001@g.us") + `_` +
		expected("15550100009@bot") + `:` + expected("100000000000001@lid") + `"}` + "\n"
	if got != want {
		t.Fatalf("wrote %q\nwant  %q", got, want)
	}
}

func TestPseudonymsBeforeTheKey(t *testing.T) {
	out := &syncBuffer{}
	w := NewWriter(out)
	if got := through(t, w, out, `{"msg":"15550100001:4@s.whatsapp.net and 120363000000000001@g.us"}`+"\n"); got != `{"msg":"jid:unkeyed and jid:unkeyed"}`+"\n" {
		t.Fatalf("an unkeyed writer wrote %q", got)
	}
	w.SetKey(testKey)
	if got := through(t, w, out, `{"msg":"15550100001@s.whatsapp.net"}`+"\n"); got != `{"msg":"`+expected("15550100001@s.whatsapp.net")+`"}`+"\n" {
		t.Fatalf("after SetKey the writer wrote %q", got)
	}
	other := bytes.Repeat([]byte{0x01}, keySize)
	w.SetKey(other)
	if got := through(t, w, out, `{"msg":"15550100001@s.whatsapp.net"}`+"\n"); strings.Contains(got, expected("15550100001@s.whatsapp.net")) || !strings.Contains(got, "jid:") {
		t.Fatalf("after a second SetKey the writer wrote %q, want a pseudonym under the new key", got)
	}
	other[0] = 0xff
	if got := through(t, w, out, `{"msg":"15550100001@s.whatsapp.net"}`+"\n"); !strings.Contains(got, pseudonymUnder(bytes.Repeat([]byte{0x01}, keySize), "15550100001@s.whatsapp.net")) {
		t.Fatalf("changing the caller's key slice changed the writer's key: %q", got)
	}
}

func pseudonymUnder(key []byte, canonical string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(canonical))
	return "jid:" + hex.EncodeToString(mac.Sum(nil))[:8]
}

func TestTextThatIsNotAnIdentifierIsKept(t *testing.T) {
	for _, line := range []string{
		`{"msg":"go.mau.fi/whatsmeow@v0.0.0-20260929112325-8b41cfe6d9c4/send.go:12"}`,
		`{"msg":"golang.org/x/net@v0.40.0","server":"s.whatsapp.net","group":"g.us"}`,
		`{"msg":"someone@example.test wrote","to":"ops@lidar.example"}`,
		`{"msg":"an @lid and an @g.us without a user"}`,
		`{"msg":"servers run on: 15550100150@lidx, 15550100151@g.us2, 15550100152@s.whatsapp.network"}`,
		`{"msg":"value <nil>","list":"[<nil> <nil>]"}`,
		`{"stack":"main.(*T).M(...)\n\t<autogenerated>:1 +0x1c"}`,
		`{"msg":"a < b and c > d, x<3, got <unknown> state, <-chan int"}`,
		`{"msg":"if a <b = c then <T> <U>, map[<nil>:<nil>], <nil>\t<nil>"}`,
		`{"msg":"<message> without attributes"}`,
		`{"msg":"plain"}`,
		`plain text`,
	} {
		w, out := keyedWriter(t)
		want := line + "\n"
		if got := through(t, w, out, want); got != want {
			t.Errorf("Write(%q) wrote %q, want it unchanged", want, got)
		}
	}
}

func TestEmailLikeTextOnAServerNameIsRedacted(t *testing.T) {
	w, out := keyedWriter(t)
	if got := through(t, w, out, `{"msg":"15550100001@lid.example.test"}`+"\n"); strings.Contains(got, "15550100001") {
		t.Fatalf("Write kept the user of an identifier-shaped address: %q", got)
	}
}

func TestLinesCarryingXMLAreDropped(t *testing.T) {
	dropped := `{"time":"2026-01-02T03:04:05.006Z","level":"WARN","msg":"log line dropped","event":"log_dropped","reason":"xml"}` + "\n"
	for _, line := range []string{
		`{"msg":"</message>"}`,
		`{"msg":"</stream:stream>"}`,
		`{"msg":"<ack/>"}`,
		`{"msg":"<ping />"}`,
		`{"msg":"<iq id=\"1\" type=\"get\"/>"}`,
		`{"msg":"<iq id=\"1\" type=\"get\"><ping/></iq>"}`,
		`{"msg":"<message from=\"15550100001@s.whatsapp.net\" id=\"3EB0\">"}`,
		`{"msg":"<receipt id='1' type='read'>"}`,
		`{"msg":"<iq\n id=\"1\"\tto=\"s.whatsapp.net\">"}`,
		`{"msg":"<!-- 300 bytes -->"}`,
		`{"msg":"<![CDATA[x]]>"}`,
		`{"msg":"<?xml version=\"1.0\"?>"}`,
		`{"msg":"<iq id=\"1\"/>"}`,
		`{"msg":"<notification type=\"server\">"}`,
		`{"node":"<success t=\"1700000000\" lid=\"100000000000001@lid\"/>","msg":"ok"}`,
		`<message from="15550100001@s.whatsapp.net">hi</message>`,
		`{"msg":"<receipt notify=\"Eve \"Mallory\" Doe\" from=\"15550100053@s.whatsapp.net\"/>"}`,
		`{"msg":"<presence name=\"a\"b\" type=\"available\">"}`,
		`{"msg":"<presence name='a'b' type='available'/>"}`,
		`{"msg":"<message id=\"3EB0"}`,
		`{"msg":"<message><body>secret text 15550100054"}`,
		`{"msg":"<message><!-- 300 bytes"}`,
	} {
		w, out := keyedWriter(t)
		if got := through(t, w, out, line+"\n"); got != dropped {
			t.Errorf("Write(%q) wrote %q, want the log_dropped line", line, got)
		}
	}

	lw, lout := keyedWriter(t)
	logger := New(lw, slog.LevelDebug)
	for _, text := range []string{
		`<receipt notify="Eve "Mallory" Doe" from="15550100053@s.whatsapp.net"/>`,
		`<receipt notify="Eve "Mallory" Doe" from="15550100053@s.whatsapp.net">`,
		`<presence name="a"b" type="available"/>`,
		`<message><body>secret text 15550100054`,
	} {
		w, out := keyedWriter(t)
		if got := through(t, w, out, text+"\n"); got != dropped {
			t.Errorf("Write(%q) wrote %q, want the log_dropped line", text, got)
		}
		before := len(lout.String())
		logger.Info(text, "node", text)
		if got := lout.String()[before:]; got != dropped {
			t.Errorf("logging %q wrote %q, want the log_dropped line", text, got)
		}
	}
}

func TestLinesCarryingEscapedTagsAreDropped(t *testing.T) {
	dropped := `{"time":"2026-01-02T03:04:05.006Z","level":"WARN","msg":"log line dropped","event":"log_dropped","reason":"xml"}` + "\n"
	backslash := string(rune(92))
	lt, gt := backslash+"u003c", backslash+"u003e"
	stanza, err := json.Marshal(map[string]string{"msg": `<message to="` + canary + `"><body>synthetic secret text</body></message>`})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(stanza, []byte(lt)) || bytes.IndexByte(stanza, '<') >= 0 {
		t.Fatalf("json.Marshal wrote %s with its tags unescaped, so this test proves nothing", stanza)
	}
	quoted, err := json.Marshal(map[string]string{"msg": `<receipt notify="Eve "Mallory" Doe" from="` + canary + `"/>`})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	adjacent, err := json.Marshal(map[string]string{"msg": `<message><body>synthetic secret text ` + canary})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for name, line := range map[string]string{
		"json.Marshal":                  string(stanza),
		"quote inside an attribute":     string(quoted),
		"tag followed by a tag":         string(adjacent),
		"self-closing tag":              `{"msg":"` + lt + "ack/" + gt + `"}`,
		"upper-case hexadecimal digits": `{"msg":"` + backslash + "u003Cack/" + backslash + "u003E" + `"}`,
		"closing tag":                   `{"msg":"` + lt + "/message" + gt + `"}`,
	} {
		w, out := keyedWriter(t)
		if got := through(t, w, out, line+"\n"); got != dropped {
			t.Errorf("%s: Write(%q) wrote %q, want the log_dropped line", name, line, got)
		}
	}

	w, out := keyedWriter(t)
	logger := New(w, slog.LevelDebug)
	for _, text := range []string{
		string(stanza),
		`{"msg":"` + lt + "iq" + backslash + `n id=\"1\"/` + gt + `"}`,
	} {
		before := len(out.String())
		logger.Error("failed", "error", errors.New(text))
		if got := out.String()[before:]; got != dropped {
			t.Errorf("logging an error of text %q wrote %q, want the log_dropped line", text, got)
		}
	}

	for _, line := range []string{
		`{"msg":"a ` + lt + ` b and c ` + gt + ` d"}`,
		`{"msg":"value ` + lt + "nil" + gt + `"}`,
		`{"msg":"quoted ` + backslash + `"text` + backslash + `" and ` + lt + "unknown" + gt + `"}`,
	} {
		w, out := keyedWriter(t)
		if got := through(t, w, out, line+"\n"); got != line+"\n" {
			t.Errorf("Write(%q) wrote %q, want it unchanged", line, got)
		}
	}
}

func TestEachLineOfAWriteIsCheckedOnItsOwn(t *testing.T) {
	w, out := keyedWriter(t)
	got := through(t, w, out, "{\"a\":\"15550100001@s.whatsapp.net\"}\n{\"b\":\"<ack/>\"}\n{\"c\":\"kept\"}\n")
	want := `{"a":"` + expected("15550100001@s.whatsapp.net") + `"}` + "\n" +
		`{"time":"2026-01-02T03:04:05.006Z","level":"WARN","msg":"log line dropped","event":"log_dropped","reason":"xml"}` + "\n" +
		`{"c":"kept"}` + "\n"
	if got != want {
		t.Fatalf("wrote %q\nwant  %q", got, want)
	}
}

func TestALineIsWrittenOnlyWhenItEnds(t *testing.T) {
	dropped := func(reason string) string {
		return `{"time":"2026-01-02T03:04:05.006Z","level":"WARN","msg":"log line dropped","event":"log_dropped","reason":"` + reason + `"}` + "\n"
	}
	split := expected("15550100173@s.whatsapp.net")
	tests := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{
			name:   "identifier split across two writes",
			chunks: []string{`{"msg":"155501001`, "73@s.whatsapp.net\"}\n"},
			want:   []string{"", `{"msg":"` + split + `"}` + "\n"},
		},
		{
			name:   "identifier split at its server",
			chunks: []string{`{"msg":"15550100173@s.what`, "sapp.net\"}\n{\"n\":1}\n"},
			want:   []string{"", `{"msg":"` + split + `"}` + "\n" + `{"n":1}` + "\n"},
		},
		{
			name:   "tag split across two writes",
			chunks: []string{`{"msg":"<iq id=`, `\"1\"/>"}` + "\n"},
			want:   []string{"", dropped("xml")},
		},
		{
			name:   "line ended by a write of its newline",
			chunks: []string{`{"msg":"15550100173@s.whatsapp.net"}`, "\n"},
			want:   []string{"", `{"msg":"` + split + `"}` + "\n"},
		},
		{
			name:   "whole lines and the start of the next",
			chunks: []string{"{\"a\":1}\n{\"b\":\"155501001", "73@s.whatsapp.net\"}\n"},
			want:   []string{`{"a":1}` + "\n", `{"b":"` + split + `"}` + "\n"},
		},
		{
			name:   "line of the largest length in pieces",
			chunks: []string{strings.Repeat("a", maxLineBytes-1), "b", "\n"},
			want:   []string{"", "", strings.Repeat("a", maxLineBytes-1) + "b\n"},
		},
		{
			name:   "line one byte too long in pieces",
			chunks: []string{strings.Repeat("a", maxLineBytes), canary, "\n{\"next\":true}\n"},
			want:   []string{"", "", dropped("too_long") + `{"next":true}` + "\n"},
		},
		{
			name:   "line one byte too long in one write",
			chunks: []string{strings.Repeat("a", maxLineBytes+1) + "\n"},
			want:   []string{dropped("too_long")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out := keyedWriter(t)
			for i, chunk := range tt.chunks {
				if got := through(t, w, out, chunk); got != tt.want[i] {
					t.Fatalf("write %d of %q wrote %q, want %q", i, chunk, got, tt.want[i])
				}
			}
			if strings.Contains(out.String(), "155501001") {
				t.Fatalf("part of an identifier reached the output: %q", out.String())
			}
		})
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriteReportsTheOutputError(t *testing.T) {
	failure := errors.New("synthetic output failure")
	w := NewWriter(failingWriter{failure})
	if n, err := w.Write([]byte("{}\n")); n != 0 || !errors.Is(err, failure) {
		t.Fatalf("Write = %d, %v; want 0 and the output's error", n, err)
	}
}

func TestEscapedCharactersNextToAnIdentifierKeepTheLineValid(t *testing.T) {
	w, out := keyedWriter(t)
	logger := New(w, slog.LevelDebug)
	for _, s := range []string{
		"\n" + canary,
		"\r\t" + canary,
		"\x1b" + canary,
		"\x01" + canary,
		"\x00" + canaryUser + ":4@lid",
		"\\" + canary,
		"\"" + canary + "\"",
		"\n@lid",
		"\x1b@g.us",
		"\x1f\x1e\x1d@s.whatsapp.net",
		" " + canary,
	} {
		logger.Info("escape", "value", s)
	}
	for line := range strings.Lines(out.String()) {
		if !json.Valid([]byte(line)) {
			t.Errorf("the writer broke a JSON line: %q", line)
		}
	}
	if strings.Contains(out.String(), canaryUser) {
		t.Fatalf("the canary survived next to an escape:\n%s", out.String())
	}
}

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

type panicky struct{}

func (*panicky) String() string { panic(canary) }

type failure struct{}

func (*failure) Error() string { panic(canary) }

type blob []byte

type holder struct {
	Chat string
	Data []byte
}

type valuer struct{ v slog.Value }

func (v valuer) LogValue() slog.Value { return v.v }

func grants(t *testing.T, chat policy.CanonicalChat) []any {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	read, ok := policy.DecideRead(&policy.Client{ID: "synthetic-client", Read: map[policy.CanonicalChat]struct{}{chat: {}}, ExpiresAt: expires}, time.Now())
	if !ok {
		t.Fatal("DecideRead refused the synthetic client")
	}
	write, ok := policy.DecideWrite(&policy.Client{ID: "synthetic-client", Write: map[policy.CanonicalChat]struct{}{chat: {}}, ExpiresAt: expires}, time.Now())
	if !ok {
		t.Fatal("DecideWrite refused the synthetic client")
	}
	secret := token.NewAdmin()
	cred, err := policy.ParseAdminCredential(token.Hash(secret))
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	admin, ok := policy.DecideAdmin(cred, secret)
	if !ok {
		t.Fatal("DecideAdmin refused the token")
	}
	return []any{read, &read, write, admin, cred}
}

func TestAttributesRenderFailClosed(t *testing.T) {
	chat, ok := policy.Normalize(canary)
	if !ok {
		t.Fatalf("Normalize(%q) failed", canary)
	}
	w, out := keyedWriter(t)
	logger := New(w, slog.LevelDebug)
	tests := []struct {
		name  string
		value any
		want  any
	}{
		{name: "byte slice", value: []byte(canary), want: "[26 bytes]"},
		{name: "empty byte slice", value: []byte{}, want: "[0 bytes]"},
		{name: "error", value: errors.New("dial " + canary + " failed"), want: "dial " + expected(canary) + " failed"},
		{name: "wrapped error", value: fmt.Errorf("outer: %w", errors.New(canary)), want: "outer: " + expected(canary)},
		{name: "Stringer", value: stringer{"peer " + canary}, want: "peer " + expected(canary)},
		{name: "struct", value: holder{Chat: canary, Data: []byte(canary)}, want: "[logx.holder]"},
		{name: "pointer to struct", value: &holder{Chat: canary}, want: "[*logx.holder]"},
		{name: "map", value: map[string]string{"chat": canary}, want: "[map[string]string]"},
		{name: "slice of strings", value: []string{canary}, want: "[[]string]"},
		{name: "array of bytes", value: [4]byte{1, 2, 3, 4}, want: "[[4]uint8]"},
		{name: "named byte slice", value: blob(canary), want: "[logx.blob]"},
		{name: "policy chat", value: chat, want: "[seal.Chat]"},
		{name: "pointer to policy chat", value: &chat, want: "[*seal.Chat]"},
		{name: "Stringer that panics", value: &panicky{}, want: "[*logx.panicky]"},
		{name: "error that panics", value: &failure{}, want: "[*logx.failure]"},
		{name: "nil", value: nil, want: nil},
		{name: "string", value: canary, want: expected(canary)},
		{name: "integer", value: 42, want: float64(42)},
		{name: "LogValuer of a struct", value: valuer{slog.AnyValue(holder{Chat: canary})}, want: "[logx.holder]"},
		{name: "LogValuer of a string", value: valuer{slog.StringValue(canary)}, want: expected(canary)},
	}
	for _, g := range grants(t, chat) {
		tests = append(tests, struct {
			name  string
			value any
			want  any
		}{name: fmt.Sprintf("%T", g), value: g, want: fmt.Sprintf("[%T]", g)})
	}
	for _, tt := range tests {
		before := len(out.String())
		logger.Info("render", "value", tt.value)
		line := out.String()[before:]
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%s: not one JSON line: %v: %q", tt.name, err, line)
		}
		if got := rec["value"]; got != tt.want {
			t.Errorf("%s: value = %#v, want %#v", tt.name, got, tt.want)
		}
	}
	if strings.Contains(out.String(), canaryUser) {
		t.Fatalf("the canary reached the output:\n%s", out.String())
	}
}

func TestTheCanaryNeverReachesTheOutput(t *testing.T) {
	chat, ok := policy.Normalize(canary)
	if !ok {
		t.Fatalf("Normalize(%q) failed", canary)
	}
	w, out := keyedWriter(t)
	logger := New(w, slog.LevelDebug)
	alerts := New(w, slog.LevelWarn)
	err := fmt.Errorf("wrapped: %w", errors.Join(errors.New(canary), errors.New("<iq to=\""+canary+"\"/>")))
	values := []any{
		canary, []byte(canary), errors.New(canary), err, stringer{canary}, holder{Chat: canaryUser, Data: []byte(canary)},
		&holder{Chat: canaryUser}, map[string]any{"phone": canaryUser}, []any{canaryUser}, chat, &chat,
		valuer{slog.StringValue(canary)}, valuer{slog.GroupValue(slog.String(canary, canary))},
		valuer{slog.AnyValue(holder{Chat: canaryUser})}, json.RawMessage(canary), blob(canary),
		&panicky{}, &failure{}, [1]string{canaryUser},
	}
	values = append(values, grants(t, chat)...)
	for i, v := range values {
		key := "k" + strconv.Itoa(i)
		logger.Info(canary, key, v)
		logger.Info("message "+canary, slog.Any(key, v))
		logger.With(key, v, canary, v).WithGroup(canary).Info("with", key, v)
		logger.Info("group", slog.Group(canary, slog.Any(key, v), slog.String(canary, canary)))
		logger.LogAttrs(context.Background(), slog.LevelDebug, "attrs", slog.Any(key, v))
		alerts.Warn("alert "+canary, key, v)
		slog.NewLogLogger(logger.Handler(), slog.LevelWarn).Printf("bridge %s", canary)
	}
	if _, err := fmt.Fprintf(w, `{"_aws":{"Timestamp":1},"Name":"%s"}`+"\n", canary); err != nil {
		t.Fatalf("direct write: %v", err)
	}
	text := out.String()
	if strings.Contains(text, canaryUser) || strings.Contains(text, "whatsapp.net\"") || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(canary))[:12]) {
		t.Fatalf("the canary reached the output:\n%s", text)
	}
	for line := range strings.Lines(text) {
		if !json.Valid([]byte(line)) {
			t.Fatalf("not a JSON line: %q", line)
		}
	}
	if !strings.Contains(text, expected(canary)) {
		t.Fatalf("no pseudonym in the output, so the test proves nothing:\n%s", text)
	}
}

func TestConcurrentWritersNeitherInterleaveNorLeak(t *testing.T) {
	out := &syncBuffer{}
	w := NewWriter(out)
	logger := New(w, slog.LevelInfo)
	alerts := New(w, slog.LevelWarn)
	const workers, lines = 16, 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := range lines {
				switch j % 3 {
				case 0:
					logger.Info("message", "worker", i, "chat", canary, "err", errors.New(canary))
				case 1:
					alerts.Warn("alert "+canary, "n", j)
				default:
					_, _ = fmt.Fprintf(w, `{"metric":%d,"chat":"%s"}`+"\n{\"second\":\"<ack/>\"}\n", j, canary)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		w.SetKey(testKey)
	}()
	close(start)
	wg.Wait()
	count := 0
	for line := range strings.Lines(out.String()) {
		count++
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("an interleaved or broken line: %q", line)
		}
		if strings.Contains(line, canaryUser) {
			t.Fatalf("the canary reached the output: %q", line)
		}
		if strings.Contains(line, "jid:") && !strings.Contains(line, "jid:unkeyed") && !strings.Contains(line, expected(canary)) {
			t.Fatalf("a pseudonym under neither state of the key: %q", line)
		}
	}
	if want := workers*lines + workers*(lines/3); count != want {
		t.Fatalf("%d lines, want %d", count, want)
	}
}

func TestLoggerWritesUTCJSON(t *testing.T) {
	at := time.Date(2026, time.January, 2, 3, 4, 5, 6_000_000, time.FixedZone("synthetic", -4*60*60))
	record := slog.NewRecord(at, slog.LevelInfo, "message", 0)
	record.AddAttrs(slog.String("event", "probe"))
	out := &syncBuffer{}
	if err := New(NewWriter(out), nil).Handler().Handle(t.Context(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var rec struct {
		Time  string `json:"time"`
		Level string `json:"level"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal([]byte(out.String()), &rec); err != nil {
		t.Fatalf("not JSON: %v: %q", err, out.String())
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil || !strings.HasSuffix(rec.Time, "Z") || !ts.Equal(at) || rec.Level != "INFO" || rec.Event != "probe" {
		t.Fatalf("record = %+v (%v), want %v written in UTC, level INFO and the event attribute", rec, err, at)
	}
}

func TestLevelFiltersRecords(t *testing.T) {
	out := &syncBuffer{}
	logger := New(NewWriter(out), slog.LevelWarn)
	logger.Info("filtered")
	logger.Warn("kept")
	if got := strings.Count(out.String(), "\n"); got != 1 || !strings.Contains(out.String(), `"msg":"kept"`) {
		t.Fatalf("output %q, want only the WARN line", out.String())
	}
}

func TestMisuseIsRefused(t *testing.T) {
	for name, f := range map[string]func(){
		"NewWriter without an output": func() { NewWriter(nil) },
		"New without a Writer":        func() { New(nil, nil) },
		"SetKey with a short key":     func() { NewWriter(&syncBuffer{}).SetKey(make([]byte, keySize-1)) },
		"SetKey with no key":          func() { NewWriter(&syncBuffer{}).SetKey(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			f()
		})
	}
}
