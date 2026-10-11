package main

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/dortort/wawarden/internal/backup"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	adminstore "github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/inspect"
	"github.com/dortort/wawarden/internal/store/session"
)

const verifyCanary = "15550100077"

type quietBackups struct{}

func (quietBackups) BackupDone(int64, int64, int64, time.Duration) {}
func (quietBackups) BackupFailed(string)                           {}
func (quietBackups) BackupPruned(int, int)                         {}
func (quietBackups) BackupPruneFailed(string)                      {}

func decryptedBackup(t *testing.T) (plaintext []byte, key string) {
	t.Helper()
	key = writeKey(t, 60)
	master, refusal := keys.LoadFile(key)
	if refusal != nil {
		t.Fatalf("LoadFile: %v", refusal)
	}
	dir := t.TempDir()
	logger := logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)
	out := logx.NewWriter(io.Discard)
	out.SetKey(make([]byte, 32))
	archive, err := ingest.Open(t.Context(), ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger, Master: master, AuditOut: out})
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}
	defer func() { _ = archive.Close() }()
	alice, ok := policy.Normalize(verifyCanary + "@s.whatsapp.net")
	if !ok {
		t.Fatal("Normalize")
	}
	if err := archive.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		if _, _, err := tx.InsertMessage(ingest.Message{Chat: alice, ID: "M1", Sender: alice, Origin: ingest.OriginLive, Timestamp: time.Now(),
			Kind: ingest.KindText, Text: "synthetic secret words", Ingested: time.Now()}); err != nil {
			return err
		}
		return tx.SetPushName(alice, "Alice Example", time.Now(), ingest.OriginLive)
	}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	c, _, err := archive.Clients().Create(t.Context(), policy.ClientSpec{Name: "agent", AllChats: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := archive.Audit().Record(t.Context(), adminstore.Event{At: time.Now(), Client: c.ID, Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	sess, err := session.Open(t.Context(), session.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer func() { _ = sess.Close() }()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	tk, err := backup.New(backup.Options{DataDir: dir, UID: os.Geteuid(), Recipient: id.Recipient().String(), Version: "v1.0.0-test",
		Archive: archive, Session: sess, Notify: quietBackups{}})
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}
	name, err := tk.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, backup.Dir, name)) //nolint:gosec // G304: the test's own backup
	if err != nil {
		t.Fatalf("open the backup: %v", err)
	}
	defer func() { _ = f.Close() }()
	r, err := age.Decrypt(f, id)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plaintext, err = io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return plaintext, key
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func leftIn(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func withManifest(t *testing.T, plaintext []byte, change func(m map[string]any)) []byte {
	t.Helper()
	var out bytes.Buffer
	tr, tw := tar.NewReader(bytes.NewReader(plaintext)), tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if h.Name == "manifest.json" {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			change(m)
			if body, err = json.Marshal(m); err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

func withArchiveVersion(t *testing.T, plaintext []byte, version int) []byte {
	t.Helper()
	patched := slices.Clone(plaintext)
	at := bytes.Index(patched, []byte("SQLite format 3\x00"))
	if at < 0 {
		t.Fatal("the backup holds no database")
	}
	binary.BigEndian.PutUint32(patched[at+60:], uint32(version)) //nolint:gosec // G115: a small schema version
	return withManifest(t, patched, func(m map[string]any) {
		m["databases"].([]any)[0].(map[string]any)["schema_version"] = version
	})
}

func TestBackupVerify(t *testing.T) {
	plaintext, key := decryptedBackup(t)
	work := privateDir(t)
	keep := filepath.Join(t.TempDir(), "restored")
	code, stdout, stderr := invokeWith(t, []string{"backup", "verify", "--work-dir", work, "--extract-to", keep, "-"}, nil, bytes.NewReader(plaintext))
	if code != 0 || stderr != "" {
		t.Fatalf("backup verify = %d\n%s\nstderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"format: 1\n", "written by: v1.0.0-test\n", "archive integrity: ok\n", "archive schema version: 4\n", "archive schema: ok\n",
		"archive chats: 1\n", "archive messages: 1\n", "archive history blobs pending: 0\n", "archive active clients: 1\n", "archive audit rows: 2\n",
		"device store integrity: ok\n", "device store schema: ok\n", "device store paired: no\n", "device store pre-keys: 0\n", "device store sessions: 0\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("backup verify lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.HasSuffix(stdout, "verified: yes\n") {
		t.Fatalf("backup verify does not end with its verdict:\n%s", stdout)
	}
	for _, leak := range []string{verifyCanary, "Alice", "synthetic secret", "agent"} {
		if strings.Contains(stdout+stderr, leak) {
			t.Fatalf("backup verify printed %q:\n%s", leak, stdout)
		}
	}
	if left := leftIn(t, work); len(left) != 0 {
		t.Fatalf("the work directory holds %v", left)
	}
	if got := leftIn(t, keep); !slices.Equal(got, []string{"archive.db", "session.db"}) {
		t.Fatalf("--extract-to holds %v", got)
	}
	if fi, err := os.Lstat(keep); err != nil || fi.Mode() != fs.ModeDir|0o700 {
		t.Fatalf("--extract-to has mode %v, %v", fi.Mode(), err)
	}
	code, stdout, stderr = invokeWith(t, []string{"audit", "verify", "--db", filepath.Join(keep, "archive.db"), "--master-key-file", key}, nil, nil)
	if code != 0 || !strings.Contains(stdout, "rows failing: 0\n") {
		t.Fatalf("audit verify of the restored archive = %d\n%s\n%s", code, stdout, stderr)
	}
	code, _, stderr = invokeWith(t, []string{"backup", "verify", "--work-dir", work, "--extract-to", keep, "-"}, nil, bytes.NewReader(plaintext))
	if code != exitFailed || !strings.Contains(stderr, "--extract-to") {
		t.Fatalf("backup verify into an existing directory = %d %q", code, stderr)
	}
}

func TestBackupVerifyExitCodes(t *testing.T) {
	plaintext, _ := decryptedBackup(t)
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil { //nolint:gosec // G301: a work directory others can enter, which the command refuses
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil { //nolint:gosec // G302: a work directory others can enter, which the command refuses
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		stream  []byte
		work    string
		code    int
		verdict string
		stderr  string
	}{
		{name: "an empty stream, as from a wrong identity", stream: nil, code: exitFailed, stderr: "could not be read"},
		{name: "a truncated stream", stream: plaintext[:len(plaintext)/2], code: exitFailed, stderr: "could not be read"},
		{name: "data after the archive", stream: append(slices.Clone(plaintext), "synthetic"...), code: exitUnverified, stderr: "not a complete backup"},
		{name: "a manifest with another archive version", stream: withManifest(t, plaintext, func(m map[string]any) {
			m["databases"].([]any)[0].(map[string]any)["schema_version"] = 3
		}), code: exitUnverified, verdict: "archive schema: differs from the manifest's 3\n"},
		{name: "a manifest with another device store version", stream: withManifest(t, plaintext, func(m map[string]any) {
			m["databases"].([]any)[1].(map[string]any)["schema_version"] = 1
		}), code: exitUnverified, verdict: "device store schema: differs from the manifest's 1\n"},
		{name: "an archive newer than this build", stream: withArchiveVersion(t, plaintext, inspect.LatestArchiveVersion()+1),
			code: exitUnverified, verdict: "archive schema: newer than this build, which reads up to " + strconv.Itoa(inspect.LatestArchiveVersion()) + "\n"},
		{name: "a work directory open to others", stream: plaintext, work: open, code: exitFailed, stderr: "--work-dir"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			work := tt.work
			if work == "" {
				work = privateDir(t)
			}
			code, stdout, stderr := invokeWith(t, []string{"backup", "verify", "--work-dir", work, "-"}, nil, bytes.NewReader(tt.stream))
			if code != tt.code || !strings.Contains(stderr, tt.stderr) || !strings.Contains(stdout, tt.verdict) {
				t.Fatalf("backup verify = %d\n%s\nstderr %q", code, stdout, stderr)
			}
			if tt.verdict != "" && !strings.HasSuffix(stdout, "verified: no\n") {
				t.Fatalf("a failed check ends with:\n%s", stdout)
			}
			if left := leftIn(t, work); len(left) != 0 {
				t.Fatalf("the work directory holds %v", left)
			}
		})
	}
}

func TestBackupVerifyPrintsNoForeignVersionOrControlCharacter(t *testing.T) {
	plaintext, _ := decryptedBackup(t)
	stream := withManifest(t, plaintext, func(m map[string]any) { m["version"] = "v1.0.0\x1b]0;synthetic\x07" })
	code, stdout, stderr := invokeWith(t, []string{"backup", "verify", "--work-dir", privateDir(t), "-"}, nil, bytes.NewReader(stream))
	if code != 0 || stderr != "" || !strings.Contains(stdout, "written by: unrecognised\n") || strings.ContainsAny(stdout, "\x1b\x07") {
		t.Fatalf("backup verify of a manifest with a control character in its version = %d\n%q\nstderr %q", code, stdout, stderr)
	}
	var out bytes.Buffer
	failed := inspect.Integrity{Problems: []string{"page 2: \x1b[2Jsynthetic\x07"}}
	if report(&out, backup.Extracted{}, inspect.Archive{Integrity: failed}, inspect.Device{Integrity: failed}) {
		t.Fatal("a failed integrity check was reported as verified")
	}
	if !strings.Contains(out.String(), "archive integrity problem: page 2: ") || strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("report printed %q", out.String())
	}
}

func TestBackupVerifyUsageErrors(t *testing.T) {
	work := privateDir(t)
	for _, args := range [][]string{
		{"backup"},
		{"backup", "check"},
		{"backup", "verify"},
		{"backup", "verify", "-"},
		{"backup", "verify", "--work-dir", work},
		{"backup", "verify", "--work-dir", "", "-"},
		{"backup", "verify", "--work-dir", work, "backup.tar"},
		{"backup", "verify", "--work-dir", work, "-", "-"},
		{"backup", "verify", "--work-dir", work, "--identity", "x", "-"},
	} {
		code, stdout, stderr := invokeWith(t, args, nil, strings.NewReader("synthetic"))
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, "usage:") {
			t.Fatalf("%q = %d %q %q", args, code, stdout, stderr)
		}
	}
	if left := leftIn(t, work); len(left) != 0 {
		t.Fatalf("a usage error touched the work directory: %v", left)
	}
}
