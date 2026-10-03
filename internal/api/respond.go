package api

import (
	"net/http"
	"strconv"

	"github.com/dortort/wawarden/internal/api/dto"
)

const (
	codeForbidden        = "forbidden"
	codeInternal         = "internal_error"
	codeMethodNotAllowed = "method_not_allowed"
	codeNotFound         = "not_found"
	codeTooManyRequests  = "too_many_requests"
	codeUnauthorized     = "unauthorized"
)

func setSecurityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeResponse(w, status, dto.Error{Code: code})
}

func writeResponse(w http.ResponseWriter, status int, resp dto.Response) {
	contentType, body, err := dto.Encode(resp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
