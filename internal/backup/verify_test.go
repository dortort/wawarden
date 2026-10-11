package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"filippo.io/age"
)

var verifyNames = [2]string{"archive.db", "session.db"}

func plenty(string) (uint64, error) { return 1 << 40, nil }

func workDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	mkdirMode(t, dir, 0o700)
	return dir
}

func extractOptions(dir string) ExtractOptions {
	return ExtractOptions{WorkDir: dir, UID: os.Geteuid(), Names: verifyNames, Free: plenty}
}

func decrypted(t *testing.T, path string, id age.Identity) []byte {
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
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read the plaintext: %v", err)
	}
	return b
}

type tarMember struct {
	name     string
	typeflag byte
	body     []byte
	size     int64
}

func tarOf(t *testing.T, members ...tarMember) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	short := false
	for _, m := range members {
		h := header(m.name, int64(len(m.body)), epoch)
		if m.typeflag != 0 {
			h.Typeflag = m.typeflag
			if m.typeflag == tar.TypeSymlink {
				h.Linkname, h.Size = "/etc/passwd", 0
			}
		}
		if m.size != 0 {
			h.Size, short = m.size, true
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(m.body); err != nil && m.size == 0 {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil && !short {
		t.Fatalf("Close: %v", err)
	}
	return b.Bytes()
}

func manifestOf(t *testing.T, m manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

func goodManifest(archive, session []byte) manifest {
	return manifest{Format: 1, Version: "v0.0.0-test", Created: "2026-10-05T12:00:00Z", Databases: []entry{
		{Name: "archive.db", SchemaVersion: 4, Bytes: int64(len(archive))},
		{Name: "session.db", SchemaVersion: 16, Bytes: int64(len(session))},
	}}
}

func TestExtractReadsARealBackup(t *testing.T) {
	saved := syscall.Umask(0)
	defer syscall.Umask(saved)
	dir := t.TempDir()
	archive, sess := stores(t, dir)
	seedBlobs(t, archive, "blob", 20, 100)
	id := identity(t)
	tk, _ := taker(t, dir, id.Recipient().String(), archive, sess)
	name, err := tk.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	work := workDir(t)
	e, err := Extract(t.Context(), bytes.NewReader(decrypted(t, filepath.Join(dir, Dir, name), id)), extractOptions(work))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if filepath.Dir(e.Dir) != work || mode(t, e.Dir) != fs.ModeDir|0o700 {
		t.Fatalf("extracted into %s with mode %v", e.Dir, mode(t, e.Dir))
	}
	if e.Format != 1 || e.Version != "v0.0.0-test" || !e.Created.Equal(epoch) {
		t.Fatalf("manifest read as %+v", e)
	}
	for i, want := range []int{archive.SchemaVersion(), sess.SchemaVersion()} {
		m := e.Members[i]
		if m.SchemaVersion != want || filepath.Dir(m.Path) != e.Dir || mode(t, m.Path) != 0o600 {
			t.Fatalf("member %d = %+v with mode %v", i, m, mode(t, m.Path))
		}
		fi, err := os.Stat(m.Path)
		if err != nil || fi.Size() != m.Bytes {
			t.Fatalf("member %d holds %v bytes, want %d", i, fi, m.Bytes)
		}
	}
	keep := filepath.Join(t.TempDir(), "restored")
	if err := Keep(e, keep, os.Geteuid(), verifyNames); err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if got := entries(t, keep); !slices.Equal(got, verifyNames[:]) || mode(t, keep) != fs.ModeDir|0o700 || mode(t, filepath.Join(keep, "archive.db")) != 0o600 {
		t.Fatalf("kept %v with mode %v", got, mode(t, keep))
	}
	if err := Keep(e, keep, os.Geteuid(), verifyNames); !errors.Is(err, ErrKeep) {
		t.Fatalf("Keep into an existing directory = %v", err)
	}
	if err := os.RemoveAll(e.Dir); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, work); len(got) != 0 {
		t.Fatalf("the work directory holds %v", got)
	}
}

func TestExtractRefusesAnythingButACompleteBackup(t *testing.T) {
	archive, session := bytes.Repeat([]byte("archive page "), 1000), bytes.Repeat([]byte("session page "), 100)
	good := goodManifest(archive, session)
	with := func(change func(m *manifest)) []byte {
		m := good
		m.Databases = slices.Clone(good.Databases)
		change(&m)
		return manifestOf(t, m)
	}
	a, s := tarMember{name: "archive.db", body: archive}, tarMember{name: "session.db", body: session}
	man := tarMember{name: manifestName, body: manifestOf(t, good)}
	complete := tarOf(t, a, s, man)
	tests := []struct {
		name   string
		stream []byte
		want   error
	}{
		{name: "nothing", stream: nil, want: ErrUnreadable},
		{name: "not a tar", stream: bytes.Repeat([]byte("x"), 2048), want: ErrRejected},
		{name: "cut inside a member", stream: complete[:2000], want: ErrUnreadable},
		{name: "cut inside a header", stream: complete[:100], want: ErrUnreadable},
		{name: "cut before the manifest", stream: tarOf(t, a, s), want: ErrUnreadable},
		{name: "a wrongly named member", stream: tarOf(t, tarMember{name: "archive.sqlite", body: archive}, s, man), want: ErrRejected},
		{name: "a member in a directory", stream: tarOf(t, tarMember{name: "x/archive.db", body: archive}, s, man), want: ErrRejected},
		{name: "a member with a dot path", stream: tarOf(t, tarMember{name: "./archive.db", body: archive}, s, man), want: ErrRejected},
		{name: "the archive twice", stream: tarOf(t, a, a, man), want: ErrRejected},
		{name: "the members swapped", stream: tarOf(t, s, a, man), want: ErrRejected},
		{name: "a symbolic link", stream: tarOf(t, tarMember{name: "archive.db", typeflag: tar.TypeSymlink}, s, man), want: ErrRejected},
		{name: "a directory", stream: tarOf(t, tarMember{name: "archive.db", typeflag: tar.TypeDir}, s, man), want: ErrRejected},
		{name: "a header beyond any backup", stream: tarOf(t, tarMember{name: "archive.db", size: maxMemberBytes + 1}), want: ErrRejected},
		{name: "a manifest beyond its limit", stream: tarOf(t, a, s, tarMember{name: manifestName, body: bytes.Repeat([]byte(" "), maxManifestBytes+1)}), want: ErrRejected},
		{name: "a member after the manifest", stream: tarOf(t, a, s, man, a), want: ErrRejected},
		{name: "data after the archive", stream: append(slices.Clone(complete), "synthetic"...), want: ErrRejected},
		{name: "data in place of the end of the archive", stream: append(slices.Clone(complete[:len(complete)-1024]), bytes.Repeat([]byte("x"), 1024)...), want: ErrRejected},
		{name: "cut inside the end of the archive", stream: complete[:len(complete)-1024+100], want: ErrUnreadable},
		{name: "a manifest that is not JSON", stream: tarOf(t, a, s, tarMember{name: manifestName, body: []byte("{")}), want: ErrRejected},
		{name: "a manifest with an unknown field", stream: tarOf(t, a, s, tarMember{name: manifestName, body: []byte(`{"format":1,"extra":true}`)}), want: ErrRejected},
		{name: "a manifest of another format", stream: tarOf(t, a, s, tarMember{name: manifestName, body: with(func(m *manifest) { m.Format = 2 })}), want: ErrRejected},
		{name: "a manifest without a time", stream: tarOf(t, a, s, tarMember{name: manifestName, body: with(func(m *manifest) { m.Created = "" })}), want: ErrRejected},
		{name: "a manifest with one database", stream: tarOf(t, a, s, tarMember{name: manifestName, body: with(func(m *manifest) { m.Databases = m.Databases[:1] })}), want: ErrRejected},
		{name: "a manifest with another size", stream: tarOf(t, a, s, tarMember{name: manifestName, body: with(func(m *manifest) { m.Databases[1].Bytes++ })}), want: ErrRejected},
		{name: "a manifest with another name", stream: tarOf(t, a, s, tarMember{name: manifestName, body: with(func(m *manifest) { m.Databases[0].Name = "session.db" })}), want: ErrRejected},
		{name: "a manifest followed by a second value", stream: tarOf(t, a, s, tarMember{name: manifestName, body: append(manifestOf(t, good), " {}"...)}), want: ErrRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := workDir(t)
			e, err := Extract(t.Context(), bytes.NewReader(tt.stream), extractOptions(work))
			if !errors.Is(err, tt.want) || e.Dir != "" {
				t.Fatalf("Extract = %+v, %v, want %v", e, err, tt.want)
			}
			if got := entries(t, work); len(got) != 0 {
				t.Fatalf("the work directory holds %v after a refusal", got)
			}
		})
	}
	work := workDir(t)
	e, err := Extract(t.Context(), bytes.NewReader(complete), extractOptions(work))
	if err != nil || e.Members[0].SchemaVersion != 4 || e.Members[1].SchemaVersion != 16 {
		t.Fatalf("the complete stream = %+v, %v", e, err)
	}
	padded := append(slices.Clone(complete), make([]byte, 10240)...)
	if _, err := Extract(t.Context(), bytes.NewReader(padded), extractOptions(workDir(t))); err != nil {
		t.Fatalf("a stream padded with zero blocks = %v", err)
	}
	opts := extractOptions(workDir(t))
	opts.Free = func(string) (uint64, error) { return uint64(len(archive)) - 1, nil }
	if _, err := Extract(t.Context(), bytes.NewReader(complete), opts); !errors.Is(err, ErrUnreadable) || len(entries(t, opts.WorkDir)) != 0 {
		t.Fatalf("a member larger than the free space = %v", err)
	}
}

func TestExtractRefusesAWorkDirectoryThatIsNotPrivate(t *testing.T) {
	base := t.TempDir()
	target := workDir(t)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, "open")
	mkdirMode(t, open, 0o755)
	for _, dir := range []string{filepath.Join(base, "missing"), link, file, open} {
		if _, err := Extract(t.Context(), bytes.NewReader(nil), extractOptions(dir)); !errors.Is(err, ErrWorkDir) {
			t.Errorf("Extract into %s = %v", filepath.Base(dir), err)
		}
	}
	opts := extractOptions(workDir(t))
	opts.UID = os.Geteuid() + 1
	if _, err := Extract(t.Context(), bytes.NewReader(nil), opts); !errors.Is(err, ErrWorkDir) {
		t.Errorf("Extract into another user's directory = %v", err)
	}
	if got := entries(t, target); len(got) != 0 {
		t.Fatalf("the link's target holds %v", got)
	}
}

type cancelling struct {
	r      io.Reader
	after  int
	cancel context.CancelFunc
}

func (c *cancelling) Read(p []byte) (int, error) {
	if c.after <= 0 {
		c.cancel()
	}
	n, err := c.r.Read(p[:min(len(p), 512)])
	c.after -= n
	return n, err
}

func TestAStopDuringExtractLeavesNothing(t *testing.T) {
	archive, session := bytes.Repeat([]byte("archive page "), 1000), bytes.Repeat([]byte("session page "), 100)
	stream := tarOf(t, tarMember{name: "archive.db", body: archive}, tarMember{name: "session.db", body: session},
		tarMember{name: manifestName, body: manifestOf(t, goodManifest(archive, session))})
	work := workDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := Extract(ctx, &cancelling{r: bytes.NewReader(stream), after: 4096, cancel: cancel}, extractOptions(work))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Extract = %v, want the cancellation", err)
	}
	if got := entries(t, work); len(got) != 0 {
		t.Fatalf("the work directory holds %v after a stop", got)
	}
}
