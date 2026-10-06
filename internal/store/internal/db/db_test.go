package db

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	var o Options
	defaults(&o)
	for _, tt := range []struct {
		name      string
		got, want time.Duration
	}{
		{"read deadline", o.ReadTimeout, 2 * time.Second},
		{"write deadline", o.WriteTimeout, 10 * time.Second},
		{"rewrite deadline", o.RewriteTimeout, 5 * time.Minute},
		{"lock wait", o.acquireFor, 5 * time.Minute},
		{"busy timeout", o.busyTimeout, 5 * time.Second},
	} {
		if tt.got != tt.want {
			t.Errorf("default %s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	opts, _ := testOptions(t)
	if d := mustOpen(t, opts); d.readTimeout != 2*time.Second || d.writeTimeout != 10*time.Second || d.rewriteTimeout != 5*time.Minute {
		t.Errorf("an opened database has deadlines %v, %v and %v, want 2s, 10s and 5m", d.readTimeout, d.writeTimeout, d.rewriteTimeout)
	}
}

func TestOpenAppliesAndVerifiesThePragmas(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	want := map[string]string{
		"PRAGMA foreign_keys":  "1",
		"PRAGMA journal_mode":  "truncate",
		"PRAGMA synchronous":   "2",
		"PRAGMA locking_mode":  "exclusive",
		"PRAGMA temp_store":    "2",
		"PRAGMA secure_delete": "1",
		"PRAGMA busy_timeout":  "5000",
	}
	for query, value := range want {
		var got string
		if err := d.Read(t.Context(), "test.pragma", func(ctx context.Context, q Querier) error {
			return q.QueryRowContext(ctx, query).Scan(&got)
		}); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != value {
			t.Errorf("%s = %q, want %q", query, got, value)
		}
	}
	if !d.Healthy() || d.Profile() != Local {
		t.Fatalf("Healthy = %v, Profile = %q after Open", d.Healthy(), d.Profile())
	}
}

func TestOpenRefusesPragmasThatDoNotReadBack(t *testing.T) {
	for _, tt := range []struct{ replace, with string }{
		{"journal_mode(TRUNCATE)", "journal_mode(PERSIST)"},
		{"journal_mode(TRUNCATE)", "journal_mode(WAL)"},
		{"journal_mode(TRUNCATE)", "journal_mode(DELETE)"},
		{"secure_delete(ON)", "secure_delete(FAST)"},
		{"secure_delete(ON)", "secure_delete(OFF)"},
		{"synchronous(FULL)", "synchronous(NORMAL)"},
		{"foreign_keys(1)", "foreign_keys(0)"},
		{"locking_mode(EXCLUSIVE)", "locking_mode(NORMAL)"},
		{"temp_store(MEMORY)", "temp_store(FILE)"},
		{"busy_timeout(5000)", "busy_timeout(4999)"},
	} {
		t.Run(tt.with, func(t *testing.T) {
			opts, _ := testOptions(t)
			opts.pragmas = slices.Clone(requiredPragmas(defaultBusyTimeout))
			i := slices.Index(opts.pragmas, tt.replace)
			if i < 0 {
				t.Fatalf("%s is not a required pragma", tt.replace)
			}
			opts.pragmas[i] = tt.with
			d, err := Open(t.Context(), Archive, opts)
			if err == nil {
				_ = d.Close()
				t.Fatalf("Open accepted %s", tt.with)
			}
			if _, ok := errors.AsType[*pragmaMismatch](err); !ok {
				t.Fatalf("Open with %s = %v, want a pragma mismatch", tt.with, err)
			}
		})
	}
}

func TestOpenConvertsAWriteAheadLogDatabase(t *testing.T) {
	opts, _ := testOptions(t)
	wal := opts
	wal.pragmas = slices.Clone(requiredPragmas(defaultBusyTimeout))
	wal.pragmas[slices.Index(wal.pragmas, "journal_mode(TRUNCATE)")] = "journal_mode(WAL)"
	d, err := Open(t.Context(), Archive, wal)
	if err == nil {
		_ = d.Close()
		t.Fatal("Open accepted journal_mode(WAL)")
	}
	header := func() []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(opts.DataDir, "archive.db"))
		if err != nil || len(b) < 20 {
			t.Fatalf("read the database header: %v (%d bytes)", err, len(b))
		}
		return b[18:20]
	}
	if got := header(); got[0] != 2 || got[1] != 2 {
		t.Fatalf("file format bytes %v after a WAL open, want [2 2], or this test proves nothing", got)
	}
	d = mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	if got := header(); got[0] != 1 || got[1] != 1 {
		t.Fatalf("file format bytes %v, want [1 1]: a rollback journal", got)
	}
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	saved := syscall.Umask(0)
	defer syscall.Umask(saved)
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	if err := d.Write(t.Context(), "test.hold", func(ctx context.Context, q Querier) error {
		if _, err := q.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		for _, name := range []string{"archive.db", "archive.db-journal"} {
			fi, err := os.Lstat(filepath.Join(opts.DataDir, name))
			if err != nil {
				return err
			}
			if fi.Mode() != 0o600 {
				t.Errorf("%s has mode %v under umask 0, want 0600", name, fi.Mode())
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestOpenRefusesFilesItCannotTrust(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		owner  func(fs.FileInfo) (int, bool)
		reason string
	}{
		{name: "a symbolic link", reason: "archive_db_not_regular", setup: func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "elsewhere.db")
			writeFile(t, target, 0o600)
			symlink(t, target, filepath.Join(dir, "archive.db"))
		}},
		{name: "a directory", reason: "archive_db_not_regular", setup: func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "archive.db"), 0o700); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
		}},
		{name: "group-readable", reason: "archive_db_permissions", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "archive.db"), 0o640)
		}},
		{name: "world-writable", reason: "archive_db_permissions", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "archive.db"), 0o602)
		}},
		{name: "setuid", reason: "archive_db_permissions", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "archive.db"), 0o600|fs.ModeSetuid)
		}},
		{name: "another owner", reason: "archive_db_foreign_owner", owner: func(fs.FileInfo) (int, bool) { return os.Geteuid() + 1, true }},
		{name: "an unknown owner", reason: "archive_db_foreign_owner", owner: func(fs.FileInfo) (int, bool) { return 0, false }},
		{name: "a journal symbolic link", reason: "archive_journal_not_regular", setup: func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			writeFile(t, target, 0o600)
			symlink(t, target, filepath.Join(dir, "archive.db-journal"))
		}},
		{name: "a readable journal", reason: "archive_journal_permissions", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "archive.db-journal"), 0o644)
		}},
		{name: "a journal of another owner", reason: "archive_journal_foreign_owner", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "archive.db-journal"), 0o600)
		}, owner: func(fi fs.FileInfo) (int, bool) {
			if fi.Name() == "archive.db-journal" {
				return os.Geteuid() + 1, true
			}
			return os.Geteuid(), true
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, _ := testOptions(t)
			if tt.setup != nil {
				tt.setup(t, opts.DataDir)
			}
			opts.owner = tt.owner
			d, err := Open(t.Context(), Archive, opts)
			if err == nil {
				_ = d.Close()
				t.Fatalf("Open accepted %s", tt.name)
			}
			r, ok := errors.AsType[*Refusal](err)
			if !ok || r.Reason != tt.reason {
				t.Fatalf("Open = %v, want a refusal with reason %s", err, tt.reason)
			}
			if strings.Contains(r.Error(), opts.DataDir) {
				t.Fatalf("the refusal %q names the data directory", r.Error())
			}
		})
	}
}

func writeFile(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
}

func TestOpenChecksTheFilesystem(t *testing.T) {
	failing := func(string, *syscall.Statfs_t) error { return syscall.EIO }
	network := func(_ string, st *syscall.Statfs_t) error { networkStatfs(st); return nil }
	local := func(_ string, st *syscall.Statfs_t) error { localStatfs(st); return nil }
	for _, tt := range []struct {
		name    string
		profile Profile
		statfs  func(string, *syscall.Statfs_t) error
		reason  string
	}{
		{name: "local profile on a network filesystem", profile: Local, statfs: network, reason: "storage_network_filesystem"},
		{name: "an uninspectable filesystem", profile: Local, statfs: failing, reason: "storage_filesystem_unknown"},
		{name: "nfs profile on an uninspectable filesystem", profile: NFS, statfs: failing, reason: "storage_filesystem_unknown"},
		{name: "nfs profile on a network filesystem", profile: NFS, statfs: network},
		{name: "nfs profile on a local filesystem", profile: NFS, statfs: local},
		{name: "local profile on a local filesystem", profile: Local, statfs: local},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := checkFilesystem(t.TempDir(), tt.profile, tt.statfs)
			switch {
			case tt.reason == "" && r != nil:
				t.Fatalf("checkFilesystem = %v, want acceptance", r)
			case tt.reason != "" && (r == nil || r.Reason != tt.reason):
				t.Fatalf("checkFilesystem = %v, want reason %s", r, tt.reason)
			}
		})
	}
	opts, _ := testOptions(t)
	opts.statfs = network
	if _, err := Open(t.Context(), Archive, opts); err == nil {
		t.Fatal("Open accepted profile local on a network filesystem")
	}
	if _, err := os.Lstat(filepath.Join(opts.DataDir, "archive.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused Open created the database file: %v", err)
	}
}

func TestOpenRefusesUnknownProfilesAndNames(t *testing.T) {
	opts, _ := testOptions(t)
	opts.Profile = "efs"
	if _, err := Open(t.Context(), Archive, opts); err == nil {
		t.Fatal("Open accepted an unknown storage profile")
	}
	opts.Profile = ""
	if _, err := Open(t.Context(), Archive, opts); err == nil {
		t.Fatal("Open accepted the zero storage profile")
	}
	opts.Profile = Local
	if _, err := Open(t.Context(), Name(0), opts); err == nil {
		t.Fatal("Open accepted the zero database name")
	}
	opts.Logger = nil
	if _, err := Open(t.Context(), Archive, opts); err == nil {
		t.Fatal("Open accepted a nil logger")
	}
}

func TestTheProbeSeesTheLock(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	path := filepath.Join(opts.DataDir, "archive.db")
	if got := probeFromAnotherProcess(t, path); got != probeBusy {
		t.Fatalf("another process probing a held database = %d, want busy", got)
	}
	if got := probeFromThisProcess(t, path); got != probeBusy {
		t.Fatalf("another connection of this process probing a held database = %d, want busy", got)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := probeFromAnotherProcess(t, path); got != probeFree {
		t.Fatalf("another process probing a closed database = %d, want free", got)
	}
	if d.Healthy() {
		t.Fatal("a closed database reports healthy")
	}
	if err := d.Read(t.Context(), "test.closed", func(context.Context, Querier) error { return nil }); !errors.Is(err, errClosed) {
		t.Fatalf("Read after Close = %v, want errClosed", err)
	}
}

func holdConnection(t *testing.T, d *DB) (release func(), done <-chan error) {
	t.Helper()
	inside, released, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		result <- d.Read(context.Background(), "test.held", func(context.Context, Querier) error {
			close(inside)
			<-released
			return nil
		})
	}()
	<-inside
	return sync.OnceFunc(func() { close(released) }), result
}

func TestCloseReturnsOnceTheLockIsReleased(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	release, done := holdConnection(t, d)
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := probeFromThisProcess(t, filepath.Join(opts.DataDir, "archive.db")); got != probeFree {
		t.Fatalf("another connection of this process probing the database right after Close = %d, want free", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("the call that held the connection = %v", err)
	}
}

func TestCloseGivesUpOnAConnectionThatStaysInUse(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	d.closeWait = 50 * time.Millisecond
	release, done := holdConnection(t, d)
	defer release()
	if err := d.Close(); !errors.Is(err, errStillHeld) {
		t.Fatalf("Close while a call holds the connection = %v, want errStillHeld", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("the call that held the connection = %v", err)
	}
	if got := probeFromThisProcess(t, filepath.Join(opts.DataDir, "archive.db")); got != probeFree {
		t.Fatalf("probing the database once the call ended = %d, want free", got)
	}
}

func TestOpenWaitsForTheLock(t *testing.T) {
	opts, _ := testOptions(t)
	shortTimeouts(&opts)
	holder := mustOpen(t, opts)
	waiter := opts
	logger, logs := newLogger()
	waiter.Logger = logger
	waiter.acquireFor = time.Minute
	opened := make(chan error, 1)
	go func() {
		d, err := Open(context.Background(), Archive, waiter)
		if err == nil {
			defer func() { _ = d.Close() }()
			err = d.Read(context.Background(), "test.after_wait", func(context.Context, Querier) error { return nil })
		}
		opened <- err
	}()
	deadline := time.After(10 * time.Second)
	for len(logs.events("db_lock_wait")) == 0 {
		select {
		case err := <-opened:
			t.Fatalf("Open returned %v while another connection held the lock", err)
		case <-deadline:
			t.Fatal("no db_lock_wait event while another connection held the lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := holder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("Open after the holder closed = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open still waits after the holder closed")
	}
	if waits := logs.events("db_lock_wait"); len(waits) != 1 || waits[0]["database"] != "archive" || waits[0]["level"] != "WARN" {
		t.Fatalf("db_lock_wait events = %v, want one WARN naming the archive", waits)
	}
}

func TestOpenGivesUpOnTheLock(t *testing.T) {
	opts, _ := testOptions(t)
	shortTimeouts(&opts)
	mustOpen(t, opts)
	waiter := opts
	waiter.acquireFor = 300 * time.Millisecond
	began := time.Now()
	d, err := Open(t.Context(), Archive, waiter)
	if err == nil {
		_ = d.Close()
		t.Fatal("Open succeeded while another connection held the lock")
	}
	if !strings.Contains(err.Error(), "still locked") {
		t.Fatalf("Open = %v, want a still-locked error", err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("Open gave up after %v, want about its 300 ms bound", elapsed)
	}
}

func TestOpenGivesUpOnTheLockWhenItsWaitEndsBeforeAnAttempt(t *testing.T) {
	opts, _ := testOptions(t)
	shortTimeouts(&opts)
	mustOpen(t, opts)
	waiter := opts
	waiter.acquireFor = time.Nanosecond
	d, err := Open(t.Context(), Archive, waiter)
	if err == nil {
		_ = d.Close()
		t.Fatal("Open succeeded while another connection held the lock")
	}
	if want := "db: archive.db is still locked by another process after 1ns"; err.Error() != want {
		t.Fatalf("Open = %q, want %q: the end of the lock wait is not a stop signal", err, want)
	}
}

func TestOpenStopsWaitingWhenCancelled(t *testing.T) {
	opts, _ := testOptions(t)
	shortTimeouts(&opts)
	mustOpen(t, opts)
	waiter := opts
	waiter.acquireFor = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	d, err := Open(ctx, Archive, waiter)
	if err == nil {
		_ = d.Close()
		t.Fatal("Open succeeded while another connection held the lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open = %v, want the caller's cancellation", err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("Open returned %v after its context ended, want within one busy timeout", elapsed)
	}
}

func TestReconnectingIsRefused(t *testing.T) {
	opts, logs := testOptions(t)
	d := mustOpen(t, opts)
	if _, err := d.connector.Connect(t.Context()); !errors.Is(err, errReconnect) {
		t.Fatalf("a second Connect = %v, want errReconnect", err)
	}
	if _, err := d.connector.Connect(t.Context()); !errors.Is(err, errReconnect) {
		t.Fatalf("a third Connect = %v, want errReconnect", err)
	}
	if d.Healthy() {
		t.Fatal("the database reports healthy after its connection was lost")
	}
	if lost := logs.events("db_lost"); len(lost) != 1 || lost[0]["database"] != "archive" || lost[0]["level"] != "ERROR" {
		t.Fatalf("db_lost events = %v, want exactly one ERROR naming the archive", lost)
	}
}

func TestInterruptedCallsKeepTheConnectionAndTheLock(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(d *DB, ctx context.Context) error
	}{
		{name: "read", call: func(d *DB, ctx context.Context) error {
			return d.Read(ctx, "test.slow_read", func(ctx context.Context, q Querier) error {
				var n int
				return q.QueryRowContext(ctx, slowQuery).Scan(&n)
			})
		}},
		{name: "write", call: func(d *DB, ctx context.Context) error {
			return d.Write(ctx, "test.slow_write", func(ctx context.Context, q Querier) error {
				_, err := q.ExecContext(ctx, "INSERT INTO t "+slowQuery)
				return err
			})
		}},
		{name: "statement", call: slowStatement},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts, logs := testOptions(t)
			d := mustOpen(t, opts)
			exec1(t, d, "CREATE TABLE t(x INTEGER)")
			read, write := d.readTimeout, d.writeTimeout
			d.readTimeout, d.writeTimeout = 100*time.Millisecond, 100*time.Millisecond
			err := tt.call(d, t.Context())
			d.readTimeout, d.writeTimeout = read, write
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the slow %s = %v, want its deadline exceeded", tt.name, err)
			}
			path := filepath.Join(opts.DataDir, "archive.db")
			if got := probeFromAnotherProcess(t, path); got != probeBusy {
				t.Fatalf("another process after an interrupted %s = %d, want busy: the lock went with the connection", tt.name, got)
			}
			if got := d.sql.Stats().OpenConnections; got != 1 {
				t.Fatalf("%d open connections, want the one that holds the lock", got)
			}
			if n := count(t, d, "SELECT count(*) FROM t"); n != 0 {
				t.Fatalf("%d rows after an interrupted write, want none", n)
			}
			if !d.Healthy() || len(logs.events("db_lost")) != 0 {
				t.Fatal("the database lost its connection after an interrupted call")
			}
			events := logs.events("db_deadline")
			if len(events) != 1 {
				t.Fatalf("db_deadline events = %v, want one", events)
			}
			e := events[0]
			dump, _ := e["goroutines"].(string)
			if e["level"] != "ERROR" || e["database"] != "archive" || e["operation"] != "test.slow_"+tt.name || e["timeout_ms"] != float64(100) ||
				!strings.HasPrefix(dump, "goroutine profile: total ") {
				t.Fatalf("db_deadline event = %v", e)
			}
		})
	}
}

func park(depth int, parked *sync.WaitGroup, release <-chan struct{}) {
	if depth > 0 {
		park(depth-1, parked, release)
		return
	}
	parked.Done()
	<-release
}

func TestTheDeadlineEventKeepsItsGoroutineDumpUnderTheLineLimit(t *testing.T) {
	var parked sync.WaitGroup
	release := make(chan struct{})
	defer close(release)
	for depth := range 300 {
		parked.Add(1)
		go park(depth, &parked, release)
	}
	parked.Wait()
	opts, logs := testOptions(t)
	opts.ReadTimeout = 100 * time.Millisecond
	d := mustOpen(t, opts)
	if err := d.Read(t.Context(), "test.slow_read", func(ctx context.Context, q Querier) error {
		var n int
		return q.QueryRowContext(ctx, slowQuery).Scan(&n)
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the slow read = %v, want its deadline exceeded", err)
	}
	if dropped := logs.events("log_dropped"); len(dropped) != 0 {
		t.Fatalf("log_dropped events = %v: the deadline event was too long to log", dropped)
	}
	events := logs.events("db_deadline")
	if len(events) != 1 {
		t.Fatalf("db_deadline events = %d, want one", len(events))
	}
	const marker = "[truncated]\n"
	dump, _ := events[0]["goroutines"].(string)
	if !strings.HasSuffix(dump, marker) || len(dump) > maxGoroutineDump+len(marker) {
		t.Fatalf("the goroutine dump has %d bytes and ends %q, want at most %d ending %q", len(dump), dump[max(0, len(dump)-20):], maxGoroutineDump+len(marker), marker)
	}
}

func TestADisposableConnectionLosesTheLockOnInterrupt(t *testing.T) {
	opts, _ := testOptions(t)
	opts.ReadTimeout = 100 * time.Millisecond
	opts.disposable = true
	d := mustOpen(t, opts)
	if err := slowStatement(d, t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the slow statement = %v, want its deadline exceeded", err)
	}
	if got := probeFromAnotherProcess(t, filepath.Join(opts.DataDir, "archive.db")); got != probeFree {
		t.Fatalf("another process after an interrupted statement on a disposable connection = %d, want free, or the kept connection proves nothing", got)
	}
}

func slowStatement(d *DB, ctx context.Context) error {
	return d.within(ctx, "test.slow_statement", d.readTimeout, func(ctx context.Context) error {
		var n int
		return d.sql.QueryRowContext(ctx, slowQuery).Scan(&n)
	})
}

func TestNoDeadlineEventWhenTheCallerGivesUp(t *testing.T) {
	opts, logs := testOptions(t)
	d := mustOpen(t, opts)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := d.Read(ctx, "test.slow_read", func(ctx context.Context, q Querier) error {
		var n int
		return q.QueryRowContext(ctx, slowQuery).Scan(&n)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the slow read = %v, want the caller's deadline", err)
	}
	if events := logs.events("db_deadline"); len(events) != 0 {
		t.Fatalf("db_deadline events = %v, want none for the caller's own deadline", events)
	}
}

func TestTheDeadlineEventComesWhileTheCallStillRuns(t *testing.T) {
	opts, logs := testOptions(t)
	d := mustOpen(t, opts)
	release := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- d.within(t.Context(), "test.stuck", 100*time.Millisecond, func(context.Context) error {
			<-release
			return nil
		})
	}()
	for start := time.Now(); len(logs.events("db_deadline")) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 30*time.Second {
			close(release)
			t.Fatal("no db_deadline event while a call ran past its deadline")
		}
	}
	select {
	case err := <-returned:
		t.Fatalf("the call returned (%v) before it was released, so this test proves nothing", err)
	default:
	}
	e := logs.events("db_deadline")[0]
	dump, _ := e["goroutines"].(string)
	if e["operation"] != "test.stuck" || e["timeout_ms"] != float64(100) || !strings.Contains(dump, t.Name()) {
		t.Fatalf("db_deadline event = %v, want test.stuck with a dump that shows the stuck call", e)
	}
	close(release)
	if err := <-returned; err != nil {
		t.Fatalf("the released call = %v, want its own result", err)
	}
	if err := d.within(t.Context(), "test.quick", time.Minute, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("a quick call = %v", err)
	}
	if events := logs.events("db_deadline"); len(events) != 1 {
		t.Fatalf("db_deadline events = %d, want one for the stuck call and none for the quick one", len(events))
	}
}

func TestACallThatItsDeadlineEndsIsReportedOnce(t *testing.T) {
	opts, logs := testOptions(t)
	d := mustOpen(t, opts)
	const calls = 50
	for range calls {
		err := d.within(t.Context(), "test.ended", time.Millisecond, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a call that its deadline ended = %v", err)
		}
	}
	if events := logs.events("db_deadline"); len(events) != calls {
		t.Fatalf("db_deadline events = %d, want one for each of the %d calls that their deadline ended", len(events), calls)
	}
}

func TestRewriteRunsUnderItsOwnDeadline(t *testing.T) {
	opts, logs := testOptions(t)
	d := mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	outlive := func(ctx context.Context, q Querier) error {
		if _, err := q.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	}
	d.writeTimeout, d.rewriteTimeout = 100*time.Millisecond, time.Minute
	if err := d.Write(t.Context(), "test.write", outlive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a write beyond the write deadline = %v, want its deadline exceeded", err)
	}
	if err := d.Rewrite(t.Context(), "test.rewrite", outlive); err != nil {
		t.Fatalf("a rewrite beyond the write deadline but within its own = %v", err)
	}
	if n := count(t, d, "SELECT count(*) FROM t"); n != 1 {
		t.Fatalf("%d rows, want the rewrite's one", n)
	}
	d.writeTimeout, d.rewriteTimeout = time.Minute, 100*time.Millisecond
	if err := d.Rewrite(t.Context(), "test.rewrite", outlive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a rewrite beyond its own deadline = %v, want its deadline exceeded", err)
	}
	if n := count(t, d, "SELECT count(*) FROM t"); n != 1 {
		t.Fatalf("%d rows after an interrupted rewrite, want it rolled back", n)
	}
	events := logs.events("db_deadline")
	if len(events) != 2 || events[1]["operation"] != "test.rewrite" || events[1]["timeout_ms"] != float64(100) {
		t.Fatalf("db_deadline events = %v, want the write's and then the rewrite's", events)
	}
}

func TestWriteRollsBackOnError(t *testing.T) {
	opts, _ := testOptions(t)
	d := mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	failure := errors.New("synthetic failure")
	err := d.Write(t.Context(), "test.fail", func(ctx context.Context, q Querier) error {
		if _, err := q.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("Write = %v, want the synthetic failure", err)
	}
	if n := count(t, d, "SELECT count(*) FROM t"); n != 0 {
		t.Fatalf("%d rows after a failed write, want none", n)
	}
}

func TestFreeBytes(t *testing.T) {
	opts, _ := testOptions(t)
	opts.statfs = func(_ string, st *syscall.Statfs_t) error {
		localStatfs(st)
		st.Bavail = 10
		return nil
	}
	d := mustOpen(t, opts)
	if got, err := d.FreeBytes(); err != nil || got != 10*statfsUnit {
		t.Fatalf("FreeBytes = %d, %v, want %d", got, err, 10*statfsUnit)
	}
	d.statfs = func(string, *syscall.Statfs_t) error { return syscall.EIO }
	if _, err := d.FreeBytes(); err == nil {
		t.Fatal("FreeBytes hid a statfs failure")
	}
	real, _ := testOptions(t)
	if got, err := mustOpen(t, real).FreeBytes(); err != nil || got == 0 {
		t.Fatalf("FreeBytes on the test's filesystem = %d, %v", got, err)
	}
}

func TestFileURIKeepsThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a ?#%28%(b)")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	opts, _ := testOptions(t)
	opts.DataDir = dir
	d := mustOpen(t, opts)
	exec1(t, d, "CREATE TABLE t(x INTEGER)")
	if _, err := os.Lstat(filepath.Join(dir, "archive.db")); err != nil {
		t.Fatalf("the database is not at its path: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the parent directory holds %v (%v), want only the data directory", entries, err)
	}
}

func TestTheSessionDatabaseIsCheckedAndLockedLikeTheArchive(t *testing.T) {
	saved := syscall.Umask(0)
	defer syscall.Umask(saved)
	opts, _ := testOptions(t)
	archive := mustOpen(t, opts)
	session, err := Open(t.Context(), Session, opts)
	if err != nil {
		t.Fatalf("Open session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	raw := session.RawHandle()
	if raw == nil || raw == archive.RawHandle() {
		t.Fatal("the session database does not have a handle of its own")
	}
	for query, want := range map[string]string{
		"PRAGMA foreign_keys":  "1",
		"PRAGMA journal_mode":  "truncate",
		"PRAGMA locking_mode":  "exclusive",
		"PRAGMA secure_delete": "1",
	} {
		var got string
		if err := raw.QueryRowContext(t.Context(), query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s through the raw handle = %q, %v, want %q", query, got, err, want)
		}
	}
	if _, err := raw.ExecContext(t.Context(), "CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatalf("write through the raw handle: %v", err)
	}
	path := filepath.Join(opts.DataDir, "session.db")
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode() != 0o600 {
		t.Fatalf("session.db: %v, mode %v under umask 0, want 0600", err, fi.Mode())
	}
	if got := probeFromAnotherProcess(t, path); got != probeBusy {
		t.Fatalf("another process probing the held session database = %d, want busy", got)
	}
	if _, err := session.connector.Connect(t.Context()); !errors.Is(err, errReconnect) {
		t.Fatalf("a second connection to the session database = %v, want errReconnect", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := probeFromAnotherProcess(t, path); got != probeFree {
		t.Fatalf("another process probing the closed session database = %d, want free", got)
	}

	other, _ := testOptions(t)
	writeFile(t, filepath.Join(other.DataDir, "session.db"), 0o640)
	if d, err := Open(t.Context(), Session, other); err == nil {
		_ = d.Close()
		t.Fatal("Open accepted a group-readable session.db")
	} else if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "session_db_permissions" || !strings.Contains(r.Error(), "session.db") {
		t.Fatalf("Open = %v, want the refusal session_db_permissions naming session.db", err)
	}
	writeFile(t, filepath.Join(other.DataDir, "session.db"), 0o600)
	writeFile(t, filepath.Join(other.DataDir, "session.db-journal"), 0o604)
	if d, err := Open(t.Context(), Session, other); err == nil {
		_ = d.Close()
		t.Fatal("Open accepted a world-readable session.db-journal")
	} else if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "session_journal_permissions" {
		t.Fatalf("Open = %v, want the refusal session_journal_permissions", err)
	}
}
