//go:build dev

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

type fakeService struct {
	t       *testing.T
	admin   string
	token   string
	secret  string
	dataDir string
	stdout  *output
	stop    func() int
}

func serveFake(t *testing.T, mode string) *fakeService {
	t.Helper()
	secret := token.NewAdmin()
	s := &fakeService{t: t, admin: freeAddr(t), token: tokenFile(t, secret), secret: secret, dataDir: filepath.Join(t.TempDir(), "data"), stdout: newOutput()}
	environ := []string{
		"WAWARDEN_DATA_DIR=" + s.dataDir,
		"WAWARDEN_LISTEN=" + freeAddr(t),
		"WAWARDEN_ADMIN_LISTEN=" + s.admin,
		"WAWARDEN_HEALTH_LISTEN=" + freeAddr(t),
		"WAWARDEN_ADMIN_TOKEN_SHA256=" + token.Hash(secret),
		"WAWARDEN_OWNER_PHONE=+15550100009",
		"WAWARDEN_HISTORY_MAX_BYTES=1048576",
		"WAWARDEN_DEV_FAKE_ENGINE=" + mode,
	}
	ctx, cancel := context.WithCancel(t.Context())
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"serve", "--allow-root"}, environ, nil, s.stdout, newOutput()) }()
	s.stop = sync.OnceValue(func() int {
		cancel()
		return <-exited
	})
	t.Cleanup(func() { s.stop() })
	s.stdout.waitFor(t, "ready")
	if f, o := s.events("fake_engine"), s.events("session_opened"); len(f) != 1 || len(o) != 0 {
		t.Fatalf("fake_engine events %v and session_opened events %v: pairing anything but the fake engine would contact WhatsApp", f, o)
	}
	return s
}

func (s *fakeService) adminCLI(command string) (int, string, string) {
	s.t.Helper()
	return invoke(s.t, []string{"admin", command, "--addr", "http://" + s.admin, "--token-file", s.token}, nil)
}

func (s *fakeService) status() map[string]string {
	s.t.Helper()
	code, out, errText := s.adminCLI("status")
	if code != 0 {
		s.t.Fatalf("admin status = %d %q", code, errText)
	}
	fields := map[string]string{}
	for line := range strings.Lines(out) {
		k, v, _ := strings.Cut(strings.TrimSuffix(line, "\n"), ": ")
		fields[k] = v
	}
	return fields
}

func (s *fakeService) waitForStatus(what string, cond func(map[string]string) bool) map[string]string {
	s.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		st := s.status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting until %s: status %v", what, st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *fakeService) metrics() string {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, "http://"+s.admin+"/metrics", nil)
	if err != nil {
		s.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		s.t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		s.t.Fatalf("GET /metrics = %d, %v", resp.StatusCode, err)
	}
	return string(body)
}

func (s *fakeService) events(name string) []map[string]any {
	var out []map[string]any
	for line := range strings.Lines(s.stdout.String()) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == name {
			out = append(out, rec)
		}
	}
	return out
}

func (s *fakeService) pair() {
	s.t.Helper()
	code, out, errText := s.adminCLI("pair")
	if code != 0 || out != "pairing code: FAKE-C0DE\n" {
		s.t.Fatalf("admin pair = %d %q %q", code, out, errText)
	}
}

func TestFakeEnginePairsIngestsAndArchives(t *testing.T) {
	s := serveFake(t, "1")
	if st := s.status(); st["state"] != "unpaired" || st["paired"] != "false" || st["messages"] != "0" {
		t.Fatalf("status before pairing %v", st)
	}
	s.pair()
	st := s.waitForStatus("the script is ingested and both bad blobs are quarantined", func(st map[string]string) bool {
		return st["history blobs quarantined"] == "2" && st["history blobs pending"] == "0" && st["inbox backlog"] == "0" && len(s.events("quarantine")) == 2
	})
	want := map[string]string{"state": "connected", "reason": "none", "paired": "true", "chats": "3", "messages": "17", "inbox quarantined": "0"}
	for k, v := range want {
		if st[k] != v {
			t.Fatalf("status %s = %q, want %q (status %v)", k, st[k], v, st)
		}
	}

	metrics := s.metrics()
	for _, line := range []string{
		"wawarden_paired 1",
		"wawarden_connected 1",
		"wawarden_messages_ingested_total 17",
		`wawarden_ingest_dropped_total{reason="chat_rejected"} 2`,
		`wawarden_ingest_dropped_total{reason="foreign_reference"} 2`,
		`wawarden_ingest_dropped_total{reason="history_not_primary"} 1`,
		`wawarden_ingest_dropped_total{reason="no_target"} 1`,
		`wawarden_ingest_dropped_total{reason="not_admin"} 1`,
		`wawarden_ingest_dropped_total{reason="not_original_sender"} 1`,
		`wawarden_ingest_quarantined_total{queue="history"} 2`,
	} {
		if !slices.Contains(strings.Split(metrics, "\n"), line) {
			t.Errorf("/metrics lacks %q", line)
		}
	}
	for _, absent := range []string{`reason="not_paired"`, `queue="inbox"`, "wawarden_ingest_refused_total{", "wawarden_panics_total{"} {
		if strings.Contains(metrics, absent) {
			t.Errorf("/metrics holds %s", absent)
		}
	}

	var states []string
	for _, e := range s.events("engine_state") {
		states = append(states, e["state"].(string))
	}
	if want := []string{"connecting", "connected", "connecting", "connected"}; !slices.Equal(states, want) {
		t.Errorf("engine states %q, want %q: a pairing, a connection, the scripted drop and the reconnection", states, want)
	}
	if q := s.events("quarantine"); len(q) != 2 || q[0]["queue"] != "history" || q[1]["queue"] != "history" || q[0]["attempts"] != float64(3) {
		t.Errorf("quarantine events %v", q)
	}
	if m := s.events("admin_mutation"); len(m) != 1 || m[0]["action"] != "pair" || m[0]["outcome"] != "ok" {
		t.Errorf("admin_mutation events %v", m)
	}
	for _, name := range []string{"pair_rejected", "disconnected", "panic", "connect_failed"} {
		if e := s.events(name); len(e) != 0 {
			t.Errorf("%s events %v", name, e)
		}
	}

	if code := s.stop(); code != 0 {
		t.Fatalf("serve stopped with %d", code)
	}
	for _, leak := range []string{"FAKE-C0DE", "Hello from the fake engine", "15550100021", "15550100009", "Synthetic group"} {
		if strings.Contains(s.stdout.String(), leak) {
			t.Errorf("the service's output holds %q", leak)
		}
	}
	checkArchive(t, s.dataDir)
}

func checkArchive(t *testing.T, dataDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store, err := ingest.Open(ctx, ingest.Options{DataDir: dataDir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)})
	if err != nil {
		t.Fatalf("open the archive: %v", err)
	}
	defer func() { _ = store.Close() }()
	chat := func(raw string) policy.CanonicalChat {
		c, ok := policy.Normalize(raw)
		if !ok {
			t.Fatalf("Normalize(%s)", raw)
		}
		return c
	}
	alice, bob, carol, owner, group := chat("15550100021@s.whatsapp.net"), chat("15550100022@s.whatsapp.net"), chat("15550100023@s.whatsapp.net"),
		chat("15550100009@s.whatsapp.net"), chat("120363000000000021@g.us")
	tests := []struct {
		chat, sender policy.CanonicalChat
		id           string
		check        func(ingest.Found) bool
		what         string
	}{
		{alice, alice, "FAKE-H01", func(f ingest.Found) bool { return f.Text == "An old message from Alice" }, "the inline bootstrap blob is applied"},
		{carol, carol, "FAKE-H04", func(f ingest.Found) bool { return f.Text == "Carol, from the downloaded history" }, "the downloaded blob is applied"},
		{alice, alice, "FAKE-L01", func(f ingest.Found) bool {
			return f.Text == "Hello from the fake engine, edited" && !f.EditedAt.IsZero()
		}, "the sender's edit is applied"},
		{alice, owner, "FAKE-L02", func(f ingest.Found) bool { return f.Revoked && f.FromMe }, "the owner's revoke is applied"},
		{group, bob, "FAKE-L03", func(f ingest.Found) bool { return f.Revoked }, "the admin's revoke is applied"},
		{group, alice, "FAKE-L04", func(f ingest.Found) bool { return f.Text == "Alice quotes Bob" && !f.Revoked && f.EditedAt.IsZero() }, "another member's edit and a cross-chat revoke are refused"},
		{group, alice, "FAKE-L05", func(f ingest.Found) bool { return !f.Revoked }, "a revoke by a member who is not an admin is refused"},
		{group, bob, "FAKE-L14", func(f ingest.Found) bool { return f.Kind == ingest.KindReaction }, "the reaction is stored"},
		{group, alice, "FAKE-L16", func(f ingest.Found) bool { return f.Kind == ingest.KindPollUpdate }, "the poll update is stored"},
		{alice, alice, "FAKE-L18", func(f ingest.Found) bool { return f.Kind == ingest.KindMedia }, "the media message is stored"},
	}
	if err := store.Read(ctx, "test.fake_archive", func(r *ingest.Reader) error {
		for _, tt := range tests {
			f, ok, err := r.ResolveInChat(tt.chat, tt.id, tt.sender)
			if err != nil || !ok || !tt.check(f) {
				t.Errorf("%s: %s = %+v, %v, %v", tt.what, tt.id, f, ok, err)
			}
		}
		for _, id := range []string{"FAKE-L19", "FAKE-L21"} {
			if _, ok, err := r.ResolveInChat(alice, id, alice); ok || err != nil {
				t.Errorf("%s was stored: %v", id, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the archive: %v", err)
	}
}

func TestFakeEngineRejectsAWrongAccount(t *testing.T) {
	s := serveFake(t, "wrong_account")
	s.pair()
	s.waitForStatus("the wrong account is logged out", func(st map[string]string) bool {
		return st["state"] == "unpaired" && st["paired"] == "false" && len(s.events("pair_rejected")) == 1 && strings.Contains(s.metrics(), "wawarden_paired 0\n")
	})
	if r := s.events("pair_rejected"); r[0]["stage"] != "after_pairing" {
		t.Fatalf("pair_rejected events %v", r)
	}
	metrics := s.metrics()
	if !strings.Contains(metrics, "wawarden_paired 0\n") || !strings.Contains(metrics, "wawarden_connected 0\n") {
		t.Fatalf("/metrics after the rejection:\n%s", metrics)
	}
	if st := s.status(); st["messages"] != "0" || st["chats"] != "0" {
		t.Fatalf("status after the rejection %v", st)
	}
	if e := s.events("logout_failed"); len(e) != 0 {
		t.Fatalf("logout_failed events %v", e)
	}
	if code := s.stop(); code != 0 {
		t.Fatalf("serve stopped with %d", code)
	}
}
