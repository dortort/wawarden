package main

import (
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	adminstore "github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"

	_ "modernc.org/sqlite"
)

func writeKey(t *testing.T, first byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = first + byte(i)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func auditedArchive(t *testing.T) (archive, key, log, captured string) {
	t.Helper()
	key = writeKey(t, 40)
	master, r := keys.LoadFile(key)
	if r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	dir := t.TempDir()
	var lines bytes.Buffer
	out := logx.NewWriter(&lines)
	out.SetKey(make([]byte, 32))
	s, err := ingest.Open(t.Context(), ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal,
		Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo), Master: master, AuditOut: out})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, _, err := s.Clients().Create(t.Context(), policy.ClientSpec{Name: "agent", AllChats: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := range 3 {
		if err := s.Audit().Record(t.Context(), adminstore.Event{At: time.Now().Add(time.Duration(i) * time.Second), Client: c.ID, Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if _, err := s.Clients().Revoke(t.Context(), c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	captured = "not json\n{\"event\":\"ready\"}\n" + lines.String()
	log = filepath.Join(t.TempDir(), "stdout.log")
	if err := os.WriteFile(log, []byte(captured), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return filepath.Join(dir, "archive.db"), key, log, captured
}

func TestAuditVerify(t *testing.T) {
	archive, key, log, captured := auditedArchive(t)
	before, err := os.ReadFile(archive) //nolint:gosec // G304: the test's own archive
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	code, stdout, stderr := invokeWith(t, []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--log", log}, nil, nil)
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "rows: 5\nverified: 5\nrows failing: 0\nfirst failing row: none\nproblem: none\nchain head: ") ||
		!strings.HasSuffix(stdout, "shipped heads: 5\nshipped heads missing: 0\nfirst missing head: none\n") {
		t.Fatalf("audit verify = %d\n%s\nstderr %q", code, stdout, stderr)
	}
	after, err := os.ReadFile(archive) //nolint:gosec // G304: the test's own archive
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("audit verify changed the copy: %v", err)
	}
	code, _, stderr = invokeWith(t, []string{"audit", "verify", "--db", archive, "--master-key-file", key}, nil, nil)
	if code != 0 || !strings.Contains(stderr, "without --log") {
		t.Fatalf("audit verify without a log = %d, stderr %q, want 0 and the truncation caveat", code, stderr)
	}

	var wrapped strings.Builder
	for line := range strings.Lines(captured) {
		wrapped.WriteString(`{"timestamp":1,"message":` + strconv.Quote(strings.TrimSuffix(line, "\n")) + "}\n")
	}
	enveloped := filepath.Join(t.TempDir(), "enveloped.log")
	if err := os.WriteFile(enveloped, []byte(wrapped.String()), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	code, stdout, stderr = invokeWith(t, []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--log", enveloped}, nil, nil)
	if code != exitUnverified || !strings.Contains(stdout, "rows failing: 0\n") || !strings.Contains(stdout, "shipped heads: 0\n") || !strings.Contains(stderr, "holds no line with a chain_head") {
		t.Fatalf("a log without a chain head = %d\n%s\nstderr %q, want %d and the truncation caveat", code, stdout, stderr, exitUnverified)
	}

	extended := filepath.Join(t.TempDir(), "extended.log")
	if err := os.WriteFile(extended, []byte(captured+`{"ts":"x","client":"aaaaaaaa","action":"messages.read","chat_hmac":null,"ok":true,"reason":"ok","chain_head":"00112233445566778899aabbccddeeff"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	code, stdout, _ = invokeWith(t, []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--log", extended}, nil, nil)
	if code != exitUnverified || !strings.Contains(stdout, "rows failing: 0\n") || !strings.Contains(stdout, "shipped heads missing: 1\nfirst missing head: 00112233445566778899aabbccddeeff\n") {
		t.Fatalf("a head shipped for a row that is gone = %d\n%s", code, stdout)
	}
	code, stdout, _ = invokeWith(t, []string{"audit", "verify", "--db", archive, "--master-key-file", writeKey(t, 90), "--log", log}, nil, nil)
	if code != exitUnverified || !strings.Contains(stdout, "first failing row: 1\nproblem: unknown_key\n") {
		t.Fatalf("another master key = %d\n%s", code, stdout)
	}

	d, err := sql.Open("sqlite", "file:"+archive)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for _, s := range []string{"DROP TRIGGER audit_no_update", "UPDATE audit SET reason = 'denied' WHERE id = 3"} {
		if _, err := d.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	code, stdout, _ = invokeWith(t, []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--log", log}, nil, nil)
	if code != exitUnverified || !strings.Contains(stdout, "rows failing: 1\nfirst failing row: 3\nproblem: hmac_mismatch\n") {
		t.Fatalf("an edited row = %d\n%s", code, stdout)
	}
}

func TestAuditVerifyRefusesALogThatIsAFIFO(t *testing.T) {
	archive, key, _, _ := auditedArchive(t)
	fifo := filepath.Join(t.TempDir(), "stdout.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		stdout, stderr := newOutput(), newOutput()
		code := run(t.Context(), []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--log", fifo}, nil, nil, stdout, stderr)
		done <- result{code, stdout.String(), stderr.String()}
	}()
	select {
	case r := <-done:
		if r.code != exitFailed || r.stdout != "" || !strings.Contains(r.stderr, "the log cannot be read") {
			t.Fatalf("audit verify with a FIFO log = %d %q %q", r.code, r.stdout, r.stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("audit verify blocked opening a FIFO log")
	}
}

func TestAuditVerifyMisuse(t *testing.T) {
	archive, key, _, _ := auditedArchive(t)
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{args: []string{"audit"}, code: exitUsage, want: "usage:"},
		{args: []string{"audit", "check"}, code: exitUsage, want: "usage:"},
		{args: []string{"audit", "verify", "--db", archive}, code: exitUsage, want: "give --db and --master-key-file"},
		{args: []string{"audit", "verify", "--master-key-file", key}, code: exitUsage, want: "give --db and --master-key-file"},
		{args: []string{"audit", "verify", "--db", archive, "--master-key-file", key, "extra"}, code: exitUsage, want: "usage:"},
		{args: []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--bogus"}, code: exitUsage, want: "not a flag of this command"},
		{args: []string{"audit", "verify", "--db", filepath.Join(t.TempDir(), "absent"), "--master-key-file", key}, code: exitFailed, want: "audit verify:"},
		{args: []string{"audit", "verify", "--db", archive, "--master-key-file", filepath.Join(t.TempDir(), "absent")}, code: exitFailed, want: "master key file cannot be opened"},
		{args: []string{"audit", "verify", "--db", archive, "--master-key-file", key, "--log", t.TempDir()}, code: exitFailed, want: "the log cannot be read"},
	} {
		code, stdout, stderr := invokeWith(t, tt.args, nil, nil)
		if code != tt.code || stdout != "" || !strings.Contains(stderr, tt.want) {
			t.Fatalf("run(%q) = %d %q %q, want %d with %q", tt.args, code, stdout, stderr, tt.code, tt.want)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(archive), "absent")); err == nil {
		t.Fatal("verify created a missing copy")
	}
}
