package stage_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dortort/wawarden/internal/stage"
)

const (
	marker     = "WAWARDEN-PLAINTEXT-MARKER-0123456789"
	macLength  = 10
	sectorSize = 32 << 10
	padding    = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10"
)

type file interface {
	io.Reader
	io.Writer
	io.Seeker
	io.ReaderAt
	io.WriterAt
	Truncate(size int64) error
	Stat() (os.FileInfo, error)
}

var _ file = (*stage.Sealed)(nil)

type media struct {
	plain     []byte
	body      []byte
	iv        []byte
	cipherKey []byte
	macKey    []byte
}

func encrypt(t testing.TB, size int) media {
	t.Helper()
	iv, cipherKey, macKey := sha256.Sum256([]byte("synthetic iv")), sha256.Sum256([]byte("synthetic cipher key")), sha256.Sum256([]byte("synthetic mac key"))
	m := media{plain: bytes.Repeat([]byte(marker), size/len(marker)+1)[:size], iv: iv[:aes.BlockSize], cipherKey: cipherKey[:], macKey: macKey[:]}
	pad := aes.BlockSize - size%aes.BlockSize
	m.body = append(append(make([]byte, 0, size+pad+macLength), m.plain...), bytes.Repeat([]byte{padding[pad]}, pad)...)
	block, err := aes.NewCipher(m.cipherKey)
	if err != nil {
		t.Fatalf("the test cipher could not be set up: %v", err)
	}
	cipher.NewCBCEncrypter(block, m.iv).CryptBlocks(m.body, m.body)
	mac := hmac.New(sha256.New, m.macKey)
	mac.Write(m.iv)
	mac.Write(m.body)
	m.body = append(m.body, mac.Sum(nil)[:macLength]...)
	return m
}

type chunks struct {
	r   io.Reader
	rng *rand.Rand
}

func (c chunks) Read(p []byte) (int, error) { return c.r.Read(p[:1+c.rng.IntN(len(p))]) }

func newSpool(t testing.TB, local bool) (*stage.Spool, string) {
	t.Helper()
	data := t.TempDir()
	s, err := stage.New(stage.Options{Data: data, Local: local, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, filepath.Join(data, stage.Dir)
}

func admit(t testing.TB, s *stage.Spool, declared int64) (*stage.Sealed, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	f, err := s.Admit(declared, cancel)
	if err != nil {
		t.Fatalf("Admit(%d): %v", declared, err)
	}
	t.Cleanup(f.Erase)
	return f, ctx
}

func entries(t testing.TB, dir string) []fs.DirEntry {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the staging directory could not be listed: %v", err)
	}
	return list
}

func raw(t testing.TB, dir string) []byte {
	t.Helper()
	list := entries(t, dir)
	if len(list) != 1 {
		t.Fatalf("the staging directory holds %d entries, want the one spool file", len(list))
	}
	b, err := os.ReadFile(filepath.Clean(filepath.Join(dir, list[0].Name())))
	if err != nil {
		t.Fatalf("the spool file could not be read from disk: %v", err)
	}
	return b
}

func fetch(t testing.TB, f file, m media, failures int) int64 {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a seeded generator makes the chunk sizes reproducible
	whole := sha256.Sum256(m.body)
	for try := 0; ; try++ {
		src := io.Reader(bytes.NewReader(m.body))
		if try < failures {
			src = io.LimitReader(src, int64(len(m.body)*(try+1)/(failures+1)))
		}
		hasher := sha256.New()
		n, err := io.Copy(f, io.TeeReader(chunks{src, rng}, hasher))
		if err != nil {
			t.Fatalf("copying the body into the spool (attempt %d): %v", try, err)
		}
		if try == failures {
			if !bytes.Equal(hasher.Sum(nil), whole[:]) {
				t.Fatal("the body's hash differs after the copy")
			}
			return n
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("seeking back for a retry: %v", err)
		}
	}
}

func verify(t testing.TB, f file, m media, n int64) {
	t.Helper()
	mac := make([]byte, macLength)
	if _, err := f.ReadAt(mac, n-macLength); err != nil {
		t.Fatalf("reading the MAC: %v", err)
	}
	if err := f.Truncate(n - macLength); err != nil {
		t.Fatalf("truncating the MAC: %v", err)
	}
	h := hmac.New(sha256.New, m.macKey)
	h.Write(m.iv)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seeking before the MAC pass: %v", err)
	}
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("the MAC pass: %v", err)
	}
	if !hmac.Equal(h.Sum(nil)[:macLength], mac) {
		t.Fatal("the MAC does not match the staged ciphertext")
	}
}

func decrypt(f file, key, iv []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	cbc := cipher.NewCBCDecrypter(block, iv)
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size%aes.BlockSize != 0 {
		return fmt.Errorf("a staged size of %d is not a multiple of the block size", size)
	}
	bufSize := min(int64(sectorSize), size)
	buf := make([]byte, bufSize)
	var at int64
	var last byte
	for at < size {
		if at+bufSize > size {
			buf = buf[:size-at]
		}
		if _, err := io.ReadFull(f, buf); err != nil {
			return err
		}
		cbc.CryptBlocks(buf, buf)
		if n, err := f.WriteAt(buf, at); err != nil || n != len(buf) {
			return fmt.Errorf("an in-place write of %d bytes wrote %d: %w", len(buf), n, err)
		}
		at += int64(len(buf))
		last = buf[len(buf)-1]
	}
	if int64(last) > size {
		return errors.New("the padding is longer than the staged size")
	}
	return f.Truncate(size - int64(last))
}

func open(t testing.TB, f file, m media) {
	t.Helper()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seeking before decryption: %v", err)
	}
	if err := decrypt(f, m.cipherKey, m.iv); err != nil {
		t.Fatalf("decrypting in place: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seeking before the hash pass: %v", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		t.Fatalf("the plaintext hash pass: %v", err)
	}
	want := sha256.Sum256(m.plain)
	if !bytes.Equal(hasher.Sum(nil), want[:]) {
		t.Fatal("the plaintext hash differs after decryption")
	}
	buf := make([]byte, 1<<20)
	for at := 0; at < len(m.plain); at += len(buf) {
		n, err := f.ReadAt(buf, int64(at))
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("reading back at %d: %v", at, err)
		}
		if !bytes.Equal(buf[:n], m.plain[at:min(at+len(buf), len(m.plain))]) {
			t.Fatalf("reading back at %d returned other bytes", at)
		}
	}
}

func requireRefused(t testing.TB, ctx context.Context, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want %v", err, want)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the refusal %v does not read as a cancellation", err)
	}
	if !errors.Is(context.Cause(ctx), want) {
		t.Fatalf("the job context ended with cause %v, want %v", context.Cause(ctx), want)
	}
}

func TestReplayOfTheLibraryAccessSequence(t *testing.T) {
	type replay struct {
		size     int
		failures int
		local    bool
	}
	var cases []replay
	for _, c := range []struct{ size, failures int }{{1, 0}, {15, 0}, {16, 1}, {32758, 0}, {32768, 1}, {5<<20 + 7, 2}} {
		cases = append(cases, replay{c.size, c.failures, false}, replay{c.size, c.failures, true})
	}
	cases = append(cases, replay{64 << 20, 0, true})
	for _, c := range cases {
		t.Run(fmt.Sprintf("size %d, %d failed attempts, local %v", c.size, c.failures, c.local), func(t *testing.T) {
			t.Parallel()
			m := encrypt(t, c.size)
			s, dir := newSpool(t, c.local)
			f, ctx := admit(t, s, int64(c.size))
			n := fetch(t, f, m, c.failures)
			if n != int64(len(m.body)) || f.Size() != n {
				t.Fatalf("the copy staged %d bytes with a size of %d, want %d", n, f.Size(), len(m.body))
			}
			if c.local {
				if list := entries(t, dir); len(list) != 0 {
					t.Fatalf("the staging directory holds %d names on the local profile, want none", len(list))
				}
				verify(t, f, m, n)
				open(t, f, m)
				if err := context.Cause(ctx); err != nil {
					t.Fatalf("a valid replay ended the job: %v", err)
				}
				return
			}
			fetched := raw(t, dir)
			if len(fetched) != len(m.body) {
				t.Fatalf("the disk holds %d bytes for %d staged", len(fetched), len(m.body))
			}
			requireSealed(t, fetched, m)
			verify(t, f, m, n)
			open(t, f, m)
			final := raw(t, dir)
			if len(final) != c.size {
				t.Fatalf("the disk holds %d bytes for %d staged", len(final), c.size)
			}
			requireSealed(t, final, m)
			if c.size >= aes.BlockSize {
				span := min(c.size, sectorSize)
				diskPad, mediaPad := make([]byte, span), make([]byte, span)
				for i := range span {
					diskPad[i] = fetched[i] ^ final[i]
					mediaPad[i] = m.body[i] ^ m.plain[i]
				}
				if bytes.Equal(diskPad, mediaPad) {
					t.Fatal("the two generations of the first sector xor to the ciphertext xor the plaintext")
				}
			}
			if err := context.Cause(ctx); err != nil {
				t.Fatalf("a valid replay ended the job: %v", err)
			}
			tamper(t, dir, int64(c.size/2))
			_, err := f.ReadAt(make([]byte, c.size), 0)
			requireRefused(t, ctx, err, stage.ErrAuth)
		})
	}
}

func requireSealed(t testing.TB, disk []byte, m media) {
	t.Helper()
	if len(m.plain) >= len(marker) && bytes.Contains(disk, []byte(marker)) {
		t.Fatal("the disk holds the plaintext marker")
	}
	if bytes.Contains(disk, m.body[:min(64, len(m.body))]) {
		t.Fatal("the disk holds the media ciphertext as received")
	}
}

func tamper(t testing.TB, dir string, at int64) {
	t.Helper()
	list := entries(t, dir)
	if len(list) != 1 {
		t.Fatalf("the staging directory holds %d entries, want the one spool file", len(list))
	}
	disk, err := os.OpenFile(filepath.Clean(filepath.Join(dir, list[0].Name())), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("the spool file could not be opened for tampering: %v", err)
	}
	defer func() { _ = disk.Close() }()
	b := make([]byte, 1)
	if _, err := disk.ReadAt(b, at); err != nil {
		t.Fatalf("reading the byte to flip: %v", err)
	}
	b[0] ^= 1
	if _, err := disk.WriteAt(b, at); err != nil {
		t.Fatalf("flipping a byte on disk: %v", err)
	}
}

func TestATruncatedDiskFileFailsAuthentication(t *testing.T) {
	s, dir := newSpool(t, false)
	f, ctx := admit(t, s, 3*sectorSize)
	if _, err := f.Write(bytes.Repeat([]byte{0x5a}, 2*sectorSize+5)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.Truncate(filepath.Join(dir, entries(t, dir)[0].Name()), 2*sectorSize+1); err != nil {
		t.Fatalf("truncating the disk file behind the spool: %v", err)
	}
	if _, err := f.ReadAt(make([]byte, 16), sectorSize); err != nil {
		t.Fatalf("an intact sector failed to read: %v", err)
	}
	_, err := f.ReadAt(make([]byte, 4), 2*sectorSize)
	requireRefused(t, ctx, err, stage.ErrAuth)
}

func TestSwappedSectorsFailAuthentication(t *testing.T) {
	s, dir := newSpool(t, false)
	f, ctx := admit(t, s, 2*sectorSize)
	if _, err := f.Write(bytes.Repeat([]byte{0x5a}, 2*sectorSize)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	disk, err := os.OpenFile(filepath.Clean(filepath.Join(dir, entries(t, dir)[0].Name())), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("the spool file could not be opened for tampering: %v", err)
	}
	defer func() { _ = disk.Close() }()
	b := make([]byte, 2*sectorSize)
	if _, err := disk.ReadAt(b, 0); err != nil {
		t.Fatalf("reading the two sectors to swap: %v", err)
	}
	if _, err := disk.WriteAt(append(bytes.Clone(b[sectorSize:]), b[:sectorSize]...), 0); err != nil {
		t.Fatalf("swapping the two sectors on disk: %v", err)
	}
	_, err = f.ReadAt(make([]byte, 2*sectorSize), 0)
	requireRefused(t, ctx, err, stage.ErrAuth)
}

func TestAStaleOffsetTripsTheLimit(t *testing.T) {
	m := encrypt(t, 40000)
	s, _ := newSpool(t, true)
	f, ctx := admit(t, s, int64(len(m.plain)))
	fetch(t, f, m, 0)
	_, err := io.Copy(f, bytes.NewReader(m.body))
	requireRefused(t, ctx, err, stage.ErrLimit)
}

func TestRefusalsCancelTheJob(t *testing.T) {
	const declared, written = 1000, 100
	for _, c := range []struct {
		name string
		op   func(*stage.Sealed) error
		want error
	}{
		{"a write past the declared length", func(f *stage.Sealed) error {
			_, err := f.Write(make([]byte, declared+33-written))
			return err
		}, stage.ErrLimit},
		{"a write at the end of the declared length", func(f *stage.Sealed) error {
			_, err := f.WriteAt([]byte{1}, declared+32)
			return err
		}, stage.ErrLimit},
		{"a write at an offset that cannot be reached", func(f *stage.Sealed) error {
			_, err := f.WriteAt([]byte{1}, 1<<62)
			return err
		}, stage.ErrLimit},
		{"a write that leaves a hole", func(f *stage.Sealed) error {
			_, err := f.WriteAt([]byte{1}, written+1)
			return err
		}, stage.ErrHole},
		{"a write at a stale offset past the end", func(f *stage.Sealed) error {
			if _, err := f.Seek(written+1, io.SeekStart); err != nil {
				return err
			}
			_, err := f.Write([]byte{1})
			return err
		}, stage.ErrHole},
		{"a truncate that grows", func(f *stage.Sealed) error { return f.Truncate(written + 1) }, stage.ErrGrow},
		{"a negative write offset", func(f *stage.Sealed) error {
			_, err := f.WriteAt([]byte{1}, -1)
			return err
		}, stage.ErrOffset},
		{"a negative read offset", func(f *stage.Sealed) error {
			_, err := f.ReadAt(make([]byte, 1), -1)
			return err
		}, stage.ErrOffset},
		{"a seek before the start", func(f *stage.Sealed) error {
			_, err := f.Seek(-written-1, io.SeekEnd)
			return err
		}, stage.ErrOffset},
		{"a seek from an unknown origin", func(f *stage.Sealed) error {
			_, err := f.Seek(0, 3)
			return err
		}, stage.ErrOffset},
		{"a negative truncate", func(f *stage.Sealed) error { return f.Truncate(-1) }, stage.ErrOffset},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, dir := newSpool(t, true)
			f, ctx := admit(t, s, declared)
			if _, err := f.Write(bytes.Repeat([]byte{7}, written)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			err := c.op(f)
			requireRefused(t, ctx, err, c.want)
			if strings.Contains(err.Error(), dir) {
				t.Fatalf("the refusal %q names the staging directory", err)
			}
			_, readErr := f.ReadAt(make([]byte, 1), 0)
			_, writeErr := f.Write([]byte{1})
			_, seekErr := f.Seek(0, io.SeekStart)
			_, statErr := f.Stat()
			for i, later := range []error{readErr, writeErr, seekErr, statErr, f.Truncate(0)} {
				if !errors.Is(later, c.want) {
					t.Fatalf("operation %d after the refusal returned %v, want %v", i, later, c.want)
				}
			}
		})
	}
}

func TestPositionedAccessLeavesTheOffset(t *testing.T) {
	s, _ := newSpool(t, true)
	f, _ := admit(t, s, 3*sectorSize)
	if _, err := f.Write(bytes.Repeat([]byte{1}, sectorSize+10)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n, err := f.WriteAt(bytes.Repeat([]byte{2}, sectorSize), 5); err != nil || n != sectorSize {
		t.Fatalf("WriteAt = %d, %v", n, err)
	}
	if n, err := f.ReadAt(make([]byte, 20), 0); err != nil || n != 20 {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if at, err := f.Seek(0, io.SeekCurrent); err != nil || at != sectorSize+10 {
		t.Fatalf("the offset is %d (%v) after positioned access, want %d", at, err, sectorSize+10)
	}
	if n, err := f.Write([]byte{3, 3}); err != nil || n != 2 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	got := make([]byte, sectorSize+12)
	if n, err := f.ReadAt(got, 0); err != nil || n != len(got) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	want := bytes.Join([][]byte{bytes.Repeat([]byte{1}, 5), bytes.Repeat([]byte{2}, sectorSize), bytes.Repeat([]byte{1}, 5), {3, 3}}, nil)
	if !bytes.Equal(got, want) {
		t.Fatal("the spool does not hold the bytes written around the offset")
	}
}

func TestReadsAndStat(t *testing.T) {
	s, _ := newSpool(t, true)
	f, _ := admit(t, s, 100)
	if n, err := f.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read of an empty spool = %d, %v, want 0, EOF", n, err)
	}
	if _, err := f.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n, err := f.ReadAt(make([]byte, 4), 10); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt at the end = %d, %v, want 0, EOF", n, err)
	}
	if n, err := f.ReadAt(make([]byte, 4), 8); n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt across the end = %d, %v, want 2, EOF", n, err)
	}
	if _, err := f.Seek(-3, io.SeekEnd); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 8)
	if n, err := f.Read(buf); n != 3 || err != nil || string(buf[:n]) != "789" {
		t.Fatalf("Read across the end = %d %q, %v, want 3 \"789\", nil", n, buf[:n], err)
	}
	if at, err := f.Seek(-2, io.SeekCurrent); err != nil || at != 8 {
		t.Fatalf("Seek back = %d, %v", at, err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() != 10 || st.IsDir() || st.Mode() != 0o600 || st.Name() == "" || !st.ModTime().IsZero() || st.Sys() != nil {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	if _, ok := any(f).(io.ReaderFrom); ok {
		t.Fatal("the spool implements io.ReaderFrom, so io.Copy would bypass its Write")
	}
	if _, ok := any(f).(io.WriterTo); ok {
		t.Fatal("the spool implements io.WriterTo, so io.Copy would bypass its Read")
	}
}

func TestTruncateResealsAndTheSizeFollows(t *testing.T) {
	s, dir := newSpool(t, false)
	f, ctx := admit(t, s, 4*sectorSize)
	content := make([]byte, 3*sectorSize+100)
	for i := range content {
		content[i] = byte(i * 7)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, size := range []int64{3*sectorSize + 100, 2*sectorSize + 17, 2 * sectorSize, sectorSize - 1, 0} {
		if err := f.Truncate(size); err != nil {
			t.Fatalf("Truncate(%d): %v", size, err)
		}
		if f.Size() != size {
			t.Fatalf("Size after Truncate(%d) = %d", size, f.Size())
		}
		if disk := raw(t, dir); int64(len(disk)) != size {
			t.Fatalf("the disk holds %d bytes after Truncate(%d)", len(disk), size)
		}
		got := make([]byte, size)
		if n, err := f.ReadAt(got, 0); int64(n) != size || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("ReadAt after Truncate(%d) = %d, %v", size, n, err)
		}
		if !bytes.Equal(got, content[:size]) {
			t.Fatalf("Truncate(%d) changed the bytes it kept", size)
		}
	}
	if _, err := f.WriteAt(content[:2*sectorSize+3], 0); err != nil {
		t.Fatalf("rewriting after the truncation: %v", err)
	}
	got := make([]byte, 2*sectorSize+3)
	if _, err := f.ReadAt(got, 0); err != nil || !bytes.Equal(got, content[:len(got)]) {
		t.Fatalf("reading the rewrite back: %v", err)
	}
	if err := context.Cause(ctx); err != nil {
		t.Fatalf("truncation ended the job: %v", err)
	}
}

func TestEraseOnTheLocalProfile(t *testing.T) {
	s, dir := newSpool(t, true)
	f, ctx := admit(t, s, 2*sectorSize)
	if list := entries(t, dir); len(list) != 0 {
		t.Fatalf("the staging directory holds %d names after admission on the local profile", len(list))
	}
	if _, err := f.Write(bytes.Repeat([]byte(marker), 1000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.Erase()
	f.Erase()
	if list := entries(t, dir); len(list) != 0 {
		t.Fatalf("the staging directory holds %d entries after Erase", len(list))
	}
	requireErased(t, f)
	if err := context.Cause(ctx); err != nil {
		t.Fatalf("Erase ended the job with %v", err)
	}
	admit(t, s, 1)
}

func TestEraseOnTheNetworkProfile(t *testing.T) {
	s, dir := newSpool(t, false)
	f, _ := admit(t, s, 2*sectorSize)
	list := entries(t, dir)
	if len(list) != 1 || !list[0].Type().IsRegular() {
		t.Fatalf("the staging directory holds %v after admission on the network profile, want one file", list)
	}
	info, err := list[0].Info()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the spool file has mode %v (%v), want 0600", info.Mode(), err)
	}
	if _, err := f.Write([]byte(marker)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.Erase()
	if list := entries(t, dir); len(list) != 0 {
		t.Fatalf("the staging directory still holds %d entries after Erase", len(list))
	}
	requireErased(t, f)
}

func requireErased(t testing.TB, f *stage.Sealed) {
	t.Helper()
	_, readErr := f.ReadAt(make([]byte, 1), 0)
	_, writeErr := f.WriteAt([]byte{1}, 0)
	_, seekErr := f.Seek(0, io.SeekStart)
	_, statErr := f.Stat()
	for i, err := range []error{readErr, writeErr, seekErr, statErr, f.Truncate(0)} {
		if !errors.Is(err, stage.ErrErased) {
			t.Fatalf("operation %d after Erase returned %v, want the erased refusal", i, err)
		}
	}
	if f.Size() != 0 {
		t.Fatalf("Size after Erase = %d, want 0", f.Size())
	}
}

func TestConcurrentReadsAndErase(t *testing.T) {
	s, _ := newSpool(t, true)
	f, _ := admit(t, s, 3*sectorSize)
	content := bytes.Repeat([]byte(marker), 3*sectorSize/len(marker))
	if _, err := f.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	started := make(chan struct{}, len(results))
	for i := range results {
		wg.Go(func() {
			buf := make([]byte, 40000)
			for at := int64(i * 1000); ; at = (at + 4099) % int64(len(content)) {
				n, err := f.ReadAt(buf, at)
				if err != nil && !errors.Is(err, io.EOF) {
					results[i] = err
					return
				}
				if !bytes.Equal(buf[:n], content[at:at+int64(n)]) {
					results[i] = fmt.Errorf("a read at %d returned other bytes", at)
					return
				}
				select {
				case started <- struct{}{}:
				default:
				}
			}
		})
	}
	<-started
	f.Erase()
	wg.Wait()
	for i, err := range results {
		if !errors.Is(err, stage.ErrErased) {
			t.Fatalf("reader %d ended with %v, want the erased refusal", i, err)
		}
	}
}

func TestOneDiskObjectAtATime(t *testing.T) {
	s, _ := newSpool(t, true)
	f, _ := admit(t, s, 10)
	_, err := s.Admit(10, func(error) {})
	if !errors.Is(err, stage.ErrBusy) || errors.Is(err, context.Canceled) {
		t.Fatalf("a second admission returned %v, want the busy refusal", err)
	}
	f.Erase()
	admit(t, s, 10)
}

func TestTheStagingDirectory(t *testing.T) {
	t.Run("created at 0700 on first admission only", func(t *testing.T) {
		s, dir := newSpool(t, true)
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("New created the staging directory (%v)", err)
		}
		if n, err := s.Sweep(); n != 0 || err != nil {
			t.Fatalf("Sweep of an absent directory = %d, %v", n, err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Sweep created the staging directory (%v)", err)
		}
		admit(t, s, 1)
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("the staging directory is %v (%v), want a 0700 directory", fi, err)
		}
	})
	for _, c := range []struct {
		name string
		make func(t *testing.T, dir string)
		uid  int
	}{
		{"a group-readable directory", func(t *testing.T, dir string) { mkdir(t, dir, 0o750) }, os.Geteuid()},
		{"a symbolic link to a private directory", func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			mkdir(t, target, 0o700)
			if err := os.Symlink(target, dir); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		}, os.Geteuid()},
		{"a regular file", func(t *testing.T, dir string) {
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		}, os.Geteuid()},
		{"another user's directory", func(t *testing.T, dir string) { mkdir(t, dir, 0o700) }, os.Geteuid() + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := t.TempDir()
			dir := filepath.Join(data, stage.Dir)
			c.make(t, dir)
			s, err := stage.New(stage.Options{Data: data, Local: true, UID: c.uid})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := s.Admit(1, func(error) {}); !errors.Is(err, stage.ErrDirectory) {
				t.Fatalf("Admit = %v, want the directory refusal", err)
			}
		})
	}
	t.Run("a relative data directory", func(t *testing.T) {
		if _, err := stage.New(stage.Options{Data: "data", Local: true, UID: os.Geteuid()}); !errors.Is(err, stage.ErrDirectory) {
			t.Fatalf("New with a relative data directory = %v, want the directory refusal", err)
		}
		if _, err := stage.Purge(""); !errors.Is(err, stage.ErrDirectory) {
			t.Fatalf("Purge with an empty data directory = %v, want the directory refusal", err)
		}
	})
}

func mkdir(t testing.TB, dir string, mode fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

func write(t testing.TB, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

const (
	strayName = "sealed-00112233445566778899aabbccddeeff"
	otherName = "sealed-ffeeddccbbaa99887766554433221100"
)

func TestSweepKeepsTheLiveSpool(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprintf("local %v", local), func(t *testing.T) {
			s, dir := newSpool(t, local)
			f, ctx := admit(t, s, 3*sectorSize)
			if _, err := f.Write(bytes.Repeat([]byte(marker), 2000)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			live := entries(t, dir)
			write(t, filepath.Join(dir, strayName))
			write(t, filepath.Join(dir, otherName))
			for _, name := range []string{"notes.txt", "sealed-0011", "sealed-FEDCBA9876543210FEDCBA9876543210", strayName + ".bak"} {
				write(t, filepath.Join(dir, name))
			}
			mkdir(t, filepath.Join(dir, "sealed-0123456789abcdef0123456789abcdef"), 0o700)
			outside := filepath.Join(t.TempDir(), "outside")
			write(t, outside)
			if err := os.Symlink(outside, filepath.Join(dir, "sealed-abcdefabcdefabcdefabcdefabcdefab")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			n, err := s.Sweep()
			if n != 2 || err != nil {
				t.Fatalf("Sweep = %d, %v, want the two stray spool files", n, err)
			}
			if exists(filepath.Join(dir, strayName)) || exists(filepath.Join(dir, otherName)) {
				t.Fatal("Sweep kept a stray spool file")
			}
			for _, e := range live {
				if !exists(filepath.Join(dir, e.Name())) {
					t.Fatal("Sweep removed the live spool's name")
				}
			}
			if left := entries(t, dir); len(left) != len(live)+6 {
				t.Fatalf("Sweep left %d entries, want the live spool and six others", len(left))
			}
			if !exists(outside) {
				t.Fatal("Sweep removed a symbolic link's target")
			}
			got := make([]byte, len(marker))
			if _, err := f.ReadAt(got, sectorSize); err != nil || context.Cause(ctx) != nil {
				t.Fatalf("the live spool failed after the sweep: %v", err)
			}
		})
	}
}

func TestPurge(t *testing.T) {
	t.Run("an absent directory is left absent", func(t *testing.T) {
		data := t.TempDir()
		if n, err := stage.Purge(data); n != 0 || err != nil {
			t.Fatalf("Purge = %d, %v", n, err)
		}
		if exists(filepath.Join(data, stage.Dir)) {
			t.Fatal("Purge created the staging directory")
		}
	})
	t.Run("every regular file goes", func(t *testing.T) {
		data := t.TempDir()
		dir := filepath.Join(data, stage.Dir)
		mkdir(t, dir, 0o700)
		write(t, filepath.Join(dir, strayName))
		write(t, filepath.Join(dir, "notes.txt"))
		mkdir(t, filepath.Join(dir, "nested"), 0o700)
		write(t, filepath.Join(dir, "nested", "kept"))
		outside := filepath.Join(t.TempDir(), "outside")
		write(t, outside)
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		if n, err := stage.Purge(data); n != 2 || err != nil {
			t.Fatalf("Purge = %d, %v, want the two regular files", n, err)
		}
		if exists(filepath.Join(dir, strayName)) || exists(filepath.Join(dir, "notes.txt")) {
			t.Fatal("Purge kept a regular file")
		}
		if !exists(filepath.Join(dir, "nested", "kept")) || !exists(outside) {
			t.Fatal("Purge reached past the staging directory")
		}
	})
	t.Run("a symbolic link in its place is not followed", func(t *testing.T) {
		data := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		mkdir(t, target, 0o700)
		write(t, filepath.Join(target, strayName))
		if err := os.Symlink(target, filepath.Join(data, stage.Dir)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		n, err := stage.Purge(data)
		if n != 0 || err == nil || strings.Contains(err.Error(), data) {
			t.Fatalf("Purge through a symbolic link = %d, %v, want a refusal that names no path", n, err)
		}
		if !exists(filepath.Join(target, strayName)) {
			t.Fatal("Purge followed a symbolic link")
		}
	})
}
