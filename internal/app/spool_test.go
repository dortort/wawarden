package app

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/stage"
)

const strayStaged = "sealed-0123456789abcdef0123456789abcdef"

func strayFiles(t *testing.T, data string) string {
	t.Helper()
	dir := filepath.Join(data, stage.Dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, name := range []string{strayStaged, "synthetic-leftover"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "kept"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	return dir
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func mediaConfig(t *testing.T, maxBytes int64) config.Config {
	t.Helper()
	cfg := testConfig(t, "")
	cfg.MediaMaxBytes, cfg.MediaRefRetentionDays = maxBytes, config.DefaultMediaRefRetentionDays
	return cfg
}

func TestStartRemovesEveryStagedFileWhateverTheCap(t *testing.T) {
	for _, maxBytes := range []int64{0, 1 << 20, 32 << 20} {
		t.Run(strconv.FormatInt(maxBytes, 10)+" bytes", func(t *testing.T) {
			cfg := mediaConfig(t, maxBytes)
			dir := strayFiles(t, cfg.DataDir)
			a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), nil, noClients{}, idle())
			if err != nil {
				t.Fatalf("newAppWith: %v", err)
			}
			t.Cleanup(func() { _ = run(t, a)() })
			if got := names(t, dir); !slices.Equal(got, []string{"kept"}) {
				t.Fatalf("spool/ holds %v after the start, want only the subdirectory kept", got)
			}
		})
	}
}

func TestStartLeavesAnAbsentSpoolAbsent(t *testing.T) {
	for _, maxBytes := range []int64{0, 32 << 20} {
		cfg := mediaConfig(t, maxBytes)
		a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), nil, noClients{}, idle())
		if err != nil {
			t.Fatalf("newAppWith: %v", err)
		}
		if err := run(t, a)(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(cfg.DataDir, stage.Dir)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a start with a cap of %d bytes created spool/: %v", maxBytes, err)
		}
	}
}

func TestAStartThatCannotPurgeTheSpoolFails(t *testing.T) {
	cfg := mediaConfig(t, 0)
	target := t.TempDir()
	kept := filepath.Join(target, strayStaged)
	if err := os.WriteFile(kept, []byte("synthetic"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(cfg.DataDir, stage.Dir)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), nil, noClients{}, idle())
	if err == nil {
		t.Cleanup(func() { _ = run(t, a)() })
		t.Fatal("a start went on with a spool/ it could not purge")
	}
	if strings.Contains(err.Error(), cfg.DataDir) {
		t.Fatalf("the start failure %q names the data directory", err)
	}
	if _, err := os.Lstat(kept); err != nil {
		t.Fatalf("the purge followed spool/ to another directory: %v", err)
	}
	requireReleased(t, cfg.DataDir)
}

func TestARelativeDataDirectoryStartsAndPurges(t *testing.T) {
	cfg := mediaConfig(t, 32<<20)
	dir := strayFiles(t, cfg.DataDir)
	t.Chdir(filepath.Dir(cfg.DataDir))
	cfg.DataDir = filepath.Base(cfg.DataDir)
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), nil, noClients{}, idle())
	if err != nil {
		t.Fatalf("newAppWith with a relative data directory: %v", err)
	}
	if err := run(t, a)(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("spool/ holds %v after the start, want only the subdirectory kept", got)
	}
}
