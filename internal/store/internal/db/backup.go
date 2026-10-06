package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const backupPagesPerStep = 256

var (
	errBackupConnChanged = errors.New("db: backup: the database connection changed between steps")
	errBackupPanicked    = errors.New("db: backup: a step panicked")
)

type stepper interface {
	Step(n int32) (bool, error)
	Finish() error
}

func startBackup(kc *keptConn, dst string) (stepper, error) {
	b, err := kc.NewBackup(dst)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (d *DB) Backup(ctx context.Context, staging string, write func(name string, size int64, r io.Reader) error) error {
	staging = filepath.Clean(staging)
	if err := create(staging); err != nil {
		return fmt.Errorf("db: backup: create the staging copy: %s", cause(err))
	}
	defer func() { _ = os.Remove(staging) }()
	if err := d.copyTo(ctx, fileURI(staging, []string{"journal_mode(MEMORY)"})); err != nil {
		return err
	}
	f, err := os.OpenFile(staging, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("db: backup: open the staging copy: %s", cause(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("db: backup: inspect the staging copy: %s", cause(err))
	}
	if err := write(d.name.file(), fi.Size(), io.LimitReader(f, fi.Size())); err != nil {
		return fmt.Errorf("db: backup: write the copy: %w", err)
	}
	return nil
}

func (d *DB) copyTo(ctx context.Context, dst string) error {
	var source *keptConn
	var b stepper
	finish := func() error {
		if b == nil {
			return nil
		}
		for {
			err := d.step(context.WithoutCancel(ctx), func(kc *keptConn) error {
				if kc != source {
					return errBackupConnChanged
				}
				return safely(b.Finish)
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		}
	}
	for more := true; more; {
		if err := ctx.Err(); err != nil {
			return errors.Join(fmt.Errorf("db: backup: %w", err), finish())
		}
		err := d.step(ctx, func(kc *keptConn) error {
			if b == nil {
				var nb stepper
				if err := safely(func() (err error) {
					nb, err = d.newBackup(kc, dst)
					return err
				}); err != nil {
					return err
				}
				source, b = kc, nb
			}
			if kc != source {
				return errBackupConnChanged
			}
			return safely(func() (err error) {
				more, err = b.Step(d.backupPages)
				return err
			})
		})
		if err != nil {
			return errors.Join(fmt.Errorf("db: backup: %w", err), finish())
		}
		if more && d.stepped != nil {
			d.stepped()
		}
	}
	if err := finish(); err != nil {
		return fmt.Errorf("db: backup: %w", err)
	}
	return nil
}

func (d *DB) step(ctx context.Context, fn func(*keptConn) error) error {
	return d.within(ctx, "db.backup", d.writeTimeout, func(ctx context.Context) error {
		conn, err := d.sql.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return conn.Raw(func(dc any) error {
			kc, ok := dc.(*keptConn)
			if !ok {
				return errBackupConnChanged
			}
			return fn(kc)
		})
	})
}

func safely(fn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errBackupPanicked
		}
	}()
	return fn()
}
