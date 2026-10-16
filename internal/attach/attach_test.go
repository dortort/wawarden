package attach

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/webp"
)

var (
	red   = color.RGBA{R: 0xff, A: 0xff}
	green = color.RGBA{G: 0xff, A: 0xff}
	blue  = color.RGBA{B: 0xff, A: 0xff}
	white = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
)

var errTestSpool = errors.New("synthetic spool failure")

type memSpool struct {
	b        []byte
	readErr  error
	writeErr error
}

func newSpool(b []byte) *memSpool {
	return &memSpool{b: bytes.Clone(b)}
}

func (m *memSpool) Size() int64 {
	return int64(len(m.b))
}

func (m *memSpool) ReadAt(p []byte, off int64) (int, error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memSpool) WriteAt(p []byte, off int64) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, errors.New("write past the end")
	}
	return copy(m.b[off:], p), nil
}

func (m *memSpool) Truncate(size int64) error {
	if size < 0 || size > int64(len(m.b)) {
		return errors.New("truncate out of range")
	}
	m.b = m.b[:size]
	return nil
}

func be16(n int) []byte {
	if n < 0 || n > math.MaxUint16 {
		panic("be16 out of range")
	}
	return binary.BigEndian.AppendUint16(nil, uint16(n))
}

func be32(n int) []byte {
	if n < 0 || n > math.MaxUint32 {
		panic("be32 out of range")
	}
	return binary.BigEndian.AppendUint32(nil, uint32(n))
}

func le32(n int) []byte {
	if n < 0 || n > math.MaxUint32 {
		panic("le32 out of range")
	}
	return binary.LittleEndian.AppendUint32(nil, uint32(n))
}

func le24(n int) []byte {
	return []byte{byte(n & 0xff), byte(n >> 8 & 0xff), byte(n >> 16 & 0xff)}
}

func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func filled(w, h int, c color.Color) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(m, m.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	return m
}

func quadrants(w, h int, tl, tr, bl, br color.Color) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for _, q := range []struct {
		r image.Rectangle
		c color.Color
	}{
		{image.Rect(0, 0, w/2, h/2), tl},
		{image.Rect(w/2, 0, w, h/2), tr},
		{image.Rect(0, h/2, w/2, h), bl},
		{image.Rect(w/2, h/2, w, h), br},
	} {
		draw.Draw(m, q.r, image.NewUniform(q.c), image.Point{}, draw.Src)
	}
	return m
}

func encodeJPEG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return b.Bytes()
}

func encodePNG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return b.Bytes()
}

func encodeGIF(t testing.TB, w, h int, colours ...color.Color) []byte {
	t.Helper()
	anim := &gif.GIF{}
	for _, c := range colours {
		frame := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{c, color.Transparent})
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, anim); err != nil {
		t.Fatalf("gif.EncodeAll: %v", err)
	}
	return b.Bytes()
}

func jpegSegment(t testing.TB, j []byte, marker byte) int {
	t.Helper()
	for i := 2; i+4 <= len(j); {
		if j[i] != 0xff {
			t.Fatalf("no marker at %d", i)
		}
		if j[i+1] == marker {
			return i
		}
		i += 2 + int(binary.BigEndian.Uint16(j[i+2:]))
	}
	t.Fatalf("no %#x segment", marker)
	return 0
}

func patchSOF(t testing.TB, j []byte, w, h int, progressive bool) []byte {
	t.Helper()
	out := bytes.Clone(j)
	i := jpegSegment(t, out, 0xc0)
	if progressive {
		out[i+1] = 0xc2
	}
	copy(out[i+5:], be16(h))
	copy(out[i+7:], be16(w))
	return out
}

func insertSegment(j []byte, at int, marker byte, payload []byte) []byte {
	return cat(j[:at], []byte{0xff, marker}, be16(len(payload)+2), payload, j[at:])
}

const (
	gpsMarker     = "SYNTHETIC-GPS-48.8584N-2.2945E"
	trailerMarker = "SYNTHETIC-POLYGLOT-TRAILER"
)

func exifTIFF(order binary.AppendByteOrder, orientation uint16) []byte {
	const gpsIFD = 8 + 2 + 2*12 + 4
	const gpsData = gpsIFD + 2 + 12 + 4
	b := []byte("II*\x00")
	if order == binary.BigEndian {
		b = []byte("MM\x00*")
	}
	b = order.AppendUint32(b, 8)
	b = order.AppendUint16(b, 2)
	b = order.AppendUint16(b, 0x0112)
	b = order.AppendUint16(b, 3)
	b = order.AppendUint32(b, 1)
	b = order.AppendUint16(b, orientation)
	b = order.AppendUint16(b, 0)
	b = order.AppendUint16(b, 0x8825)
	b = order.AppendUint16(b, 4)
	b = order.AppendUint32(b, 1)
	b = order.AppendUint32(b, gpsIFD)
	b = order.AppendUint32(b, 0)
	b = order.AppendUint16(b, 1)
	b = order.AppendUint16(b, 0x001b)
	b = order.AppendUint16(b, 7)
	b = order.AppendUint32(b, uint32(len(gpsMarker)))
	b = order.AppendUint32(b, gpsData)
	b = order.AppendUint32(b, 0)
	return append(b, gpsMarker...)
}

func withExif(j []byte, orientation uint16) []byte {
	return insertSegment(j, 2, 0xe1, cat([]byte("Exif\x00\x00"), exifTIFF(binary.LittleEndian, orientation)))
}

type bitWriter struct {
	b   []byte
	acc uint64
	n   uint
}

func (w *bitWriter) put(v uint64, bits uint) {
	w.acc |= v << w.n
	w.n += bits
	for w.n >= 8 {
		w.b = append(w.b, byte(w.acc&0xff))
		w.acc >>= 8
		w.n -= 8
	}
}

func (w *bitWriter) bytes() []byte {
	if w.n > 0 {
		return append(w.b, byte(w.acc&0xff))
	}
	return w.b
}

func vp8lSolid(w, h uint64, c color.NRGBA) []byte {
	var bw bitWriter
	bw.put(0x2f, 8)
	bw.put((w-1)&0x3fff, 14)
	bw.put((h-1)&0x3fff, 14)
	bw.put(1, 1)
	bw.put(0, 3)
	bw.put(0, 1)
	bw.put(0, 1)
	bw.put(0, 1)
	for _, v := range []uint8{c.G, c.R, c.B, c.A} {
		bw.put(1, 1)
		bw.put(0, 1)
		bw.put(1, 1)
		bw.put(uint64(v), 8)
	}
	bw.put(1, 1)
	bw.put(0, 1)
	bw.put(0, 1)
	bw.put(0, 1)
	return bw.bytes()
}

func riffChunk(id string, payload ...[]byte) []byte {
	body := cat(payload...)
	out := cat([]byte(id), le32(len(body)), body)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func webpFile(chunks ...[]byte) []byte {
	body := cat(chunks...)
	return cat([]byte("RIFF"), le32(4+len(body)), []byte("WEBP"), body)
}

func vp8xChunk(flags byte, w, h int) []byte {
	return riffChunk("VP8X", []byte{flags, 0, 0, 0}, le24(w-1), le24(h-1))
}

func anmfChunk(x, y, w, h int, frame ...[]byte) []byte {
	return riffChunk("ANMF", le24(x/2), le24(y/2), le24(w-1), le24(h-1), le24(100), []byte{0}, cat(frame...))
}

func animatedWebP(cw, ch int, frames ...[]byte) []byte {
	return webpFile(cat(vp8xChunk(0x12, cw, ch), riffChunk("ANIM", make([]byte, 6))), cat(frames...))
}

func mp4Box(typ string, payload ...[]byte) []byte {
	body := cat(payload...)
	return cat(be32(8+len(body)), []byte(typ), body)
}

func decodeOutcome(t testing.TB, out Outcome) image.Image {
	t.Helper()
	var m image.Image
	var err error
	switch out.ServedType {
	case typeJPEG:
		m, err = jpeg.Decode(bytes.NewReader(out.Image))
	case typePNG:
		m, err = png.Decode(bytes.NewReader(out.Image))
	default:
		t.Fatalf("normalised served type %q", out.ServedType)
	}
	if err != nil {
		t.Fatalf("decode the normalised image: %v", err)
	}
	return m
}

func near(c color.Color, want color.RGBA) bool {
	r, g, b, a := c.RGBA()
	d := func(x uint32, y uint8) bool { return x>>8 >= uint32(y)-min(uint32(y), 40) && x>>8 <= uint32(y)+40 }
	return d(r, want.R) && d(g, want.G) && d(b, want.B) && d(a, want.A)
}

func TestFormatWord(t *testing.T) {
	tar := make([]byte, 512)
	copy(tar[257:], "ustar\x0000")
	for _, tt := range []struct {
		name string
		head []byte
		want string
	}{
		{"jpeg", []byte("\xff\xd8\xff\xe0"), formatImage},
		{"png", []byte("\x89PNG\r\n\x1a\n"), formatImage},
		{"gif", []byte("GIF89a"), formatImage},
		{"webp", webpFile(riffChunk("VP8L", vp8lSolid(1, 1, color.NRGBA{A: 0xff}))), formatImage},
		{"tiff", []byte("II*\x00\x08\x00\x00\x00"), formatImage},
		{"bmp", cat([]byte("BM"), make([]byte, 12), le32(40)), formatImage},
		{"ico", []byte("\x00\x00\x01\x00\x01\x00"), formatImage},
		{"ogg", []byte("OggS\x00\x02"), formatAudio},
		{"mp3 with ID3", []byte("ID3\x04\x00"), formatAudio},
		{"mp3 frame", []byte("\xff\xfb\x90\x00"), formatAudio},
		{"adts", []byte("\xff\xf1\x50\x80"), formatAudio},
		{"wav", cat([]byte("RIFF"), le32(4), []byte("WAVE")), formatAudio},
		{"m4a", cat(be32(16), []byte("ftypM4A "), be32(0)), formatAudio},
		{"flac", []byte("fLaC"), formatAudio},
		{"amr", []byte("#!AMR\n"), formatAudio},
		{"mp4", cat(be32(16), []byte("ftypisom"), be32(0)), formatVideo},
		{"webm", []byte("\x1a\x45\xdf\xa3"), formatVideo},
		{"avi", cat([]byte("RIFF"), le32(4), []byte("AVI ")), formatVideo},
		{"pdf", []byte("%PDF-1.7"), formatPDF},
		{"zip", []byte("PK\x03\x04"), formatZip},
		{"empty zip", []byte("PK\x05\x06"), formatZip},
		{"ole2", []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"), formatOLE2},
		{"gzip", []byte("\x1f\x8b\x08"), formatArchive},
		{"rar", []byte("Rar!\x1a\x07\x01"), formatArchive},
		{"7z", []byte("7z\xbc\xaf\x27\x1c"), formatArchive},
		{"xz", []byte("\xfd7zXZ\x00"), formatArchive},
		{"bzip2", []byte("BZh9"), formatArchive},
		{"zstd", []byte("\x28\xb5\x2f\xfd"), formatArchive},
		{"tar", tar, formatArchive},
		{"pe", []byte("MZ\x90\x00"), formatExecutable},
		{"elf", []byte("\x7fELF\x02"), formatExecutable},
		{"mach-o", []byte("\xcf\xfa\xed\xfe"), formatExecutable},
		{"fat mach-o", []byte("\xca\xfe\xba\xbe"), formatExecutable},
		{"script", []byte("#!/bin/sh\n"), formatExecutable},
		{"plain text", []byte("hello, world\n"), formatText},
		{"html", []byte("<!DOCTYPE html><html></html>"), formatText},
		{"svg", []byte(`<svg></svg>`), formatText},
		{"xml", []byte(`<?xml version="1.0"?><a/>`), formatText},
		{"binary", []byte("\x00\x01\x02\x03"), formatUnknown},
		{"empty", nil, formatText},
		{"text that starts like a bitmap", []byte("BMW owners"), formatUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := format(tt.head); got != tt.want {
				t.Fatalf("format = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServedTypePerKind(t *testing.T) {
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	svg := []byte(`<svg onload="alert(1)"></svg>`)
	xmlSVG := []byte(`<?xml version="1.0"?><svg/>`)
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\n")
	ogg := cat([]byte("OggS\x00\x02"), make([]byte, 40))
	mp3 := cat([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 40))
	m4a := cat(be32(20), []byte("ftypM4A "), be32(0), []byte("mp42"), make([]byte, 16))
	wav := cat([]byte("RIFF"), le32(36), []byte("WAVEfmt "), make([]byte, 24))
	lottie := cat([]byte("PK\x03\x04"), make([]byte, 40))
	for _, tt := range []struct {
		name          string
		kind, variant string
		data          []byte
		want          Outcome
		err           error
	}{
		{name: "html image", kind: kindImage, data: html, err: ErrUnsupported},
		{name: "svg image", kind: kindImage, data: svg, err: ErrUnsupported},
		{name: "xml svg image", kind: kindImage, data: xmlSVG, err: ErrUnsupported},
		{name: "pdf image", kind: kindImage, data: pdf, err: ErrUnsupported},
		{name: "html sticker", kind: kindSticker, data: html, err: ErrUnsupported},
		{name: "svg sticker", kind: kindSticker, variant: "animated", data: svg, err: ErrUnsupported},
		{name: "pdf sticker", kind: kindSticker, data: pdf, err: ErrUnsupported},
		{name: "html document", kind: kindDocument, data: html, want: Outcome{ServedType: typeOctetStream, Format: formatText}},
		{name: "svg document", kind: kindDocument, data: svg, want: Outcome{ServedType: typeOctetStream, Format: formatText}},
		{name: "xml svg document", kind: kindDocument, data: xmlSVG, want: Outcome{ServedType: typeOctetStream, Format: formatText}},
		{name: "pdf document", kind: kindDocument, data: pdf, want: Outcome{ServedType: typePDF, Format: formatPDF}},
		{name: "ogg document", kind: kindDocument, data: ogg, want: Outcome{ServedType: typeOctetStream, Format: formatAudio}},
		{name: "zip document", kind: kindDocument, data: lottie, want: Outcome{ServedType: typeOctetStream, Format: formatZip}},
		{name: "ogg voice note", kind: kindAudio, variant: "voice_note", data: ogg, want: Outcome{ServedType: "audio/ogg", Format: formatAudio}},
		{name: "mp3 audio", kind: kindAudio, data: mp3, want: Outcome{ServedType: "audio/mpeg", Format: formatAudio}},
		{name: "mp4 audio", kind: kindAudio, data: m4a, want: Outcome{ServedType: "audio/mp4", Format: formatAudio}},
		{name: "wav audio", kind: kindAudio, data: wav, want: Outcome{ServedType: typeOctetStream, Format: formatAudio}},
		{name: "html audio", kind: kindAudio, data: html, want: Outcome{ServedType: typeOctetStream, Format: formatText}},
		{name: "pdf audio", kind: kindAudio, data: pdf, want: Outcome{ServedType: typeOctetStream, Format: formatPDF}},
		{name: "lottie sticker", kind: kindSticker, variant: variantLottie, data: lottie, want: Outcome{ServedType: typeOctetStream, Format: formatZip}},
		{name: "html lottie sticker", kind: kindSticker, variant: variantLottie, data: html, want: Outcome{ServedType: typeOctetStream, Format: formatText}},
		{name: "html video", kind: kindVideo, data: html, err: ErrUnsupported},
		{name: "html gif", kind: kindVideo, variant: "gif", data: html, err: ErrUnsupported},
		{name: "empty document", kind: kindDocument, want: Outcome{ServedType: typeOctetStream, Format: formatText}},
		{name: "empty image", kind: kindImage, err: ErrUnsupported},
		{name: "unknown kind", kind: "contact", data: pdf, err: ErrUnsupported},
		{name: "a video variant on an image", kind: kindImage, variant: "gif", data: pdf, err: ErrUnsupported},
		{name: "an audio variant on a video", kind: kindVideo, variant: "voice_note", data: pdf, err: ErrUnsupported},
		{name: "a sticker variant on a document", kind: kindDocument, variant: variantLottie, data: pdf, err: ErrUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSpool(tt.data)
			got, err := Apply(t.Context(), tt.kind, tt.variant, s)
			if !errors.Is(err, tt.err) || tt.err == nil && err != nil {
				t.Fatalf("Apply error = %v, want %v", err, tt.err)
			}
			if got.ServedType != tt.want.ServedType || got.Format != tt.want.Format || got.Normalised || got.Image != nil {
				t.Fatalf("Apply = %+v, want %+v", got, tt.want)
			}
			if strings.HasPrefix(got.ServedType, "image/") || strings.HasPrefix(got.ServedType, "text/") {
				t.Fatalf("a raw outcome is served as %q", got.ServedType)
			}
			if !bytes.Equal(s.b, tt.data) {
				t.Fatal("the policy changed a raw spool")
			}
		})
	}
}

func TestMetadataAndTrailerAreStripped(t *testing.T) {
	raw := append(withExif(encodeJPEG(t, filled(48, 32, red)), 1), "PK\x03\x04"+trailerMarker...)
	if !bytes.Contains(raw, []byte(gpsMarker)) || !bytes.Contains(raw, []byte(trailerMarker)) {
		t.Fatal("the fixture lacks its GPS block or its trailer")
	}
	for _, kind := range []string{kindImage, kindSticker, kindDocument} {
		t.Run(kind, func(t *testing.T) {
			s := newSpool(raw)
			out, err := Apply(t.Context(), kind, "", s)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !out.Normalised || out.Format != formatImage || out.ServedType != typeJPEG {
				t.Fatalf("Apply = %+v, want a normalised JPEG", out)
			}
			for _, leak := range []string{gpsMarker, trailerMarker, "Exif", "PK\x03\x04"} {
				if bytes.Contains(out.Image, []byte(leak)) {
					t.Fatalf("the normalised image still holds %q", leak)
				}
			}
			if !bytes.HasSuffix(out.Image, []byte{0xff, 0xd9}) {
				t.Fatal("bytes follow the normalised image's EOI")
			}
			if m := decodeOutcome(t, out); m.Bounds() != image.Rect(0, 0, 48, 32) || !near(m.At(24, 16), red) {
				t.Fatalf("decoded %v with %v at the centre", m.Bounds(), m.At(24, 16))
			}
			if !bytes.Equal(s.b, raw) {
				t.Fatal("the policy wrote to an image spool; the caller replaces it")
			}
		})
	}
}

func TestOnlyImageDocumentsAreNormalised(t *testing.T) {
	jpegBytes := encodeJPEG(t, filled(20, 10, blue))
	pdf := cat([]byte("%PDF-1.4\n"), jpegBytes)
	zip := cat([]byte("PK\x03\x04"), jpegBytes)
	for _, tt := range []struct {
		name       string
		data       []byte
		normalised bool
		served     string
		format     string
	}{
		{"jpeg", jpegBytes, true, typeJPEG, formatImage},
		{"opaque png", encodePNG(t, filled(20, 10, green)), true, typeJPEG, formatImage},
		{"png with alpha", encodePNG(t, filled(20, 10, color.NRGBA{R: 0xff, A: 0x80})), true, typePNG, formatImage},
		{"gif", encodeGIF(t, 20, 10, red), true, typeJPEG, formatImage},
		{"webp", webpFile(riffChunk("VP8L", vp8lSolid(20, 10, color.NRGBA{G: 0xff, A: 0xff}))), true, typeJPEG, formatImage},
		{"pdf holding a jpeg", pdf, false, typePDF, formatPDF},
		{"zip holding a jpeg", zip, false, typeOctetStream, formatZip},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Apply(t.Context(), kindDocument, "", newSpool(tt.data))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if out.Normalised != tt.normalised || out.ServedType != tt.served || out.Format != tt.format || (out.Image != nil) != tt.normalised {
				t.Fatalf("Apply = {%q %q %v %d bytes}, want {%q %q %v}", out.ServedType, out.Format, out.Normalised, len(out.Image), tt.served, tt.format, tt.normalised)
			}
			if tt.normalised {
				if m := decodeOutcome(t, out); m.Bounds() != image.Rect(0, 0, 20, 10) {
					t.Fatalf("decoded bounds %v, want 20x10", m.Bounds())
				}
			}
		})
	}
}

func TestRefusedImagesStayRawDocuments(t *testing.T) {
	base := encodeJPEG(t, filled(64, 48, red))
	pngBytes := encodePNG(t, filled(64, 48, green))
	corruptPNG := bytes.Clone(pngBytes)
	corruptPNG[bytes.Index(corruptPNG, []byte("IDAT"))+8] ^= 0xff
	gifBytes := encodeGIF(t, 64, 48, red, blue)
	webpBytes := webpFile(riffChunk("VP8L", vp8lSolid(64, 48, color.NRGBA{B: 0xff, A: 0xff})))
	tight := production
	tight.output = 64
	for _, tt := range []struct {
		name string
		data []byte
		lim  limits
	}{
		{"jpeg over 50 MP", patchSOF(t, base, 10000, 5001, false), production},
		{"progressive jpeg over the budget under 50 MP", patchSOF(t, base, 4800, 3600, true), production},
		{"truncated jpeg", base[:len(base)/2], production},
		{"corrupt jpeg", patchSOF(t, base, 64, 0, false), production},
		{"truncated png", pngBytes[:len(pngBytes)-20], production},
		{"corrupt png", corruptPNG, production},
		{"truncated gif", gifBytes[:len(gifBytes)/2], production},
		{"corrupt gif", cat(gifBytes[:6], be16(0), be16(0), gifBytes[10:]), production},
		{"truncated webp", webpBytes[:len(webpBytes)-4], production},
		{"corrupt webp", cat(webpBytes[:20], []byte{0x00}, webpBytes[21:]), production},
		{"output over the cap", base, tight},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSpool(tt.data)
			out, err := apply(t.Context(), kindDocument, "", s, tt.lim)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if out.ServedType != typeOctetStream || out.Format != formatImage || out.Normalised || out.Image != nil {
				t.Fatalf("apply = %+v, want a raw octet-stream image", out)
			}
			if !bytes.Equal(s.b, tt.data) {
				t.Fatal("a refused document's bytes changed")
			}
			for _, kind := range []string{kindImage, kindSticker} {
				s := newSpool(tt.data)
				if out, err := apply(t.Context(), kind, "", s, tt.lim); !errors.Is(err, ErrUnsupported) || out.ServedType != "" || out.Image != nil {
					t.Fatalf("as %s: %+v, %v, want ErrUnsupported", kind, out, err)
				}
				if !bytes.Equal(s.b, tt.data) {
					t.Fatalf("as %s: the spool changed", kind)
				}
			}
		})
	}
}

func TestAnimatedGIFDocumentKeepsItsFirstFrame(t *testing.T) {
	out, err := Apply(t.Context(), kindDocument, "", newSpool(encodeGIF(t, 16, 16, red, blue, green)))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	m := decodeOutcome(t, out)
	if !out.Normalised || m.Bounds() != image.Rect(0, 0, 16, 16) || !near(m.At(8, 8), red) {
		t.Fatalf("Apply = normalised %v, %v with %v at the centre, want the red first frame", out.Normalised, m.Bounds(), m.At(8, 8))
	}
}

func TestLongEdgeIsScaledTo2048(t *testing.T) {
	for _, tt := range []struct {
		name string
		img  image.Image
		want image.Rectangle
	}{
		{"wide", filled(3000, 30, green), image.Rect(0, 0, 2048, 20)},
		{"tall", filled(25, 4100, green), image.Rect(0, 0, 12, 2048)},
		{"at the edge", filled(2048, 8, green), image.Rect(0, 0, 2048, 8)},
		{"small", filled(40, 20, green), image.Rect(0, 0, 40, 20)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Apply(t.Context(), kindDocument, "", newSpool(encodePNG(t, tt.img)))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			m := decodeOutcome(t, out)
			if !out.Normalised || m.Bounds() != tt.want || !near(m.At(m.Bounds().Dx()/2, m.Bounds().Dy()/2), green) {
				t.Fatalf("Apply = normalised %v, %v, want %v", out.Normalised, m.Bounds(), tt.want)
			}
		})
	}
}

func TestScalingAveragesBoxes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 1))
	for x, c := range []color.RGBA{red, blue, white, white} {
		src.SetRGBA(x, 0, c)
	}
	dst := render(src, src.Bounds(), 1, 2)
	if got, want := dst.RGBAAt(0, 0), (color.RGBA{R: 0x80, B: 0x80, A: 0xff}); got != want {
		t.Fatalf("left box = %v, want %v", got, want)
	}
	if got := dst.RGBAAt(1, 0); got != white {
		t.Fatalf("right box = %v, want white", got)
	}
}

func TestOrientationIsApplied(t *testing.T) {
	stored := quadrants(32, 16, red, green, blue, white)
	for _, tt := range []struct {
		orientation    uint16
		tl, tr, bl, br color.RGBA
	}{
		{1, red, green, blue, white},
		{2, green, red, white, blue},
		{3, white, blue, green, red},
		{4, blue, white, red, green},
		{5, red, blue, green, white},
		{6, blue, red, white, green},
		{7, white, green, blue, red},
		{8, green, white, red, blue},
	} {
		t.Run(fmt.Sprint(tt.orientation), func(t *testing.T) {
			out, err := Apply(t.Context(), kindImage, "", newSpool(withExif(encodeJPEG(t, stored), tt.orientation)))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			m := decodeOutcome(t, out)
			w, h := 32, 16
			if tt.orientation >= 5 {
				w, h = 16, 32
			}
			if m.Bounds() != image.Rect(0, 0, w, h) {
				t.Fatalf("bounds %v, want %dx%d", m.Bounds(), w, h)
			}
			for _, q := range []struct {
				x, y int
				want color.RGBA
			}{{w / 4, h / 4, tt.tl}, {3 * w / 4, h / 4, tt.tr}, {w / 4, 3 * h / 4, tt.bl}, {3 * w / 4, 3 * h / 4, tt.br}} {
				if !near(m.At(q.x, q.y), q.want) {
					t.Fatalf("pixel (%d,%d) = %v, want %v", q.x, q.y, m.At(q.x, q.y), q.want)
				}
			}
		})
	}
}

func TestEXIFOrientationParsing(t *testing.T) {
	le := exifTIFF(binary.LittleEndian, 6)
	wrongType := bytes.Clone(le)
	wrongType[12] = 4
	for _, tt := range []struct {
		name string
		tiff []byte
		want int
	}{
		{"little endian", le, 6},
		{"big endian", exifTIFF(binary.BigEndian, 8), 8},
		{"out of range", exifTIFF(binary.LittleEndian, 9), 1},
		{"zero", exifTIFF(binary.LittleEndian, 0), 1},
		{"not a short", wrongType, 1},
		{"truncated entry", le[:20], 1},
		{"offset past the end", cat([]byte("II*\x00"), le32(4096)), 1},
		{"no byte order", cat([]byte("XX*\x00"), le[4:]), 1},
		{"empty", nil, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := exifOrientation(tt.tiff); got != tt.want {
				t.Fatalf("exifOrientation = %d, want %d", got, tt.want)
			}
		})
	}
}

func jpegInfoFor(w, h int, progressive bool, sampling ...int) jpegInfo {
	info := jpegInfo{width: w, height: h, progressive: progressive, jfif: true}
	for i, hv := range sampling {
		info.comps = append(info.comps, jpegComponent{id: i + 1, h: hv >> 4, v: hv & 0x0f})
	}
	return info
}

func TestBudgetArithmetic(t *testing.T) {
	const (
		s420 = 0x22
		s444 = 0x11
	)
	for _, tt := range []struct {
		name   string
		info   jpegInfo
		memory int64
		fits   bool
	}{
		{"baseline 4:2:0 at the 50 MP cap", jpegInfoFor(10000, 5000, false, s420, s444, s444), 10000*5008 + 2*5000*2504, true},
		{"baseline 4:2:0 at 48 MP", jpegInfoFor(8000, 6000, false, s420, s444, s444), 8000*6000 + 2*4000*3000, true},
		{"progressive 4:2:0 at 12 MP", jpegInfoFor(4000, 3000, true, s420, s444, s444), 4000*3008 + 2*2000*1504 + 250*188*6*256, true},
		{"progressive 4:2:0 at 13.8 MP", jpegInfoFor(4288, 3216, true, s420, s444, s444), 4288*3216 + 2*2144*1608 + 268*201*6*256, true},
		{"progressive 4:2:0 at 14 MP", jpegInfoFor(4320, 3240, true, s420, s444, s444), 4320*3248 + 2*2160*1624 + 270*203*6*256, false},
		{"progressive 4:2:0 at 20 MP", jpegInfoFor(5472, 3648, true, s420, s444, s444), 5472*3648 + 2*2736*1824 + 342*228*6*256, false},
		{"baseline 4:4:4 at 48 MP", jpegInfoFor(8000, 6000, false, s444, s444, s444), 3 * 8000 * 6000, false},
		{"baseline greyscale at 50 MP", jpegInfoFor(10000, 5000, false, s444), 10000 * 5000, true},
		{"progressive greyscale at 50 MP", jpegInfoFor(10000, 5000, true, s444), 10000*5000 + 1250*625*256, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.memory(); got != tt.memory {
				t.Fatalf("memory = %d, want %d", got, tt.memory)
			}
			p := plan{canvas: image.Rect(0, 0, tt.info.width, tt.info.height), memory: tt.info.memory}
			if got := p.within(production); got != tt.fits {
				t.Fatalf("within = %v, want %v", got, tt.fits)
			}
		})
	}
}

func TestDecoderMemoryCountsColourConversions(t *testing.T) {
	base := encodeJPEG(t, filled(32, 16, red))
	eoi := len(base) - 2
	rgb := insertSegment(base, eoi, 0xee, cat([]byte("Adobe"), make([]byte, 6), []byte{0}))
	m, err := jpeg.Decode(bytes.NewReader(rgb))
	if err != nil {
		t.Fatalf("jpeg.Decode: %v", err)
	}
	if _, ok := m.(*image.RGBA); !ok {
		t.Fatalf("the decoder returned %T, want the RGB conversion it allocates for an Adobe transform 0", m)
	}
	plain, err := scanJPEG(bufio.NewReader(bytes.NewReader(base)))
	if err != nil {
		t.Fatalf("scanJPEG: %v", err)
	}
	converted, err := scanJPEG(bufio.NewReader(bytes.NewReader(rgb)))
	if err != nil {
		t.Fatalf("scanJPEG: %v", err)
	}
	if got, want := converted.memory()-plain.memory(), int64(4*32*16); got != want {
		t.Fatalf("an APP14 after the scan adds %d bytes, want %d", got, want)
	}
	jfif := insertSegment(rgb, 2, 0xe0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))
	if info, err := scanJPEG(bufio.NewReader(bytes.NewReader(jfif))); err != nil || info.memory() != plain.memory() {
		t.Fatalf("a JFIF image with an Adobe segment: %v, %d bytes, want %d", err, info.memory(), plain.memory())
	}
}

func TestPixelCap(t *testing.T) {
	free := func() int64 { return 0 }
	for _, tt := range []struct {
		canvas image.Rectangle
		fits   bool
	}{
		{image.Rect(0, 0, 10000, 5000), true},
		{image.Rect(0, 0, 50_000_000, 1), true},
		{image.Rect(0, 0, 50_000_001, 1), false},
		{image.Rect(0, 0, 7072, 7071), false},
		{image.Rect(0, 0, 0, 10), false},
	} {
		if got := (plan{canvas: tt.canvas, memory: free}).within(production); got != tt.fits {
			t.Errorf("within(%v) = %v, want %v", tt.canvas, got, tt.fits)
		}
	}
}

func TestAnimatedWebPGivesItsFirstFrame(t *testing.T) {
	redFrame := riffChunk("VP8L", vp8lSolid(8, 8, color.NRGBA{R: 0xff, A: 0xff}))
	blueFrame := riffChunk("VP8L", vp8lSolid(8, 8, color.NRGBA{B: 0xff, A: 0xff}))
	full := animatedWebP(8, 8, anmfChunk(0, 0, 8, 8, redFrame), anmfChunk(0, 0, 8, 8, blueFrame))
	if _, err := webp.Decode(bytes.NewReader(full)); err == nil {
		t.Fatal("x/image decoded an animated WebP; the fixture does not exercise the frame extractor")
	}
	offset := animatedWebP(16, 8, anmfChunk(8, 0, 8, 8, redFrame), anmfChunk(0, 0, 8, 8, blueFrame))
	for _, tt := range []struct {
		name, kind, variant string
		data                []byte
		bounds              image.Rectangle
		opaque              image.Point
		clear               image.Point
	}{
		{"animated sticker", kindSticker, "animated", full, image.Rect(0, 0, 8, 8), image.Pt(4, 4), image.Pt(-1, -1)},
		{"sticker sent without the variant", kindSticker, "", full, image.Rect(0, 0, 8, 8), image.Pt(4, 4), image.Pt(-1, -1)},
		{"document", kindDocument, "", full, image.Rect(0, 0, 8, 8), image.Pt(4, 4), image.Pt(-1, -1)},
		{"frame offset on a wider canvas", kindSticker, "animated", offset, image.Rect(0, 0, 16, 8), image.Pt(12, 4), image.Pt(4, 4)},
		{"still sticker", kindSticker, "", webpFile(redFrame), image.Rect(0, 0, 8, 8), image.Pt(4, 4), image.Pt(-1, -1)},
		{"extended still sticker", kindSticker, "", webpFile(vp8xChunk(0, 8, 8), redFrame), image.Rect(0, 0, 8, 8), image.Pt(4, 4), image.Pt(-1, -1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Apply(t.Context(), tt.kind, tt.variant, newSpool(tt.data))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			m := decodeOutcome(t, out)
			if !out.Normalised || m.Bounds() != tt.bounds || !near(m.At(tt.opaque.X, tt.opaque.Y), red) {
				t.Fatalf("Apply = %q normalised %v, %v with %v, want the red first frame", out.ServedType, out.Normalised, m.Bounds(), m.At(tt.opaque.X, tt.opaque.Y))
			}
			if tt.clear.X >= 0 {
				if _, _, _, a := m.At(tt.clear.X, tt.clear.Y).RGBA(); out.ServedType != typePNG || a != 0 {
					t.Fatalf("%q with alpha %d off the frame, want a transparent PNG canvas", out.ServedType, a)
				}
			}
		})
	}
}

func TestWebPFrameStream(t *testing.T) {
	vp8 := riffChunk("VP8 ", []byte("synthetic-vp8"))
	alph := riffChunk("ALPH", []byte{0, 2, 3})
	for _, tt := range []struct {
		name  string
		data  []byte
		want  []byte
		alpha int
		err   error
	}{
		{"still with alpha", webpFile(vp8xChunk(0x10, 5, 3), riffChunk("ICCP", []byte("icc")), alph, vp8, riffChunk("EXIF", []byte(gpsMarker))),
			webpFile(vp8xChunk(0x10, 5, 3), alph, vp8), alphaRaw, nil},
		{"lossless alpha in a frame", animatedWebP(9, 9, anmfChunk(2, 4, 5, 3, riffChunk("ALPH", []byte{0x11}), vp8)),
			webpFile(vp8xChunk(0x10, 5, 3), riffChunk("ALPH", []byte{0x11}), vp8), alphaLossless, nil},
		{"still without alpha", webpFile(vp8xChunk(0, 5, 3), riffChunk("XMP ", []byte(gpsMarker)), vp8), webpFile(vp8), alphaNone, nil},
		{"alpha without the flag", webpFile(vp8xChunk(0, 5, 3), alph, vp8), nil, 0, errMalformed},
		{"alpha before lossless", webpFile(vp8xChunk(0x10, 5, 3), alph, riffChunk("VP8L", []byte("x"))), nil, 0, errMalformed},
		{"two alpha chunks", webpFile(vp8xChunk(0x10, 5, 3), alph, alph, vp8), nil, 0, errMalformed},
		{"unknown alpha compression", webpFile(vp8xChunk(0x10, 5, 3), riffChunk("ALPH", []byte{2}), vp8), nil, 0, errMalformed},
		{"frame outside the canvas", animatedWebP(8, 8, anmfChunk(6, 0, 4, 4, vp8)), nil, 0, errMalformed},
		{"short frame header", animatedWebP(8, 8, riffChunk("ANMF", make([]byte, 8))), nil, 0, errMalformed},
		{"no frame", animatedWebP(8, 8), nil, 0, errMalformed},
		{"chunk past the end", webpFile(vp8xChunk(0, 5, 3), []byte("VP8 \xff\xff\x00\x00")), nil, 0, errMalformed},
		{"no extended header", webpFile(riffChunk("ICCP", []byte("icc")), vp8), nil, 0, errMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &source{ctx: t.Context(), s: newSpool(tt.data)}
			f, err := parseWebP(src, production)
			if !errors.Is(err, tt.err) || tt.err == nil && err != nil {
				t.Fatalf("parseWebP error = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			f.riff = binary.LittleEndian.Uint32(tt.want[4:8])
			got, err := io.ReadAll(f.stream(src))
			if err != nil || !bytes.Equal(got, tt.want) || f.alpha != tt.alpha {
				t.Fatalf("stream = %q, %v, alpha %d, want %q, alpha %d", got, err, f.alpha, tt.want, tt.alpha)
			}
		})
	}
}

const (
	locationMarker = "SYNTHETIC-LOCATION+48.8584+002.2945/"
	metaMarker     = "SYNTHETIC-ITUNES-META"
	trackMarker    = "SYNTHETIC-TRACK-NAME"
	xmpMarker      = "SYNTHETIC-XMP-PACKET"
)

func ftypBox(major string, compatible ...string) []byte {
	return mp4Box("ftyp", []byte(major), be32(0x200), []byte(strings.Join(compatible, "")))
}

func mp4Fixture(ftyp, mdat []byte) []byte {
	stco := mp4Box("stco", be32(0), be32(2), be32(1000), be32(2000))
	co64 := mp4Box("co64", be32(0), be32(1), be32(0), be32(3000))
	stbl := mp4Box("stbl", mp4Box("stsd", be32(0), be32(0)), stco, co64)
	trak := mp4Box("trak", mp4Box("tkhd", make([]byte, 84)), mp4Box("mdia", mp4Box("mdhd", make([]byte, 24)), mp4Box("minf", stbl)),
		mp4Box("udta", mp4Box("name", []byte(trackMarker))))
	moov := mp4Box("moov", mp4Box("mvhd", make([]byte, 100)), trak, mp4Box("udta", mp4Box("\xa9xyz", []byte(locationMarker))),
		mp4Box("meta", be32(0), mp4Box("ilst", []byte(metaMarker))))
	return cat(ftyp, moov, mp4Box("uuid", []byte("0123456789abcdef"), []byte(xmpMarker)), mdat)
}

func blanked(file []byte) []byte {
	out := bytes.Clone(file)
	for _, typ := range []string{"udta", "meta", "uuid"} {
		for i := 0; ; {
			j := bytes.Index(out[i:], []byte(typ))
			if j < 0 {
				break
			}
			off := i + j - 4
			end := off + int(binary.BigEndian.Uint32(out[off:]))
			copy(out[off+4:], "free")
			clear(out[off+8 : end])
			i = end
		}
	}
	return out
}

func TestMP4MetadataBoxesBecomeFree(t *testing.T) {
	mdat := mp4Box("mdat", []byte("synthetic media samples"))
	file := mp4Fixture(ftypBox("isom", "isom", "mp42"), mdat)
	quicktime := mp4Fixture(ftypBox("qt  ", "qt  "), mdat)
	want := blanked(file)
	if bytes.Equal(want, file) || bytes.Count(want, []byte("free")) != 4 {
		t.Fatal("the fixture has no metadata boxes to blank")
	}
	toEOF := cat(file[:len(file)-len(mdat)], be32(0), []byte("mdat"), []byte("samples to the end of the file"))
	large := cat(file[:len(file)-len(mdat)], be32(1), []byte("mdat"), be32(0), be32(16+7), []byte("samples"))
	for _, tt := range []struct {
		name, variant string
		data          []byte
		want          []byte
		served        string
	}{
		{"no trailer", "", file, want, typeMP4},
		{"polyglot trailer", "", cat(file, []byte("<html>SYNTHETIC-POLYGLOT</html>")), want, typeMP4},
		{"zip trailer", "gif", cat(file, []byte("PK\x03\x04"+trailerMarker)), want, typeMP4},
		{"short trailer", "video_note", cat(file, []byte("\x00\x00\x01")), want, typeMP4},
		{"last box to the end of the file", "", toEOF, blanked(toEOF), typeMP4},
		{"large size", "", cat(large, []byte("trailer")), blanked(large), typeMP4},
		{"quicktime brand", "", quicktime, blanked(quicktime), typeOctetStream},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSpool(tt.data)
			out, err := Apply(t.Context(), kindVideo, tt.variant, s)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if out.ServedType != tt.served || out.Format != formatVideo || out.Normalised || out.Image != nil {
				t.Fatalf("Apply = %+v, want %s video", out, tt.served)
			}
			if !bytes.Equal(s.b, tt.want) {
				t.Fatalf("scrubbed %d bytes, want %d: %q", len(s.b), len(tt.want), s.b)
			}
			for _, leak := range []string{locationMarker, metaMarker, trackMarker, xmpMarker, trailerMarker, "<html>"} {
				if bytes.Contains(s.b, []byte(leak)) {
					t.Fatalf("the scrubbed file still holds %q", leak)
				}
			}
			stco := bytes.Index(s.b, []byte("stco")) + 12
			co64 := bytes.Index(s.b, []byte("co64")) + 12
			if got := [3]uint64{uint64(binary.BigEndian.Uint32(s.b[stco:])), uint64(binary.BigEndian.Uint32(s.b[stco+4:])), binary.BigEndian.Uint64(s.b[co64:])}; got != [3]uint64{1000, 2000, 3000} {
				t.Fatalf("chunk offsets %v, want them unchanged", got)
			}
		})
	}
}

func TestMalformedMP4IsRefused(t *testing.T) {
	ftyp := ftypBox("isom", "isom", "mp42")
	nest := mp4Box("mvhd", make([]byte, 8))
	for range 20 {
		nest = mp4Box("trak", nest)
	}
	few := production
	few.units = 8
	for _, tt := range []struct {
		name string
		data []byte
		lim  limits
	}{
		{"no ftyp first", cat(mp4Box("moov", mp4Box("mvhd")), ftyp), production},
		{"no moov", cat(ftyp, mp4Box("mdat", []byte("x"))), production},
		{"only a header", ftyp[:6], production},
		{"empty", nil, production},
		{"child past its parent", cat(ftyp, mp4Box("moov", be32(64), []byte("mvhd"), make([]byte, 8))), production},
		{"zero-size child", cat(ftyp, mp4Box("moov", be32(0), []byte("mvhd"), make([]byte, 8))), production},
		{"child shorter than a header", cat(ftyp, mp4Box("moov", mp4Box("mvhd"), []byte{0, 0, 0})), production},
		{"child size under its header", cat(ftyp, mp4Box("moov", be32(4), []byte("mvhd"))), production},
		{"large size past the parent", cat(ftyp, mp4Box("moov", be32(1), []byte("mvhd"), be32(0), be32(4096))), production},
		{"large size under its header", cat(ftyp, mp4Box("moov", be32(1), []byte("mvhd"), be32(0), be32(8))), production},
		{"large size over the int64 range", cat(ftyp, mp4Box("moov", be32(1), []byte("mvhd"), be32(math.MaxUint32), be32(math.MaxUint32))), production},
		{"garbage before moov", cat(ftyp, []byte("<html>junk</html>"), mp4Box("moov", mp4Box("mvhd"))), production},
		{"containers nested too deep", cat(ftyp, mp4Box("moov", nest)), production},
		{"too many boxes", cat(ftyp, mp4Box("moov", bytes.Repeat(mp4Box("free"), 10))), few},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSpool(tt.data)
			if _, err := apply(t.Context(), kindVideo, "", s, tt.lim); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("apply error = %v, want ErrUnsupported", err)
			}
			if !bytes.Equal(s.b, tt.data) {
				t.Fatal("a refused video changed")
			}
		})
	}
}

func TestOneNormalisationAtATime(t *testing.T) {
	data := encodeJPEG(t, filled(8, 8, red))
	slot <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := Apply(ctx, kindDocument, "", newSpool(data))
	<-slot
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply while another normalisation runs = %v, want the caller's deadline", err)
	}
	if out, err := Apply(t.Context(), kindDocument, "", newSpool(data)); err != nil || !out.Normalised {
		t.Fatalf("Apply after the slot freed = %+v, %v", out, err)
	}
	if len(slot) != 0 {
		t.Fatal("Apply kept the normalisation slot")
	}
}

type hookSpool struct {
	*memSpool
	reads  int
	onRead func(int)
}

func (h *hookSpool) ReadAt(p []byte, off int64) (int, error) {
	h.reads++
	h.onRead(h.reads)
	return h.memSpool.ReadAt(p, off)
}

func TestCancellationIsNeverARefusal(t *testing.T) {
	data := encodeJPEG(t, filled(64, 64, red))
	done, cancel := context.WithCancel(t.Context())
	cancel()
	for kind := range variants {
		if _, err := Apply(done, kind, "", newSpool(data)); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s with a cancelled context: %v", kind, err)
		}
	}
	for after := 1; after <= 6; after++ {
		ctx, cancel := context.WithCancel(t.Context())
		s := &hookSpool{memSpool: newSpool(data), onRead: func(n int) {
			if n == after {
				cancel()
			}
		}}
		out, err := Apply(ctx, kindDocument, "", s)
		cancel()
		switch {
		case errors.Is(err, context.Canceled) && out.ServedType == "":
		case err == nil && out.Normalised && after > 2:
		default:
			t.Fatalf("cancelled during read %d: %+v, %v, want the cancellation or the finished image, never a raw document", after, out, err)
		}
	}
}

func TestSpoolFailuresAreNotRefusals(t *testing.T) {
	jpegBytes := encodeJPEG(t, filled(8, 8, red))
	video := mp4Fixture(ftypBox("isom", "isom", "mp42"), mp4Box("mdat", []byte("x")))
	for _, tt := range []struct {
		name, kind string
		data       []byte
		spool      func(*memSpool)
		after      int
	}{
		{"document head", kindDocument, jpegBytes, func(s *memSpool) { s.readErr = errTestSpool }, 0},
		{"document decode", kindDocument, jpegBytes, nil, 2},
		{"image decode", kindImage, jpegBytes, nil, 3},
		{"audio head", kindAudio, jpegBytes, func(s *memSpool) { s.readErr = errTestSpool }, 0},
		{"video walk", kindVideo, video, nil, 3},
		{"video write", kindVideo, video, func(s *memSpool) { s.writeErr = errTestSpool }, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newSpool(tt.data)
			if tt.spool != nil {
				tt.spool(m)
			}
			s := &hookSpool{memSpool: m, onRead: func(n int) {
				if tt.after > 0 && n >= tt.after {
					m.readErr = errTestSpool
				}
			}}
			out, err := Apply(t.Context(), tt.kind, "", s)
			if !errors.Is(err, ErrSpool) || out.ServedType != "" {
				t.Fatalf("Apply = %+v, %v, want ErrSpool", out, err)
			}
			if strings.Contains(err.Error(), errTestSpool.Error()) {
				t.Fatal("the error carries the spool's own text")
			}
		})
	}
}

func interlacedPNG(p []byte) []byte {
	out := bytes.Clone(p)
	out[28] = 1
	copy(out[29:33], be32(int(crc32.ChecksumIEEE(out[12:29]))))
	return out
}

func TestDecoderMemoryPerColourModel(t *testing.T) {
	const rows = 2 * (8*10 + 1)
	rgba := encodePNG(t, filled(10, 4, red))
	for _, tt := range []struct {
		name string
		data []byte
		want int64
	}{
		{"paletted png", encodePNG(t, image.NewPaletted(image.Rect(0, 0, 10, 4), color.Palette{red, blue})), 10*4*1 + rows},
		{"greyscale png", encodePNG(t, image.NewGray(image.Rect(0, 0, 10, 4))), 10*4*4 + rows},
		{"16-bit greyscale png", encodePNG(t, image.NewGray16(image.Rect(0, 0, 10, 4))), 10*4*8 + rows},
		{"16-bit colour png", encodePNG(t, image.NewNRGBA64(image.Rect(0, 0, 10, 4))), 10*4*8 + rows},
		{"truecolour png", rgba, 10*4*4 + rows},
		{"interlaced truecolour png", interlacedPNG(rgba), 10*4*4*3/2 + rows},
		{"gif", encodeGIF(t, 10, 4, red), 2 * 10 * 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &source{ctx: t.Context(), s: newSpool(tt.data)}
			head, err := src.head()
			if err != nil {
				t.Fatalf("head: %v", err)
			}
			var p plan
			if imageCodec(head) == codecGIF {
				p, err = planGIF(src)
			} else {
				p, err = planPNG(src, head)
			}
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got := p.memory(); got != tt.want {
				t.Fatalf("memory = %d, want %d", got, tt.want)
			}
		})
	}
	for _, tt := range []struct {
		name  string
		frame webpFrame
		want  int64
	}{
		{"lossless", webpFrame{size: image.Pt(10, 4), lossless: true}, 9 * 10 * 4},
		{"lossy", webpFrame{size: image.Pt(20, 17), bits: chunk{n: 100}}, 256*2*2*3/2 + 16*2*2 + 100},
		{"lossy with raw alpha", webpFrame{size: image.Pt(20, 17), bits: chunk{n: 100}, alpha: alphaRaw}, 256*2*2*3/2 + 16*2*2 + 100 + 20*17},
		{"lossy with lossless alpha", webpFrame{size: image.Pt(20, 17), bits: chunk{n: 100}, alpha: alphaLossless}, 256*2*2*3/2 + 16*2*2 + 100 + 10*20*17},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.frame.memory(); got != tt.want {
				t.Fatalf("memory = %d, want %d", got, tt.want)
			}
		})
	}
}
