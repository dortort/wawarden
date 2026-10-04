// Package admin reads the archive's counters for the admin status route: counts and a time, never an identifier.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

const LastIngestKey = "last_ingest_at"

type Counters struct {
	Chats            int64
	Messages         int64
	BlobsPending     int64
	BlobsQuarantined int64
	InboxBacklog     int64
	InboxQuarantined int64
	LastIngest       time.Time
}

type Reader struct{ db *db.DB }

func New(d *db.DB) *Reader { return &Reader{db: d} }

const selectCounters = `SELECT
	(SELECT count(*) FROM chats),
	(SELECT count(*) FROM messages),
	(SELECT count(*) FROM history_blobs WHERE processed_at IS NULL AND quarantined = 0),
	(SELECT count(*) FROM history_blobs WHERE quarantined = 1),
	(SELECT count(*) FROM inbox WHERE quarantined = 0),
	(SELECT count(*) FROM inbox WHERE quarantined = 1),
	(SELECT value FROM sync_state WHERE key = ?)`

func (r *Reader) Counters(ctx context.Context) (Counters, error) {
	var c Counters
	var last sql.NullString
	err := r.db.Read(ctx, "admin.counters", func(ctx context.Context, q db.Querier) error {
		return q.QueryRowContext(ctx, selectCounters, LastIngestKey).Scan(
			&c.Chats, &c.Messages, &c.BlobsPending, &c.BlobsQuarantined, &c.InboxBacklog, &c.InboxQuarantined, &last)
	})
	if err != nil {
		return Counters{}, fmt.Errorf("admin: read the archive's counters: %w", err)
	}
	if last.Valid {
		ms, err := strconv.ParseInt(last.String, 10, 64)
		if err != nil {
			return Counters{}, errors.New("admin: the last ingest time is not a number of milliseconds")
		}
		c.LastIngest = time.UnixMilli(ms).UTC()
	}
	return c, nil
}
