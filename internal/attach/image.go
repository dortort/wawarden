package attach

import (
	"bufio"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
)

type limits struct {
	pixels  int64
	budget  int64
	edge    int64
	output  int
	quality int
	units   int
	depth   int
}

var production = limits{pixels: 50_000_000, budget: 128 << 20, edge: 2048, output: 16 << 20, quality: 85, units: 1 << 16, depth: 16}

var (
	errRefused   = errors.New("attach: the normaliser refused the image")
	errMalformed = errors.New("attach: malformed image")
	errTooLarge  = errors.New("attach: normalised image over the output cap")
)

var slot = make(chan struct{}, 1)

type plan struct {
	canvas      image.Rectangle
	memory      func() int64
	orientation int
	decode      func() (image.Image, error)
}

func normalise(src *source, head []byte, lim limits) ([]byte, string, error) {
	select {
	case slot <- struct{}{}:
	case <-src.ctx.Done():
		return nil, "", src.ctx.Err()
	}
	defer func() { <-slot }()
	out, served, err := normaliseHeld(src, head, lim)
	return out, served, src.fail(err, errRefused)
}

func normaliseHeld(src *source, head []byte, lim limits) ([]byte, string, error) {
	var p plan
	var err error
	switch imageCodec(head) {
	case codecJPEG:
		p, err = planJPEG(src)
	case codecPNG:
		p, err = planPNG(src, head)
	case codecGIF:
		p, err = planGIF(src)
	case codecWebP:
		p, err = planWebP(src, lim)
	default:
		err = errRefused
	}
	if err != nil {
		return nil, "", err
	}
	if !p.within(lim) {
		return nil, "", errRefused
	}
	m, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	if m.Bounds().Empty() || !m.Bounds().In(p.canvas) {
		return nil, "", errMalformed
	}
	return encode(render(m, p.canvas, p.orientation, lim.edge), lim)
}

func (p plan) within(lim limits) bool {
	w, h := int64(p.canvas.Dx()), int64(p.canvas.Dy())
	if w <= 0 || h <= 0 || w > lim.pixels || h > lim.pixels || w*h > lim.pixels {
		return false
	}
	dw, dh := fit(w, h, lim.edge)
	return p.memory()+dw*dh*4+dw*32+int64(lim.output) <= lim.budget
}

func fit(w, h, edge int64) (int64, int64) {
	long := max(w, h)
	if long <= edge {
		return w, h
	}
	return max(1, (w*edge+long/2)/long), max(1, (h*edge+long/2)/long)
}

func reader(src *source) io.Reader {
	return bufio.NewReaderSize(io.NewSectionReader(src, 0, src.s.Size()), 64<<10)
}

type jpegComponent struct {
	id, h, v int
}

type jpegInfo struct {
	width, height int
	progressive   bool
	comps         []jpegComponent
	jfif          bool
	adobe         bool
	transform     byte
	orientation   int
}

func planJPEG(src *source) (plan, error) {
	info, err := scanJPEG(bufio.NewReaderSize(io.NewSectionReader(src, 0, src.s.Size()), 64<<10))
	if err != nil {
		return plan{}, err
	}
	return plan{
		canvas:      image.Rect(0, 0, info.width, info.height),
		memory:      info.memory,
		orientation: info.orientation,
		decode:      func() (image.Image, error) { return jpeg.Decode(reader(src)) },
	}, nil
}

func scanJPEG(r *bufio.Reader) (jpegInfo, error) {
	info := jpegInfo{orientation: 1}
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil || soi != [2]byte{0xff, 0xd8} {
		return info, errMalformed
	}
	exif := false
	for {
		marker, err := nextMarker(r)
		if err != nil {
			return info, err
		}
		if marker == 0 {
			continue
		}
		for marker == 0xff {
			if marker, err = r.ReadByte(); err != nil {
				return info, err
			}
		}
		if marker == 0xd9 {
			break
		}
		if 0xd0 <= marker && marker <= 0xd7 {
			continue
		}
		var length [2]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return info, err
		}
		n := int(binary.BigEndian.Uint16(length[:])) - 2
		if n < 0 {
			return info, errMalformed
		}
		switch {
		case marker == 0xc0 || marker == 0xc1 || marker == 0xc2:
			if info.comps != nil {
				return info, errMalformed
			}
			info.progressive = marker == 0xc2
			err = info.frame(r, n)
		case marker == 0xda && info.comps == nil:
			return info, errMalformed
		case marker == 0xe0 && n >= 5:
			var app [5]byte
			if _, err = io.ReadFull(r, app[:]); err == nil {
				info.jfif = string(app[:]) == "JFIF\x00"
				_, err = r.Discard(n - 5)
			}
		case marker == 0xe1 && !exif:
			app := make([]byte, n)
			if _, err = io.ReadFull(r, app); err == nil && len(app) >= 6 && string(app[:6]) == "Exif\x00\x00" {
				exif = true
				info.orientation = exifOrientation(app[6:])
			}
		case marker == 0xee && n >= 12:
			var app [12]byte
			if _, err = io.ReadFull(r, app[:]); err == nil {
				if string(app[:5]) == "Adobe" {
					info.adobe, info.transform = true, app[11]
				}
				_, err = r.Discard(n - 12)
			}
		case marker == 0xc4 || marker == 0xda || marker == 0xdb || marker == 0xdd || 0xe0 <= marker && marker <= 0xef || marker == 0xfe:
			_, err = r.Discard(n)
		default:
			return info, errMalformed
		}
		if err != nil {
			return info, err
		}
	}
	if info.comps == nil {
		return info, errMalformed
	}
	return info, nil
}

func nextMarker(r *bufio.Reader) (byte, error) {
	for {
		_, err := r.ReadSlice(0xff)
		if err == nil {
			return r.ReadByte()
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return 0, err
		}
	}
}

func (info *jpegInfo) frame(r io.Reader, n int) error {
	if n != 6+3*1 && n != 6+3*3 && n != 6+3*4 {
		return errMalformed
	}
	sof := make([]byte, n)
	if _, err := io.ReadFull(r, sof); err != nil {
		return err
	}
	if sof[0] != 8 || int(sof[5])*3+6 != n {
		return errMalformed
	}
	info.height = int(binary.BigEndian.Uint16(sof[1:3]))
	info.width = int(binary.BigEndian.Uint16(sof[3:5]))
	info.comps = make([]jpegComponent, sof[5])
	for i := range info.comps {
		c := sof[6+3*i : 9+3*i]
		h, v := int(c[1]>>4), int(c[1]&0x0f)
		if h < 1 || h > 4 || v < 1 || v > 4 {
			return errMalformed
		}
		if len(info.comps) == 1 {
			h, v = 1, 1
		}
		info.comps[i] = jpegComponent{id: int(c[0]), h: h, v: v}
	}
	return nil
}

func (info *jpegInfo) memory() int64 {
	maxH, maxV := 1, 1
	for _, c := range info.comps {
		maxH, maxV = max(maxH, c.h), max(maxV, c.v)
	}
	mxx := int64((info.width + 8*maxH - 1) / (8 * maxH))
	myy := int64((info.height + 8*maxV - 1) / (8 * maxV))
	w, h := 8*int64(maxH)*mxx, 8*int64(maxV)*myy
	pixels := int64(info.width) * int64(info.height)
	total := w * h
	if len(info.comps) > 1 {
		cw, ch := chroma(info.comps, maxH, maxV, w, h)
		total += 2 * cw * ch
	}
	switch {
	case len(info.comps) == 4:
		total += 8*int64(info.comps[3].h)*mxx*8*int64(info.comps[3].v)*myy + 4*pixels
	case len(info.comps) == 3 && info.rgb():
		total += 4 * pixels
	}
	if info.progressive {
		for _, c := range info.comps {
			total += mxx * myy * int64(c.h*c.v) * 64 * 4
		}
	}
	return total
}

func chroma(c []jpegComponent, maxH, maxV int, w, h int64) (int64, int64) {
	if c[1].h != c[2].h || c[1].v != c[2].v || c[0].h != maxH || c[0].v != maxV {
		return w, h
	}
	switch (maxH/c[1].h)<<4 | maxV/c[1].v {
	case 0x12:
		return w, (h + 1) / 2
	case 0x21:
		return (w + 1) / 2, h
	case 0x22:
		return (w + 1) / 2, (h + 1) / 2
	case 0x41:
		return (w + 3) / 4, h
	case 0x42:
		return (w + 3) / 4, (h + 1) / 2
	}
	return w, h
}

func (info *jpegInfo) rgb() bool {
	if info.jfif {
		return false
	}
	if info.adobe && info.transform == 0 {
		return true
	}
	return info.comps[0].id == 'R' && info.comps[1].id == 'G' && info.comps[2].id == 'B'
}

func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int64(order.Uint32(tiff[4:8]))
	if ifd+2 > int64(len(tiff)) {
		return 1
	}
	entries := int64(order.Uint16(tiff[ifd:]))
	for i := range entries {
		e := ifd + 2 + 12*i
		if e+12 > int64(len(tiff)) {
			return 1
		}
		if order.Uint16(tiff[e:]) != 0x0112 {
			continue
		}
		if order.Uint16(tiff[e+2:]) != 3 || order.Uint32(tiff[e+4:]) != 1 {
			return 1
		}
		if v := order.Uint16(tiff[e+8:]); v >= 1 && v <= 8 {
			return int(v)
		}
		return 1
	}
	return 1
}

func planPNG(src *source, head []byte) (plan, error) {
	cfg, err := png.DecodeConfig(reader(src))
	if err != nil {
		return plan{}, err
	}
	interlaced := len(head) > 28 && head[28] != 0
	return plan{
		canvas: image.Rect(0, 0, cfg.Width, cfg.Height),
		memory: func() int64 {
			w, h := int64(cfg.Width), int64(cfg.Height)
			decoded := w * h * pngPixelBytes(cfg.ColorModel)
			if interlaced {
				decoded += decoded / 2
			}
			return decoded + 2*(8*w+1)
		},
		orientation: 1,
		decode:      func() (image.Image, error) { return png.Decode(reader(src)) },
	}, nil
}

func pngPixelBytes(m color.Model) int64 {
	switch m {
	case color.Gray16Model, color.RGBA64Model, color.NRGBA64Model:
		return 8
	case color.GrayModel, color.RGBAModel, color.NRGBAModel:
		// A tRNS chunk makes image/png decode greyscale as NRGBA.
		return 4
	}
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	return 8
}

func planGIF(src *source) (plan, error) {
	cfg, err := gif.DecodeConfig(reader(src))
	if err != nil {
		return plan{}, err
	}
	return plan{
		canvas:      image.Rect(0, 0, cfg.Width, cfg.Height),
		memory:      func() int64 { return 2 * int64(cfg.Width) * int64(cfg.Height) },
		orientation: 1,
		decode:      func() (image.Image, error) { return gif.Decode(reader(src)) },
	}, nil
}

func render(m image.Image, canvas image.Rectangle, orientation int, edge int64) *image.RGBA {
	cw, ch := int64(canvas.Dx()), int64(canvas.Dy())
	w64, h64 := fit(cw, ch, edge)
	dw, dh := int(w64), int(h64)
	ow, oh := dw, dh
	if orientation >= 5 {
		ow, oh = dh, dw
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))
	at := sampler(m)
	in := m.Bounds()
	xs := make([]int, dw+1)
	for i := range xs {
		xs[i] = canvas.Min.X + int(int64(i)*cw/w64)
	}
	acc := make([]int64, 4*dw)
	for dy := range dh {
		y0 := canvas.Min.Y + int(int64(dy)*ch/h64)
		y1 := canvas.Min.Y + int(int64(dy+1)*ch/h64)
		clear(acc)
		for y := max(y0, in.Min.Y); y < min(y1, in.Max.Y); y++ {
			for dx := range dw {
				a := acc[4*dx : 4*dx+4 : 4*dx+4]
				for x := max(xs[dx], in.Min.X); x < min(xs[dx+1], in.Max.X); x++ {
					r, g, b, alpha := at(x, y)
					a[0] += int64(r)
					a[1] += int64(g)
					a[2] += int64(b)
					a[3] += int64(alpha)
				}
			}
		}
		for dx := range dw {
			n := int64(y1-y0) * int64(xs[dx+1]-xs[dx])
			ox, oy := orient(dx, dy, dw, dh, orientation)
			p := dst.Pix[dst.PixOffset(ox, oy):][:4:4]
			for c := range p {
				p[c] = uint8(min(max((acc[4*dx+c]+n/2)/n>>8, 0), math.MaxUint8))
			}
		}
	}
	return dst
}

func orient(x, y, w, h, orientation int) (int, int) {
	switch orientation {
	case 2:
		return w - 1 - x, y
	case 3:
		return w - 1 - x, h - 1 - y
	case 4:
		return x, h - 1 - y
	case 5:
		return y, x
	case 6:
		return h - 1 - y, x
	case 7:
		return h - 1 - y, w - 1 - x
	case 8:
		return y, w - 1 - x
	}
	return x, y
}

func sampler(m image.Image) func(x, y int) (r, g, b, a uint32) {
	switch m := m.(type) {
	case *image.YCbCr:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			r, g, b := color.YCbCrToRGB(m.Y[m.YOffset(x, y)], m.Cb[m.COffset(x, y)], m.Cr[m.COffset(x, y)])
			return uint32(r) * 0x101, uint32(g) * 0x101, uint32(b) * 0x101, 0xffff
		}
	case *image.Gray:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			v := uint32(m.Pix[m.PixOffset(x, y)]) * 0x101
			return v, v, v, 0xffff
		}
	case *image.RGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			p := m.Pix[m.PixOffset(x, y):][:4:4]
			return uint32(p[0]) * 0x101, uint32(p[1]) * 0x101, uint32(p[2]) * 0x101, uint32(p[3]) * 0x101
		}
	case *image.NRGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			p := m.Pix[m.PixOffset(x, y):][:4:4]
			a := uint32(p[3]) * 0x101
			return uint32(p[0]) * 0x101 * a / 0xffff, uint32(p[1]) * 0x101 * a / 0xffff, uint32(p[2]) * 0x101 * a / 0xffff, a
		}
	case *image.Paletted:
		var table [256][4]uint32
		for i, c := range m.Palette[:min(len(m.Palette), len(table))] {
			table[i][0], table[i][1], table[i][2], table[i][3] = c.RGBA()
		}
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			c := table[m.Pix[m.PixOffset(x, y)]]
			return c[0], c[1], c[2], c[3]
		}
	}
	return func(x, y int) (uint32, uint32, uint32, uint32) { return m.At(x, y).RGBA() }
}

type capped struct {
	b   []byte
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if len(p) > c.max-len(c.b) {
		return 0, errTooLarge
	}
	if need := len(c.b) + len(p); need > cap(c.b) {
		grown := make([]byte, len(c.b), min(max(2*cap(c.b), need, 64<<10), c.max))
		copy(grown, c.b)
		c.b = grown
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

func encode(m *image.RGBA, lim limits) ([]byte, string, error) {
	out := &capped{max: lim.output}
	if m.Opaque() {
		if err := jpeg.Encode(out, m, &jpeg.Options{Quality: lim.quality}); err != nil {
			return nil, "", err
		}
		return out.b, typeJPEG, nil
	}
	if err := png.Encode(out, m); err != nil {
		return nil, "", err
	}
	return out.b, typePNG, nil
}
