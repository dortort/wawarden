package wa

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
	"github.com/dortort/wawarden/internal/stage"
)

const (
	mediaRAMThreshold = 16 << 20
	imageCeiling      = 16 << 20
	stickerCeiling    = 1 << 20
	mediaHashSize     = 32
	descriptorVersion = 1
	descriptorFields  = 10
)

var (
	errMediaOptions = errors.New("wa: an attachment cap is at most 2 GiB, and a cap above 0 needs a reference sealer, a retention and, above 16 MiB, a disk spool")
	errMediaSource  = errors.New("wa: the attachment source carries no message")
	errMediaLength  = errors.New("wa: the attachment is longer than its declared length")
)

type MediaSealer interface {
	SealMedia(id string, pt []byte) (string, error)
	OpenMedia(id, text string) ([]byte, error)
}

type stagedFile interface {
	engine.Staged
	whatsmeow.File
}

var (
	_ stagedFile = (*capFile)(nil)
	_ stagedFile = (*stage.Sealed)(nil)
)

type mediaDownloader func(ctx context.Context, cli *whatsmeow.Client, src mediasrc.Source, dst whatsmeow.File) error

func downloadMediaFile(ctx context.Context, cli *whatsmeow.Client, src mediasrc.Source, dst whatsmeow.File) error {
	msg, ok := src.Message()
	if !ok {
		return errMediaSource
	}
	return cli.DownloadToFile(ctx, msg, dst)
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

func (mc capture) open(req engine.MediaRequest) (descriptor, error) {
	if mc.max <= 0 {
		return descriptor{}, engine.ErrMediaUnavailable
	}
	pt, err := mc.sealer.OpenMedia(req.ID, req.Ref)
	if err != nil {
		return descriptor{}, engine.ErrMediaUnavailable
	}
	d, ok := parseDescriptor(pt)
	clear(pt)
	if !ok {
		return descriptor{}, engine.ErrMediaUnavailable
	}
	declared, ok := d.declared()
	if !ok || d.kind != req.Kind || declared != req.Declared {
		return descriptor{}, engine.ErrMediaUnavailable
	}
	if declared > mc.limit(d.kind) {
		return descriptor{}, engine.ErrMediaTooLarge
	}
	return d, nil
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

func downloadable(d descriptor) mediasrc.Source {
	path, length := proto.String(d.directPath), proto.Uint64(d.length)
	switch d.kind {
	case engine.MediaImage:
		return mediasrc.Wrap(&waE2E.ImageMessage{DirectPath: path, MediaKey: d.mediaKey, FileSHA256: d.fileSHA256, FileEncSHA256: d.fileEncSHA256, FileLength: length})
	case engine.MediaVideo:
		return mediasrc.Wrap(&waE2E.VideoMessage{DirectPath: path, MediaKey: d.mediaKey, FileSHA256: d.fileSHA256, FileEncSHA256: d.fileEncSHA256, FileLength: length})
	case engine.MediaAudio:
		return mediasrc.Wrap(&waE2E.AudioMessage{DirectPath: path, MediaKey: d.mediaKey, FileSHA256: d.fileSHA256, FileEncSHA256: d.fileEncSHA256, FileLength: length})
	case engine.MediaDocument:
		return mediasrc.Wrap(&waE2E.DocumentMessage{DirectPath: path, MediaKey: d.mediaKey, FileSHA256: d.fileSHA256, FileEncSHA256: d.fileEncSHA256, FileLength: length})
	case engine.MediaSticker:
		return mediasrc.Wrap(&waE2E.StickerMessage{DirectPath: path, MediaKey: d.mediaKey, FileSHA256: d.fileSHA256, FileEncSHA256: d.fileEncSHA256, FileLength: length})
	}
	return mediasrc.Wrap(nil)
}

func (c *Client) FetchMedia(ctx context.Context, req engine.MediaRequest) (engine.Staged, error) {
	d, err := c.capture.open(req)
	if err != nil {
		return nil, err
	}
	declared := req.Declared
	deadline := mediaRAMDeadline
	if declared > mediaRAMThreshold {
		deadline = mediaDiskDeadline(declared)
	}
	if !req.Deadline.IsZero() {
		deadline = min(deadline, req.Deadline.Sub(c.now()))
	}
	if deadline <= 0 {
		return nil, engine.ErrMediaFailed
	}
	src := downloadable(d)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var staged stagedFile
	if declared <= mediaRAMThreshold {
		staged = newCapFile(declared+mediaOverhead, declared)
	} else if staged, err = c.spool.Admit(declared, cancel); err != nil {
		return nil, engine.ErrMediaFailed
	}
	job, stop := withMediaJob(ctx, declared, deadline)
	err = c.fetch(job, c.current(), src, staged)
	cause := context.Cause(job)
	stop()
	if err == nil && declared > 0 && staged.Size() > declared {
		err = errMediaLength
	}
	if err != nil {
		staged.Erase()
		return nil, fetchError(err, cause)
	}
	return staged, nil
}

func fetchError(err, cause error) error {
	if t, ok := errors.AsType[*mediaThrottledError](cause); ok {
		return &engine.ThrottledError{RetryAfter: t.RetryAfter}
	}
	switch {
	case cause != nil:
		return engine.ErrMediaFailed
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return engine.ErrMediaExpired
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrClientIsNil):
		return engine.ErrNotConnected
	}
	return engine.ErrMediaFailed
}
