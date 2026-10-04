package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	canary      = "Zq7xKv9wPj3mRt5nLb8cGy4hD"
	replacement = "an ordinary replacement sentence"
)

var controls = []string{"good morning, see you at noon", "the meeting moved to friday", "thanks, that works for me"}

func trigrams(s string) []string {
	r := []rune(s)
	var out []string
	for i := 0; i+3 <= len(r); i++ {
		out = append(out, string(r[i:i+3]))
	}
	return out
}

type proof struct {
	t        *testing.T
	s        *Store
	dir      string
	trigrams []string
}

func newProof(t *testing.T) *proof {
	t.Helper()
	opts := testOptions(t)
	opts.readTimeout = time.Minute
	s := openWith(t, opts)
	for i, text := range controls {
		insert(t, s, textMessage(t, groupJID, fmt.Sprintf("CTRL%d", i), bob, text))
	}
	p := &proof{t: t, s: s, dir: opts.DataDir}
	baseline := p.files()
	for _, tri := range trigrams(canary) {
		for _, form := range []string{tri, strings.ToLower(tri)} {
			if !bytes.Contains(baseline, []byte(form)) && !slices.Contains(p.trigrams, form) {
				p.trigrams = append(p.trigrams, form)
			}
		}
	}
	if len(p.trigrams) < len(trigrams(canary)) {
		t.Fatalf("only %d of the canary's trigrams are distinctive", len(p.trigrams))
	}
	return p
}

func TestTheProofsReadDeadlineReachesTheDatabase(t *testing.T) {
	opts := testOptions(t)
	opts.readTimeout = time.Nanosecond
	s, err := Open(t.Context(), opts)
	if err == nil {
		_ = s.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open with a read deadline of 1 ns = %v, want the schema read to run out of time", err)
	}
}

func (p *proof) files() []byte {
	p.t.Helper()
	var all []byte
	for _, name := range []string{"archive.db", "archive.db-journal"} {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(p.dir, name)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.t.Fatalf("read %s: %v", name, err)
		}
		all = append(all, data...)
	}
	return all
}

func (p *proof) leaks() []string {
	p.t.Helper()
	var out []string
	if n := matches(p.t, p.s, canary); n > 0 {
		out = append(out, fmt.Sprintf("MATCH finds %d rows", n))
	}
	read(p.t, p.s, func(r *Reader) error {
		if _, err := r.q.ExecContext(r.ctx, "CREATE VIRTUAL TABLE temp.vocab USING fts5vocab(main, messages_fts, 'row')"); err != nil {
			return err
		}
		for _, tri := range trigrams(strings.ToLower(canary)) {
			var n int
			if err := r.q.QueryRowContext(r.ctx, "SELECT count(*) FROM temp.vocab WHERE term = ?", tri).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				out = append(out, "fts5vocab holds "+tri)
			}
		}
		var n int
		if err := r.q.QueryRowContext(r.ctx, "SELECT count(*) FROM messages WHERE instr(text, ?) OR instr(text_display, ?)", canary, canary).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			out = append(out, "a message row holds the text")
		}
		return nil
	})
	raw := p.files()
	if bytes.Contains(raw, []byte(canary)) {
		out = append(out, "the raw bytes hold the whole text")
	}
	for _, tri := range p.trigrams {
		if bytes.Contains(raw, []byte(tri)) {
			out = append(out, "the raw bytes hold "+tri)
		}
	}
	return out
}

func (p *proof) plant() Ref {
	p.t.Helper()
	var seq int64
	write(p.t, p.s, func(tx *Tx) error {
		var err error
		seq, err = tx.AppendInbox([]byte(`{"text":"`+canary+`"}`), epoch)
		return err
	})
	m := textMessage(p.t, alice, "CANARY", alice, "before "+canary+" after")
	m.ExpiresAt = epoch.Add(time.Hour)
	var ref Ref
	write(p.t, p.s, func(tx *Tx) error {
		var err error
		if ref, _, err = tx.InsertMessage(m); err != nil {
			return err
		}
		return tx.DeleteInbox(seq)
	})
	if leaks := p.leaks(); !slices.Contains(leaks, "the raw bytes hold the whole text") || !slices.ContainsFunc(leaks, func(l string) bool { return strings.HasPrefix(l, "MATCH") }) ||
		!slices.ContainsFunc(leaks, func(l string) bool { return strings.HasPrefix(l, "fts5vocab") }) {
		p.t.Fatalf("the planted canary is not detected (%q), so its absence would prove nothing", leaks)
	}
	return ref
}

func (p *proof) assertGone() {
	p.t.Helper()
	if leaks := p.leaks(); len(leaks) > 0 {
		p.t.Fatalf("the canary survives on disk while the database is open: %q", leaks)
	}
	ftsIntegrity(p.t, p.s)
	if err := p.s.Close(); err != nil {
		p.t.Fatalf("Close: %v", err)
	}
	raw := p.files()
	if bytes.Contains(raw, []byte(canary)) || slices.ContainsFunc(p.trigrams, func(tri string) bool { return bytes.Contains(raw, []byte(tri)) }) {
		p.t.Fatal("the canary survives in the closed database file or its journal")
	}
}

func (p *proof) controlsSurvive() {
	p.t.Helper()
	for _, text := range controls {
		if matches(p.t, p.s, text) != 1 {
			p.t.Fatalf("the control message %q is no longer found: the deletion went too far", text)
		}
	}
}

func TestRevokedTextIsGoneFromDisk(t *testing.T) {
	p := newProof(t)
	ref := p.plant()
	write(t, p.s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	p.controlsSurvive()
	p.assertGone()
}

func TestExpiredTextIsGoneFromDisk(t *testing.T) {
	p := newProof(t)
	p.plant()
	write(t, p.s, func(tx *Tx) error {
		n, err := tx.PurgeExpired(epoch.Add(2*time.Hour), 10)
		if err == nil && n != 1 {
			err = fmt.Errorf("purged %d messages, want the canary", n)
		}
		return err
	})
	p.controlsSurvive()
	p.assertGone()
}

func TestEditedTextIsGoneFromDisk(t *testing.T) {
	p := newProof(t)
	ref := p.plant()
	write(t, p.s, func(tx *Tx) error { return tx.ApplyEdit(ref, replacement, epoch.Add(time.Minute)) })
	if matches(t, p.s, replacement) != 1 {
		t.Fatal("the edited text is not indexed")
	}
	p.controlsSurvive()
	p.assertGone()
}

func TestTheDeletionProofDetectsAWeakerProcedure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		before string
		revoke func(t *testing.T, p *proof, ref Ref)
		want   string
	}{
		{
			name:   "without secure_delete",
			before: "PRAGMA secure_delete = OFF",
			revoke: func(t *testing.T, p *proof, ref Ref) {
				write(t, p.s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
			},
			want: "the raw bytes hold the whole text",
		},
		{
			name:   "without the full-text secure-delete option",
			before: "INSERT INTO messages_fts (messages_fts, rank) VALUES ('secure-delete', 0)",
			revoke: func(t *testing.T, p *proof, ref Ref) {
				write(t, p.s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
			},
			want: "the raw bytes hold ",
		},
		{
			name: "without the full-text delete command",
			revoke: func(t *testing.T, p *proof, ref Ref) {
				write(t, p.s, func(tx *Tx) error {
					if _, err := tx.q.ExecContext(tx.ctx, forgetText, ref.seq); err != nil {
						return err
					}
					if _, err := tx.q.ExecContext(tx.ctx, setRevoked, ref.seq); err != nil {
						return err
					}
					_, err := tx.q.ExecContext(tx.ctx, insertFTS, ref.seq, nil)
					return err
				})
			},
			want: "MATCH finds",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newProof(t)
			if tt.before != "" {
				write(t, p.s, func(tx *Tx) error {
					_, err := tx.q.ExecContext(tx.ctx, tt.before)
					return err
				})
			}
			ref := p.plant()
			tt.revoke(t, p, ref)
			if leaks := p.leaks(); !slices.ContainsFunc(leaks, func(l string) bool { return strings.HasPrefix(l, tt.want) }) {
				t.Fatalf("leaks %q, want one starting %q: the proof would pass a procedure %s", leaks, tt.want, tt.name)
			}
		})
	}
}
