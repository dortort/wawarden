//go:build dev

package fake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/dortort/wawarden/internal/engine"
)

const (
	refPhoto       = "FAKE-MEDIA-PHOTO"
	refVoiceNote   = "FAKE-MEDIA-VOICE-NOTE"
	refVideo       = "FAKE-MEDIA-VIDEO"
	refPDF         = "FAKE-MEDIA-PDF"
	refSticker     = "FAKE-MEDIA-STICKER"
	refJPEGDoc     = "FAKE-MEDIA-JPEG-DOCUMENT"
	refCorruptDoc  = "FAKE-MEDIA-CORRUPT-JPEG-DOCUMENT"
	refExpired     = "FAKE-MEDIA-EXPIRED"
	largeMediaSize = 8<<20 + 64<<10

	jpegFixture = "" +
		"\xff\xd8\xff\xdb\x00\x84\x00\x10\x0b\x0c\x0e\x0c\x0a\x10\x0e\x0d\x0e\x12\x11\x10\x13\x18\x28\x1a\x18\x16\x16\x18\x31\x23\x25\x1d" +
		"\x28\x3a\x33\x3d\x3c\x39\x33\x38\x37\x40\x48\x5c\x4e\x40\x44\x57\x45\x37\x38\x50\x6d\x51\x57\x5f\x62\x67\x68\x67\x3e\x4d\x71\x79" +
		"\x70\x64\x78\x5c\x65\x67\x63\x01\x11\x12\x12\x18\x15\x18\x2f\x1a\x1a\x2f\x63\x42\x38\x42\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63" +
		"\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63\x63" +
		"\x63\x63\x63\x63\x63\x63\x63\x63\xff\xc0\x00\x0b\x08\x00\x08\x00\x08\x01\x01\x11\x00\xff\xc4\x00\xd2\x00\x00\x01\x05\x01\x01\x01" +
		"\x01\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x10\x00\x02\x01\x03\x03\x02\x04\x03\x05" +
		"\x05\x04\x04\x00\x00\x01\x7d\x01\x02\x03\x00\x04\x11\x05\x12\x21\x31\x41\x06\x13\x51\x61\x07\x22\x71\x14\x32\x81\x91\xa1\x08\x23" +
		"\x42\xb1\xc1\x15\x52\xd1\xf0\x24\x33\x62\x72\x82\x09\x0a\x16\x17\x18\x19\x1a\x25\x26\x27\x28\x29\x2a\x34\x35\x36\x37\x38\x39\x3a" +
		"\x43\x44\x45\x46\x47\x48\x49\x4a\x53\x54\x55\x56\x57\x58\x59\x5a\x63\x64\x65\x66\x67\x68\x69\x6a\x73\x74\x75\x76\x77\x78\x79\x7a" +
		"\x83\x84\x85\x86\x87\x88\x89\x8a\x92\x93\x94\x95\x96\x97\x98\x99\x9a\xa2\xa3\xa4\xa5\xa6\xa7\xa8\xa9\xaa\xb2\xb3\xb4\xb5\xb6\xb7" +
		"\xb8\xb9\xba\xc2\xc3\xc4\xc5\xc6\xc7\xc8\xc9\xca\xd2\xd3\xd4\xd5\xd6\xd7\xd8\xd9\xda\xe1\xe2\xe3\xe4\xe5\xe6\xe7\xe8\xe9\xea\xf1" +
		"\xf2\xf3\xf4\xf5\xf6\xf7\xf8\xf9\xfa\xff\xda\x00\x08\x01\x01\x00\x00\x3f\x00\x8f\x44\xfe\x1a\xff\xd9"

	webpFixture = "" +
		"\x52\x49\x46\x46\x18\x00\x00\x00\x57\x45\x42\x50\x56\x50\x38\x4c\x0c\x00\x00\x00\x2f\x0f\xc0\x03\x10\xe8\x5f\x55\x4a\xdd\xff\x00"

	oggFixture = "" +
		"\x4f\x67\x67\x53\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00\x41\x57\x41\x57\x00\x00\x00\x00\x05\xc0\x3d\x57\x01\x13\x4f\x70\x75\x73" +
		"\x48\x65\x61\x64\x01\x01\x38\x01\x80\xbb\x00\x00\x00\x00\x00\x4f\x67\x67\x53\x00\x04\x00\x00\x00\x00\x00\x00\x00\x00\x41\x57\x41" +
		"\x57\x01\x00\x00\x00\x91\x59\x26\xeb\x01\x2d\x4f\x70\x75\x73\x54\x61\x67\x73\x1d\x00\x00\x00\x77\x61\x77\x61\x72\x64\x65\x6e\x20" +
		"\x73\x79\x6e\x74\x68\x65\x74\x69\x63\x20\x76\x6f\x69\x63\x65\x20\x6e\x6f\x74\x65\x00\x00\x00\x00"
)

var errStagedRange = errors.New("fake: the staged attachment was read or written out of range")

var mediaFixtures = map[string]func() []byte{
	refPhoto:      func() []byte { return []byte(jpegFixture) },
	refVoiceNote:  func() []byte { return []byte(oggFixture) },
	refVideo:      mp4Fixture,
	refPDF:        pdfFixture,
	refSticker:    func() []byte { return []byte(webpFixture) },
	refJPEGDoc:    func() []byte { return []byte(jpegFixture) },
	refCorruptDoc: func() []byte { return []byte(jpegFixture[:len(jpegFixture)/2]) },
}

func mediaMessages() []engine.Message {
	attachment := func(id string, kind engine.MediaKind, variant, mediaType, ref string, size int) engine.Message {
		return engine.Message{Chat: alice, ID: id, Sender: alice, Kind: engine.KindMedia, MediaType: mediaType, MediaRef: ref, MediaKind: kind,
			MediaVariant: variant, MediaBytes: int64(size)}
	}
	return []engine.Message{
		attachment("FAKE-L22", engine.MediaAudio, engine.VariantVoiceNote, "audio/ogg", refVoiceNote, len(oggFixture)),
		attachment("FAKE-L23", engine.MediaVideo, "", "video/mp4", refVideo, largeMediaSize),
		attachment("FAKE-L24", engine.MediaDocument, "", "application/pdf", refPDF, largeMediaSize),
		attachment("FAKE-L25", engine.MediaSticker, "", "image/webp", refSticker, len(webpFixture)),
		attachment("FAKE-L26", engine.MediaDocument, "", "image/jpeg", refJPEGDoc, len(jpegFixture)),
		attachment("FAKE-L27", engine.MediaDocument, "", "image/jpeg", refCorruptDoc, len(jpegFixture)/2),
		attachment("FAKE-L28", engine.MediaImage, "", "image/jpeg", refExpired, len(jpegFixture)),
	}
}

func (c *Client) FetchMedia(ctx context.Context, req engine.MediaRequest) (engine.Staged, error) {
	c.mu.Lock()
	connected := ctx.Err() == nil && c.connected
	c.mu.Unlock()
	if !connected {
		return nil, engine.ErrNotConnected
	}
	if req.Ref == refExpired {
		return nil, engine.ErrMediaExpired
	}
	fixture, ok := mediaFixtures[req.Ref]
	if !ok {
		return nil, engine.ErrMediaUnavailable
	}
	return &staged{b: fixture()}, nil
}

func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	n := 8 + len(body)
	return bytes.Join([][]byte{{byte(n >> 24 & 0xff), byte(n >> 16 & 0xff), byte(n >> 8 & 0xff), byte(n & 0xff)}, []byte(typ), body}, nil)
}

func mp4Fixture() []byte {
	head := bytes.Join([][]byte{
		box("ftyp", []byte("mp42"), make([]byte, 4), []byte("mp42isom")),
		box("moov", box("mvhd", make([]byte, 100)), box("udta", box("name", []byte("A synthetic WaWarden video")))),
	}, nil)
	filler := bytes.Repeat([]byte("synthetic video "), largeMediaSize/16)
	return append(head, box("mdat", filler[:largeMediaSize-len(head)-8])...)
}

func pdfFixture() []byte {
	content := "BT /F1 12 Tf 20 45 Td (A synthetic WaWarden document) Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 100] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Length " + decimal(len(content)) + " >>\nstream\n" + content + "\nendstream",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.Grow(largeMediaSize)
	b.WriteString("%PDF-1.4\n")
	xref := "xref\n0 " + decimal(len(objects)+1) + "\n0000000000 65535 f \n"
	for i, o := range objects {
		xref += zeroPadded(b.Len(), 10) + " 00000 n \n"
		b.WriteString(decimal(i+1) + " 0 obj\n" + o + "\nendobj\n")
	}
	tail := "trailer\n<< /Size " + decimal(len(objects)+1) + " /Root 1 0 R >>\nstartxref\n"
	end := "\n%%EOF\n"
	line := []byte("% synthetic padding that sends this document in two parts          ")
	for left := largeMediaSize - b.Len() - len(xref) - len(tail) - len(decimal(largeMediaSize)) - len(end); left > 0; {
		n := min(len(line), left)
		b.Write(line[:n-1])
		b.WriteByte('\n')
		left -= n
	}
	start := b.Len()
	b.WriteString(xref + tail + decimal(start) + end)
	return b.Bytes()
}

func zeroPadded(n, width int) string {
	s := decimal(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

type staged struct {
	mu sync.Mutex
	b  []byte
}

func (s *staged) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.b))
}

func (s *staged) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case off < 0:
		return 0, errStagedRange
	case off >= int64(len(s.b)):
		return 0, io.EOF
	}
	n := copy(p, s.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *staged) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off < 0 || off+int64(len(p)) > int64(len(s.b)) {
		return 0, errStagedRange
	}
	return copy(s.b[off:], p), nil
}

func (s *staged) Truncate(size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if size < 0 || size > int64(len(s.b)) {
		return errStagedRange
	}
	s.b = s.b[:size]
	return nil
}

func (s *staged) Erase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.b)
	s.b = nil
}
