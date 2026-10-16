package mediasrc

import (
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func zero[T any]() T {
	var v T
	return v
}

func TestTheZeroSourceCarriesNoMessage(t *testing.T) {
	var declared Source
	literal := Source{}
	for name, s := range map[string]Source{
		"a declared variable": declared,
		"new":                 *new(Source),
		"a composite literal": literal,
		"a generic zero":      zero[Source](),
		"Wrap(nil)":           Wrap(nil),
	} {
		if m, ok := s.Message(); ok || m != nil {
			t.Errorf("Message() of %s = %v, %v, want nil, false", name, m, ok)
		}
	}
}

func TestWrapCarriesItsMessage(t *testing.T) {
	for name, m := range map[string]whatsmeow.DownloadableMessage{
		"an image":   &waE2E.ImageMessage{DirectPath: proto.String("/example/i1")},
		"a video":    &waE2E.VideoMessage{DirectPath: proto.String("/example/v1")},
		"an audio":   &waE2E.AudioMessage{DirectPath: proto.String("/example/a1")},
		"a document": &waE2E.DocumentMessage{DirectPath: proto.String("/example/d1")},
		"a sticker":  &waE2E.StickerMessage{DirectPath: proto.String("/example/s1")},
	} {
		got, ok := Wrap(m).Message()
		if !ok || got != m {
			t.Errorf("Wrap(%s).Message() = %v, %v, want the same message and true", name, got, ok)
		}
	}
}
