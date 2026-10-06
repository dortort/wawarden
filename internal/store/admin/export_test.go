package admin

import (
	"context"
	"database/sql"
	"errors"

	"github.com/dortort/wawarden/internal/store/internal/db"
	"github.com/dortort/wawarden/internal/token"
)

var ErrRolledBack = errors.New("admin: a synthetic change was rolled back")

func (c *Clients) MarkStale() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
}

func (c *Clients) RollBack(ctx context.Context, stmt string) error {
	var settle func(bool)
	err := c.db.Write(ctx, "test.roll_back", func(ctx context.Context, q db.Querier) error {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
		settle = c.Stage(ctx, q)
		return ErrRolledBack
	})
	if settle != nil {
		settle(err == nil)
	}
	return err
}

type Operations struct{ Hashes, Compares, AgainstDummy int }

func (c *Clients) CountOperations() *Operations {
	ops := &Operations{}
	digest, match := c.digest, c.match
	c.digest = func(s string) token.Digest {
		ops.Hashes++
		return digest(s)
	}
	c.match = func(stored, presented token.Digest) bool {
		ops.Compares++
		if token.Match(stored, c.dummy) {
			ops.AgainstDummy++
		}
		return match(stored, presented)
	}
	return ops
}

type AuditRow struct {
	ID, TS                      int64
	Client, Action, Reason, Key string
	Chat, ChatHMAC, Peer        sql.NullString
	OK                          bool
}

func RowMAC(key []byte, r AuditRow, prev []byte) []byte {
	return rowMAC(key, row{id: r.ID, ts: r.TS, client: r.Client, action: r.Action, chat: r.Chat, chatHMAC: r.ChatHMAC, ok: r.OK, reason: r.Reason, peer: r.Peer, keyID: r.Key}, prev)
}
