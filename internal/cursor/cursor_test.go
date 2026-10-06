package cursor_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/cursor"
)

const (
	keyID      = "0a1b2c3d"
	otherKeyID = "0a1b2c3e"
)

var (
	key      = bytes.Repeat([]byte{0x42}, 32)
	otherKey = bytes.Repeat([]byte{0x43}, 32)
	binding  = cursor.Binding{Client: "abcdefgh", Endpoint: "messages", Filter: "00112233445566778899aabbccddeeff"}
	position = cursor.Position{1767323045000, 4242, 7}
	ref      = cursor.Ref{Chat: "15550100001@s.whatsapp.net", ID: "3EB0SYNTHETIC0001", Sender: "15550100001@s.whatsapp.net"}
)

func sealer(t testing.TB, k []byte, id string) *cursor.Sealer {
	t.Helper()
	s, err := cursor.New(k, id)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func requireInvalid(t testing.TB, err error) {
	t.Helper()
	if err != cursor.ErrInvalid || err.Error() != "invalid cursor" {
		t.Fatalf("err = %v, want the single invalid cursor error", err)
	}
}

func TestNewRefusesMalformedKeys(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  []byte
		id   string
	}{
		{name: "short key", key: key[:16], id: keyID},
		{name: "long key", key: append(bytes.Clone(key), 0), id: keyID},
		{name: "short id", key: key, id: "0a1b2c"},
		{name: "non-hex id", key: key, id: "0a1b2c3g"},
		{name: "empty id", key: key, id: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if s, err := cursor.New(tt.key, tt.id); err == nil || s != nil {
				t.Fatalf("New = %v, %v, want a refusal", s, err)
			}
		})
	}
}

func TestCursorRoundTripsAndStaysShort(t *testing.T) {
	s := sealer(t, key, keyID)
	for _, p := range []cursor.Position{{}, position, {-1, -9223372036854775808, 9223372036854775807}} {
		text, err := s.Cursor(binding, p)
		if err != nil {
			t.Fatalf("Cursor: %v", err)
		}
		if len(text) > cursor.MaxCursor || strings.ContainsAny(text, ":+/=") {
			t.Fatalf("cursor %q is not a short base64url text", text)
		}
		got, err := s.OpenCursor(binding, text)
		if err != nil || got != p {
			t.Fatalf("OpenCursor = %v, %v, want %v", got, err, p)
		}
		again, err := s.Cursor(binding, p)
		if err != nil || again == text || len(again) != len(text) {
			t.Fatalf("a second seal = %q, %v: want a different text of the same length", again, err)
		}
	}
}

func TestCursorOpensOnlyUnderItsBinding(t *testing.T) {
	s := sealer(t, key, keyID)
	search := cursor.Binding{Client: binding.Client, Endpoint: "search", Filter: "*", Query: `"alpha" AND "beta"`}
	for _, b := range []cursor.Binding{binding, search} {
		text, err := s.Cursor(b, position)
		if err != nil {
			t.Fatalf("Cursor: %v", err)
		}
		for name, other := range map[string]cursor.Binding{
			"client":       {Client: "abcdefgi", Endpoint: b.Endpoint, Filter: b.Filter, Query: b.Query},
			"endpoint":     {Client: b.Client, Endpoint: "changes", Filter: b.Filter, Query: b.Query},
			"filter":       {Client: b.Client, Endpoint: b.Endpoint, Filter: "ffeeddccbbaa99887766554433221100", Query: b.Query},
			"no filter":    {Client: b.Client, Endpoint: b.Endpoint, Query: b.Query},
			"query":        {Client: b.Client, Endpoint: b.Endpoint, Filter: b.Filter, Query: `"alpha"`},
			"field shifts": {Client: b.Client + b.Endpoint, Filter: b.Filter, Query: b.Query},
		} {
			t.Run(b.Endpoint+"/"+name, func(t *testing.T) {
				_, err := s.OpenCursor(other, text)
				requireInvalid(t, err)
			})
		}
	}
}

func TestCursorRefusesEveryAlteration(t *testing.T) {
	s := sealer(t, key, keyID)
	var text string
	for !strings.ContainsAny(text, "-_") {
		var err error
		if text, err = s.Cursor(binding, position); err != nil {
			t.Fatalf("Cursor: %v", err)
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range raw {
		for _, bit := range []byte{0x01, 0x80} {
			altered := bytes.Clone(raw)
			altered[i] ^= bit
			_, err := s.OpenCursor(binding, base64.RawURLEncoding.EncodeToString(altered))
			requireInvalid(t, err)
		}
	}
	for name, input := range map[string]string{
		"empty":                "",
		"truncated":            text[:len(text)-1],
		"truncated by a block": base64.RawURLEncoding.EncodeToString(raw[:len(raw)-1]),
		"extended":             base64.RawURLEncoding.EncodeToString(append(bytes.Clone(raw), 0)),
		"trailing newline":     text + "\n",
		"embedded CR":          text[:10] + "\r" + text[10:],
		"embedded CRLF":        text[:20] + "\r\n" + text[20:],
		"padded":               text + "==",
		"standard alphabet":    base64.RawStdEncoding.EncodeToString(raw),
		"header only":          base64.RawURLEncoding.EncodeToString(raw[:5]),
		"over the cap":         text + strings.Repeat("A", cursor.MaxCursor),
		"reference prefix":     cursor.RefPrefix + text,
		"other version":        base64.RawURLEncoding.EncodeToString(append([]byte{0x02}, raw[1:]...)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.OpenCursor(binding, input)
			requireInvalid(t, err)
		})
	}
}

func TestCursorRefusesOtherKeys(t *testing.T) {
	text, err := sealer(t, key, keyID).Cursor(binding, position)
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	for name, s := range map[string]*cursor.Sealer{
		"rotated key, same id": sealer(t, otherKey, keyID),
		"same key, other id":   sealer(t, key, otherKeyID),
		"no key":               nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.OpenCursor(binding, text)
			requireInvalid(t, err)
		})
	}
	var none *cursor.Sealer
	if _, err := none.Cursor(binding, position); err == nil {
		t.Fatal("a missing sealer sealed a cursor")
	}
	if _, err := none.SealRef(binding.Client, ref); err == nil {
		t.Fatal("a missing sealer sealed a reference")
	}
}

func TestRefRoundTripsUnderItsClientOnly(t *testing.T) {
	s := sealer(t, key, keyID)
	text, err := s.SealRef(binding.Client, ref)
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	if !strings.HasPrefix(text, cursor.RefPrefix) || len(text) > cursor.MaxRef {
		t.Fatalf("reference %q lacks the prefix or exceeds the cap", text)
	}
	got, err := s.OpenRef(binding.Client, text)
	if err != nil || got != ref {
		t.Fatalf("OpenRef = %+v, %v, want %+v", got, err, ref)
	}
	_, err = s.OpenRef("abcdefgi", text)
	requireInvalid(t, err)
	_, err = s.OpenCursor(binding, strings.TrimPrefix(text, cursor.RefPrefix))
	requireInvalid(t, err)
	for name, input := range map[string]string{
		"no prefix":        strings.TrimPrefix(text, cursor.RefPrefix),
		"other prefix":     "m2_" + strings.TrimPrefix(text, cursor.RefPrefix),
		"trailing newline": text + "\n",
		"over the cap":     text + strings.Repeat("A", cursor.MaxRef),
		"truncated":        text[:len(text)-2],
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.OpenRef(binding.Client, input)
			requireInvalid(t, err)
		})
	}
}

func TestRefFieldsCannotShift(t *testing.T) {
	s := sealer(t, key, keyID)
	shifted := cursor.Ref{Chat: ref.Chat + ref.ID, ID: "", Sender: ref.Sender}
	text, err := s.SealRef(binding.Client, shifted)
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	got, err := s.OpenRef(binding.Client, text)
	if err != nil || got != shifted {
		t.Fatalf("OpenRef = %+v, %v, want %+v", got, err, shifted)
	}
}

func TestTheLongestReferenceFitsItsCap(t *testing.T) {
	s := sealer(t, key, keyID)
	longest := cursor.Ref{Chat: "120363999999999999999999@g.us", ID: strings.Repeat("~", 128), Sender: "999999999999999999999999@lid"}
	text, err := s.SealRef(binding.Client, longest)
	if err != nil || len(text) > cursor.MaxRef {
		t.Fatalf("SealRef = %d characters, %v, want at most %d", len(text), err, cursor.MaxRef)
	}
	if _, err := s.SealRef(binding.Client, cursor.Ref{ID: strings.Repeat("x", cursor.MaxRef)}); err == nil || errors.Is(err, cursor.ErrInvalid) {
		t.Fatalf("an oversized reference sealed or failed as a cursor error: %v", err)
	}
}
