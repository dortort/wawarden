package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
)

const (
	probeEnv    = "DBTEST_PROBE"
	probeFree   = 0
	probeFailed = 2
	probeBusy   = 3
)

func TestMain(m *testing.M) {
	if path := os.Getenv(probeEnv); path != "" {
		os.Exit(probe(path))
	}
	os.Exit(m.Run())
}

func probe(path string) int {
	other, err := sql.Open("sqlite", fileURI(path, []string{"busy_timeout(0)"}))
	if err != nil {
		return probeFailed
	}
	defer func() { _ = other.Close() }()
	var n int
	err = other.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_schema").Scan(&n)
	switch {
	case err == nil:
		return probeFree
	case busy(err):
		return probeBusy
	}
	return probeFailed
}

func probeFromAnotherProcess(t *testing.T, path string) int {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^$") //nolint:gosec // G204: the command is the running test binary, from os.Executable
	cmd.Env = append(os.Environ(), probeEnv+"="+path)
	err = cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("run the probe process: %v", err)
	}
	return probeFree
}

func probeFromThisProcess(t *testing.T, path string) int {
	t.Helper()
	return probe(path)
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) events(name string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.buf.String()) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == name {
			out = append(out, rec)
		}
	}
	return out
}

func newLogger() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	w := logx.NewWriter(buf)
	w.SetKey(make([]byte, 32))
	return logx.New(w, slog.LevelDebug), buf
}

func testOptions(t *testing.T) (Options, *logBuffer) {
	t.Helper()
	logger, logs := newLogger()
	return Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: Local, Logger: logger}, logs
}

func mustOpen(t *testing.T, opts Options) *DB {
	t.Helper()
	d, err := Open(t.Context(), Archive, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func exec1(t *testing.T, d *DB, query string, args ...any) {
	t.Helper()
	if err := d.Write(t.Context(), "test.exec", func(ctx context.Context, q Querier) error {
		_, err := q.ExecContext(ctx, query, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func count(t *testing.T, d *DB, query string) int {
	t.Helper()
	var n int
	if err := d.Read(t.Context(), "test.count", func(ctx context.Context, q Querier) error {
		return q.QueryRowContext(ctx, query).Scan(&n)
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

const slowQuery = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 2000000000) SELECT count(*) FROM c"

func shortTimeouts(o *Options) {
	o.busyTimeout = 100 * time.Millisecond
	o.retryBase = 10 * time.Millisecond
}
