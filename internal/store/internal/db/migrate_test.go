package db

import (
	"context"
	"errors"
	"testing"
)

func schemaVersion(t *testing.T, d *DB) int {
	t.Helper()
	return count(t, d, "PRAGMA user_version")
}

func TestMigrateAppliesEachStepOnce(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	steps := []string{
		"CREATE TABLE a(x INTEGER) STRICT; INSERT INTO a VALUES (1);",
		"CREATE TABLE b(y TEXT) STRICT;",
	}
	if v, err := d.Migrate(t.Context(), steps[:1]); err != nil || v != 1 {
		t.Fatalf("Migrate to 1 = %d, %v", v, err)
	}
	if v, err := d.Migrate(t.Context(), steps); err != nil || v != 2 {
		t.Fatalf("Migrate to 2 = %d, %v", v, err)
	}
	if v, err := d.Migrate(t.Context(), steps); err != nil || v != 2 {
		t.Fatalf("Migrate again = %d, %v", v, err)
	}
	if got := schemaVersion(t, d); got != 2 {
		t.Fatalf("user_version = %d, want 2", got)
	}
	if n := count(t, d, "SELECT count(*) FROM a"); n != 1 {
		t.Fatalf("the first step ran %d times, want once", n)
	}
}

func TestMigrateRollsBackAFailedStep(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	steps := []string{"CREATE TABLE a(x INTEGER) STRICT;", "CREATE TABLE b(y TEXT) STRICT; SELECT no_such_function();"}
	if _, err := d.Migrate(t.Context(), steps); err == nil {
		t.Fatal("Migrate accepted a failing step")
	}
	if got := schemaVersion(t, d); got != 1 {
		t.Fatalf("user_version = %d after the second step failed, want 1", got)
	}
	if n := count(t, d, "SELECT count(*) FROM sqlite_schema WHERE name = 'b'"); n != 0 {
		t.Fatal("the failed step left its table behind")
	}
}

func TestMigrateRefusesANewerSchema(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	if err := d.Write(t.Context(), "test.version", func(ctx context.Context, q Querier) error {
		_, err := q.ExecContext(ctx, "PRAGMA user_version = 3")
		return err
	}); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	_, err := d.Migrate(t.Context(), []string{"CREATE TABLE a(x INTEGER);", "CREATE TABLE b(x INTEGER);"})
	if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "archive_schema_newer" {
		t.Fatalf("Migrate = %v, want an archive_schema_newer refusal", err)
	}
	if n := count(t, d, "SELECT count(*) FROM sqlite_schema"); n != 0 {
		t.Fatal("Migrate changed a database whose schema is newer than it knows")
	}
}
