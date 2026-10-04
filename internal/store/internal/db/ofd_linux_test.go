package db

import (
	"bufio"
	"io"
	"os"
	"os/exec"
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

func TestNFSProfileUsesClassicLocks(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	holder := exec.CommandContext(t.Context(), executable, "-test.run=^$") //nolint:gosec // G204: the command is the running test binary, from os.Executable
	holder.Env = append(os.Environ(), holdNFSEnv+"="+dir)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := holder.Start(); err != nil {
		t.Fatalf("start the holder process: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = holder.Wait()
	})
	lines := bufio.NewScanner(stdout)
	next := func() string {
		t.Helper()
		if !lines.Scan() {
			_ = stdin.Close()
			t.Fatalf("the holder process stopped early: %v", holder.Wait())
		}
		return lines.Text()
	}
	if got := next(); got != "open ofd=false local=false" {
		t.Fatalf("the holder reports %q, want classic locks and the local profile's lock kind refused", got)
	}
	path := filepath.Join(dir, "archive.db")
	if got := probeFromAnotherProcess(t, path); got != probeBusy {
		t.Fatalf("another process probing a database held with profile nfs = %d, want busy", got)
	}
	if _, err := io.WriteString(stdin, "stray\n"); err != nil {
		t.Fatalf("signal the holder: %v", err)
	}
	if got := next(); got != "stray closed" {
		t.Fatalf("the holder reports %q", got)
	}
	if got := probeFromAnotherProcess(t, path); got != probeFree {
		t.Fatalf("another process after a stray close in the holder = %d, want free: profile nfs uses classic locks, which any close drops", got)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("the holder process: %v", err)
	}
}

func TestOneLockKindPerProcess(t *testing.T) {
	opts, _ := testOptions(t)
	held := mustOpen(t, opts)
	if err := selectLocking(Local); err != nil {
		t.Fatalf("selectLocking(local) with open-file-description locks in effect = %v", err)
	}
	if err := selectLocking(NFS); err == nil {
		t.Fatal("selectLocking(nfs) accepted classic locks in a process that already locks with open-file-description locks")
	}
	nfs := opts
	nfs.DataDir, nfs.Profile = t.TempDir(), NFS
	d, err := Open(t.Context(), Archive, nfs)
	if err == nil {
		_ = d.Close()
		t.Fatal("Open accepted profile nfs in a process that already locks with open-file-description locks")
	}
	if !held.OFDLocking() {
		t.Fatal("a refused Open changed the lock kind")
	}
}
