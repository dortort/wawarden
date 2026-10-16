package wa

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/dortort/wawarden/internal/engine"
)

const (
	imageCeiling      = 16 << 20
	stickerCeiling    = 1 << 20
	mediaHashSize     = 32
	descriptorVersion = 1
	descriptorFields  = 10
)

var errMediaOptions = errors.New("wa: an attachment cap is at most 2 GiB, and a cap above 0 needs a reference sealer and a retention")

type MediaSealer interface {
	SealMedia(id string, pt []byte) (string, error)
	OpenMedia(id, text string) ([]byte, error)
}

type capture struct {
	max       int64
	retention time.Duration
	sealer    MediaSealer
	now       func() time.Time
}

func (mc capture) limit(kind engine.MediaKind) int64 {
	switch kind {
	case engine.MediaImage:
		return min(mc.max, imageCeiling)
	case engine.MediaSticker:
		return min(mc.max, stickerCeiling)
	}
	return mc.max
}

func (mc capture) fresh(m engine.Message) bool {
	until := m.Timestamp.Add(mc.retention)
	if m.Expiration > 0 {
		if own := m.Timestamp.Add(m.Expiration); own.Before(until) {
			until = own
		}
	}
	return mc.now().Before(until)
}

func (mc capture) attach(out engine.Message, m *waE2E.Message, viewOnce bool) engine.Message {
	if mc.max <= 0 || out.Kind != engine.KindMedia || out.ID == "" || viewOnce {
		return out
	}
	part, ok := mediaOf(m)
	if !ok || part.viewOnce {
		return out
	}
	d := descriptor{
		directPath:    part.item.GetDirectPath(),
		mediaKey:      part.item.GetMediaKey(),
		fileSHA256:    part.item.GetFileSHA256(),
		fileEncSHA256: part.item.GetFileEncSHA256(),
		length:        part.item.GetFileLength(),
		kind:          part.kind,
		variant:       part.variant,
		chat:          out.Chat,
		sender:        out.Sender,
		fromMe:        out.FromMe,
	}
	declared, ok := d.declared()
	if !ok || declared > mc.limit(d.kind) || !mc.fresh(out) {
		return out
	}
	pt := d.marshal()
	ref, err := mc.sealer.SealMedia(out.ID, pt)
	clear(pt)
	if err != nil {
		return out
	}
	out.MediaRef, out.MediaKind, out.MediaVariant, out.MediaBytes = ref, d.kind, d.variant, declared
	return out
}

type descriptor struct {
	directPath    string
	mediaKey      []byte
	fileSHA256    []byte
	fileEncSHA256 []byte
	length        uint64
	kind          engine.MediaKind
	variant       string
	chat          string
	sender        string
	fromMe        bool
}

var mediaVariants = map[engine.MediaKind][]string{
	engine.MediaImage:    {""},
	engine.MediaVideo:    {"", engine.VariantGIF, engine.VariantVideoNote},
	engine.MediaAudio:    {"", engine.VariantVoiceNote},
	engine.MediaDocument: {""},
	engine.MediaSticker:  {"", engine.VariantAnimated, engine.VariantLottie},
}

func (d descriptor) declared() (int64, bool) {
	if !strings.HasPrefix(d.directPath, "/") || len(d.mediaKey) == 0 || len(d.fileSHA256) != mediaHashSize || len(d.fileEncSHA256) != mediaHashSize ||
		!slices.Contains(mediaVariants[d.kind], d.variant) || d.length > mediaMaxDeclared {
		return 0, false
	}
	return int64(d.length), true
}

func (d descriptor) marshal() []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], d.length)
	from := []byte{0}
	if d.fromMe {
		from[0] = 1
	}
	out := []byte{descriptorVersion}
	for _, f := range [descriptorFields][]byte{[]byte(d.directPath), d.mediaKey, d.fileSHA256, d.fileEncSHA256, length[:], []byte(d.kind), []byte(d.variant), []byte(d.chat), []byte(d.sender), from} {
		out = binary.AppendUvarint(out, uint64(len(f)))
		out = append(out, f...)
	}
	return out
}

func parseDescriptor(pt []byte) (descriptor, bool) {
	if len(pt) == 0 || pt[0] != descriptorVersion {
		return descriptor{}, false
	}
	var f [descriptorFields][]byte
	rest := pt[1:]
	for i := range f {
		n, used := binary.Uvarint(rest)
		if used <= 0 {
			return descriptor{}, false
		}
		if rest = rest[used:]; n > uint64(len(rest)) {
			return descriptor{}, false
		}
		f[i], rest = rest[:n], rest[n:]
	}
	if len(rest) != 0 || len(f[4]) != 8 || len(f[9]) != 1 || f[9][0] > 1 {
		return descriptor{}, false
	}
	d := descriptor{
		directPath:    string(f[0]),
		mediaKey:      bytes.Clone(f[1]),
		fileSHA256:    bytes.Clone(f[2]),
		fileEncSHA256: bytes.Clone(f[3]),
		length:        binary.BigEndian.Uint64(f[4]),
		kind:          engine.MediaKind(f[5]),
		variant:       string(f[6]),
		chat:          string(f[7]),
		sender:        string(f[8]),
		fromMe:        f[9][0] == 1,
	}
	if !bytes.Equal(d.marshal(), pt) {
		return descriptor{}, false
	}
	return d, true
}

func (c *Client) FetchMedia(context.Context, engine.MediaRequest) (engine.Staged, error) {
	return nil, engine.ErrMediaUnavailable
}
