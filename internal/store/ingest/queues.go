package ingest

import (
	"database/sql"
	"errors"
	"regexp"
	"time"
)

const MaxInboxBacklog = 5000

var (
	ErrInboxFull = errors.New("ingest: the inbox backlog is full")
	blobID       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

type InboxItem struct {
	Seq        int64
	ReceivedAt time.Time
	Payload    []byte
	Attempts   int
}

type Blob struct {
	ID         string
	ReceivedAt time.Time
	Ref        []byte
	Attempts   int
}

const (
	countBacklog      = "SELECT count(*) FROM inbox WHERE quarantined = 0"
	insertInbox       = "INSERT INTO inbox (received_at, payload) VALUES (?, ?) RETURNING seq"
	selectNextInbox   = "SELECT seq, received_at, payload, attempts FROM inbox WHERE quarantined = 0 ORDER BY seq LIMIT 1"
	bumpInboxAttempt  = "UPDATE inbox SET attempts = attempts + 1 WHERE seq = ? AND quarantined = 0 RETURNING attempts"
	deleteInbox       = "DELETE FROM inbox WHERE seq = ?"
	quarantineInbox   = "UPDATE inbox SET quarantined = 1, payload = NULL WHERE seq = ?"
	insertBlob        = "INSERT INTO history_blobs (id, received_at, ref) VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING"
	selectPending     = "SELECT id, received_at, ref, attempts FROM history_blobs WHERE processed_at IS NULL AND quarantined = 0 ORDER BY received_at, id LIMIT ?"
	bumpBlobAttempt   = "UPDATE history_blobs SET attempts = attempts + 1 WHERE id = ? AND processed_at IS NULL AND quarantined = 0 RETURNING attempts"
	markBlobProcessed = "UPDATE history_blobs SET processed_at = ?, ref = NULL WHERE id = ? AND processed_at IS NULL AND quarantined = 0"
	quarantineBlob    = "UPDATE history_blobs SET quarantined = 1, ref = NULL WHERE id = ? AND processed_at IS NULL"
)

func (tx *Tx) AppendInbox(payload []byte, at time.Time) (int64, error) {
	if len(payload) == 0 || at.IsZero() {
		return 0, invalid("inbox item")
	}
	var backlog int
	if err := tx.q.QueryRowContext(tx.ctx, countBacklog).Scan(&backlog); err != nil {
		return 0, err
	}
	if backlog >= MaxInboxBacklog {
		return 0, ErrInboxFull
	}
	var seq int64
	err := tx.q.QueryRowContext(tx.ctx, insertInbox, ms(at), payload).Scan(&seq)
	return seq, err
}

func (r *Reader) NextInbox() (InboxItem, bool, error) {
	var item InboxItem
	var received int64
	switch err := r.q.QueryRowContext(r.ctx, selectNextInbox).Scan(&item.Seq, &received, &item.Payload, &item.Attempts); {
	case errors.Is(err, sql.ErrNoRows):
		return InboxItem{}, false, nil
	case err != nil:
		return InboxItem{}, false, err
	}
	item.ReceivedAt = fromMS(received)
	return item, true, nil
}

func (tx *Tx) RecordInboxAttempt(seq int64) (int, error) {
	var attempts int
	err := tx.q.QueryRowContext(tx.ctx, bumpInboxAttempt, seq).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return attempts, err
}

func (tx *Tx) DeleteInbox(seq int64) error {
	return oneRow(tx.q.ExecContext(tx.ctx, deleteInbox, seq))
}

func (tx *Tx) QuarantineInbox(seq int64) error {
	return oneRow(tx.q.ExecContext(tx.ctx, quarantineInbox, seq))
}

func (tx *Tx) RecordBlob(id string, ref []byte, at time.Time) (bool, error) {
	if !blobID.MatchString(id) || len(ref) == 0 || at.IsZero() {
		return false, invalid("history blob")
	}
	res, err := tx.q.ExecContext(tx.ctx, insertBlob, id, ms(at), ref)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *Reader) PendingBlobs(limit int) ([]Blob, error) {
	if limit <= 0 {
		return nil, invalid("limit")
	}
	rows, err := r.q.QueryContext(r.ctx, selectPending, limit)
	if err != nil {
		return nil, err
	}
	var out []Blob
	for rows.Next() {
		var b Blob
		var received int64
		if err := rows.Scan(&b.ID, &received, &b.Ref, &b.Attempts); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		b.ReceivedAt = fromMS(received)
		out = append(out, b)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (tx *Tx) RecordBlobAttempt(id string) (int, error) {
	var attempts int
	err := tx.q.QueryRowContext(tx.ctx, bumpBlobAttempt, id).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return attempts, err
}

func (tx *Tx) MarkBlobProcessed(id string, at time.Time) error {
	if at.IsZero() {
		return invalid("time")
	}
	return oneRow(tx.q.ExecContext(tx.ctx, markBlobProcessed, ms(at), id))
}

func (tx *Tx) QuarantineBlob(id string) error {
	return oneRow(tx.q.ExecContext(tx.ctx, quarantineBlob, id))
}

func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	switch {
	case err != nil:
		return err
	case n != 1:
		return ErrNotFound
	}
	return nil
}
