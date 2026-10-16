package stage

import (
	"context"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	sectorSize = 32 << 10
	tagSize    = 16
)

func refusal(text string) error { return &failure{text: text, canceled: true} }

var (
	ErrOffset     = refusal("stage: negative offset or unknown origin")
	ErrLimit      = refusal("stage: write past the declared length")
	ErrHole       = refusal("stage: write past the end of the spool")
	ErrGrow       = refusal("stage: truncate cannot grow the spool")
	ErrAuth       = refusal("stage: a spool sector failed authentication")
	ErrErased     = refusal("stage: the spool was erased")
	errGeneration = refusal("stage: a spool sector was rewritten too often")
)

type Sealed struct {
	spool  *Spool
	name   string
	kept   bool
	mu     sync.Mutex
	f      *os.File
	aead   cipher.AEAD
	tags   []byte
	gens   []uint32
	sec    []byte
	ct     []byte
	nonce  [12]byte
	size   int64
	off    int64
	limit  int64
	cancel context.CancelCauseFunc
	err    error
}

func (s *Sealed) refuse(err error) error {
	s.err = err
	if s.cancel != nil {
		s.cancel(err)
	}
	return err
}

func (s *Sealed) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

func (s *Sealed) Stat() (fs.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return fileInfo(s.size), nil
}

func (s *Sealed) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.writeAt(p, s.off)
	s.off += int64(n)
	return n, err
}

func (s *Sealed) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeAt(p, off)
}

func (s *Sealed) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.readAt(p, s.off)
	s.off += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		return n, nil
	}
	return n, err
}

func (s *Sealed) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readAt(p, off)
}

func (s *Sealed) Seek(offset int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	base, ok := s.origin(whence)
	if !ok || base+offset < 0 {
		return 0, s.refuse(ErrOffset)
	}
	s.off = base + offset
	return s.off, nil
}

func (s *Sealed) origin(whence int) (int64, bool) {
	switch whence {
	case io.SeekStart:
		return 0, true
	case io.SeekCurrent:
		return s.off, true
	case io.SeekEnd:
		return s.size, true
	}
	return 0, false
}

func (s *Sealed) Truncate(size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	switch {
	case size < 0:
		return s.refuse(ErrOffset)
	case size > s.size:
		return s.refuse(ErrGrow)
	case size == s.size:
		return nil
	}
	defer clear(s.sec)
	if tail := size % sectorSize; tail != 0 {
		sector := size / sectorSize
		if err := s.open(sector, min(s.size-sector*sectorSize, sectorSize)); err != nil {
			return s.refuse(err)
		}
		if err := s.seal(sector, tail); err != nil {
			return s.refuse(err)
		}
	}
	if err := s.f.Truncate(size); err != nil {
		return s.refuse(fault("stage: the spool file could not be truncated", err, true))
	}
	s.size = size
	return nil
}

func (s *Sealed) Erase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	_ = s.f.Close()
	if s.kept {
		_ = os.Remove(filepath.Join(s.spool.dir, s.name))
	}
	clear(s.tags)
	clear(s.gens)
	clear(s.sec)
	clear(s.ct)
	s.f, s.aead, s.tags, s.gens, s.sec, s.ct = nil, nil, nil, nil, nil, nil
	s.size, s.off, s.err = 0, 0, ErrErased
	s.spool.release()
}

func (s *Sealed) writeAt(p []byte, off int64) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	switch {
	case off < 0:
		return 0, s.refuse(ErrOffset)
	case off > s.limit || int64(len(p)) > s.limit-off:
		return 0, s.refuse(ErrLimit)
	case off > s.size:
		return 0, s.refuse(ErrHole)
	}
	defer clear(s.sec)
	n := 0
	end := off + int64(len(p))
	for pos := off; pos < end; {
		sector := pos / sectorSize
		start := sector * sectorSize
		stop := min(end, start+sectorSize)
		valid := min(s.size-start, sectorSize)
		if pos > start || stop-start < valid {
			if err := s.open(sector, valid); err != nil {
				return n, s.refuse(err)
			}
		}
		copy(s.sec[pos-start:stop-start], p[pos-off:stop-off])
		if err := s.seal(sector, max(valid, stop-start)); err != nil {
			return n, s.refuse(err)
		}
		n += int(stop - pos)
		s.size = max(s.size, stop)
		pos = stop
	}
	return n, nil
}

func (s *Sealed) readAt(p []byte, off int64) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if off < 0 {
		return 0, s.refuse(ErrOffset)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	end := s.size
	if int64(len(p)) < end-off {
		end = off + int64(len(p))
	}
	defer clear(s.sec)
	n := 0
	for pos := off; pos < end; {
		sector := pos / sectorSize
		start := sector * sectorSize
		stop := min(end, start+sectorSize)
		if err := s.open(sector, min(s.size-start, sectorSize)); err != nil {
			return n, s.refuse(err)
		}
		n += copy(p[pos-off:stop-off], s.sec[pos-start:stop-start])
		pos = stop
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *Sealed) open(sector, valid int64) error {
	if _, err := s.f.ReadAt(s.ct[:valid], sector*sectorSize); err != nil {
		if errors.Is(err, io.EOF) {
			return ErrAuth
		}
		return fault("stage: the spool file could not be read", err, true)
	}
	copy(s.ct[valid:valid+tagSize], s.tags[sector*tagSize:])
	if _, err := s.aead.Open(s.sec[:0], s.nonceFor(sector, s.gens[sector]), s.ct[:valid+tagSize], nil); err != nil {
		return ErrAuth
	}
	return nil
}

func (s *Sealed) seal(sector, valid int64) error {
	gen := s.gens[sector] + 1
	if gen == 0 {
		return errGeneration
	}
	s.gens[sector] = gen
	out := s.aead.Seal(s.ct[:0], s.nonceFor(sector, gen), s.sec[:valid], nil)
	copy(s.tags[sector*tagSize:(sector+1)*tagSize], out[valid:])
	if _, err := s.f.WriteAt(out[:valid], sector*sectorSize); err != nil {
		return fault("stage: the spool file could not be written", err, true)
	}
	return nil
}

func (s *Sealed) nonceFor(sector int64, gen uint32) []byte {
	binary.BigEndian.PutUint64(s.nonce[:8], uint64(sector)) //nolint:gosec // G115: a sector index is never negative, as no offset below zero is accepted
	binary.BigEndian.PutUint32(s.nonce[8:], gen)
	return s.nonce[:]
}

type fileInfo int64

func (fileInfo) Name() string       { return "sealed" }
func (i fileInfo) Size() int64      { return int64(i) }
func (fileInfo) Mode() fs.FileMode  { return 0o600 }
func (fileInfo) ModTime() time.Time { return time.Time{} }
func (fileInfo) IsDir() bool        { return false }
func (fileInfo) Sys() any           { return nil }
