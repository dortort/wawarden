package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

func TestEngineErrorsBecomeFixedAdminCodes(t *testing.T) {
	for _, m := range adminErrors {
		if got := adminError(fmt.Errorf("synthetic: %w", m.engine)); got != m.api {
			t.Errorf("adminError(%v) = %v, want %v", m.engine, got, m.api)
		}
	}
	for _, err := range []error{
		engine.ErrAlreadyPaired, engine.ErrAlreadyConnected, engine.ErrNotPaired, engine.ErrOwnerPhoneMissing,
		engine.ErrOwnerMismatch, engine.ErrPairRateLimited, engine.ErrPairFailed, engine.ErrStopped,
	} {
		if adminError(err) == err {
			t.Errorf("the engine's %v reaches the API unmapped", err)
		}
	}
	other := errors.New("synthetic")
	if adminError(other) != other || adminError(nil) != nil {
		t.Fatal("an unknown error or nil was changed")
	}
}

func post(t *testing.T, to netip.AddrPort, path string, header http.Header, body string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+to.String()+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	for k, values := range header {
		req.Header[k] = values
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func TestAdminRoutesDriveTheEngine(t *testing.T) {
	const canary = "CANA-RY77"
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	client := newStubClient()
	client.unpaired, client.pairCode = true, canary
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	ingested := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	alice, _ := policy.Normalize("15550100001@s.whatsapp.net")
	if err := a.archive.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		_, _, err := tx.InsertMessage(ingest.Message{Chat: alice, ID: "M1", Sender: alice, Origin: ingest.OriginLive,
			Timestamp: ingested.Add(-time.Minute), Kind: ingest.KindText, Text: "synthetic", Ingested: ingested})
		return err
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stop := run(t, a)
	logs.waitFor(t, "ready")
	admin := addr(t, a, "admin")

	r := mustDo(t, http.MethodGet, admin, "/admin/v1/status", bearer(adminToken))
	want := `{"state":"unpaired","reason":"","paired":false,"counts":{"chats":1,"messages":1,"history_blobs_pending":0,` +
		`"history_blobs_quarantined":0,"inbox_backlog":0,"inbox_quarantined":0},"last_ingest_at":"2026-10-05T08:00:00Z","version":"` + buildinfo.Read().Version + `"}`
	if r.status != http.StatusOK || r.body != want {
		t.Fatalf("status = %d %s, want %s", r.status, r.body, want)
	}
	if r := post(t, admin, "/admin/v1/reconnect", bearer(adminToken), "{}"); r.status != http.StatusConflict || r.body != `{"error":"not_paired"}` {
		t.Fatalf("reconnect while unpaired = %d %s", r.status, r.body)
	}
	r = post(t, admin, "/admin/v1/pair", bearer(adminToken), "{}")
	if r.status != http.StatusOK || r.body != `{"code":"`+canary+`"}` {
		t.Fatalf("pair = %d %s", r.status, r.body)
	}
	if !client.called("connect") || !client.called("pair_phone") {
		t.Fatalf("pairing did not connect and request a code: %v", client.calls)
	}
	if r := mustDo(t, http.MethodGet, admin, "/admin/v1/status", bearer(token.NewAdmin())); r.status != http.StatusUnauthorized {
		t.Fatalf("status with another token = %d", r.status)
	}
	metricsBody := mustDo(t, http.MethodGet, admin, "/metrics", bearer(adminToken)).body
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var mutations []string
	for _, rec := range logs.find("admin_mutation") {
		mutations = append(mutations, rec["action"].(string)+":"+rec["outcome"].(string))
	}
	if got := strings.Join(mutations, " "); got != "reconnect:not_paired pair:ok" {
		t.Fatalf("admin_mutation events %q", got)
	}
	if f := logs.find("admin_auth_failure"); len(f) != 1 || f[0]["count"] != float64(1) {
		t.Fatalf("admin_auth_failure events %v", f)
	}
	for name, out := range map[string]string{"standard output": logs.buf.String(), "metrics": metricsBody} {
		if strings.Contains(out, canary) || strings.Contains(out, strings.ReplaceAll(canary, "-", "")) {
			t.Fatalf("the pairing code reached %s:\n%s", name, out)
		}
	}
}

func TestAdminRoutesWithoutAnEngine(t *testing.T) {
	adminToken := token.NewAdmin()
	a, logs, stop := start(t, testConfig(t, adminToken), noClients{})
	logs.waitFor(t, "ready")
	admin := addr(t, a, "admin")
	if r := mustDo(t, http.MethodGet, admin, "/admin/v1/status", bearer(adminToken)); r.status != http.StatusServiceUnavailable || r.body != `{"error":"engine_unavailable"}` {
		t.Fatalf("status without an engine = %d %s", r.status, r.body)
	}
	for _, path := range []string{"/admin/v1/pair", "/admin/v1/reconnect"} {
		if r := post(t, admin, path, bearer(adminToken), "{}"); r.status != http.StatusServiceUnavailable || r.body != `{"error":"engine_unavailable"}` {
			t.Fatalf("%s without an engine = %d %s", path, r.status, r.body)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	var outcomes []string
	for _, rec := range logs.find("admin_mutation") {
		outcomes = append(outcomes, rec["outcome"].(string))
	}
	if strings.Join(outcomes, " ") != "engine_unavailable engine_unavailable" {
		t.Fatalf("admin_mutation outcomes %v", outcomes)
	}
}
