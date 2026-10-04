package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
)

const jsonType = "application/json"

type sample struct {
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to"`
	Count   int    `json:"count"`
	Flag    bool   `json:"flag"`
	Options *struct {
		Level int `json:"level"`
	} `json:"options"`
	Items []struct {
		Name string `json:"name"`
	} `json:"items"`
	Nested any `json:"nested"`
}

func jsonRequest(t *testing.T, contentTypes []string, body string) (*httptest.ResponseRecorder, *Request) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/probe", strings.NewReader(body))
	for _, ct := range contentTypes {
		r.Header.Add("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	return rec, &Request{w: rec, req: r}
}

func nested(depth int) string {
	return strings.Repeat(`{"nested":`, depth-1) + "{}" + strings.Repeat("}", depth-1)
}

func TestDecodeJSON(t *testing.T) {
	atCap := `{"text":"` + strings.Repeat("a", maxBodyBytes-len(`{"text":""}`)) + `"}`
	bs := string(rune(92))
	tests := []struct {
		name  string
		types []string
		body  string
		want  error
	}{
		{name: "fields", types: []string{jsonType}, body: `{"text":"hi","reply_to":"x","count":3,"flag":true}`},
		{name: "empty object", types: []string{jsonType}, body: `{}`},
		{name: "whitespace around the object", types: []string{jsonType}, body: " \n\t{\"text\":\"hi\"}\r\n "},
		{name: "nested objects and arrays", types: []string{jsonType}, body: `{"options":{"level":1},"items":[{"name":"a"},{"name":"b"}]}`},
		{name: "the same key in different objects", types: []string{jsonType}, body: `{"items":[{"name":"a"},{"name":"b"}],"nested":{"name":"c"}}`},
		{name: "charset utf-8", types: []string{"application/json; charset=utf-8"}, body: `{}`},
		{name: "charset in another case", types: []string{"application/json;charset=UTF-8"}, body: `{}`},
		{name: "media type in another case", types: []string{"Application/JSON"}, body: `{}`},
		{name: "null for a field", types: []string{jsonType}, body: `{"options":null}`},
		{name: "depth of eight", types: []string{jsonType}, body: nested(8)},
		{name: "exactly at the cap", types: []string{jsonType}, body: atCap},
		{name: "escaped key", types: []string{jsonType}, body: `{"te` + bs + `u0078t":"hi"}`},
		{name: "escaped character in a value", types: []string{jsonType}, body: `{"text":"` + bs + `u00e9 ` + bs + `uD83D` + bs + `uDE00"}`},
		{name: "text with every escape", types: []string{jsonType}, body: `{"text":"\"\\\/\b\f\n\r\té"}`},

		{name: "one byte over the cap", types: []string{jsonType}, body: atCap + " ", want: errTooLarge},
		{name: "no content type", body: `{}`, want: errMediaType},
		{name: "two content types", types: []string{jsonType, jsonType}, body: `{}`, want: errMediaType},
		{name: "text/json", types: []string{"text/json"}, body: `{}`, want: errMediaType},
		{name: "problem+json", types: []string{"application/problem+json"}, body: `{}`, want: errMediaType},
		{name: "form", types: []string{"application/x-www-form-urlencoded"}, body: `text=hi`, want: errMediaType},
		{name: "multipart", types: []string{"multipart/form-data; boundary=synthetic"}, body: `{}`, want: errMediaType},
		{name: "plain text", types: []string{"text/plain"}, body: `{}`, want: errMediaType},
		{name: "another charset", types: []string{"application/json; charset=latin1"}, body: `{}`, want: errMediaType},
		{name: "another parameter", types: []string{"application/json; version=2"}, body: `{}`, want: errMediaType},
		{name: "two media types in one header", types: []string{"application/json, text/plain"}, body: `{}`, want: errMediaType},
		{name: "empty content type", types: []string{""}, body: `{}`, want: errMediaType},

		{name: "unknown field", types: []string{jsonType}, body: `{"text":"a","nope":1}`, want: errBadBody},
		{name: "unknown nested field", types: []string{jsonType}, body: `{"options":{"level":1,"other":2}}`, want: errBadBody},
		{name: "duplicate key", types: []string{jsonType}, body: `{"text":"a","text":"b"}`, want: errBadBody},
		{name: "duplicate key after unescaping", types: []string{jsonType}, body: `{"text":"a","te` + bs + `u0078t":"b"}`, want: errBadBody},
		{name: "duplicate keys escaped in two ways", types: []string{jsonType}, body: `{"te` + bs + `u0078t":"a","` + bs + `u0074ext":"b"}`, want: errBadBody},
		{name: "duplicate key after unescaping in free-form content", types: []string{jsonType}, body: `{"nested":{"a":1,"` + bs + `u0061":2}}`, want: errBadBody},
		{name: "duplicate key after unescaping at depth six", types: []string{jsonType}, body: `{"nested":{"a":{"b":{"c":{"d":{"e":1,"` + bs + `u0065":2}}}}}}`, want: errBadBody},
		{name: "escaped upper-case key", types: []string{jsonType}, body: `{"` + bs + `u0054ext":"a"}`, want: errBadBody},
		{name: "escaped key that folds to a field", types: []string{jsonType}, body: `{"te` + bs + `u017Ft":"a"}`, want: errBadBody},
		{name: "duplicate nested key", types: []string{jsonType}, body: `{"options":{"level":1,"level":2}}`, want: errBadBody},
		{name: "duplicate key in an array element", types: []string{jsonType}, body: `{"items":[{"name":"a"},{"name":"b","name":"c"}]}`, want: errBadBody},
		{name: "duplicate key with values of other types", types: []string{jsonType}, body: `{"nested":[1,2],"nested":3}`, want: errBadBody},
		{name: "duplicate key in free-form content", types: []string{jsonType}, body: `{"nested":{"a":1,"a":2}}`, want: errBadBody},
		{name: "key differing in case", types: []string{jsonType}, body: `{"text":"a","Text":"b"}`, want: errBadBody},
		{name: "upper-case key", types: []string{jsonType}, body: `{"TEXT":"a"}`, want: errBadBody},
		{name: "key that folds to a field", types: []string{jsonType}, body: "{\"teſt\":\"a\"}", want: errBadBody},
		{name: "camel-case key", types: []string{jsonType}, body: `{"replyTo":"a"}`, want: errBadBody},
		{name: "key starting with a digit", types: []string{jsonType}, body: `{"1text":"a"}`, want: errBadBody},
		{name: "key starting with an underscore", types: []string{jsonType}, body: `{"_text":"a"}`, want: errBadBody},
		{name: "empty key", types: []string{jsonType}, body: `{"":"a"}`, want: errBadBody},
		{name: "key with a hyphen", types: []string{jsonType}, body: `{"reply-to":"a"}`, want: errBadBody},
		{name: "upper-case key in free-form content", types: []string{jsonType}, body: `{"nested":{"Name":1}}`, want: errBadBody},
		{name: "trailing value", types: []string{jsonType}, body: `{"text":"a"} {"text":"b"}`, want: errBadBody},
		{name: "trailing garbage", types: []string{jsonType}, body: `{"text":"a"}x`, want: errBadBody},
		{name: "two objects", types: []string{jsonType}, body: `{}{}`, want: errBadBody},
		{name: "trailing comma", types: []string{jsonType}, body: `{"text":"a",}`, want: errBadBody},
		{name: "top-level array", types: []string{jsonType}, body: `[]`, want: errBadBody},
		{name: "top-level null", types: []string{jsonType}, body: `null`, want: errBadBody},
		{name: "top-level string", types: []string{jsonType}, body: `"text"`, want: errBadBody},
		{name: "top-level number", types: []string{jsonType}, body: `1`, want: errBadBody},
		{name: "empty body", types: []string{jsonType}, body: ``, want: errBadBody},
		{name: "whitespace only", types: []string{jsonType}, body: " \n", want: errBadBody},
		{name: "truncated", types: []string{jsonType}, body: `{"text":"a"`, want: errBadBody},
		{name: "byte-order mark", types: []string{jsonType}, body: "\xef\xbb\xbf{}", want: errBadBody},
		{name: "invalid UTF-8 in a value", types: []string{jsonType}, body: "{\"text\":\"\xff\"}", want: errBadBody},
		{name: "invalid UTF-8 in a key", types: []string{jsonType}, body: "{\"te\xc3\":\"a\"}", want: errBadBody},
		{name: "depth of nine", types: []string{jsonType}, body: nested(9), want: errBadBody},
		{name: "deep array", types: []string{jsonType}, body: `{"nested":` + strings.Repeat("[", 8) + strings.Repeat("]", 8) + `}`, want: errBadBody},
		{name: "depth bomb", types: []string{jsonType}, body: `{"nested":` + strings.Repeat("[", 50000) + strings.Repeat("]", 50000) + `}`, want: errTooLarge},
		{name: "depth bomb under the cap", types: []string{jsonType}, body: `{"nested":` + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + `}`, want: errBadBody},
		{name: "wrong type", types: []string{jsonType}, body: `{"text":5}`, want: errBadBody},
		{name: "number out of range", types: []string{jsonType}, body: `{"count":1e400}`, want: errBadBody},
		{name: "fraction for an integer", types: []string{jsonType}, body: `{"count":1.5}`, want: errBadBody},
		{name: "single quotes", types: []string{jsonType}, body: `{'text':'a'}`, want: errBadBody},
		{name: "comment", types: []string{jsonType}, body: `{"text":"a"/* x */}`, want: errBadBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, req := jsonRequest(t, tt.types, tt.body)
			var dst sample
			err := req.DecodeJSON(&dst)
			if err != tt.want {
				t.Fatalf("DecodeJSON = %v, want %v", err, tt.want)
			}
			if err != nil && strings.ContainsAny(err.Error(), `{}"`) {
				t.Fatalf("the error %q repeats request content", err)
			}
		})
	}
}

func TestDecodedValues(t *testing.T) {
	bs := string(rune(92))
	_, req := jsonRequest(t, []string{jsonType}, `{"te`+bs+`u0078t":"hi `+bs+`u00e9","reply_to":"ref","count":-3,"flag":true,"options":{"level":2},"items":[{"name":"a"}]}`)
	var got sample
	if err := req.DecodeJSON(&got); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if got.Text != "hi é" || got.ReplyTo != "ref" || got.Count != -3 || !got.Flag || got.Options == nil || got.Options.Level != 2 || len(got.Items) != 1 || got.Items[0].Name != "a" {
		t.Fatalf("decoded %+v", got)
	}
}

type tripwire struct {
	t    *testing.T
	read bool
}

func (b *tripwire) Read([]byte) (int, error) {
	b.read = true
	b.t.Error("the request body was read")
	return 0, io.ErrUnexpectedEOF
}

func (*tripwire) Close() error { return nil }

func TestRefusalsBeforeTheBodyLeaveItUnread(t *testing.T) {
	tests := []struct {
		name          string
		contentType   string
		contentLength int64
		want          error
	}{
		{name: "media type", contentType: "text/plain", contentLength: 10, want: errMediaType},
		{name: "announced length over the cap", contentType: jsonType, contentLength: 16385, want: errTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, req := jsonRequest(t, []string{tt.contentType}, "")
			body := &tripwire{t: t}
			req.req.Body, req.req.ContentLength = body, tt.contentLength
			if err := req.DecodeJSON(&sample{}); err != tt.want {
				t.Fatalf("DecodeJSON = %v, want %v", err, tt.want)
			}
			if body.read || rec.Header().Get("Connection") != "close" {
				t.Fatalf("read %v, Connection %q: a refused body is never read and its connection is closed", body.read, rec.Header().Get("Connection"))
			}
		})
	}
}

func TestChunkedBodyOverTheCap(t *testing.T) {
	_, req := jsonRequest(t, []string{jsonType}, `{"text":"`+strings.Repeat("a", 3*maxBodyBytes)+`"}`)
	req.req.ContentLength = -1
	if err := req.DecodeJSON(&sample{}); err != errTooLarge {
		t.Fatalf("DecodeJSON = %v, want %v", err, errTooLarge)
	}
}

func sizedBody(size int) string {
	return `{"text":"` + strings.Repeat("a", size-len(`{"text":""}`)) + `"}`
}

func TestTheBodyCapIs16384Bytes(t *testing.T) {
	for _, tt := range []struct {
		size    int
		chunked bool
		want    error
	}{
		{size: 16384},
		{size: 16384, chunked: true},
		{size: 16385, want: errTooLarge},
		{size: 16385, chunked: true, want: errTooLarge},
	} {
		_, req := jsonRequest(t, []string{jsonType}, sizedBody(tt.size))
		if tt.chunked {
			req.req.ContentLength = -1
		}
		if err := req.DecodeJSON(&sample{}); err != tt.want {
			t.Errorf("DecodeJSON of %d bytes, chunked %v = %v, want %v", tt.size, tt.chunked, err, tt.want)
		}
	}

	f := newDecodeFixture(t)
	lengths := make(chan int64, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lengths <- r.ContentLength
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	tests := []struct {
		name    string
		size    int
		chunked bool
		status  int
		body    string
	}{
		{name: "16384 bytes announced", size: 16384, status: http.StatusOK, body: `{"status":"ok"}`},
		{name: "16384 bytes chunked", size: 16384, chunked: true, status: http.StatusOK, body: `{"status":"ok"}`},
		{name: "16385 bytes announced", size: 16385, status: http.StatusRequestEntityTooLarge, body: `{"error":"` + codeBodyTooLarge + `"}`},
		{name: "16385 bytes chunked", size: 16385, chunked: true, status: http.StatusRequestEntityTooLarge, body: `{"error":"` + codeBodyTooLarge + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/probe/decode", strings.NewReader(sizedBody(tt.size)))
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+keyWriter)
			req.Header.Set("Content-Type", jsonType)
			wantLength := int64(tt.size)
			if tt.chunked {
				req.ContentLength, wantLength = -1, -1
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if closeErr := resp.Body.Close(); err != nil || closeErr != nil {
				t.Fatalf("read the response: %v, %v", err, closeErr)
			}
			decodedText := -1
			if resp.StatusCode == http.StatusOK {
				decodedText = len((<-f.decoded).Text)
			}
			if length := <-lengths; length != wantLength {
				t.Fatalf("the server saw a Content-Length of %d, want %d", length, wantLength)
			}
			if resp.StatusCode != tt.status || string(got) != tt.body {
				t.Fatalf("response = %d %q, want %d %q", resp.StatusCode, got, tt.status, tt.body)
			}
			if want := tt.size - len(`{"text":""}`); tt.status == http.StatusOK && decodedText != want {
				t.Fatalf("decoded %d bytes of text, want %d", decodedText, want)
			}
		})
	}
}

func TestADetachedRequestDecodesNothing(t *testing.T) {
	for _, req := range []*Request{nil, {}} {
		if err := req.DecodeJSON(&sample{}); err != errBadBody {
			t.Fatalf("DecodeJSON on %#v = %v, want %v", req, err, errBadBody)
		}
	}
}

type decodeFixture struct {
	handler http.Handler
	decoded chan sample
}

const (
	keyWriter = "synthetic-writer-client"
	keyReader = "synthetic-reader-client"
)

func newDecodeFixture(t *testing.T) *decodeFixture {
	t.Helper()
	chat, ok := policy.Normalize("120363000000000001@g.us")
	if !ok {
		t.Fatal("Normalize refused the synthetic group")
	}
	f := &decodeFixture{decoded: make(chan sample, 1)}
	f.handler = NewClientHandler(ClientDeps{
		Authenticator: fakeAuthenticator{
			keyWriter: &policy.Client{ID: "client-writer", Write: map[policy.CanonicalChat]struct{}{chat: {}}, ExpiresAt: testNow.Add(time.Hour)},
			keyReader: liveClient("client-reader"),
		},
		Metrics: metrics.NewRegistry(),
		Now:     fixedNow,
	})
	f.handler.(*pipeline).router.write("POST /probe/decode", func(_ context.Context, _ policy.WriteGrant, r *Request) (dto.Response, error) {
		var in sample
		if err := r.DecodeJSON(&in); err != nil {
			return nil, err
		}
		f.decoded <- in
		return dto.Health{Status: "ok"}, nil
	})
	return f
}

func TestDecodingRoutesAnswerWithFixedCodes(t *testing.T) {
	f := newDecodeFixture(t)
	tests := []struct {
		name, contentType, body string
		status                  int
		code                    string
	}{
		{name: "accepted", contentType: jsonType, body: `{"text":"hi"}`, status: http.StatusOK},
		{name: "unsupported media type", contentType: "text/plain", body: `{"text":"hi"}`, status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType},
		{name: "too large", contentType: jsonType, body: `{"text":"` + strings.Repeat("a", maxBodyBytes) + `"}`, status: http.StatusRequestEntityTooLarge, code: codeBodyTooLarge},
		{name: "invalid", contentType: jsonType, body: `{"text":"<synthetic secret>","text":"x"}`, status: http.StatusBadRequest, code: codeInvalidBody},
		{name: "unknown field", contentType: jsonType, body: `{"synthetic_secret":"x"}`, status: http.StatusBadRequest, code: codeInvalidBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/probe/decode", strings.NewReader(tt.body))
			r.Header.Set("Authorization", "Bearer "+keyWriter)
			r.Header.Set("Content-Type", tt.contentType)
			rec := serve(f.handler, r)
			if tt.code == "" {
				if rec.Code != tt.status || rec.Body.String() != `{"status":"ok"}` || (<-f.decoded).Text != "hi" {
					t.Fatalf("response = %d %q, want 200 from the handler", rec.Code, rec.Body.String())
				}
				return
			}
			requireError(t, rec, tt.status, tt.code)
			requireSecurityHeaders(t, rec.Header())
			if strings.Contains(rec.Body.String(), "synthetic") {
				t.Fatalf("the response %q repeats the request", rec.Body.String())
			}
		})
	}
}

func TestDecodingIsReachableOnlyAfterAuthenticationAndADecision(t *testing.T) {
	f := newDecodeFixture(t)
	tests := []struct {
		name   string
		key    string
		status int
		code   string
	}{
		{name: "no credential", status: http.StatusUnauthorized, code: codeUnauthorized},
		{name: "unknown credential", key: "synthetic-unknown-client", status: http.StatusUnauthorized, code: codeUnauthorized},
		{name: "client without a write grant", key: keyReader, status: http.StatusNotFound, code: codeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{"Content-Type": {jsonType}}
			if tt.key != "" {
				header.Set("Authorization", "Bearer "+tt.key)
			}
			r := newRequest(t, http.MethodPost, "/probe/decode", header)
			r.ContentLength = 13
			requireError(t, serve(f.handler, r), tt.status, tt.code)
			select {
			case got := <-f.decoded:
				t.Fatalf("the handler decoded %+v without a grant", got)
			default:
			}
		})
	}
}

func FuzzDecodeJSON(f *testing.F) {
	bs := string(rune(92))
	for _, seed := range []struct{ contentType, body string }{
		{jsonType, `{"text":"hi","reply_to":"x","count":3,"flag":true}`},
		{jsonType, `{"te` + bs + `u0078t":"hi ` + bs + `u00e9"}`},
		{jsonType, `{"te` + bs + `u0078t":"a","text":"b"}`},
		{jsonType, `{"options":{"level":1},"items":[{"name":"a"}],"nested":{"a":[1,{"b":null}]}}`},
		{jsonType, `{"text":"a","text":"b"}`},
		{jsonType, nested(9)},
		{jsonType, `{"text":"a"} {}`},
		{jsonType, "\xef\xbb\xbf{}"},
		{"application/json; charset=utf-8", `{"nested":"é"}`},
		{"text/plain", `{}`},
	} {
		f.Add(seed.contentType, []byte(seed.body))
	}
	fixed := map[error]bool{errMediaType: true, errTooLarge: true, errBadBody: true}
	f.Fuzz(func(t *testing.T, contentType string, body []byte) {
		_, req := jsonRequest(t, []string{contentType}, string(body))
		var dst sample
		err := req.DecodeJSON(&dst)
		if err != nil {
			if !fixed[err] {
				t.Fatalf("DecodeJSON = %#v, not one of the fixed refusals", err)
			}
			return
		}
		if !utf8.Valid(body) || strings.HasPrefix(string(body), byteOrderMark) || !json.Valid(body) || len(body) > maxBodyBytes {
			t.Fatalf("DecodeJSON accepted %q", body)
		}
		var generic map[string]any
		if err := json.Unmarshal(body, &generic); err != nil || generic == nil {
			t.Fatalf("DecodeJSON accepted %q, which is not a JSON object: %v", body, err)
		}
		if depth, ok := conforming(generic); !ok || depth > maxJSONDepth {
			t.Fatalf("DecodeJSON accepted %q, of depth %d with keys conforming %v", body, depth, ok)
		}
		again, err := json.Marshal(dst)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var roundTrip sample
		if err := decodeStrict(again, &roundTrip); err != nil {
			t.Fatalf("the re-encoded value %s is refused: %v", again, err)
		}
		if reencoded, _ := json.Marshal(roundTrip); string(reencoded) != string(again) {
			t.Fatalf("round trip changed %s into %s", again, reencoded)
		}
	})
}

func conforming(v any) (int, bool) {
	switch x := v.(type) {
	case map[string]any:
		depth := 0
		for k, e := range x {
			if !snakeCaseModel(k) {
				return 0, false
			}
			d, ok := conforming(e)
			if !ok {
				return 0, false
			}
			depth = max(depth, d)
		}
		return depth + 1, true
	case []any:
		depth := 0
		for _, e := range x {
			d, ok := conforming(e)
			if !ok {
				return 0, false
			}
			depth = max(depth, d)
		}
		return depth + 1, true
	}
	return 0, true
}

func snakeCaseModel(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		lower, digit := r >= 'a' && r <= 'z', r >= '0' && r <= '9'
		if !lower && (i == 0 || !digit && r != '_') {
			return false
		}
	}
	return true
}
