package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

func migrate(ctx context.Context, d *db.DB) (int, error) {
	return d.Migrate(ctx, []string{schemaV1, schemaV2, schemaV3})
}

const (
	changeBackfillBatch = 10000
	selectUnnumbered    = "SELECT seq FROM messages WHERE change_seq = 0 ORDER BY seq LIMIT 1"
	selectLastSeq       = "SELECT coalesce(max(seq), 0) FROM messages"
	numberChanges       = "UPDATE messages SET change_seq = seq WHERE seq > ?1 AND seq <= ?2 AND change_seq = 0"
)

func backfillChanges(ctx context.Context, d *db.DB) error {
	var first, last int64
	err := d.Read(ctx, "ingest.change_backfill", func(ctx context.Context, q db.Querier) error {
		if err := q.QueryRowContext(ctx, selectUnnumbered).Scan(&first); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, selectLastSeq).Scan(&last)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingest: find the messages without a change number: %w", err)
	}
	for from := first - 1; from < last; from += changeBackfillBatch {
		if err := d.Rewrite(ctx, "ingest.change_backfill", func(ctx context.Context, q db.Querier) error {
			_, err := q.ExecContext(ctx, numberChanges, from, min(from+changeBackfillBatch, last))
			return err
		}); err != nil {
			return fmt.Errorf("ingest: number the changes of messages %d to %d: %w", from+1, min(from+changeBackfillBatch, last), err)
		}
	}
	return nil
}

const schemaV1 = `
CREATE TABLE chats (
	jid TEXT PRIMARY KEY,
	kind INTEGER NOT NULL CHECK (kind BETWEEN 1 AND 3),
	name TEXT,
	name_source TEXT CHECK (name_source = 'push_name' OR name_source = 'group_subject'),
	last_ts INTEGER
) STRICT;

CREATE TABLE chat_aliases (
	alias TEXT PRIMARY KEY,
	jid TEXT NOT NULL REFERENCES chats (jid) ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE TABLE lid_map (
	lid TEXT PRIMARY KEY,
	pn TEXT NOT NULL UNIQUE,
	source TEXT NOT NULL CHECK (source = 'sender_alt' OR source = 'recipient_alt' OR source = 'history'),
	learned_ts INTEGER NOT NULL
) STRICT;

CREATE TABLE contacts (
	jid TEXT PRIMARY KEY,
	push_name TEXT,
	name_source TEXT CHECK (name_source = 'push_name'),
	updated_ts INTEGER NOT NULL
) STRICT;

CREATE TABLE group_participants (
	group_jid TEXT NOT NULL REFERENCES chats (jid) ON UPDATE CASCADE ON DELETE CASCADE,
	user_jid TEXT NOT NULL,
	is_admin INTEGER NOT NULL CHECK (is_admin BETWEEN 0 AND 1),
	PRIMARY KEY (group_jid, user_jid)
) STRICT, WITHOUT ROWID;

CREATE INDEX group_participants_user ON group_participants (user_jid);

CREATE TABLE messages (
	seq INTEGER PRIMARY KEY,
	chat_jid TEXT NOT NULL REFERENCES chats (jid) ON UPDATE CASCADE,
	id TEXT NOT NULL,
	sender_jid TEXT NOT NULL,
	from_me INTEGER NOT NULL CHECK (from_me BETWEEN 0 AND 1),
	origin TEXT NOT NULL CHECK (origin = 'live' OR origin = 'history'),
	sender_alt TEXT,
	addressing_mode TEXT CHECK (addressing_mode = 'pn' OR addressing_mode = 'lid'),
	ts INTEGER NOT NULL,
	kind TEXT NOT NULL CHECK (kind = 'text' OR kind = 'media' OR kind = 'reaction' OR kind = 'poll_update' OR kind = 'other'),
	text TEXT,
	text_display TEXT,
	media_type TEXT,
	quoted_ref INTEGER REFERENCES messages (seq),
	quote_verified INTEGER CHECK (quote_verified BETWEEN 0 AND 1),
	edited_ts INTEGER,
	revoked INTEGER NOT NULL DEFAULT 0 CHECK (revoked BETWEEN 0 AND 1),
	expires_at INTEGER,
	UNIQUE (chat_jid, id, sender_jid)
) STRICT;

CREATE INDEX messages_sender ON messages (sender_jid);

CREATE INDEX messages_expiring ON messages (expires_at) WHERE expires_at IS NOT NULL AND text IS NOT NULL;

CREATE VIRTUAL TABLE messages_fts USING fts5 (text, content = 'messages', content_rowid = 'seq', tokenize = 'trigram');

INSERT INTO messages_fts (messages_fts, rank) VALUES ('secure-delete', 1);

CREATE TABLE history_blobs (
	id TEXT PRIMARY KEY,
	received_at INTEGER NOT NULL,
	ref BLOB,
	attempts INTEGER NOT NULL DEFAULT 0,
	processed_at INTEGER,
	quarantined INTEGER NOT NULL DEFAULT 0 CHECK (quarantined BETWEEN 0 AND 1)
) STRICT;

CREATE TABLE inbox (
	seq INTEGER PRIMARY KEY,
	received_at INTEGER NOT NULL,
	payload BLOB,
	attempts INTEGER NOT NULL DEFAULT 0,
	quarantined INTEGER NOT NULL DEFAULT 0 CHECK (quarantined BETWEEN 0 AND 1)
) STRICT;

CREATE INDEX inbox_backlog ON inbox (seq) WHERE quarantined = 0;

CREATE TABLE sync_state (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT, WITHOUT ROWID;
`

const schemaV2 = `
ALTER TABLE chats ADD COLUMN name_origin TEXT CHECK (name_origin = 'live' OR name_origin = 'history');

ALTER TABLE chats ADD COLUMN members_live INTEGER NOT NULL DEFAULT 0 CHECK (members_live BETWEEN 0 AND 1);

ALTER TABLE contacts ADD COLUMN origin TEXT NOT NULL DEFAULT 'live' CHECK (origin = 'live' OR origin = 'history');

ALTER TABLE group_participants ADD COLUMN origin TEXT NOT NULL DEFAULT 'live' CHECK (origin = 'live' OR origin = 'history');

ALTER TABLE group_participants ADD COLUMN present INTEGER NOT NULL DEFAULT 1 CHECK (present BETWEEN 0 AND 1);
`

const schemaV3 = `
ALTER TABLE chats ADD COLUMN ref TEXT CHECK (ref IS NULL OR length(ref) = 32);

UPDATE chats SET ref = lower(hex(randomblob(16)));

CREATE UNIQUE INDEX chats_ref ON chats (ref);

CREATE INDEX chats_last ON chats (last_ts);

CREATE TRIGGER chats_assign_ref AFTER INSERT ON chats WHEN NEW.ref IS NULL BEGIN
	UPDATE chats SET ref = lower(hex(randomblob(16))) WHERE rowid = NEW.rowid;
END;

CREATE TABLE contact_names (
	jid TEXT PRIMARY KEY,
	full_name TEXT,
	first_name TEXT,
	updated_ts INTEGER NOT NULL
) STRICT;

ALTER TABLE messages ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;

CREATE INDEX messages_chat_ts ON messages (chat_jid, ts);

CREATE INDEX messages_change ON messages (change_seq);

CREATE INDEX messages_change_chat ON messages (chat_jid, change_seq);

CREATE TRIGGER messages_change_insert AFTER INSERT ON messages BEGIN
	UPDATE messages SET change_seq = (SELECT max(change_seq) FROM messages) + 1 WHERE seq = NEW.seq;
END;

CREATE TRIGGER messages_change_update AFTER UPDATE OF text, text_display, edited_ts, revoked ON messages BEGIN
	UPDATE messages SET change_seq = (SELECT max(change_seq) FROM messages) + 1 WHERE seq = NEW.seq;
END;

CREATE TABLE clients (
	id TEXT PRIMARY KEY CHECK (length(id) = 8),
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	token_hash BLOB NOT NULL CHECK (length(token_hash) = 32),
	read_all INTEGER NOT NULL CHECK (read_all BETWEEN 0 AND 1),
	allow_first_contact INTEGER NOT NULL DEFAULT 0 CHECK (allow_first_contact BETWEEN 0 AND 1),
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL CHECK (expires_at > created_at AND expires_at - created_at <= 31622400000),
	revoked_at INTEGER
) STRICT;

CREATE TABLE client_read_chats (
	client_id TEXT NOT NULL REFERENCES clients (id),
	chat_jid TEXT NOT NULL,
	PRIMARY KEY (client_id, chat_jid)
) STRICT, WITHOUT ROWID;

CREATE INDEX client_read_chats_chat ON client_read_chats (chat_jid);

CREATE TABLE client_write_chats (
	client_id TEXT NOT NULL REFERENCES clients (id),
	chat_jid TEXT NOT NULL,
	PRIMARY KEY (client_id, chat_jid)
) STRICT, WITHOUT ROWID;

CREATE INDEX client_write_chats_chat ON client_write_chats (chat_jid);

CREATE TRIGGER client_write_chats_insert BEFORE INSERT ON client_write_chats
WHEN (SELECT read_all FROM clients WHERE id = NEW.client_id) = 1 BEGIN
	SELECT RAISE(ABORT, 'a client that reads all chats holds no write chat');
END;

CREATE TRIGGER client_write_chats_update BEFORE UPDATE ON client_write_chats
WHEN (SELECT read_all FROM clients WHERE id = NEW.client_id) = 1 BEGIN
	SELECT RAISE(ABORT, 'a client that reads all chats holds no write chat');
END;

CREATE TRIGGER clients_read_all BEFORE UPDATE OF read_all ON clients
WHEN NEW.read_all = 1 AND EXISTS (SELECT 1 FROM client_write_chats WHERE client_id = NEW.id) BEGIN
	SELECT RAISE(ABORT, 'a client that reads all chats holds no write chat');
END;

CREATE TABLE audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	client_id TEXT NOT NULL,
	action TEXT NOT NULL,
	chat TEXT,
	chat_hmac TEXT,
	ok INTEGER NOT NULL CHECK (ok BETWEEN 0 AND 1),
	reason TEXT NOT NULL,
	peer TEXT,
	key_id TEXT NOT NULL,
	row_hmac BLOB NOT NULL CHECK (length(row_hmac) = 32)
) STRICT;

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit BEGIN
	SELECT RAISE(ABORT, 'audit rows are append-only');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit BEGIN
	SELECT RAISE(ABORT, 'audit rows are append-only');
END;

CREATE TRIGGER audit_no_replace BEFORE INSERT ON audit
WHEN NEW.id IS NOT NULL AND EXISTS (SELECT 1 FROM audit WHERE id = NEW.id) BEGIN
	SELECT RAISE(ABORT, 'audit rows are append-only');
END;
`
