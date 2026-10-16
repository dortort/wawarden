// Package attach is the content policy for downloaded attachments: the served type, the format word, the image normaliser and the MP4 scrub.
package attach

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

var (
	ErrUnsupported = errors.New("attach: media unsupported")
	ErrSpool       = errors.New("attach: spool access failed")
)

type Spool interface {
	Size() int64
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Truncate(size int64) error
}

type Outcome struct {
	ServedType string
	Format     string
	Normalised bool
	Image      []byte
}

const (
	kindImage    = "image"
	kindVideo    = "video"
	kindAudio    = "audio"
	kindDocument = "document"
	kindSticker  = "sticker"

	variantLottie = "lottie"

	typeOctetStream = "application/octet-stream"
	typePDF         = "application/pdf"
	typeJPEG        = "image/jpeg"
	typePNG         = "image/png"
	typeMP4         = "video/mp4"

	formatImage      = "image"
	formatAudio      = "audio"
	formatVideo      = "video"
	formatPDF        = "pdf"
	formatText       = "text"
	formatZip        = "zip"
	formatOLE2       = "ole2"
	formatArchive    = "archive"
	formatExecutable = "executable"
	formatUnknown    = "unknown"

	codecJPEG = "jpeg"
	codecPNG  = "png"
	codecGIF  = "gif"
	codecWebP = "webp"

	headSize = 512
)

var variants = map[string][]string{
	kindImage:    {""},
	kindVideo:    {"", "gif", "video_note"},
	kindAudio:    {"", "voice_note"},
	kindDocument: {""},
	kindSticker:  {"", "animated", variantLottie},
}

var audioTypes = map[string]string{
	"application/ogg": "audio/ogg",
	"audio/mpeg":      "audio/mpeg",
	typeMP4:           "audio/mp4",
}

func Apply(ctx context.Context, kind, variant string, s Spool) (Outcome, error) {
	return apply(ctx, kind, variant, s, production)
}

func apply(ctx context.Context, kind, variant string, s Spool, lim limits) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if !slices.Contains(variants[kind], variant) || s.Size() < 0 {
		return Outcome{}, ErrUnsupported
	}
	src := &source{ctx: ctx, s: s}
	head, err := src.head()
	if err != nil {
		return Outcome{}, src.fail(err, ErrSpool)
	}
	switch {
	case kind == kindVideo:
		return scrubbed(src, lim)
	case kind == kindAudio:
		served, ok := audioTypes[http.DetectContentType(head)]
		if !ok {
			served = typeOctetStream
		}
		return Outcome{ServedType: served, Format: format(head)}, nil
	case kind == kindSticker && variant == variantLottie:
		return Outcome{ServedType: typeOctetStream, Format: format(head)}, nil
	case kind == kindDocument && imageCodec(head) == "":
		served := typeOctetStream
		if bytes.HasPrefix(head, []byte("%PDF-")) {
			served = typePDF
		}
		return Outcome{ServedType: served, Format: format(head)}, nil
	}
	img, served, err := normalise(src, head, lim)
	switch {
	case err == nil:
		return Outcome{ServedType: served, Format: formatImage, Normalised: true, Image: img}, nil
	case !errors.Is(err, errRefused):
		return Outcome{}, err
	case kind == kindDocument:
		return Outcome{ServedType: typeOctetStream, Format: formatImage}, nil
	}
	return Outcome{}, ErrUnsupported
}

func scrubbed(src *source, lim limits) (Outcome, error) {
	if err := scrubMP4(src, lim); err != nil {
		return Outcome{}, err
	}
	head, err := src.head()
	if err != nil {
		return Outcome{}, src.fail(err, ErrSpool)
	}
	served := typeOctetStream
	if http.DetectContentType(head) == typeMP4 {
		served = typeMP4
	}
	return Outcome{ServedType: served, Format: formatVideo}, nil
}

type source struct {
	ctx context.Context
	s   Spool
	err error
}

func (src *source) ReadAt(p []byte, off int64) (int, error) {
	if err := src.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := src.s.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) && src.err == nil {
		src.err = err
	}
	return n, err
}

func (src *source) readFull(p []byte, off int64) error {
	n, err := src.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (src *source) head() ([]byte, error) {
	head := make([]byte, min(headSize, src.s.Size()))
	return head, src.readFull(head, 0)
}

func (src *source) fail(err, refusal error) error {
	switch {
	case err == nil:
		return nil
	case src.ctx.Err() != nil:
		return src.ctx.Err()
	case src.err != nil:
		return ErrSpool
	}
	return refusal
}

func imageCodec(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return codecJPEG
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return codecPNG
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return codecGIF
	case riffForm(head, "WEBP"):
		return codecWebP
	}
	return ""
}

type magic struct {
	format string
	match  func([]byte) bool
}

var magics = []magic{
	{formatImage, func(b []byte) bool { return imageCodec(b) != "" }},
	{formatImage, prefixed("II*\x00", "MM\x00*")},
	{formatImage, isBMP},
	{formatImage, isICO},
	{formatAudio, prefixed("OggS", "ID3", "fLaC", "#!AMR")},
	{formatAudio, isMPEGAudio},
	{formatAudio, func(b []byte) bool { return riffForm(b, "WAVE") }},
	{formatAudio, func(b []byte) bool { return ftypBrand(b) == "M4A " || ftypBrand(b) == "M4B " || ftypBrand(b) == "M4P " }},
	{formatVideo, func(b []byte) bool { return ftypBrand(b) != "" }},
	{formatVideo, prefixed("\x1a\x45\xdf\xa3")},
	{formatVideo, func(b []byte) bool { return riffForm(b, "AVI ") }},
	{formatPDF, prefixed("%PDF-")},
	{formatZip, prefixed("PK\x03\x04", "PK\x05\x06", "PK\x07\x08")},
	{formatOLE2, prefixed("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")},
	{formatArchive, prefixed("\x1f\x8b", "Rar!\x1a\x07", "7z\xbc\xaf\x27\x1c", "\xfd7zXZ\x00", "BZh", "\x28\xb5\x2f\xfd")},
	{formatArchive, func(b []byte) bool { return len(b) >= 262 && string(b[257:262]) == "ustar" }},
	{formatExecutable, prefixed("MZ", "\x7fELF", "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe", "#!")},
	{formatText, func(b []byte) bool { return strings.HasPrefix(http.DetectContentType(b), "text/") }},
}

func format(head []byte) string {
	for _, m := range magics {
		if m.match(head) {
			return m.format
		}
	}
	return formatUnknown
}

func prefixed(sigs ...string) func([]byte) bool {
	return func(b []byte) bool {
		return slices.ContainsFunc(sigs, func(sig string) bool { return bytes.HasPrefix(b, []byte(sig)) })
	}
}

func riffForm(b []byte, form string) bool {
	return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == form
}

func ftypBrand(b []byte) string {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return ""
	}
	return string(b[8:12])
}

func isBMP(b []byte) bool {
	if len(b) < 18 || string(b[:2]) != "BM" {
		return false
	}
	switch binary.LittleEndian.Uint32(b[14:18]) {
	case 12, 40, 52, 56, 64, 108, 124:
		return true
	}
	return false
}

func isICO(b []byte) bool {
	return len(b) >= 6 && string(b[:4]) == "\x00\x00\x01\x00" && binary.LittleEndian.Uint16(b[4:6]) > 0
}

func isMPEGAudio(b []byte) bool {
	if len(b) < 3 || b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return false
	}
	if b[1]&0x06 == 0 {
		return b[1]&0xf6 == 0xf0
	}
	return b[1]&0x18 != 0x08 && b[2]&0xf0 != 0xf0
}
