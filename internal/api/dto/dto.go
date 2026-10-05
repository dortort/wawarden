// Package dto declares every response body the API can send, so nothing else can reach a caller.
package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/dortort/wawarden/internal/metrics"
)

const jsonContentType = "application/json; charset=utf-8"

var (
	errNoResponse      = errors.New("dto: no response to encode")
	errNoRegistry      = errors.New("dto: no metrics registry to encode")
	errForeignResponse = errors.New("dto: the response type is declared outside package dto")
)

var pkgPath = reflect.TypeFor[Error]().PkgPath()

type Response interface {
	encode() (contentType string, body []byte, err error)
}

func Encode(r Response) (contentType string, body []byte, err error) {
	if r == nil {
		return "", nil, errNoResponse
	}
	t := reflect.TypeOf(r)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() != pkgPath {
		return "", nil, errForeignResponse
	}
	return r.encode()
}

type Error struct {
	Code string `json:"error"`
}

func (e Error) encode() (string, []byte, error) { return encodeJSON(e) }

type Health struct {
	Status string `json:"status"`
}

func (h Health) encode() (string, []byte, error) { return encodeJSON(h) }

type Status struct {
	State        string       `json:"state"`
	Reason       string       `json:"reason"`
	Paired       bool         `json:"paired"`
	Counts       StatusCounts `json:"counts"`
	LastIngestAt *string      `json:"last_ingest_at"`
	Version      string       `json:"version"`
}

type StatusCounts struct {
	Chats            int64 `json:"chats"`
	Messages         int64 `json:"messages"`
	BlobsPending     int64 `json:"history_blobs_pending"`
	BlobsQuarantined int64 `json:"history_blobs_quarantined"`
	InboxBacklog     int64 `json:"inbox_backlog"`
	InboxQuarantined int64 `json:"inbox_quarantined"`
}

func (s Status) encode() (string, []byte, error) { return encodeJSON(s) }

type Pairing struct {
	Code string `json:"code"`
}

func (p Pairing) encode() (string, []byte, error) { return encodeJSON(p) }

type Accepted struct {
	Status string `json:"status"`
}

func (a Accepted) encode() (string, []byte, error) { return encodeJSON(a) }

type Prometheus struct {
	reg *metrics.Registry
}

func Metrics(reg *metrics.Registry) Prometheus { return Prometheus{reg: reg} }

func (p Prometheus) encode() (string, []byte, error) {
	if p.reg == nil {
		return "", nil, errNoRegistry
	}
	var b bytes.Buffer
	if err := p.reg.WriteText(&b); err != nil {
		return "", nil, err
	}
	return metrics.ContentType, b.Bytes(), nil
}

func encodeJSON(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return jsonContentType, b, nil
}
