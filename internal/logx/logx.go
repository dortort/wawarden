// Package logx is the one path from a logger to its output: fail-closed attribute rendering over a writer that pseudonymises chat identifiers and drops lines carrying XML.
package logx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	keySize     = 32
	unkeyed     = "jid:unkeyed"
	phoneServer = "s.whatsapp.net"
	legacyPhone = "c.us"
)

const (
	xmlName      = `[A-Za-z_][\w:.-]*`
	xmlSpace     = `(?:\s|\\+[nrt])+`
	xmlAttribute = xmlSpace + xmlName + `\s*=\s*(?:\\?"[^"]*"|'[^']*')`
)

var (
	jidPattern  = regexp.MustCompile(`(?i)[0-9a-z._:+-]+@(s\.whatsapp\.net|c\.us|lid|g\.us|broadcast|newsletter|hosted\.lid|hosted|bot|msgr|interop)\b`)
	userPattern = regexp.MustCompile(`[0-9][0-9._:-]*$`)
	xmlPattern  = regexp.MustCompile(`</` + xmlName + `\s*>|<` + xmlName + `(?:` + xmlAttribute + `)*\s*/>|<` + xmlName + `(?:` + xmlAttribute + `)+\s*>|<!--|<!\[CDATA\[|<\?xml`)
	escapedTag  = regexp.MustCompile(`\\+(u003[cCeE]|")`)
)

type Writer struct {
	mu  sync.Mutex
	out io.Writer
	key []byte
	now func() time.Time
}

func NewWriter(out io.Writer) *Writer {
	if out == nil {
		panic("logx: NewWriter needs an output")
	}
	return &Writer{out: out, now: time.Now}
}

func (w *Writer) SetKey(key []byte) {
	if len(key) != keySize {
		panic("logx: SetKey needs a 32-byte key")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.key = bytes.Clone(key)
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []byte
	for line := range bytes.Lines(p) {
		out = append(out, w.scrub(line)...)
	}
	if _, err := w.out.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *Writer) scrub(line []byte) []byte {
	body, newline := bytes.CutSuffix(line, []byte("\n"))
	if carriesXML(body) {
		return fmt.Appendf(nil, `{"time":%q,"level":"WARN","msg":"log line dropped","event":"log_dropped","reason":"xml"}`+"\n",
			w.now().UTC().Format(time.RFC3339Nano))
	}
	out := w.pseudonymise(body)
	if newline {
		out = append(out, '\n')
	}
	return out
}

func carriesXML(line []byte) bool {
	if bytes.IndexByte(line, '\\') >= 0 {
		line = escapedTag.ReplaceAllFunc(line, unescapeTag)
	}
	return bytes.IndexByte(line, '<') >= 0 && xmlPattern.Match(line)
}

func unescapeTag(escape []byte) []byte {
	switch escape[len(escape)-1] {
	case 'c', 'C':
		return []byte("<")
	case 'e', 'E':
		return []byte(">")
	}
	return []byte(`"`)
}

func (w *Writer) pseudonymise(line []byte) []byte {
	if bytes.IndexByte(line, '@') < 0 {
		return bytes.Clone(line)
	}
	escaped := escapeMask(line)
	var out []byte
	last := 0
	for _, m := range jidPattern.FindAllSubmatchIndex(line, -1) {
		start, at := m[0], m[2]-1
		for i := start; i < at; i++ {
			if escaped != nil && escaped[i] {
				start = i + 1
			}
		}
		if start == at {
			continue
		}
		user := line[start:at]
		if loc := userPattern.FindIndex(user); loc != nil {
			start += loc[0]
			user = user[loc[0]:]
		}
		out = append(out, line[last:start]...)
		out = append(out, w.pseudonym(user, line[m[2]:m[3]])...)
		last = m[1]
	}
	return append(out, line[last:]...)
}

func escapeMask(line []byte) []bool {
	if bytes.IndexByte(line, '\\') < 0 {
		return nil
	}
	mask := make([]bool, len(line))
	for i := 0; i < len(line); i++ {
		if line[i] != '\\' {
			continue
		}
		n := 2
		if i+1 < len(line) && line[i+1] == 'u' {
			n = 6
		}
		for j := i; j < i+n && j < len(line); j++ {
			mask[j] = true
		}
		i += n - 1
	}
	return mask
}

func (w *Writer) pseudonym(user, server []byte) string {
	if w.key == nil {
		return unkeyed
	}
	if i := bytes.IndexAny(user, ".:"); i >= 0 {
		user = user[:i]
	}
	host := strings.ToLower(string(server))
	if host == legacyPhone {
		host = phoneServer
	}
	mac := hmac.New(sha256.New, w.key)
	mac.Write(bytes.ToLower(user))
	mac.Write([]byte("@" + host))
	return "jid:" + hex.EncodeToString(mac.Sum(nil)[:4])
}

func New(w *Writer, level slog.Leveler) *slog.Logger {
	if w == nil {
		panic("logx: New needs a Writer")
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: replaceAttr}))
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	switch v := a.Value; {
	case len(groups) == 0 && a.Key == slog.TimeKey && v.Kind() == slog.KindTime:
		return slog.Time(slog.TimeKey, v.Time().UTC())
	case v.Kind() == slog.KindAny && v.Any() != nil:
		return slog.String(a.Key, render(v.Any()))
	}
	return a
}

func render(v any) (s string) {
	defer func() {
		if recover() != nil {
			s = typeName(v)
		}
	}()
	switch x := v.(type) {
	case []byte:
		return fmt.Sprintf("[%d bytes]", len(x))
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	}
	return typeName(v)
}

func typeName(v any) string { return fmt.Sprintf("[%T]", v) }
