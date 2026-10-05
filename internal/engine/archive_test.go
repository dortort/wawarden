package engine

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const testDeadline = 5 * time.Minute

type pipeRig struct {
	*rig
	ingestOpts ingest.Options
	archive    *ingest.Store
	p          *pipeline
}

func newPipeRig(t *testing.T, options ...option) *pipeRig {
	t.Helper()
	r := &pipeRig{rig: newRig(t, options...)}
	r.ingestOpts = ingest.Options{DataDir: r.opts.DataDir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1, Logger: r.opts.Logger,
		ReadTimeout: testDeadline, WriteTimeout: testDeadline, RewriteTimeout: testDeadline}
	r.open()
	r.opts.Archive = r.archive
	r.p = newPipeline(r.opts)
	r.p.ctx = t.Context()
	return r
}

func (r *pipeRig) open() {
	r.t.Helper()
	s, err := ingest.Open(r.t.Context(), r.ingestOpts)
	if err != nil {
		r.t.Fatalf("ingest.Open: %v", err)
	}
	r.archive = s
	r.t.Cleanup(func() { _ = s.Close() })
}

func (r *pipeRig) reopen() {
	r.t.Helper()
	if err := r.archive.Close(); err != nil {
		r.t.Fatalf("Close: %v", err)
	}
	r.open()
	r.opts.Archive = r.archive
	r.p.archive = r.archive
}

func (r *pipeRig) restart() {
	r.t.Helper()
	r.reopen()
	r.reg = metrics.NewRegistry()
	r.opts.Metrics = r.reg
	r.p = newPipeline(r.opts)
	r.p.ctx = r.t.Context()
}

func (r *pipeRig) bulkInbox(n int) {
	r.t.Helper()
	if err := r.archive.Close(); err != nil {
		r.t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.opts.DataDir, "archive.db"))
	if err != nil {
		r.t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.ExecContext(r.t.Context(), "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) INSERT INTO inbox (received_at, payload) SELECT 0, x'7b7d' FROM n", n)
	if err = errors.Join(err, db.Close()); err != nil {
		r.t.Fatalf("bulk insert: %v", err)
	}
	r.open()
	r.opts.Archive = r.archive
	r.p.archive = r.archive
}

func (r *pipeRig) deliver(evs ...Event) {
	r.t.Helper()
	for _, ev := range evs {
		if !r.p.accept(ev) {
			r.t.Fatalf("%T was not acknowledged", ev)
		}
	}
}

func (r *pipeRig) drain() {
	r.t.Helper()
	r.p.drainInbox(r.t.Context())
}

func (r *pipeRig) ingest(evs ...Event) {
	r.t.Helper()
	for _, ev := range evs {
		r.deliver(ev)
		r.drain()
	}
}

func (r *pipeRig) write(fn func(*ingest.Tx) error) {
	r.t.Helper()
	if err := r.archive.Write(r.t.Context(), "test.write", fn); err != nil {
		r.t.Fatalf("Write: %v", err)
	}
}

func (r *pipeRig) dropped(reason string) float64 {
	return r.counter("wawarden_ingest_dropped_total", "reason", reason)
}

func (r *pipeRig) find(chatJID, id, senderJID string) (ingest.Found, bool) {
	r.t.Helper()
	var f ingest.Found
	var ok bool
	if err := r.archive.Read(r.t.Context(), "test.find", func(rd *ingest.Reader) error {
		var err error
		f, ok, err = rd.ResolveInChat(chat(r.t, chatJID), id, chat(r.t, senderJID))
		return err
	}); err != nil {
		r.t.Fatalf("Read: %v", err)
	}
	return f, ok
}

func (r *pipeRig) must(chatJID, id, senderJID string) ingest.Found {
	r.t.Helper()
	f, ok := r.find(chatJID, id, senderJID)
	if !ok {
		r.t.Fatalf("no message %s from %s in %s", id, senderJID, chatJID)
	}
	return f
}

func (r *pipeRig) inspect() *sql.DB {
	r.t.Helper()
	if err := r.archive.Close(); err != nil {
		r.t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.opts.DataDir, "archive.db")+"?mode=ro")
	if err != nil {
		r.t.Fatalf("sql.Open: %v", err)
	}
	r.t.Cleanup(func() { _ = db.Close() })
	return db
}

func query[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(t.Context(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func chat(t *testing.T, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q) rejected a test identifier", jid)
	}
	return c
}

func text(chatJID, id, sender, body string) Message {
	return Message{Chat: chatJID, ID: id, Sender: sender, Timestamp: epoch, Kind: KindText, Text: body}
}
