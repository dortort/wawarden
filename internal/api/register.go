// Package api serves the client and admin surfaces through policy-classed registrations, plus a fixed loopback health endpoint.
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

type class string

const (
	classRead  class = "read"
	classWrite class = "write"
	classAdmin class = "admin"
)

type route struct {
	pattern string
	class   class
}

type Request struct {
	w   http.ResponseWriter
	req *http.Request
}

type router struct {
	mux        *http.ServeMux
	routes     []route
	now        func() time.Time
	credential policy.AdminCredential
}

func newRouter(now func() time.Time, credential policy.AdminCredential) *router {
	return &router{mux: http.NewServeMux(), now: now, credential: credential}
}

func (rt *router) read(pattern string, h func(context.Context, policy.ReadGrant, *Request) (dto.Response, error)) {
	rt.register(route{pattern: pattern, class: classRead}, decided(func(r *http.Request) (policy.ReadGrant, bool) {
		return policy.DecideRead(clientFrom(r.Context()), rt.now())
	}, h))
}

func (rt *router) write(pattern string, h func(context.Context, policy.WriteGrant, *Request) (dto.Response, error)) {
	rt.register(route{pattern: pattern, class: classWrite}, decided(func(r *http.Request) (policy.WriteGrant, bool) {
		return policy.DecideWrite(clientFrom(r.Context()), rt.now())
	}, h))
}

func (rt *router) admin(pattern string, h func(context.Context, policy.AdminGrant, *Request) (dto.Response, error)) {
	rt.register(route{pattern: pattern, class: classAdmin}, decided(func(r *http.Request) (policy.AdminGrant, bool) {
		presented, _ := bearerToken(r.Header)
		return policy.DecideAdmin(rt.credential, presented)
	}, h))
}

func decided[G any](decide func(*http.Request) (G, bool), h func(context.Context, G, *Request) (dto.Response, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g, ok := decide(r)
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound)
			return
		}
		resp, err := h(r.Context(), g, &Request{w: w, req: r})
		if refusal, ok := errors.AsType[*bodyError](err); ok {
			writeError(w, refusal.status, refusal.code)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal)
			return
		}
		writeResponse(w, http.StatusOK, resp)
	}
}

func (rt *router) register(rte route, h http.HandlerFunc) {
	rt.mux.Handle(rte.pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(*fallbackWriter); ok {
			f.routed = true
			w = f.w
		}
		h(w, r)
	}))
	rt.routes = append(rt.routes, rte)
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f := &fallbackWriter{w: w, header: make(http.Header)}
	rt.mux.ServeHTTP(f, r)
	if !f.routed && !f.written {
		writeError(w, http.StatusNotFound, codeNotFound)
	}
}

// Only registered routes write directly; any response ServeMux makes itself becomes the uniform 404 or 405.
type fallbackWriter struct {
	w       http.ResponseWriter
	header  http.Header
	routed  bool
	written bool
}

func (f *fallbackWriter) Header() http.Header { return f.header }

func (f *fallbackWriter) Write(p []byte) (int, error) {
	f.WriteHeader(http.StatusOK)
	return len(p), nil
}

func (f *fallbackWriter) WriteHeader(status int) {
	if f.written {
		return
	}
	f.written = true
	if status == http.StatusMethodNotAllowed {
		f.w.Header().Set("Allow", f.header.Get("Allow"))
		writeError(f.w, http.StatusMethodNotAllowed, codeMethodNotAllowed)
		return
	}
	writeError(f.w, http.StatusNotFound, codeNotFound)
}
