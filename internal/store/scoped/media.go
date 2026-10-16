package scoped

import (
	"context"
	"database/sql"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

const selectAttachment = `SELECT m.chat_jid, m.id, m.sender_jid, r.ref, r.kind, r.variant, r.declared, r.expires_at FROM messages m
		LEFT JOIN media_refs r ON r.seq = m.seq AND m.revoked = 0 AND m.kind = 'media' AND m.media_type IS NOT NULL
		WHERE m.chat_jid = ?1 AND m.id = ?2 AND m.sender_jid = ?3{scope m.chat_jid}`

var mediaKinds = map[string]bool{"image": true, "video": true, "audio": true, "document": true, "sticker": true}

type SealedRef struct {
	text string
}

func (r SealedRef) Sealed() string { return r.text }

type Attachment struct {
	Chat     policy.CanonicalChat `json:"-"`
	ID       string               `json:"-"`
	Sender   policy.CanonicalChat `json:"-"`
	Ref      SealedRef            `json:"-"`
	Kind     string               `json:"-"`
	Variant  string               `json:"-"`
	Declared int64                `json:"-"`
	Expires  time.Time            `json:"-"`
	Usable   bool                 `json:"-"`
}

func (r *Reader) Attachment(g policy.ReadGrant, ctx context.Context, chat policy.CanonicalChat, id string, sender policy.CanonicalChat, now time.Time) (Attachment, bool, error) {
	if now.IsZero() {
		return Attachment{}, false, ErrInvalid
	}
	if !chat.Valid() || !messageID.MatchString(id) || sender.Kind() != policy.PhoneChat && sender.Kind() != policy.LIDChat {
		return Attachment{}, false, nil
	}
	var a Attachment
	var found bool
	err := r.read(g, ctx, "scoped.attachment", func(ctx context.Context, q querier) error {
		chat, err := canonical(ctx, q, chat)
		if err != nil {
			return err
		}
		if sender, err = canonical(ctx, q, sender); err != nil || !g.Media() || !g.Allows(chat) {
			return err
		}
		got, err := rowsOf[Attachment]{q, scanAttachment}.QueryContext(ctx, selectAttachment, chat.JID(), id, sender.JID())
		if err != nil || len(got) == 0 {
			return err
		}
		a, found = got[0], true
		if !a.Expires.After(now) {
			a = Attachment{Chat: a.Chat, ID: a.ID, Sender: a.Sender}
		}
		return nil
	})
	if err != nil {
		return Attachment{}, false, err
	}
	return a, found, nil
}

func scanAttachment(rs rowScanner) (Attachment, error) {
	var a Attachment
	var chat, sender string
	var ref, kind, variant sql.NullString
	var declared, expires sql.NullInt64
	if err := rs.Scan(&chat, &a.ID, &sender, &ref, &kind, &variant, &declared, &expires); err != nil {
		return Attachment{}, err
	}
	var err error
	if a.Chat, err = stored(chat, policy.InvalidChat); err != nil {
		return Attachment{}, err
	}
	if a.Sender, err = storedUser(sender); err != nil {
		return Attachment{}, err
	}
	if !ref.Valid {
		return a, nil
	}
	if ref.String == "" || !mediaKinds[kind.String] || !declared.Valid || !expires.Valid {
		return Attachment{}, errCorrupt
	}
	a.Ref, a.Kind, a.Variant, a.Declared, a.Expires, a.Usable = SealedRef{text: ref.String}, kind.String, variant.String, declared.Int64, fromMS(expires), true
	return a, nil
}
