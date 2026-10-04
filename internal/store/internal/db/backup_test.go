package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T, d *DB, rows int) {
	t.Helper()
	exec1(t, d, "CREATE TABLE t(seq INTEGER PRIMARY KEY, body BLOB NOT NULL) STRICT")
	exec1(t, d, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?) INSERT INTO t(body) SELECT randomblob(2000) FROM c", rows)
}

func openCopy(t *testing.T, data []byte) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	copied, err := sql.Open("sqlite", fileURI(path, nil))
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	t.Cleanup(func() { _ = copied.Close() })
	var check string
	if err := copied.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check of the copy = %q, %v", check, err)
	}
	return copied
}

func TestBackupCopiesInStepsWhileWritesContinue(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	d.backupPages = 4
	seed(t, d, 40)
	steps := 0
	d.stepped = func() {
		steps++
		exec1(t, d, "INSERT INTO t(body) VALUES (randomblob(10))")
	}
	staging := filepath.Join(t.TempDir(), "staging")
	var out bytes.Buffer
	if err := d.Backup(t.Context(), staging, &out); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if steps < 2 {
		t.Fatalf("the backup paused %d times between steps, want several, or this test proves nothing", steps)
	}
	want := count(t, d, "SELECT count(*) FROM t")
	var got int
	if err := openCopy(t, out.Bytes()).QueryRowContext(t.Context(), "SELECT count(*) FROM t").Scan(&got); err != nil || got != want {
		t.Fatalf("the copy holds %d rows (%v), want %d: every write made between steps", got, err, want)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the staging copy is left behind: %v", err)
	}
	if !d.Healthy() || d.sql.Stats().OpenConnections != 1 {
		t.Fatal("the backup changed the database's connection")
	}
}

func TestBackupStagesPrivatelyAndNeverOverwrites(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	d.backupPages = 4
	seed(t, d, 40)
	staging := filepath.Join(t.TempDir(), "staging")
	var mode fs.FileMode
	siblings := map[string]bool{}
	d.stepped = func() {
		fi, err := os.Lstat(staging)
		if err != nil {
			t.Errorf("Lstat the staging copy: %v", err)
			return
		}
		mode = fi.Mode()
		entries, _ := os.ReadDir(filepath.Dir(staging))
		for _, e := range entries {
			siblings[e.Name()] = true
		}
	}
	var out bytes.Buffer
	if err := d.Backup(t.Context(), staging, &out); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if mode != 0o600 {
		t.Fatalf("the staging copy has mode %v, want 0600", mode)
	}
	if len(siblings) != 1 || !siblings["staging"] {
		t.Fatalf("the staging directory held %v during the backup, want only the staging copy and no journal", siblings)
	}
	writeFile(t, staging, 0o600)
	if err := d.Backup(t.Context(), staging, &out); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("Backup over an existing file = %v, want a refusal", err)
	}
	if _, err := os.Lstat(staging); err != nil {
		t.Fatalf("a refused backup removed a file it did not create: %v", err)
	}
}

func TestBackupStopsWhenCancelled(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	d.backupPages = 4
	seed(t, d, 40)
	ctx, cancel := context.WithCancel(t.Context())
	d.stepped = cancel
	staging := filepath.Join(t.TempDir(), "staging")
	var out bytes.Buffer
	if err := d.Backup(ctx, staging, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("Backup = %v, want the cancellation", err)
	}
	if out.Len() != 0 {
		t.Fatal("a cancelled backup wrote a partial copy")
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the staging copy is left behind: %v", err)
	}
	if !d.Healthy() || count(t, d, "SELECT count(*) FROM t") != 40 {
		t.Fatal("a cancelled backup disturbed the database")
	}
}

func TestBackupStepsRecoverPanics(t *testing.T) {
	if err := safely(func() error { panic("synthetic panic") }); !errors.Is(err, errBackupPanicked) {
		t.Fatalf("safely = %v, want errBackupPanicked", err)
	}
	failure := errors.New("synthetic failure")
	if err := safely(func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("safely = %v, want the step's own error", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }

func TestBackupReportsAFailedWrite(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	seed(t, d, 10)
	staging := filepath.Join(t.TempDir(), "staging")
	if err := d.Backup(t.Context(), staging, failingWriter{}); err == nil {
		t.Fatal("Backup hid a failed write")
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the staging copy is left behind: %v", err)
	}
}
