// Package backup writes encrypted backups of both databases under backups/ in the data directory; it holds only an age recipient, so the service can never read a backup back.
package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
)

const Dir = "backups"

const (
	stagingDir     = "tmp"
	fileSuffix     = ".age"
	stageSuffix    = ".stage"
	manifestName   = "manifest.json"
	nameLayout     = "20060102T150405Z"
	manifestFormat = 1
)

const (
	reasonDirectory = "directory"
	reasonExists    = "exists"
	reasonOutput    = "output"
	reasonArchive   = "archive"
	reasonSession   = "session"
	reasonManifest  = "manifest"
	reasonEncrypt   = "encrypt"
	reasonSync      = "sync"
	reasonRename    = "rename"
	reasonCleanup   = "cleanup"
	reasonCancelled = "cancelled"
	reasonSpace     = "space"

	reasonUnexpected = "unexpected_entry"
	reasonRemove     = "remove"
)

var (
	errRecipient     = errors.New("backup: the recipient is not one age recipient")
	errDirectory     = errors.New("backup: the directory is not a directory owned by this user with mode 0700")
	errExists        = errors.New("backup: a backup with this name exists")
	errShortCopy     = errors.New("backup: a database copy ended before its size")
	errMissingSource = errors.New("backup: both databases and a notifier are required")
	errSpace         = errors.New("backup: the free space would fall below the floor")
	errKeep          = errors.New("backup: at least one backup must be kept")
	errUnexpected    = errors.New("backup: the directory holds an entry that is not a backup")
)

var namePattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.age$`)

type Database interface {
	Backup(ctx context.Context, staging string, write func(name string, size int64, r io.Reader) error) error
	SchemaVersion() int
	Changes(ctx context.Context) (int64, error)
	Size() (int64, error)
}

type Notifier interface {
	BackupDone(bytes, archiveBytes, sessionBytes int64, took time.Duration)
	BackupFailed(reason string)
	BackupPruned(removed, kept int)
	BackupPruneFailed(reason string)
}

type Options struct {
	DataDir   string
	UID       int
	Recipient string
	Version   string
	Archive   Database
	Session   Database
	Notify    Notifier
	Now       func() time.Time
	Space     func() (free, floor uint64, err error)
}

type Taker struct {
	dir       string
	uid       int
	recipient age.Recipient
	version   string
	archive   Database
	session   Database
	notify    Notifier
	now       func() time.Time
	space     func() (free, floor uint64, err error)

	output    func(io.Writer) io.Writer
	syncFile  func(*os.File) error
	rename    func(oldpath, newpath string) error
	syncDir   func(string) error
	removeAll func(string) error
	remove    func(string) error
	lstat     func(string) (fs.FileInfo, error)
}

type failure struct {
	reason string
	err    error
}

func (f *failure) Error() string { return "backup: " + f.reason + ": " + f.err.Error() }

func (f *failure) Unwrap() error { return f.err }

type manifest struct {
	Format    int     `json:"format"`
	Version   string  `json:"version"`
	Created   string  `json:"created"`
	Databases []entry `json:"databases"`
}

type entry struct {
	Name          string `json:"name"`
	SchemaVersion int    `json:"schema_version"`
	Bytes         int64  `json:"bytes"`
}

func CheckRecipient(s string) error {
	_, err := parseRecipient(s)
	return err
}

func parseRecipient(s string) (age.Recipient, error) {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return nil, errRecipient
	}
	recipients, err := age.ParseRecipients(strings.NewReader(s))
	if err != nil || len(recipients) != 1 {
		return nil, errRecipient
	}
	return recipients[0], nil
}

func New(opts Options) (*Taker, error) {
	recipient, err := parseRecipient(opts.Recipient)
	if err != nil {
		return nil, err
	}
	if opts.Archive == nil || opts.Session == nil || opts.Notify == nil {
		return nil, errMissingSource
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Taker{
		dir: filepath.Join(opts.DataDir, Dir), uid: opts.UID, recipient: recipient, version: opts.Version,
		archive: opts.Archive, session: opts.Session, notify: opts.Notify, now: now, space: opts.Space,
		output: func(w io.Writer) io.Writer { return w }, syncFile: (*os.File).Sync, rename: os.Rename, syncDir: syncDir, removeAll: os.RemoveAll,
		remove: os.Remove, lstat: os.Lstat,
	}, nil
}

func RemoveStaging(dataDir string) error {
	return os.RemoveAll(filepath.Join(dataDir, Dir, stagingDir))
}

func (t *Taker) Take(ctx context.Context) (string, error) {
	began := t.now()
	name := began.UTC().Format(nameLayout) + fileSuffix
	written, sizes, err := t.take(ctx, began, name)
	if err != nil {
		reason := reasonOutput
		if f, ok := errors.AsType[*failure](err); ok {
			reason = f.reason
		}
		if ctx.Err() != nil {
			reason = reasonCancelled
		}
		t.notify.BackupFailed(reason)
		return "", err
	}
	t.notify.BackupDone(written, sizes[0], sizes[1], t.now().Sub(began))
	return name, nil
}

func (t *Taker) take(ctx context.Context, began time.Time, name string) (int64, [2]int64, error) {
	var sizes [2]int64
	if err := t.preflight(); err != nil {
		return 0, sizes, &failure{reasonSpace, err}
	}
	staging := filepath.Join(t.dir, stagingDir)
	for _, dir := range []string{t.dir, staging} {
		if err := t.prepare(dir); err != nil {
			return 0, sizes, &failure{reasonDirectory, err}
		}
	}
	written, err := t.write(ctx, began, staging, name, &sizes)
	if err != nil {
		return 0, sizes, errors.Join(err, t.removeAll(staging))
	}
	if err := t.removeAll(staging); err != nil {
		return 0, sizes, errors.Join(&failure{reasonCleanup, err}, os.Remove(filepath.Join(t.dir, name)))
	}
	return written, sizes, nil
}

func (t *Taker) preflight() error {
	if t.space == nil {
		return nil
	}
	free, floor, err := t.space()
	if err != nil || floor == 0 {
		return err
	}
	a, errA := t.archive.Size()
	s, errS := t.session.Size()
	if err := errors.Join(errA, errS); err != nil {
		return err
	}
	if a < 0 || s < 0 {
		return errSpace
	}
	need := []uint64{floor, uint64(max(a, s)), uint64(a), uint64(s)}
	var sum uint64
	for _, n := range need {
		if sum+n < sum {
			return errSpace
		}
		sum += n
	}
	if free < sum {
		return errSpace
	}
	return nil
}

func (t *Taker) Prune(ctx context.Context, keep int, protect string) (removed, kept int, err error) {
	if keep < 1 {
		return 0, 0, errKeep
	}
	removed, kept, reason, err := t.prune(ctx, keep, protect)
	switch {
	case reason != "":
		t.notify.BackupPruneFailed(reason)
	case err == nil && removed > 0:
		t.notify.BackupPruned(removed, kept)
	}
	return removed, kept, err
}

func (t *Taker) prune(ctx context.Context, keep int, protect string) (removed, kept int, reason string, err error) {
	if _, err := os.Lstat(t.dir); errors.Is(err, fs.ErrNotExist) {
		return 0, 0, "", nil
	}
	if err := t.prepare(t.dir); err != nil {
		return 0, 0, reasonDirectory, err
	}
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return 0, 0, reasonDirectory, err
	}
	type backupFile struct {
		name string
		at   time.Time
	}
	var files []backupFile
	for _, e := range entries {
		fi, err := t.lstat(filepath.Join(t.dir, e.Name()))
		if err != nil {
			return 0, 0, reasonDirectory, err
		}
		if e.Name() == stagingDir && owned(fi, t.uid, true) {
			continue
		}
		at, ok := parseName(e.Name())
		if !ok || !owned(fi, t.uid, false) {
			return 0, 0, reasonUnexpected, errUnexpected
		}
		files = append(files, backupFile{e.Name(), at})
	}
	slices.SortFunc(files, func(a, b backupFile) int { return b.at.Compare(a.at) })
	for i, f := range files {
		if i < keep || f.name == protect {
			kept++
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, len(files) - removed, "", errors.Join(err, t.syncRemoved(removed))
		}
		if err := t.remove(filepath.Join(t.dir, f.name)); err != nil {
			return removed, len(files) - removed, reasonRemove, errors.Join(err, t.syncRemoved(removed))
		}
		removed++
	}
	if err := t.syncRemoved(removed); err != nil {
		return removed, kept, reasonSync, err
	}
	return removed, kept, "", nil
}

func (t *Taker) syncRemoved(removed int) error {
	if removed == 0 {
		return nil
	}
	return t.syncDir(t.dir)
}

func owned(fi fs.FileInfo, uid int, dir bool) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return false
	}
	mode := fi.Mode() & (fs.ModeType | fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if dir {
		return mode == fs.ModeDir|0o700
	}
	return mode == 0o600
}

func parseName(name string) (time.Time, bool) {
	if !namePattern.MatchString(name) {
		return time.Time{}, false
	}
	stem := strings.TrimSuffix(name, fileSuffix)
	at, err := time.Parse(nameLayout, stem)
	if err != nil || at.Format(nameLayout) != stem {
		return time.Time{}, false
	}
	return at, true
}

func (t *Taker) write(ctx context.Context, began time.Time, staging, name string, sizes *[2]int64) (int64, error) {
	partial, final := filepath.Join(staging, name), filepath.Join(t.dir, name)
	if _, err := os.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
		return 0, &failure{reasonExists, errors.Join(errExists, err)}
	}
	f, err := os.OpenFile(filepath.Clean(partial), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return 0, &failure{reasonOutput, err}
	}
	out := &countingWriter{w: t.output(f)}
	if err := t.encrypt(ctx, began, staging, out, sizes); err != nil {
		return 0, errors.Join(err, f.Close())
	}
	if err := t.syncFile(f); err != nil {
		return 0, &failure{reasonSync, errors.Join(err, f.Close())}
	}
	if err := f.Close(); err != nil {
		return 0, &failure{reasonSync, err}
	}
	if err := t.rename(partial, final); err != nil {
		return 0, &failure{reasonRename, err}
	}
	if err := t.syncDir(t.dir); err != nil {
		return 0, &failure{reasonSync, errors.Join(err, os.Remove(final))}
	}
	return out.n, nil
}

func (t *Taker) encrypt(ctx context.Context, began time.Time, staging string, out io.Writer, sizes *[2]int64) error {
	enc, err := age.Encrypt(out, t.recipient)
	if err != nil {
		return &failure{reasonEncrypt, err}
	}
	tw := tar.NewWriter(enc)
	stamp := began.UTC().Truncate(time.Second)
	m := manifest{Format: manifestFormat, Version: t.version, Created: stamp.Format(time.RFC3339)}
	for i, src := range []struct {
		label, reason string
		db            Database
	}{{"archive", reasonArchive, t.archive}, {"session", reasonSession, t.session}} {
		err := src.db.Backup(ctx, filepath.Join(staging, src.label+stageSuffix), func(name string, size int64, r io.Reader) error {
			if err := tw.WriteHeader(header(name, size, stamp)); err != nil {
				return err
			}
			n, err := io.Copy(tw, contextReader{ctx: ctx, r: r})
			if err == nil && n != size {
				err = errShortCopy
			}
			sizes[i] = size
			m.Databases = append(m.Databases, entry{Name: name, SchemaVersion: src.db.SchemaVersion(), Bytes: size})
			return err
		})
		if err != nil {
			return &failure{src.reason, err}
		}
	}
	body, err := json.Marshal(m)
	if err == nil {
		err = tw.WriteHeader(header(manifestName, int64(len(body)), stamp))
	}
	if err == nil {
		_, err = tw.Write(body)
	}
	if err != nil {
		return &failure{reasonManifest, err}
	}
	if err := errors.Join(tw.Close(), enc.Close()); err != nil {
		return &failure{reasonEncrypt, err}
	}
	return nil
}

func header(name string, size int64, at time.Time) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600, ModTime: at, Format: tar.FormatPAX}
}

func (t *Taker) prepare(dir string) error {
	if err := os.Mkdir(dir, 0o700); err == nil {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; this clears the setgid bit Linux copies from a setgid parent
			return err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0o700 || !ok || int(st.Uid) != t.uid {
		return fmt.Errorf("%w: %s", errDirectory, filepath.Base(dir))
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
