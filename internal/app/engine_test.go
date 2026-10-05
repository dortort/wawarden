package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/session"
	"github.com/dortort/wawarden/internal/token"
)

type stubClient struct {
	mu        sync.Mutex
	handler   func(engine.Event) bool
	calls     []string
	connected chan struct{}
	unpaired  bool
	pairCode  string
}

func newStubClient() *stubClient { return &stubClient{connected: make(chan struct{}, 16)} }

func fixed(p engineParts) engineSource {
	return func(context.Context, config.Config, *logx.Writer, *slog.Logger, *slog.Logger) (engineParts, error) {
		return p, nil
	}
}

func (c *stubClient) record(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *stubClient) called(call string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(strings.Join(c.calls, " "), call)
}

func (c *stubClient) Connect(context.Context) error {
	c.record("connect")
	c.connected <- struct{}{}
	return nil
}

func (c *stubClient) Disconnect() { c.record("disconnect") }

func (c *stubClient) Paired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.unpaired
}

func (c *stubClient) Account() string { return "15550100009" }
func (c *stubClient) PairPhone(context.Context, string) (string, error) {
	c.record("pair_phone")
	if c.pairCode == "" {
		return "", errors.New("synthetic")
	}
	return c.pairCode, nil
}
func (c *stubClient) Logout(context.Context) error { return nil }
func (c *stubClient) Version() engine.Version      { return engine.Version{2, 3000, 1} }
func (c *stubClient) SetVersion(engine.Version)    { c.record("set_version") }
func (c *stubClient) DownloadHistory(context.Context, engine.HistoryRef, io.Writer) error {
	return errors.New("synthetic: no download")
}
func (c *stubClient) AckHistory(context.Context, engine.HistoryRef) error { return nil }

func (c *stubClient) OnEvent(handler func(engine.Event) bool) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.handler = nil
	}
}

func (c *stubClient) emit(ev engine.Event) bool {
	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()
	return handler != nil && handler(ev)
}

type stubVersions struct{ calls *atomic.Int32 }

func (v stubVersions) Latest(context.Context) (engine.Version, error) {
	if v.calls != nil {
		v.calls.Add(1)
	}
	return engine.Version{2, 3000, 1}, nil
}

type stubDecoder struct{}

func (stubDecoder) Decode([]byte) (engine.History, error) { return engine.History{}, nil }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartsWithoutAnEngineAndSaysSo(t *testing.T) {
	adminToken := token.NewAdmin()
	a, logs, stop := start(t, testConfig(t, adminToken), noClients{})
	absent := logs.find("engine_absent")
	if len(absent) != 1 || absent[0]["level"] != "WARN" {
		t.Fatalf("engine_absent events %v", absent)
	}
	r := mustDo(t, http.MethodGet, addr(t, a, "admin"), "/metrics", bearer(adminToken))
	if r.status != http.StatusOK || strings.Contains(r.body, "wawarden_connected") || strings.Contains(r.body, "wawarden_paired") {
		t.Fatalf("/metrics without an engine = %d %q", r.status, r.body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestEngineRunsWhenAClientIsSupplied(t *testing.T) {
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	client := newStubClient()
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	if len(logs.find("engine_absent")) != 0 {
		t.Fatal("engine_absent was logged although a client was supplied")
	}
	stop := run(t, a)
	<-client.connected
	if !client.emit(engine.Connected{}) {
		t.Fatal("the engine did not register its event handler")
	}
	admin, health := addr(t, a, "admin"), addr(t, a, "health")
	waitUntil(t, "the Connected gauge is 1", func() bool {
		return strings.Contains(mustDo(t, http.MethodGet, admin, "/metrics", bearer(adminToken)).body, "wawarden_connected 1")
	})
	if !client.emit(engine.Message{Chat: "15550100001@s.whatsapp.net", ID: "M1", Sender: "15550100001@s.whatsapp.net", Timestamp: time.Now(), Kind: engine.KindText, Text: "synthetic"}) {
		t.Fatal("a message was not acknowledged")
	}
	alice, _ := policy.Normalize("15550100001@s.whatsapp.net")
	waitUntil(t, "the message is in the archive", func() bool {
		var found bool
		_ = a.archive.Read(t.Context(), "test.find", func(r *ingest.Reader) error {
			var e error
			_, found, e = r.ResolveInChat(alice, "M1", alice)
			return e
		})
		return found
	})
	client.emit(engine.StreamReplaced{})
	if st := a.engine.Status(); st.State != engine.StateDisconnected || st.Reason != engine.ReasonReplaced {
		t.Fatalf("engine status %+v", st)
	}
	if r := mustDo(t, http.MethodGet, health, "/healthz", nil); r.status != http.StatusOK {
		t.Fatalf("/healthz with the engine disconnected = %d %q", r.status, r.body)
	}
	if d := logs.find("disconnected"); len(d) != 1 || d[0]["reason"] != "replaced" {
		t.Fatalf("disconnected events %v", d)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !client.called("disconnect") || client.emit(engine.Connected{}) {
		t.Fatalf("after shutdown the client was not disconnected or still delivers events: %v", client.calls)
	}
	requireReleased(t, cfg.DataDir)
}

func TestTheRestartBudgetReachesTheEngine(t *testing.T) {
	for _, tt := range []struct {
		prior  int
		budget bool
	}{{engine.MaxRecentStarts - 1, false}, {engine.MaxRecentStarts, true}} {
		cfg := testConfig(t, "")
		cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
		s, err := openArchive(t, t.Context(), cfg.DataDir)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		for range tt.prior {
			if _, err := s.RecordStart(t.Context(), time.Now()); err != nil {
				t.Fatalf("RecordStart: %v", err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		client := newStubClient()
		logs := &syncBuffer{}
		a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
		if err != nil {
			t.Fatalf("newAppWith: %v", err)
		}
		stop := run(t, a)
		waitUntil(t, "the service is ready", func() bool { return len(logs.find("ready")) == 1 })
		if o := logs.find("archive_opened"); len(o) != 1 || o[0]["recent_starts"] != float64(tt.prior+1) {
			t.Fatalf("archive_opened %v", o)
		}
		if !tt.budget {
			select {
			case <-client.connected:
			case <-time.After(10 * time.Second):
				t.Fatalf("start %d of the window did not connect", tt.prior+1)
			}
		} else {
			if st := a.engine.Status(); st.State != engine.StateDisconnected || st.Reason != engine.ReasonRestartBudget {
				t.Fatalf("engine status %+v at start %d of the window", st, tt.prior+1)
			}
			if d := logs.find("disconnected"); len(d) != 1 || d[0]["reason"] != "restart_budget" {
				t.Fatalf("disconnected events %v", d)
			}
		}
		if err := stop(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		client.mu.Lock()
		connected := slices.Contains(client.calls, "connect")
		client.mu.Unlock()
		if tt.budget && connected {
			t.Fatal("the engine connected beyond the restart budget")
		}
	}
}

func TestIncompleteEnginePartsAreRefused(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.HistoryMaxBytes = config.DefaultHistoryMaxBytes
	if _, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, fixed(engineParts{client: newStubClient()})); err == nil {
		t.Fatal("an engine without a version source or a decoder was accepted")
	}
	requireReleased(t, cfg.DataDir)
}

func TestHistoryCapLimitsAgree(t *testing.T) {
	if config.DefaultHistoryMaxBytes != engine.DefaultHistoryMaxBytes || config.MaxHistoryMaxBytes != engine.MaxHistoryMaxBytes {
		t.Fatal("config and engine disagree on the history cap")
	}
}

type stubSession struct {
	healthy atomic.Bool
	closed  atomic.Int32
}

func (s *stubSession) Healthy() bool      { return s.healthy.Load() }
func (s *stubSession) Close() error       { s.closed.Add(1); return nil }
func (s *stubSession) SchemaVersion() int { return 1 }
func (s *stubSession) Backup(context.Context, string, func(string, int64, io.Reader) error) error {
	return errors.New("synthetic: no device store to back up")
}

func TestAnUnpairedEngineStaysIdleAndSaysSoOnce(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	client := newStubClient()
	client.unpaired = true
	var fetches atomic.Int32
	logs := &syncBuffer{}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{}, fixed(engineParts{client: client, versions: stubVersions{calls: &fetches}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	stop := run(t, a)
	waitUntil(t, "the service is ready", func() bool { return len(logs.find("ready")) == 1 })
	if u := logs.find("unpaired"); len(u) != 1 || u[0]["level"] != "WARN" {
		t.Fatalf("unpaired events %v, want one warning", u)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	client.mu.Lock()
	calls := slices.Clone(client.calls)
	client.mu.Unlock()
	if slices.Contains(calls, "connect") || slices.Contains(calls, "set_version") || fetches.Load() != 0 {
		t.Fatalf("an unpaired engine made calls %v and %d version fetches", calls, fetches.Load())
	}
	if st := a.engine.Status(); st.State != engine.StateDisconnected || st.Reason != engine.ReasonShutdown || st.Paired {
		t.Fatalf("engine status after shutdown %+v", st)
	}
}

func TestTheDeviceStoreIsPartOfHealthAndClosedOnShutdown(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.HistoryMaxBytes = config.DefaultHistoryMaxBytes
	store := &stubSession{}
	store.healthy.Store(true)
	client := newStubClient()
	client.unpaired = true
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}, session: store}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	stop := run(t, a)
	health := addr(t, a, "health")
	waitUntil(t, "/healthz answers 200", func() bool { return mustDo(t, http.MethodGet, health, "/healthz", nil).status == http.StatusOK })
	store.healthy.Store(false)
	if r := mustDo(t, http.MethodGet, health, "/healthz", nil); r.status != http.StatusServiceUnavailable {
		t.Fatalf("/healthz with the device store lost = %d %q, want 503", r.status, r.body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if store.closed.Load() != 1 {
		t.Fatalf("the device store was closed %d times on shutdown, want once", store.closed.Load())
	}
	requireReleased(t, cfg.DataDir)
}

func TestAFailingEngineSourceClosesTheArchive(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.HistoryMaxBytes = config.DefaultHistoryMaxBytes
	failure := errors.New("synthetic: no device store")
	failing := func(context.Context, config.Config, *logx.Writer, *slog.Logger, *slog.Logger) (engineParts, error) {
		return engineParts{}, failure
	}
	if _, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, failing); !errors.Is(err, failure) {
		t.Fatalf("newAppWith = %v, want the source's failure", err)
	}
	requireReleased(t, cfg.DataDir)
	store := &stubSession{}
	if _, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, fixed(engineParts{client: newStubClient(), session: store})); err == nil {
		t.Fatal("an engine without a version source or a decoder was accepted")
	}
	if store.closed.Load() != 1 {
		t.Fatal("a refused engine left its device store open")
	}
	requireReleased(t, cfg.DataDir)
}

func TestReleaseBuildsOpenTheSessionAndWaitForPairing(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.OwnerPhone, cfg.HistoryMaxBytes = "+15550100009", config.DefaultHistoryMaxBytes
	logs := &syncBuffer{}
	a, err := New(t.Context(), cfg, logx.NewWriter(logs))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stop := run(t, a)
	waitUntil(t, "the service is ready", func() bool { return len(logs.find("ready")) == 1 })
	if o := logs.find("session_opened"); len(o) != 1 || o[0]["paired"] != false {
		t.Fatalf("session_opened events %v", o)
	}
	if u := logs.find("unpaired"); len(u) != 1 {
		t.Fatalf("unpaired events %v", u)
	}
	for _, event := range []string{"engine_absent", "engine_state", "version_refresh_failed", "version_updated", "connect_failed"} {
		if found := logs.find(event); len(found) != 0 {
			t.Fatalf("%s events %v: an unpaired release build must make no connection", event, found)
		}
	}
	fi, err := os.Lstat(filepath.Join(cfg.DataDir, "session.db"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("session.db: %v, %v", fi, err)
	}
	if st := a.engine.Status(); st.State != engine.StateUnpaired || st.Paired {
		t.Fatalf("engine status %+v", st)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	requireReleased(t, cfg.DataDir)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	store, err := session.Open(ctx, session.Options{DataDir: cfg.DataDir, UID: os.Geteuid(), Profile: session.Profile(config.StorageLocal), Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)})
	if err != nil {
		t.Fatalf("session.db is still locked after shutdown: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestReleaseBuildsRefuseASessionTheyCannotTrust(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.HistoryMaxBytes = config.DefaultHistoryMaxBytes
	path := filepath.Join(cfg.DataDir, "session.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil { //nolint:gosec // G302: the refusal under test needs a group-readable session.db
		t.Fatalf("Chmod: %v", err)
	}
	_, err := New(t.Context(), cfg, logx.NewWriter(io.Discard))
	if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "session_db_permissions" {
		t.Fatalf("New = %v, want the refusal session_db_permissions", err)
	}
	requireReleased(t, cfg.DataDir)
}
