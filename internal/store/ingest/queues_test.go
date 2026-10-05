package ingest

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestInboxKeepsOrderAttemptsAndQuarantine(t *testing.T) {
	s := openStore(t)
	var first, second int64
	write(t, s, func(tx *Tx) error {
		var err error
		if first, err = tx.AppendInbox([]byte("first event"), epoch); err != nil {
			return err
		}
		second, err = tx.AppendInbox([]byte("second event"), epoch.Add(time.Second))
		return err
	})
	next := func() (InboxItem, bool) {
		var item InboxItem
		var ok bool
		read(t, s, func(r *Reader) error {
			var err error
			item, ok, err = r.NextInbox()
			return err
		})
		return item, ok
	}
	if item, ok := next(); !ok || item.Seq != first || string(item.Payload) != "first event" || item.Attempts != 0 || !item.ReceivedAt.Equal(epoch) {
		t.Fatalf("NextInbox = %+v, %v", item, ok)
	}
	for want := 1; want <= 3; want++ {
		var attempts int
		write(t, s, func(tx *Tx) error {
			var err error
			attempts, err = tx.RecordInboxAttempt(first)
			return err
		})
		if attempts != want {
			t.Fatalf("RecordInboxAttempt = %d, want %d", attempts, want)
		}
	}
	write(t, s, func(tx *Tx) error { return tx.QuarantineInbox(first) })
	if payload := scalar[[]byte](t, s, "SELECT payload FROM inbox WHERE seq = ?", first); payload != nil {
		t.Fatal("a quarantined inbox row kept its payload")
	}
	if item, ok := next(); !ok || item.Seq != second {
		t.Fatalf("NextInbox after a quarantine = %+v, %v, want the second row", item, ok)
	}
	if err := s.Write(t.Context(), "test.attempt", func(tx *Tx) error { _, err := tx.RecordInboxAttempt(first); return err }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an attempt on a quarantined row = %v, want ErrNotFound", err)
	}
	write(t, s, func(tx *Tx) error { return tx.DeleteInbox(second) })
	if _, ok := next(); ok {
		t.Fatal("NextInbox returned a row after the last one was deleted")
	}
	for name, call := range map[string]func(*Tx) error{
		"delete a missing row":     func(tx *Tx) error { return tx.DeleteInbox(second) },
		"quarantine a missing row": func(tx *Tx) error { return tx.QuarantineInbox(99) },
	} {
		if err := s.Write(t.Context(), "test.inbox", call); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
	for name, call := range map[string]func(*Tx) error{
		"an empty payload": func(tx *Tx) error { _, err := tx.AppendInbox(nil, epoch); return err },
		"no time":          func(tx *Tx) error { _, err := tx.AppendInbox([]byte("x"), time.Time{}); return err },
	} {
		if err := s.Write(t.Context(), "test.inbox", call); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestInboxBacklogIsBounded(t *testing.T) {
	s := openStore(t)
	write(t, s, func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?) INSERT INTO inbox (received_at, payload) SELECT ?, x'00' FROM c", MaxInboxBacklog-1, ms(epoch))
		return err
	})
	var last int64
	write(t, s, func(tx *Tx) error {
		var err error
		last, err = tx.AppendInbox([]byte("the last that fits"), epoch)
		return err
	})
	full := func() error {
		return s.Write(t.Context(), "test.append", func(tx *Tx) error { _, err := tx.AppendInbox([]byte("one too many"), epoch); return err })
	}
	if err := full(); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("appending to a backlog of %d = %v, want ErrInboxFull", MaxInboxBacklog, err)
	}
	write(t, s, func(tx *Tx) error { return tx.QuarantineInbox(last) })
	if err := full(); err != nil {
		t.Fatalf("appending once a row left the backlog = %v", err)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM inbox WHERE quarantined = 0"); n != MaxInboxBacklog {
		t.Fatalf("backlog %d, want %d", n, MaxInboxBacklog)
	}
}

func TestHistoryBlobs(t *testing.T) {
	s := openStore(t)
	write(t, s, func(tx *Tx) error {
		for i, id := range []string{"blob-b", "blob-a", "blob-c"} {
			inserted, err := tx.RecordBlob(id, []byte("ref "+id), epoch.Add(time.Duration(i%2)*time.Minute))
			if err != nil || !inserted {
				return errors.Join(err, errors.New("not inserted"))
			}
		}
		inserted, err := tx.RecordBlob("blob-a", []byte("another ref"), epoch)
		if err != nil || inserted {
			return errors.Join(err, errors.New("a known blob was recorded again"))
		}
		return nil
	})
	pending := func() []Blob {
		var out []Blob
		read(t, s, func(r *Reader) error {
			var err error
			out, err = r.PendingBlobs(10)
			return err
		})
		return out
	}
	var ids []string
	for _, b := range pending() {
		ids = append(ids, b.ID)
	}
	if strings.Join(ids, ",") != "blob-b,blob-c,blob-a" {
		t.Fatalf("pending blobs %v, want them by arrival", ids)
	}
	if b := pending()[2]; string(b.Ref) != "ref blob-a" || b.Attempts != 0 || !b.ReceivedAt.Equal(epoch.Add(time.Minute)) {
		t.Fatalf("pending blob %+v", b)
	}
	var attempts int
	write(t, s, func(tx *Tx) error {
		var err error
		attempts, err = tx.RecordBlobAttempt("blob-b")
		return err
	})
	if attempts != 1 {
		t.Fatalf("RecordBlobAttempt = %d", attempts)
	}
	write(t, s, func(tx *Tx) error { return tx.ReleaseBlobAttempt("blob-b") })
	if b := pending()[0]; b.ID != "blob-b" || b.Attempts != 0 {
		t.Fatalf("pending blob %+v after its attempt was released", b)
	}
	if err := s.Write(t.Context(), "test.blob", func(tx *Tx) error { return tx.ReleaseBlobAttempt("blob-b") }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("releasing an attempt not recorded = %v, want ErrNotFound", err)
	}
	write(t, s, func(tx *Tx) error {
		_, err := tx.RecordBlobAttempt("blob-b")
		return err
	})
	write(t, s, func(tx *Tx) error {
		if err := tx.MarkBlobProcessed("blob-b", epoch.Add(time.Hour)); err != nil {
			return err
		}
		return tx.QuarantineBlob("blob-c")
	})
	if n := scalar[int](t, s, "SELECT count(*) FROM history_blobs WHERE ref IS NOT NULL"); n != 1 {
		t.Fatalf("%d blobs keep their download reference, want only the pending one", n)
	}
	if left := pending(); len(left) != 1 || left[0].ID != "blob-a" {
		t.Fatalf("pending blobs %+v, want only blob-a", left)
	}
	for id, want := range map[string]bool{"blob-a": true, "blob-b": false, "blob-c": false, "blob-z": false} {
		var got bool
		read(t, s, func(r *Reader) error {
			var err error
			got, err = r.BlobPending(id)
			return err
		})
		if got != want {
			t.Errorf("BlobPending(%q) = %v, want %v", id, got, want)
		}
	}
	for name, call := range map[string]func(*Tx) error{
		"process a quarantined blob":  func(tx *Tx) error { return tx.MarkBlobProcessed("blob-c", epoch) },
		"process a processed blob":    func(tx *Tx) error { return tx.MarkBlobProcessed("blob-b", epoch) },
		"quarantine a processed blob": func(tx *Tx) error { return tx.QuarantineBlob("blob-b") },
		"attempt a processed blob":    func(tx *Tx) error { _, err := tx.RecordBlobAttempt("blob-b"); return err },
		"release a processed blob":    func(tx *Tx) error { return tx.ReleaseBlobAttempt("blob-b") },
	} {
		if err := s.Write(t.Context(), "test.blob", call); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
	for name, call := range map[string]func(*Tx) error{
		"an id with a path":        func(tx *Tx) error { _, err := tx.RecordBlob("../keys", []byte("x"), epoch); return err },
		"an empty id":              func(tx *Tx) error { _, err := tx.RecordBlob("", []byte("x"), epoch); return err },
		"an empty reference":       func(tx *Tx) error { _, err := tx.RecordBlob("blob-d", nil, epoch); return err },
		"no time":                  func(tx *Tx) error { _, err := tx.RecordBlob("blob-d", []byte("x"), time.Time{}); return err },
		"no processing time":       func(tx *Tx) error { return tx.MarkBlobProcessed("blob-a", time.Time{}) },
		"an overlong id":           func(tx *Tx) error { _, err := tx.RecordBlob(strings.Repeat("a", 129), []byte("x"), epoch); return err },
		"a non-positive list":      func(tx *Tx) error { _, err := tx.PendingBlobs(0); return err },
		"a pending id with a path": func(tx *Tx) error { _, err := tx.BlobPending("../keys"); return err },
	} {
		if err := s.Write(t.Context(), "test.blob", call); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestSyncStateAndTheRestartCounter(t *testing.T) {
	s := openStore(t)
	write(t, s, func(tx *Tx) error { return tx.SetSyncValue("history_settled_at", "1791115200000") })
	var v string
	var ok bool
	read(t, s, func(r *Reader) error {
		var err error
		v, ok, err = r.SyncValue("history_settled_at")
		return err
	})
	if !ok || v != "1791115200000" {
		t.Fatalf("SyncValue = %q, %v", v, ok)
	}
	for _, key := range []string{"process_starts", "last_ingest_at", "Upper", "", "with space", strings.Repeat("k", 65)} {
		if err := s.Write(t.Context(), "test.sync", func(tx *Tx) error { return tx.SetSyncValue(key, "1") }); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetSyncValue(%q) = %v, want ErrInvalid", key, err)
		}
	}
	for i, at := range []time.Duration{0, time.Minute, 5 * time.Minute, 9 * time.Minute, 11 * time.Minute} {
		n, err := s.RecordStart(t.Context(), epoch.Add(at))
		if err != nil {
			t.Fatalf("RecordStart: %v", err)
		}
		if want := []int{1, 2, 3, 4, 3}[i]; n != want {
			t.Fatalf("start %d at +%v counts %d recent starts, want %d", i, at, n, want)
		}
	}
	if n, _ := s.RecordStart(t.Context(), epoch.Add(time.Hour)); n != 1 {
		t.Fatalf("a start an hour later counts %d, want 1", n)
	}
	for range 2 * maxStarts {
		if _, err := s.RecordStart(t.Context(), epoch.Add(2*time.Hour)); err != nil {
			t.Fatalf("RecordStart: %v", err)
		}
	}
	if n, _ := s.RecordStart(t.Context(), epoch.Add(2*time.Hour)); n != maxStarts {
		t.Fatalf("a burst counts %d starts, want the cap %d", n, maxStarts)
	}
	write(t, s, func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, upsertSync, startsKey, "1,two")
		return err
	})
	if _, err := s.RecordStart(t.Context(), epoch); !errors.Is(err, errCorruptHistory) {
		t.Fatalf("RecordStart over an unreadable history = %v", err)
	}
}

func TestStartsLaterThanTheClockAreForgotten(t *testing.T) {
	s := openStore(t)
	for range 5 {
		if _, err := s.RecordStart(t.Context(), epoch); err != nil {
			t.Fatalf("RecordStart: %v", err)
		}
	}
	for i, back := range []time.Duration{24 * time.Hour, 23 * time.Hour, time.Hour, time.Minute} {
		if n, err := s.RecordStart(t.Context(), epoch.Add(-back)); err != nil || n != 1 {
			t.Fatalf("start %d, %v before the five, counts %d recent starts, %v; want 1", i+1, back, n, err)
		}
	}
	if n, err := s.RecordStart(t.Context(), epoch); err != nil || n != 2 {
		t.Fatalf("a start back at the time of the five counts %d recent starts, %v; want 2", n, err)
	}
}

func TestFreeSpaceFloorHasHysteresis(t *testing.T) {
	const floor = 1000
	for _, tt := range []struct {
		was  bool
		free uint64
		want bool
	}{
		{false, 999, true}, {false, 1000, false}, {true, 1000, true}, {true, 1249, true}, {true, 1250, false}, {false, 1100, false},
	} {
		if got := pausedAfter(tt.was, tt.free, floor); got != tt.want {
			t.Errorf("pausedAfter(%v, %d, %d) = %v, want %v", tt.was, tt.free, floor, got, tt.want)
		}
	}
	if pausedAfter(true, 0, 0) {
		t.Error("a floor of 0 pauses")
	}
	if resumeAt(math.MaxUint64) != math.MaxUint64 || resumeAt(math.MaxUint64-1) != math.MaxUint64 {
		t.Error("resumeAt overflows")
	}
	if floorFor(ProfileNFS, floor) != 0 || floorFor(ProfileLocal, floor) != floor {
		t.Error("the floor does not apply to profile local only")
	}

	s := openStore(t)
	s.floor = math.MaxUint64
	space, err := s.CheckSpace()
	if err != nil || !space.Paused || !space.Changed || space.Free == 0 || space.Floor != math.MaxUint64 {
		t.Fatalf("CheckSpace below the floor = %+v, %v", space, err)
	}
	if space, _ = s.CheckSpace(); !space.Paused || space.Changed {
		t.Fatalf("CheckSpace again = %+v, want paused and unchanged", space)
	}
	s.floor = 1
	if space, _ = s.CheckSpace(); space.Paused || !space.Changed {
		t.Fatalf("CheckSpace far above the floor = %+v, want resumed", space)
	}
}

func TestStoreReadIsReadOnlyInPractice(t *testing.T) {
	s := openStore(t)
	err := s.Read(t.Context(), "test.read", func(r *Reader) error {
		_, err := r.q.ExecContext(r.ctx, "INSERT INTO sync_state (key, value) VALUES ('k', 'v')")
		return err
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM sync_state WHERE key = 'k'"); n != 0 {
		t.Fatal("a write inside Read was committed")
	}
}
