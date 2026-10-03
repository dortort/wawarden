// Package dto declares every response body the API can send, so nothing else can reach a caller.
package dto

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/dortort/wawarden/internal/metrics"
)

const jsonContentType = "application/json; charset=utf-8"

var (
	errNoResponse = errors.New("dto: no response to encode")
	errNoRegistry = errors.New("dto: no metrics registry to encode")
)

type Response interface {
	encode() (contentType string, body []byte, err error)
}

func Encode(r Response) (contentType string, body []byte, err error) {
	if r == nil {
		return "", nil, errNoResponse
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
