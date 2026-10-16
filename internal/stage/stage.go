// Package stage owns the staging directory of the data directory: it admits one disk spool at a time and seals every 32 KiB sector of it with AES-256-GCM under a key held only in memory.
package stage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const Dir = "spool"

const (
	overhead    = 32
	maxDeclared = 2 << 30
	networkKeep = 256 << 20
	namePrefix  = "sealed-"
	nameBytes   = 16
	keyBytes    = 32
)

type failure struct {
	text     string
	errno    syscall.Errno
	canceled bool
}

func (e *failure) Error() string { return e.text }

func (e *failure) Is(target error) bool { return e.canceled && target == context.Canceled }

func (e *failure) Unwrap() error {
	if e.errno == 0 {
		return nil
	}
	return e.errno
}

func fault(text string, err error, canceled bool) error {
	f := &failure{text: text, canceled: canceled}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		f.errno = errno
	}
	return f
}

var (
	ErrBusy      error = &failure{text: "stage: another disk spool is in use"}
	ErrSpace     error = &failure{text: "stage: not enough free space to stage on disk"}
	ErrDirectory error = &failure{text: "stage: the staging directory is not a private directory of this user"}
	errDeclared  error = &failure{text: "stage: the declared length is out of range"}
	errRandom    error = &failure{text: "stage: no random bytes for a disk spool"}
	errCipher    error = &failure{text: "stage: the spool cipher could not be set up"}
)

type Options struct {
	Data    string
	Local   bool
	MinFree uint64
	UID     int
}

type Spool struct {
	mu     sync.Mutex
	dir    string
	local  bool
	keep   uint64
	uid    int
	statfs func(string, *syscall.Statfs_t) error
	random io.Reader
	live   *Sealed
}

func New(o Options) (*Spool, error) {
	dir, err := spoolDir(o.Data)
	if err != nil {
		return nil, err
	}
	keep := uint64(networkKeep)
	if o.Local {
		keep = resumeAt(o.MinFree)
	}
	return &Spool{dir: dir, local: o.Local, keep: keep, uid: o.UID, statfs: syscall.Statfs, random: rand.Reader}, nil
}

func spoolDir(data string) (string, error) {
	if !filepath.IsAbs(data) {
		return "", ErrDirectory
	}
	return filepath.Join(data, Dir), nil
}

func resumeAt(floor uint64) uint64 {
	if floor > math.MaxUint64-floor/4 {
		return math.MaxUint64
	}
	return floor + floor/4
}

func (s *Spool) Admit(declared int64, cancel context.CancelCauseFunc) (*Sealed, error) {
	if declared < 1 || declared > maxDeclared {
		return nil, errDeclared
	}
	limit := declared + overhead
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live != nil {
		return nil, ErrBusy
	}
	if err := s.prepare(); err != nil {
		return nil, err
	}
	var st syscall.Statfs_t
	if err := s.statfs(s.dir, &st); err != nil {
		return nil, fault("stage: the staging directory's filesystem could not be inspected", err, false)
	}
	if free, need := availableBytes(&st), uint64(limit); free < need || free-need < s.keep {
		return nil, ErrSpace
	}
	f, err := s.create(limit, cancel)
	if err != nil {
		return nil, err
	}
	s.live = f
	return f, nil
}

func (s *Spool) prepare() error {
	if err := os.Mkdir(s.dir, 0o700); err == nil {
		if err := os.Chmod(s.dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; this clears the setgid bit Linux copies from a setgid parent
			return fault("stage: the staging directory could not be restricted", err, false)
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return fault("stage: the staging directory could not be created", err, false)
	}
	fi, err := os.Lstat(s.dir)
	if err != nil {
		return fault("stage: the staging directory could not be inspected", err, false)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0o700 || !ok || int(st.Uid) != s.uid {
		return ErrDirectory
	}
	return nil
}

func (s *Spool) create(limit int64, cancel context.CancelCauseFunc) (*Sealed, error) {
	var key [keyBytes]byte
	defer clear(key[:])
	var id [nameBytes]byte
	if _, err := io.ReadFull(s.random, key[:]); err != nil {
		return nil, errRandom
	}
	if _, err := io.ReadFull(s.random, id[:]); err != nil {
		return nil, errRandom
	}
	aead, err := newAEAD(key[:])
	if err != nil {
		return nil, errCipher
	}
	name := namePrefix + hex.EncodeToString(id[:])
	path := filepath.Join(s.dir, name)
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fault("stage: the spool file could not be created", err, false)
	}
	if s.local {
		if err := os.Remove(path); err != nil {
			_ = f.Close()
			return nil, fault("stage: the spool file could not be unlinked", err, false)
		}
	}
	sectors := (limit + sectorSize - 1) / sectorSize
	return &Sealed{
		spool:  s,
		name:   name,
		kept:   !s.local,
		f:      f,
		aead:   aead,
		tags:   make([]byte, sectors*tagSize),
		gens:   make([]uint32, sectors),
		sec:    make([]byte, sectorSize),
		ct:     make([]byte, sectorSize+tagSize),
		limit:  limit,
		cancel: cancel,
	}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Spool) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = nil
}

func (s *Spool) Sweep() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := ""
	if s.live != nil {
		live = s.live.name
	}
	return remove(s.dir, func(name string) bool { return name != live && spoolName(name) })
}

func Purge(data string) (int, error) {
	dir, err := spoolDir(data)
	if err != nil {
		return 0, err
	}
	return remove(dir, func(string) bool { return true })
}

func spoolName(name string) bool {
	id, ok := strings.CutPrefix(name, namePrefix)
	if !ok || len(id) != 2*nameBytes {
		return false
	}
	return strings.Trim(id, "0123456789abcdef") == ""
}

func remove(dir string, match func(string) bool) (int, error) {
	d, err := os.OpenFile(filepath.Clean(dir), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fault("stage: the staging directory could not be opened", err, false)
	}
	entries, err := d.ReadDir(-1)
	if err = errors.Join(err, d.Close()); err != nil {
		return 0, fault("stage: the staging directory could not be listed", err, false)
	}
	removed := 0
	var failed error
	for _, e := range entries {
		if !e.Type().IsRegular() || !match(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		} else if !errors.Is(err, fs.ErrNotExist) {
			failed = fault("stage: a staged file could not be removed", err, false)
		}
	}
	return removed, failed
}
