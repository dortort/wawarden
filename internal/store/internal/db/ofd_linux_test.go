package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalProfileKeepsTheLockAcrossAStrayClose(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	if !d.OFDLocking() {
		t.Fatal("storage profile local did not enable open-file-description locks")
	}
	path := filepath.Join(opts.DataDir, "archive.db")
	stray, err := os.Open(path) //nolint:gosec // G304: the test's own database file
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := stray.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := probeFromAnotherProcess(t, path); got != probeBusy {
		t.Fatalf("another process after a stray close of the database file = %d, want busy", got)
	}
}
