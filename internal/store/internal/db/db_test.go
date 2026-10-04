package db

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
