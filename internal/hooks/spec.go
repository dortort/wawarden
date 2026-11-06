// Package hooks holds the client webhooks: their validated settings, the filters that pick the new messages they post, the payload templates, and the dispatcher that delivers them.
package hooks

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/notify"
)

const (
	MethodPost  = http.MethodPost
	MethodPut   = http.MethodPut
	MethodPatch = http.MethodPatch

	ContentJSON = "application/json"
	ContentText = "text/plain"

	DirectionAny      = "any"
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"

	MaxPerClient     = 10
	MaxURLBytes      = 2048
	MaxPayloadBytes  = 16 << 10
	MaxHeaders       = 8
	MaxHeaderName    = 64
	MaxHeaderValue   = 1024
	MaxFilterChats   = 64
	MaxContainsChars = 128
	chatRefChars     = 32
)

var (
	methods      = []string{MethodPost, MethodPut, MethodPatch}
	contentTypes = []string{ContentJSON, ContentText}
	directions   = []string{DirectionAny, DirectionInbound, DirectionOutbound}
	kinds        = []string{"text", "media", "reaction", "poll_update", "other"}

	headerName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	headerValue = regexp.MustCompile(`^[\x20-\x7e]*$`)
	chatRef     = regexp.MustCompile(`^[0-9a-f]{32}$`)

	// Headers the dispatcher sets itself, or that would change how the request is framed or routed.
	reservedHeaders = []string{"host", "content-type", "content-length", "content-encoding", "transfer-encoding", "connection", "keep-alive",
		"upgrade", "te", "trailer", "expect", "user-agent", "proxy-authorization", "proxy-connection", "forwarded", "x-forwarded-for",
		"x-forwarded-host", "x-forwarded-proto", "x-real-ip"}
)

// Kinds lists the message kinds a filter may name.
func Kinds() []any {
	out := make([]any, len(kinds))
	for i, k := range kinds {
		out[i] = k
	}
	return out
}

// Spec is one webhook's settings as its client chose them.
type Spec struct {
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
	Payload     string            `json:"payload"`
	Filter      Filter            `json:"filter"`
}

// Filter picks the new messages a webhook posts: a message must pass every set field.
type Filter struct {
	Chats     []string `json:"chats,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Contains  string   `json:"contains,omitempty"`
}

type SpecError struct{ code string }

func (e *SpecError) Error() string { return "hooks: the webhook is refused: " + e.code }

func (e *SpecError) Code() string { return e.code }

var (
	ErrURLInvalid         = &SpecError{code: "webhook_url_invalid"}
	ErrMethodInvalid      = &SpecError{code: "webhook_method_invalid"}
	ErrContentTypeInvalid = &SpecError{code: "webhook_content_type_invalid"}
	ErrHeaderInvalid      = &SpecError{code: "webhook_header_invalid"}
	ErrPayloadInvalid     = &SpecError{code: "webhook_payload_invalid"}
	ErrFilterInvalid      = &SpecError{code: "webhook_filter_invalid"}
	ErrTooMany            = &SpecError{code: "webhook_limit_reached"}
)

// Normalize checks a spec and fills its defaults: POST, a JSON body, any direction.
func Normalize(s Spec) (Spec, error) {
	out := Spec{URL: s.URL, Method: strings.ToUpper(s.Method), ContentType: s.ContentType, Headers: map[string]string{}, Payload: s.Payload}
	if out.Method == "" {
		out.Method = MethodPost
	}
	if out.ContentType == "" {
		out.ContentType = ContentJSON
	}
	switch {
	case len(s.URL) > MaxURLBytes || notify.CheckURL(s.URL) != nil:
		return Spec{}, ErrURLInvalid
	case !slices.Contains(methods, out.Method):
		return Spec{}, ErrMethodInvalid
	case !slices.Contains(contentTypes, out.ContentType):
		return Spec{}, ErrContentTypeInvalid
	case len(s.Headers) > MaxHeaders:
		return Spec{}, ErrHeaderInvalid
	}
	for name, value := range s.Headers {
		canonical := http.CanonicalHeaderKey(name)
		lower := strings.ToLower(name)
		if len(name) > MaxHeaderName || !headerName.MatchString(name) || slices.Contains(reservedHeaders, lower) ||
			strings.HasPrefix(lower, "wawarden-") || strings.HasPrefix(lower, "sec-") || strings.HasPrefix(lower, "proxy-") ||
			len(value) > MaxHeaderValue || !headerValue.MatchString(value) {
			return Spec{}, ErrHeaderInvalid
		}
		if _, dup := out.Headers[canonical]; dup {
			return Spec{}, ErrHeaderInvalid
		}
		out.Headers[canonical] = value
	}
	if s.Payload != "" {
		if len(s.Payload) > MaxPayloadBytes || !utf8.ValidString(s.Payload) {
			return Spec{}, ErrPayloadInvalid
		}
		if _, err := parseTemplate(s.Payload, out.ContentType); err != nil {
			return Spec{}, ErrPayloadInvalid
		}
	}
	f, err := normalizeFilter(s.Filter)
	if err != nil {
		return Spec{}, err
	}
	out.Filter = f
	return out, nil
}

func normalizeFilter(f Filter) (Filter, error) {
	out := Filter{Direction: f.Direction, Contains: f.Contains}
	if out.Direction == "" {
		out.Direction = DirectionAny
	}
	switch {
	case !slices.Contains(directions, out.Direction),
		len(f.Chats) > MaxFilterChats,
		len(f.Kinds) > len(kinds),
		!utf8.ValidString(f.Contains), utf8.RuneCountInString(f.Contains) > MaxContainsChars:
		return Filter{}, ErrFilterInvalid
	}
	for _, c := range f.Chats {
		if len(c) != chatRefChars || !chatRef.MatchString(c) {
			return Filter{}, ErrFilterInvalid
		}
		if !slices.Contains(out.Chats, c) {
			out.Chats = append(out.Chats, c)
		}
	}
	for _, k := range f.Kinds {
		if !slices.Contains(kinds, k) {
			return Filter{}, ErrFilterInvalid
		}
		if !slices.Contains(out.Kinds, k) {
			out.Kinds = append(out.Kinds, k)
		}
	}
	slices.Sort(out.Chats)
	slices.Sort(out.Kinds)
	return out, nil
}

// Match reports whether a message passes the filter.
func (f Filter) Match(m dto.Message) bool {
	switch {
	case len(f.Chats) > 0 && !slices.Contains(f.Chats, m.Chat),
		f.Direction == DirectionInbound && m.FromMe,
		f.Direction == DirectionOutbound && !m.FromMe,
		len(f.Kinds) > 0 && !slices.Contains(f.Kinds, m.Kind):
		return false
	case f.Contains == "":
		return true
	case m.Text == nil:
		return false
	}
	return strings.Contains(strings.ToLower(*m.Text), strings.ToLower(f.Contains))
}

// EncodeFilter and DecodeFilter keep a filter in the archive as JSON.
func EncodeFilter(f Filter) (string, error) {
	b, err := json.Marshal(f)
	return string(b), err
}

func DecodeFilter(s string) (Filter, error) {
	var f Filter
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return Filter{}, err
	}
	return normalizeFilter(f)
}

func EncodeHeaders(h map[string]string) (string, error) {
	if h == nil {
		h = map[string]string{}
	}
	b, err := json.Marshal(h)
	return string(b), err
}

func DecodeHeaders(s string) (map[string]string, error) {
	h := map[string]string{}
	if err := json.Unmarshal([]byte(s), &h); err != nil {
		return nil, err
	}
	return h, nil
}
