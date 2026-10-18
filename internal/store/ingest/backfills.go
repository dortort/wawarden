package ingest

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	BackfillInterval = 5 * time.Minute
	BackfillPerDay   = 20
	MaxBackfillCount = 50
	backfillDay      = 24 * time.Hour
)

const (
	selectAnchor       = "SELECT id, from_me, ts FROM messages WHERE chat_jid = ?1 ORDER BY ts, seq LIMIT 1"
	selectNewestAnchor = "SELECT id, from_me, ts FROM messages WHERE chat_jid = ?1 ORDER BY ts DESC, seq DESC LIMIT 1"
	selectQuota        = `SELECT
		(SELECT max(requested_at) FROM backfills WHERE requested_at > ?1),
		(SELECT count(*) FROM backfills WHERE requested_at >= ?2)`
	insertBackfill   = "INSERT INTO backfills (request_id, chat_jid, count, requested_at) VALUES (?1, ?2, ?3, ?4)"
	bindBackfill     = "UPDATE backfills SET request_id = ?2 WHERE request_id = ?1 AND completed_at IS NULL"
	cancelBackfill   = "DELETE FROM backfills WHERE request_id = ?1 AND completed_at IS NULL"
	completeBackfill = "UPDATE backfills SET completed_at = ?2 WHERE request_id = ?1 AND completed_at IS NULL"
	countBackfills   = "SELECT count(*) FROM backfills WHERE requested_at >= ?1"
)

var ErrNoAnchor = errors.New("ingest: the chat holds no message to request history before")

type QuotaError struct {
	RetryAfter time.Duration
	Daily      bool
}

func (e *QuotaError) Error() string {
	if e.Daily {
		return "ingest: the daily backfill quota is spent"
	}
	return "ingest: a backfill was requested less than five minutes ago"
}

type AnchorKind uint8

const (
	AnchorOldest AnchorKind = iota
	AnchorNewest
)

type BackfillRequest struct {
	ID     string
	Chat   policy.CanonicalChat
	Count  int
	Anchor AnchorKind
	Now    time.Time
}

type BackfillAnchor struct {
	ID        string
	FromMe    bool
	Timestamp time.Time
}

type BackfillTicket struct {
	ID     string
	Chat   policy.CanonicalChat
	Count  int
	Anchor BackfillAnchor
}

func (s *Store) ReserveBackfill(g policy.AdminGrant, ctx context.Context, in BackfillRequest) (BackfillTicket, error) {
	switch {
	case !g.Valid():
		return BackfillTicket{}, ErrNotAllowed
	case !messageID.MatchString(in.ID):
		return BackfillTicket{}, invalid("request id")
	case !in.Chat.Valid():
		return BackfillTicket{}, invalid("chat")
	case in.Count < 1 || in.Count > MaxBackfillCount:
		return BackfillTicket{}, invalid("count")
	case in.Anchor != AnchorOldest && in.Anchor != AnchorNewest:
		return BackfillTicket{}, invalid("anchor")
	case in.Now.IsZero():
		return BackfillTicket{}, invalid("time")
	}
	var ticket BackfillTicket
	err := s.Write(ctx, "ingest.backfill_reserve", func(tx *Tx) error {
		var err error
		ticket, err = tx.reserveBackfill(in)
		return err
	})
	if err != nil {
		return BackfillTicket{}, err
	}
	return ticket, nil
}

func (tx *Tx) reserveBackfill(in BackfillRequest) (BackfillTicket, error) {
	chat, err := tx.Canonical(in.Chat)
	if err != nil {
		return BackfillTicket{}, err
	}
	row := func() *sql.Row {
		if in.Anchor == AnchorNewest {
			return tx.q.QueryRowContext(tx.ctx, selectNewestAnchor, chat.JID())
		}
		return tx.q.QueryRowContext(tx.ctx, selectAnchor, chat.JID())
	}
	var anchor BackfillAnchor
	var ts int64
	switch err := row().Scan(&anchor.ID, &anchor.FromMe, &ts); {
	case errors.Is(err, sql.ErrNoRows):
		return BackfillTicket{}, ErrNoAnchor
	case err != nil:
		return BackfillTicket{}, err
	}
	anchor.Timestamp = fromMS(ts)
	now := in.Now.UTC()
	midnight := now.Truncate(backfillDay)
	var last sql.NullInt64
	var today int64
	if err := tx.q.QueryRowContext(tx.ctx, selectQuota, ms(now.Add(-BackfillInterval)), ms(midnight)).Scan(&last, &today); err != nil {
		return BackfillTicket{}, err
	}
	var quota *QuotaError
	if today >= BackfillPerDay {
		quota = &QuotaError{RetryAfter: midnight.Add(backfillDay).Sub(now), Daily: true}
	}
	if last.Valid {
		if wait := fromMS(last.Int64).Add(BackfillInterval).Sub(now); quota == nil || wait > quota.RetryAfter {
			quota = &QuotaError{RetryAfter: wait, Daily: quota != nil}
		}
	}
	if quota != nil {
		quota.RetryAfter = max(wholeSeconds(quota.RetryAfter), time.Second)
		return BackfillTicket{}, quota
	}
	if _, err := tx.q.ExecContext(tx.ctx, insertBackfill, in.ID, chat.JID(), in.Count, ms(now)); err != nil {
		return BackfillTicket{}, err
	}
	return BackfillTicket{ID: in.ID, Chat: chat, Count: in.Count, Anchor: anchor}, nil
}

func (s *Store) BindBackfill(ctx context.Context, provisional, requestID string) error {
	if !messageID.MatchString(provisional) || !messageID.MatchString(requestID) {
		return invalid("request id")
	}
	return s.Write(ctx, "ingest.backfill_bind", func(tx *Tx) error {
		return oneRow(tx.q.ExecContext(tx.ctx, bindBackfill, provisional, requestID))
	})
}

func (s *Store) CancelBackfill(ctx context.Context, provisional string) error {
	if !messageID.MatchString(provisional) {
		return invalid("request id")
	}
	return s.Write(ctx, "ingest.backfill_cancel", func(tx *Tx) error {
		return oneRow(tx.q.ExecContext(tx.ctx, cancelBackfill, provisional))
	})
}

func (tx *Tx) CompleteBackfill(requestID string, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, invalid("time")
	}
	if !messageID.MatchString(requestID) {
		return false, nil
	}
	res, err := tx.q.ExecContext(tx.ctx, completeBackfill, requestID, ms(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *Reader) BackfillsSince(since time.Time) (int, error) {
	if since.IsZero() {
		return 0, invalid("time")
	}
	var n int
	err := r.q.QueryRowContext(r.ctx, countBackfills, ms(since)).Scan(&n)
	return n, err
}
