package ingest

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

func openAtVersion(t *testing.T, opts Options, steps []string, seed ...string) {
	t.Helper()
	d, err := db.Open(t.Context(), db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout, RewriteTimeout: opts.RewriteTimeout})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := d.Migrate(t.Context(), steps); err != nil {
		t.Fatalf("Migrate to schema version %d: %v", len(steps), err)
	}
	if err := d.Write(t.Context(), "test.seed", func(ctx context.Context, q db.Querier) error {
		for _, stmt := range seed {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func seedMessages(chatJID string, n int) []string {
	return []string{
		"INSERT INTO chats (jid, kind, last_ts) VALUES ('" + chatJID + "', 1, 1)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < " + strconv.Itoa(n) + ") " +
			"INSERT INTO messages (chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display) " +
			"SELECT '" + chatJID + "', 'M' || i, '" + chatJID + "', 0, 'live', i, 'text', 'synthetic ' || i, 'synthetic ' || i FROM n",
	}
}

var chatRef = regexp.MustCompile(`^[0-9a-f]{32}$`)

func requireNumbered(t *testing.T, s *Store, want int) {
	t.Helper()
	if n := scalar[int](t, s, "SELECT count(*) FROM messages"); n != want {
		t.Fatalf("%d messages, want %d", n, want)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages WHERE change_seq != seq"); n != 0 {
		t.Fatalf("%d messages whose change number is not their sequence number after the backfill", n)
	}
}

func TestMigratingToVersion3NumbersEveryMessageAndNamesEveryChat(t *testing.T) {
	opts := testOptions(t)
	openAtVersion(t, opts, []string{schemaV1, schemaV2}, append(seedMessages(alice, 25001),
		"INSERT INTO chats (jid, kind, name, name_source) VALUES ('"+groupJID+"', 3, 'Subject', 'group_subject')")...)
	s := openWith(t, opts)
	if s.SchemaVersion() != 4 {
		t.Fatalf("schema version %d, want 4", s.SchemaVersion())
	}
	requireNumbered(t, s, 25001)
	refs := strings.Fields(scalar[string](t, s, "SELECT group_concat(ref, ' ') FROM chats"))
	if len(refs) != 2 || refs[0] == refs[1] || !chatRef.MatchString(refs[0]) || !chatRef.MatchString(refs[1]) {
		t.Fatalf("chat references %q, want two distinct 32-digit lower-case hex strings", refs)
	}
	insert(t, s, textMessage(t, alice, "after", alice, "after the migration"))
	if cs := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'after'"); cs != 25002 {
		t.Fatalf("the first message after the migration has change number %d, want 25002", cs)
	}
}

func TestTheChangeBackfillResumesAfterAStop(t *testing.T) {
	opts := testOptions(t)
	openAtVersion(t, opts, []string{schemaV1, schemaV2}, seedMessages(alice, 30)...)
	openAtVersion(t, opts, []string{schemaV1, schemaV2, schemaV3}, "UPDATE messages SET change_seq = seq WHERE seq <= 12")
	s := openWith(t, opts)
	requireNumbered(t, s, 30)
}

func TestOpeningAnEmptyArchiveNumbersFromOne(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "M1", alice, "first"))
	if cs := scalar[int](t, s, "SELECT change_seq FROM messages"); cs != 1 {
		t.Fatalf("change number %d, want 1", cs)
	}
}

func changeOf(t *testing.T, s *Store, id string) int {
	t.Helper()
	return scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = ?", id)
}

func TestEveryChangeTakesTheNextChangeNumber(t *testing.T) {
	s := openStore(t)
	a := textMessage(t, alice, "A", alice, "first text")
	a.ExpiresAt = epoch.Add(time.Hour)
	insert(t, s, a)
	refB := insert(t, s, textMessage(t, alice, "B", alice, "second text"))
	insert(t, s, textMessage(t, bob, "C", bob, "third text"))
	if got := []int{changeOf(t, s, "A"), changeOf(t, s, "B"), changeOf(t, s, "C")}; got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("change numbers after three inserts %v, want 1, 2, 3", got)
	}
	insert(t, s, textMessage(t, alice, "A", alice, "a duplicate"))
	if changeOf(t, s, "A") != 1 {
		t.Fatal("a duplicate insert changed the change number")
	}
	last := 3
	for _, step := range []struct {
		name string
		id   string
		fn   func(*Tx) error
	}{
		{"an edit", "B", func(tx *Tx) error { return tx.ApplyEdit(refB, "second, edited", epoch.Add(time.Minute)) }},
		{"a revoke", "B", func(tx *Tx) error { return tx.ApplyRevoke(refB) }},
		{"an expiry", "A", func(tx *Tx) error { _, err := tx.PurgeExpired(epoch.Add(2*time.Hour), 10); return err }},
	} {
		write(t, s, step.fn)
		got := changeOf(t, s, step.id)
		if got <= last || got != scalar[int](t, s, "SELECT max(change_seq) FROM messages") {
			t.Fatalf("after %s the message has change number %d, want the largest, above %d", step.name, got, last)
		}
		last = got
	}
	if res := learn(t, s, bobLID, bob, MappingSenderAlt); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID = %+v", res)
	}
	if changeOf(t, s, "C") != 3 {
		t.Fatal("re-keying a chat changed the change number of its messages")
	}
	if n := scalar[int](t, s, "SELECT count(*) - count(DISTINCT change_seq) FROM messages"); n != 0 {
		t.Fatalf("%d repeated change numbers", n)
	}
}

func TestChatsKeepOneReferenceForLife(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "A", alice, "by phone"))
	write(t, s, func(tx *Tx) error { return tx.SetChatName(chat(t, groupJID), "Subject", NameGroupSubject, OriginLive) })
	ref := scalar[string](t, s, "SELECT ref FROM chats WHERE jid = ?", alice)
	if !chatRef.MatchString(ref) || !chatRef.MatchString(scalar[string](t, s, "SELECT ref FROM chats WHERE jid = ?", groupJID)) {
		t.Fatal("a new chat has no reference")
	}
	insert(t, s, textMessage(t, alice, "B", alice, "again"))
	if res := learn(t, s, aliceLID, alice, MappingSenderAlt); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID = %+v", res)
	}
	if got := scalar[string](t, s, "SELECT ref FROM chats WHERE jid = ?", aliceLID); got != ref {
		t.Fatalf("the re-keyed chat's reference %q, want %q", got, ref)
	}
	err := s.Write(t.Context(), "test.duplicate", func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, "UPDATE chats SET ref = ?1 WHERE jid = ?2", ref, groupJID)
		return err
	})
	if err == nil {
		t.Fatal("two chats share a reference")
	}
}

func execAll(t *testing.T, s *Store, stmts ...string) error {
	t.Helper()
	return s.Write(t.Context(), "test.exec", func(tx *Tx) error {
		for _, stmt := range stmts {
			if _, err := tx.q.ExecContext(tx.ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

const (
	hash32 = "x'00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'"
	dayMS  = 86400000
)

func clientRow(id, name string, readAll int, created, expires int64) string {
	return "INSERT INTO clients (id, name, token_hash, read_all, created_at, expires_at) VALUES ('" + id + "', '" + name + "', " + hash32 + ", " +
		strconv.Itoa(readAll) + ", " + strconv.FormatInt(created, 10) + ", " + strconv.FormatInt(expires, 10) + ")"
}

func TestClientTablesRefuseInvalidRows(t *testing.T) {
	s := openStore(t)
	if err := execAll(t, s,
		clientRow("aaaaaaaa", "Reader", 0, 1, 1+366*dayMS),
		clientRow("bbbbbbbb", "Everything", 1, 1, 2),
		"INSERT INTO client_read_chats (client_id, chat_jid) VALUES ('aaaaaaaa', '"+alice+"')",
		"INSERT INTO client_write_chats (client_id, chat_jid) VALUES ('aaaaaaaa', '"+alice+"')",
	); err != nil {
		t.Fatalf("valid client rows refused: %v", err)
	}
	const (
		scopeRefusal   = "a client that reads all chats holds no write chat"
		replaceRefusal = "a client row is never replaced"
		replacedClient = " INTO clients (id, name, token_hash, read_all, created_at, expires_at) VALUES ('aaaaaaaa', 'Reader', " + hash32 + ", 1, 1, 2)"
	)
	for name, tt := range map[string]struct{ stmt, want string }{
		"a short id":                              {clientRow("cccccc", "Short", 0, 1, 2), "CHECK constraint failed"},
		"a name taken in another case":            {clientRow("cccccccc", "READER", 0, 1, 2), "UNIQUE constraint failed: clients.name"},
		"a short token hash":                      {"INSERT INTO clients (id, name, token_hash, read_all, created_at, expires_at) VALUES ('cccccccc', 'Hash', x'00', 0, 1, 2)", "CHECK constraint failed"},
		"read_all out of range":                   {clientRow("cccccccc", "Flag", 2, 1, 2), "CHECK constraint failed"},
		"an expiry before creation":               {clientRow("cccccccc", "Early", 0, 5, 5), "CHECK constraint failed"},
		"an expiry beyond 366 days":               {clientRow("cccccccc", "Late", 0, 1, 2+366*dayMS), "CHECK constraint failed"},
		"a read chat of an unknown client":        {"INSERT INTO client_read_chats (client_id, chat_jid) VALUES ('zzzzzzzz', '" + alice + "')", "FOREIGN KEY constraint failed"},
		"a write chat for a read-all client":      {"INSERT INTO client_write_chats (client_id, chat_jid) VALUES ('bbbbbbbb', '" + alice + "')", scopeRefusal},
		"a write chat moved to a read-all client": {"UPDATE client_write_chats SET client_id = 'bbbbbbbb'", scopeRefusal},
		"read_all for a client with write chats":  {"UPDATE clients SET read_all = 1 WHERE id = 'aaaaaaaa'", scopeRefusal},
		"a client replaced by an insert":          {"INSERT OR REPLACE" + replacedClient, replaceRefusal},
		"a client replaced":                       {"REPLACE" + replacedClient, replaceRefusal},
	} {
		if err := execAll(t, s, tt.stmt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", name, err, tt.want)
		}
	}
	if got := scalar[string](t, s, "SELECT read_all || expires_at || (SELECT count(*) FROM client_write_chats WHERE client_id = id) FROM clients WHERE id = 'aaaaaaaa'"); got != "0"+strconv.FormatInt(1+366*dayMS, 10)+"1" {
		t.Fatalf("client aaaaaaaa = %q, want it unchanged with its write chat", got)
	}
}

func TestAuditRowsAreAppendOnly(t *testing.T) {
	s := openStore(t)
	row := "INSERT INTO audit (ts, client_id, action, ok, reason, key_id, row_hmac) VALUES (1, 'aaaaaaaa', 'read', 1, 'ok', 'k1', " + hash32 + ")"
	if err := execAll(t, s, row, row); err != nil {
		t.Fatalf("audit rows refused: %v", err)
	}
	forged := " INTO audit (id, ts, client_id, action, ok, reason, key_id, row_hmac) VALUES (1, 99, 'bbbbbbbb', 'forged', 1, 'ok', 'k1', " + hash32 + ")"
	for name, tt := range map[string]struct{ stmt, want string }{
		"an update":            {"UPDATE audit SET ok = 0", "audit rows are append-only"},
		"a delete":             {"DELETE FROM audit WHERE id = 1", "audit rows are append-only"},
		"an insert or replace": {"INSERT OR REPLACE" + forged, "audit rows are append-only"},
		"a replace":            {"REPLACE" + forged, "audit rows are append-only"},
		"a short row MAC":      {"INSERT INTO audit (ts, client_id, action, ok, reason, key_id, row_hmac) VALUES (1, 'aaaaaaaa', 'read', 1, 'ok', 'k1', x'00')", "CHECK constraint failed"},
	} {
		if err := execAll(t, s, tt.stmt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s of the audit table = %v, want %q", name, err, tt.want)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM audit WHERE ok = 1"); n != 2 {
		t.Fatalf("%d audit rows, want the two appended", n)
	}
	if got := scalar[string](t, s, "SELECT ts || client_id || action FROM audit WHERE id = 1"); got != "1aaaaaaaaread" {
		t.Fatalf("audit row 1 = %q, want it unchanged", got)
	}
}
