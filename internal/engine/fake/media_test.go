//go:build dev

package fake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/attach"
	"github.com/dortort/wawarden/internal/engine"
)

func scriptedAttachments(t *testing.T, c *Client) []engine.Message {
	t.Helper()
	var out []engine.Message
	for _, ev := range c.script {
		if m, ok := ev.(engine.Message); ok && m.MediaRef != "" {
			out = append(out, m)
		}
	}
	return out
}

func linked(t *testing.T) *Client {
	t.Helper()
	c, _ := newClient(t, false)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return c
}

func TestEachScriptedAttachmentGetsItsIntendedOutcome(t *testing.T) {
	c := linked(t)
	want := map[string]attach.Outcome{
		"FAKE-L18": {ServedType: "image/jpeg", Format: "image", Normalised: true},
		"FAKE-L22": {ServedType: "audio/ogg", Format: "audio"},
		"FAKE-L23": {ServedType: "video/mp4", Format: "video"},
		"FAKE-L24": {ServedType: "application/pdf", Format: "pdf"},
		"FAKE-L25": {ServedType: "image/jpeg", Format: "image", Normalised: true},
		"FAKE-L26": {ServedType: "image/jpeg", Format: "image", Normalised: true},
		"FAKE-L27": {ServedType: "application/octet-stream", Format: "image"},
	}
	kinds := map[engine.MediaKind]bool{}
	images := map[string][]byte{}
	var seen []string
	for _, m := range scriptedAttachments(t, c) {
		kinds[m.MediaKind] = true
		if m.MediaRef == refExpired {
			continue
		}
		seen = append(seen, m.ID)
		s, err := c.FetchMedia(t.Context(), engine.MediaRequest{ID: m.ID, Ref: m.MediaRef, Kind: m.MediaKind, Declared: m.MediaBytes})
		if err != nil || s.Size() != m.MediaBytes {
			t.Fatalf("FetchMedia(%s) = %v, want %d bytes", m.ID, err, m.MediaBytes)
		}
		out, err := attach.Apply(t.Context(), string(m.MediaKind), m.MediaVariant, s)
		if err != nil {
			t.Fatalf("Apply(%s) = %v", m.ID, err)
		}
		images[m.ID], out.Image = out.Image, nil
		if !reflect.DeepEqual(out, want[m.ID]) {
			t.Errorf("%s (%s) gets %+v, want %+v", m.ID, m.MediaKind, out, want[m.ID])
		}
		if out.Normalised != (len(images[m.ID]) > 0) {
			t.Errorf("%s is normalised %v with %d image bytes", m.ID, out.Normalised, len(images[m.ID]))
		}
	}
	if slices.Sort(seen); !slices.Equal(seen, slices.Sorted(func(yield func(string) bool) {
		for id := range want {
			if !yield(id) {
				return
			}
		}
	})) {
		t.Fatalf("the script fetches %v, want every fixture once", seen)
	}
	for _, k := range []engine.MediaKind{engine.MediaImage, engine.MediaVideo, engine.MediaAudio, engine.MediaDocument, engine.MediaSticker} {
		if !kinds[k] {
			t.Errorf("the script has no %s attachment", k)
		}
	}
	if !bytes.Equal(images["FAKE-L18"], images["FAKE-L26"]) {
		t.Error("the JPEG sent as a document is not normalised to the photo's bytes")
	}
}

func TestTheVideoFixtureLosesItsMetadataAndKeepsItsSize(t *testing.T) {
	s := &staged{b: mp4Fixture()}
	if _, err := attach.Apply(t.Context(), "video", "", s); err != nil {
		t.Fatalf("Apply = %v", err)
	}
	if s.Size() != largeMediaSize || bytes.Contains(s.b, []byte("A synthetic WaWarden video")) || !bytes.Contains(s.b, []byte("free")) {
		t.Fatalf("the scrubbed video holds %d bytes and its metadata %v", s.Size(), bytes.Contains(s.b, []byte("A synthetic WaWarden video")))
	}
}

func TestTheLargeFixturesComeInTwoParts(t *testing.T) {
	for name, b := range map[string][]byte{"video": mp4Fixture(), "document": pdfFixture()} {
		if len(b) != largeMediaSize || largeMediaSize <= 8<<20 || largeMediaSize > 16<<20 {
			t.Errorf("the %s fixture holds %d bytes, want %d: over one 8 MiB part and under the 16 MiB memory threshold", name, len(b), largeMediaSize)
		}
	}
}

func TestThePDFFixtureIsAWellFormedDocument(t *testing.T) {
	b := pdfFixture()
	if !bytes.HasPrefix(b, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(b, []byte("\n%%EOF\n")) {
		t.Fatal("the document lacks its header or its end marker")
	}
	tail := b[bytes.LastIndex(b, []byte("startxref\n"))+len("startxref\n"):]
	start, err := strconv.Atoi(string(tail[:bytes.IndexByte(tail, '\n')]))
	if err != nil || !bytes.HasPrefix(b[start:], []byte("xref\n0 6\n0000000000 65535 f \n")) {
		t.Fatalf("startxref %d does not point at the cross-reference table: %v", start, err)
	}
	rows := strings.Split(string(b[start:]), "\n")[3:8]
	for i, row := range rows {
		off, err := strconv.Atoi(row[:10])
		if err != nil || len(row) != 19 || !bytes.HasPrefix(b[off:], []byte(strconv.Itoa(i+1)+" 0 obj\n")) {
			t.Errorf("cross-reference row %d %q does not point at object %d", i+1, row, i+1)
		}
	}
	stream := b[bytes.Index(b, []byte("stream\n"))+len("stream\n"):]
	if end := bytes.Index(stream, []byte("\nendstream")); end != len("BT /F1 12 Tf 20 45 Td (A synthetic WaWarden document) Tj ET") || !bytes.Contains(b, []byte("/Length "+strconv.Itoa(end)+" >>")) {
		t.Errorf("the content stream holds %d bytes, which its length does not state", end)
	}
}

func TestFetchesNeedAConnectionAndAKnownLiveReference(t *testing.T) {
	c, _ := newClient(t, false)
	if _, err := c.FetchMedia(t.Context(), engine.MediaRequest{Ref: refPhoto}); !errors.Is(err, engine.ErrNotConnected) {
		t.Fatalf("a fetch before a connection = %v", err)
	}
	c = linked(t)
	for ref, want := range map[string]error{refExpired: engine.ErrMediaExpired, "FAKE-MEDIA-UNKNOWN": engine.ErrMediaUnavailable} {
		if s, err := c.FetchMedia(t.Context(), engine.MediaRequest{Ref: ref}); !errors.Is(err, want) || s != nil {
			t.Errorf("a fetch of %s = %v, %v, want %v", ref, s, err, want)
		}
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.FetchMedia(ended, engine.MediaRequest{Ref: refPhoto}); !errors.Is(err, engine.ErrNotConnected) {
		t.Errorf("a fetch with an ended context = %v", err)
	}
	first, err := c.FetchMedia(t.Context(), engine.MediaRequest{Ref: refPhoto})
	if err != nil {
		t.Fatalf("FetchMedia = %v", err)
	}
	first.Erase()
	second, err := c.FetchMedia(t.Context(), engine.MediaRequest{Ref: refPhoto})
	if err != nil || second.Size() != int64(len(jpegFixture)) {
		t.Fatalf("a second fetch after an erase = %v, %v: each fetch stages its own copy", second, err)
	}
}

func TestTheStagedCopyKeepsWithinItsBytes(t *testing.T) {
	s := &staged{b: []byte("synthetic")}
	p := make([]byte, 4)
	if n, err := s.ReadAt(p, 7); n != 2 || !errors.Is(err, io.EOF) || string(p[:n]) != "ic" {
		t.Fatalf("a read past the end = %d, %v", n, err)
	}
	if n, err := s.ReadAt(p, 9); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("a read at the end = %d, %v", n, err)
	}
	for _, off := range []int64{-1, 7} {
		if _, err := s.WriteAt([]byte("XYZ"), off); !errors.Is(err, errStagedRange) {
			t.Errorf("a write at %d = %v", off, err)
		}
	}
	if _, err := s.ReadAt(p, -1); !errors.Is(err, errStagedRange) {
		t.Errorf("a read at -1 = %v", err)
	}
	if n, err := s.WriteAt([]byte("SY"), 0); n != 2 || err != nil || string(s.b) != "SYnthetic" {
		t.Fatalf("a write within the bytes = %d, %v, %q", n, err, s.b)
	}
	if err := s.Truncate(10); !errors.Is(err, errStagedRange) {
		t.Errorf("growing by Truncate = %v", err)
	}
	if err := s.Truncate(2); err != nil || s.Size() != 2 {
		t.Fatalf("Truncate(2) = %v, size %d", err, s.Size())
	}
	backing := s.b[:cap(s.b)]
	s.Erase()
	if s.Size() != 0 || slices.ContainsFunc(backing[:2], func(b byte) bool { return b != 0 }) {
		t.Fatalf("Erase left %d bytes and %q", s.Size(), backing)
	}
}
