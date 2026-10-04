package db

import (
	"context"
	"fmt"
	"strconv"
)

func (d *DB) Migrate(ctx context.Context, steps []string) (int, error) {
	var version int
	if err := d.Read(ctx, "db.schema_version", func(ctx context.Context, q Querier) error {
		return q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	}); err != nil {
		return 0, fmt.Errorf("db: read the schema version of %s: %w", d.name.file(), err)
	}
	if version > len(steps) {
		return 0, &Refusal{Reason: d.name.label() + "_schema_newer", detail: fmt.Sprintf("%s has schema version %d, newer than the %d this build knows: it was written by a newer release", d.name.file(), version, len(steps))}
	}
	for next := version + 1; next <= len(steps); next++ {
		err := d.Write(ctx, "db.migrate", func(ctx context.Context, q Querier) error {
			if _, err := q.ExecContext(ctx, steps[next-1]); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(next)); err != nil {
				return err
			}
			var got int
			if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&got); err != nil {
				return err
			}
			if got != next {
				return fmt.Errorf("schema version reads back %d, want %d", got, next)
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("db: migrate %s to schema version %d: %w", d.name.file(), next, err)
		}
	}
	return len(steps), nil
}
