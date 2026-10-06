package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T, d *DB, rows int) {
	t.Helper()
	exec1(t, d, "CREATE TABLE t(seq INTEGER PRIMARY KEY, body BLOB NOT NULL) STRICT")
	for left := rows; left > 0; left -= seedBatch {
		exec1(t, d, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?) INSERT INTO t(body) SELECT randomblob(2000) FROM c", min(left, seedBatch))
	}
}

const seedBatch = 100

func into(w io.Writer) func(string, int64, io.Reader) error {
	return func(_ string, _ int64, r io.Reader) error {
		_, err := io.Copy(w, r)
		return err
	}
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
	var name string
	var size int64
	if err := d.Backup(t.Context(), staging, func(n string, s int64, r io.Reader) error {
		name, size = n, s
		_, err := io.Copy(&out, r)
		return err
	}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if name != "archive.db" || size != int64(out.Len()) || size == 0 {
		t.Fatalf("the copy was handed over as %q of %d bytes, and %d bytes were read: want archive.db and its exact size", name, size, out.Len())
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

func TestBackupStepsAtTheDefaultSize(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	seed(t, d, 1500)
	pages := count(t, d, "PRAGMA page_count")
	if pages <= 2*backupPagesPerStep {
		t.Fatalf("the database has %d pages, want more than two steps' worth, or this test proves nothing", pages)
	}
	steps := 0
	d.stepped = func() { steps++ }
	var out bytes.Buffer
	if err := d.Backup(t.Context(), filepath.Join(t.TempDir(), "staging"), into(&out)); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if want := (pages - 1) / backupPagesPerStep; steps != want {
		t.Fatalf("the backup of %d pages paused %d times between steps, want %d: one every %d pages", pages, steps, want, backupPagesPerStep)
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
	if err := d.Backup(t.Context(), staging, into(&out)); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if mode != 0o600 {
		t.Fatalf("the staging copy has mode %v, want 0600", mode)
	}
	if len(siblings) != 1 || !siblings["staging"] {
		t.Fatalf("the staging directory held %v during the backup, want only the staging copy and no journal", siblings)
	}
	writeFile(t, staging, 0o600)
	if err := d.Backup(t.Context(), staging, into(&out)); err == nil || !strings.Contains(err.Error(), "staging") {
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
	if err := d.Backup(ctx, staging, into(&out)); !errors.Is(err, context.Canceled) {
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

type panickingBackup struct{ inStart, inStep, inFinish bool }

func (b panickingBackup) Step(int32) (bool, error) {
	if b.inStep {
		panic("synthetic step panic")
	}
	return false, nil
}

func (b panickingBackup) Finish() error {
	if b.inFinish {
		panic("synthetic finish panic")
	}
	return nil
}

func TestBackupRecoversAPanic(t *testing.T) {
	for _, tt := range []struct {
		name string
		b    panickingBackup
	}{
		{"in a step", panickingBackup{inStep: true}},
		{"in finish", panickingBackup{inFinish: true}},
		{"in start", panickingBackup{inStart: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts, _ := testOptions(t)
			d := mustOpen(t, opts)
			seed(t, d, 10)
			d.newBackup = func(*keptConn, string) (stepper, error) {
				if tt.b.inStart {
					panic("synthetic start panic")
				}
				return tt.b, nil
			}
			staging := filepath.Join(t.TempDir(), "staging")
			var out bytes.Buffer
			if err := d.Backup(t.Context(), staging, into(&out)); !errors.Is(err, errBackupPanicked) {
				t.Fatalf("Backup = %v, want errBackupPanicked", err)
			}
			if out.Len() != 0 {
				t.Fatal("a backup that panicked wrote a copy")
			}
			if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the staging copy is left behind: %v", err)
			}
			if !d.Healthy() || d.sql.Stats().OpenConnections != 1 {
				t.Fatal("a backup that panicked changed the database's connection")
			}
			if got := probeFromAnotherProcess(t, filepath.Join(opts.DataDir, "archive.db")); got != probeBusy {
				t.Fatalf("another process after a backup that panicked = %d, want busy", got)
			}
			if n := count(t, d, "SELECT count(*) FROM t"); n != 10 {
				t.Fatalf("%d rows after a backup that panicked, want 10", n)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }

func TestBackupReportsAFailedWrite(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	seed(t, d, 10)
	staging := filepath.Join(t.TempDir(), "staging")
	if err := d.Backup(t.Context(), staging, into(failingWriter{})); err == nil {
		t.Fatal("Backup hid a failed write")
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the staging copy is left behind: %v", err)
	}
}
