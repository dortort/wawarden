package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

const (
	fillerCount    = 3000
	canaryAlphabet = "АБВГДЕЖЗИЙКЛМНОПабвгдежзийклмноп"
	replacement    = "an ordinary replacement sentence"
)

var (
	controls   = []string{"good morning, see you at noon", "the meeting moved to friday", "thanks, that works for me"}
	canary     = synthetic(2, canaryAlphabet, 1500)
	companions = func() []string {
		out := make([]string, 25)
		for i := range out {
			out[i] = synthetic(uint64(100+i), canaryAlphabet, 60)
		}
		return out
	}()

	baselineOnce sync.Once
	baselineFile []byte
	baselineErr  error
)

func synthetic(seed uint64, alphabet string, n int) string {
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // G404: a fixed seed keeps the synthetic test data and its index layout reproducible
	letters := []rune(alphabet)
	out := make([]rune, n)
	for i := range out {
		out[i] = letters[r.IntN(len(letters))]
	}
	return string(out)
}

func trigrams(s string) []string {
	r := []rune(s)
	var out []string
	for i := 0; i+3 <= len(r); i++ {
		out = append(out, string(r[i:i+3]))
	}
	return out
}

func hits(raw []byte, set map[string]bool) []string {
	sizes := map[int]bool{}
	var leads [256]bool
	for form := range set {
		sizes[len(form)] = true
		leads[form[0]] = true
	}
	found := map[string]bool{}
	for size := range sizes {
		for i := 0; i+size <= len(raw); i++ {
			if leads[raw[i]] && set[string(raw[i:i+size])] {
				found[string(raw[i:i+size])] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for tri := range found {
		out = append(out, tri)
	}
	slices.Sort(out)
	return out
}

func buildBaseline(dir string) ([]byte, error) {
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: dir, UID: os.Geteuid(), Profile: ProfileLocal, MinFreeBytes: 1, Logger: testLogger(),
		ReadTimeout: testDeadline, WriteTimeout: testDeadline, RewriteTimeout: testDeadline})
	if err != nil {
		return nil, err
	}
	group, _ := policy.Normalize(groupJID)
	sender, _ := policy.Normalize(bob)
	texts := slices.Clone(controls)
	for i := range fillerCount {
		texts = append(texts, synthetic(uint64(1000+i), "abcdefghijklm ", 40))
	}
	for i, text := range texts {
		m := Message{Chat: group, ID: fmt.Sprintf("CTRL%d", i), Sender: sender, Origin: OriginLive, Timestamp: epoch, Kind: KindText, Text: text, Ingested: epoch}
		if err := s.Write(ctx, "test.baseline", func(tx *Tx) error {
			_, _, err := tx.InsertMessage(m)
			return err
		}); err != nil {
			return nil, errors.Join(err, s.Close())
		}
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Clean(filepath.Join(dir, "archive.db")))
}

func baseline(t *testing.T) []byte {
	t.Helper()
	baselineOnce.Do(func() { baselineFile, baselineErr = buildBaseline(t.TempDir()) })
	if baselineErr != nil {
		t.Fatalf("build the baseline archive: %v", baselineErr)
	}
	return baselineFile
}

type proof struct {
	t        *testing.T
	s        *Store
	opts     Options
	trigrams map[string]bool
	terms    map[string]bool
	segments []string
}

func newProof(t *testing.T) *proof {
	t.Helper()
	base := baseline(t)
	opts := testOptions(t)
	if err := os.WriteFile(filepath.Join(opts.DataDir, "archive.db"), base, 0o600); err != nil {
		t.Fatalf("copy the baseline archive: %v", err)
	}
	p := &proof{t: t, s: openWith(t, opts), opts: opts, trigrams: map[string]bool{}, terms: map[string]bool{}}
	forms := map[string]bool{}
	for _, tri := range trigrams(canary) {
		forms[tri], forms[strings.ToLower(tri)] = true, true
	}
	kept := map[string]bool{}
	for _, tri := range hits(base, forms) {
		kept[tri] = true
	}
	for _, text := range companions {
		for _, tri := range trigrams(text + text) {
			kept[tri], kept[strings.ToLower(tri)] = true, true
		}
	}
	for form := range forms {
		if !kept[form] {
			p.trigrams[form] = true
		}
	}
	for _, tri := range trigrams(strings.ToLower(canary)) {
		if p.trigrams[tri] {
			p.terms[tri] = true
		}
	}
	if len(p.trigrams) < len(trigrams(canary)) || len(p.terms) < len(trigrams(canary))/2 {
		t.Fatalf("only %d forms and %d index terms of the canary are distinctive", len(p.trigrams), len(p.terms))
	}
	return p
}

func TestTheProofsReadDeadlineReachesTheDatabase(t *testing.T) {
	opts := testOptions(t)
	opts.ReadTimeout = time.Nanosecond
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
		data, err := os.ReadFile(filepath.Clean(filepath.Join(p.opts.DataDir, name)))
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
	if n := matches(p.t, p.s, string([]rune(canary)[:40])); n > 0 {
		out = append(out, fmt.Sprintf("MATCH finds %d rows", n))
	}
	read(p.t, p.s, func(r *Reader) error {
		if _, err := r.q.ExecContext(r.ctx, "CREATE VIRTUAL TABLE temp.vocab USING fts5vocab(main, messages_fts, 'row')"); err != nil {
			return err
		}
		terms, err := column(r, "SELECT term FROM temp.vocab WHERE term >= ?", "а")
		if err != nil {
			return err
		}
		for _, term := range terms {
			if p.terms[term] {
				out = append(out, "fts5vocab holds "+term)
			}
		}
		keys, err := column(r, "SELECT term FROM messages_fts_idx")
		if err != nil {
			return err
		}
		for _, key := range keys {
			if len(key) > 1 && key[0] == '0' && p.terms[key[1:]] {
				out = append(out, "an index page key holds "+key[1:])
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
	for _, tri := range hits(raw, p.trigrams) {
		out = append(out, "the raw bytes hold "+tri)
	}
	return out
}

func column(r *Reader, query string, args ...any) ([]string, error) {
	rows, err := r.q.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var v []byte
		if err := rows.Scan(&v); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, string(v))
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func leaksWith(leaks []string, prefix string) bool {
	return slices.ContainsFunc(leaks, func(l string) bool { return strings.HasPrefix(l, prefix) })
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
	for i, text := range companions {
		if i == len(companions)/2 {
			write(p.t, p.s, func(tx *Tx) error {
				var err error
				if ref, _, err = tx.InsertMessage(m); err != nil {
					return err
				}
				return tx.DeleteInbox(seq)
			})
		}
		insert(p.t, p.s, textMessage(p.t, alice, fmt.Sprintf("COMPANION%d", i), alice, text))
	}
	p.detected()
	return ref
}

func (p *proof) detected() {
	p.t.Helper()
	if keys := scalar[int](p.t, p.s, "SELECT count(*) FROM messages_fts_idx"); keys == 0 {
		p.t.Fatal("the full-text index has a single page per segment, so its page keys are not under test")
	}
	leaks := p.leaks()
	for _, want := range []string{"the raw bytes hold the whole text", "MATCH", "fts5vocab", "an index page key holds"} {
		if !leaksWith(leaks, want) {
			p.t.Fatalf("the planted canary is not detected by %q, so its absence would prove nothing", want)
		}
	}
	p.segments = p.segmentIDs()
}

func (p *proof) segmentIDs() []string {
	p.t.Helper()
	var ids []string
	read(p.t, p.s, func(r *Reader) error {
		var err error
		ids, err = column(r, "SELECT DISTINCT segid FROM messages_fts_idx")
		return err
	})
	return ids
}

func (p *proof) rewritten() {
	p.t.Helper()
	if now := p.segmentIDs(); len(now) != 1 || slices.Contains(p.segments, now[0]) {
		p.t.Fatalf("the index has segments %v after the deletion and had %v before: the deletion left no stale page key to rewrite, so this test proves nothing", now, p.segments)
	}
}

func (p *proof) assertGone() {
	p.t.Helper()
	if leaks := p.leaks(); len(leaks) > 0 {
		p.t.Fatalf("the canary survives on disk while the database is open: %q", leaks)
	}
	if _, due := p.syncValue(rewriteDueKey); due {
		p.t.Fatal("an index rewrite is still due")
	}
	ftsIntegrity(p.t, p.s)
	if err := p.s.Close(); err != nil {
		p.t.Fatalf("Close: %v", err)
	}
	raw := p.files()
	if bytes.Contains(raw, []byte(canary)) || len(hits(raw, p.trigrams)) > 0 {
		p.t.Fatalf("the canary survives in the closed database file or its journal: %q", hits(raw, p.trigrams))
	}
}

func (p *proof) syncValue(key string) (string, bool) {
	p.t.Helper()
	var v string
	var ok bool
	read(p.t, p.s, func(r *Reader) error {
		var err error
		v, ok, err = r.SyncValue(key)
		return err
	})
	return v, ok
}

func (p *proof) controlsSurvive() {
	p.t.Helper()
	for _, text := range slices.Concat(controls, companions) {
		if matches(p.t, p.s, text) != 1 {
			p.t.Fatalf("the control message %q is no longer found: the deletion went too far", text)
		}
	}
}

func (p *proof) writeWithoutTheRewrite(fn func(*Tx) error) {
	p.t.Helper()
	if err := p.s.db.Write(p.t.Context(), "test.write", func(ctx context.Context, q db.Querier) error {
		return fn(&Tx{Reader: Reader{ctx: ctx, q: q}})
	}); err != nil {
		p.t.Fatalf("Write: %v", err)
	}
}

func TestRevokedTextIsGoneFromDisk(t *testing.T) {
	p := newProof(t)
	ref := p.plant()
	write(t, p.s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	p.rewritten()
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
	p.rewritten()
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
	p.rewritten()
	p.controlsSurvive()
	p.assertGone()
}

func TestRevokedTextIsGoneFromAnIndexOfOneSegment(t *testing.T) {
	p := newProof(t)
	ref := p.plant()
	write(t, p.s, func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, optimizeIndex)
		return err
	})
	if n := scalar[int](t, p.s, "SELECT count(DISTINCT segid) FROM messages_fts_idx"); n != 1 {
		t.Fatalf("%d segments after optimize, want one", n)
	}
	p.detected()
	write(t, p.s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	p.rewritten()
	p.controlsSurvive()
	p.assertGone()
}

func TestAnIndexRewriteCutShortRunsAtTheNextOpen(t *testing.T) {
	p := newProof(t)
	ref := p.plant()
	p.writeWithoutTheRewrite(func(tx *Tx) error {
		if err := tx.ApplyRevoke(ref); err != nil {
			return err
		}
		due, err := tx.markStaleKeys()
		if err == nil && !due {
			err = errors.New("the revoke leaves no stale page key, so this test proves nothing")
		}
		return err
	})
	if !leaksWith(p.leaks(), "an index page key holds") {
		t.Fatal("the revoke without its rewrite leaves no stale page key, so this test proves nothing")
	}
	if v, due := p.syncValue(rewriteDueKey); !due || v != "1" {
		t.Fatalf("index rewrite due = %q, %v after the revoke committed, want it recorded", v, due)
	}
	if err := p.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	instant := p.opts
	instant.WriteTimeout = time.Nanosecond
	reopened, err := Open(t.Context(), instant)
	if err != nil {
		t.Fatalf("Open with a write deadline of 1 ns = %v, want the rewrite to run under its own deadline", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p.s = openWith(t, p.opts)
	p.rewritten()
	p.controlsSurvive()
	p.assertGone()
}

func TestAFailedIndexRewriteStaysDueUntilTheNextOpen(t *testing.T) {
	p := newProof(t)
	ref := p.plant()
	if err := p.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	failing := p.opts
	failing.RewriteTimeout = time.Nanosecond
	p.s = openWith(t, failing)
	err := p.s.Write(t.Context(), "test.revoke", func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	if !errors.Is(err, ErrRewritePending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a revoke whose rewrite runs out of time = %v, want ErrRewritePending wrapping the deadline", err)
	}
	if found, ok := resolve(t, p.s, alice, "CANARY", alice); !ok || !found.Revoked {
		t.Fatal("the revoke did not commit")
	}
	if !leaksWith(p.leaks(), "an index page key holds") {
		t.Fatal("the failed rewrite leaves no stale page key, so this test proves nothing")
	}
	if _, due := p.syncValue(rewriteDueKey); !due {
		t.Fatal("the failed rewrite is not recorded as due")
	}
	if err := p.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p.s = openWith(t, p.opts)
	p.rewritten()
	p.controlsSurvive()
	p.assertGone()
}

func TestTheIndexRewriteKeyIsReserved(t *testing.T) {
	s := openStore(t)
	err := s.Write(t.Context(), "test.write", func(tx *Tx) error { return tx.SetSyncValue(rewriteDueKey, "0") })
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("SetSyncValue(%q) = %v, want it refused", rewriteDueKey, err)
	}
}

func pastThePageKeys(leaks []string) bool {
	keys := map[string]bool{}
	for _, l := range leaks {
		if key, ok := strings.CutPrefix(l, "an index page key holds "); ok {
			keys[key] = true
		}
	}
	return slices.ContainsFunc(leaks, func(l string) bool {
		tri, ok := strings.CutPrefix(l, "the raw bytes hold ")
		return ok && tri != "the whole text" && !keys[strings.ToLower(tri)]
	})
}

func TestTheDeletionProofDetectsAWeakerProcedure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		before string
		revoke func(t *testing.T, p *proof, ref Ref)
		want   string
		found  func([]string) bool
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
			name:   "without the full-text secure-delete option, where no rewrite follows",
			before: "INSERT INTO messages_fts (messages_fts, rank) VALUES ('secure-delete', 0)",
			revoke: func(_ *testing.T, p *proof, ref Ref) {
				p.writeWithoutTheRewrite(func(tx *Tx) error { return tx.ApplyRevoke(ref) })
			},
			found: pastThePageKeys,
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
		{
			name: "without the index rewrite",
			revoke: func(_ *testing.T, p *proof, ref Ref) {
				p.writeWithoutTheRewrite(func(tx *Tx) error { return tx.ApplyRevoke(ref) })
			},
			want: "an index page key holds ",
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
			leaks := p.leaks()
			if tt.found == nil && !leaksWith(leaks, tt.want) {
				t.Fatalf("leaks %q, want one starting %q: the proof would pass a procedure %s", leaks, tt.want, tt.name)
			}
			if tt.found != nil && !tt.found(leaks) {
				t.Fatalf("leaks %q, want raw trigrams that no stale page key explains: the proof would pass a procedure %s", leaks, tt.name)
			}
		})
	}
}
