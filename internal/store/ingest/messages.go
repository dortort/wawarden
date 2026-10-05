package ingest

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sanitize"
	"github.com/dortort/wawarden/internal/store/admin"
)

type Origin string

const (
	OriginLive    Origin = "live"
	OriginHistory Origin = "history"
)

type Kind string

const (
	KindText       Kind = "text"
	KindMedia      Kind = "media"
	KindReaction   Kind = "reaction"
	KindPollUpdate Kind = "poll_update"
	KindOther      Kind = "other"
)

type Addressing string

const (
	AddressingUnknown Addressing = ""
	AddressingPN      Addressing = "pn"
	AddressingLID     Addressing = "lid"
)

type Ref struct {
	seq  int64
	chat policy.CanonicalChat
}

func (r Ref) Valid() bool { return r.seq > 0 && r.chat.Valid() }

type Quote struct {
	Ref      Ref
	Verified bool
}

type Message struct {
	Chat       policy.CanonicalChat
	ID         string
	Sender     policy.CanonicalChat
	SenderAlt  policy.CanonicalChat
	FromMe     bool
	Origin     Origin
	Addressing Addressing
	Timestamp  time.Time
	Kind       Kind
	Text       string
	MediaType  string
	Quote      *Quote
	ExpiresAt  time.Time
	Ingested   time.Time
}

type Found struct {
	Ref     Ref
	Sender  policy.CanonicalChat
	FromMe  bool
	Kind    Kind
	Text    string
	Revoked bool
}

const (
	insertMessage = `INSERT INTO messages (chat_jid, id, sender_jid, from_me, origin, sender_alt, addressing_mode, ts, kind,
		text, text_display, media_type, quoted_ref, quote_verified, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_jid, id, sender_jid) DO NOTHING RETURNING seq`
	selectSeq       = "SELECT seq FROM messages WHERE chat_jid = ? AND id = ? AND sender_jid = ?"
	selectFound     = "SELECT seq, from_me, kind, text, revoked FROM messages WHERE chat_jid = ? AND id = ? AND sender_jid = ?"
	selectChatOfSeq = "SELECT chat_jid FROM messages WHERE seq = ?"
	selectForChange = "SELECT chat_jid, kind, text, revoked FROM messages WHERE seq = ?"
	selectExpired   = "SELECT seq, text FROM messages WHERE expires_at <= ? AND text IS NOT NULL ORDER BY expires_at, seq LIMIT ?"
	insertChat      = "INSERT INTO chats (jid, kind, last_ts) VALUES (?1, ?2, ?3) ON CONFLICT (jid) DO UPDATE SET last_ts = max(coalesce(last_ts, ?3), ?3)"
	insertFTS       = "INSERT INTO messages_fts (rowid, text) VALUES (?, ?)"
	deleteFTS       = "INSERT INTO messages_fts (messages_fts, rowid, text) VALUES ('delete', ?, ?)"
	forgetText      = "UPDATE messages SET text = NULL, text_display = NULL WHERE seq = ?"
	setEditedText   = "UPDATE messages SET text = ?, text_display = ?, edited_ts = ? WHERE seq = ?"
	setRevoked      = "UPDATE messages SET revoked = 1 WHERE seq = ?"
	setLastIngest   = "INSERT INTO sync_state (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value"
)

func (m *Message) check() error {
	switch {
	case !m.Chat.Valid():
		return invalid("chat")
	case !messageID.MatchString(m.ID):
		return invalid("message id")
	case !m.Sender.Valid() || !user(m.Sender):
		return invalid("sender")
	case m.SenderAlt.Valid() && !user(m.SenderAlt):
		return invalid("alternate sender")
	case m.Origin != OriginLive && m.Origin != OriginHistory:
		return invalid("origin")
	case m.Addressing != AddressingUnknown && m.Addressing != AddressingPN && m.Addressing != AddressingLID:
		return invalid("addressing mode")
	case m.Timestamp.IsZero() || m.Ingested.IsZero():
		return invalid("time")
	case m.MediaType != "" && !mediaType.MatchString(m.MediaType):
		return invalid("media type")
	case m.Quote != nil && !m.Quote.Ref.Valid():
		return invalid("quote")
	}
	switch m.Kind {
	case KindText, KindMedia, KindOther:
	case KindReaction, KindPollUpdate:
		if m.Text != "" || m.MediaType != "" {
			return invalid("a reaction or poll update carries no content")
		}
	default:
		return invalid("kind")
	}
	return nil
}

func (tx *Tx) InsertMessage(m Message) (Ref, bool, error) {
	if err := m.check(); err != nil {
		return Ref{}, false, err
	}
	chat, err := tx.Canonical(m.Chat)
	if err != nil {
		return Ref{}, false, err
	}
	sender, err := tx.Canonical(m.Sender)
	if err != nil {
		return Ref{}, false, err
	}
	var quoted sql.NullInt64
	var verified sql.NullInt64
	if m.Quote != nil {
		if err := tx.sameChat(m.Quote.Ref, chat); err != nil {
			return Ref{}, false, err
		}
		quoted = sql.NullInt64{Int64: m.Quote.Ref.seq, Valid: true}
		verified = sql.NullInt64{Int64: boolInt(m.Quote.Verified), Valid: true}
	}
	if _, err := tx.q.ExecContext(tx.ctx, insertChat, chat.JID(), kindCode(chat.Kind()), ms(m.Timestamp)); err != nil {
		return Ref{}, false, err
	}
	text, display := nullString(m.Text), sql.NullString{}
	if text.Valid {
		display = sql.NullString{String: sanitize.Display(m.Text), Valid: true}
	}
	var alt sql.NullString
	if m.SenderAlt.Valid() {
		alt = sql.NullString{String: m.SenderAlt.JID(), Valid: true}
	}
	var expires sql.NullInt64
	if !m.ExpiresAt.IsZero() {
		expires = sql.NullInt64{Int64: ms(m.ExpiresAt), Valid: true}
	}
	var seq int64
	err = tx.q.QueryRowContext(tx.ctx, insertMessage, chat.JID(), m.ID, sender.JID(), boolInt(m.FromMe), string(m.Origin), alt,
		nullString(string(m.Addressing)), ms(m.Timestamp), string(m.Kind), text, display, nullString(m.MediaType), quoted, verified, expires).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.q.QueryRowContext(tx.ctx, selectSeq, chat.JID(), m.ID, sender.JID()).Scan(&seq); err != nil {
			return Ref{}, false, err
		}
		return Ref{seq: seq, chat: chat}, false, nil
	}
	if err != nil {
		return Ref{}, false, err
	}
	if _, err := tx.q.ExecContext(tx.ctx, insertFTS, seq, text); err != nil {
		return Ref{}, false, err
	}
	if _, err := tx.q.ExecContext(tx.ctx, setLastIngest, admin.LastIngestKey, formatMS(m.Ingested)); err != nil {
		return Ref{}, false, err
	}
	return Ref{seq: seq, chat: chat}, true, nil
}

func (tx *Tx) sameChat(ref Ref, chat policy.CanonicalChat) error {
	if ref.chat != chat {
		return ErrWrongChat
	}
	var stored string
	switch err := tx.q.QueryRowContext(tx.ctx, selectChatOfSeq, ref.seq).Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case stored != chat.JID():
		return ErrWrongChat
	}
	return nil
}

func (r *Reader) ResolveInChat(chat policy.CanonicalChat, id string, sender policy.CanonicalChat) (Found, bool, error) {
	if !messageID.MatchString(id) {
		return Found{}, false, invalid("message id")
	}
	chat, err := r.Canonical(chat)
	if err != nil {
		return Found{}, false, err
	}
	if sender, err = r.canonicalUser(sender, "sender"); err != nil {
		return Found{}, false, err
	}
	var seq int64
	var fromMe, revoked bool
	var kind string
	var text sql.NullString
	switch err := r.q.QueryRowContext(r.ctx, selectFound, chat.JID(), id, sender.JID()).Scan(&seq, &fromMe, &kind, &text, &revoked); {
	case errors.Is(err, sql.ErrNoRows):
		return Found{}, false, nil
	case err != nil:
		return Found{}, false, err
	}
	return Found{Ref: Ref{seq: seq, chat: chat}, Sender: sender, FromMe: fromMe, Kind: Kind(kind), Text: text.String, Revoked: revoked}, true, nil
}

type changeable struct {
	kind    Kind
	text    sql.NullString
	revoked bool
}

func (tx *Tx) load(ref Ref) (changeable, error) {
	if !ref.Valid() {
		return changeable{}, invalid("reference")
	}
	var c changeable
	var chat, kind string
	switch err := tx.q.QueryRowContext(tx.ctx, selectForChange, ref.seq).Scan(&chat, &kind, &c.text, &c.revoked); {
	case errors.Is(err, sql.ErrNoRows):
		return changeable{}, ErrNotFound
	case err != nil:
		return changeable{}, err
	case chat != ref.chat.JID():
		return changeable{}, ErrWrongChat
	}
	c.kind = Kind(kind)
	return c, nil
}

func (tx *Tx) forget(seq int64, old sql.NullString) error {
	if _, err := tx.q.ExecContext(tx.ctx, deleteFTS, seq, old); err != nil {
		return err
	}
	if old.Valid {
		if err := tx.noteForgotten(old.String); err != nil {
			return err
		}
	}
	_, err := tx.q.ExecContext(tx.ctx, forgetText, seq)
	return err
}

func (tx *Tx) ApplyEdit(ref Ref, text string, at time.Time) error {
	if text == "" || at.IsZero() {
		return invalid("edit")
	}
	c, err := tx.load(ref)
	switch {
	case err != nil:
		return err
	case c.revoked:
		return ErrRevoked
	case c.kind == KindReaction || c.kind == KindPollUpdate:
		return invalid("a reaction or poll update cannot be edited")
	}
	if err := tx.forget(ref.seq, c.text); err != nil {
		return err
	}
	if _, err := tx.q.ExecContext(tx.ctx, setEditedText, text, sanitize.Display(text), ms(at), ref.seq); err != nil {
		return err
	}
	_, err = tx.q.ExecContext(tx.ctx, insertFTS, ref.seq, text)
	return err
}

func (tx *Tx) ApplyRevoke(ref Ref) error {
	c, err := tx.load(ref)
	if err != nil || c.revoked {
		return err
	}
	if err := tx.forget(ref.seq, c.text); err != nil {
		return err
	}
	if _, err := tx.q.ExecContext(tx.ctx, setRevoked, ref.seq); err != nil {
		return err
	}
	_, err = tx.q.ExecContext(tx.ctx, insertFTS, ref.seq, nil)
	return err
}

func (tx *Tx) PurgeExpired(now time.Time, limit int) (int, error) {
	if now.IsZero() || limit <= 0 {
		return 0, invalid("purge")
	}
	rows, err := tx.q.QueryContext(tx.ctx, selectExpired, ms(now), limit)
	if err != nil {
		return 0, err
	}
	type expired struct {
		seq  int64
		text sql.NullString
	}
	var due []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.seq, &e.text); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		due = append(due, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for _, e := range due {
		if err := tx.forget(e.seq, e.text); err != nil {
			return 0, err
		}
		if _, err := tx.q.ExecContext(tx.ctx, insertFTS, e.seq, nil); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}
