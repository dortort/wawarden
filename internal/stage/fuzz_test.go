package stage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"testing"
)

const (
	opWrite = iota
	opWriteAt
	opRead
	opReadAt
	opSeek
	opTruncate
	opStat
	opCount
)

const (
	maxSteps  = 64
	maxLength = 4*sectorSize + 64
	negative  = 0xffffff
)

type program struct{ b []byte }

func (p *program) take(n int) int {
	v := 0
	for range n {
		v <<= 8
		if len(p.b) > 0 {
			v |= int(p.b[0])
			p.b = p.b[1:]
		}
	}
	return v
}

func (p *program) length() int { return p.take(3) % maxLength }

func (p *program) offset(size int64) int64 {
	raw := p.take(3)
	if raw == negative {
		return -1
	}
	return int64(raw) % (size + 2)
}

func (p *program) signed() int64 {
	v := p.take(3)
	if v >= 1<<23 {
		v -= 1 << 24
	}
	return int64(v)
}

type model struct {
	data  []byte
	off   int64
	limit int64
	err   error
}

func (m *model) refuse(err error) error {
	m.err = err
	return err
}

func (m *model) size() int64 { return int64(len(m.data)) }

func (m *model) writeAt(p []byte, off int64) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	switch {
	case off < 0:
		return 0, m.refuse(ErrOffset)
	case off > m.limit || int64(len(p)) > m.limit-off:
		return 0, m.refuse(ErrLimit)
	case off > m.size():
		return 0, m.refuse(ErrHole)
	}
	if end := off + int64(len(p)); end > m.size() {
		m.data = append(m.data, make([]byte, end-m.size())...)
	}
	return copy(m.data[off:], p), nil
}

func (m *model) readAt(p []byte, off int64) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if off < 0 {
		return 0, m.refuse(ErrOffset)
	}
	if off >= m.size() {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *model) write(p []byte) (int, error) {
	n, err := m.writeAt(p, m.off)
	m.off += int64(n)
	return n, err
}

func (m *model) read(p []byte) (int, error) {
	n, err := m.readAt(p, m.off)
	m.off += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		return n, nil
	}
	return n, err
}

func (m *model) seek(offset int64, whence int) (int64, error) {
	if m.err != nil {
		return 0, m.err
	}
	bases := map[int]int64{io.SeekStart: 0, io.SeekCurrent: m.off, io.SeekEnd: m.size()}
	base, ok := bases[whence]
	if !ok || base+offset < 0 {
		return 0, m.refuse(ErrOffset)
	}
	m.off = base + offset
	return m.off, nil
}

func (m *model) truncate(size int64) error {
	if m.err != nil {
		return m.err
	}
	switch {
	case size < 0:
		return m.refuse(ErrOffset)
	case size > m.size():
		return m.refuse(ErrGrow)
	}
	m.data = m.data[:size]
	return nil
}

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func pattern(n, step int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[(step*131+i*7)%len(alphabet)]
	}
	return b
}

func sameError(got, want error) bool {
	if want == nil {
		return got == nil
	}
	return errors.Is(got, want)
}

type nonceUse struct {
	sector int
	gen    uint32
}

type checker struct {
	t      *testing.T
	ctx    context.Context
	f      *Sealed
	m      *model
	gens   []uint32
	nonces map[nonceUse][tagSize]byte
}

func (c *checker) result(step int, name string, n int, err error, wantN int, wantErr error) {
	c.t.Helper()
	if n != wantN || !sameError(err, wantErr) {
		c.t.Fatalf("step %d: %s = %d, %v, want %d, %v", step, name, n, err, wantN, wantErr)
	}
}

func (c *checker) state(step int) {
	c.t.Helper()
	f, m := c.f, c.m
	if f.Size() != m.size() {
		c.t.Fatalf("step %d: Size = %d, want %d", step, f.Size(), m.size())
	}
	if m.err == nil && f.off != m.off {
		c.t.Fatalf("step %d: offset %d, want %d", step, f.off, m.off)
	}
	if cause := context.Cause(c.ctx); !sameError(cause, m.err) {
		c.t.Fatalf("step %d: the job ended with %v, want %v", step, cause, m.err)
	}
	disk, err := f.f.Stat()
	if err != nil || disk.Size() != f.size {
		c.t.Fatalf("step %d: the disk holds %v bytes (%v) for a size of %d", step, disk, err, f.size)
	}
	for i, gen := range f.gens {
		if gen < c.gens[i] {
			c.t.Fatalf("step %d: sector %d went back from generation %d to %d", step, i, c.gens[i], gen)
		}
		if gen == 0 {
			continue
		}
		var tag [tagSize]byte
		copy(tag[:], f.tags[i*tagSize:])
		use := nonceUse{i, gen}
		if old, ok := c.nonces[use]; ok && old != tag {
			c.t.Fatalf("step %d: sector %d sealed twice under generation %d", step, i, gen)
		}
		c.nonces[use] = tag
	}
	copy(c.gens, f.gens)
}

func run(t *testing.T, spool *Spool, declared int64, code []byte) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f, err := spool.Admit(declared, cancel)
	if err != nil {
		t.Fatalf("Admit(%d): %v", declared, err)
	}
	defer f.Erase()
	m := &model{limit: declared + overhead}
	c := &checker{t: t, ctx: ctx, f: f, m: m, gens: make([]uint32, len(f.gens)), nonces: map[nonceUse][tagSize]byte{}}
	p := &program{b: code}
	for step := 0; step < maxSteps && len(p.b) > 0; step++ {
		switch p.take(1) % opCount {
		case opWrite:
			b := pattern(p.length(), step)
			n, err := f.Write(b)
			wantN, wantErr := m.write(b)
			c.result(step, "Write", n, err, wantN, wantErr)
		case opWriteAt:
			off := p.offset(m.size())
			b := pattern(p.length(), step)
			n, err := f.WriteAt(b, off)
			wantN, wantErr := m.writeAt(b, off)
			c.result(step, "WriteAt", n, err, wantN, wantErr)
		case opRead:
			length := p.length()
			got, want := make([]byte, length), make([]byte, length)
			n, err := f.Read(got)
			wantN, wantErr := m.read(want)
			c.result(step, "Read", n, err, wantN, wantErr)
			if !bytes.Equal(got[:n], want[:n]) {
				t.Fatalf("step %d: Read returned other bytes", step)
			}
		case opReadAt:
			off := p.offset(m.size())
			length := p.length()
			got, want := make([]byte, length), make([]byte, length)
			n, err := f.ReadAt(got, off)
			wantN, wantErr := m.readAt(want, off)
			c.result(step, "ReadAt", n, err, wantN, wantErr)
			if !bytes.Equal(got[:n], want[:n]) {
				t.Fatalf("step %d: ReadAt returned other bytes", step)
			}
		case opSeek:
			whence := p.take(1) % 4
			offset := p.signed()
			at, err := f.Seek(offset, whence)
			wantAt, wantErr := m.seek(offset, whence)
			if at != wantAt || !sameError(err, wantErr) {
				t.Fatalf("step %d: Seek(%d, %d) = %d, %v, want %d, %v", step, offset, whence, at, err, wantAt, wantErr)
			}
		case opTruncate:
			size := p.offset(m.size())
			if err, wantErr := f.Truncate(size), m.truncate(size); !sameError(err, wantErr) {
				t.Fatalf("step %d: Truncate(%d) = %v, want %v", step, size, err, wantErr)
			}
		case opStat:
			fi, err := f.Stat()
			if !sameError(err, m.err) || (err == nil && fi.Size() != m.size()) {
				t.Fatalf("step %d: Stat = %v, %v, want a size of %d and %v", step, fi, err, m.size(), m.err)
			}
		}
		c.state(step)
	}
	if m.err == nil {
		got := make([]byte, m.size()+1)
		if n, err := f.ReadAt(got, 0); int64(n) != m.size() || !errors.Is(err, io.EOF) || !bytes.Equal(got[:n], m.data) {
			t.Fatalf("reading the whole spool back = %d, %v, or other bytes", n, err)
		}
	}
	f.Erase()
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrErased) {
		t.Fatalf("ReadAt after Erase = %v, want the erased refusal", err)
	}
	if list, err := os.ReadDir(spool.dir); err != nil || len(list) != 0 {
		t.Fatalf("the staging directory holds %d entries after Erase (%v)", len(list), err)
	}
}

type encoder []byte

func (e encoder) u24(v uint32) encoder {
	return append(e, binary.BigEndian.AppendUint32(nil, v)[1:]...)
}

func (e encoder) write(n uint32) encoder        { return append(e, opWrite).u24(n) }
func (e encoder) writeAt(off, n uint32) encoder { return append(e, opWriteAt).u24(off).u24(n) }
func (e encoder) read(n uint32) encoder         { return append(e, opRead).u24(n) }
func (e encoder) readAt(off, n uint32) encoder  { return append(e, opReadAt).u24(off).u24(n) }
func (e encoder) truncate(size uint32) encoder  { return append(e, opTruncate).u24(size) }
func (e encoder) stat() encoder                 { return append(e, opStat) }

func (e encoder) seek(offset uint32, whence byte) encoder {
	return append(e, opSeek, whence).u24(offset)
}

func seeds() map[uint32][]byte {
	const body = 70026
	library := encoder{}.write(20000).write(20000).write(20000).write(body-60000).
		readAt(body-10, 10).truncate(body-10).
		seek(0, io.SeekStart).read(sectorSize).read(sectorSize).read(sectorSize).read(sectorSize).
		seek(0, io.SeekStart).
		read(sectorSize).writeAt(0, sectorSize).read(sectorSize).writeAt(sectorSize, sectorSize).read(body-10-2*sectorSize).writeAt(2*sectorSize, body-10-2*sectorSize).
		truncate(body-17).seek(0, io.SeekStart).read(sectorSize).read(sectorSize).read(sectorSize).stat()
	retry := encoder{}.write(30000).seek(0, io.SeekStart).write(sectorSize).write(17000).stat().
		truncate(40000).writeAt(40000, 100).readAt(0, 2*sectorSize).truncate(sectorSize).writeAt(sectorSize, 5000).readAt(sectorSize-3, 10)
	stale := encoder{}.write(body).write(sectorSize).stat()
	hole := encoder{}.write(100).writeAt(101, 1).read(1)
	far := encoder{}.write(100).seek(10, io.SeekEnd).write(1).stat()
	back := encoder{}.write(100).seek(0xfffffd, io.SeekEnd).read(10).seek(0xffff00, io.SeekCurrent).stat()
	grow := encoder{}.write(10).truncate(11).stat()
	before := encoder{}.write(10).readAt(negative, 1).write(1)
	origin := encoder{}.write(10).seek(0, 3).stat()
	return map[uint32][]byte{
		70000: library,
		50000: retry,
		70001: stale,
		1000:  hole,
		1001:  far,
		1002:  grow,
		1003:  before,
		1004:  origin,
		1005:  back,
	}
}

func FuzzSealed(f *testing.F) {
	for declared, code := range seeds() {
		f.Add(declared-1, code)
	}
	spool, err := New(Options{Data: f.TempDir(), Local: true, UID: os.Geteuid()})
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	spool.statfs = freeSpace(math.MaxUint64)
	f.Fuzz(func(t *testing.T, declared uint32, code []byte) {
		run(t, spool, 1+int64(declared)%(4*sectorSize), code)
	})
}
