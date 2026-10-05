package wa

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	read := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	saved := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = saved
		_ = w.Close()
	}()
	fn()
	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatalf("close the pipe: %v", err)
	}
	return <-read
}
