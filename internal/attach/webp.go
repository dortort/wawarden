package attach

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"image"
	"io"
	"math"

	"golang.org/x/image/webp"
)

const (
	alphaNone = iota
	alphaRaw
	alphaLossless
)

type chunk struct {
	head [8]byte
	id   string
	off  int64
	n    int64
}

type webpFrame struct {
	canvas   image.Rectangle
	size     image.Point
	dims     [6]byte
	riff     uint32
	alph     chunk
	bits     chunk
	alpha    int
	lossless bool
}

func planWebP(src *source, lim limits) (plan, error) {
	f, err := parseWebP(src, lim)
	if err != nil {
		return plan{}, err
	}
	riff := 4 + 8 + f.bits.n + f.bits.n&1
	if f.alpha != alphaNone {
		riff += 8 + 10 + 8 + f.alph.n + f.alph.n&1
	}
	if riff < 0 || riff > math.MaxUint32 {
		return plan{}, errMalformed
	}
	f.riff = uint32(riff)
	cfg, err := webp.DecodeConfig(f.stream(src))
	if err != nil {
		return plan{}, err
	}
	switch {
	case f.size == image.Point{}:
		f.size = image.Pt(cfg.Width, cfg.Height)
		f.canvas = image.Rectangle{Max: f.size}
	case f.size != image.Pt(cfg.Width, cfg.Height):
		return plan{}, errMalformed
	}
	return plan{
		canvas:      f.canvas,
		memory:      f.memory,
		orientation: 1,
		decode:      func() (image.Image, error) { return webp.Decode(f.stream(src)) },
	}, nil
}

func parseWebP(src *source, lim limits) (webpFrame, error) {
	var hdr [12]byte
	if err := src.readFull(hdr[:], 0); err != nil {
		return webpFrame{}, err
	}
	end := 8 + int64(binary.LittleEndian.Uint32(hdr[4:8]))
	if !riffForm(hdr[:], "WEBP") || end > src.s.Size() {
		return webpFrame{}, errMalformed
	}
	units := 1
	c, next, err := src.nextChunk(12, end)
	if err != nil {
		return webpFrame{}, err
	}
	switch c.id {
	case "VP8 ", "VP8L":
		var f webpFrame
		_, err := f.add(src, c, false)
		return f, err
	case "VP8X":
	default:
		return webpFrame{}, errMalformed
	}
	var x [10]byte
	if c.n != int64(len(x)) {
		return webpFrame{}, errMalformed
	}
	if err := src.readFull(x[:], c.off); err != nil {
		return webpFrame{}, err
	}
	f := webpFrame{size: image.Pt(uint24(x[4:7])+1, uint24(x[7:10])+1), dims: [6]byte(x[4:10])}
	f.canvas = image.Rectangle{Max: f.size}
	animated, alpha := x[0]&0x02 != 0, x[0]&0x10 != 0
	for {
		if units++; units > lim.units {
			return webpFrame{}, errMalformed
		}
		if c, next, err = src.nextChunk(next, end); err != nil {
			return webpFrame{}, err
		}
		switch {
		case animated && c.id == "ANMF":
			return f.firstFrame(src, c, lim, units)
		case !animated:
			if done, err := f.add(src, c, alpha); done || err != nil {
				return f, err
			}
		}
	}
}

func (f webpFrame) firstFrame(src *source, anmf chunk, lim limits, units int) (webpFrame, error) {
	var h [16]byte
	if anmf.n < int64(len(h)) {
		return webpFrame{}, errMalformed
	}
	if err := src.readFull(h[:], anmf.off); err != nil {
		return webpFrame{}, err
	}
	at := image.Pt(2*uint24(h[0:3]), 2*uint24(h[3:6]))
	f.size, f.dims = image.Pt(uint24(h[6:9])+1, uint24(h[9:12])+1), [6]byte(h[6:12])
	if !(image.Rectangle{Min: at, Max: at.Add(f.size)}).In(f.canvas) {
		return webpFrame{}, errMalformed
	}
	f.canvas = f.canvas.Sub(at)
	next, end := anmf.off+int64(len(h)), anmf.off+anmf.n
	for {
		if units++; units > lim.units {
			return webpFrame{}, errMalformed
		}
		c, after, err := src.nextChunk(next, end)
		if err != nil {
			return webpFrame{}, err
		}
		if done, err := f.add(src, c, true); done || err != nil {
			return f, err
		}
		next = after
	}
}

func (f *webpFrame) add(src *source, c chunk, alphaAllowed bool) (bool, error) {
	switch c.id {
	case "ALPH":
		var mode [1]byte
		if !alphaAllowed || f.alpha != alphaNone || c.n < 1 {
			return false, errMalformed
		}
		if err := src.readFull(mode[:], c.off); err != nil {
			return false, err
		}
		switch mode[0] & 0x03 {
		case 0:
			f.alpha = alphaRaw
		case 1:
			f.alpha = alphaLossless
		default:
			return false, errMalformed
		}
		f.alph = c
	case "VP8 ":
		f.bits = c
		return true, nil
	case "VP8L":
		if f.alpha != alphaNone {
			return false, errMalformed
		}
		f.bits, f.lossless = c, true
		return true, nil
	}
	return false, nil
}

func (src *source) nextChunk(off, end int64) (chunk, int64, error) {
	var h [8]byte
	if off+int64(len(h)) > end {
		return chunk{}, 0, errMalformed
	}
	if err := src.readFull(h[:], off); err != nil {
		return chunk{}, 0, err
	}
	c := chunk{head: h, id: string(h[:4]), off: off + int64(len(h)), n: int64(binary.LittleEndian.Uint32(h[4:]))}
	if c.off+c.n > end {
		return chunk{}, 0, errMalformed
	}
	return c, c.off + c.n + c.n&1, nil
}

func (f webpFrame) stream(src *source) io.Reader {
	var head bytes.Buffer
	var parts []io.Reader
	add := func(c chunk) {
		head.Write(c.head[:])
		parts = append(parts, bytes.NewReader(bytes.Clone(head.Bytes())), io.NewSectionReader(src, c.off, c.n))
		head.Reset()
		if c.n&1 != 0 {
			head.WriteByte(0)
		}
	}
	head.WriteString("RIFF")
	head.Write(binary.LittleEndian.AppendUint32(nil, f.riff))
	head.WriteString("WEBP")
	if f.alpha != alphaNone {
		head.WriteString("VP8X")
		head.Write([]byte{10, 0, 0, 0, 0x10, 0, 0, 0})
		head.Write(f.dims[:])
		add(f.alph)
	}
	add(f.bits)
	parts = append(parts, bytes.NewReader(head.Bytes()))
	return bufio.NewReaderSize(io.MultiReader(parts...), 64<<10)
}

func (f webpFrame) memory() int64 {
	w, h := int64(f.size.X), int64(f.size.Y)
	var total int64
	if f.lossless {
		total = 9 * w * h
	} else {
		mbw, mbh := (w+15)/16, (h+15)/16
		total = 256*mbw*mbh*3/2 + 16*mbw*mbh + f.bits.n
	}
	switch f.alpha {
	case alphaRaw:
		total += w * h
	case alphaLossless:
		total += 10 * w * h
	}
	return total
}

func uint24(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}
