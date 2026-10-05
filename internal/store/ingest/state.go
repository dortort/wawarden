package ingest

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/store/admin"
)

const (
	RestartWindow = 10 * time.Minute
	maxStarts     = 64
	startsKey     = "process_starts"
)

const (
	selectSync = "SELECT value FROM sync_state WHERE key = ?"
	upsertSync = "INSERT INTO sync_state (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value"
)

var (
	errCorruptHistory = errors.New("ingest: the stored start history is unreadable")
	syncKey           = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	reservedKeys      = map[string]bool{startsKey: true, admin.LastIngestKey: true, rewriteDueKey: true}
)

func (r *Reader) SyncValue(key string) (string, bool, error) {
	if !syncKey.MatchString(key) {
		return "", false, invalid("sync key")
	}
	var v string
	switch err := r.q.QueryRowContext(r.ctx, selectSync, key).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return v, true, nil
}

func (tx *Tx) SetSyncValue(key, value string) error {
	if !syncKey.MatchString(key) || reservedKeys[key] {
		return invalid("sync key")
	}
	_, err := tx.q.ExecContext(tx.ctx, upsertSync, key, value)
	return err
}

func (s *Store) RecordStart(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, invalid("time")
	}
	var recent int
	err := s.Write(ctx, "ingest.record_start", func(tx *Tx) error {
		v, _, err := tx.SyncValue(startsKey)
		if err != nil {
			return err
		}
		starts, err := parseStarts(v)
		if err != nil {
			return err
		}
		cutoff := ms(now.Add(-RestartWindow))
		kept := []string{}
		for _, t := range starts {
			if t > cutoff {
				kept = append(kept, strconv.FormatInt(t, 10))
			}
		}
		kept = append(kept, formatMS(now))
		kept = kept[max(0, len(kept)-maxStarts):]
		recent = len(kept)
		_, err = tx.q.ExecContext(tx.ctx, upsertSync, startsKey, strings.Join(kept, ","))
		return err
	})
	return recent, err
}

func parseStarts(v string) ([]int64, error) {
	if v == "" {
		return nil, nil
	}
	var out []int64
	for field := range strings.SplitSeq(v, ",") {
		t, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, errCorruptHistory
		}
		out = append(out, t)
	}
	return out, nil
}

type Space struct {
	Free    uint64
	Floor   uint64
	Paused  bool
	Changed bool
}

func (s *Store) CheckSpace() (Space, error) {
	free, err := s.db.FreeBytes()
	if err != nil {
		return Space{}, err
	}
	floor := floorFor(s.db.Profile(), s.floor)
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.paused
	s.paused = pausedAfter(was, free, floor)
	return Space{Free: free, Floor: floor, Paused: s.paused, Changed: s.paused != was}, nil
}

func floorFor(profile Profile, floor uint64) uint64 {
	if profile != ProfileLocal {
		return 0
	}
	return floor
}

func pausedAfter(paused bool, free, floor uint64) bool {
	switch {
	case floor == 0:
		return false
	case free < floor:
		return true
	case free >= resumeAt(floor):
		return false
	}
	return paused
}

func resumeAt(floor uint64) uint64 {
	if floor > math.MaxUint64-floor/4 {
		return math.MaxUint64
	}
	return floor + floor/4
}
