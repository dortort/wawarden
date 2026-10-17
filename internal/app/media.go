package app

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/attach"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/stage"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	mediaRAMThreshold = 16 << 20
	coolDownDefault   = time.Minute
	coolDownMin       = 30 * time.Second
	coolDownMax       = 10 * time.Minute
	headroomWait      = time.Minute
)

var (
	errMediaNotStopped = errors.New("app: the attachment jobs did not stop in time")
	errSpoolErased     = errors.New("app: the attachment spool was erased")
	errSpoolOffset     = errors.New("app: negative attachment offset")
)

type mediaFetcher interface {
	Ready() bool
	FetchMedia(ctx context.Context, req engine.MediaRequest) (engine.Staged, error)
}

type contentPolicy func(ctx context.Context, kind, variant string, s attach.Spool) (attach.Outcome, error)

type mediaService struct {
	fetcher mediaFetcher
	spool   *stage.Spool
	now     func() time.Time
	policy  contentPolicy

	mu    sync.Mutex
	until time.Time
}

func newMediaService(fetcher mediaFetcher, spool *stage.Spool, now func() time.Time) *mediaService {
	return &mediaService{fetcher: fetcher, spool: spool, now: now, policy: attach.Apply}
}

func (s *mediaService) Precheck(declared int64) error {
	if wait := s.cooling(); wait > 0 {
		return api.Busy(wait)
	}
	if !s.fetcher.Ready() {
		return api.ErrNotConnected
	}
	if declared > mediaRAMThreshold && s.spool.Headroom(declared) != nil {
		return api.Busy(headroomWait)
	}
	return nil
}

func (s *mediaService) Fetch(ctx context.Context, ref scoped.SealedRef, req api.MediaFetch) (api.MediaSpool, error) {
	staged, err := s.fetcher.FetchMedia(ctx, engine.MediaRequest{
		ID: req.ID, Ref: ref.Sealed(), Kind: engine.MediaKind(req.Kind), Declared: req.Declared, Chat: req.Chat, Sender: req.Sender,
	})
	if err != nil {
		return nil, s.failure(err)
	}
	fetched := staged.Size()
	out, err := s.policy(ctx, req.Kind, req.Variant, staged)
	if err != nil {
		staged.Erase()
		if errors.Is(err, attach.ErrUnsupported) {
			return nil, api.ErrMediaUnsupported
		}
		return nil, api.ErrMediaFailed
	}
	if out.Normalised {
		staged.Erase()
		return &servedSpool{body: imageBody(out.Image), size: int64(len(out.Image)), fetched: fetched, outcome: out, inMemory: true}, nil
	}
	_, disk := staged.(*stage.Sealed)
	return &servedSpool{body: staged, size: staged.Size(), fetched: fetched, outcome: out, inMemory: !disk}, nil
}

func (s *mediaService) failure(err error) error {
	switch {
	case errors.Is(err, engine.ErrMediaThrottled):
		return api.Busy(s.coolDown(engine.RetryAfter(err)))
	case errors.Is(err, engine.ErrNotConnected):
		return api.ErrNotConnected
	case errors.Is(err, engine.ErrMediaExpired):
		return api.ErrMediaExpired
	case errors.Is(err, engine.ErrMediaTooLarge):
		return api.ErrMediaTooLarge
	case errors.Is(err, engine.ErrMediaUnavailable):
		return api.ErrMediaUnavailable
	}
	return api.ErrMediaFailed
}

func (s *mediaService) coolDown(wait time.Duration) time.Duration {
	if wait <= 0 {
		wait = coolDownDefault
	}
	wait = min(max(wait, coolDownMin), coolDownMax)
	s.mu.Lock()
	defer s.mu.Unlock()
	if until := s.now().Add(wait); until.After(s.until) {
		s.until = until
	}
	return wait
}

func (s *mediaService) cooling() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.until.Sub(s.now())
}

type spoolBody interface {
	ReadAt(p []byte, off int64) (int, error)
	Erase()
}

type servedSpool struct {
	mu       sync.RWMutex
	body     spoolBody
	size     int64
	fetched  int64
	outcome  attach.Outcome
	inMemory bool
	erased   bool
}

func (s *servedSpool) Size() int64        { return s.size }
func (s *servedSpool) Fetched() int64     { return s.fetched }
func (s *servedSpool) ServedType() string { return s.outcome.ServedType }
func (s *servedSpool) Format() string     { return s.outcome.Format }
func (s *servedSpool) Normalised() bool   { return s.outcome.Normalised }
func (s *servedSpool) InMemory() bool     { return s.inMemory }

func (s *servedSpool) ReadAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.erased {
		return 0, errSpoolErased
	}
	return s.body.ReadAt(p, off)
}

func (s *servedSpool) Erase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.erased {
		return
	}
	s.erased = true
	s.body.Erase()
}

type imageBody []byte

func (b imageBody) ReadAt(p []byte, off int64) (int, error) {
	switch {
	case off < 0:
		return 0, errSpoolOffset
	case off >= int64(len(b)):
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (b imageBody) Erase() { clear(b) }

func clientLive(clients *admin.Clients) func(ctx context.Context, id string) (bool, error) {
	return func(ctx context.Context, id string) (bool, error) {
		c, err := clients.Get(ctx, id)
		switch {
		case errors.Is(err, admin.ErrNotFound):
			return false, nil
		case err != nil:
			return false, err
		}
		return c.State == admin.StateActive, nil
	}
}

type mediaRun struct {
	stop    context.CancelFunc
	stopped chan struct{}
}

func (m *mediaRun) halt() {
	if m != nil {
		m.stop()
	}
}

func (m *mediaRun) wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return errMediaNotStopped
	}
}
