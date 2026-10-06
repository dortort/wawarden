package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	maxBodyBytes  = 16 << 10
	maxJSONDepth  = 8
	byteOrderMark = "\xef\xbb\xbf"
)

var (
	errMediaType = &codedError{status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType}
	errTooLarge  = &codedError{status: http.StatusRequestEntityTooLarge, code: codeBodyTooLarge}
	errBadBody   = &codedError{status: http.StatusBadRequest, code: codeInvalidBody}
)

func (r *Request) DecodeJSON(dst any) error { return r.decodeJSON(dst, maxBodyBytes) }

func (r *Request) decodeJSON(dst any, limit int64) error {
	if r == nil || r.w == nil || r.req == nil || r.req.Body == nil {
		return errBadBody
	}
	if !jsonMediaType(r.req.Header) {
		ignoreBody(r.w, r.req)
		return errMediaType
	}
	if r.req.ContentLength > limit {
		ignoreBody(r.w, r.req)
		return errTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(r.w, r.req.Body, limit))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return errTooLarge
	}
	if err != nil {
		return errBadBody
	}
	return decodeStrict(body, dst)
}

func jsonMediaType(h http.Header) bool {
	values := h.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return false
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func decodeStrict(body []byte, dst any) error {
	if !utf8.Valid(body) || bytes.HasPrefix(body, []byte(byteOrderMark)) || !wellFormed(body) {
		return errBadBody
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadBody
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errBadBody
	}
	return nil
}

type jsonFrame struct {
	object  bool
	wantKey bool
	keys    map[string]struct{}
}

func wellFormed(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var stack []*jsonFrame
	complete := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return complete
		}
		if err != nil || complete {
			return false
		}
		var top *jsonFrame
		if n := len(stack); n > 0 {
			top = stack[n-1]
		}
		if top == nil && tok != json.Delim('{') {
			return false
		}
		if top != nil && top.object && top.wantKey {
			if tok == json.Delim('}') {
				stack = stack[:len(stack)-1]
				complete = valueDone(stack)
				continue
			}
			key, ok := tok.(string)
			if _, seen := top.keys[key]; !ok || seen || !snakeCase(key) {
				return false
			}
			top.keys[key] = struct{}{}
			top.wantKey = false
			continue
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			if len(stack) == maxJSONDepth {
				return false
			}
			stack = append(stack, &jsonFrame{object: tok == json.Delim('{'), wantKey: true, keys: map[string]struct{}{}})
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
			complete = valueDone(stack)
		default:
			valueDone(stack)
		}
	}
}

func valueDone(stack []*jsonFrame) bool {
	if len(stack) == 0 {
		return true
	}
	if top := stack[len(stack)-1]; top.object {
		top.wantKey = true
	}
	return false
}

func snakeCase(key string) bool {
	if key == "" || key[0] < 'a' || key[0] > 'z' {
		return false
	}
	for i := 1; i < len(key); i++ {
		if c := key[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}
