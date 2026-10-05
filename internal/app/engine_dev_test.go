//go:build dev

package app

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
)

func TestTheFakeEngineOpensNoSessionAndWaitsForPairing(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	cfg.Dev.FakeEngine = true
	logs := &syncBuffer{}
	a, err := New(t.Context(), cfg, logx.NewWriter(logs))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stop := run(t, a)
	waitUntil(t, "the service is ready", func() bool { return len(logs.find("ready")) == 1 })
	if f := logs.find("fake_engine"); len(f) != 1 || f[0]["level"] != "WARN" || f[0]["wrong_account"] != false {
		t.Fatalf("fake_engine events %v", f)
	}
	for _, event := range []string{"session_opened", "engine_absent", "engine_state"} {
		if found := logs.find(event); len(found) != 0 {
			t.Fatalf("%s events %v with the fake engine", event, found)
		}
	}
	if st := a.engine.Status(); st.State != engine.StateUnpaired || st.Paired {
		t.Fatalf("engine status %+v", st)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cfg.DataDir, "session.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("session.db with the fake engine: %v", err)
	}
	requireReleased(t, cfg.DataDir)
}

func TestARefusedFakeEngineClosesTheArchive(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100021", config.DefaultHistoryMaxBytes
	cfg.Dev.FakeEngine = true
	if _, err := New(t.Context(), cfg, logx.NewWriter(io.Discard)); err == nil {
		t.Fatal("New accepted an owner number that the fake engine uses for a synthetic contact")
	}
	requireReleased(t, cfg.DataDir)
}
