package ingest

import (
	"bytes"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	alice     = "15550100001@s.whatsapp.net"
	bob       = "15550100002@s.whatsapp.net"
	carol     = "15550100003@s.whatsapp.net"
	aliceLID  = "100000000000001@lid"
	bobLID    = "100000000000002@lid"
	carolLID  = "100000000000003@lid"
	ownerJID  = "15550100009@s.whatsapp.net"
	groupJID  = "120363000000000001@g.us"
	group2JID = "120363000000000002@g.us"
)

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

func testLogger() *slog.Logger {
	w := logx.NewWriter(&syncBuffer{})
	w.SetKey(make([]byte, 32))
	return logx.New(w, slog.LevelDebug)
}

func chat(t *testing.T, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q) rejected a test identifier", jid)
	}
	return c
}

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ProfileLocal, Logger: testLogger()}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	return openWith(t, testOptions(t))
}

func openWith(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func write(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	if err := s.Write(t.Context(), "test.write", fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func read(t *testing.T, s *Store, fn func(*Reader) error) {
	t.Helper()
	if err := s.Read(t.Context(), "test.read", fn); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

func scalar[T any](t *testing.T, s *Store, query string, args ...any) T {
	t.Helper()
	var v T
	read(t, s, func(r *Reader) error { return r.q.QueryRowContext(r.ctx, query, args...).Scan(&v) })
	return v
}

func textMessage(t *testing.T, chatJID, id, senderJID, text string) Message {
	t.Helper()
	return Message{
		Chat: chat(t, chatJID), ID: id, Sender: chat(t, senderJID), Origin: OriginLive,
		Timestamp: epoch, Kind: KindText, Text: text, Ingested: epoch,
	}
}

func insert(t *testing.T, s *Store, m Message) Ref {
	t.Helper()
	var ref Ref
	write(t, s, func(tx *Tx) error {
		var err error
		ref, _, err = tx.InsertMessage(m)
		return err
	})
	return ref
}

func resolve(t *testing.T, s *Store, chatJID, id, senderJID string) (Found, bool) {
	t.Helper()
	var f Found
	var ok bool
	read(t, s, func(r *Reader) error {
		var err error
		f, ok, err = r.ResolveInChat(chat(t, chatJID), id, chat(t, senderJID))
		return err
	})
	return f, ok
}

func ftsIntegrity(t *testing.T, s *Store) {
	t.Helper()
	write(t, s, func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, "INSERT INTO messages_fts (messages_fts, rank) VALUES ('integrity-check', 1)")
		return err
	})
}

func matches(t *testing.T, s *Store, phrase string) int {
	t.Helper()
	return scalar[int](t, s, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?", `"`+phrase+`"`)
}
