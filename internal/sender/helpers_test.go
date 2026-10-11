package sender_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sender"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	alice    = "15550100001@s.whatsapp.net"
	bob      = "15550100002@s.whatsapp.net"
	carol    = "15550100003@s.whatsapp.net"
	ownerPN  = "15550100009@s.whatsapp.net"
	groupJID = "120363000000000001@g.us"
	writerID = "aaaaaaaa"
	otherID  = "bbbbbbbb"
)

var (
	epoch     = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	farFuture = epoch.Add(300 * 24 * time.Hour)
)

type tb interface {
	Helper()
	Fatalf(format string, args ...any)
	Fatal(args ...any)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func storeOptions(dir string) ingest.Options {
	w := logx.NewWriter(&syncBuffer{})
	w.SetKey(make([]byte, 32))
	return ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1, Logger: logx.New(w, slog.LevelDebug),
		ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, RewriteTimeout: 5 * time.Minute}
}

func openStore(t tb, dir string) *ingest.Store {
	t.Helper()
	s, err := ingest.Open(context.Background(), storeOptions(dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func chat(t tb, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q) rejected a test identifier", jid)
	}
	return c
}

func grantFor(t tb, id string, firstContact bool, chats ...string) policy.WriteGrant {
	t.Helper()
	write := make(map[policy.CanonicalChat]struct{}, len(chats))
	for _, c := range chats {
		write[chat(t, c)] = struct{}{}
	}
	g, ok := policy.DecideWrite(&policy.Client{ID: id, Write: write, ExpiresAt: farFuture, AllowFirstContact: firstContact}, epoch)
	if !ok {
		t.Fatal("DecideWrite refused the test client")
	}
	return g
}

func key(t tb, s string) sender.Key {
	t.Helper()
	k, ok := sender.ParseKey(s)
	if !ok {
		t.Fatalf("ParseKey(%q) refused a test key", s)
	}
	return k
}

func inbound(t tb, s *ingest.Store, jids ...string) {
	t.Helper()
	err := s.Write(context.Background(), "test.inbound", func(tx *ingest.Tx) error {
		for i, jid := range jids {
			m := ingest.Message{Chat: chat(t, jid), ID: "IN" + strconv.Itoa(i), Sender: chat(t, jid), Origin: ingest.OriginLive,
				Timestamp: epoch.Add(-time.Hour), Kind: ingest.KindText, Text: "synthetic inbound", Ingested: epoch.Add(-time.Hour)}
			if _, _, err := tx.InsertMessage(m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type call struct {
	msg sender.Message
	at  time.Time
}

type engine struct {
	mu      sync.Mutex
	clock   *clock
	calls   []call
	ready   bool
	n       int
	gate    chan struct{}
	entered chan struct{}
	outcome error
}

func newEngine(c *clock) *engine { return &engine{clock: c, ready: true} }

func (e *engine) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

func (e *engine) NewMessageID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	return "3EB0SYNTHETIC" + strconv.Itoa(e.n)
}

func (e *engine) Send(ctx context.Context, m sender.Message) (sender.Ack, error) {
	e.mu.Lock()
	e.calls = append(e.calls, call{msg: m, at: e.clock.Now()})
	gate, entered, outcome := e.gate, e.entered, e.outcome
	e.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return sender.Ack{}, sender.ErrOutcomeUnknown
		}
	}
	if outcome != nil {
		return sender.Ack{}, outcome
	}
	own, _ := policy.Normalize(ownerPN)
	return sender.Ack{Sender: own, At: e.clock.Now()}, nil
}

func (e *engine) sent() []call {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]call(nil), e.calls...)
}

func (e *engine) set(fn func(*engine)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn(e)
}

type notes struct {
	mu         sync.Mutex
	unknown    []string
	unrecorded int
}

func (n *notes) SendOutcomeUnknown(reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unknown = append(n.unknown, reason)
}

func (n *notes) SendUnrecorded() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unrecorded++
}

func (n *notes) counts() ([]string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.unknown...), n.unrecorded
}

type settleFault struct {
	*ingest.Store
	mu    sync.Mutex
	fails int
	err   error
}

func (f *settleFault) SettleSend(g policy.WriteGrant, ctx context.Context, in ingest.Settlement) (ingest.SentRow, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return ingest.SentRow{}, f.err
	}
	f.mu.Unlock()
	return f.Store.SettleSend(g, ctx, in)
}

type leavingClient struct {
	*engine
	leave context.CancelFunc
}

func (c leavingClient) Send(ctx context.Context, m sender.Message) (sender.Ack, error) {
	ack, err := c.engine.Send(ctx, m)
	c.leave()
	return ack, err
}

type fixture struct {
	dir    string
	store  *ingest.Store
	clock  *clock
	engine *engine
	notes  *notes
	svc    *sender.Service
}

func newFixture(t testing.TB, perMinute, globalPerHour int) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), clock: &clock{now: epoch}, notes: &notes{}}
	f.store = openStore(t, f.dir)
	t.Cleanup(func() { _ = f.store.Close() })
	f.engine = newEngine(f.clock)
	f.svc = f.service(f.store, perMinute, globalPerHour)
	inbound(t, f.store, alice, bob)
	return f
}

func (f *fixture) service(st sender.Store, perMinute, globalPerHour int) *sender.Service {
	return sender.New(f.options(st, perMinute, globalPerHour))
}

func (f *fixture) options(st sender.Store, perMinute, globalPerHour int) sender.Options {
	return sender.Options{Store: st, Engine: f.engine, Notifier: f.notes, PerMinute: perMinute, GlobalPerHour: globalPerHour,
		Holders: func(context.Context, time.Time) (int, error) { return 1, nil }, Now: f.clock.Now, Pacer: func() time.Duration { return sender.PacerBase }}
}

func request(t tb, k, jid, text string) sender.Request {
	t.Helper()
	return sender.Request{Key: key(t, k), Chat: chat(t, jid), ChatRef: "ref:" + jid, Text: text}
}

func failure(err error) (*sender.Failure, bool) { return errors.AsType[*sender.Failure](err) }
