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
	updateChatName = `UPDATE chats SET name = ?1, name_source = ?2, name_origin = ?3
		WHERE jid = ?4 AND (?3 = 'live' OR name IS NULL OR name_origin = 'history')`
	upsertPushName = `INSERT INTO contacts (jid, push_name, name_source, updated_ts, origin) VALUES (?, ?, 'push_name', ?, ?)
		ON CONFLICT (jid) DO UPDATE SET push_name = excluded.push_name, name_source = excluded.name_source, updated_ts = excluded.updated_ts, origin = excluded.origin
		WHERE excluded.origin = 'live' OR contacts.origin = 'history'`
	selectMembersLive     = "SELECT members_live FROM chats WHERE jid = ?"
	markMembersLive       = "UPDATE chats SET members_live = 1 WHERE jid = ?"
	deleteParticipants    = "DELETE FROM group_participants WHERE group_jid = ?"
	deleteHistoryMembers  = "DELETE FROM group_participants WHERE group_jid = ? AND origin = 'history'"
	insertLiveParticipant = "INSERT INTO group_participants (group_jid, user_jid, is_admin, origin, present) VALUES (?, ?, ?, 'live', 1)"
	insertHistoryMember   = "INSERT INTO group_participants (group_jid, user_jid, is_admin, origin, present) VALUES (?, ?, ?, 'history', 1) ON CONFLICT (group_jid, user_jid) DO NOTHING"
	upsertLiveParticipant = `INSERT INTO group_participants (group_jid, user_jid, is_admin, origin, present) VALUES (?, ?, ?, 'live', ?)
		ON CONFLICT (group_jid, user_jid) DO UPDATE SET is_admin = excluded.is_admin, origin = excluded.origin, present = excluded.present`
	selectAdmin = "SELECT is_admin FROM group_participants WHERE group_jid = ? AND user_jid = ? AND present = 1"
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

func (tx *Tx) SetChatName(chat policy.CanonicalChat, name string, source NameSource, origin Origin) error {
	if err := knownOrigin(origin); err != nil {
		return err
	}
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
	_, err = tx.q.ExecContext(tx.ctx, updateChatName, nullString(name), string(source), string(origin), chat.JID())
	return err
}

func (tx *Tx) SetPushName(u policy.CanonicalChat, name string, at time.Time, origin Origin) error {
	if err := knownOrigin(origin); err != nil {
		return err
	}
	u, err := tx.canonicalUser(u, "user")
	if err != nil {
		return err
	}
	if at.IsZero() {
		return invalid("time")
	}
	_, err = tx.q.ExecContext(tx.ctx, upsertPushName, u.JID(), nullString(name), ms(at), string(origin))
	return err
}

func (tx *Tx) ReplaceParticipants(g policy.CanonicalChat, participants []Participant, origin Origin) error {
	if err := knownOrigin(origin); err != nil {
		return err
	}
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
	if origin == OriginHistory {
		return tx.historyMembers(g, users, participants)
	}
	if _, err := tx.q.ExecContext(tx.ctx, markMembersLive, g.JID()); err != nil {
		return err
	}
	if _, err := tx.q.ExecContext(tx.ctx, deleteParticipants, g.JID()); err != nil {
		return err
	}
	for i, p := range participants {
		if _, err := tx.q.ExecContext(tx.ctx, insertLiveParticipant, g.JID(), users[i], boolInt(p.Admin)); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) historyMembers(g policy.CanonicalChat, users []string, participants []Participant) error {
	var live bool
	if err := tx.q.QueryRowContext(tx.ctx, selectMembersLive, g.JID()).Scan(&live); err != nil || live {
		return err
	}
	if _, err := tx.q.ExecContext(tx.ctx, deleteHistoryMembers, g.JID()); err != nil {
		return err
	}
	for i, p := range participants {
		if _, err := tx.q.ExecContext(tx.ctx, insertHistoryMember, g.JID(), users[i], boolInt(p.Admin)); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) SetParticipant(g, u policy.CanonicalChat, admin bool) error {
	return tx.liveParticipant(g, u, admin, true)
}

func (tx *Tx) RemoveParticipant(g, u policy.CanonicalChat) error {
	return tx.liveParticipant(g, u, false, false)
}

func (tx *Tx) liveParticipant(g, u policy.CanonicalChat, admin, present bool) error {
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
	_, err = tx.q.ExecContext(tx.ctx, upsertLiveParticipant, g.JID(), u.JID(), boolInt(admin), boolInt(present))
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

func knownOrigin(o Origin) error {
	if o != OriginLive && o != OriginHistory {
		return invalid("origin")
	}
	return nil
}
