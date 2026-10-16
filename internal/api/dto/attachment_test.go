package dto_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/api/dto"
)

const attachmentPolicy = "default-src 'none'; sandbox; frame-ancestors 'none'"

var fixedDeliveryHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Content-Disposition", "Content-Security-Policy",
	"Cross-Origin-Resource-Policy",
}

var servedCombinations = []struct {
	kind, variant, served, disposition string
}{
	{"image", "", "image/jpeg", `attachment; filename="wawarden-image.jpg"`},
	{"image", "", "image/png", `attachment; filename="wawarden-image.png"`},
	{"video", "", "video/mp4", `attachment; filename="wawarden-video.mp4"`},
	{"video", "", "application/octet-stream", `attachment; filename="wawarden-video.bin"`},
	{"video", "gif", "video/mp4", `attachment; filename="wawarden-video.mp4"`},
	{"video", "gif", "application/octet-stream", `attachment; filename="wawarden-video.bin"`},
	{"video", "video_note", "video/mp4", `attachment; filename="wawarden-video.mp4"`},
	{"video", "video_note", "application/octet-stream", `attachment; filename="wawarden-video.bin"`},
	{"audio", "", "audio/ogg", `attachment; filename="wawarden-audio.ogg"`},
	{"audio", "", "audio/mpeg", `attachment; filename="wawarden-audio.mp3"`},
	{"audio", "", "audio/mp4", `attachment; filename="wawarden-audio.m4a"`},
	{"audio", "", "application/octet-stream", `attachment; filename="wawarden-audio.bin"`},
	{"audio", "voice_note", "audio/ogg", `attachment; filename="wawarden-audio.ogg"`},
	{"audio", "voice_note", "audio/mpeg", `attachment; filename="wawarden-audio.mp3"`},
	{"audio", "voice_note", "audio/mp4", `attachment; filename="wawarden-audio.m4a"`},
	{"audio", "voice_note", "application/octet-stream", `attachment; filename="wawarden-audio.bin"`},
	{"document", "", "application/pdf", `attachment; filename="wawarden-document.pdf"`},
	{"document", "", "application/octet-stream", `attachment; filename="wawarden-document.bin"`},
	{"document", "", "image/jpeg", `attachment; filename="wawarden-document.jpg"`},
	{"document", "", "image/png", `attachment; filename="wawarden-document.png"`},
	{"sticker", "", "image/jpeg", `attachment; filename="wawarden-sticker.jpg"`},
	{"sticker", "", "image/png", `attachment; filename="wawarden-sticker.png"`},
	{"sticker", "animated", "image/jpeg", `attachment; filename="wawarden-sticker.jpg"`},
	{"sticker", "animated", "image/png", `attachment; filename="wawarden-sticker.png"`},
	{"sticker", "lottie", "application/octet-stream", `attachment; filename="wawarden-sticker.bin"`},
}

func newSized(t *testing.T, size int64) dto.Attachment {
	t.Helper()
	a, err := dto.NewAttachment("document", "", "application/pdf", size, sampleTag, bytes.NewReader(syntheticBody(size)), sampleExpiry, nil)
	if err != nil {
		t.Fatalf("NewAttachment: %v", err)
	}
	return a
}

func syntheticBody(size int64) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func plan(t *testing.T, a dto.Attachment, rangeHeader, ifRange string) dto.Delivery {
	t.Helper()
	d, err := a.Plan(rangeHeader, ifRange)
	if err != nil {
		t.Fatalf("Plan(%q, %q): %v", rangeHeader, ifRange, err)
	}
	return d
}

func headerList(d dto.Delivery) [][2]string {
	var h [][2]string
	for name, value := range d.Header() {
		h = append(h, [2]string{name, value})
	}
	return h
}

func TestEveryServedTypeHasItsGoldenHeaders(t *testing.T) {
	for _, c := range servedCombinations {
		t.Run(c.kind+"/"+c.variant+"/"+c.served, func(t *testing.T) {
			a, err := dto.NewAttachment(c.kind, c.variant, c.served, 1234, sampleTag, strings.NewReader(""), sampleExpiry, nil)
			if err != nil {
				t.Fatalf("NewAttachment: %v", err)
			}
			whole := [][2]string{
				{"Content-Type", c.served},
				{"Content-Length", "1234"},
				{"Accept-Ranges", "bytes"},
				{"ETag", sampleETag},
				{"Content-Disposition", c.disposition},
				{"Content-Security-Policy", attachmentPolicy},
				{"Cross-Origin-Resource-Policy", "same-origin"},
			}
			if got := headerList(plan(t, a, "", "")); !slices.Equal(got, whole) {
				t.Fatalf("200 headers = %q\nwant %q", got, whole)
			}
			partial := slices.Concat(whole[:1], [][2]string{{"Content-Length", "10"}, {"Content-Range", "bytes 100-109/1234"}}, whole[2:])
			if got := headerList(plan(t, a, "bytes=100-109", "")); !slices.Equal(got, partial) {
				t.Fatalf("206 headers = %q\nwant %q", got, partial)
			}
			refused := [][2]string{{"Content-Range", "bytes */1234"}}
			if got := headerList(plan(t, a, "bytes=1234-", "")); !slices.Equal(got, refused) {
				t.Fatalf("416 headers = %q\nwant %q", got, refused)
			}
		})
	}
}

func TestNewAttachmentRefusesWhatThePolicyCannotServe(t *testing.T) {
	type args struct {
		kind, variant, served string
		size                  int64
		tag                   [16]byte
		body                  io.ReaderAt
		expires               time.Time
	}
	valid := func(change func(*args)) args {
		a := args{kind: "document", served: "application/pdf", size: 10, tag: sampleTag, body: strings.NewReader("SYNTHETIC"), expires: sampleExpiry}
		change(&a)
		return a
	}
	tests := map[string]args{
		"an unknown kind":                     valid(func(a *args) { a.kind, a.served = "contact", "application/octet-stream" }),
		"an unknown variant":                  valid(func(a *args) { a.kind, a.variant, a.served = "audio", "podcast", "audio/ogg" }),
		"another kind's variant":              valid(func(a *args) { a.kind, a.variant, a.served = "image", "gif", "image/jpeg" }),
		"a kind spelled with its variant":     valid(func(a *args) { a.kind, a.served = "sticker/lottie", "application/octet-stream" }),
		"an unknown served type":              valid(func(a *args) { a.served = "text/html" }),
		"a served type with parameters":       valid(func(a *args) { a.served = "application/pdf; charset=utf-8" }),
		"an image served raw":                 valid(func(a *args) { a.kind, a.served = "image", "application/octet-stream" }),
		"a lottie sticker served as an image": valid(func(a *args) { a.kind, a.variant, a.served = "sticker", "lottie", "image/png" }),
		"a still sticker served raw":          valid(func(a *args) { a.kind, a.served = "sticker", "application/octet-stream" }),
		"a video served as audio":             valid(func(a *args) { a.kind, a.served = "video", "audio/mp4" }),
		"audio served as a document":          valid(func(a *args) { a.kind, a.served = "audio", "application/pdf" }),
		"a negative size":                     valid(func(a *args) { a.size = -1 }),
		"a zero tag":                          valid(func(a *args) { a.tag = [16]byte{} }),
		"no body":                             valid(func(a *args) { a.body = nil }),
		"no expiry":                           valid(func(a *args) { a.expires = time.Time{} }),
	}
	base := valid(func(*args) {})
	if _, err := dto.NewAttachment(base.kind, base.variant, base.served, base.size, base.tag, base.body, base.expires, nil); err != nil {
		t.Fatalf("NewAttachment refused the valid base: %v", err)
	}
	for name, a := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := dto.NewAttachment(a.kind, a.variant, a.served, a.size, a.tag, a.body, a.expires, nil); err == nil {
				t.Fatal("NewAttachment accepted it")
			}
		})
	}
	d, err := (dto.Attachment{}).Plan("", "")
	if err == nil || d.Code() != 0 || d.Body() != nil || d.Offset() != 0 || d.Length() != 0 || d.Whole() || len(headerList(d)) != 0 {
		t.Fatalf("Plan of an attachment built without NewAttachment = %d, %v; want an empty plan and an error", d.Code(), err)
	}
}

func TestPlanHonoursOneSatisfiableRange(t *testing.T) {
	const size = 3000
	tests := []struct {
		name, rangeHeader, ifRange string
		code                       int
		offset, length             int64
	}{
		{name: "no range", code: 200, length: size},
		{name: "a closed range", rangeHeader: "bytes=0-9", code: 206, length: 10},
		{name: "the unit in capitals", rangeHeader: "BYTES=0-9", code: 206, length: 10},
		{name: "space around the spec", rangeHeader: "bytes= 5-9 ", code: 206, offset: 5, length: 5},
		{name: "an open range", rangeHeader: "bytes=10-", code: 206, offset: 10, length: size - 10},
		{name: "a suffix", rangeHeader: "bytes=-10", code: 206, offset: size - 10, length: 10},
		{name: "a suffix past the start", rangeHeader: "bytes=-5000", code: 206, length: size},
		{name: "an end past the size", rangeHeader: "bytes=2990-5000", code: 206, offset: 2990, length: 10},
		{name: "an end past int64", rangeHeader: "bytes=0-99999999999999999999999", code: 206, length: size},
		{name: "the last byte", rangeHeader: "bytes=2999-2999", code: 206, offset: 2999, length: 1},
		{name: "several ranges", rangeHeader: "bytes=0-1,5-9", code: 200, length: size},
		{name: "a trailing comma", rangeHeader: "bytes=0-1,", code: 200, length: size},
		{name: "a reversed range", rangeHeader: "bytes=9-0", code: 200, length: size},
		{name: "letters", rangeHeader: "bytes=a-b", code: 200, length: size},
		{name: "a signed start", rangeHeader: "bytes=+1-2", code: 200, length: size},
		{name: "a negative end", rangeHeader: "bytes=1--2", code: 200, length: size},
		{name: "another unit", rangeHeader: "items=0-1", code: 200, length: size},
		{name: "no spec", rangeHeader: "bytes=", code: 200, length: size},
		{name: "a bare dash", rangeHeader: "bytes=-", code: 200, length: size},
		{name: "no dash", rangeHeader: "bytes=5", code: 200, length: size},
		{name: "the unit alone", rangeHeader: "bytes", code: 200, length: size},
		{name: "a start at the size", rangeHeader: "bytes=3000-", code: 416},
		{name: "a start past the size", rangeHeader: "bytes=3001-3005", code: 416},
		{name: "a start past int64", rangeHeader: "bytes=99999999999999999999-", code: 416},
		{name: "an empty suffix", rangeHeader: "bytes=-0", code: 416},
		{name: "a matching If-Range", rangeHeader: "bytes=0-9", ifRange: sampleETag, code: 206, length: 10},
		{name: "a stale If-Range", rangeHeader: "bytes=0-9", ifRange: `"00000000000000000000000000000000"`, code: 200, length: size},
		{name: "a weak If-Range", rangeHeader: "bytes=0-9", ifRange: "W/" + sampleETag, code: 200, length: size},
		{name: "an unquoted If-Range", rangeHeader: "bytes=0-9", ifRange: strings.Trim(sampleETag, `"`), code: 200, length: size},
		{name: "a dated If-Range", rangeHeader: "bytes=0-9", ifRange: "Mon, 05 Oct 2026 12:00:00 GMT", code: 200, length: size},
		{name: "a stale If-Range on an unsatisfiable range", rangeHeader: "bytes=3000-", ifRange: `"stale"`, code: 200, length: size},
		{name: "If-Range without Range", ifRange: `"stale"`, code: 200, length: size},
	}
	a := newSized(t, size)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := plan(t, a, tt.rangeHeader, tt.ifRange)
			if d.Code() != tt.code || d.Offset() != tt.offset || d.Length() != tt.length {
				t.Fatalf("Plan = %d at %d for %d, want %d at %d for %d", d.Code(), d.Offset(), d.Length(), tt.code, tt.offset, tt.length)
			}
			if got, want := d.Whole(), tt.code != 416 && tt.length == size; got != want {
				t.Fatalf("Whole = %v, want %v", got, want)
			}
			got := make([]byte, d.Length())
			if n, err := d.Body().ReadAt(got, 0); int64(n) != d.Length() || err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("Body read %d bytes, %v", n, err)
			}
			if want := syntheticBody(size)[tt.offset : tt.offset+tt.length]; !bytes.Equal(got, want) {
				t.Fatal("the body is not the planned section of the source")
			}
		})
	}
}

func TestAnEmptyAttachmentHasNoSatisfiableRange(t *testing.T) {
	a := newSized(t, 0)
	for header, code := range map[string]int{"": 200, "bytes=0-": 416, "bytes=-5": 416, "bytes=0-0": 416} {
		d := plan(t, a, header, "")
		if d.Code() != code || d.Length() != 0 || d.Whole() != (code == 200) {
			t.Fatalf("Plan(%q) = %d for %d whole %v, want %d", header, d.Code(), d.Length(), d.Whole(), code)
		}
	}
}

func TestPlanEmitsOnlyTheFixedHeaders(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := rapid.SampledFrom(servedCombinations).Draw(t, "combination")
		kind := rapid.OneOf(rapid.Just(c.kind), rapid.String()).Draw(t, "kind")
		size := rapid.Int64Range(-2, 1<<40).Draw(t, "size")
		a, err := dto.NewAttachment(kind, c.variant, c.served, size, sampleTag, strings.NewReader(""), sampleExpiry, nil)
		servable := slices.ContainsFunc(servedCombinations, func(s struct{ kind, variant, served, disposition string }) bool {
			return s.kind == kind && s.variant == c.variant && s.served == c.served
		})
		if !servable || size < 0 {
			if err == nil {
				t.Fatalf("NewAttachment(%q, size %d) accepted", kind, size)
			}
			return
		}
		if err != nil {
			t.Fatalf("NewAttachment: %v", err)
		}
		bound := strconv.FormatInt(size, 10)
		rangeHeader := rapid.OneOf(
			rapid.Just(""),
			rapid.String(),
			rapid.StringMatching(`(?i:bytes)=[0-9]{0,22}-[0-9]{0,22}`),
			rapid.StringMatching(`bytes=[0-9]{0,3}-[0-9]{0,3}(,[0-9]{0,3}-[0-9]{0,3})?`),
			rapid.SampledFrom([]string{"bytes=" + bound + "-", "bytes=-" + bound, "bytes=0-" + bound, "bytes=-0"}),
		).Draw(t, "range")
		ifRange := rapid.OneOf(rapid.Just(""), rapid.Just(sampleETag), rapid.String()).Draw(t, "ifRange")
		d, err := a.Plan(rangeHeader, ifRange)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		seen := map[string]string{}
		for name, value := range d.Header() {
			if !slices.Contains(fixedDeliveryHeaders, name) {
				t.Fatalf("Plan emitted %s", name)
			}
			if _, twice := seen[name]; twice {
				t.Fatalf("Plan emitted %s twice", name)
			}
			seen[name] = value
		}
		if d.Body().Size() != d.Length() || d.Offset() < 0 || d.Length() < 0 || d.Offset()+d.Length() > size {
			t.Fatalf("Plan covers %d for %d of %d", d.Offset(), d.Length(), size)
		}
		switch d.Code() {
		case 200:
			if d.Offset() != 0 || d.Length() != size || !d.Whole() || seen["Content-Range"] != "" {
				t.Fatalf("a 200 covers %d for %d of %d, whole %v, range %q", d.Offset(), d.Length(), size, d.Whole(), seen["Content-Range"])
			}
		case 206:
			want := "bytes " + strconv.FormatInt(d.Offset(), 10) + "-" + strconv.FormatInt(d.Offset()+d.Length()-1, 10) + "/" + bound
			if d.Length() == 0 || seen["Content-Range"] != want || d.Whole() != (d.Length() == size) {
				t.Fatalf("a 206 covers %d for %d of %d, whole %v, range %q", d.Offset(), d.Length(), size, d.Whole(), seen["Content-Range"])
			}
		case 416:
			if len(seen) != 1 || seen["Content-Range"] != "bytes */"+bound || d.Length() != 0 || d.Whole() {
				t.Fatalf("a 416 carries %q for %d", seen, d.Length())
			}
			return
		default:
			t.Fatalf("Plan answered %d", d.Code())
		}
		if seen["Content-Length"] != strconv.FormatInt(d.Length(), 10) {
			t.Fatalf("Content-Length %q for a length of %d", seen["Content-Length"], d.Length())
		}
	})
}

func TestDeliveryHeadersAreTheFixedSet(t *testing.T) {
	got := dto.DeliveryHeaders()
	if !slices.Equal(got, fixedDeliveryHeaders) {
		t.Fatalf("DeliveryHeaders = %q, want %q", got, fixedDeliveryHeaders)
	}
	got[0] = "Set-Cookie"
	if dto.DeliveryHeaders()[0] != "Content-Type" {
		t.Fatal("DeliveryHeaders hands out its own list")
	}
	var names []string
	for name := range plan(t, newSized(t, 10), "", "").Header() {
		names = append(names, name)
		if len(names) == 2 {
			break
		}
	}
	if !slices.Equal(names, fixedDeliveryHeaders[:2]) {
		t.Fatalf("the first two headers = %q, want %q", names, fixedDeliveryHeaders[:2])
	}
}

func TestAMediaStatusEncodesAFlatAttachmentInfo(t *testing.T) {
	contentType, body, err := dto.Encode(sampleMediaStatus(dto.MediaFetching))
	want := `{"kind":"document","variant":"","state":"fetching","served_type":"application/pdf","format":"pdf","normalised":false,` +
		`"bytes":3000,"size_declared":3000,"delivery":"part","reason":"too_large_for_inline","parts":1,"offset":0,"length":3000,` +
		`"rest_route":"/v1/messages/{mref}/media","expires_at":"2026-10-05T12:10:00.000Z","retry_after_seconds":2,"untrusted":true,` +
		`"notice":"synthetic notice"}`
	if err != nil || contentType != "application/json; charset=utf-8" || string(body) != want {
		t.Fatalf("Encode = %q, %s, %v\nwant %s", contentType, body, err, want)
	}
	_, body, err = dto.Encode(dto.MediaStatus{AttachmentInfo: dto.AttachmentInfo{State: dto.MediaReady}})
	want = `{"kind":"","variant":"","state":"ready","served_type":"","format":"","normalised":false,"bytes":0,"size_declared":0,` +
		`"delivery":"","reason":null,"parts":0,"offset":0,"length":0,"rest_route":"","expires_at":null,"untrusted":false,"notice":""}`
	if err != nil || string(body) != want {
		t.Fatalf("Encode = %s, %v\nwant %s", body, err, want)
	}
}

func TestTheAttachmentCarriesItsExpiryAndFinish(t *testing.T) {
	var sent []int64
	a, err := dto.NewAttachment("image", "", "image/png", 10, sampleTag, strings.NewReader(""), sampleExpiry, func(n int64, complete bool) {
		if complete {
			sent = append(sent, n)
		}
	})
	if err != nil {
		t.Fatalf("NewAttachment: %v", err)
	}
	a.Finished(10, true)
	a.Finished(4, false)
	if !a.Expires().Equal(sampleExpiry) || !slices.Equal(sent, []int64{10}) {
		t.Fatalf("Expires = %v, finishes %v", a.Expires(), sent)
	}
	sampleAttachment(t).Finished(1, true)
}
