package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

type stubClient struct {
	mu        sync.Mutex
	handler   func(engine.Event) bool
	calls     []string
	connected chan struct{}
}

func newStubClient() *stubClient { return &stubClient{connected: make(chan struct{}, 16)} }

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

func (c *stubClient) Disconnect()     { c.record("disconnect") }
func (c *stubClient) Paired() bool    { return true }
func (c *stubClient) Account() string { return "15550100009" }
func (c *stubClient) PairPhone(context.Context, string) (string, error) {
	return "", errors.New("synthetic")
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

type stubVersions struct{}

func (stubVersions) Latest(context.Context) (engine.Version, error) {
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
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{}, engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}})
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
		a, err := newAppWith(t.Context(), cfg, logx.NewWriter(logs), noClients{}, engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}})
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
	if _, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, engineParts{client: newStubClient()}); err == nil {
		t.Fatal("an engine without a version source or a decoder was accepted")
	}
	requireReleased(t, cfg.DataDir)
}

func TestHistoryCapLimitsAgree(t *testing.T) {
	if config.DefaultHistoryMaxBytes != engine.DefaultHistoryMaxBytes || config.MaxHistoryMaxBytes != engine.MaxHistoryMaxBytes {
		t.Fatal("config and engine disagree on the history cap")
	}
}
