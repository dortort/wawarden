package attach

import (
	"encoding/binary"
	"errors"
	"math"
)

var (
	blankedBoxes   = map[string]bool{"udta": true, "meta": true, "uuid": true}
	containerBoxes = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true, "edts": true, "dinf": true, "mvex": true}
)

var (
	freeType = []byte("free")
	errUnits = errors.New("attach: too many boxes or chunks")
)

type box struct {
	typ      string
	off, end int64
	body     int64
}

type boxWalk struct {
	src    *source
	lim    limits
	units  int
	blanks []box
}

func scrubMP4(src *source, lim limits) error {
	w := &boxWalk{src: src, lim: lim}
	end, err := w.top(src.s.Size())
	if err != nil {
		return src.fail(err, ErrUnsupported)
	}
	zeros := make([]byte, 32<<10)
	for _, b := range w.blanks {
		if _, err := src.s.WriteAt(freeType, b.off+4); err != nil {
			return ErrSpool
		}
		for off := b.body; off < b.end; off += int64(len(zeros)) {
			if _, err := src.s.WriteAt(zeros[:min(int64(len(zeros)), b.end-off)], off); err != nil {
				return ErrSpool
			}
		}
	}
	if end < src.s.Size() {
		if err := src.s.Truncate(end); err != nil {
			return ErrSpool
		}
	}
	return nil
}

func (w *boxWalk) top(size int64) (int64, error) {
	off, moov := int64(0), false
	for off < size {
		b, err := w.box(off, size, true)
		switch {
		case errors.Is(err, errMalformed) && moov:
			return off, nil
		case err != nil:
			return 0, err
		case off == 0 && b.typ != "ftyp":
			return 0, errMalformed
		case b.typ == "moov":
			moov = true
		}
		if err := w.visit(b, 1); err != nil {
			return 0, err
		}
		off = b.end
	}
	if !moov {
		return 0, errMalformed
	}
	return off, nil
}

func (w *boxWalk) visit(b box, depth int) error {
	switch {
	case blankedBoxes[b.typ]:
		w.blanks = append(w.blanks, b)
	case containerBoxes[b.typ]:
		if depth > w.lim.depth {
			return errMalformed
		}
		for off := b.body; off < b.end; {
			child, err := w.box(off, b.end, false)
			if err != nil {
				return err
			}
			if err := w.visit(child, depth+1); err != nil {
				return err
			}
			off = child.end
		}
	}
	return nil
}

func (w *boxWalk) box(off, limit int64, top bool) (box, error) {
	if w.units++; w.units > w.lim.units {
		return box{}, errUnits
	}
	var h [16]byte
	if limit-off < 8 {
		return box{}, errMalformed
	}
	if err := w.src.readFull(h[:8], off); err != nil {
		return box{}, err
	}
	b := box{typ: string(h[4:8]), off: off, body: off + 8}
	if top && !printable(h[4:8]) {
		return box{}, errMalformed
	}
	switch size := binary.BigEndian.Uint32(h[:4]); size {
	case 0:
		if !top {
			return box{}, errMalformed
		}
		b.end = limit
	case 1:
		if limit-off < 16 {
			return box{}, errMalformed
		}
		if err := w.src.readFull(h[8:16], off+8); err != nil {
			return box{}, err
		}
		large := binary.BigEndian.Uint64(h[8:16])
		if large > math.MaxInt64 || int64(large) > limit-off {
			return box{}, errMalformed
		}
		b.body, b.end = off+16, off+int64(large)
	default:
		b.end = off + int64(size)
	}
	if b.end < b.body || b.end > limit {
		return box{}, errMalformed
	}
	return b, nil
}

func printable(typ []byte) bool {
	for _, c := range typ {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
