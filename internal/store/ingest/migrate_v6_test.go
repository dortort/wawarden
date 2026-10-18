package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

func seedVersion5(t *testing.T, opts Options) int {
	t.Helper()
	d, err := db.Open(t.Context(), db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout, RewriteTimeout: opts.RewriteTimeout})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := d.Migrate(t.Context(), []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5}); err != nil {
		t.Fatalf("Migrate to schema version 5: %v", err)
	}
	var changes int
	if err := d.Write(t.Context(), "test.seed", func(ctx context.Context, q db.Querier) error {
		for _, stmt := range []string{
			"INSERT INTO chats (jid, kind) VALUES ('" + alice + "', 1), ('" + groupJID + "', 3)",
			"INSERT INTO messages (chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display, media_type, revoked, expires_at) VALUES " +
				"('" + alice + "', 'T1', '" + alice + "', 0, 'live', 1, 'text', 'synthetic', 'synthetic', NULL, 0, NULL), " +
				"('" + groupJID + "', 'M1', '" + alice + "', 0, 'history', 2, 'media', NULL, NULL, 'image/jpeg', 0, NULL)",
			"INSERT INTO media_refs (seq, ref, kind, declared, expires_at, added_at) SELECT seq, 'sealed', 'image', 1, 3, 2 FROM messages WHERE id = 'M1'",
		} {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return q.QueryRowContext(ctx, "SELECT max(change_seq) FROM messages").Scan(&changes)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return changes
}

func TestMigratingToVersion6MarksNoAttachmentGone(t *testing.T) {
	opts := testOptions(t)
	changes := seedVersion5(t, opts)
	s := openWith(t, opts)
	if s.SchemaVersion() != 6 || LatestSchemaVersion() != 6 {
		t.Fatalf("schema version %d, latest %d, want 6", s.SchemaVersion(), LatestSchemaVersion())
	}
	if got := scalar[string](t, s, "SELECT group_concat(id || ':' || media_gone, ',') FROM (SELECT * FROM messages ORDER BY id)"); got != "M1:0,T1:0" {
		t.Fatalf("media_gone after the migration = %q, want every message unmarked", got)
	}
	if got := scalar[int](t, s, "SELECT max(change_seq) FROM messages"); got != changes {
		t.Fatalf("the migration moved the newest change number from %d to %d", changes, got)
	}
	if got := scalar[string](t, s, "SELECT ref FROM media_refs"); got != "sealed" {
		t.Fatalf("the stored reference after the migration = %q", got)
	}
	for name, tt := range map[string]struct{ stmt, want string }{
		"media_gone out of range": {"UPDATE messages SET media_gone = 2 WHERE id = 'M1'", "CHECK constraint failed"},
		"no media_gone":           {"UPDATE messages SET media_gone = NULL WHERE id = 'M1'", "NOT NULL constraint failed"},
	} {
		if err := execAll(t, s, tt.stmt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", name, err, tt.want)
		}
	}
	before := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'M1'")
	if err := execAll(t, s, "UPDATE messages SET media_gone = 1 WHERE id = 'M1'"); err != nil {
		t.Fatalf("marking an attachment gone: %v", err)
	}
	if after := scalar[int](t, s, "SELECT change_seq FROM messages WHERE id = 'M1'"); after != before {
		t.Fatalf("marking an attachment gone moved the change number from %d to %d", before, after)
	}
	if plan := queryPlan(t, s, markMediaGone, groupJID, "M1", alice); !strings.Contains(plan, "SEARCH messages USING INDEX") || strings.Contains(plan, "SCAN") {
		t.Errorf("%q plans as %q, want a search of the message key", markMediaGone, plan)
	}
}
