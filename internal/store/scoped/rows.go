package scoped

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

type ChatPosition struct {
	LastTS  int64 `json:"-"`
	Row     int64 `json:"-"`
	Undated bool  `json:"-"`
}

type MessagePosition struct {
	TS  int64 `json:"-"`
	Seq int64 `json:"-"`
}

type SearchPosition struct {
	Upper int64 `json:"-"`
}

type ChangePosition struct {
	ChangeSeq int64 `json:"-"`
	Since     int64 `json:"-"`
}

type Direction uint8

const (
	Older Direction = iota
	Newer
)

type Chat struct {
	Chat       policy.CanonicalChat `json:"-"`
	Ref        string               `json:"-"`
	Name       string               `json:"-"`
	NameSource string               `json:"-"`
	LastAt     time.Time            `json:"-"`
	Position   ChatPosition         `json:"-"`
}

type Message struct {
	Chat          policy.CanonicalChat `json:"-"`
	ChatRef       string               `json:"-"`
	ID            string               `json:"-"`
	Sender        policy.CanonicalChat `json:"-"`
	PushName      string               `json:"-"`
	SavedName     string               `json:"-"`
	FromMe        bool                 `json:"-"`
	At            time.Time            `json:"-"`
	Kind          string               `json:"-"`
	Text          string               `json:"-"`
	TextDisplay   string               `json:"-"`
	MediaType     string               `json:"-"`
	ReplyID       string               `json:"-"`
	ReplySender   policy.CanonicalChat `json:"-"`
	QuoteVerified bool                 `json:"-"`
	EditedAt      time.Time            `json:"-"`
	Revoked       bool                 `json:"-"`
	Position      MessagePosition      `json:"-"`
	Change        ChangePosition       `json:"-"`
}

type ChatPage struct {
	Chats []Chat       `json:"-"`
	Next  ChatPosition `json:"-"`
	More  bool         `json:"-"`
}

type MessagePage struct {
	Chat     Chat            `json:"-"`
	Messages []Message       `json:"-"`
	Next     MessagePosition `json:"-"`
	More     bool            `json:"-"`
}

type SearchPage struct {
	Messages []Message      `json:"-"`
	Next     SearchPosition `json:"-"`
	More     bool           `json:"-"`
}

type ChangePage struct {
	Messages []Message      `json:"-"`
	Next     ChangePosition `json:"-"`
	More     bool           `json:"-"`
}

var errCorrupt = errors.New("scoped: a stored identifier is not canonical")

func stored(jid string, kind policy.ChatKind) (policy.CanonicalChat, error) {
	c, ok := policy.Normalize(jid)
	if !ok || c.JID() != jid || c.Kind() != kind && kind != policy.InvalidChat {
		return policy.CanonicalChat{}, errCorrupt
	}
	return c, nil
}

func storedUser(jid string) (policy.CanonicalChat, error) {
	c, err := stored(jid, policy.InvalidChat)
	if err == nil && c.Kind() != policy.PhoneChat && c.Kind() != policy.LIDChat {
		return policy.CanonicalChat{}, errCorrupt
	}
	return c, err
}

func chatKind(code int64) policy.ChatKind {
	switch code {
	case 1:
		return policy.PhoneChat
	case 2:
		return policy.LIDChat
	case 3:
		return policy.GroupChat
	}
	return policy.InvalidChat
}

func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64).UTC()
}

func scanChat(rs rowScanner) (Chat, error) {
	var c Chat
	var jid string
	var kind int64
	var name, source sql.NullString
	var last sql.NullInt64
	if err := rs.Scan(&c.Position.Row, &jid, &kind, &c.Ref, &name, &source, &last); err != nil {
		return Chat{}, err
	}
	k := chatKind(kind)
	if k == policy.InvalidChat {
		return Chat{}, errCorrupt
	}
	chat, err := stored(jid, k)
	if err != nil {
		return Chat{}, err
	}
	c.Chat, c.Name, c.NameSource, c.LastAt = chat, name.String, source.String, fromMS(last)
	c.Position.LastTS, c.Position.Undated = last.Int64, !last.Valid
	return c, nil
}

func scanFirst(rs rowScanner) (sql.NullInt64, error) {
	var first sql.NullInt64
	err := rs.Scan(&first)
	return first, err
}

func scanMessage(rs rowScanner) (Message, error) {
	var m Message
	var chat, sender string
	var pushName, savedName, text, display, media, replyID, replySender sql.NullString
	var verified, edited sql.NullInt64
	var ts int64
	if err := rs.Scan(&m.Position.Seq, &ts, &m.Change.ChangeSeq, &chat, &m.ChatRef, &m.ID, &sender, &pushName, &savedName,
		&m.FromMe, &m.Kind, &text, &display, &media, &replyID, &replySender, &verified, &edited, &m.Revoked); err != nil {
		return Message{}, err
	}
	var err error
	if m.Chat, err = stored(chat, policy.InvalidChat); err != nil {
		return Message{}, err
	}
	if m.Sender, err = storedUser(sender); err != nil {
		return Message{}, err
	}
	if replyID.Valid {
		if m.ReplySender, err = storedUser(replySender.String); err != nil {
			return Message{}, err
		}
		m.ReplyID, m.QuoteVerified = replyID.String, verified.Int64 == 1
	}
	m.Position.TS, m.At, m.EditedAt = ts, time.UnixMilli(ts).UTC(), fromMS(edited)
	m.PushName, m.SavedName, m.Text, m.TextDisplay, m.MediaType = pushName.String, savedName.String, text.String, display.String, media.String
	return m, nil
}
