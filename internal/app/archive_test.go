package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/ingest"
)

func openArchive(t *testing.T, ctx context.Context, dir string) (*ingest.Store, error) {
	t.Helper()
	logger := logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)
	return ingest.Open(ctx, ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger})
}

func requireReleased(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	s, err := openArchive(t, ctx, dir)
	if err != nil {
		t.Fatalf("the archive is still locked: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestHealthIsServedOnlyWhileTheArchiveIsOpen(t *testing.T) {
	a, _, stop := start(t, testConfig(t, ""), noClients{})
	health := addr(t, a, "health")
	if r := mustDo(t, http.MethodGet, health, "/healthz", nil); r.status != http.StatusOK {
		t.Fatalf("/healthz with the archive open = %d %q", r.status, r.body)
	}
	if err := a.archive.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r := mustDo(t, http.MethodGet, health, "/healthz", nil); r.status != http.StatusServiceUnavailable || r.body != `{"status":"unavailable"}` {
		t.Fatalf("/healthz with the archive closed = %d %q, want 503", r.status, r.body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestEachStartIsRecordedAndTheArchiveClosedOnShutdown(t *testing.T) {
	cfg := testConfig(t, "")
	for want := 1; want <= 2; want++ {
		_, logs, stop := start(t, cfg, noClients{})
		if err := stop(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		opened := logs.find("archive_opened")
		if len(opened) != 1 {
			t.Fatalf("archive_opened events = %v", opened)
		}
		e := opened[0]
		if e["level"] != "INFO" || e["schema_version"] != float64(3) || e["profile"] != "local" || e["ofd_locking"] != (runtime.GOOS == "linux") || e["recent_starts"] != float64(want) {
			t.Fatalf("archive_opened = %v, want start %d", e, want)
		}
		requireReleased(t, cfg.DataDir)
	}
}

func TestNewRefusesAnArchiveItCannotTrust(t *testing.T) {
	cfg := testConfig(t, "")
	path := filepath.Join(cfg.DataDir, "archive.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the refusal under test needs a world-readable archive
		t.Fatalf("Chmod: %v", err)
	}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, idle())
	if err == nil {
		t.Cleanup(func() { _ = run(t, a)() })
	}
	if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "archive_db_permissions" {
		t.Fatalf("newAppWith = %v, want an archive_db_permissions refusal", err)
	}
}

func TestNewWaitsForTheArchiveAndStopsWhenCancelled(t *testing.T) {
	cfg := testConfig(t, "")
	holder, err := openArchive(t, t.Context(), cfg.DataDir)
	if err != nil {
		t.Fatalf("hold the archive: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	a, err := newAppWith(ctx, cfg, logx.NewWriter(io.Discard), noClients{}, idle())
	if err == nil {
		t.Cleanup(func() { _ = run(t, a)() })
	}
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, new(*Refusal)) {
		t.Fatalf("newAppWith while another process holds the archive = %v, want the cancellation", err)
	}
	if elapsed := time.Since(began); elapsed > 15*time.Second {
		t.Fatalf("newAppWith returned %v after its context ended", elapsed)
	}
}

func TestAFailedStartClosesTheArchive(t *testing.T) {
	a, _, stop := start(t, testConfig(t, ""), noClients{})
	cfg := testConfig(t, "")
	cfg.HealthListen = addr(t, a, "health")
	if _, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, idle()); err == nil {
		t.Fatal("newAppWith bound an address already in use")
	}
	requireReleased(t, cfg.DataDir)
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
