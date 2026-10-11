package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/app"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/ingest"
)

var rotated = regexp.MustCompile(`^previous key id: ([0-9a-f]{8})\nnew key id: ([0-9a-f]{8})\n$`)

func serveOnce(t *testing.T, data string) *output {
	t.Helper()
	saved := runApp
	t.Cleanup(func() { runApp = saved })
	runApp = func(a *app.App, ctx context.Context) error {
		stopped, stop := context.WithCancel(ctx)
		stop()
		return saved(a, stopped)
	}
	environ := []string{"WAWARDEN_DATA_DIR=" + data, "WAWARDEN_LISTEN=" + freeAddr(t), "WAWARDEN_HEALTH_LISTEN=" + freeAddr(t)}
	stdout := newOutput()
	if code := run(t.Context(), []string{"serve", "--allow-root"}, environ, nil, stdout, newOutput()); code != 0 {
		t.Fatalf("serve = %d, want 0; stdout %q", code, stdout.String())
	}
	return stdout
}

func secretOf(t *testing.T, data string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(data, "keys", "master")) //nolint:gosec // G304: the test's own data directory
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

func TestKeysRotateRetiresTheMasterOnlyWhileTheServiceIsStopped(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	serveOnce(t, data)
	first := secretOf(t, data)
	before, r := keys.Load(data, os.Geteuid())
	if r != nil {
		t.Fatalf("Load: %v", r)
	}

	held, err := ingest.Open(t.Context(), ingest.Options{DataDir: data, UID: os.Geteuid(), Profile: ingest.ProfileLocal,
		Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo), Master: before})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	code, stdout, stderr := invokeWith(t, []string{"keys", "rotate", "--data-dir", data}, nil, nil)
	if code != exitFailed || stdout != "" || !strings.Contains(stderr, "keys rotate: keys: the master key is replaced only while the service is stopped") ||
		!strings.Contains(stderr, "locked by another process") || strings.Contains(stderr, data) {
		t.Fatalf("keys rotate with the archive held = %d %q %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(filepath.Join(data, "keys", "retired")); err == nil || !bytes.Equal(secretOf(t, data), first) {
		t.Fatal("a refused rotation changed the keys directory")
	}
	if err := held.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	code, stdout, stderr = invokeWith(t, []string{"keys", "rotate", "--data-dir", data}, nil, nil)
	m := rotated.FindStringSubmatch(stdout)
	if code != 0 || stderr != "" || m == nil || m[1] != before.ID() || m[2] == m[1] {
		t.Fatalf("keys rotate = %d %q %q, want both key ids", code, stdout, stderr)
	}
	second := secretOf(t, data)
	retired, err := os.ReadFile(filepath.Join(data, "keys", "retired", m[1])) //nolint:gosec // G304: the test's own data directory
	if err != nil || !bytes.Equal(retired, first) {
		t.Fatalf("keys/retired/%s does not hold the previous key: %v", m[1], err)
	}
	for _, secret := range [][]byte{first, second, before.AuditChainKey(), before.LogRedactKey()} {
		for _, form := range []string{hex.EncodeToString(secret), string(secret)} {
			if strings.Contains(stdout+stderr, form) {
				t.Fatal("keys rotate printed key material")
			}
		}
	}

	out := serveOnce(t, data)
	loaded := out.waitFor(t, "keys_loaded")
	if loaded["key_id"] != m[2] || loaded["retired"] != float64(1) {
		t.Fatalf("the start after the rotation logged %v, want key id %s and one retired key", loaded, m[2])
	}
}

func TestKeysRotateMisuse(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{args: []string{"keys"}, code: exitUsage, want: "usage:"},
		{args: []string{"keys", "show"}, code: exitUsage, want: "usage:"},
		{args: []string{"keys", "rotate"}, code: exitUsage, want: "give --data-dir"},
		{args: []string{"keys", "rotate", "--data-dir", ""}, code: exitUsage, want: "give --data-dir"},
		{args: []string{"keys", "rotate", "--data-dir", data, "extra"}, code: exitUsage, want: "give --data-dir"},
		{args: []string{"keys", "rotate", "--data-dir"}, code: exitUsage, want: "--data-dir is not given as it must be"},
		{args: []string{"keys", "rotate", "--bogus"}, code: exitUsage, want: "not a flag of this command"},
		{args: []string{"keys", "rotate", "--data-dir", data}, code: exitFailed, want: "keys rotate: keys: keys/ cannot be inspected"},
	} {
		code, stdout, stderr := invokeWith(t, tt.args, nil, nil)
		if code != tt.code || stdout != "" || !strings.Contains(stderr, tt.want) || strings.Contains(stderr, data) {
			t.Fatalf("run(%q) = %d %q %q, want %d with %q", tt.args, code, stdout, stderr, tt.code, tt.want)
		}
	}
	if _, err := os.Lstat(data); err == nil {
		t.Fatal("keys rotate created a missing data directory")
	}
}
