// Package db is the only holder of a database handle: it opens a SQLite file of the data directory with verified pragmas under an exclusive lock, keeps its one connection alive, and runs every call under a deadline.
package db

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

type Name uint8

const (
	Archive Name = 1
	Session Name = 2
)

func (n Name) label() string {
	switch n {
	case Archive:
		return "archive"
	case Session:
		return "session"
	}
	return ""
}

func (n Name) file() string { return n.label() + ".db" }

type Profile string

const (
	Local Profile = "local"
	NFS   Profile = "nfs"
)

const (
	defaultBusyTimeout    = 5 * time.Second
	defaultAcquireFor     = 5 * time.Minute
	defaultRetryBase      = 500 * time.Millisecond
	maxRetryWait          = 15 * time.Second
	defaultReadTimeout    = 2 * time.Second
	defaultWriteTimeout   = 10 * time.Second
	defaultRewriteTimeout = 5 * time.Minute
	defaultCloseWait      = time.Second
	maxGoroutineDump      = 32 << 10
	sqliteBusy            = 5
)

type Options struct {
	DataDir        string
	UID            int
	Profile        Profile
	Logger         *slog.Logger
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	RewriteTimeout time.Duration

	owner       func(fs.FileInfo) (int, bool)
	statfs      func(string, *syscall.Statfs_t) error
	busyTimeout time.Duration
	acquireFor  time.Duration
	retryBase   time.Duration
	pragmas     []string
	disposable  bool
	ofdEnabled  func() bool
}

type Refusal struct {
	Reason string
	detail string
}

func (r *Refusal) Error() string { return "db: " + r.detail }

type DB struct {
	name           Name
	path           string
	dir            string
	sql            *sql.DB
	connector      *connector
	logger         *slog.Logger
	profile        Profile
	statfs         func(string, *syscall.Statfs_t) error
	readTimeout    time.Duration
	writeTimeout   time.Duration
	rewriteTimeout time.Duration
	closeWait      time.Duration
	closed         atomic.Bool
	backupPages    int32
	newBackup      func(*keptConn, string) (stepper, error)
	stepped        func()
}

type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func Open(ctx context.Context, name Name, opts Options) (*DB, error) {
	if name.label() == "" {
		return nil, errors.New("db: unknown database")
	}
	if opts.Logger == nil {
		return nil, errors.New("db: Open needs a logger")
	}
	defaults(&opts)
	if opts.Profile != Local && opts.Profile != NFS {
		return nil, fmt.Errorf("db: unknown storage profile %q", string(opts.Profile))
	}
	dir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, &Refusal{Reason: "storage_filesystem_unknown", detail: "the data directory's absolute path cannot be determined"}
	}
	if r := checkFilesystem(dir, opts.Profile, opts.statfs); r != nil {
		return nil, r
	}
	path := filepath.Join(dir, name.file())
	if r := prepareFiles(name, path, opts.UID, opts.owner); r != nil {
		return nil, r
	}
	if err := selectLocking(opts.Profile); err != nil {
		return nil, err
	}
	d := &DB{
		name:           name,
		path:           path,
		dir:            dir,
		logger:         opts.Logger,
		profile:        opts.Profile,
		statfs:         opts.statfs,
		readTimeout:    opts.ReadTimeout,
		writeTimeout:   opts.WriteTimeout,
		rewriteTimeout: opts.RewriteTimeout,
		closeWait:      defaultCloseWait,
		backupPages:    backupPagesPerStep,
		newBackup:      startBackup,
	}
	d.connector = newConnector(fileURI(path, opts.pragmas), verify(opts.busyTimeout), !opts.disposable, d.lost)
	d.sql = sql.OpenDB(d.connector)
	d.sql.SetMaxOpenConns(1)
	d.sql.SetMaxIdleConns(1)
	d.sql.SetConnMaxLifetime(0)
	d.sql.SetConnMaxIdleTime(0)
	if err := d.acquire(ctx, opts.acquireFor, opts.retryBase); err != nil {
		return nil, errors.Join(err, d.sql.Close())
	}
	if opts.Profile == Local && runtime.GOOS == "linux" && !opts.ofdEnabled() {
		return nil, errors.Join(&Refusal{Reason: "storage_ofd_unavailable", detail: "storage profile local needs open-file-description locks, and the kernel or the data directory's filesystem refused them"}, d.sql.Close())
	}
	return d, nil
}

func defaults(o *Options) {
	if o.owner == nil {
		o.owner = fileOwner
	}
	if o.statfs == nil {
		o.statfs = syscall.Statfs
	}
	if o.busyTimeout == 0 {
		o.busyTimeout = defaultBusyTimeout
	}
	if o.acquireFor == 0 {
		o.acquireFor = defaultAcquireFor
	}
	if o.retryBase == 0 {
		o.retryBase = defaultRetryBase
	}
	if o.ReadTimeout == 0 {
		o.ReadTimeout = defaultReadTimeout
	}
	if o.WriteTimeout == 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	if o.RewriteTimeout == 0 {
		o.RewriteTimeout = defaultRewriteTimeout
	}
	if o.pragmas == nil {
		o.pragmas = requiredPragmas(o.busyTimeout)
	}
	if o.ofdEnabled == nil {
		o.ofdEnabled = sqlite.OFDLockingEnabled
	}
}

func requiredPragmas(busy time.Duration) []string {
	return []string{
		"foreign_keys(1)",
		"journal_mode(TRUNCATE)",
		"synchronous(FULL)",
		"locking_mode(EXCLUSIVE)",
		"temp_store(MEMORY)",
		"secure_delete(ON)",
		"busy_timeout(" + strconv.FormatInt(busy.Milliseconds(), 10) + ")",
	}
}

func fileURI(path string, pragmas []string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}).String()
}

func selectLocking(profile Profile) error {
	_, err := sqlite.OFDLocking(profile == Local)
	if err == nil || errors.Is(err, sqlite.ErrOFDLockingUnavailable) {
		return nil
	}
	return fmt.Errorf("db: storage profile %s cannot select its lock kind: another database of this process already locks with the other kind", profile)
}

type pragmaCheck struct {
	query string
	want  string
}

func verify(busy time.Duration) func(sqlite.ExecQuerierContext) error {
	checks := []pragmaCheck{
		{"PRAGMA foreign_keys", "1"},
		{"PRAGMA journal_mode", "truncate"},
		{"PRAGMA synchronous", "2"},
		{"PRAGMA locking_mode", "exclusive"},
		{"PRAGMA temp_store", "2"},
		{"PRAGMA secure_delete", "1"},
		{"PRAGMA busy_timeout", strconv.FormatInt(busy.Milliseconds(), 10)},
	}
	return func(c sqlite.ExecQuerierContext) error {
		ctx := context.Background()
		for _, check := range checks {
			got, err := pragmaValue(ctx, c, check.query)
			if err != nil {
				return fmt.Errorf("read %s: %w", check.query, err)
			}
			if got != check.want {
				return &pragmaMismatch{query: check.query, got: got, want: check.want}
			}
		}
		if _, err := c.ExecContext(ctx, "BEGIN EXCLUSIVE", nil); err != nil {
			return fmt.Errorf("take the exclusive lock: %w", err)
		}
		if _, err := c.ExecContext(ctx, "COMMIT", nil); err != nil {
			return fmt.Errorf("take the exclusive lock: %w", err)
		}
		return nil
	}
}

type pragmaMismatch struct{ query, got, want string }

func (e *pragmaMismatch) Error() string {
	return fmt.Sprintf("%s reads back %q, want %q", e.query, e.got, e.want)
}

func pragmaValue(ctx context.Context, c sqlite.ExecQuerierContext, query string) (string, error) {
	rows, err := c.QueryContext(ctx, query, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	values := make([]driver.Value, len(rows.Columns()))
	if len(values) != 1 {
		return "", errors.New("not exactly one column")
	}
	if err := rows.Next(values); err != nil {
		return "", err
	}
	switch v := values[0].(type) {
	case int64:
		return strconv.FormatInt(v, 10), nil
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	}
	return "", fmt.Errorf("unexpected value of type %T", values[0])
}

func (d *DB) acquire(ctx context.Context, within, base time.Duration) error {
	actx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	wait := base
	warned := false
	for {
		err := d.sql.PingContext(actx)
		switch {
		case err == nil:
			return nil
		case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("db: %s is still locked by another process after %v", d.name.file(), within)
		case !busy(err):
			return fmt.Errorf("db: open %s: %w", d.name.file(), err)
		case !warned:
			warned = true
			d.logger.Warn("database locked by another process; retrying",
				slog.String("event", "db_lock_wait"), slog.String("database", d.name.label()),
				slog.String("within", within.String()))
		}
		timer := time.NewTimer(wait)
		select {
		case <-actx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return fmt.Errorf("db: open %s: %w", d.name.file(), ctx.Err())
			}
			return fmt.Errorf("db: %s is still locked by another process after %v", d.name.file(), within)
		case <-timer.C:
		}
		wait = min(2*wait, maxRetryWait)
	}
}

func busy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqliteBusy
}

func (d *DB) lost() {
	d.logger.Error("database connection lost: the service no longer holds the lock and refuses to reconnect",
		slog.String("event", "db_lost"), slog.String("database", d.name.label()))
}

func (d *DB) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	err := d.sql.Close()
	if !d.connector.awaitRelease(d.closeWait) {
		return errors.Join(err, errStillHeld)
	}
	return err
}

func (d *DB) Healthy() bool { return !d.closed.Load() && !d.connector.lostConnection() }

func (d *DB) RawHandle() *sql.DB { return d.sql }

func (d *DB) Profile() Profile { return d.profile }

func (d *DB) OFDLocking() bool { return sqlite.OFDLockingEnabled() }

func (d *DB) Read(ctx context.Context, op string, fn func(context.Context, Querier) error) error {
	return d.within(ctx, op, d.readTimeout, func(ctx context.Context) error {
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		return fn(ctx, tx)
	})
}

func (d *DB) Write(ctx context.Context, op string, fn func(context.Context, Querier) error) error {
	return d.write(ctx, op, d.writeTimeout, fn)
}

func (d *DB) Rewrite(ctx context.Context, op string, fn func(context.Context, Querier) error) error {
	return d.write(ctx, op, d.rewriteTimeout, fn)
}

func (d *DB) write(ctx context.Context, op string, timeout time.Duration, fn func(context.Context, Querier) error) error {
	return d.within(ctx, op, timeout, func(ctx context.Context) error {
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return errors.Join(err, rollback(tx))
		}
		return tx.Commit()
	})
}

func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}

func (d *DB) within(ctx context.Context, op string, timeout time.Duration, fn func(context.Context) error) error {
	if d.closed.Load() {
		return errClosed
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := fn(dctx)
	if err != nil && ctx.Err() == nil && errors.Is(dctx.Err(), context.DeadlineExceeded) {
		d.logger.Error("database call exceeded its deadline",
			slog.String("event", "db_deadline"), slog.String("database", d.name.label()),
			slog.String("operation", op), slog.Int64("timeout_ms", timeout.Milliseconds()),
			slog.String("goroutines", goroutineDump()))
	}
	return err
}

var (
	errClosed    = errors.New("db: the database is closed")
	errStillHeld = errors.New("db: the connection that holds the lock was still in use when the database was closed")
)

func goroutineDump() string {
	var b bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&b, 1); err != nil {
		return "unavailable"
	}
	dump := b.String()
	if len(dump) <= maxGoroutineDump {
		return dump
	}
	dump = dump[:maxGoroutineDump]
	if i := strings.LastIndexByte(dump, '\n'); i > 0 {
		dump = dump[:i+1]
	}
	return dump + "[truncated]\n"
}

func (d *DB) FreeBytes() (uint64, error) {
	var st syscall.Statfs_t
	if err := d.statfs(d.dir, &st); err != nil {
		return 0, fmt.Errorf("db: inspect the data directory's filesystem: %s", cause(err))
	}
	return availableBytes(&st), nil
}
