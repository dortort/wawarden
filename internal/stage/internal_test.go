package stage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func freeSpace(free uint64) func(string, *syscall.Statfs_t) error {
	return func(_ string, st *syscall.Statfs_t) error {
		st.Bavail = free
		st.Bsize = 1
		return nil
	}
}

func testSpool(t testing.TB, data string, local bool, minFree, free uint64) *Spool {
	t.Helper()
	s, err := New(Options{Data: data, Local: local, MinFree: minFree, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.statfs = freeSpace(free)
	return s
}

func testAdmit(t testing.TB, s *Spool, declared int64) (*Sealed, context.Context) {
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

func TestAdmitArithmetic(t *testing.T) {
	const declared = 20 << 20
	const need = declared + overhead
	for _, c := range []struct {
		name    string
		local   bool
		minFree uint64
		free    uint64
		want    error
	}{
		{"local keeps a quarter more than the floor", true, 256 << 20, need + 320<<20, nil},
		{"local one byte short of that", true, 256 << 20, need + 320<<20 - 1, ErrSpace},
		{"local rounds the quarter down", true, 7, need + 8, nil},
		{"local one byte short of a small floor", true, 7, need + 7, ErrSpace},
		{"local with no floor", true, 0, need, nil},
		{"local with no floor one byte short", true, 0, need - 1, ErrSpace},
		{"local with a floor whose quarter saturates", true, math.MaxUint64 - 1, math.MaxUint64, ErrSpace},
		{"network keeps 256 MiB", false, 0, need + 256<<20, nil},
		{"network one byte short of that", false, 0, need + 256<<20 - 1, ErrSpace},
		{"network ignores the floor", false, 1 << 40, need + 256<<20, nil},
		{"less free than the declared length", true, 0, 10, ErrSpace},
		{"nothing free on the network profile", false, 0, 0, ErrSpace},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testSpool(t, t.TempDir(), c.local, c.minFree, c.free)
			f, err := s.Admit(declared, func(error) {})
			if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("Admit with %d free = %v, want %v", c.free, err, c.want)
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					t.Fatalf("the admission refusal %v reads as a cancellation", err)
				}
				return
			}
			f.Erase()
		})
	}
}

func TestDeclaredLengths(t *testing.T) {
	s := testSpool(t, t.TempDir(), true, 0, math.MaxUint64)
	for _, declared := range []int64{-1, 0, maxDeclared + 1, math.MaxInt64} {
		if _, err := s.Admit(declared, func(error) {}); !errors.Is(err, errDeclared) {
			t.Fatalf("Admit(%d) = %v, want the declared-length refusal", declared, err)
		}
	}
	f, _ := testAdmit(t, s, maxDeclared)
	sectors := (maxDeclared + overhead + sectorSize - 1) / sectorSize
	if len(f.gens) != sectors || len(f.tags) != sectors*tagSize {
		t.Fatalf("the sector tables hold %d generations and %d tag bytes, want %d and %d", len(f.gens), len(f.tags), sectors, sectors*tagSize)
	}
	if ram := len(f.gens)*4 + len(f.tags); ram > 1400<<10 {
		t.Fatalf("the sector tables take %d bytes at the largest declared length", ram)
	}
}

func TestInspectionFailures(t *testing.T) {
	data := t.TempDir()
	s := testSpool(t, data, true, 0, 0)
	s.statfs = func(string, *syscall.Statfs_t) error {
		return &os.PathError{Op: "statfs", Path: data, Err: syscall.EIO}
	}
	_, err := s.Admit(10, func(error) {})
	if !errors.Is(err, syscall.EIO) || errors.Is(err, context.Canceled) || strings.Contains(err.Error(), data) {
		t.Fatalf("Admit with a failing statfs = %v, want the errno under a text that names no path", err)
	}
	s.statfs = freeSpace(math.MaxUint64)
	testAdmit(t, s, 10)
}

type failingReader struct{ left int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, errors.New("synthetic: no randomness")
	}
	n := min(len(p), r.left)
	r.left -= n
	return rand.Read(p[:n])
}

func TestRandomnessFailuresFailClosed(t *testing.T) {
	for _, left := range []int{0, keyBytes, keyBytes + nameBytes - 1} {
		data := t.TempDir()
		s := testSpool(t, data, false, 0, math.MaxUint64)
		s.random = &failingReader{left: left}
		if _, err := s.Admit(10, func(error) {}); !errors.Is(err, errRandom) {
			t.Fatalf("Admit with %d random bytes = %v, want the randomness refusal", left, err)
		}
		if list, err := os.ReadDir(filepath.Join(data, Dir)); err != nil || len(list) != 0 {
			t.Fatalf("a refused admission left %d entries (%v)", len(list), err)
		}
		s.random = rand.Reader
		testAdmit(t, s, 10)
	}
	if _, err := newAEAD(make([]byte, 7)); err == nil {
		t.Fatal("a seven-byte key set up a cipher")
	}
}

func TestGenerationOverflowIsRefused(t *testing.T) {
	s := testSpool(t, t.TempDir(), true, 0, math.MaxUint64)
	for _, op := range []func(*Sealed) error{
		func(f *Sealed) error {
			_, err := f.WriteAt([]byte{1}, 0)
			return err
		},
		func(f *Sealed) error { return f.Truncate(5) },
	} {
		f, ctx := testAdmit(t, s, 100)
		f.gens[0] = math.MaxUint32 - 1
		if _, err := f.Write(bytes.Repeat([]byte{9}, 10)); err != nil || f.gens[0] != math.MaxUint32 {
			t.Fatalf("sealing under the last generation: %v", err)
		}
		err := op(f)
		if !errors.Is(err, errGeneration) || !errors.Is(err, context.Canceled) || !errors.Is(context.Cause(ctx), errGeneration) {
			t.Fatalf("a rewrite past the last generation = %v (cause %v), want the generation refusal", err, context.Cause(ctx))
		}
		if f.gens[0] != math.MaxUint32 {
			t.Fatalf("the refused rewrite moved the generation to %d", f.gens[0])
		}
		f.Erase()
	}
}

func TestDiskErrorsKeepOnlyTheErrno(t *testing.T) {
	data := t.TempDir()
	s := testSpool(t, data, false, 0, math.MaxUint64)
	for _, c := range []struct {
		name string
		flag int
		op   func(*Sealed) error
	}{
		{"a write", os.O_RDONLY, func(f *Sealed) error {
			_, err := f.WriteAt([]byte{1}, 0)
			return err
		}},
		{"a read", os.O_WRONLY, func(f *Sealed) error {
			_, err := f.ReadAt(make([]byte, 1), 0)
			return err
		}},
		{"a truncation", os.O_RDONLY, func(f *Sealed) error { return f.Truncate(sectorSize) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, ctx := testAdmit(t, s, 3*sectorSize)
			if _, err := f.Write(make([]byte, 2*sectorSize)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			swapped, err := os.OpenFile(filepath.Join(s.dir, f.name), c.flag, 0)
			if err != nil {
				t.Fatalf("reopening the spool file: %v", err)
			}
			_ = f.f.Close()
			f.f = swapped
			err = c.op(f)
			var errno syscall.Errno
			if !errors.As(err, &errno) || errno == 0 || !errors.Is(err, context.Canceled) || !errors.Is(context.Cause(ctx), errno) {
				t.Fatalf("%s through a misopened handle = %v (cause %v), want a cancelling errno", c.name, err, context.Cause(ctx))
			}
			if strings.Contains(err.Error(), data) || strings.Contains(err.Error(), f.name) {
				t.Fatalf("the disk error %q names the spool path", err)
			}
			f.Erase()
		})
	}
}

func TestAdmissionWithoutACancelFunction(t *testing.T) {
	s := testSpool(t, t.TempDir(), true, 0, math.MaxUint64)
	f, err := s.Admit(10, nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer f.Erase()
	if _, err := f.Write(make([]byte, 43)); !errors.Is(err, ErrLimit) {
		t.Fatalf("a write past the limit = %v, want the limit refusal", err)
	}
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, ErrLimit) {
		t.Fatalf("a read after the refusal = %v, want the limit refusal", err)
	}
}

func TestPurgeRemovalFailure(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, Dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, namePrefix+strings.Repeat("ab", nameBytes)), nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: the failure under test needs a directory its owner cannot write
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: the temporary directory's removal needs its owner's write bit back
	n, err := Purge(data)
	if os.Geteuid() == 0 {
		if n != 1 || err != nil {
			t.Fatalf("Purge as root = %d, %v, want the file removed, as root may write any directory", n, err)
		}
		return
	}
	if n != 0 || err == nil || errors.Is(err, context.Canceled) || strings.Contains(err.Error(), data) {
		t.Fatalf("Purge of a read-only directory = %d, %v, want a refusal that names no path", n, err)
	}
}
