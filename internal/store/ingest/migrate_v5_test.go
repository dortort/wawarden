package ingest

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

func seedVersion4(t *testing.T, opts Options) int {
	t.Helper()
	d, err := db.Open(t.Context(), db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout, RewriteTimeout: opts.RewriteTimeout})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := d.Migrate(t.Context(), []string{schemaV1, schemaV2, schemaV3, schemaV4}); err != nil {
		t.Fatalf("Migrate to schema version 4: %v", err)
	}
	var changes int
	if err := d.Write(t.Context(), "test.seed", func(ctx context.Context, q db.Querier) error {
		for _, stmt := range []string{
			"INSERT INTO chats (jid, kind) VALUES ('" + alice + "', 1), ('" + groupJID + "', 3)",
			"INSERT INTO messages (chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display, media_type, revoked, expires_at) VALUES " +
				"('" + alice + "', 'R1', '" + alice + "', 0, 'live', 1, 'media', NULL, NULL, 'image/jpeg', 1, NULL), " +
				"('" + alice + "', 'R2', '" + alice + "', 0, 'live', 2, 'text', NULL, NULL, NULL, 1, NULL), " +
				"('" + groupJID + "', 'M1', '" + alice + "', 0, 'history', 3, 'media', 'synthetic caption', 'synthetic caption', 'video/mp4', 0, NULL), " +
				"('" + groupJID + "', 'E1', '" + bob + "', 0, 'live', 4, 'media', NULL, NULL, 'image/png', 0, 5)",
			clientRow("aaaaaaaa", "Reader", 0, 1, 2),
		} {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return q.QueryRowContext(ctx, "SELECT max(change_seq) FROM messages").Scan(&changes)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	audit := admin.NewAudit(d, opts.Master, io.Discard)
	for i := range 3 {
		if err := audit.Record(t.Context(), admin.Event{At: epoch.Add(time.Duration(i) * time.Second), Client: "aaaaaaaa", Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return changes
}

func TestMigratingToVersion5AddsMediaReferencesAndClearsRevokedMediaTypes(t *testing.T) {
	opts := testOptions(t)
	opts.Master = testMaster(t)
	changes := seedVersion4(t, opts)
	s := openWith(t, opts)
	if s.SchemaVersion() != 6 || LatestSchemaVersion() != 6 {
		t.Fatalf("schema version %d, latest %d, want 6", s.SchemaVersion(), LatestSchemaVersion())
	}
	rep, err := admin.Verify(t.Context(), copyArchive(t, s), opts.Master, nil)
	if err != nil || !rep.OK() || rep.Rows != 3 || rep.Verified != 3 {
		t.Fatalf("Verify after the migration = %+v, %v, want the three rows intact", rep, err)
	}
	for _, table := range []string{"media_refs", "revoke_tombstones"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", table); n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	for _, index := range []string{"media_refs_expiry", "media_refs_added", "revoke_tombstones_expiry", "messages_media_expiring"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = ?", index); n != 1 {
			t.Errorf("index %s missing", index)
		}
	}
	if got := scalar[string](t, s, "SELECT group_concat(id || ':' || coalesce(media_type, '-'), ',') FROM (SELECT * FROM messages ORDER BY id)"); got != "E1:image/png,M1:video/mp4,R1:-,R2:-" {
		t.Fatalf("media types after the migration = %q, want them cleared on the revoked messages only", got)
	}
	if got := scalar[int](t, s, "SELECT max(change_seq) FROM messages"); got != changes {
		t.Fatalf("the migration moved the newest change number from %d to %d", changes, got)
	}
	if got := scalar[string](t, s, "SELECT group_concat(id || allow_media, ',') FROM clients"); got != "aaaaaaaa0" {
		t.Fatalf("clients after the migration = %q, want allow_media off", got)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM audit WHERE message_hmac IS NULL AND bytes IS NULL"); n != 3 {
		t.Fatalf("%d audit rows without a message HMAC and a byte count, want all three", n)
	}
	m1 := scalar[int64](t, s, "SELECT seq FROM messages WHERE id = 'M1'")
	refInsert := func(seq int64, ref, kind, variant string, declared string) string {
		return "INSERT INTO media_refs (seq, ref, kind, variant, declared, expires_at, added_at) VALUES (" + strconv.FormatInt(seq, 10) + ", " + ref + ", '" + kind + "', " + variant + ", " + declared + ", 2, 1)"
	}
	if err := execAll(t, s,
		refInsert(m1, "'"+strings.Repeat("~", maxMediaRef)+"'", "video", "'video_note'", "2147483648"),
		"INSERT INTO revoke_tombstones (chat_jid, id, sender_jid, expires_at) VALUES ('"+groupJID+"', 'X1', '"+alice+"', 1)",
		clientRow("bbbbbbbb", "Media", 0, 1, 2),
		"UPDATE clients SET allow_media = 1 WHERE id = 'bbbbbbbb'",
	); err != nil {
		t.Fatalf("valid rows refused: %v", err)
	}
	if got := scalar[string](t, s, "SELECT group_concat(id || allow_media, ',') FROM (SELECT * FROM clients ORDER BY id)"); got != "aaaaaaaa0,bbbbbbbb1" {
		t.Fatalf("clients = %q, want a new client off until it is turned on", got)
	}
	r1 := scalar[int64](t, s, "SELECT seq FROM messages WHERE id = 'R1'")
	auditRow := func(column, value string) string {
		return "INSERT INTO audit (ts, client_id, action, ok, reason, key_id, row_hmac, " + column + ") VALUES (1, 'aaaaaaaa', 'read', 1, 'ok', 'k1', " + hash32 + ", " + value + ")"
	}
	for name, tt := range map[string]struct{ stmt, want string }{
		"a reference of an unknown message":  {refInsert(999, "'sealed'", "image", "NULL", "1"), "FOREIGN KEY constraint failed"},
		"a second reference for a message":   {refInsert(m1, "'sealed'", "image", "NULL", "1"), "UNIQUE constraint failed: media_refs.seq"},
		"an empty reference":                 {refInsert(r1, "''", "image", "NULL", "1"), "CHECK constraint failed"},
		"an overlong reference":              {refInsert(r1, "'"+strings.Repeat("~", maxMediaRef+1)+"'", "image", "NULL", "1"), "CHECK constraint failed"},
		"a reference with a space":           {refInsert(r1, "'sealed reference'", "image", "NULL", "1"), "CHECK constraint failed"},
		"a reference beyond ASCII":           {refInsert(r1, "'sealedé'", "image", "NULL", "1"), "CHECK constraint failed"},
		"a reference without a kind":         {refInsert(r1, "'sealed'", "", "NULL", "1"), "CHECK constraint failed"},
		"an unknown kind":                    {refInsert(r1, "'sealed'", "contact", "NULL", "1"), "CHECK constraint failed"},
		"a variant of another kind":          {refInsert(r1, "'sealed'", "image", "'gif'", "1"), "CHECK constraint failed"},
		"a voice note that is not audio":     {refInsert(r1, "'sealed'", "video", "'voice_note'", "1"), "CHECK constraint failed"},
		"an animation that is not a sticker": {refInsert(r1, "'sealed'", "document", "'animated'", "1"), "CHECK constraint failed"},
		"an unknown variant":                 {refInsert(r1, "'sealed'", "sticker", "'panorama'", "1"), "CHECK constraint failed"},
		"a negative declared size":           {refInsert(r1, "'sealed'", "image", "NULL", "-1"), "CHECK constraint failed"},
		"a declared size beyond 2 GiB":       {refInsert(r1, "'sealed'", "image", "NULL", "2147483649"), "CHECK constraint failed"},
		"a reference without an expiry":      {"INSERT INTO media_refs (seq, ref, kind, declared, added_at) VALUES (" + strconv.FormatInt(r1, 10) + ", 'sealed', 'image', 1, 1)", "NOT NULL constraint failed"},
		"a second tombstone for a message":   {"INSERT INTO revoke_tombstones (chat_jid, id, sender_jid, expires_at) VALUES ('" + groupJID + "', 'X1', '" + alice + "', 2)", "UNIQUE constraint failed"},
		"a tombstone without an expiry":      {"INSERT INTO revoke_tombstones (chat_jid, id, sender_jid) VALUES ('" + groupJID + "', 'X2', '" + alice + "')", "NOT NULL constraint failed"},
		"allow_media out of range":           {"UPDATE clients SET allow_media = 2 WHERE id = 'aaaaaaaa'", "CHECK constraint failed"},
		"a short message HMAC":               {auditRow("message_hmac", "x'00'"), "CHECK constraint failed"},
		"an overlong message HMAC":           {auditRow("message_hmac", "zeroblob(33)"), "CHECK constraint failed"},
		"a message HMAC as text":             {auditRow("message_hmac", "'M1'"), "cannot store TEXT value in BLOB column"},
		"a negative byte count":              {auditRow("bytes", "-1"), "CHECK constraint failed"},
		"an update of a byte count":          {"UPDATE audit SET bytes = 1 WHERE id = 1", "audit rows are append-only"},
	} {
		if err := execAll(t, s, tt.stmt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", name, err, tt.want)
		}
	}
	if err := execAll(t, s, auditRow("message_hmac", "zeroblob(16)"), auditRow("message_hmac", hash32), auditRow("bytes", "0")); err != nil {
		t.Fatalf("valid audit rows refused: %v", err)
	}
	before := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'M1'")
	if err := execAll(t, s, "UPDATE messages SET media_type = NULL WHERE id = 'M1'"); err != nil {
		t.Fatalf("clearing a media type: %v", err)
	}
	if after := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'M1'"); after != before {
		t.Fatalf("clearing a media type moved the change number from %d to %d", before, after)
	}
	if err := execAll(t, s, "DELETE FROM messages WHERE id = 'M1'"); err != nil {
		t.Fatalf("deleting a message: %v", err)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM media_refs"); n != 0 {
		t.Fatal("a message's reference outlived the message")
	}
	for query, want := range map[string]string{
		selectTombstone: "revoke_tombstones USING PRIMARY KEY",
		expireMediaRef:  "media_refs USING INTEGER PRIMARY KEY",
		deleteMediaRef:  "media_refs USING INTEGER PRIMARY KEY",
		deleteTombstone: "revoke_tombstones USING PRIMARY KEY",
	} {
		args := []any{groupJID, "X1", alice, 1}
		switch query {
		case expireMediaRef:
			args = []any{1, groupJID, "X1", alice}
		case deleteMediaRef:
			args = []any{1}
		case deleteTombstone:
			args = args[:3]
		}
		if plan := queryPlan(t, s, query, args...); !strings.Contains(plan, want) || strings.Contains(plan, "SCAN messages") || strings.Contains(plan, "SCAN media_refs") || strings.Contains(plan, "SCAN revoke_tombstones") {
			t.Errorf("%q plans as %q, want a search of %s", query, plan, want)
		}
	}
}
