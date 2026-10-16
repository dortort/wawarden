package engine

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func withMedia(m Message, kind MediaKind, variant string, declared int64) Message {
	m.Kind, m.MediaType = KindMedia, "application/octet-stream"
	m.MediaRef, m.MediaKind, m.MediaVariant, m.MediaBytes = syntheticRef, kind, variant, declared
	return m
}

func TestPayloadsWrittenAsVersionOneStillDecode(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Event
	}{
		{
			`{"v":1,"message":{"chat":"` + group + `","id":"M1","sender":"` + alice + `","sender_alt":"` + aliceLID + `","addressing":"pn","from_me":true,` +
				`"ts":"2026-10-04T12:00:00Z","push_name":"Alice Example","kind":"media","text":"caption","media_type":"image/jpeg",` +
				`"target":{"remote_jid":"` + group + `","id":"M0","participant":"` + bob + `"},"reply":{"id":"M0","participant":"` + bob + `","text":"quoted"},"expiration":3600000000000}}`,
			Message{Chat: group, ID: "M1", Sender: alice, SenderAlt: aliceLID, Addressing: "pn", FromMe: true, Timestamp: epoch, PushName: "Alice Example",
				Kind: KindMedia, Text: "caption", MediaType: "image/jpeg", Target: &Key{RemoteJID: group, ID: "M0", Participant: bob},
				Reply: &Reply{ID: "M0", Participant: bob, Text: "quoted"}, Expiration: time.Hour},
		},
		{
			`{"v":1,"group":{"chat":"` + group + `","subject":"Synthetic Group","members":[{"user":"` + alice + `","admin":true}],"left":["` + carol + `"],"ts":"2026-10-04T12:00:00Z"}}`,
			Group{Chat: group, Subject: "Synthetic Group", Members: []Participant{{User: alice, Admin: true}}, Left: []string{carol}, Timestamp: epoch},
		},
		{
			`{"v":1,"contact":{"user":"` + alice + `","full_name":"Alice Saved","first_name":"Alice"}}`,
			ContactName{User: alice, FullName: "Alice Saved", FirstName: "Alice"},
		},
	} {
		got, err := decodePayload([]byte(tt.in))
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("a version 1 %T decodes as %#v, %v", tt.want, got, err)
		}
	}
}

func TestAVersionOneRowLeftInTheInboxIsAppliedAfterTheUpgrade(t *testing.T) {
	r := newPipeRig(t)
	r.appendRaw([]byte(`{"v":1,"message":{"chat":"` + alice + `","id":"M1","sender":"` + alice + `","ts":"2026-10-04T12:00:00Z","kind":"text","text":"written before the upgrade"}}`))
	r.drain()
	if found := r.must(alice, "M1", alice); found.Text != "written before the upgrade" {
		t.Fatalf("the version 1 row was applied as %+v", found)
	}
	if n := query[int](t, r.inspect(), "SELECT count(*) FROM inbox"); n != 0 {
		t.Fatalf("%d inbox rows left after the version 1 row was applied", n)
	}
}

func TestPayloadsAreWrittenAsVersionTwoWithTheAttachmentReference(t *testing.T) {
	plain := mustPayload(t, dm("M1", alice, "no attachment"))
	if !bytes.HasPrefix(plain, []byte(`{"v":2,`)) {
		t.Fatalf("a payload is written as %s", plain)
	}
	if bytes.Contains(plain, []byte(`"media_ref"`)) || bytes.Contains(plain, []byte(`"media_kind"`)) || bytes.Contains(plain, []byte(`"media_variant"`)) || bytes.Contains(plain, []byte(`"media_bytes"`)) {
		t.Fatalf("a message without an attachment carries attachment fields: %s", plain)
	}
	for _, tt := range []struct {
		kind    MediaKind
		variant string
	}{
		{MediaImage, ""},
		{MediaVideo, ""},
		{MediaVideo, VariantGIF},
		{MediaVideo, VariantVideoNote},
		{MediaAudio, ""},
		{MediaAudio, VariantVoiceNote},
		{MediaDocument, ""},
		{MediaSticker, ""},
		{MediaSticker, VariantAnimated},
		{MediaSticker, VariantLottie},
	} {
		m := withMedia(dm("M2", alice, "caption"), tt.kind, tt.variant, 2<<30)
		b := mustPayload(t, m)
		want := `"media_ref":"` + syntheticRef + `","media_kind":"` + string(tt.kind) + `",`
		if tt.variant != "" {
			want += `"media_variant":"` + tt.variant + `",`
		}
		want += `"media_bytes":2147483648`
		if !bytes.HasPrefix(b, []byte(`{"v":2,`)) || !bytes.Contains(b, []byte(want)) {
			t.Errorf("a %s %q attachment is written as %s", tt.kind, tt.variant, b)
		}
		got, err := decodePayload(b)
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Errorf("a %s %q attachment round trips as %#v, %v", tt.kind, tt.variant, got, err)
		}
	}
}

func TestPayloadsOfAnUnknownVersionAreRefused(t *testing.T) {
	body := `"message":{"chat":"` + alice + `","id":"M1","sender":"` + alice + `","ts":"2026-10-04T12:00:00Z"}}`
	if _, err := decodePayload([]byte(`{"v":2,` + body)); err != nil {
		t.Fatalf("a version 2 message is refused: %v", err)
	}
	for _, v := range []string{`{`, `{"v":0,`, `{"v":3,`, `{"v":-1,`, `{"v":20,`, `{"v":"2",`, `{"v":2.5,`} {
		if _, err := decodePayload([]byte(v + body)); err == nil {
			t.Errorf("decodePayload(%q) accepted", v+body)
		}
	}
}

func TestAVersionOnePayloadCannotCarryAnAttachment(t *testing.T) {
	head := `"message":{"chat":"` + alice + `","id":"M1","sender":"` + alice + `","ts":"2026-10-04T12:00:00Z","kind":"media",`
	for _, field := range []string{`"media_ref":"` + syntheticRef + `"`, `"media_kind":"image"`, `"media_variant":"gif"`, `"media_bytes":1`} {
		if _, err := decodePayload([]byte(`{"v":1,` + head + field + `}}`)); err == nil {
			t.Errorf("a version 1 message with %s was accepted", field)
		}
		if _, err := decodePayload([]byte(`{"v":2,` + head + field + `}}`)); err != nil {
			t.Errorf("a version 2 message with %s was refused: %v", field, err)
		}
	}
}

func TestMessageSizeCountsTheAttachmentReference(t *testing.T) {
	base := dm("M1", alice, "caption")
	for _, tt := range []struct {
		kind    MediaKind
		variant string
	}{{MediaDocument, ""}, {MediaVideo, VariantVideoNote}, {MediaSticker, VariantLottie}} {
		m := withMedia(base, tt.kind, tt.variant, 1<<20)
		m.Kind, m.MediaType = base.Kind, base.MediaType
		if got, want := m.size()-base.size(), len(syntheticRef)+len(tt.kind)+len(tt.variant); got != want {
			t.Errorf("a %s %q reference adds %d to the size, want %d", tt.kind, tt.variant, got, want)
		}
	}
	long := base
	long.MediaRef = strings.Repeat("r", 4096)
	if got := long.size() - base.size(); got != 4096 {
		t.Fatalf("a 4096-byte reference adds %d to the size", got)
	}
}

func TestAReferencePushingAMessageOverTheCapDropsIt(t *testing.T) {
	r := newPipeRig(t)
	at := dm("M1", alice, "")
	at.Text = strings.Repeat("x", maxMessageBytes-at.size())
	if at.size() != maxMessageBytes {
		t.Fatalf("the message at the cap has size %d", at.size())
	}
	over := at
	over.ID = "M2"
	over.Text = at.Text[:len(at.Text)-len(syntheticRef)+1]
	over.MediaRef = syntheticRef
	if over.size() != maxMessageBytes+1 {
		t.Fatalf("the message over the cap has size %d", over.size())
	}
	r.deliver(at, over)
	if r.dropped(dropTooLarge) != 1 {
		t.Fatalf("too_large drops %v, want 1", r.dropped(dropTooLarge))
	}
	r.drain()
	r.must(alice, "M1", alice)
	if _, ok := r.find(alice, "M2", alice); ok {
		t.Fatal("the message whose reference took it over the cap was stored")
	}
}
