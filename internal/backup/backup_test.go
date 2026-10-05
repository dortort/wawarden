package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	_ "modernc.org/sqlite"

	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/session"
)

var epoch = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const backupName = "20261005T120000Z.age"

type recorder struct {
	mu     sync.Mutex
	done   [][4]int64
	failed []string
}

func (r *recorder) BackupDone(bytes, archiveBytes, sessionBytes int64, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = append(r.done, [4]int64{bytes, archiveBytes, sessionBytes, int64(took)})
}

func (r *recorder) BackupFailed(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, reason)
}

func identity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	return id
}

func stores(t *testing.T, dir string) (*ingest.Store, *session.Store) {
	t.Helper()
	logger := logx.New(logx.NewWriter(io.Discard), slog.LevelInfo)
	archive, err := ingest.Open(t.Context(), ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, MinFreeBytes: 1, Logger: logger})
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	sess, err := session.Open(t.Context(), session.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return archive, sess
}

func taker(t *testing.T, dir string, recipient string, archive, sess Database) (*Taker, *recorder) {
	t.Helper()
	rec := &recorder{}
	tk, err := New(Options{DataDir: dir, UID: os.Geteuid(), Recipient: recipient, Version: "v0.0.0-test", Archive: archive, Session: sess, Notify: rec,
		Now: func() time.Time { return epoch }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tk, rec
}

func seedBlobs(t *testing.T, s *ingest.Store, prefix string, n, refBytes int) {
	t.Helper()
	ref := bytes.Repeat([]byte{7}, refBytes)
	if err := s.Write(t.Context(), "test.seed", func(tx *ingest.Tx) error {
		for i := range n {
			if _, err := tx.RecordBlob(fmt.Sprintf("%s-%d", prefix, i), ref, epoch); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

type member struct {
	name string
	mode int64
	body []byte
}

func decrypt(t *testing.T, path string, id age.Identity) []member {
	t.Helper()
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("open the backup: %v", err)
	}
	defer func() { _ = f.Close() }()
	r, err := age.Decrypt(f, id)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	var out []member
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read the archive: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", h.Name, err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Fatalf("member %s has type %c, want a regular file", h.Name, h.Typeflag)
		}
		out = append(out, member{name: h.Name, mode: h.Mode, body: body})
	}
}

func openCopy(t *testing.T, data []byte) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var check string
	if err := db.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE written(x)"); err == nil {
		t.Fatal("the copy was opened writable")
	}
	return db
}

func scalar(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	return fi.Mode()
}

type observed struct {
	Database
	seen func(staging string)
}

func (o observed) Backup(ctx context.Context, staging string, write func(string, int64, io.Reader) error) error {
	return o.Database.Backup(ctx, staging, func(name string, size int64, r io.Reader) error {
		o.seen(staging)
		return write(name, size, r)
	})
}

func TestARoundTripRestoresBothDatabases(t *testing.T) {
	saved := syscall.Umask(0)
	defer syscall.Umask(saved)
	dir := t.TempDir()
	archive, sess := stores(t, dir)
	seedBlobs(t, archive, "blob", 50, 100)
	id := identity(t)
	staged := map[string]fs.FileMode{}
	watch := func(staging string) {
		staged[filepath.Base(staging)] = mode(t, staging)
		staged["tmp"] = mode(t, filepath.Dir(staging))
		staged[backupName] = mode(t, filepath.Join(filepath.Dir(staging), backupName))
	}
	tk, rec := taker(t, dir, id.Recipient().String(), observed{archive, watch}, observed{sess, watch})

	if err := tk.Take(t.Context()); err != nil {
		t.Fatalf("Take: %v", err)
	}
	backups := filepath.Join(dir, Dir)
	if got := entries(t, backups); !slices.Equal(got, []string{backupName}) {
		t.Fatalf("backups/ holds %v, want only %s", got, backupName)
	}
	if m := mode(t, backups); m != fs.ModeDir|0o700 {
		t.Fatalf("backups/ has mode %v, want 0700", m)
	}
	path := filepath.Join(backups, backupName)
	if m := mode(t, path); m != 0o600 {
		t.Fatalf("the backup has mode %v, want 0600", m)
	}
	want := map[string]fs.FileMode{"archive.stage": 0o600, "session.stage": 0o600, "tmp": fs.ModeDir | 0o700, backupName: 0o600}
	if !maps.Equal(staged, want) {
		t.Fatalf("modes while staging %v, want %v", staged, want)
	}

	members := decrypt(t, path, id)
	var names []string
	for _, m := range members {
		names = append(names, m.name)
		if m.mode != 0o600 {
			t.Fatalf("member %s has mode %o", m.name, m.mode)
		}
	}
	if !slices.Equal(names, []string{"archive.db", "session.db", manifestName}) {
		t.Fatalf("members %v", names)
	}
	var raw map[string]any
	if err := json.Unmarshal(members[2].body, &raw); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if keys := slices.Sorted(maps.Keys(raw)); !slices.Equal(keys, []string{"created", "databases", "format", "version"}) {
		t.Fatalf("manifest keys %v", keys)
	}
	var m manifest
	if err := json.Unmarshal(members[2].body, &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	wantManifest := manifest{Format: 1, Version: "v0.0.0-test", Created: "2026-10-05T12:00:00Z", Databases: []entry{
		{Name: "archive.db", SchemaVersion: archive.SchemaVersion(), Bytes: int64(len(members[0].body))},
		{Name: "session.db", SchemaVersion: sess.SchemaVersion(), Bytes: int64(len(members[1].body))},
	}}
	if fmt.Sprint(m) != fmt.Sprint(wantManifest) {
		t.Fatalf("manifest %+v, want %+v", m, wantManifest)
	}

	archived := openCopy(t, members[0].body)
	if n := scalar(t, archived, "SELECT count(*) FROM history_blobs"); n != 50 {
		t.Fatalf("the archive copy holds %d blobs, want 50", n)
	}
	if v := scalar(t, archived, "PRAGMA user_version"); int(v) != archive.SchemaVersion() {
		t.Fatalf("the archive copy is at schema %d, want %d", v, archive.SchemaVersion())
	}
	device := openCopy(t, members[1].body)
	if v := scalar(t, device, "SELECT version FROM whatsmeow_version"); int(v) != sess.SchemaVersion() {
		t.Fatalf("the device store copy is at schema %d, want %d", v, sess.SchemaVersion())
	}
	if n := scalar(t, device, "SELECT count(*) FROM whatsmeow_device"); n != 0 {
		t.Fatalf("the device store copy holds %d devices, want the empty store", n)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if len(rec.failed) != 0 || len(rec.done) != 1 {
		t.Fatalf("events done %v failed %v, want one backup_done", rec.done, rec.failed)
	}
	if d := rec.done[0]; d[0] != fi.Size() || d[1] != int64(len(members[0].body)) || d[2] != int64(len(members[1].body)) || d[3] != 0 {
		t.Fatalf("backup_done %v, want the file's size %d and the copies' sizes", d, fi.Size())
	}
}

func TestABackupTakenWhileIngestWrites(t *testing.T) {
	dir := t.TempDir()
	archive, sess := stores(t, dir)
	seedBlobs(t, archive, "seed", 1200, 4000)
	id := identity(t)
	var written atomic.Int64
	stop := make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		defer close(failed)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			err := archive.Write(context.Background(), "test.ingest", func(tx *ingest.Tx) error {
				_, err := tx.RecordBlob(fmt.Sprintf("live-%d", i), []byte("ref"), epoch)
				return err
			})
			if err != nil {
				failed <- err
				return
			}
			written.Add(1)
		}
	}()
	for written.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	var before, copied int64
	watch := func(string) {
		if copied == 0 {
			copied = written.Load()
		}
	}
	before = written.Load()
	tk, rec := taker(t, dir, id.Recipient().String(), observed{archive, watch}, sess)
	err := tk.Take(t.Context())
	close(stop)
	if werr := <-failed; werr != nil {
		t.Fatalf("an ingest write failed during the backup: %v", werr)
	}
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if copied <= before {
		t.Fatalf("no ingest write completed while the archive was copied (%d before, %d when copied): the copy held the connection throughout", before, copied)
	}
	members := decrypt(t, filepath.Join(dir, Dir, backupName), id)
	got := scalar(t, openCopy(t, members[0].body), "SELECT count(*) FROM history_blobs WHERE id LIKE 'live-%'")
	if got < before || got > copied {
		t.Fatalf("the copy holds %d live writes, want between %d and %d: a consistent point in time", got, before, copied)
	}
	if len(rec.done) != 1 {
		t.Fatalf("backup_done events %v", rec.done)
	}
}

func mkdirMode(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

type fakeDB struct {
	name   string
	body   []byte
	size   int64
	fail   error
	during func()
}

func (f fakeDB) Backup(_ context.Context, staging string, write func(string, int64, io.Reader) error) error {
	if err := os.WriteFile(staging, f.body, 0o600); err != nil {
		return err
	}
	if f.during != nil {
		f.during()
	}
	if err := write(f.name, f.size, bytes.NewReader(f.body)); err != nil {
		return err
	}
	return f.fail
}

func (fakeDB) SchemaVersion() int { return 1 }

func fakeArchive() fakeDB {
	body := bytes.Repeat([]byte("archive page "), 20000)
	return fakeDB{name: "archive.db", body: body, size: int64(len(body))}
}

func fakeSession() fakeDB {
	body := bytes.Repeat([]byte("session page "), 3000)
	return fakeDB{name: "session.db", body: body, size: int64(len(body))}
}

type failingWriter struct {
	w    io.Writer
	left int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		n, _ := f.w.Write(p[:f.left])
		f.left = 0
		return n, syscall.ENOSPC
	}
	f.left -= len(p)
	return f.w.Write(p)
}

func TestEveryFailureLeavesNoStagingOrPartialFile(t *testing.T) {
	synthetic := errors.New("synthetic failure")
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		archive func(cancel context.CancelFunc) fakeDB
		session func() fakeDB
		hook    func(tk *Taker)
		reason  string
		keep    []string
	}{
		{name: "backups is a file", reason: reasonDirectory, keep: nil, prepare: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, Dir), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "backups grants access to others", reason: reasonDirectory, prepare: func(t *testing.T, dir string) {
			mkdirMode(t, filepath.Join(dir, Dir), 0o755)
		}},
		{name: "the staging directory is a symbolic link", reason: reasonDirectory, keep: []string{stagingDir}, prepare: func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, Dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, Dir, stagingDir)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a backup with the same name exists", reason: reasonExists, keep: []string{backupName}, prepare: func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, Dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, Dir, backupName), []byte("earlier"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the archive copy fails", reason: reasonArchive, archive: func(context.CancelFunc) fakeDB {
			a := fakeArchive()
			a.fail = synthetic
			return a
		}},
		{name: "the archive copy is shorter than its size", reason: reasonArchive, archive: func(context.CancelFunc) fakeDB {
			a := fakeArchive()
			a.size++
			return a
		}},
		{name: "the session copy fails", reason: reasonSession, session: func() fakeDB {
			s := fakeSession()
			s.fail = synthetic
			return s
		}},
		{name: "the service stops during the copy", reason: reasonCancelled, archive: func(cancel context.CancelFunc) fakeDB {
			a := fakeArchive()
			a.during = cancel
			return a
		}},
		{name: "the disk fills during the archive", reason: reasonArchive, hook: func(tk *Taker) {
			tk.output = func(w io.Writer) io.Writer { return &failingWriter{w: w, left: 100 << 10} }
		}},
		{name: "the disk fills on the last chunk", reason: reasonEncrypt, hook: func(tk *Taker) {
			tk.output = func(w io.Writer) io.Writer { return &failingWriter{w: w, left: 290 << 10} }
		}},
		{name: "fsync of the file fails", reason: reasonSync, hook: func(tk *Taker) {
			tk.syncFile = func(*os.File) error { return synthetic }
		}},
		{name: "the rename fails", reason: reasonRename, hook: func(tk *Taker) {
			tk.rename = func(string, string) error { return synthetic }
		}},
		{name: "fsync of the directory fails", reason: reasonSync, hook: func(tk *Taker) {
			tk.syncDir = func(string) error { return synthetic }
		}},
		{name: "removing the staging directory fails", reason: reasonCleanup, hook: func(tk *Taker) {
			tk.removeAll = func(path string) error { return errors.Join(os.RemoveAll(path), synthetic) }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.prepare != nil {
				tt.prepare(t, dir)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			a, s := fakeArchive(), fakeSession()
			if tt.archive != nil {
				a = tt.archive(cancel)
			}
			if tt.session != nil {
				s = tt.session()
			}
			tk, rec := taker(t, dir, identity(t).Recipient().String(), a, s)
			if tt.hook != nil {
				tt.hook(tk)
			}
			if err := tk.Take(ctx); err == nil {
				t.Fatal("Take succeeded")
			}
			if len(rec.done) != 0 || !slices.Equal(rec.failed, []string{tt.reason}) {
				t.Fatalf("events done %v failed %v, want one backup_failed %s", rec.done, rec.failed, tt.reason)
			}
			if fi, err := os.Lstat(filepath.Join(dir, Dir)); err == nil && fi.IsDir() {
				if got := entries(t, filepath.Join(dir, Dir)); !slices.Equal(got, tt.keep) {
					t.Fatalf("backups/ holds %v after the failure, want %v", got, tt.keep)
				}
			}
		})
	}
}

func TestAFailedSizeCheckNeverEncryptsBeyondTheDeclaredSize(t *testing.T) {
	a := fakeArchive()
	a.size--
	tk, rec := taker(t, t.TempDir(), identity(t).Recipient().String(), a, fakeSession())
	if err := tk.Take(t.Context()); err == nil || !slices.Equal(rec.failed, []string{reasonArchive}) {
		t.Fatalf("Take = %v with events %v, want the archive failure", err, rec.failed)
	}
}

func TestTruncatedAndAlteredBackupsFailToDecrypt(t *testing.T) {
	dir := t.TempDir()
	id := identity(t)
	tk, _ := taker(t, dir, id.Recipient().String(), fakeArchive(), fakeSession())
	if err := tk.Take(t.Context()); err != nil {
		t.Fatalf("Take: %v", err)
	}
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, Dir, backupName)))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	read := func(b []byte) error {
		r, err := age.Decrypt(bytes.NewReader(b), id)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, r)
		return err
	}
	if err := read(data); err != nil {
		t.Fatalf("the intact backup does not decrypt: %v", err)
	}
	for _, n := range []int{0, 10, 100, 200, 64 << 10, len(data) / 2, len(data) - 17, len(data) - 1} {
		if err := read(data[:n]); err == nil {
			t.Errorf("a backup truncated to %d of %d bytes decrypted", n, len(data))
		}
	}
	for _, at := range []int{0, 20, 60, 150, 300, 64 << 10, len(data) / 2, len(data) - 1} {
		flipped := bytes.Clone(data)
		flipped[at] ^= 0x04
		if err := read(flipped); err == nil {
			t.Errorf("a backup with a bit flipped at byte %d decrypted", at)
		}
	}
	if err := read(data); err != nil {
		t.Fatal("the test altered the original")
	}
	if err := read(append(bytes.Clone(data), 0)); err == nil {
		t.Error("a backup with a trailing byte decrypted")
	}
	other := identity(t)
	if _, err := age.Decrypt(bytes.NewReader(data), other); err == nil {
		t.Error("another identity decrypted the backup")
	}
}

func TestRecipientsAreParsedWithoutEchoingTheValue(t *testing.T) {
	x := identity(t)
	hybrid, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatalf("GenerateHybridIdentity: %v", err)
	}
	for _, valid := range []string{x.Recipient().String(), hybrid.Recipient().String()} {
		if err := CheckRecipient(valid); err != nil {
			t.Errorf("CheckRecipient refused a valid recipient: %v", err)
		}
	}
	secret := x.String()
	recipient := x.Recipient().String()
	broken := recipient[:len(recipient)-1] + "q"
	if strings.HasSuffix(recipient, "q") {
		broken = recipient[:len(recipient)-1] + "p"
	}
	invalid := map[string]string{
		"empty":                  "",
		"an X25519 secret key":   secret,
		"a hybrid secret key":    hybrid.String(),
		"a broken checksum":      broken,
		"leading space":          " " + recipient,
		"trailing newline":       recipient + "\n",
		"two recipients":         recipient + "\n" + hybrid.Recipient().String(),
		"a comment":              "# " + recipient,
		"an ssh key":             "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISyntheticSyntheticSyntheticSynthetic",
		"a plugin recipient":     "age1synthetic1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		"a recipient and secret": recipient + secret,
		"a non-ASCII character":  recipient + "é",
	}
	for name, v := range invalid {
		err := CheckRecipient(v)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if v != "" && (strings.Contains(err.Error(), v) || strings.Contains(err.Error(), strings.TrimSpace(v)[5:20])) {
			t.Errorf("%s: the error echoes the value: %v", name, err)
		}
		if _, err := New(Options{Recipient: v, Archive: fakeArchive(), Session: fakeSession(), Notify: &recorder{}}); err == nil || strings.Contains(err.Error(), "AGE-SECRET") {
			t.Errorf("%s: New = %v", name, err)
		}
	}
	if _, err := New(Options{Recipient: recipient}); err == nil {
		t.Error("New without databases or a notifier was accepted")
	}
}

func TestStaleStagingIsRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveStaging(dir); err != nil {
		t.Fatalf("RemoveStaging with nothing staged: %v", err)
	}
	staging := filepath.Join(dir, Dir, stagingDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"archive.stage", backupName} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte("plaintext"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStaging(dir); err != nil {
		t.Fatalf("RemoveStaging: %v", err)
	}
	if got := entries(t, filepath.Join(dir, Dir)); len(got) != 0 {
		t.Fatalf("backups/ holds %v after RemoveStaging", got)
	}
}
