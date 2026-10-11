package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

const (
	anchorDomain = "wawarden/audit-anchor/v1\x00"
	// The longest a row's time can precede its commit: the archive's write timeout.
	commitLag = 10 * time.Second

	ReasonAnchorInvalid = "anchor_invalid"
)

const (
	selectAuditBounds  = `SELECT min(id), max(id) FROM audit`
	selectPruneThrough = `SELECT max(id) FROM audit WHERE id BETWEEN ?1 AND ?2 AND ts < ?3`
	selectAnchorTarget = `SELECT min(?2, (SELECT max(ts) FROM (SELECT ts FROM audit WHERE id <= ?1 UNION ALL SELECT ts FROM audit_anchor))), key_id, row_hmac FROM audit WHERE id = ?1`
	upsertAnchor       = `INSERT INTO audit_anchor (id, through, ts, key_id, row_hmac, pruned_at, mac_key_id, mac) VALUES (1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET through = excluded.through, ts = excluded.ts, key_id = excluded.key_id, row_hmac = excluded.row_hmac,
pruned_at = excluded.pruned_at, mac_key_id = excluded.mac_key_id, mac = excluded.mac`
	deleteAuditThrough = `DELETE FROM audit WHERE id <= ?`
	selectAnchorTable  = `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'audit_anchor'`
	selectAnchor       = `SELECT through, ts, key_id, row_hmac, pruned_at, mac_key_id, mac FROM audit_anchor WHERE id = 1`
)

type anchor struct {
	through  int64
	ts       int64
	keyID    string
	rowHMAC  []byte
	prunedAt int64
	macKeyID string
	mac      []byte
}

type PruneResult struct {
	Rows       int64
	Through    int64
	AnchorHead string
}

func (a *Audit) Prune(ctx context.Context, cutoff, now time.Time, batch int) (PruneResult, error) {
	if a == nil || a.db == nil || a.chain == nil || batch < 1 {
		return PruneResult{}, errAuditUnkeyed
	}
	var res PruneResult
	err := a.db.Write(ctx, "admin.audit_prune", func(ctx context.Context, q db.Querier) error {
		var lo, tail sql.NullInt64
		if err := q.QueryRowContext(ctx, selectAuditBounds).Scan(&lo, &tail); err != nil {
			return err
		}
		if !lo.Valid || tail.Int64-1 < lo.Int64 {
			return nil
		}
		var through sql.NullInt64
		hi := min(lo.Int64+int64(batch)-1, tail.Int64-1)
		if err := q.QueryRowContext(ctx, selectPruneThrough, lo.Int64, hi, cutoff.UnixMilli()).Scan(&through); err != nil {
			return err
		}
		if !through.Valid {
			return nil
		}
		an := anchor{through: through.Int64, prunedAt: now.UnixMilli(), macKeyID: a.keyID}
		if err := q.QueryRowContext(ctx, selectAnchorTarget, an.through, cutoff.Add(commitLag).UnixMilli()).Scan(&an.ts, &an.keyID, &an.rowHMAC); err != nil {
			return err
		}
		an.mac = anchorMAC([]byte(*a.chain), an)
		if _, err := q.ExecContext(ctx, upsertAnchor, an.through, an.ts, an.keyID, an.rowHMAC, an.prunedAt, an.macKeyID, an.mac); err != nil {
			return err
		}
		deleted, err := q.ExecContext(ctx, deleteAuditThrough, an.through)
		if err != nil {
			return err
		}
		n, err := deleted.RowsAffected()
		if err != nil {
			return err
		}
		res = PruneResult{Rows: n, Through: an.through, AnchorHead: hex.EncodeToString(an.rowHMAC[:headBytes])}
		return nil
	})
	if err != nil {
		return PruneResult{}, err
	}
	return res, nil
}

func readAnchor(ctx context.Context, q db.Querier) (anchor, bool, error) {
	var tables int64
	if err := q.QueryRowContext(ctx, selectAnchorTable).Scan(&tables); err != nil || tables == 0 {
		return anchor{}, false, err
	}
	var an anchor
	switch err := q.QueryRowContext(ctx, selectAnchor).Scan(&an.through, &an.ts, &an.keyID, &an.rowHMAC, &an.prunedAt, &an.macKeyID, &an.mac); {
	case errors.Is(err, sql.ErrNoRows):
		return anchor{}, false, nil
	case err != nil:
		return anchor{}, false, err
	}
	return an, true, nil
}

func anchorMAC(key []byte, an anchor) []byte {
	b := []byte(anchorDomain)
	b, _ = binary.Append(b, binary.BigEndian, [3]int64{an.through, an.ts, an.prunedAt})
	b = field(b, true, an.keyID)
	b = append(b, an.rowHMAC...)
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return m.Sum(nil)
}
