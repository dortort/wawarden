package db

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestMigrateRunsWithoutTheWriteDeadline(t *testing.T) {
	opts, logs := testOptions(t)
	d := mustOpen(t, opts)
	d.writeTimeout, d.rewriteTimeout = time.Nanosecond, time.Nanosecond
	step := "CREATE TABLE a(x INTEGER) STRICT; WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000) INSERT INTO a SELECT i FROM n;"
	if err := d.Write(t.Context(), "test.write", func(ctx context.Context, q Querier) error {
		_, err := q.ExecContext(ctx, step)
		return err
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a write under a 1 ns deadline = %v, want its deadline exceeded", err)
	}
	if v, err := d.Migrate(t.Context(), []string{step}); err != nil || v != 1 {
		t.Fatalf("Migrate under a 1 ns write and rewrite deadline = %d, %v", v, err)
	}
	if n := count(t, d, "SELECT count(*) FROM a"); n != 20000 {
		t.Fatalf("%d rows, want the step's 20000", n)
	}
	if events := logs.events("db_deadline"); len(events) != 1 || events[0]["operation"] != "test.write" {
		t.Fatalf("db_deadline events = %v, want the write's only", events)
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
