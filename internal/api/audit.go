package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	actionUnrouted = "rest.unrouted"
	maxPeer        = 64
	auditWait      = 2 * time.Second
)

type ReadEvent struct {
	At      time.Time
	Client  string
	Action  string
	Chat    policy.CanonicalChat
	OK      bool
	Reason  string
	Peer    string
	Message cursor.Ref
	Bytes   int64
}

type Auditor interface {
	Record(ctx context.Context, e ReadEvent) error
}

type auditNote struct {
	action  string
	chat    policy.CanonicalChat
	reason  string
	message cursor.Ref
	bytes   int64
}

type noteKey struct{}

func noteFrom(ctx context.Context) *auditNote {
	n, _ := ctx.Value(noteKey{}).(*auditNote)
	return n
}

type writerState uint8

const (
	unrecorded writerState = iota
	recorded
	replaced
)

// Every response passes WriteHeader here first, so its audit row commits before a byte reaches the client.
type auditWriter struct {
	http.ResponseWriter
	commit    func(action, reason string, bytes int64) error
	note      *auditNote
	failed    func(action string, err error)
	state     writerState
	completed bool
}

func audited(w http.ResponseWriter, r *http.Request, audit Auditor, failed func(action string, err error), now func() time.Time) (http.ResponseWriter, *http.Request) {
	note := &auditNote{action: actionUnrouted}
	r = r.WithContext(context.WithValue(r.Context(), noteKey{}, note))
	client := clientFrom(r.Context())
	commit := func(action, reason string, bytes int64) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditWait)
		defer cancel()
		err := audit.Record(ctx, ReadEvent{
			At: now(), Client: client.ID, Action: action, Chat: note.chat, OK: reason == outcomeOK, Reason: reason, Peer: peer(r.RemoteAddr),
			Message: note.message, Bytes: bytes,
		})
		if err != nil && r.Context().Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errBusy
		}
		return err
	}
	return &auditWriter{ResponseWriter: w, commit: commit, note: note, failed: failed}, r
}

func (a *auditWriter) record(status int) error {
	reason := a.note.reason
	if reason == "" {
		reason = statusReason(status)
	}
	return a.commit(a.note.action, reason, a.note.bytes)
}

func auditCommitted(w http.ResponseWriter) bool {
	a, ok := w.(*auditWriter)
	return ok && a.state == recorded
}

func completeAudit(w http.ResponseWriter, action, reason string, bytes int64) {
	a, ok := w.(*auditWriter)
	if !ok || a.state != recorded || a.completed {
		return
	}
	a.completed = true
	if err := a.commit(action, reason, bytes); err != nil {
		a.failed(action, err)
	}
}

func (a *auditWriter) WriteHeader(status int) {
	switch a.state {
	case recorded:
		a.ResponseWriter.WriteHeader(status)
		return
	case replaced:
		return
	}
	if err := a.record(status); err != nil {
		a.state = replaced
		h := a.Header()
		h.Del("Retry-After")
		h.Del("Allow")
		if errors.Is(err, errBusy) {
			setRetryAfter(h, errBusy.retryAfter)
			writeError(a.ResponseWriter, errBusy.status, errBusy.code)
			return
		}
		writeError(a.ResponseWriter, http.StatusInternalServerError, codeInternal)
		return
	}
	a.state = recorded
	a.ResponseWriter.WriteHeader(status)
}

func (a *auditWriter) Write(p []byte) (int, error) {
	if a.state == unrecorded {
		a.WriteHeader(http.StatusOK)
	}
	if a.state == replaced {
		return len(p), nil
	}
	return a.ResponseWriter.Write(p)
}

func (a *auditWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }

func statusReason(status int) string {
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusPartialContent:
		return outcomeOK
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusMethodNotAllowed:
		return codeMethodNotAllowed
	case http.StatusRequestedRangeNotSatisfiable:
		return codeRangeNotSatisfiable
	}
	return codeInternal
}

func peer(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil || len(host) > maxPeer {
		return ""
	}
	return host
}
