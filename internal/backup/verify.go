package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

const (
	maxManifestBytes = 64 << 10
	maxMemberBytes   = 1 << 40
	memberPrefix     = "member-"
	verifyPrefix     = "verify-"
)

var (
	ErrUnreadable = errors.New("backup: the stream could not be read")
	ErrRejected   = errors.New("backup: the stream is not a complete backup")
	ErrWorkDir    = errors.New("backup: the work directory is not an existing directory owned by this user with mode 0700")
	ErrKeep       = errors.New("backup: the files cannot be kept")
)

type Member struct {
	Path          string
	SchemaVersion int
	Bytes         int64
}

type Extracted struct {
	Dir     string
	Format  int
	Version string
	Created time.Time
	Members [2]Member
}

type ExtractOptions struct {
	WorkDir string
	UID     int
	Names   [2]string
	Free    func(dir string) (uint64, error)
}

func Extract(ctx context.Context, r io.Reader, opts ExtractOptions) (Extracted, error) {
	fi, err := os.Lstat(opts.WorkDir)
	if err != nil || !owned(fi, opts.UID, true) {
		return Extracted{}, ErrWorkDir
	}
	dir, err := os.MkdirTemp(opts.WorkDir, verifyPrefix)
	if err != nil {
		return Extracted{}, unreadable("a directory cannot be created in the work directory")
	}
	e, err := extract(ctx, contextReader{ctx: ctx, r: r}, dir, opts)
	if err != nil {
		return Extracted{}, errors.Join(err, os.RemoveAll(dir))
	}
	e.Dir = dir
	return e, nil
}

func extract(ctx context.Context, r io.Reader, dir string, opts ExtractOptions) (Extracted, error) {
	var e Extracted
	tr := tar.NewReader(r)
	for i, name := range opts.Names {
		h, err := next(ctx, tr, name, maxMemberBytes)
		if err != nil {
			return e, err
		}
		free, err := opts.Free(dir)
		if err != nil {
			return e, unreadable("the free space of the work directory cannot be read")
		}
		if h.Size < 0 || uint64(h.Size) > free {
			return e, unreadable("the work directory has too little free space for a member")
		}
		path := filepath.Join(dir, memberPrefix+strconv.Itoa(i))
		if err := writeMember(ctx, path, tr, h.Size); err != nil {
			return e, err
		}
		e.Members[i] = Member{Path: path, Bytes: h.Size}
	}
	h, err := next(ctx, tr, manifestName, maxManifestBytes)
	if err != nil {
		return e, err
	}
	body, err := io.ReadAll(tr)
	if err != nil || int64(len(body)) != h.Size {
		return e, cut(ctx, "the stream ended inside the manifest")
	}
	m, err := readManifest(body, opts.Names, e.Members)
	if err != nil {
		return e, err
	}
	e.Format, e.Version, e.Created = m.Format, m.Version, m.created
	for i, db := range m.Databases {
		e.Members[i].SchemaVersion = db.SchemaVersion
	}
	switch _, err := tr.Next(); {
	case err == nil:
		return e, rejected("a member follows the manifest")
	case errors.Is(err, tar.ErrHeader):
		return e, rejected("data follows the manifest")
	case !errors.Is(err, io.EOF):
		return e, cut(ctx, "the stream ended before the end of the archive")
	}
	if err := trailing(ctx, r); err != nil {
		return e, err
	}
	return e, nil
}

func next(ctx context.Context, tr *tar.Reader, name string, limit int64) (*tar.Header, error) {
	h, err := tr.Next()
	switch {
	case errors.Is(err, io.EOF):
		return nil, cut(ctx, "the stream ended before every member was read")
	case errors.Is(err, tar.ErrHeader):
		return nil, rejected("the stream holds an invalid tar header")
	case err != nil:
		return nil, cut(ctx, "the stream ended inside a tar header")
	case h.Typeflag != tar.TypeReg:
		return nil, rejected("a member is not a regular file")
	case h.Name != name:
		return nil, rejected("a member is missing, repeated, out of order or wrongly named")
	case h.Size < 0 || h.Size > limit:
		return nil, rejected("a member is larger than a backup holds")
	}
	return h, nil
}

func writeMember(ctx context.Context, path string, r io.Reader, size int64) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return unreadable("a member cannot be written to the work directory")
	}
	n, err := io.Copy(f, r)
	if err := errors.Join(err, f.Close()); err != nil || n != size {
		return cut(ctx, "the stream ended inside a member, or the member could not be written")
	}
	return nil
}

type checkedManifest struct {
	manifest
	created time.Time
}

func readManifest(body []byte, names [2]string, members [2]Member) (checkedManifest, error) {
	var m checkedManifest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m.manifest); err != nil || dec.More() {
		return m, rejected("the manifest is not valid")
	}
	if m.Format != manifestFormat {
		return m, rejected("the manifest has an unknown format")
	}
	created, err := time.Parse(time.RFC3339, m.Created)
	if err != nil {
		return m, rejected("the manifest has no valid creation time")
	}
	m.created = created.UTC()
	if len(m.Databases) != len(names) {
		return m, rejected("the manifest does not list both databases")
	}
	for i, db := range m.Databases {
		if db.Name != names[i] || db.Bytes != members[i].Bytes {
			return m, rejected("the manifest does not match the members")
		}
	}
	return m, nil
}

func trailing(ctx context.Context, r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if slices.ContainsFunc(buf[:n], func(c byte) bool { return c != 0 }) {
			return rejected("data follows the end of the archive")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cut(ctx, "the stream could not be read to its end")
		}
	}
}

func Keep(e Extracted, dir string, uid int, names [2]string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return ErrKeep
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; this clears the setgid bit Linux copies from a setgid parent
		return ErrKeep
	}
	if fi, err := os.Lstat(dir); err != nil || !owned(fi, uid, true) {
		return ErrKeep
	}
	for i, m := range e.Members {
		if err := os.Rename(m.Path, filepath.Join(dir, names[i])); err != nil {
			return ErrKeep
		}
	}
	if err := syncDir(dir); err != nil {
		return ErrKeep
	}
	return nil
}

func unreadable(why string) error { return &verifyError{kind: ErrUnreadable, why: why} }

func rejected(why string) error { return &verifyError{kind: ErrRejected, why: why} }

func cut(ctx context.Context, why string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return unreadable(why)
}

type verifyError struct {
	kind error
	why  string
}

func (e *verifyError) Error() string { return e.kind.Error() + ": " + e.why }

func (e *verifyError) Unwrap() error { return e.kind }
