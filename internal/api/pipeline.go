package api

import (
	"net/http"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
)

type pipeline struct {
	name         string
	router       *router
	authenticate func(*http.Request) (*http.Request, bool)
	failures     *metrics.Counter
	throttle     *bucket
}

func (p *pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	recovering(p.name, w, r, p.serve)
}

func (p *pipeline) serve(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Sec-Fetch-Site")) > 0 {
		writeError(w, http.StatusForbidden, codeForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed)
		return
	}
	authenticated, ok := p.authenticate(r)
	if !ok {
		p.failures.Inc()
		if !p.throttle.allow() {
			writeError(w, http.StatusTooManyRequests, codeTooManyRequests)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, codeUnauthorized)
		return
	}
	p.router.ServeHTTP(w, authenticated)
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
