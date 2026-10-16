package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

type MediaKind string

const (
	MediaImage    MediaKind = "image"
	MediaVideo    MediaKind = "video"
	MediaAudio    MediaKind = "audio"
	MediaDocument MediaKind = "document"
	MediaSticker  MediaKind = "sticker"
)

const (
	VariantGIF       = "gif"
	VariantVideoNote = "video_note"
	VariantVoiceNote = "voice_note"
	VariantAnimated  = "animated"
	VariantLottie    = "lottie"
)

var (
	ErrMediaUnavailable = errors.New("engine: the attachment is not available")
	ErrMediaExpired     = errors.New("engine: the attachment has expired")
	ErrMediaTooLarge    = errors.New("engine: the attachment is larger than allowed")
	ErrMediaFailed      = errors.New("engine: fetching the attachment failed")
	ErrMediaThrottled   = errors.New("engine: attachment fetches are throttled")
)

type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	if e.RetryAfter <= 0 {
		return ErrMediaThrottled.Error()
	}
	return ErrMediaThrottled.Error() + ", retry after " + strconv.FormatInt(int64((e.RetryAfter+time.Second-1)/time.Second), 10) + "s"
}

func (e *ThrottledError) Unwrap() error { return ErrMediaThrottled }

func RetryAfter(err error) time.Duration {
	if t, ok := errors.AsType[*ThrottledError](err); ok && t.RetryAfter > 0 {
		return t.RetryAfter
	}
	return 0
}

type MediaRequest struct {
	ID       string
	Ref      string
	Kind     MediaKind
	Declared int64
	Deadline time.Time
	Chat     policy.CanonicalChat
	Sender   policy.CanonicalChat
}

type Staged interface {
	Size() int64
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Truncate(size int64) error
	Erase()
}

func (e *Engine) FetchMedia(ctx context.Context, req MediaRequest) (Staged, error) {
	if !e.Ready() {
		return nil, ErrNotConnected
	}
	var staged Staged
	err := guarded("engine.media", func() error {
		s, err := e.sup.client.FetchMedia(ctx, req)
		if err == nil && s != nil {
			staged = s
			return nil
		}
		if s != nil {
			s.Erase()
		}
		return cmp.Or(err, ErrMediaFailed)
	})
	if err != nil {
		err = mediaError(err)
		if errors.Is(err, ErrMediaExpired) {
			e.expireMediaRef(ctx, req)
		}
		return nil, err
	}
	return staged, nil
}

func (e *Engine) expireMediaRef(ctx context.Context, req MediaRequest) {
	if !req.Chat.Valid() || !req.Sender.Valid() {
		return
	}
	p := e.pipe
	if err := p.write(context.WithoutCancel(ctx), "engine.media_expire", func(tx *ingest.Tx) error {
		return tx.ExpireMediaRef(req.Chat, req.ID, req.Sender, p.clock.Now())
	}); err != nil {
		p.logger.Warn("marking an expired attachment's reference failed", slog.String("event", "media_expire_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
	}
}

func mediaError(err error) error {
	if errors.Is(err, ErrMediaThrottled) {
		return &ThrottledError{RetryAfter: RetryAfter(err)}
	}
	for _, sentinel := range []error{ErrNotConnected, ErrMediaUnavailable, ErrMediaExpired, ErrMediaTooLarge} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return ErrMediaFailed
}
