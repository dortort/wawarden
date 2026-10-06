package db

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
)

const (
	probeEnv    = "DBTEST_PROBE"
	holdNFSEnv  = "DBTEST_HOLD_NFS"
	probeFree   = 0
	probeFailed = 2
	probeBusy   = 3
)

func TestMain(m *testing.M) {
	if path := os.Getenv(probeEnv); path != "" {
		os.Exit(probe(path))
	}
	if dir := os.Getenv(holdNFSEnv); dir != "" {
		os.Exit(holdWithProfileNFS(dir))
	}
	os.Exit(m.Run())
}

func holdWithProfileNFS(dir string) int {
	logger, _ := newLogger()
	d, err := Open(context.Background(), Archive, Options{DataDir: dir, UID: os.Geteuid(), Profile: NFS, Logger: logger})
	if err != nil {
		return probeFailed
	}
	defer func() { _ = d.Close() }()
	in := bufio.NewScanner(os.Stdin)
	if _, err := fmt.Fprintf(os.Stdout, "open ofd=%t local=%t\n", d.OFDLocking(), selectLocking(Local) == nil); err != nil || !in.Scan() {
		return probeFailed
	}
	stray, err := os.Open(filepath.Join(dir, "archive.db")) //nolint:gosec // G304: the test's own database file
	if err != nil || stray.Close() != nil {
		return probeFailed
	}
	if _, err := fmt.Fprintln(os.Stdout, "stray closed"); err != nil {
		return probeFailed
	}
	in.Scan()
	return probeFree
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

const (
	slowQuery      = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < " + slowRows + ") SELECT count(*) FROM c"
	interruptBound = 3 * time.Second
)

func interrupted(t *testing.T, call func() error) error {
	t.Helper()
	start := time.Now()
	err := call()
	if elapsed := time.Since(start); elapsed > interruptBound {
		t.Fatalf("the slow call returned after %v: its interrupt was lost, as when the deadline fires between the driver arming the interrupt and the statement's first step, which clears it", elapsed)
	}
	return err
}

func shortTimeouts(o *Options) {
	o.busyTimeout = 100 * time.Millisecond
	o.retryBase = 10 * time.Millisecond
}
