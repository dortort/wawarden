package app

import (
	"context"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/admin"
)

func startWith(t *testing.T, dataDir string, master *keys.Master, rows int) *syncBuffer {
	t.Helper()
	cfg := testConfig(t, "")
	cfg.DataDir = dataDir
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), master, noClients{}, idle())
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	stop := run(t, a)
	logs.waitFor(t, "ready")
	for range rows {
		if err := a.archive.Audit().Record(t.Context(), admin.Event{At: time.Now(), Client: "aaaaaaaa", Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return logs
}

func TestAStartAnnouncesAnAuditChainLeftUnderAnotherKey(t *testing.T) {
	previous, current := rotatedMasters(t)
	tests := []struct {
		name   string
		before *keys.Master
		rows   int
		level  string
		key    string
	}{
		{name: "the newest row under a retired key", before: previous, rows: 1, level: "INFO", key: "retired"},
		{name: "the newest row under an unknown key", before: testMaster(t), rows: 1, level: "WARN", key: "missing"},
		{name: "the newest row under the current key", before: current, rows: 1},
		{name: "an empty chain", before: previous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			startWith(t, dir, tt.before, tt.rows)
			logs := startWith(t, dir, current, 0)
			changed := logs.find("audit_key_changed")
			if tt.key == "" {
				if len(changed) != 0 {
					t.Fatalf("audit_key_changed %v, want none", changed)
				}
				return
			}
			if len(changed) != 1 || changed[0]["level"] != tt.level || changed[0]["previous_key"] != tt.key ||
				changed[0]["previous_key_id"] != tt.before.ID() || changed[0]["key_id"] != current.ID() || len(changed[0]) != 7 {
				t.Fatalf("audit_key_changed %v, want one %s line naming %s as %s", changed, tt.level, tt.before.ID(), tt.key)
			}
			again := startWith(t, dir, current, 1)
			if len(again.find("audit_key_changed")) != 1 {
				t.Fatal("a start before the first row under the current key did not announce the change again")
			}
			if after := startWith(t, dir, current, 0); len(after.find("audit_key_changed")) != 0 {
				t.Fatal("a start after a row under the current key announced a change")
			}
		})
	}
}

func TestAnUnreadableAuditChainIsLoggedWithoutAnEvent(t *testing.T) {
	a, logs := open(t, testConfig(t, ""), noClients{})
	t.Cleanup(func() { _ = a.closeStores() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.announceKeyChange(ctx)
	if unread := logs.find("audit_key_unread"); len(unread) != 1 || unread[0]["level"] != "WARN" || len(logs.find("audit_key_changed")) != 0 {
		t.Fatalf("audit_key_unread %v, want one warning and no event", unread)
	}
}
