package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/dortort/wawarden/internal/api/dto"
)

const (
	EventMessageCreated = "message.created"

	maxPlaceholders = 64
	maxTemplateDeep = 32
	MaxBodyBytes    = 1 << 20
)

var (
	placeholder      = regexp.MustCompile(`\{\{\s*([a-z_]+(?:\.[a-z_]+)*)\s*\}\}`)
	wholePlaceholder = regexp.MustCompile(`^\{\{\s*([a-z_]+(?:\.[a-z_]+)*)\s*\}\}$`)

	errTemplate  = errors.New("hooks: the payload template is not valid")
	errBodyLarge = errors.New("hooks: the rendered payload is larger than 1 MiB")

	// Every path a template may name, taken from the default body with every field present.
	knownPaths = pathsOf(mustGeneric(Envelope{Message: &dto.Message{}}))
)

// Envelope is the default body of a delivery, and the values a payload template can name.
type Envelope struct {
	ID      string       `json:"id"`
	Event   string       `json:"event"`
	Time    string       `json:"time"`
	Webhook string       `json:"webhook"`
	Message *dto.Message `json:"message"`
}

type template struct {
	src         string
	contentType string
}

func parseTemplate(src, contentType string) (*template, error) {
	names := placeholder.FindAllStringSubmatch(src, -1)
	if len(names) > maxPlaceholders {
		return nil, errTemplate
	}
	for _, n := range names {
		if !knownPaths[n[1]] {
			return nil, errTemplate
		}
	}
	if contentType == ContentJSON {
		if !json.Valid([]byte(src)) || depth(src) > maxTemplateDeep {
			return nil, errTemplate
		}
	}
	return &template{src: src, contentType: contentType}, nil
}

func depth(src string) int {
	dec := json.NewDecoder(strings.NewReader(src))
	deepest, current := 0, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return deepest
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			current++
			deepest = max(deepest, current)
		case json.Delim('}'), json.Delim(']'):
			current--
		}
	}
}

// Body renders the delivery's body: the envelope itself without a template, the template with its placeholders filled otherwise.
func Body(e Envelope, payload, contentType string) ([]byte, error) {
	if payload == "" {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		return limited(b)
	}
	t, err := parseTemplate(payload, contentType)
	if err != nil {
		return nil, err
	}
	values, err := generic(e)
	if err != nil {
		return nil, err
	}
	if t.contentType == ContentText {
		return limited([]byte(interpolate(t.src, values)))
	}
	return renderJSON(t.src, values)
}

func limited(b []byte) ([]byte, error) {
	if len(b) > MaxBodyBytes {
		return nil, errBodyLarge
	}
	return b, nil
}

func renderJSON(src string, values any) ([]byte, error) {
	type frame struct {
		object  bool
		count   int
		wantKey bool
	}
	var out bytes.Buffer
	var stack []frame
	before := func() {
		if len(stack) == 0 {
			return
		}
		f := &stack[len(stack)-1]
		switch {
		case f.object && !f.wantKey:
			out.WriteByte(':')
		case f.count > 0:
			out.WriteByte(',')
		}
	}
	after := func() {
		if len(stack) == 0 {
			return
		}
		f := &stack[len(stack)-1]
		switch {
		case f.object && f.wantKey:
			f.wantKey = false
		case f.object:
			f.wantKey = true
			f.count++
		default:
			f.count++
		}
	}
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errTemplate
		}
		if out.Len() > MaxBodyBytes {
			return nil, errBodyLarge
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				before()
				out.WriteRune(rune(v))
				stack = append(stack, frame{object: v == '{', wantKey: v == '{'})
			default:
				stack = stack[:len(stack)-1]
				out.WriteRune(rune(v))
				after()
			}
			continue
		case string:
			isKey := len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].wantKey
			before()
			if m := wholePlaceholder.FindStringSubmatch(v); m != nil && !isKey {
				raw, err := json.Marshal(lookup(values, m[1]))
				if err != nil {
					return nil, err
				}
				out.Write(raw)
			} else {
				text := v
				if !isKey {
					text = interpolate(v, values)
				}
				raw, err := json.Marshal(text)
				if err != nil {
					return nil, err
				}
				out.Write(raw)
			}
		case json.Number:
			before()
			out.WriteString(v.String())
		case bool:
			before()
			out.WriteString(strconv.FormatBool(v))
		case nil:
			before()
			out.WriteString("null")
		}
		after()
	}
	return limited(out.Bytes())
}

func interpolate(s string, values any) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		return text(lookup(values, placeholder.FindStringSubmatch(m)[1]))
	})
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func lookup(values any, path string) any {
	cur := values
	for part := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func generic(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func mustGeneric(v any) any {
	out, err := generic(v)
	if err != nil {
		panic("hooks: the envelope does not encode: " + err.Error())
	}
	return out
}

func pathsOf(v any) map[string]bool {
	out := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, child := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			walk(p, child)
		}
	}
	walk("", v)
	return out
}
