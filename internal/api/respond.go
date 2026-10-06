package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
)

const (
	codeBodyTooLarge         = "body_too_large"
	codeBusy                 = "busy"
	codeForbidden            = "forbidden"
	codeInternal             = "internal_error"
	codeInvalidBody          = "invalid_body"
	codeInvalidCursor        = "invalid_cursor"
	codeMethodNotAllowed     = "method_not_allowed"
	codeNotFound             = "not_found"
	codeRateLimited          = "rate_limited"
	codeTooManyRequests      = "too_many_requests"
	codeUnauthorized         = "unauthorized"
	codeUnsupportedMediaType = "unsupported_media_type"
)

type codedError struct {
	status     int
	code       string
	retryAfter time.Duration
}

func (e *codedError) Error() string { return "api: request refused: " + e.code }

func setSecurityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

func setRetryAfter(h http.Header, wait time.Duration) {
	h.Set("Retry-After", strconv.FormatInt(int64(max((wait+time.Second-1)/time.Second, 1)), 10))
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
