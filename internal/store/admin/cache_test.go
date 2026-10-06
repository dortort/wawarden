package admin_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	cacheReadTimeout = 100 * time.Millisecond
	concurrentGuests = 30
)

func (f *fixture) holdWrite(t *testing.T) (release func()) {
	t.Helper()
	held, done, released := make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		done <- f.store.Write(t.Context(), "test.hold", func(*ingest.Tx) error {
			close(held)
			<-released
			return nil
		})
	}()
	<-held
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(released)
			if err := <-done; err != nil {
				t.Errorf("the held write = %v", err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

type answer struct {
	client *policy.Client
	ok     bool
	err    error
	took   time.Duration
}

func authenticateAll(f *fixture, t *testing.T, presented []string) []answer {
	t.Helper()
	out := make([]answer, len(presented))
	var wg sync.WaitGroup
	for i, p := range presented {
		wg.Go(func() {
			began := time.Now()
			c, ok, err := f.clients.Authenticate(t.Context(), p)
			out[i] = answer{client: c, ok: ok, err: err, took: time.Since(began)}
		})
	}
	wg.Wait()
	return out
}

func guesses(valid ...string) []string {
	out := append([]string(nil), valid...)
	for range concurrentGuests {
		out = append(out, "ww_not_a_token")
	}
	return out
}

func TestAuthenticationNeverWaitsOnTheArchiveAfterAChange(t *testing.T) {
	f := openTimed(t, t.TempDir(), syntheticMaster(t, 100), cacheReadTimeout)
	live, liveToken := f.create(t, policy.ClientSpec{Name: "live", Read: []string{phoneA}})
	revoked, revokedToken := f.create(t, policy.ClientSpec{Name: "revoked", Read: []string{phoneB}})
	if _, err := f.clients.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	other, _ := f.create(t, policy.ClientSpec{Name: "other", Read: []string{phoneB}})
	for _, step := range []struct {
		name   string
		change func(t *testing.T)
	}{
		{name: "a create", change: func(t *testing.T) { f.create(t, policy.ClientSpec{Name: "created", Read: []string{phoneB}}) }},
		{name: "a revoke", change: func(t *testing.T) {
			if _, err := f.clients.Revoke(t.Context(), other.ID); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
		}},
		{name: "a re-key", change: func(t *testing.T) {
			err := f.store.Write(t.Context(), "test.rekey", func(tx *ingest.Tx) error {
				res, err := tx.LearnLID(chat(t, lidA), chat(t, phoneA), ingest.MappingSenderAlt, start)
				if err == nil && !res.Rescoped {
					err = errors.New("the mapping moved no client chat")
				}
				return err
			})
			if err != nil {
				t.Fatalf("LearnLID: %v", err)
			}
		}},
		{name: "a restart", change: func(t *testing.T) {
			if err := f.store.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			reopened := openTimed(t, f.dir, f.master, cacheReadTimeout)
			f.store, f.clients = reopened.store, reopened.clients
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.change(t)
			release := f.holdWrite(t)
			began := time.Now()
			answers := authenticateAll(f, t, guesses(liveToken, revokedToken))
			if took := time.Since(began); took >= concurrentGuests*cacheReadTimeout/2 {
				t.Fatalf("%d authentications behind a held write took %v: they waited on the archive", len(answers), took)
			}
			release()
			if a := answers[0]; a.err != nil || !a.ok || a.client.ID != live.ID {
				t.Fatalf("the valid token behind a held write after %s = %+v, want it accepted from memory", step.name, a)
			}
			for i, a := range answers[1:] {
				if a.err != nil || a.ok || a.client != nil {
					t.Fatalf("guess %d behind a held write = %+v, want a refusal", i, a)
				}
			}
		})
	}
}

func TestAStaleClientListIsUnavailableWithoutQueueingAndReloads(t *testing.T) {
	f := openTimed(t, t.TempDir(), syntheticMaster(t, 100), cacheReadTimeout)
	live, liveToken := f.create(t, policy.ClientSpec{Name: "live", Read: []string{phoneA}})
	revoked, revokedToken := f.create(t, policy.ClientSpec{Name: "revoked", Read: []string{phoneB}})
	if _, err := f.clients.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	release := f.holdWrite(t)
	f.clients.MarkStale()
	began := time.Now()
	answers := authenticateAll(f, t, guesses(liveToken, revokedToken))
	if took := time.Since(began); took >= concurrentGuests*cacheReadTimeout/2 {
		t.Fatalf("%d authentications of a stale list took %v: they queued", len(answers), took)
	}
	for i, a := range answers {
		if !errors.Is(a.err, admin.ErrUnavailable) || a.ok {
			t.Fatalf("authentication %d of a stale list = %+v, want ErrUnavailable", i, a)
		}
	}
	ops := f.clients.CountOperations()
	for _, presented := range []string{liveToken, revokedToken, "ww_not_a_token"} {
		*ops = admin.Operations{}
		c, ok, err := f.clients.Authenticate(t.Context(), presented)
		if !errors.Is(err, admin.ErrUnavailable) || ok || c != nil {
			t.Fatalf("Authenticate while the list is stale = %v, %v, %v, want ErrUnavailable", c, ok, err)
		}
		if ops.Hashes != 1 || ops.Compares != 1 || ops.AgainstDummy != 1 {
			t.Fatalf("%+v while unavailable, want one hash and one compare against the dummy for every token", *ops)
		}
	}
	release()
	if c, ok := f.authenticate(t, liveToken); !ok || c.ID != live.ID {
		t.Fatalf("the valid token after the hold = %v, %v, want it accepted once the list reloaded", c, ok)
	}
	if _, ok := f.authenticate(t, revokedToken); ok {
		t.Fatal("a revoked token authenticated after the reload")
	}
}

func TestARolledBackChangeIsNeverPublished(t *testing.T) {
	f := open(t)
	live, liveToken := f.create(t, policy.ClientSpec{Name: "live", Read: []string{phoneA}})
	if err := f.clients.RollBack(t.Context(), "UPDATE clients SET revoked_at = 1"); !errors.Is(err, admin.ErrRolledBack) {
		t.Fatalf("RollBack = %v", err)
	}
	if c, ok := f.authenticate(t, liveToken); !ok || c.ID != live.ID {
		t.Fatalf("after a revoke that rolled back, the client = %v, %v, want it still accepted", c, ok)
	}
	if _, err := f.clients.Revoke(t.Context(), live.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := f.clients.RollBack(t.Context(), "UPDATE clients SET expires_at = expires_at + 1"); !errors.Is(err, admin.ErrRolledBack) {
		t.Fatalf("RollBack = %v", err)
	}
	if _, ok := f.authenticate(t, liveToken); ok {
		t.Fatal("a revoked client authenticated after an unrelated change rolled back")
	}
}
