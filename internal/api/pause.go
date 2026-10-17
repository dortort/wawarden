package api

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const mediaPausedWait = time.Minute

var errMediaPaused = &codedError{status: http.StatusServiceUnavailable, code: codeBusy, retryAfter: mediaPausedWait}

type MediaPause struct {
	mu     sync.Mutex
	paused atomic.Bool
	drop   func()
}

func (p *MediaPause) Set(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused.Store(paused)
	if paused && p.drop != nil {
		p.drop()
	}
}

func (p *MediaPause) Paused() bool { return p.paused.Load() }

func (p *MediaPause) onPause(drop func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drop = drop
}
