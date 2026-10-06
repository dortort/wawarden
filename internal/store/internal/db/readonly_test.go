package db

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestOpenCopyReadsWithoutWriting(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	exec1(t, d, "INSERT INTO t VALUES (7)")
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(opts.DataDir, Archive.file())
	before, err := os.ReadFile(path) //nolint:gosec // G304: the test's own database file
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	listing := func() []string {
		entries, err := os.ReadDir(opts.DataDir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	files := listing()
	c, err := OpenCopy(t.Context(), path, time.Minute)
	if err != nil {
		t.Fatalf("OpenCopy: %v", err)
	}
	var x int
	if err := c.Read(t.Context(), "test.read", func(ctx context.Context, q Querier) error {
		return q.QueryRowContext(ctx, "SELECT x FROM t").Scan(&x)
	}); err != nil || x != 7 {
		t.Fatalf("Read = %d, %v, want 7", x, err)
	}
	if err := c.Write(t.Context(), "test.write", func(ctx context.Context, q Querier) error {
		_, err := q.ExecContext(ctx, "INSERT INTO t VALUES (8)")
		return err
	}); err == nil {
		t.Fatal("a write through a copy opened read-only succeeded")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // G304: the test's own database file
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("opening the copy changed the file")
	}
	if got := listing(); !slices.Equal(got, files) {
		t.Fatalf("the copy's directory held %q before OpenCopy and %q after", files, got)
	}
}

func TestOpenCopyRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{"missing": filepath.Join(dir, "absent"), "directory": dir} {
		if c, err := OpenCopy(t.Context(), path, time.Minute); err == nil || c != nil {
			t.Fatalf("OpenCopy(%s) = %v, %v, want a refusal", name, c, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("OpenCopy created the missing file")
	}
}
