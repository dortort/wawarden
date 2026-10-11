package ingest

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/store/admin"
)

func TestPruneProcessedBlobsKeepsPendingAndQuarantinedRows(t *testing.T) {
	s := openStore(t)
	cutoff := epoch.Add(-30 * 24 * time.Hour)
	write(t, s, func(tx *Tx) error {
		for _, id := range []string{"old", "recent", "pending", "quarantined", "edge"} {
			if _, err := tx.RecordBlob(id, []byte("ref"), cutoff.Add(-time.Hour)); err != nil {
				return err
			}
		}
		if err := tx.MarkBlobProcessed("old", cutoff.Add(-time.Millisecond)); err != nil {
			return err
		}
		if err := tx.MarkBlobProcessed("edge", cutoff); err != nil {
			return err
		}
		if err := tx.MarkBlobProcessed("recent", epoch); err != nil {
			return err
		}
		return tx.QuarantineBlob("quarantined")
	})
	prune := func(at time.Time) int {
		t.Helper()
		var n int
		write(t, s, func(tx *Tx) error {
			var err error
			n, err = tx.PruneProcessedBlobs(at)
			return err
		})
		return n
	}
	write(t, s, func(tx *Tx) error {
		if _, err := tx.PruneProcessedBlobs(time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("PruneProcessedBlobs without a cutoff = %v", err)
		}
		return nil
	})
	if n := prune(cutoff); n != 1 {
		t.Fatalf("PruneProcessedBlobs = %d, want the one row processed before the cutoff", n)
	}
	if n := prune(cutoff); n != 0 {
		t.Fatalf("a second prune = %d", n)
	}
	if got := scalar[string](t, s, "SELECT group_concat(id, ',') FROM (SELECT id FROM history_blobs ORDER BY id)"); got != "edge,pending,quarantined,recent" {
		t.Fatalf("blobs after the prune %q", got)
	}
	if err := execAll(t, s, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10001) "+
		"INSERT INTO history_blobs (id, received_at, processed_at) SELECT 'bulk' || i, 1, 1 FROM n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n := prune(cutoff); n != PruneBatch {
		t.Fatalf("the first batch pruned %d rows, want %d", n, PruneBatch)
	}
	if n := prune(cutoff); n != 1 {
		t.Fatalf("the second batch pruned %d rows, want 1", n)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM history_blobs"); n != 4 {
		t.Fatalf("%d blobs after the batches, want 4", n)
	}
}

func TestChangesCountWritesAndSizeIsTheFile(t *testing.T) {
	opts := testOptions(t)
	s := openWith(t, opts)
	before, err := s.Changes(t.Context())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	read(t, s, func(r *Reader) error {
		_, _, err := r.SyncValue("last_ingest_at")
		return err
	})
	if again, err := s.Changes(t.Context()); err != nil || again != before {
		t.Fatalf("a read moved Changes from %d to %d (%v)", before, again, err)
	}
	insert(t, s, textMessage(t, alice, "M1", alice, "synthetic"))
	after, err := s.Changes(t.Context())
	if err != nil || after <= before {
		t.Fatalf("Changes after a write = %d, %v, want more than %d", after, err, before)
	}
	size, err := s.Size()
	fi, statErr := os.Stat(filepath.Join(opts.DataDir, "archive.db"))
	if err != nil || statErr != nil || size != fi.Size() || size == 0 {
		t.Fatalf("Size = %d, %v, want the file's %v", size, err, fi)
	}
}

func TestSpaceLeavesThePauseToCheckSpace(t *testing.T) {
	opts := testOptions(t)
	opts.MinFreeBytes = math.MaxUint64 / 2
	s := openWith(t, opts)
	for range 2 {
		free, floor, err := s.Space()
		if err != nil || floor != opts.MinFreeBytes || free == 0 {
			t.Fatalf("Space = %d, %d, %v", free, floor, err)
		}
	}
	space, err := s.CheckSpace()
	if err != nil || !space.Paused || !space.Changed {
		t.Fatalf("CheckSpace after Space = %+v, %v, want the pause edge still to come", space, err)
	}
	if _, _, err := s.Space(); err != nil {
		t.Fatalf("Space: %v", err)
	}
	if space, err := s.CheckSpace(); err != nil || !space.Paused || space.Changed {
		t.Fatalf("CheckSpace = %+v, %v, want the pause held without a new edge", space, err)
	}
}

func TestNewestAuditKeyID(t *testing.T) {
	opts := testOptions(t)
	opts.Master = testMaster(t)
	opts.AuditOut = &syncBuffer{}
	s := openWith(t, opts)
	if id, ok, err := s.NewestAuditKeyID(t.Context()); id != "" || ok || err != nil {
		t.Fatalf("NewestAuditKeyID of an empty chain = %q, %v, %v", id, ok, err)
	}
	if err := execAll(t, s, "INSERT INTO audit (ts, client_id, action, ok, reason, key_id, row_hmac) VALUES (1, 'aaaaaaaa', 'read', 1, 'ok', 'k-older', "+hash32+")"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if id, ok, err := s.NewestAuditKeyID(t.Context()); id != "k-older" || !ok || err != nil {
		t.Fatalf("NewestAuditKeyID = %q, %v, %v", id, ok, err)
	}
	if err := s.Audit().Record(t.Context(), admin.Event{At: epoch, Client: "aaaaaaaa", Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id, ok, err := s.NewestAuditKeyID(t.Context()); id != opts.Master.ID() || !ok || err != nil {
		t.Fatalf("NewestAuditKeyID = %q, %v, %v, want the current key", id, ok, err)
	}
}
