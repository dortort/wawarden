package app

import (
	"context"
	"errors"
	"fmt"
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
			if (a.spool != nil) != (maxBytes > 0) {
				t.Fatalf("spool = %v with a cap of %d bytes", a.spool, maxBytes)
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

func sweepOnce(t *testing.T, a *App) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.sweepOn(ctx, tick)
	}()
	tick <- time.Now()
	cancel()
	<-done
}

func TestTheSweepRemovesStrayStagedFilesButNotTheLiveOne(t *testing.T) {
	data := t.TempDir()
	spool, err := stage.New(stage.Options{Data: data, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	_, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	live, err := spool.Admit(1024, cancel)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer live.Erase()
	dir := filepath.Join(data, stage.Dir)
	held := names(t, dir)
	if len(held) != 1 || !strings.HasPrefix(held[0], "sealed-") {
		t.Fatalf("spool/ holds %v while a job stages on disk, want one staged file", held)
	}
	for _, name := range []string{strayStaged, "synthetic-notes"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	logs := &syncBuffer{}
	a := &App{logger: logx.New(logx.NewWriter(logs), slog.LevelInfo), spool: spool}
	sweepOnce(t, a)
	if got, want := names(t, dir), slices.Sorted(slices.Values([]string{held[0], "synthetic-notes"})); !slices.Equal(got, want) {
		t.Fatalf("spool/ holds %v after the sweep, want %v", got, want)
	}
	if failed := logs.find("spool_sweep_failed"); len(failed) != 0 {
		t.Fatalf("spool_sweep_failed events %v", failed)
	}
}

func TestAFailedSweepIsLoggedWithoutAPath(t *testing.T) {
	data := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(data, stage.Dir)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	spool, err := stage.New(stage.Options{Data: data, Local: true, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	logs := &syncBuffer{}
	a := &App{logger: logx.New(logx.NewWriter(logs), slog.LevelInfo), spool: spool}
	sweepOnce(t, a)
	failed := logs.find("spool_sweep_failed")
	if len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["error_type"] == "" {
		t.Fatalf("spool_sweep_failed events %v, want one warning with an error type", failed)
	}
	if strings.Contains(fmt.Sprint(logs.events()), data) {
		t.Fatal("the sweep failure names the data directory")
	}
}

func TestRunSweepsTheSpoolOnItsIntervalAndStops(t *testing.T) {
	cfg := mediaConfig(t, 32<<20)
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), nil, noClients{}, idle())
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	if a.spool == nil || a.sweepEvery != time.Hour {
		t.Fatalf("spool %v swept every %v, want a spool swept hourly for a cap above 16 MiB", a.spool, a.sweepEvery)
	}
	a.sweepEvery = time.Millisecond
	dir := strayFiles(t, cfg.DataDir)
	stop := run(t, a)
	waitUntil(t, "the stray staged file swept", func() bool {
		_, err := os.Lstat(filepath.Join(dir, strayStaged))
		return errors.Is(err, fs.ErrNotExist)
	})
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"kept", "synthetic-leftover"}) {
		t.Fatalf("spool/ holds %v after the sweeps, want the subdirectory and the file not named like a staged one", got)
	}
	requireReleased(t, cfg.DataDir)
}
