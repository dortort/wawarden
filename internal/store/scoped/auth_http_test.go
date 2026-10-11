package scoped_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	authReadTimeout = 100 * time.Millisecond
	authGuesses     = 20
)

func storeClientHandler(t *testing.T, s *ingest.Store, reg *metrics.Registry) http.Handler {
	t.Helper()
	cursors, err := cursor.New(bytes.Repeat([]byte{0x31}, 32), "0a1b2c3d")
	if err != nil {
		t.Fatalf("cursor.New: %v", err)
	}
	refs, err := cursor.NewRef(bytes.Repeat([]byte{0x32}, 32), "0a1b2c3d")
	if err != nil {
		t.Fatalf("cursor.NewRef: %v", err)
	}
	return api.NewClientHandler(api.ClientDeps{
		Authenticator: s.Clients(), Metrics: reg, Archive: s.Scoped(), Audit: storeAudit{s.Audit()},
		Session: func() string { return "connected" }, Cursors: cursors, Refs: refs,
		Limits: api.ReadLimits{ReadsPerMinute: 1 << 30, SearchesPerMinute: 1 << 30},
	})
}

func authFailures(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	for line := range strings.Lines(b.String()) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "wawarden_auth_failures_total "); ok {
			return v
		}
	}
	return ""
}

func TestAValidClientBehindAHeldWriteIsBusyNeverUnauthorized(t *testing.T) {
	dir, master := t.TempDir(), syntheticMaster(t)
	open := func(t *testing.T) *ingest.Store {
		opts := testOptions(dir)
		opts.ReadTimeout, opts.Master, opts.AuditOut = authReadTimeout, master, &syncBuffer{}
		return openWith(t, opts)
	}
	s := open(t)
	insert(t, s, message(t, alice, "A1", alice, "synthetic", epoch))
	create := func(name, jid string) (string, string) {
		t.Helper()
		c, full, err := s.Clients().Create(t.Context(), policy.ClientSpec{Name: name, Read: []string{jid}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return c.ID, full
	}
	_, valid := create("agent", alice)
	revokedID, revoked := create("revoked", bob)
	if _, err := s.Clients().Revoke(t.Context(), revokedID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	otherID, _ := create("other", bob)
	for _, step := range []struct {
		name   string
		change func(t *testing.T)
	}{
		{name: "a create", change: func(t *testing.T) { create("created", bob) }},
		{name: "a revoke", change: func(t *testing.T) {
			if _, err := s.Clients().Revoke(t.Context(), otherID); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
		}},
		{name: "a re-key", change: func(t *testing.T) {
			write(t, s, func(tx *ingest.Tx) error {
				res, err := tx.LearnLID(chat(t, aliceLID), chat(t, alice), ingest.MappingSenderAlt, epoch)
				if err == nil && !res.Rescoped {
					err = errors.New("the mapping moved no client chat")
				}
				return err
			})
		}},
		{name: "a restart", change: func(t *testing.T) {
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			s = open(t)
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.change(t)
			reg := metrics.NewRegistry()
			h := storeClientHandler(t, s, reg)
			get := func(token string) *httptest.ResponseRecorder {
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/chats", nil)
				r.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				return rec
			}
			held, release := make(chan struct{}), make(chan struct{})
			done := holdUntilHeld(t, held, release, func() error { return s.Scoped().HoldWrite(context.Background(), held, release) })
			tokens := []string{valid, revoked}
			for range authGuesses {
				tokens = append(tokens, "ww_not_a_token")
			}
			answers, took := make([]*httptest.ResponseRecorder, len(tokens)), make([]time.Duration, len(tokens))
			var wg sync.WaitGroup
			for i, token := range tokens {
				wg.Go(func() {
					began := time.Now()
					answers[i] = get(token)
					took[i] = time.Since(began)
				})
			}
			wg.Wait()
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("the held write = %v", err)
			}
			if rec := answers[0]; rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"error":"busy"}` || rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("a valid client behind a held write after %s = %d %s, Retry-After %q, want 503 busy", step.name, rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
			}
			for i, rec := range answers[1:] {
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("guess %d behind a held write = %d %s, want 401", i, rec.Code, rec.Body.String())
				}
			}
			if slowest, limit := slices.Max(took[1:]), authGuesses*authReadTimeout/2; slowest >= limit {
				t.Fatalf("the slowest of %d refusals behind a held write took %v, want under %v: they waited on the archive", len(tokens)-1, slowest, limit)
			}
			if got, want := authFailures(t, reg), "21"; got != want {
				t.Fatalf("authentication failures = %s, want %s: only the revoked token and the guesses fail", got, want)
			}
			rec := get(valid)
			for range 50 {
				if rec.Code != http.StatusServiceUnavailable {
					break
				}
				rec = get(valid)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("the valid client after the release = %d %s", rec.Code, rec.Body.String())
			}
			if rec := get(revoked); rec.Code != http.StatusUnauthorized {
				t.Fatalf("the revoked client after the release = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
