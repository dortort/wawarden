package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

type Chat struct {
	Chat policy.CanonicalChat
	Ref  string
	Name string
}

const selectChats = `SELECT jid, coalesce(ref, ''), coalesce(name, '') FROM chats
	WHERE ?1 = '' OR instr(lower(coalesce(name, '')), lower(?1)) > 0 OR instr(jid, ?1) > 0 OR ref = lower(?1)
	ORDER BY last_ts IS NULL, last_ts DESC, jid LIMIT ?2`

func (r *Reader) Chats(ctx context.Context, match string, limit int) ([]Chat, bool, error) {
	out := []Chat{}
	err := r.db.Read(ctx, "admin.chats", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, selectChats, match, limit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var jid string
			var c Chat
			if err := rows.Scan(&jid, &c.Ref, &c.Name); err != nil {
				return errors.Join(err, rows.Close())
			}
			var ok bool
			if c.Chat, ok = storedChat(jid); !ok {
				return errors.Join(errCorruptRow, rows.Close())
			}
			out = append(out, c)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, false, fmt.Errorf("admin: list the chats: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
