package wa

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/dortort/wawarden/internal/logx"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) events(name string) []map[string]any {
	var out []map[string]any
	for line := range strings.Lines(l.String()) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == name {
			out = append(out, rec)
		}
	}
	return out
}

func newLogger(level slog.Level) (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	w := logx.NewWriter(buf)
	w.SetKey(bytes.Repeat([]byte{7}, 32))
	return logx.New(w, level), buf
}
