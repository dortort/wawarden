//go:build !race

package ingest

import (
	"testing"
	"time"
)

func TestMigratingALargeArchive(t *testing.T) {
	opts := testOptions(t)
	openAtVersion(t, opts, []string{schemaV1, schemaV2},
		"WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 999) "+
			"INSERT INTO chats (jid, kind, last_ts) SELECT '1555' || (1000000 + i) || '@s.whatsapp.net', 1, 500000 FROM n",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 500000) "+
			"INSERT INTO messages (chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display) "+
			"SELECT '1555' || (1000000 + i % 1000) || '@s.whatsapp.net', 'M' || i, '1555' || (1000000 + i % 1000) || '@s.whatsapp.net', 0, 'live', i, 'text', 'synthetic ' || i, 'synthetic ' || i FROM n")
	opts.WriteTimeout = time.Millisecond
	start := time.Now()
	s := openWith(t, opts)
	t.Logf("migrated 500,000 messages in %v", time.Since(start))
	if s.SchemaVersion() != 4 {
		t.Fatalf("schema version %d, want 4", s.SchemaVersion())
	}
	requireNumbered(t, s, 500000)
	if n := scalar[int](t, s, "SELECT count(DISTINCT ref) FROM chats WHERE length(ref) = 32"); n != 1000 {
		t.Fatalf("%d chats with a reference, want 1000", n)
	}
	for _, index := range []string{"messages_chat_ts", "messages_change", "messages_change_chat", "chats_ref", "chats_last"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = ?", index); n != 1 {
			t.Errorf("index %s missing", index)
		}
	}
}
