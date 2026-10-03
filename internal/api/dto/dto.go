// Package dto declares every response body the API can send, so nothing else can reach a caller.
package dto

import (
	"encoding/json"
	"errors"

	"github.com/dortort/wawarden/internal/metrics"
)

const jsonContentType = "application/json; charset=utf-8"

var errNoResponse = errors.New("dto: no response to encode")

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
	Text []byte `json:"-"`
}

func (p Prometheus) encode() (string, []byte, error) { return metrics.ContentType, p.Text, nil }

func encodeJSON(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return jsonContentType, b, nil
}
