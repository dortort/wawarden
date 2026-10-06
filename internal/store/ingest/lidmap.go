package ingest

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	selectMappedPN  = "SELECT pn FROM lid_map WHERE lid = ?"
	selectMappedLID = "SELECT lid FROM lid_map WHERE pn = ?"
	selectChatState = "SELECT EXISTS (SELECT 1 FROM chats WHERE jid = ?1), EXISTS (SELECT 1 FROM messages WHERE chat_jid = ?1)"
	selectCollision = `SELECT EXISTS (SELECT 1 FROM messages a JOIN messages b ON b.chat_jid = a.chat_jid AND b.id = a.id
		WHERE a.sender_jid = ?1 AND b.sender_jid = ?2)`
	insertMapping = "INSERT INTO lid_map (lid, pn, source, learned_ts) VALUES (?, ?, ?, ?)"
	mergeChat     = `UPDATE chats SET
		name = CASE WHEN other.outranks THEN other.name ELSE chats.name END,
		name_source = CASE WHEN other.outranks THEN other.name_source ELSE chats.name_source END,
		name_origin = CASE WHEN other.outranks THEN other.name_origin ELSE chats.name_origin END,
		last_ts = CASE WHEN other.last_ts IS NULL THEN chats.last_ts WHEN chats.last_ts IS NULL THEN other.last_ts ELSE max(chats.last_ts, other.last_ts) END
		FROM (SELECT o.name, o.name_source, o.name_origin, o.last_ts,
			o.name IS NOT NULL AND (t.name IS NULL OR o.name_origin = 'live' AND t.name_origin = 'history') AS outranks
			FROM chats o, chats t WHERE o.jid = ?1 AND t.jid = ?2) AS other WHERE chats.jid = ?2`
	repointAliases   = "UPDATE chat_aliases SET jid = ?2 WHERE jid = ?1"
	deleteChat       = "DELETE FROM chats WHERE jid = ?"
	rekeyChat        = "UPDATE chats SET jid = ?2, kind = 2 WHERE jid = ?1"
	insertAlias      = "INSERT INTO chat_aliases (alias, jid) VALUES (?, ?)"
	rekeySenders     = "UPDATE messages SET sender_jid = ?2 WHERE sender_jid = ?1"
	dropParticipants = `DELETE FROM group_participants AS p WHERE p.user_jid = ?1 AND EXISTS (SELECT 1 FROM group_participants o
		WHERE o.group_jid = p.group_jid AND o.user_jid = ?2 AND (o.origin = 'live' OR p.origin = 'history'))`
	dropOutranked = `DELETE FROM group_participants AS o WHERE o.user_jid = ?2 AND EXISTS (SELECT 1 FROM group_participants p
		WHERE p.group_jid = o.group_jid AND p.user_jid = ?1)`
	rekeyParticipants = "UPDATE group_participants SET user_jid = ?2 WHERE user_jid = ?1"
	takeNewerPushName = `UPDATE contacts SET push_name = other.push_name, name_source = other.name_source, updated_ts = other.updated_ts, origin = other.origin
		FROM (SELECT push_name, name_source, updated_ts, origin FROM contacts WHERE jid = ?1) AS other
		WHERE contacts.jid = ?2 AND (other.origin = 'live' AND contacts.origin = 'history' OR other.origin = contacts.origin AND other.updated_ts > contacts.updated_ts)`
	dropMergedContact = "DELETE FROM contacts WHERE jid = ?1 AND EXISTS (SELECT 1 FROM contacts WHERE jid = ?2)"
	rekeyContact      = "UPDATE contacts SET jid = ?2 WHERE jid = ?1"
)

func (tx *Tx) LearnLID(lid, pn policy.CanonicalChat, source MappingSource, at time.Time) (LIDResult, error) {
	switch {
	case !lid.Valid() || lid.Kind() != policy.LIDChat:
		return LIDResult{}, invalid("lid")
	case !pn.Valid() || pn.Kind() != policy.PhoneChat:
		return LIDResult{}, invalid("phone number")
	case source != MappingSenderAlt && source != MappingRecipientAlt && source != MappingHistory:
		return LIDResult{}, invalid("mapping source")
	case at.IsZero():
		return LIDResult{}, invalid("time")
	}
	var mappedPN, mappedLID sql.NullString
	if err := tx.q.QueryRowContext(tx.ctx, selectMappedPN, lid.JID()).Scan(&mappedPN); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LIDResult{}, err
	}
	if err := tx.q.QueryRowContext(tx.ctx, selectMappedLID, pn.JID()).Scan(&mappedLID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LIDResult{}, err
	}
	switch {
	case mappedPN.Valid && mappedPN.String == pn.JID():
		return LIDResult{Outcome: LIDKnown}, nil
	case mappedPN.Valid || mappedLID.Valid:
		return LIDResult{Outcome: LIDConflict, Conflict: ConflictContradicts}, nil
	}
	pnChat, pnMessages, err := tx.chatState(pn)
	if err != nil {
		return LIDResult{}, err
	}
	lidChat, lidMessages, err := tx.chatState(lid)
	if err != nil {
		return LIDResult{}, err
	}
	if pnMessages && lidMessages {
		return LIDResult{Outcome: LIDConflict, Conflict: ConflictBothHaveChats}, nil
	}
	var collision bool
	if err := tx.q.QueryRowContext(tx.ctx, selectCollision, pn.JID(), lid.JID()).Scan(&collision); err != nil {
		return LIDResult{}, err
	}
	if collision {
		return LIDResult{Outcome: LIDConflict, Conflict: ConflictMessageIDs}, nil
	}
	if err := tx.rekey(pn.JID(), lid.JID(), pnChat, lidChat, pnMessages); err != nil {
		return LIDResult{}, err
	}
	if _, err := tx.q.ExecContext(tx.ctx, insertMapping, lid.JID(), pn.JID(), string(source), ms(at)); err != nil {
		return LIDResult{}, err
	}
	return LIDResult{Outcome: LIDLearned}, nil
}

func (r *Reader) chatState(c policy.CanonicalChat) (exists, messages bool, err error) {
	err = r.q.QueryRowContext(r.ctx, selectChatState, c.JID()).Scan(&exists, &messages)
	return exists, messages, err
}

func (tx *Tx) rekey(pn, lid string, pnChat, lidChat, pnMessages bool) error {
	var err error
	exec := func(_ sql.Result, e error) {
		if err == nil {
			err = e
		}
	}
	q, ctx := tx.q, tx.ctx
	switch {
	case pnChat && lidChat && !pnMessages:
		exec(q.ExecContext(ctx, mergeChat, pn, lid))
		exec(q.ExecContext(ctx, repointAliases, pn, lid))
		exec(q.ExecContext(ctx, deleteChat, pn))
		exec(q.ExecContext(ctx, insertAlias, pn, lid))
	case pnChat && lidChat:
		exec(q.ExecContext(ctx, mergeChat, lid, pn))
		exec(q.ExecContext(ctx, repointAliases, lid, pn))
		exec(q.ExecContext(ctx, deleteChat, lid))
		exec(q.ExecContext(ctx, rekeyChat, pn, lid))
		exec(q.ExecContext(ctx, insertAlias, pn, lid))
	case pnChat:
		exec(q.ExecContext(ctx, rekeyChat, pn, lid))
		exec(q.ExecContext(ctx, insertAlias, pn, lid))
	}
	exec(q.ExecContext(ctx, rekeySenders, pn, lid))
	exec(q.ExecContext(ctx, dropParticipants, pn, lid))
	exec(q.ExecContext(ctx, dropOutranked, pn, lid))
	exec(q.ExecContext(ctx, rekeyParticipants, pn, lid))
	exec(q.ExecContext(ctx, takeNewerPushName, pn, lid))
	exec(q.ExecContext(ctx, dropMergedContact, pn, lid))
	exec(q.ExecContext(ctx, rekeyContact, pn, lid))
	return err
}
