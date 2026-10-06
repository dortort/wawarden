package scoped

import (
	"context"
	"math"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	chatColumns   = "c.rowid, c.jid, c.kind, c.ref, c.name, c.name_source, c.last_ts"
	selectChat    = "SELECT " + chatColumns + " FROM chats c WHERE c.ref = ?1{scope c.jid}"
	selectDated   = "SELECT " + chatColumns + " FROM chats c WHERE c.last_ts IS NOT NULL AND (c.last_ts, c.rowid) < (?1, ?2){scope c.jid} ORDER BY c.last_ts DESC, c.rowid DESC LIMIT ?3"
	selectUndated = "SELECT " + chatColumns + " FROM chats c WHERE c.last_ts IS NULL AND c.rowid < ?1{scope c.jid} ORDER BY c.rowid DESC LIMIT ?2"
)

func (r *Reader) Chats(g policy.ReadGrant, ctx context.Context, pos ChatPosition, limit int) (ChatPage, error) {
	if !validLimit(limit) {
		return ChatPage{}, ErrInvalid
	}
	var page ChatPage
	err := r.read(g, ctx, "scoped.chats", func(ctx context.Context, q querier) error {
		chats := rowsOf[Chat]{q, scanChat}
		var got []Chat
		var err error
		if !pos.Undated {
			ts, row := pos.LastTS, pos.Row
			if row == 0 {
				ts, row = math.MaxInt64, math.MaxInt64
			}
			if got, err = chats.QueryContext(ctx, selectDated, ts, row, limit+1); err != nil {
				return err
			}
			pos = ChatPosition{Undated: true, Row: math.MaxInt64}
		}
		if len(got) <= limit {
			undated, err := chats.QueryContext(ctx, selectUndated, pos.Row, limit+1-len(got))
			if err != nil {
				return err
			}
			got = append(got, undated...)
		}
		page.Chats = got
		if len(got) > limit {
			page.Chats, page.Next, page.More = got[:limit], got[limit-1].Position, true
		}
		return nil
	})
	return page, err
}

func (r *Reader) Chat(g policy.ReadGrant, ctx context.Context, ref string) (Chat, bool, error) {
	var c Chat
	var found bool
	err := r.read(g, ctx, "scoped.chat", func(ctx context.Context, q querier) error {
		var err error
		c, found, err = chatByRef(ctx, q, ref)
		return err
	})
	return c, found, err
}

func chatByRef(ctx context.Context, q querier, ref string) (Chat, bool, error) {
	got, err := rowsOf[Chat]{q, scanChat}.QueryContext(ctx, selectChat, ref)
	if err != nil || len(got) == 0 {
		return Chat{}, false, err
	}
	return got[0], true, nil
}
