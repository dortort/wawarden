package cursor_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

func refSealer(t testing.TB, k []byte, id string) *cursor.Sealer {
	t.Helper()
	s, err := cursor.NewRef(k, id)
	if err != nil {
		t.Fatalf("NewRef: %v", err)
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
			if s, err := cursor.NewRef(tt.key, tt.id); err == nil || s != nil {
				t.Fatalf("NewRef = %v, %v, want a refusal", s, err)
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

func TestOnlyTheReferenceSealerSealsReferences(t *testing.T) {
	if text, err := sealer(t, key, keyID).SealRef(binding.Client, ref); err == nil || text != "" || errors.Is(err, cursor.ErrInvalid) {
		t.Fatalf("a sealer from New sealed a reference: %q, %v, want no text and an error apart from the invalid cursor one", text, err)
	}
	text := mustSealRef(t, refSealer(t, key, keyID), binding.Client, ref)
	if got, err := sealer(t, key, keyID).OpenRef(binding.Client, text); err != nil || got != ref {
		t.Fatalf("a sealer from New under the same key opened the reference to %+v, %v, want %+v", got, err, ref)
	}
}

func TestRefRoundTripsUnderItsClientOnly(t *testing.T) {
	s := refSealer(t, key, keyID)
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

func TestARefIsStablePerClientAndMessage(t *testing.T) {
	s := refSealer(t, key, keyID)
	first := mustSealRef(t, s, binding.Client, ref)
	if again := mustSealRef(t, s, binding.Client, ref); again != first {
		t.Fatalf("the same message under the same client sealed to %q and %q, want one stable reference", first, again)
	}
	if again := mustSealRef(t, refSealer(t, key, keyID), binding.Client, ref); again != first {
		t.Fatalf("a second sealer with the same key sealed the message to %q, want %q", again, first)
	}
	seen := map[string]string{first: "the message"}
	nonces := map[string]string{nonceOf(t, first): "the message"}
	for _, tt := range []struct {
		name   string
		s      *cursor.Sealer
		client string
		r      cursor.Ref
	}{
		{name: "another client", s: s, client: "abcdefgi", r: ref},
		{name: "a client that extends the id", s: s, client: binding.Client + "x", r: ref},
		{name: "another chat", s: s, client: binding.Client, r: cursor.Ref{Chat: "15550100002@s.whatsapp.net", ID: ref.ID, Sender: ref.Sender}},
		{name: "another message", s: s, client: binding.Client, r: cursor.Ref{Chat: ref.Chat, ID: "3EB0SYNTHETIC0002", Sender: ref.Sender}},
		{name: "another sender", s: s, client: binding.Client, r: cursor.Ref{Chat: ref.Chat, ID: ref.ID, Sender: "15550100002@s.whatsapp.net"}},
		{name: "another key", s: refSealer(t, otherKey, keyID), client: binding.Client, r: ref},
	} {
		text := mustSealRef(t, tt.s, tt.client, tt.r)
		if other, dup := seen[text]; dup {
			t.Errorf("%s sealed to the same reference as %s", tt.name, other)
		}
		seen[text] = tt.name
		if tt.s != s {
			continue
		}
		if other, dup := nonces[nonceOf(t, text)]; dup {
			t.Errorf("%s drew the same nonce as %s", tt.name, other)
		}
		nonces[nonceOf(t, text)] = tt.name
	}
}

func mustSealRef(t *testing.T, s *cursor.Sealer, client string, r cursor.Ref) string {
	t.Helper()
	text, err := s.SealRef(client, r)
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	return text
}

func nonceOf(t *testing.T, text string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, cursor.RefPrefix))
	if err != nil || len(raw) < 5+12 {
		t.Fatalf("reference %q does not decode to a header and a nonce: %v", text, err)
	}
	return string(raw[5 : 5+12])
}

func hmacSHA256(k []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, k)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func TestARefNonceIsKeyedApartFromTheSealingKey(t *testing.T) {
	nonce := nonceOf(t, mustSealRef(t, refSealer(t, key, keyID), binding.Client, ref))
	var input []byte
	for _, f := range []string{binding.Client, ref.Chat, ref.ID, ref.Sender} {
		input = binary.AppendUvarint(input, uint64(len(f)))
		input = append(input, f...)
	}
	if raw := string(hmacSHA256(key, input)[:12]); nonce == raw {
		t.Fatalf("the reference nonce %x is an HMAC-SHA256 under the sealing key itself", nonce)
	}
	if derived := string(hmacSHA256(hmacSHA256(key, []byte("wawarden/mref-nonce/v1")), input)[:12]); nonce != derived {
		t.Fatalf("the reference nonce is %x, want %x under the nonce key derived from the sealing key", nonce, derived)
	}
}

func TestARefMatchesItsFixedVector(t *testing.T) {
	const want = "m1_AQobLD01M8YE1B595tpcy9fRF3UouehCjm0ehclfRHvRxOfGcONMcvXsQCjaoD0hCto1Vg_jJSgk-0XHd81AC03tzzCpVN6swCU6MK6lyzbLGboYpB1St0eHGHUxGU26tcFYp5yS9Yh1"
	if got := mustSealRef(t, refSealer(t, key, keyID), binding.Client, ref); got != want {
		t.Fatalf("SealRef = %q, want the fixed vector %q", got, want)
	}
}

func TestRefFieldsCannotShift(t *testing.T) {
	s := refSealer(t, key, keyID)
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
	s := refSealer(t, key, keyID)
	longest := cursor.Ref{Chat: "120363999999999999999999@g.us", ID: strings.Repeat("~", 128), Sender: "999999999999999999999999@lid"}
	text, err := s.SealRef(binding.Client, longest)
	if err != nil || len(text) > cursor.MaxRef {
		t.Fatalf("SealRef = %d characters, %v, want at most %d", len(text), err, cursor.MaxRef)
	}
	if _, err := s.SealRef(binding.Client, cursor.Ref{ID: strings.Repeat("x", cursor.MaxRef)}); err == nil || errors.Is(err, cursor.ErrInvalid) {
		t.Fatalf("an oversized reference sealed or failed as a cursor error: %v", err)
	}
}
