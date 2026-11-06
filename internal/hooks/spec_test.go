package hooks

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/api/dto"
)

func validSpec() Spec {
	return Spec{URL: "https://hooks.example.com/in"}
}

func TestNormalizeFillsTheDefaults(t *testing.T) {
	got, err := Normalize(validSpec())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Method != MethodPost || got.ContentType != ContentJSON || got.Filter.Direction != DirectionAny || got.Headers == nil || got.Payload != "" {
		t.Fatalf("Normalize = %+v", got)
	}
	s := validSpec()
	s.Method, s.Headers = "patch", map[string]string{"x-api-key": "k", "Authorization": "Bearer t"}
	s.Filter = Filter{Chats: []string{strings.Repeat("b", 32), strings.Repeat("a", 32), strings.Repeat("a", 32)}, Kinds: []string{"media", "text", "text"}}
	got, err = Normalize(s)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Method != MethodPatch || got.Headers["X-Api-Key"] != "k" || got.Headers["Authorization"] != "Bearer t" ||
		strings.Join(got.Filter.Chats, ",") != strings.Repeat("a", 32)+","+strings.Repeat("b", 32) || strings.Join(got.Filter.Kinds, ",") != "media,text" {
		t.Fatalf("Normalize = %+v", got)
	}
}

func TestNormalizeRefusesUnsafeOrMalformedSettings(t *testing.T) {
	long := strings.Repeat("a", MaxHeaderValue+1)
	for name, tt := range map[string]struct {
		edit func(*Spec)
		want error
	}{
		"plain http": {func(s *Spec) { s.URL = "http://hooks.example.com/in" }, ErrURLInvalid},
		"credentials": {func(s *Spec) {
			s.URL = (&url.URL{Scheme: "https", User: url.UserPassword("user", "pass"), Host: "hooks.example.com", Path: "/in"}).String()
		}, ErrURLInvalid},
		"a fragment":           {func(s *Spec) { s.URL = "https://hooks.example.com/in#x" }, ErrURLInvalid},
		"a long URL":           {func(s *Spec) { s.URL = "https://hooks.example.com/" + strings.Repeat("a", MaxURLBytes) }, ErrURLInvalid},
		"GET":                  {func(s *Spec) { s.Method = "GET" }, ErrMethodInvalid},
		"DELETE":               {func(s *Spec) { s.Method = "delete" }, ErrMethodInvalid},
		"another content type": {func(s *Spec) { s.ContentType = "application/x-www-form-urlencoded" }, ErrContentTypeInvalid},
		"the host header":      {func(s *Spec) { s.Headers = map[string]string{"Host": "internal"} }, ErrHeaderInvalid},
		"the content type":     {func(s *Spec) { s.Headers = map[string]string{"content-type": "text/html"} }, ErrHeaderInvalid},
		"a signature header":   {func(s *Spec) { s.Headers = map[string]string{"WaWarden-Signature": "v1=0"} }, ErrHeaderInvalid},
		"a proxy header":       {func(s *Spec) { s.Headers = map[string]string{"Proxy-Authorization": "x"} }, ErrHeaderInvalid},
		"a forwarding header":  {func(s *Spec) { s.Headers = map[string]string{"X-Forwarded-For": "127.0.0.1"} }, ErrHeaderInvalid},
		"a line break":         {func(s *Spec) { s.Headers = map[string]string{"X-A": "a\r\nHost: x"} }, ErrHeaderInvalid},
		"a space in the name":  {func(s *Spec) { s.Headers = map[string]string{"X A": "a"} }, ErrHeaderInvalid},
		"a long value":         {func(s *Spec) { s.Headers = map[string]string{"X-A": long} }, ErrHeaderInvalid},
		"names equal but case": {func(s *Spec) { s.Headers = map[string]string{"x-a": "1", "X-A": "2"} }, ErrHeaderInvalid},
		"too many headers":     {func(s *Spec) { s.Headers = manyHeaders(MaxHeaders + 1) }, ErrHeaderInvalid},
		"a payload not JSON":   {func(s *Spec) { s.Payload = `{"a":` }, ErrPayloadInvalid},
		"an unknown field":     {func(s *Spec) { s.Payload = `{"a":"{{message.secret}}"}` }, ErrPayloadInvalid},
		"an unknown root":      {func(s *Spec) { s.Payload, s.ContentType = `{{token}}`, ContentText }, ErrPayloadInvalid},
		"a long payload":       {func(s *Spec) { s.Payload = `"` + strings.Repeat("a", MaxPayloadBytes) + `"` }, ErrPayloadInvalid},
		"too many placeholders": {func(s *Spec) {
			s.Payload, s.ContentType = strings.Repeat("{{message}}", maxPlaceholders+1), ContentText
		}, ErrPayloadInvalid},
		"too deep":        {func(s *Spec) { s.Payload = strings.Repeat("[", 40) + strings.Repeat("]", 40) }, ErrPayloadInvalid},
		"invalid UTF-8":   {func(s *Spec) { s.Payload, s.ContentType = "\xff", ContentText }, ErrPayloadInvalid},
		"a chat id":       {func(s *Spec) { s.Filter.Chats = []string{"120363000000000001@g.us"} }, ErrFilterInvalid},
		"a kind":          {func(s *Spec) { s.Filter.Kinds = []string{"sticker"} }, ErrFilterInvalid},
		"a direction":     {func(s *Spec) { s.Filter.Direction = "both" }, ErrFilterInvalid},
		"a long contains": {func(s *Spec) { s.Filter.Contains = strings.Repeat("é", MaxContainsChars+1) }, ErrFilterInvalid},
	} {
		s := validSpec()
		tt.edit(&s)
		if _, err := Normalize(s); !errors.Is(err, tt.want) {
			t.Errorf("%s: Normalize = %v, want %v", name, err, tt.want)
		}
	}
}

func manyHeaders(n int) map[string]string {
	out := map[string]string{}
	for i := range n {
		out["X-H"+strings.Repeat("a", i+1)] = "v"
	}
	return out
}

func TestFilterMatch(t *testing.T) {
	text := "Your INVOICE is ready"
	inbound := dto.Message{Chat: strings.Repeat("a", 32), Kind: "text", Text: &text}
	outbound := inbound
	outbound.FromMe = true
	media := dto.Message{Chat: strings.Repeat("b", 32), Kind: "media"}
	for name, tt := range map[string]struct {
		f    Filter
		m    dto.Message
		want bool
	}{
		"empty":                      {Filter{}, media, true},
		"the chat":                   {Filter{Chats: []string{strings.Repeat("a", 32)}}, inbound, true},
		"another chat":               {Filter{Chats: []string{strings.Repeat("a", 32)}}, media, false},
		"inbound":                    {Filter{Direction: DirectionInbound}, inbound, true},
		"inbound refuses the owner":  {Filter{Direction: DirectionInbound}, outbound, false},
		"outbound":                   {Filter{Direction: DirectionOutbound}, outbound, true},
		"outbound refuses others":    {Filter{Direction: DirectionOutbound}, inbound, false},
		"a kind":                     {Filter{Kinds: []string{"media"}}, media, true},
		"another kind":               {Filter{Kinds: []string{"media"}}, inbound, false},
		"contains ignores case":      {Filter{Contains: "invoice"}, inbound, true},
		"contains misses":            {Filter{Contains: "receipt"}, inbound, false},
		"contains needs text":        {Filter{Contains: "invoice"}, media, false},
		"every field must pass":      {Filter{Chats: []string{strings.Repeat("a", 32)}, Direction: DirectionInbound, Kinds: []string{"text"}, Contains: "ready"}, inbound, true},
		"one failing field refuses":  {Filter{Chats: []string{strings.Repeat("a", 32)}, Direction: DirectionOutbound, Contains: "ready"}, inbound, false},
		"the direction any is empty": {Filter{Direction: DirectionAny}, outbound, true},
	} {
		if got := tt.f.Match(tt.m); got != tt.want {
			t.Errorf("%s: Match = %v, want %v", name, got, tt.want)
		}
	}
}

func TestFiltersAndHeadersRoundTrip(t *testing.T) {
	f := Filter{Chats: []string{strings.Repeat("a", 32)}, Direction: DirectionInbound, Kinds: []string{"text"}, Contains: "x"}
	s, err := EncodeFilter(f)
	if err != nil {
		t.Fatalf("EncodeFilter: %v", err)
	}
	back, err := DecodeFilter(s)
	if err != nil || back.Direction != f.Direction || back.Contains != f.Contains || len(back.Chats) != 1 || len(back.Kinds) != 1 {
		t.Fatalf("DecodeFilter(%s) = %+v, %v", s, back, err)
	}
	if _, err := DecodeFilter(`{"direction":"up"}`); err == nil {
		t.Fatal("DecodeFilter accepted a direction Normalize refuses")
	}
	h, err := EncodeHeaders(nil)
	if err != nil || h != "{}" {
		t.Fatalf("EncodeHeaders(nil) = %q, %v", h, err)
	}
	m, err := DecodeHeaders(`{"Authorization":"x"}`)
	if err != nil || m["Authorization"] != "x" {
		t.Fatalf("DecodeHeaders = %v, %v", m, err)
	}
	var probe map[string]any
	if json.Unmarshal([]byte(s), &probe) != nil {
		t.Fatalf("the filter %s is not a JSON object", s)
	}
}
