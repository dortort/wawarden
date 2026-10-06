package token

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"regexp"
	"strings"
	"testing"
)

var adminFormat = regexp.MustCompile(`^wwadm_[A-Za-z0-9_-]{43}_[0-9a-f]{8}$`)

const (
	sequentialToken = "wwadm_" + "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" + "_c307c63e"
	allOnesToken    = "wwadm_" + "__________________________________________8" + "_384c24f2"
)

func sequentialSecret() [secretSize]byte {
	var s [secretSize]byte
	for i := range s {
		s[i] = byte(i)
	}
	return s
}

func allOnesSecret() [secretSize]byte {
	var s [secretSize]byte
	for i := range s {
		s[i] = 0xff
	}
	return s
}

func TestFormatAdminVectors(t *testing.T) {
	tests := []struct {
		name   string
		secret [secretSize]byte
		want   string
	}{
		{name: "sequential bytes", secret: sequentialSecret(), want: sequentialToken},
		{name: "secret full of underscores", secret: allOnesSecret(), want: allOnesToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatAdmin(tt.secret); got != tt.want {
				t.Fatalf("formatAdmin() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewAdmin(t *testing.T) {
	const n = 2000
	seen := make(map[string]bool, n)
	var ones, zeros [secretSize]byte
	for range n {
		tok := NewAdmin()
		if !adminFormat.MatchString(tok) {
			t.Fatalf("NewAdmin() = %q does not match %s", tok, adminFormat)
		}
		body, sum := tok[:len(tok)-9], tok[len(tok)-8:]
		if want := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(body))); sum != want {
			t.Fatalf("checksum of %q = %s, want %s", tok, sum, want)
		}
		secret, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(body, "wwadm_"))
		if err != nil || len(secret) != secretSize {
			t.Fatalf("secret of %q decodes to %d bytes, err %v", tok, len(secret), err)
		}
		for i, b := range secret {
			ones[i] |= b
			zeros[i] |= ^b
		}
		if seen[tok] {
			t.Fatalf("NewAdmin() repeated %q after %d tokens", tok, len(seen))
		}
		seen[tok] = true
	}
	for i := range secretSize {
		if ones[i] != 0xff || zeros[i] != 0xff {
			t.Errorf("secret byte %d never varied in bits %08b across %d tokens", i, ^(ones[i] & zeros[i]), n)
		}
	}
}

func TestHash(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: sequentialToken, want: "5746c30ac416f8bc529a46efc5ec54f4aec2a4f49bbaf806b39834c123138439"},
		{in: allOnesToken, want: "f5a7ce901d4e2d0ce05435cb56cf0790c6cac9535165417c33f48fa605bde7c3"},
		{in: "", want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, tt := range tests {
		if got := Hash(tt.in); got != tt.want {
			t.Fatalf("Hash(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
	for range 100 {
		tok := NewAdmin()
		sum := sha256.Sum256([]byte(tok))
		if got, want := Hash(tok), hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("Hash(%q) = %s, want %s", tok, got, want)
		}
	}
}

var clientFormat = regexp.MustCompile(`^ww_[a-z2-7]{8}_[A-Za-z0-9_-]{43}_[0-9a-f]{8}$`)

const (
	sequentialClient = "ww_aaaqeaye_" + "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" + "_46e2849b"
	allOnesClient    = "ww_77777777_" + "__________________________________________8" + "_2ab30c75"
)

func TestFormatClientVectors(t *testing.T) {
	var sequentialID, allOnesID [clientIDSize]byte
	for i := range clientIDSize {
		sequentialID[i], allOnesID[i] = byte(i), 0xff
	}
	tests := []struct {
		name, wantID, want, wantHash string
		raw                          [clientIDSize]byte
		secret                       [secretSize]byte
	}{
		{name: "sequential bytes", raw: sequentialID, secret: sequentialSecret(), wantID: "aaaqeaye", want: sequentialClient,
			wantHash: "6136b8bcf74be53dd49b60cdcbc552d9f76b9eb6aa04c6d59ae9c42edb3717c4"},
		{name: "secret full of underscores", raw: allOnesID, secret: allOnesSecret(), wantID: "77777777", want: allOnesClient,
			wantHash: "aa12e7bedec958b52ceb1c8d41b1fcd869c4280996fdc603fd6aba80f980f272"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, full := formatClient(tt.raw, tt.secret)
			if id != tt.wantID || full != tt.want {
				t.Fatalf("formatClient() = %q, %q, want %q, %q", id, full, tt.wantID, tt.want)
			}
			if got := hex.EncodeToString(StoredDigest(full)); got != tt.wantHash {
				t.Fatalf("StoredDigest() = %s, want %s", got, tt.wantHash)
			}
			if got, ok := ParseClient(full); !ok || got != tt.wantID {
				t.Fatalf("ParseClient(%q) = %q, %v, want %q, true", full, got, ok, tt.wantID)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	const n = 2000
	seen := make(map[string]bool, n)
	ids := make(map[string]bool, n)
	for range n {
		id, tok := NewClient()
		if !clientFormat.MatchString(tok) || len(tok) != clientLen || tok[3:11] != id {
			t.Fatalf("NewClient() = %q, %q, which does not match %s with the id at offset 3", id, tok, clientFormat)
		}
		if got, ok := ParseClient(tok); !ok || got != id {
			t.Fatalf("ParseClient(%q) = %q, %v, want %q, true", tok, got, ok, id)
		}
		if !WellFormedClientID(id) {
			t.Fatalf("WellFormedClientID(%q) = false", id)
		}
		if seen[tok] || ids[id] {
			t.Fatalf("NewClient() repeated %q after %d tokens", tok, len(seen))
		}
		seen[tok], ids[id] = true, true
	}
}

func TestParseClientRefusesNonCanonicalTokens(t *testing.T) {
	recrc := func(body string) string { return body + "_" + checksum(body) }
	body := sequentialClient[:clientBodyEnd]
	tests := map[string]string{
		"empty":                       "",
		"admin token":                 sequentialToken,
		"one character short":         sequentialClient[:clientLen-1],
		"one character long":          sequentialClient + "0",
		"wrong checksum":              body + "_46e2849c",
		"upper-case checksum":         body + "_46E2849B",
		"upper-case id":               recrc("ww_AAAQEAYE" + body[11:]),
		"id outside the alphabet":     recrc("ww_aaaqeay1" + body[11:]),
		"padded id":                   recrc("ww_aaaqea==" + body[11:]),
		"secret with padding":         recrc(body[:54] + "="),
		"secret with standard base64": recrc(body[:54] + "+"),
		"non-canonical trailing bits": recrc(body[:54] + "9"),
		"newline in the secret":       recrc(body[:30] + "\n" + body[31:]),
		"newline in the id":           recrc("ww_aaaq\neye" + body[11:]),
		"wrong prefix":                recrc("wx" + body[2:]),
		"separator moved":             recrc("ww_aaaqeay_e" + body[12:]),
		"space in the token":          recrc(body[:20] + " " + body[21:]),
		"trailing newline":            sequentialClient[:clientLen-1] + "\n",
	}
	for name, tok := range tests {
		t.Run(name, func(t *testing.T) {
			if id, ok := ParseClient(tok); ok || id != "" {
				t.Fatalf("ParseClient(%q) = %q, %v, want a refusal", tok, id, ok)
			}
		})
	}
	for _, id := range []string{"", "aaaqeay", "aaaqeayee", "AAAQEAYE", "aaaqeay1", "aaaq\neye"} {
		if WellFormedClientID(id) {
			t.Fatalf("WellFormedClientID(%q) = true", id)
		}
	}
}

func TestDigestMatch(t *testing.T) {
	stored, ok := ParseDigest(StoredDigest(sequentialClient))
	if !ok {
		t.Fatal("ParseDigest refused a SHA-256")
	}
	if !Match(stored, DigestOf(sequentialClient)) {
		t.Fatal("the stored digest does not match the token it was made from")
	}
	if Match(stored, DigestOf(allOnesClient)) || Match(stored, RandomDigest()) || Match(RandomDigest(), RandomDigest()) {
		t.Fatal("a digest matched another token or a random digest")
	}
	for _, n := range []int{0, sha256.Size - 1, sha256.Size + 1} {
		if _, ok := ParseDigest(make([]byte, n)); ok {
			t.Fatalf("ParseDigest accepted %d bytes", n)
		}
	}
}

func FuzzParseClient(f *testing.F) {
	f.Add(sequentialClient)
	f.Add(allOnesClient)
	f.Add(sequentialToken)
	f.Add(sequentialClient[:clientBodyEnd] + "_00000000")
	f.Fuzz(func(t *testing.T, s string) {
		id, ok := ParseClient(s)
		if !ok {
			if id != "" {
				t.Fatalf("ParseClient(%q) refused but returned id %q", s, id)
			}
			return
		}
		raw, err := clientIDEncoding.DecodeString(id)
		secret, serr := base64.RawURLEncoding.DecodeString(s[clientIDEnd+1 : clientBodyEnd])
		if err != nil || serr != nil || len(raw) != clientIDSize || len(secret) != secretSize {
			t.Fatalf("ParseClient(%q) accepted a token whose parts do not decode", s)
		}
		gotID, full := formatClient([clientIDSize]byte(raw), [secretSize]byte(secret))
		if gotID != id || full != s || !clientFormat.MatchString(s) || !WellFormedClientID(id) {
			t.Fatalf("ParseClient(%q) accepted a token that does not re-encode to itself: %q", s, full)
		}
	})
}
