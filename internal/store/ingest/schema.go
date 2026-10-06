package ingest

var migrations = []string{schemaV1, schemaV2}

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
