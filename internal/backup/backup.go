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
)

var (
	errRecipient     = errors.New("backup: the recipient is not one age recipient")
	errDirectory     = errors.New("backup: the directory is not a directory owned by this user with mode 0700")
	errExists        = errors.New("backup: a backup with this name exists")
	errShortCopy     = errors.New("backup: a database copy ended before its size")
	errMissingSource = errors.New("backup: both databases and a notifier are required")
)

type Database interface {
	Backup(ctx context.Context, staging string, write func(name string, size int64, r io.Reader) error) error
	SchemaVersion() int
}

type Notifier interface {
	BackupDone(bytes, archiveBytes, sessionBytes int64, took time.Duration)
	BackupFailed(reason string)
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

	output    func(io.Writer) io.Writer
	syncFile  func(*os.File) error
	rename    func(oldpath, newpath string) error
	syncDir   func(string) error
	removeAll func(string) error
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
		archive: opts.Archive, session: opts.Session, notify: opts.Notify, now: now,
		output: func(w io.Writer) io.Writer { return w }, syncFile: (*os.File).Sync, rename: os.Rename, syncDir: syncDir, removeAll: os.RemoveAll,
	}, nil
}

func RemoveStaging(dataDir string) error {
	return os.RemoveAll(filepath.Join(dataDir, Dir, stagingDir))
}

func (t *Taker) Take(ctx context.Context) error {
	began := t.now()
	written, sizes, err := t.take(ctx, began)
	if err != nil {
		reason := reasonOutput
		if f, ok := errors.AsType[*failure](err); ok {
			reason = f.reason
		}
		if ctx.Err() != nil {
			reason = reasonCancelled
		}
		t.notify.BackupFailed(reason)
		return err
	}
	t.notify.BackupDone(written, sizes[0], sizes[1], t.now().Sub(began))
	return nil
}

func (t *Taker) take(ctx context.Context, began time.Time) (int64, [2]int64, error) {
	var sizes [2]int64
	staging := filepath.Join(t.dir, stagingDir)
	for _, dir := range []string{t.dir, staging} {
		if err := t.prepare(dir); err != nil {
			return 0, sizes, &failure{reasonDirectory, err}
		}
	}
	name := began.UTC().Format(nameLayout) + fileSuffix
	written, err := t.write(ctx, began, staging, name, &sizes)
	if err != nil {
		return 0, sizes, errors.Join(err, t.removeAll(staging))
	}
	if err := t.removeAll(staging); err != nil {
		return 0, sizes, errors.Join(&failure{reasonCleanup, err}, os.Remove(filepath.Join(t.dir, name)))
	}
	return written, sizes, nil
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
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600, ModTime: at, Format: tar.FormatUSTAR}
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
