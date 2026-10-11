package engine

import (
	"errors"
	"strconv"
	"time"
)

var (
	ErrNotConnected         = errors.New("engine: not connected, nothing was sent")
	ErrRecipientUnavailable = errors.New("engine: the recipient cannot be reached from this account")
	ErrRejected             = errors.New("engine: WhatsApp rejected the message")
	ErrOutcomeUnknown       = errors.New("engine: whether WhatsApp received the message is unknown")
	ErrSendBusy             = errors.New("engine: another send is in progress")
)

type RejectedError struct{ Code int }

func (e *RejectedError) Error() string {
	if e.Code == 0 {
		return ErrRejected.Error()
	}
	return ErrRejected.Error() + " with code " + strconv.Itoa(e.Code)
}

func (e *RejectedError) Unwrap() error { return ErrRejected }

func RejectionCode(err error) int {
	if r, ok := errors.AsType[*RejectedError](err); ok {
		return r.Code
	}
	return 0
}

type SendRequest struct {
	ID         string
	Chat       string
	Text       string
	Quote      *SendQuote
	Expiration time.Duration
}

type SendQuote struct {
	ID          string
	Participant string
	Text        string
}

type Sent struct {
	ID        string
	Chat      string
	Sender    string
	Timestamp time.Time
}

type BackfillRequest struct {
	Chat         string
	AnchorID     string
	AnchorFromMe bool
	AnchorTime   time.Time
	Count        int
}
