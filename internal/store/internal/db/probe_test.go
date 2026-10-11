package db

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func listing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestUnlockedTellsAHeldArchiveFromAFreeOne(t *testing.T) {
	opts, _ := testOptions(t)
	if err := Unlocked(t.Context(), opts.DataDir, Archive); err != nil {
		t.Fatalf("Unlocked without archive.db = %v, want nil", err)
	}
	if got := listing(t, opts.DataDir); len(got) != 0 {
		t.Fatalf("Unlocked created %q", got)
	}
	d := mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	err := Unlocked(t.Context(), opts.DataDir, Archive)
	if err == nil || err.Error() != "db: archive.db is locked by another process" {
		t.Fatalf("Unlocked while a handle holds archive.db = %v, want the locked error", err)
	}
	if strings.Contains(err.Error(), opts.DataDir) {
		t.Fatalf("the error %q names the data directory", err)
	}
	if got := probeFromAnotherProcess(t, filepath.Join(opts.DataDir, Archive.file())); got != probeBusy {
		t.Fatalf("after Unlocked another process probes %d, want busy: the probe released the holder's lock", got)
	}
	exec1(t, d, "INSERT INTO t VALUES (1)")
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(opts.DataDir, Archive.file())
	before, err := os.ReadFile(path) //nolint:gosec // G304: the test's own database file
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	files := listing(t, opts.DataDir)
	if err := Unlocked(t.Context(), opts.DataDir, Archive); err != nil {
		t.Fatalf("Unlocked after Close = %v, want nil", err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // G304: the test's own database file
	if err != nil || !bytes.Equal(after, before) || !slices.Equal(listing(t, opts.DataDir), files) {
		t.Fatalf("Unlocked changed the data directory: %v", err)
	}
}

func TestUnlockedRefusesWhatItCannotCheck(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, Archive.file()), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := Unlocked(t.Context(), dir, Archive); err == nil || err.Error() != "db: archive.db is not a regular file" {
		t.Fatalf("Unlocked of a directory = %v", err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, Session.file()), []byte("not a database, and long enough to be read as one"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Unlocked(t.Context(), other, Session); err == nil || err.Error() != "db: session.db cannot be read to check its lock" {
		t.Fatalf("Unlocked of a file that is no database = %v", err)
	}
	if err := Unlocked(t.Context(), other, Name(9)); err == nil {
		t.Fatal("Unlocked of an unknown database succeeded")
	}
}
