package ingest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

func testMaster(t *testing.T) *keys.Master {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(40 + i)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, r := keys.LoadFile(path)
	if r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	return m
}

func copyArchive(t *testing.T, s *Store) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy")
	err := s.Backup(t.Context(), filepath.Join(t.TempDir(), "staging"), func(_ string, _ int64, r io.Reader) error {
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: a file in the test's own directory
		if err != nil {
			return err
		}
		_, err = io.Copy(out, r)
		return errors.Join(err, out.Close())
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return path
}

func queryPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	var details []string
	read(t, s, func(r *Reader) error {
		rows, err := r.q.QueryContext(r.ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			details = append(details, detail)
		}
		return rows.Err()
	})
	return strings.Join(details, "; ")
}

func seedVersion3(t *testing.T, opts Options) {
	t.Helper()
	d, err := db.Open(t.Context(), db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout, RewriteTimeout: opts.RewriteTimeout})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := d.Migrate(t.Context(), []string{schemaV1, schemaV2, schemaV3}); err != nil {
		t.Fatalf("Migrate to schema version 3: %v", err)
	}
	if err := d.Write(t.Context(), "test.seed", func(ctx context.Context, q db.Querier) error {
		for _, stmt := range []string{
			"INSERT INTO chats (jid, kind) VALUES ('" + alice + "', 1), ('" + groupJID + "', 3)",
			"INSERT INTO messages (chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display) VALUES " +
				"('" + alice + "', 'M1', '" + alice + "', 0, 'live', 1, 'text', 'synthetic peer', 'synthetic peer'), " +
				"('" + alice + "', 'M2', '" + ownerJID + "', 1, 'history', 2, 'text', 'synthetic own', 'synthetic own')",
			"INSERT INTO lid_map (lid, pn, source, learned_ts) VALUES ('" + aliceLID + "', '" + bob + "', 'sender_alt', 1), " +
				"('" + bobLID + "', '" + carol + "', 'recipient_alt', 2), ('" + carolLID + "', '15550100004@s.whatsapp.net', 'history', 3)",
		} {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	audit := admin.NewAudit(d, opts.Master, io.Discard)
	for i := range 4 {
		if err := audit.Record(t.Context(), admin.Event{At: epoch.Add(time.Duration(i) * time.Second), Client: "aaaaaaaa", Action: "messages.read", OK: true, Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMigratingToVersion4KeepsMappingsMessagesAndTheAuditChain(t *testing.T) {
	opts := testOptions(t)
	opts.Master = testMaster(t)
	seedVersion3(t, opts)
	s := openWith(t, opts)
	if s.SchemaVersion() != 4 {
		t.Fatalf("schema version %d, want 4", s.SchemaVersion())
	}
	rep, err := admin.Verify(t.Context(), copyArchive(t, s), opts.Master, nil)
	if err != nil || !rep.OK() || rep.Rows != 4 || rep.Verified != 4 {
		t.Fatalf("Verify after the migration = %+v, %v, want the four rows intact", rep, err)
	}
	const mappings = "SELECT group_concat(lid || ' ' || pn || ' ' || source || ' ' || learned_ts, ',') FROM (SELECT * FROM lid_map ORDER BY learned_ts)"
	want := aliceLID + " " + bob + " sender_alt 1," + bobLID + " " + carol + " recipient_alt 2," + carolLID + " 15550100004@s.whatsapp.net history 3"
	if got := scalar[string](t, s, mappings); got != want {
		t.Fatalf("lid_map after the migration = %q, want %q", got, want)
	}
	if got := scalar[string](t, s, "SELECT group_concat(id || from_me || coalesce(sent_by, '-'), ',') FROM (SELECT * FROM messages ORDER BY seq)"); got != "M10-,M21-" {
		t.Fatalf("messages after the migration = %q", got)
	}
	for _, table := range []string{"idempotency", "backfills", "audit_anchor"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", table); n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	for _, index := range []string{"messages_inbound", "idempotency_window", "idempotency_client_window", "idempotency_expiry", "backfills_requested"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = ?", index); n != 1 {
			t.Errorf("index %s missing", index)
		}
	}
	for _, trigger := range []string{"audit_no_update", "audit_no_delete", "audit_no_replace", "audit_anchor_forward", "audit_anchor_keep"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE type = 'trigger' AND name = ?", trigger); n != 1 {
			t.Errorf("trigger %s missing", trigger)
		}
	}
	if plan := queryPlan(t, s, "SELECT EXISTS (SELECT 1 FROM messages WHERE chat_jid = ?1 AND from_me = 0)", alice); !strings.Contains(plan, "COVERING INDEX messages_inbound") {
		t.Errorf("the inbound probe plans as %q, want the covering index messages_inbound", plan)
	}
	if res := learn(t, s, "100000000000004@lid", "15550100005@s.whatsapp.net", MappingSelf); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID with the self source = %+v", res)
	}
	if got := scalar[string](t, s, "SELECT source FROM lid_map WHERE lid = '100000000000004@lid'"); got != "self" {
		t.Fatalf("the self mapping is stored with source %q", got)
	}
	before := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'M2'")
	if err := execAll(t, s, "UPDATE messages SET sent_by = 'aaaaaaaa' WHERE id = 'M2'"); err != nil {
		t.Fatalf("setting sent_by: %v", err)
	}
	if after := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'M2'"); after != before {
		t.Fatalf("setting sent_by moved the change number from %d to %d", before, after)
	}
	for name, tt := range map[string]struct{ stmt, want string }{
		"a mapping of a known number":     {"INSERT INTO lid_map (lid, pn, source, learned_ts) VALUES ('100000000000009@lid', '" + bob + "', 'history', 4)", "UNIQUE constraint failed: lid_map.pn"},
		"a mapping of a known lid":        {"INSERT INTO lid_map (lid, pn, source, learned_ts) VALUES ('" + aliceLID + "', '15550100008@s.whatsapp.net', 'history', 4)", "UNIQUE constraint failed: lid_map.lid"},
		"an unknown mapping source":       {"INSERT INTO lid_map (lid, pn, source, learned_ts) VALUES ('100000000000009@lid', '15550100008@s.whatsapp.net', 'guess', 4)", "CHECK constraint failed"},
		"a short sent_by":                 {"UPDATE messages SET sent_by = 'abc' WHERE id = 'M1'", "CHECK constraint failed"},
		"a short idempotency client":      {"INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at) VALUES ('aaaa', 'key-0001', 'pending', " + hash32 + ", 'X', 1, 1, 1, 2)", "CHECK constraint failed"},
		"a short idempotency key":         {"INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at) VALUES ('aaaaaaaa', 'key', 'pending', " + hash32 + ", 'X', 1, 1, 1, 2)", "CHECK constraint failed"},
		"an unknown idempotency state":    {"INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at) VALUES ('aaaaaaaa', 'key-0001', 'sent', " + hash32 + ", 'X', 1, 1, 1, 2)", "CHECK constraint failed"},
		"a short fingerprint":             {"INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at) VALUES ('aaaaaaaa', 'key-0001', 'pending', x'00', 'X', 1, 1, 1, 2)", "CHECK constraint failed"},
		"a counted flag out of range":     {"INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at) VALUES ('aaaaaaaa', 'key-0001', 'pending', " + hash32 + ", 'X', 1, 1, 2, 2)", "CHECK constraint failed"},
		"a backfill of no messages":       {"INSERT INTO backfills (request_id, chat_jid, count, requested_at) VALUES ('R1', '" + alice + "', 0, 1)", "CHECK constraint failed"},
		"a backfill of too many messages": {"INSERT INTO backfills (request_id, chat_jid, count, requested_at) VALUES ('R1', '" + alice + "', 51, 1)", "CHECK constraint failed"},
		"a second audit anchor":           {"INSERT INTO audit_anchor (id, through, ts, key_id, row_hmac, pruned_at, mac_key_id, mac) VALUES (2, 1, 1, 'k1', " + hash32 + ", 1, 'k1', " + hash32 + ")", "CHECK constraint failed"},
		"an audit delete without anchor":  {"DELETE FROM audit WHERE id = 1", "audit rows are append-only"},
	} {
		if err := execAll(t, s, tt.stmt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", name, err, tt.want)
		}
	}
	if err := execAll(t, s, "INSERT INTO audit_anchor (id, through, ts, key_id, row_hmac, pruned_at, mac_key_id, mac) VALUES (1, 2, 1, 'k1', "+hash32+", 1, 'k1', "+hash32+")",
		"DELETE FROM audit WHERE id <= 2"); err != nil {
		t.Fatalf("deleting the rows the anchor covers: %v", err)
	}
	for name, tt := range map[string]struct{ stmt, want string }{
		"a delete beyond the anchor":     {"DELETE FROM audit WHERE id = 3", "audit rows are append-only"},
		"the anchor moved back":          {"UPDATE audit_anchor SET through = 1", "the audit anchor only moves forward"},
		"the anchor kept in place":       {"UPDATE audit_anchor SET through = 2", "the audit anchor only moves forward"},
		"the anchor removed":             {"DELETE FROM audit_anchor", "the audit anchor is never removed"},
		"an update of an audit row":      {"UPDATE audit SET ok = 0 WHERE id = 3", "audit rows are append-only"},
		"the newest row under an anchor": {"UPDATE audit_anchor SET through = 4; DELETE FROM audit WHERE id <= 4", "audit rows are append-only"},
	} {
		if err := execAll(t, s, tt.stmt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", name, err, tt.want)
		}
	}
	if got := scalar[string](t, s, "SELECT group_concat(id, ',') FROM (SELECT id FROM audit ORDER BY id)"); got != "3,4" {
		t.Fatalf("audit rows %q, want 3 and 4 after the covered rows went", got)
	}
	if err := execAll(t, s, "UPDATE audit_anchor SET through = 4", "DELETE FROM audit WHERE id = 3"); err != nil {
		t.Fatalf("deleting a covered row below the newest: %v", err)
	}
	if got := scalar[string](t, s, "SELECT group_concat(id, ',') FROM audit"); got != "4" {
		t.Fatalf("audit rows %q, want the newest kept", got)
	}
}
