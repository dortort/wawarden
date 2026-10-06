package scoped

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"regexp"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	messageColumns = `m.seq, m.ts, m.change_seq, m.chat_jid, c.ref, m.id, m.sender_jid, k.push_name, coalesce(n.full_name, n.first_name),
		m.from_me, m.kind, CASE WHEN m.revoked = 0 THEN m.text END, CASE WHEN m.revoked = 0 THEN m.text_display END, m.media_type,
		q.id, q.sender_jid, m.quote_verified, m.edited_ts, m.revoked`
	messageJoins = ` JOIN chats c ON c.jid = m.chat_jid
		LEFT JOIN contacts k ON k.jid = m.sender_jid
		LEFT JOIN contact_names n ON n.jid = m.sender_jid{scope n.jid}
		LEFT JOIN messages q ON q.seq = m.quoted_ref AND q.chat_jid = m.chat_jid`
	selectOlder = "SELECT " + messageColumns + " FROM messages m" + messageJoins +
		" WHERE m.chat_jid = ?1{scope m.chat_jid} AND (m.ts, m.seq) < (?2, ?3) ORDER BY m.ts DESC, m.seq DESC LIMIT ?4"
	selectNewer = "SELECT " + messageColumns + " FROM messages m" + messageJoins +
		" WHERE m.chat_jid = ?1{scope m.chat_jid} AND (m.ts, m.seq) > (?2, ?3) ORDER BY m.ts, m.seq LIMIT ?4"
	selectMessage = "SELECT " + messageColumns + " FROM messages m" + messageJoins +
		" WHERE m.chat_jid = ?1 AND m.id = ?2 AND m.sender_jid = ?3{scope m.chat_jid}"
	selectChanges = "SELECT " + messageColumns + " FROM messages m" + messageJoins +
		" WHERE m.change_seq > ?1 AND m.change_seq <= ?2{scope m.chat_jid} ORDER BY m.change_seq LIMIT ?3"
	selectChatChanges = "SELECT " + messageColumns + " FROM messages m" + messageJoins +
		" WHERE m.chat_jid = ?4 AND m.change_seq > ?1 AND m.change_seq <= ?2{scope m.chat_jid} ORDER BY m.change_seq LIMIT ?3"
	selectFirstChange     = "SELECT min(m.change_seq) FROM messages m WHERE m.change_seq > ?1 AND m.change_seq <= ?2 AND m.ts >= ?3{scope m.chat_jid}"
	selectChatFirstChange = "SELECT min(m.change_seq) FROM messages m WHERE m.chat_jid = ?4 AND m.change_seq > ?1 AND m.change_seq <= ?2 AND m.ts >= ?3{scope m.chat_jid}"
	selectTopChange       = "SELECT coalesce(max(change_seq), 0) FROM messages"
	selectLID             = "SELECT lid FROM lid_map WHERE pn = ?1"
)

var messageID = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

func (r *Reader) Messages(g policy.ReadGrant, ctx context.Context, ref string, pos MessagePosition, dir Direction, limit int) (MessagePage, bool, error) {
	if !validLimit(limit) || dir != Older && dir != Newer {
		return MessagePage{}, false, ErrInvalid
	}
	ts, seq := int64(math.MaxInt64), int64(math.MaxInt64)
	if dir == Newer {
		ts, seq = math.MinInt64, math.MinInt64
	}
	if pos.Seq != 0 {
		ts, seq = pos.TS, pos.Seq
	}
	var page MessagePage
	var found bool
	err := r.read(g, ctx, "scoped.messages", func(ctx context.Context, q querier) error {
		var err error
		if page.Chat, found, err = chatByRef(ctx, q, ref); err != nil || !found {
			return err
		}
		messages := rowsOf[Message]{q, scanMessage}
		var got []Message
		if dir == Newer {
			got, err = messages.QueryContext(ctx, selectNewer, page.Chat.Chat.JID(), ts, seq, limit+1)
		} else {
			got, err = messages.QueryContext(ctx, selectOlder, page.Chat.Chat.JID(), ts, seq, limit+1)
		}
		if err != nil {
			return err
		}
		page.Messages = got
		if len(got) > limit {
			page.Messages, page.Next, page.More = got[:limit], got[limit-1].Position, true
		}
		return nil
	})
	return page, found, err
}

func (r *Reader) Message(g policy.ReadGrant, ctx context.Context, chat policy.CanonicalChat, id string, sender policy.CanonicalChat) (Message, bool, error) {
	if !chat.Valid() || !messageID.MatchString(id) || sender.Kind() != policy.PhoneChat && sender.Kind() != policy.LIDChat {
		return Message{}, false, nil
	}
	var m Message
	var found bool
	err := r.read(g, ctx, "scoped.message", func(ctx context.Context, q querier) error {
		chat, err := canonical(ctx, q, chat)
		if err != nil {
			return err
		}
		if sender, err = canonical(ctx, q, sender); err != nil || !g.Allows(chat) {
			return err
		}
		got, err := rowsOf[Message]{q, scanMessage}.QueryContext(ctx, selectMessage, chat.JID(), id, sender.JID())
		if err != nil || len(got) == 0 {
			return err
		}
		m, found = got[0], true
		return nil
	})
	return m, found, err
}

func canonical(ctx context.Context, q querier, c policy.CanonicalChat) (policy.CanonicalChat, error) {
	if c.Kind() != policy.PhoneChat {
		return c, nil
	}
	var lid string
	switch err := q.QueryRowContext(ctx, selectLID, c.JID()).Scan(&lid); {
	case errors.Is(err, sql.ErrNoRows):
		return c, nil
	case err != nil:
		return policy.CanonicalChat{}, err
	}
	return stored(lid, policy.LIDChat)
}

func (r *Reader) Changes(g policy.ReadGrant, ctx context.Context, ref string, pos ChangePosition, limit int) (ChangePage, bool, error) {
	if !validLimit(limit) || pos.ChangeSeq < 0 || pos.Since < 0 {
		return ChangePage{}, false, ErrInvalid
	}
	var page ChangePage
	found := true
	err := r.read(g, ctx, "scoped.changes", func(ctx context.Context, q querier) error {
		var chat string
		if ref != "" {
			c, ok, err := chatByRef(ctx, q, ref)
			if found = ok; err != nil || !ok {
				return err
			}
			chat = c.Chat.JID()
		}
		var top int64
		if err := q.QueryRowContext(ctx, selectTopChange).Scan(&top); err != nil {
			return err
		}
		messages := rowsOf[Message]{q, scanMessage}
		firsts := rowsOf[sql.NullInt64]{q, scanFirst}
		after, since := pos.ChangeSeq, pos.Since
		for i := 0; i < maxWindows && after < top && len(page.Messages) <= limit; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			upto := min(after+window, top)
			var got []Message
			var err error
			if since != 0 {
				var first []sql.NullInt64
				if chat == "" {
					first, err = firsts.QueryContext(ctx, selectFirstChange, after, upto, since)
				} else {
					first, err = firsts.QueryContext(ctx, selectChatFirstChange, after, upto, since, chat)
				}
				if err != nil {
					return err
				}
				if len(first) == 0 || !first[0].Valid {
					after = upto
					continue
				}
				after, since = first[0].Int64-1, 0
			}
			if chat == "" {
				got, err = messages.QueryContext(ctx, selectChanges, after, upto, limit+1-len(page.Messages))
			} else {
				got, err = messages.QueryContext(ctx, selectChatChanges, after, upto, limit+1-len(page.Messages), chat)
			}
			if err != nil {
				return err
			}
			page.Messages = append(page.Messages, got...)
			after = upto
		}
		page.Next, page.More = ChangePosition{ChangeSeq: after, Since: since}, after < top
		if len(page.Messages) > limit {
			page.Messages, page.Next, page.More = page.Messages[:limit], page.Messages[limit-1].Change, true
		}
		return nil
	})
	return page, found, err
}

const (
	searchFrom   = " FROM messages_fts f JOIN messages m ON m.seq = f.rowid" + messageJoins
	selectSearch = "SELECT " + messageColumns + searchFrom + " WHERE f.messages_fts MATCH ?1 AND f.rowid > ?2 AND f.rowid <= ?3{scope m.chat_jid}" +
		" AND m.revoked = 0 AND m.text IS NOT NULL ORDER BY f.rowid DESC LIMIT ?4"
	selectChatSearch = "SELECT " + messageColumns + searchFrom + " WHERE f.messages_fts MATCH ?1 AND f.rowid > ?2 AND f.rowid <= ?3 AND m.chat_jid = ?5{scope m.chat_jid}" +
		" AND m.revoked = 0 AND m.text IS NOT NULL ORDER BY f.rowid DESC LIMIT ?4"
	selectTopSeq = "SELECT coalesce(max(seq), 0) FROM messages"
)

func (r *Reader) Search(g policy.ReadGrant, ctx context.Context, query Query, ref string, pos SearchPosition, limit int) (SearchPage, bool, error) {
	if query.match == "" || !validLimit(limit) || pos.Upper < 0 {
		return SearchPage{}, false, ErrInvalid
	}
	var page SearchPage
	found := true
	err := r.read(g, ctx, "scoped.search", func(ctx context.Context, q querier) error {
		var chat string
		if ref != "" {
			c, ok, err := chatByRef(ctx, q, ref)
			if found = ok; err != nil || !ok {
				return err
			}
			chat = c.Chat.JID()
		}
		upper := pos.Upper
		if upper == 0 {
			if err := q.QueryRowContext(ctx, selectTopSeq).Scan(&upper); err != nil {
				return err
			}
			upper++
		}
		messages := rowsOf[Message]{q, scanMessage}
		for i := 0; i < maxWindows && upper > 1 && len(page.Messages) <= limit; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			lower := max(upper-1-window, 0)
			var got []Message
			var err error
			if chat == "" {
				got, err = messages.QueryContext(ctx, selectSearch, query.match, lower, upper-1, limit+1-len(page.Messages))
			} else {
				got, err = messages.QueryContext(ctx, selectChatSearch, query.match, lower, upper-1, limit+1-len(page.Messages), chat)
			}
			if err != nil {
				return err
			}
			page.Messages = append(page.Messages, got...)
			upper = lower + 1
		}
		page.Next, page.More = SearchPosition{Upper: upper}, upper > 1
		if len(page.Messages) > limit {
			page.Messages, page.Next, page.More = page.Messages[:limit], SearchPosition{Upper: page.Messages[limit-1].Position.Seq}, true
		}
		return nil
	})
	return page, found, err
}
