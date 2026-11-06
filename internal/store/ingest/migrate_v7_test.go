package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

func TestMigratingToVersion7AddsWebhooksAndKeepsClientsWithout(t *testing.T) {
	opts := testOptions(t)
	d, err := db.Open(t.Context(), db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout, RewriteTimeout: opts.RewriteTimeout})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := d.Migrate(t.Context(), []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6}); err != nil {
		t.Fatalf("Migrate to schema version 6: %v", err)
	}
	if err := d.Write(t.Context(), "test.seed", func(ctx context.Context, q db.Querier) error {
		_, err := q.ExecContext(ctx, "INSERT INTO clients (id, name, token_hash, read_all, created_at, expires_at) VALUES ('aaaaaaaa', 'agent', zeroblob(32), 1, 1, 2)")
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s := openWith(t, opts)
	if s.SchemaVersion() != 7 {
		t.Fatalf("schema version %d, want 7", s.SchemaVersion())
	}
	if got := scalar[int](t, s, "SELECT allow_webhooks FROM clients WHERE id = 'aaaaaaaa'"); got != 0 {
		t.Fatalf("an existing client allows webhooks after the migration: %d", got)
	}
	insert := "INSERT INTO webhooks (id, client_id, url, method, content_type, headers, payload, filter, secret, created_at, high_seq) VALUES "
	ok := "('wh_0123456789abcdef', 'aaaaaaaa', 'https://h.example.com', 'POST', 'application/json', '{}', NULL, '{}', zeroblob(32), 1, 0)"
	if err := execAll(t, s, insert+ok); err != nil {
		t.Fatalf("insert a webhook: %v", err)
	}
	for name, tt := range map[string]struct{ row, want string }{
		"a replaced row":    {ok, "a webhook row is never replaced"},
		"another method":    {"('wh_1123456789abcdef', 'aaaaaaaa', 'https://h.example.com', 'GET', 'application/json', '{}', NULL, '{}', zeroblob(32), 1, 0)", "CHECK constraint failed"},
		"headers not JSON":  {"('wh_2123456789abcdef', 'aaaaaaaa', 'https://h.example.com', 'POST', 'application/json', '[', NULL, '{}', zeroblob(32), 1, 0)", "CHECK constraint failed"},
		"a short secret":    {"('wh_3123456789abcdef', 'aaaaaaaa', 'https://h.example.com', 'POST', 'application/json', '{}', NULL, '{}', zeroblob(31), 1, 0)", "CHECK constraint failed"},
		"an unknown client": {"('wh_4123456789abcdef', 'zzzzzzzz', 'https://h.example.com', 'POST', 'application/json', '{}', NULL, '{}', zeroblob(32), 1, 0)", "FOREIGN KEY constraint failed"},
		"an empty payload":  {"('wh_5123456789abcdef', 'aaaaaaaa', 'https://h.example.com', 'POST', 'text/plain', '{}', '', '{}', zeroblob(32), 1, 0)", "CHECK constraint failed"},
	} {
		if err := execAll(t, s, insert+tt.row); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", name, err, tt.want)
		}
	}
}
