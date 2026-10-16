package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
)

const (
	actionMediaSent = "rest.media.sent"
	deliveryChunk   = 256 << 10
	deliveryWindow  = 30 * time.Second

	reasonClientGone   = "client_gone"
	reasonWriteTimeout = "write_timeout"
	reasonExpired      = "expired"
	reasonRevoked      = "revoked"
	reasonReadFailed   = "read_failed"
)

var (
	errDeliveryExpired = errors.New("api: the attachment job expired during its delivery")
	errDeliveryDropped = errors.New("api: the attachment job was dropped during its delivery")
)

func deliver(w http.ResponseWriter, r *http.Request, a dto.Attachment, now func() time.Time) {
	var sent int64
	complete := false
	defer func() { a.Finished(sent, complete) }()
	aw, audited := w.(*auditWriter)
	plan, err := a.Plan(strings.Join(r.Header.Values("Range"), ","), strings.Join(r.Header.Values("If-Range"), ","))
	if !audited || err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal)
		return
	}
	start := now()
	if !start.Before(a.Expires()) {
		aw.note.reason = errMediaExpired.code
		writeError(w, errMediaExpired.status, errMediaExpired.code)
		return
	}
	rc := http.NewResponseController(w)
	if rc.SetWriteDeadline(writeDeadline(start, a.Expires())) != nil {
		writeError(w, http.StatusInternalServerError, codeInternal)
		return
	}
	h := w.Header()
	for name, value := range plan.Header() {
		h.Set(name, value)
	}
	if plan.Code() == http.StatusRequestedRangeNotSatisfiable {
		writeError(w, plan.Code(), codeRangeNotSatisfiable)
		return
	}
	head := r.Method == http.MethodHead
	if !head {
		aw.note.bytes = plan.Length()
	}
	w.WriteHeader(plan.Code())
	if !auditCommitted(w) {
		return
	}
	reason := outcomeOK
	if !head {
		sent, reason = stream(r.Context(), w, rc, plan.Body(), a.Expires(), now)
	}
	completeAudit(w, actionMediaSent, reason, sent)
	complete = !head && reason == outcomeOK && plan.Whole()
}

func stream(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, body *io.SectionReader, expires time.Time, now func() time.Time) (int64, string) {
	buf := make([]byte, min(body.Size(), deliveryChunk))
	var sent int64
	for sent < body.Size() {
		if ctx.Err() != nil {
			return sent, reasonClientGone
		}
		chunk := buf[:min(int64(len(buf)), body.Size()-sent)]
		if n, err := body.ReadAt(chunk, sent); n < len(chunk) || err != nil && !errors.Is(err, io.EOF) {
			return sent, readReason(err)
		}
		t := now()
		if !t.Before(expires) {
			return sent, reasonExpired
		}
		if rc.SetWriteDeadline(writeDeadline(t, expires)) != nil {
			return sent, reasonClientGone
		}
		n, err := w.Write(chunk)
		sent += int64(n)
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			return sent, reasonWriteTimeout
		case err != nil:
			return sent, reasonClientGone
		}
	}
	return sent, outcomeOK
}

func readReason(err error) string {
	switch {
	case errors.Is(err, errDeliveryExpired):
		return reasonExpired
	case errors.Is(err, errDeliveryDropped):
		return reasonRevoked
	}
	return reasonReadFailed
}

func writeDeadline(t, expires time.Time) time.Time {
	if d := t.Add(deliveryWindow); d.Before(expires) {
		return d
	}
	return expires
}
