package api

import (
	"net/http"
	"strconv"

	"github.com/dortort/wawarden/internal/api/dto"
)

const (
	codeInternal         = "internal_error"
	codeMethodNotAllowed = "method_not_allowed"
	codeNotFound         = "not_found"
)

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
