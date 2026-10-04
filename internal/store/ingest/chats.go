package ingest

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

type NameSource string

const (
	NamePushName     NameSource = "push_name"
	NameGroupSubject NameSource = "group_subject"
)

type MappingSource string

const (
	MappingSenderAlt    MappingSource = "sender_alt"
	MappingRecipientAlt MappingSource = "recipient_alt"
	MappingHistory      MappingSource = "history"
)

type LIDOutcome uint8

const (
	LIDLearned LIDOutcome = iota + 1
	LIDKnown
	LIDConflict
)

type Conflict string

const (
	ConflictContradicts   Conflict = "mapping_contradicts"
	ConflictBothHaveChats Conflict = "both_chats_have_messages"
	ConflictMessageIDs    Conflict = "message_collision"
)

type LIDResult struct {
	Outcome  LIDOutcome
	Conflict Conflict
}

type Participant struct {
	User  policy.CanonicalChat
	Admin bool
}

const (
	selectLIDForPN = "SELECT lid FROM lid_map WHERE pn = ?"
	selectPNForLID = "SELECT pn FROM lid_map WHERE lid = ?"
	upsertChat     = `INSERT INTO chats (jid, kind) VALUES (?, ?) ON CONFLICT (jid) DO NOTHING`
	updateChatName = "UPDATE chats SET name = ?, name_source = ? WHERE jid = ?"
	upsertPushName = `INSERT INTO contacts (jid, push_name, name_source, updated_ts) VALUES (?, ?, 'push_name', ?)
		ON CONFLICT (jid) DO UPDATE SET push_name = excluded.push_name, name_source = excluded.name_source, updated_ts = excluded.updated_ts`
	deleteParticipants = "DELETE FROM group_participants WHERE group_jid = ?"
	upsertParticipant  = `INSERT INTO group_participants (group_jid, user_jid, is_admin) VALUES (?, ?, ?)
		ON CONFLICT (group_jid, user_jid) DO UPDATE SET is_admin = excluded.is_admin`
	insertParticipant = "INSERT INTO group_participants (group_jid, user_jid, is_admin) VALUES (?, ?, ?)"
	deleteParticipant = "DELETE FROM group_participants WHERE group_jid = ? AND user_jid = ?"
	selectAdmin       = "SELECT is_admin FROM group_participants WHERE group_jid = ? AND user_jid = ?"
)

func (r *Reader) Canonical(c policy.CanonicalChat) (policy.CanonicalChat, error) {
	if !c.Valid() {
		return policy.CanonicalChat{}, invalid("chat")
	}
	if c.Kind() != policy.PhoneChat {
		return c, nil
	}
	var lid string
	switch err := r.q.QueryRowContext(r.ctx, selectLIDForPN, c.JID()).Scan(&lid); {
	case errors.Is(err, sql.ErrNoRows):
		return c, nil
	case err != nil:
		return policy.CanonicalChat{}, err
	}
	return stored(lid, policy.LIDChat)
}

func (r *Reader) canonicalUser(c policy.CanonicalChat, what string) (policy.CanonicalChat, error) {
	if !c.Valid() || !user(c) {
		return policy.CanonicalChat{}, invalid(what)
	}
	return r.Canonical(c)
}

func (r *Reader) group(c policy.CanonicalChat) (policy.CanonicalChat, error) {
	if !c.Valid() || c.Kind() != policy.GroupChat {
		return policy.CanonicalChat{}, invalid("group")
	}
	return c, nil
}

func (tx *Tx) ensureChat(c policy.CanonicalChat) error {
	_, err := tx.q.ExecContext(tx.ctx, upsertChat, c.JID(), kindCode(c.Kind()))
	return err
}

func (tx *Tx) SetChatName(chat policy.CanonicalChat, name string, source NameSource) error {
	chat, err := tx.Canonical(chat)
	if err != nil {
		return err
	}
	want := NamePushName
	if chat.Kind() == policy.GroupChat {
		want = NameGroupSubject
	}
	if source != want {
		return invalid("name source")
	}
	if err := tx.ensureChat(chat); err != nil {
		return err
	}
	_, err = tx.q.ExecContext(tx.ctx, updateChatName, nullString(name), string(source), chat.JID())
	return err
}

func (tx *Tx) SetPushName(u policy.CanonicalChat, name string, at time.Time) error {
	u, err := tx.canonicalUser(u, "user")
	if err != nil {
		return err
	}
	if at.IsZero() {
		return invalid("time")
	}
	_, err = tx.q.ExecContext(tx.ctx, upsertPushName, u.JID(), nullString(name), ms(at))
	return err
}

func (tx *Tx) ReplaceParticipants(g policy.CanonicalChat, participants []Participant) error {
	g, err := tx.group(g)
	if err != nil {
		return err
	}
	users := make([]string, 0, len(participants))
	seen := map[string]bool{}
	for _, p := range participants {
		u, err := tx.canonicalUser(p.User, "participant")
		if err != nil {
			return err
		}
		if seen[u.JID()] {
			return invalid("participant listed twice")
		}
		seen[u.JID()] = true
		users = append(users, u.JID())
	}
	if err := tx.ensureChat(g); err != nil {
		return err
	}
	if _, err := tx.q.ExecContext(tx.ctx, deleteParticipants, g.JID()); err != nil {
		return err
	}
	for i, p := range participants {
		if _, err := tx.q.ExecContext(tx.ctx, insertParticipant, g.JID(), users[i], boolInt(p.Admin)); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) SetParticipant(g, u policy.CanonicalChat, admin bool) error {
	g, err := tx.group(g)
	if err != nil {
		return err
	}
	if u, err = tx.canonicalUser(u, "participant"); err != nil {
		return err
	}
	if err := tx.ensureChat(g); err != nil {
		return err
	}
	_, err = tx.q.ExecContext(tx.ctx, upsertParticipant, g.JID(), u.JID(), boolInt(admin))
	return err
}

func (tx *Tx) RemoveParticipant(g, u policy.CanonicalChat) error {
	g, err := tx.group(g)
	if err != nil {
		return err
	}
	if u, err = tx.canonicalUser(u, "participant"); err != nil {
		return err
	}
	_, err = tx.q.ExecContext(tx.ctx, deleteParticipant, g.JID(), u.JID())
	return err
}

func (r *Reader) ParticipantAdmin(g, u policy.CanonicalChat) (admin, known bool, err error) {
	if g, err = r.group(g); err != nil {
		return false, false, err
	}
	if u, err = r.canonicalUser(u, "participant"); err != nil {
		return false, false, err
	}
	var flag int64
	switch err := r.q.QueryRowContext(r.ctx, selectAdmin, g.JID(), u.JID()).Scan(&flag); {
	case errors.Is(err, sql.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	return flag == 1, true, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
