package scoped_test

import (
	"bytes"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	alice    = "15550100001@s.whatsapp.net"
	bob      = "15550100002@s.whatsapp.net"
	carol    = "15550100003@s.whatsapp.net"
	aliceLID = "100000000000001@lid"
	bobLID   = "100000000000002@lid"
	groupJID = "120363000000000001@g.us"
)

type tb interface {
	Helper()
	Fatalf(format string, args ...any)
}

var epoch = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

const testDeadline = 5 * time.Minute

func testOptions(dir string) ingest.Options {
	w := logx.NewWriter(&syncBuffer{})
	w.SetKey(make([]byte, 32))
	return ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1, Logger: logx.New(w, slog.LevelDebug),
		ReadTimeout: testDeadline, WriteTimeout: testDeadline, RewriteTimeout: testDeadline}
}

func openStore(t *testing.T) *ingest.Store {
	t.Helper()
	return openWith(t, testOptions(t.TempDir()))
}

func openWith(t *testing.T, opts ingest.Options) *ingest.Store {
	t.Helper()
	s, err := ingest.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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

func grant(t tb, jids ...string) policy.ReadGrant {
	t.Helper()
	read := map[policy.CanonicalChat]struct{}{}
	for _, jid := range jids {
		read[chat(t, jid)] = struct{}{}
	}
	g, ok := policy.DecideRead(&policy.Client{ID: "client01", Read: read, ExpiresAt: epoch.Add(24 * time.Hour)}, epoch)
	if !ok {
		t.Fatalf("DecideRead refused a live client")
	}
	return g
}

func grantAll(t tb) policy.ReadGrant {
	t.Helper()
	g, ok := policy.DecideRead(&policy.Client{ID: "client01", ReadAll: true, ExpiresAt: epoch.Add(24 * time.Hour)}, epoch)
	if !ok {
		t.Fatalf("DecideRead refused a live client")
	}
	return g
}

func write(t *testing.T, s *ingest.Store, fn func(*ingest.Tx) error) {
	t.Helper()
	if err := s.Write(t.Context(), "test.write", fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func message(t tb, chatJID, id, senderJID, text string, at time.Time) ingest.Message {
	t.Helper()
	return ingest.Message{Chat: chat(t, chatJID), ID: id, Sender: chat(t, senderJID), Origin: ingest.OriginLive,
		Timestamp: at, Kind: ingest.KindText, Text: text, Ingested: at}
}

func insert(t *testing.T, s *ingest.Store, ms ...ingest.Message) []ingest.Ref {
	t.Helper()
	var refs []ingest.Ref
	write(t, s, func(tx *ingest.Tx) error {
		for _, m := range ms {
			ref, _, err := tx.InsertMessage(m)
			if err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		return nil
	})
	return refs
}

func refOf(t *testing.T, s *ingest.Store, jid string) string {
	t.Helper()
	refs, err := s.Scoped().Column(t.Context(), "SELECT ref FROM chats WHERE jid = ?", jid)
	if err != nil || len(refs) != 1 {
		t.Fatalf("the reference of %s: %q, %v", jid, refs, err)
	}
	return refs[0]
}

func exec(t *testing.T, s *ingest.Store, stmts ...string) {
	t.Helper()
	if err := s.Scoped().Exec(t.Context(), stmts...); err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

func ids(ms []scoped.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
