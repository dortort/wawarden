package attach

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"slices"
	"strings"
	"testing"
)

var fuzzLimits = limits{pixels: 1 << 16, budget: 4 << 20, edge: 64, output: 256 << 10, quality: 85, units: 1024, depth: 8}

var formats = []string{formatImage, formatAudio, formatVideo, formatPDF, formatText, formatZip, formatOLE2, formatArchive, formatExecutable, formatUnknown}

func imageSeeds(f *testing.F) [][]byte {
	f.Helper()
	jpegBytes := encodeJPEG(f, quadrants(24, 16, red, green, blue, white))
	frame := riffChunk("VP8L", vp8lSolid(6, 4, color.NRGBA{R: 0xff, A: 0x80}))
	seeds := [][]byte{
		jpegBytes,
		withExif(jpegBytes, 6),
		append(withExif(jpegBytes, 3), "PK\x03\x04"+trailerMarker...),
		insertSegment(jpegBytes, len(jpegBytes)-2, 0xee, cat([]byte("Adobe"), make([]byte, 7))),
		patchSOF(f, jpegBytes, 300, 300, true),
		jpegBytes[:len(jpegBytes)/2],
		encodePNG(f, filled(20, 12, color.NRGBA{G: 0xff, A: 0x40})),
		encodeGIF(f, 10, 8, red, blue),
		webpFile(frame),
		webpFile(vp8xChunk(0x10, 6, 4), riffChunk("ALPH", []byte{0, 1, 2}), riffChunk("VP8 ", []byte("synthetic"))),
		animatedWebP(12, 8, anmfChunk(6, 4, 6, 4, frame), anmfChunk(0, 0, 6, 4, frame)),
	}
	return seeds
}

func FuzzFormatAndServedType(f *testing.F) {
	for _, s := range imageSeeds(f) {
		f.Add(s[:min(len(s), headSize)])
	}
	f.Add([]byte("%PDF-1.7"))
	f.Add([]byte("<!DOCTYPE html>"))
	f.Add([]byte("OggS\x00\x02"))
	f.Add(cat(be32(20), []byte("ftypM4A "), be32(0), []byte("mp42"), make([]byte, 4)))
	f.Fuzz(func(t *testing.T, data []byte) {
		if got := format(data); !slices.Contains(formats, got) {
			t.Fatalf("format = %q", got)
		}
		for _, k := range []struct{ kind, variant string }{{kindAudio, ""}, {kindAudio, "voice_note"}, {kindSticker, variantLottie}, {kindDocument, ""}} {
			if k.kind == kindDocument && imageCodec(data) != "" {
				continue
			}
			s := newSpool(data)
			out, err := apply(t.Context(), k.kind, k.variant, s, fuzzLimits)
			if err != nil || out.Normalised || out.Image != nil || !bytes.Equal(s.b, data) {
				t.Fatalf("%s: %+v, %v", k.kind, out, err)
			}
			if strings.HasPrefix(out.ServedType, "image/") || strings.HasPrefix(out.ServedType, "text/") || out.ServedType == "" {
				t.Fatalf("%s served as %q", k.kind, out.ServedType)
			}
		}
	})
}

func FuzzScanJPEG(f *testing.F) {
	for _, s := range imageSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := scanJPEG(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			return
		}
		if info.orientation < 1 || info.orientation > 8 || len(info.comps) == 0 || info.memory() < 0 {
			t.Fatalf("scanJPEG accepted %+v", info)
		}
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data)); err == nil && (cfg.Width != info.width || cfg.Height != info.height) {
			t.Fatalf("scanJPEG read %dx%d, the decoder %dx%d", info.width, info.height, cfg.Width, cfg.Height)
		}
	})
}

func FuzzEXIFOrientation(f *testing.F) {
	f.Add(exifTIFF(binary.LittleEndian, 6))
	f.Add(exifTIFF(binary.BigEndian, 8))
	f.Add([]byte("II*\x00\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, tiff []byte) {
		if got := exifOrientation(tiff); got < 1 || got > 8 {
			t.Fatalf("exifOrientation = %d", got)
		}
	})
}

func FuzzWebPFrame(f *testing.F) {
	for _, s := range imageSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		src := &source{ctx: t.Context(), s: newSpool(data)}
		fr, err := parseWebP(src, fuzzLimits)
		if err != nil {
			return
		}
		if fr.bits.off+fr.bits.n > int64(len(data)) || fr.alph.off+fr.alph.n > int64(len(data)) || fr.memory() < 0 {
			t.Fatalf("parseWebP accepted %+v for %d bytes", fr, len(data))
		}
		switch {
		case fr.size == image.Point{}:
			if fr.alpha != alphaNone || !fr.canvas.Empty() {
				t.Fatalf("parseWebP accepted a simple frame with %+v", fr)
			}
		case fr.size.X < 1 || fr.size.Y < 1 || !(image.Rectangle{Max: fr.size}).In(fr.canvas):
			t.Fatalf("frame %v outside its canvas %v", fr.size, fr.canvas)
		}
		stream, err := io.ReadAll(fr.stream(src))
		if err != nil || len(stream) > len(data)+18 {
			t.Fatalf("stream of %d bytes from %d: %v", len(stream), len(data), err)
		}
	})
}

func FuzzNormalise(f *testing.F) {
	kinds := []string{kindImage, kindSticker, kindDocument}
	for _, s := range imageSeeds(f) {
		for k := range kinds {
			f.Add(uint8(k), s)
		}
	}
	f.Fuzz(func(t *testing.T, k uint8, data []byte) {
		kind := kinds[int(k)%len(kinds)]
		s := newSpool(data)
		out, err := apply(t.Context(), kind, "", s, fuzzLimits)
		if !bytes.Equal(s.b, data) {
			t.Fatal("the policy wrote to an image spool")
		}
		if err != nil {
			if !errors.Is(err, ErrUnsupported) || kind == kindDocument {
				t.Fatalf("%s: %v", kind, err)
			}
			return
		}
		if !out.Normalised {
			if kind != kindDocument || out.Image != nil || out.ServedType != typeOctetStream && out.ServedType != typePDF {
				t.Fatalf("%s: unnormalised %+v", kind, out)
			}
			return
		}
		if out.Format != formatImage || len(out.Image) > fuzzLimits.output {
			t.Fatalf("%s: %q %q with %d bytes", kind, out.ServedType, out.Format, len(out.Image))
		}
		var cfgW, cfgH int
		switch out.ServedType {
		case typeJPEG:
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(out.Image))
			if err != nil {
				t.Fatalf("the normalised JPEG does not decode: %v", err)
			}
			cfgW, cfgH = cfg.Width, cfg.Height
		case typePNG:
			cfg, err := png.DecodeConfig(bytes.NewReader(out.Image))
			if err != nil {
				t.Fatalf("the normalised PNG does not decode: %v", err)
			}
			cfgW, cfgH = cfg.Width, cfg.Height
		default:
			t.Fatalf("normalised served as %q", out.ServedType)
		}
		if int64(max(cfgW, cfgH)) > fuzzLimits.edge || cfgW < 1 || cfgH < 1 {
			t.Fatalf("normalised to %dx%d", cfgW, cfgH)
		}
	})
}

func FuzzMP4Scrub(f *testing.F) {
	file := mp4Fixture(ftypBox("isom", "isom", "mp42"), mp4Box("mdat", []byte("samples")))
	f.Add(file)
	f.Add(cat(file, []byte("<html>"+trailerMarker+"</html>")))
	f.Add(cat(file[:len(file)-15], be32(1), []byte("mdat"), be32(0), be32(23), []byte("samples")))
	f.Add(cat(ftypBox("isom"), mp4Box("moov", be32(64), []byte("udta"))))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := newSpool(data)
		out, err := apply(t.Context(), kindVideo, "", s, fuzzLimits)
		if err != nil {
			if !errors.Is(err, ErrUnsupported) || !bytes.Equal(s.b, data) {
				t.Fatalf("a refused video: %v, changed %v", err, !bytes.Equal(s.b, data))
			}
			return
		}
		if out.Format != formatVideo || out.Normalised || out.ServedType != typeMP4 && out.ServedType != typeOctetStream {
			t.Fatalf("scrubbed %+v", out)
		}
		if len(s.b) > len(data) {
			t.Fatalf("the scrub grew %d bytes to %d", len(data), len(s.b))
		}
		for i := range s.b {
			if s.b[i] == data[i] || s.b[i] == 0 {
				continue
			}
			j := bytes.LastIndex(s.b[max(0, i-3):min(len(s.b), i+4)], freeType)
			if j < 0 {
				t.Fatalf("byte %d changed from %#x to %#x outside a blanked box", i, data[i], s.b[i])
			}
			at := max(0, i-3) + j
			if was := string(data[at : at+4]); !blankedBoxes[was] {
				t.Fatalf("a %q box at %d became free", was, at)
			}
		}
		again := newSpool(s.b)
		if _, err := apply(t.Context(), kindVideo, "", again, fuzzLimits); err != nil || !bytes.Equal(again.b, s.b) {
			t.Fatalf("a second scrub: %v, changed %v", err, !bytes.Equal(again.b, s.b))
		}
	})
}
