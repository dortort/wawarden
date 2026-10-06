package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"github.com/dortort/wawarden/internal/safego"
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
	cfg.OwnerPhone, cfg.HistoryMaxBytes, cfg.MetricsEMF = "+15550100009", config.DefaultHistoryMaxBytes, true
	client := newStubClient()
	client.unpaired, client.pairCode = true, canary
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), nil, noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
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
		`"history_blobs_quarantined":0,"inbox_backlog":0,"inbox_quarantined":0},"clients":{"active":0,"expired":0,"revoked":0,"all_chats_active":0},` +
		`"warnings":[],"last_ingest_at":"2026-10-05T08:00:00Z","version":"` + buildinfo.Read().Version + `"}`
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
	if e := emfLines(logs); len(e) != 1 || e[0]["AdminAuthFailures"] != float64(1) {
		t.Fatalf("embedded-metric-format lines %v, want the shutdown line counting the failed authentication", e)
	}
	for name, out := range map[string]string{"standard output": logs.buf.String(), "metrics": metricsBody} {
		if strings.Contains(out, canary) || strings.Contains(out, strings.ReplaceAll(canary, "-", "")) {
			t.Fatalf("the pairing code reached %s:\n%s", name, out)
		}
	}
}

func TestClientsAreCreatedAuthenticatedAuditedAndRevoked(t *testing.T) {
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), testMaster(t), nil, idle())
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	group, _ := policy.Normalize("120363000000000001@g.us")
	sender, _ := policy.Normalize("15550100002@s.whatsapp.net")
	if err := a.archive.Write(t.Context(), "test.fill", func(tx *ingest.Tx) error {
		if err := tx.SetChatName(group, "Synthetic Group", ingest.NameGroupSubject, ingest.OriginLive); err != nil {
			return err
		}
		_, _, err := tx.InsertMessage(ingest.Message{Chat: group, ID: "M1", Sender: sender, Origin: ingest.OriginLive,
			Timestamp: time.Now().Add(-time.Minute), Kind: ingest.KindText, Text: "synthetic", Ingested: time.Now()})
		return err
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stop := run(t, a)
	logs.waitFor(t, "ready")
	admin, client := addr(t, a, "admin"), addr(t, a, "client")

	if r := mustDo(t, http.MethodGet, admin, "/admin/v1/chats?match=synthetic", bearer(adminToken)); r.status != http.StatusOK ||
		!strings.Contains(r.body, `"id":"120363000000000001@g.us","kind":"group"`) || !strings.Contains(r.body, `"name":"Synthetic Group"`) {
		t.Fatalf("chats = %d %s", r.status, r.body)
	}
	created := post(t, admin, "/admin/v1/clients", bearer(adminToken), `{"name":"agent","read_chats":["120363000000000001@g.us"],"write_chats":["120363000000000001@g.us"]}`)
	var body struct {
		Client struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"client"`
		Credential string `json:"credential"`
	}
	if created.status != http.StatusOK || json.Unmarshal([]byte(created.body), &body) != nil || body.Client.State != "active" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	if id, ok := token.ParseClient(body.Credential); !ok || id != body.Client.ID {
		t.Fatalf("the credential %q is not a client token for %q", body.Credential, body.Client.ID)
	}
	if r := post(t, admin, "/admin/v1/clients", bearer(adminToken), `{"name":"AGENT","all_chats":true}`); r.status != http.StatusConflict || r.body != `{"error":"name_taken"}` {
		t.Fatalf("a second client with the name = %d %s", r.status, r.body)
	}
	if r := mustDo(t, http.MethodGet, client, "/v1/anything", bearer(body.Credential)); r.status != http.StatusNotFound {
		t.Fatalf("an authenticated client request to an unknown path = %d %s, want the uniform 404", r.status, r.body)
	}
	_, other := token.NewClient()
	if r := mustDo(t, http.MethodGet, client, "/v1/anything", bearer(other)); r.status != http.StatusUnauthorized {
		t.Fatalf("an unknown client token = %d, want 401", r.status)
	}
	if r := mustDo(t, http.MethodGet, admin, "/admin/v1/status", bearer(adminToken)); !strings.Contains(r.body, `"clients":{"active":1,"expired":0,"revoked":0,"all_chats_active":0}`) {
		t.Fatalf("status = %s", r.body)
	}
	if r := post(t, admin, "/admin/v1/clients/"+body.Client.ID+"/revoke", bearer(adminToken), "{}"); r.status != http.StatusOK || !strings.Contains(r.body, `"state":"revoked"`) {
		t.Fatalf("revoke = %d %s", r.status, r.body)
	}
	if r := mustDo(t, http.MethodGet, client, "/v1/anything", bearer(body.Credential)); r.status != http.StatusUnauthorized {
		t.Fatalf("a revoked client's request = %d, want 401 on the next request", r.status)
	}
	if r := mustDo(t, http.MethodGet, admin, "/admin/v1/clients/zzzzzzzz", bearer(adminToken)); r.status != http.StatusNotFound || r.body != `{"error":"not_found"}` {
		t.Fatalf("an unknown client = %d %s", r.status, r.body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var mutations []string
	for _, rec := range logs.find("admin_mutation") {
		mutations = append(mutations, rec["action"].(string)+":"+rec["outcome"].(string))
	}
	if got := strings.Join(mutations, " "); got != "client_create:ok client_create:name_taken client_revoke:ok" {
		t.Fatalf("admin_mutation events %q", got)
	}
	var audit []string
	for _, rec := range logs.events() {
		if head, ok := rec["chain_head"].(string); ok && len(head) == 32 && rec["client"] == body.Client.ID {
			audit = append(audit, rec["action"].(string))
		}
	}
	if got := strings.Join(audit, " "); got != "client_create client_revoke" {
		t.Fatalf("audit lines %q, want the create and the revoke on standard output", got)
	}
	out := logs.buf.String()
	if strings.Contains(out, body.Credential) || strings.Contains(out, body.Credential[12:55]) || strings.Contains(out, "120363000000000001@g.us") {
		t.Fatalf("standard output carries the credential or a chat identifier:\n%s", out)
	}
	if n := strings.Count(created.body, body.Credential); n != 1 {
		t.Fatalf("the create response holds the credential %d times", n)
	}
}

func TestNewRefusesToStartWithoutTheMasterKey(t *testing.T) {
	if _, err := New(t.Context(), testConfig(t, ""), logx.NewWriter(io.Discard), nil); !errors.Is(err, errNoMasterKey) {
		t.Fatalf("New without a master key = %v", err)
	}
}

func TestTheWebhookIsWiredFromTheConfiguration(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	cfg.Notify = config.Notify{URL: "https://" + netip.AddrFrom4([4]byte{169, 254, 169, 254}).String() + "/latest", Secret: []byte(strings.Repeat("s", 32))}
	client := newStubClient()
	client.unpaired = true
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), nil, noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	stop := run(t, a)
	logs.waitFor(t, "notify_failed")
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if f := logs.find("notify_failed"); len(f) != 1 || f[0]["reason"] != "destination_refused" || f[0]["attempts"] != float64(1) {
		t.Fatalf("notify_failed events %v, want the unpaired event refused at the metadata address", f)
	}
	if strings.Contains(logs.buf.String(), "169.254") {
		t.Fatal("the webhook URL reached the log")
	}
}

func TestAWebhookCutShortByTheGracePeriodFailsTheRun(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for {
			select {
			case c := <-accepted:
				_ = c.Close()
			default:
				return
			}
		}
	})
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	cfg.Notify = config.Notify{URL: "https://" + ln.Addr().String() + "/hook", Secret: []byte(strings.Repeat("s", 32)), AllowPrivate: true}
	client := newStubClient()
	client.unpaired = true
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), nil, noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	a.grace = 200 * time.Millisecond
	stop := run(t, a)
	var held net.Conn
	select {
	case held = <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the unpaired event was not posted")
	}
	defer func() { _ = held.Close() }()
	if err := stop(); err == nil {
		t.Fatal("Run = nil after the webhook lost an event to the grace period, want an error so that serve exits 1")
	}
	if d := logs.find("notify_dropped"); len(d) != 1 || d[0]["count"] != float64(1) {
		t.Fatalf("notify_dropped events %v, want one counting the unpaired event", d)
	}
	if s := logs.find("stopped"); len(s) != 1 || s[0]["level"] != "ERROR" {
		t.Fatalf("stopped events %v, want an error", s)
	}
}

func TestEMFLinesAreWrittenEveryIntervalAndAtShutdown(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.MetricsEMF = true
	a, logs := open(t, cfg, noClients{})
	if a.emfEvery != time.Minute {
		t.Fatalf("embedded-metric-format lines every %v, want the documented minute", a.emfEvery)
	}
	a.emfEvery = 20 * time.Millisecond
	stop := run(t, a)
	waitUntil(t, "two periodic embedded-metric-format lines", func() bool { return len(emfLines(logs)) >= 2 })
	before := len(emfLines(logs))
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if after := len(emfLines(logs)); after <= before {
		t.Fatalf("%d embedded-metric-format lines after shutdown, %d before it: want the shutdown line too", after, before)
	}
}

func emfLines(logs *syncBuffer) []map[string]any {
	var out []map[string]any
	for _, rec := range logs.events() {
		if _, ok := rec["_aws"]; ok {
			out = append(out, rec)
		}
	}
	return out
}

func TestEMFLinesGoThroughTheScrubbingWriter(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes, cfg.MetricsEMF = "+15550100009", config.DefaultHistoryMaxBytes, true
	client := newStubClient()
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), nil, noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	stop := run(t, a)
	<-client.connected
	safego.Go("probe 15550100042@s.whatsapp.net", func() { panic("synthetic") })
	logs.waitFor(t, "panic")
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	lines := emfLines(logs)
	if len(lines) != 2 {
		t.Fatalf("%d embedded-metric-format lines, want the shutdown line and one for the panic's name: %v", len(lines), lines)
	}
	total, named := lines[0], lines[1]
	if total["Paired"] != float64(1) || total["Panics"] != float64(1) || total["MessagesIngested"] != float64(0) || total["PolicyDenials"] != float64(0) {
		t.Fatalf("shutdown line %v", total)
	}
	if name, _ := named["name"].(string); named["Panics"] != float64(1) || !strings.HasPrefix(name, "probe jid:") {
		t.Fatalf("per-name line %v, want the panic's name pseudonymised", named)
	}
	if strings.Contains(logs.buf.String(), "15550100042") {
		t.Fatal("an identifier reached standard output")
	}
}

func TestNoEMFLinesUnlessEnabled(t *testing.T) {
	a, logs, stop := start(t, testConfig(t, ""), noClients{})
	logs.waitFor(t, "ready")
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if lines := emfLines(logs); len(lines) != 0 || a.emf != nil {
		t.Fatalf("embedded-metric-format lines %v without WAWARDEN_METRICS_EMF", lines)
	}
}
