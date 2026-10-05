package api

import (
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
)

type pipeline struct {
	name         string
	router       *router
	authenticate func(*http.Request) (*http.Request, bool)
	failures     *metrics.Counter
	failed       func()
	throttle     *bucket
}

func (p *pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	recovering(p.name, w, r, p.serve)
}

func (p *pipeline) serve(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Sec-Fetch-Site")) > 0 {
		refuse(w, r, http.StatusForbidden, codeForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		refuse(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed)
		return
	}
	authenticated, ok := p.authenticate(r)
	if !ok {
		p.failures.Inc()
		if p.failed != nil {
			p.failed()
		}
		if !p.throttle.allow() {
			refuse(w, r, http.StatusTooManyRequests, codeTooManyRequests)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, r, http.StatusUnauthorized, codeUnauthorized)
		return
	}
	p.router.ServeHTTP(w, authenticated)
}

func refuse(w http.ResponseWriter, r *http.Request, status int, code string) {
	ignoreBody(w, r)
	writeError(w, status, code)
}

func ignoreBody(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength == 0 {
		return
	}
	// Otherwise net/http waits for up to 256 KiB of the unread body, both before and after the reply.
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
}

func recovering(name string, w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
	if !completes(name, w, r, serve) {
		writeError(w, http.StatusInternalServerError, codeInternal)
	}
}

func completes(name string, w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) bool {
	defer safego.Recover(name)
	serve(w, r)
	return true
}
