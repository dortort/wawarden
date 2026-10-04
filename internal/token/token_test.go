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
	sequentialToken = "wwadm_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8_c307c63e" //nolint:gosec // G101: known-answer vector encoding bytes 0..31, not a credential
	allOnesToken    = "wwadm___________________________________________8_384c24f2"
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

func withChecksum(body string) string {
	return fmt.Sprintf("%s_%08x", body, crc32.ChecksumIEEE([]byte(body)))
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
			if !ValidAdmin(tt.want) {
				t.Fatalf("ValidAdmin(%q) = false, want true", tt.want)
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
		if !ValidAdmin(tok) {
			t.Fatalf("ValidAdmin(%q) = false for a generated token", tok)
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

func TestValidAdminRejectsEverySingleCharacterChange(t *testing.T) {
	tokens := []string{sequentialToken, allOnesToken, NewAdmin(), NewAdmin()}
	for _, tok := range tokens {
		for i := range len(tok) {
			for b := range 256 {
				if byte(b) == tok[i] {
					continue
				}
				mutated := tok[:i] + string([]byte{byte(b)}) + tok[i+1:]
				if ValidAdmin(mutated) {
					t.Fatalf("ValidAdmin accepted %q, which differs from %q at byte %d", mutated, tok, i)
				}
			}
		}
	}
}

func TestValidAdminRejects(t *testing.T) {
	canonical := strings.TrimSuffix(strings.TrimPrefix(sequentialToken, "wwadm_"), "_c307c63e")
	nonCanonical := canonical[:len(canonical)-1] + "9"
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "prefix only", in: "wwadm_"},
		{name: "no separator", in: "wwadm"},
		{name: "missing checksum", in: "wwadm_" + canonical},
		{name: "empty checksum", in: "wwadm_" + canonical + "_"},
		{name: "uppercase checksum", in: "wwadm_" + canonical + "_C307C63E"},
		{name: "short checksum", in: "wwadm_" + canonical + "_c307c63"},
		{name: "long checksum", in: "wwadm_" + canonical + "_c307c63e0"},
		{name: "truncated", in: sequentialToken[:len(sequentialToken)-1]},
		{name: "extended", in: sequentialToken + "0"},
		{name: "leading space", in: " " + sequentialToken},
		{name: "trailing newline", in: sequentialToken + "\n"},
		{name: "uppercase prefix", in: withChecksum("WWADM_" + canonical)},
		{name: "client prefix", in: withChecksum("ww_" + canonical)},
		{name: "no prefix", in: withChecksum(canonical)},
		{name: "doubled separator", in: withChecksum("wwadm__" + canonical)},
		{name: "short secret", in: withChecksum("wwadm_" + canonical[:42])},
		{name: "long secret", in: withChecksum("wwadm_" + canonical + "A")},
		{name: "padded secret", in: withChecksum("wwadm_" + canonical + "=")},
		{name: "standard alphabet", in: withChecksum("wwadm_" + strings.Repeat("+", 42) + "w")},
		{name: "non-zero trailing bits", in: withChecksum("wwadm_" + nonCanonical)},
		{name: "newline inside secret", in: withChecksum("wwadm_" + canonical[:20] + "\n" + canonical[20:])},
		{name: "carriage return inside secret", in: withChecksum("wwadm_" + canonical[:20] + "\r" + canonical[20:])},
		{name: "checksum of another body", in: "wwadm_" + canonical + "_384c24f2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ValidAdmin(tt.in) {
				t.Fatalf("ValidAdmin(%q) = true, want false", tt.in)
			}
		})
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

func FuzzValidAdmin(f *testing.F) {
	for _, seed := range []string{
		"",
		"wwadm_",
		"wwadm__",
		sequentialToken,
		allOnesToken,
		strings.ToUpper(sequentialToken),
		sequentialToken + "\n",
		sequentialToken[:len(sequentialToken)-1],
		withChecksum("wwadm_" + strings.Repeat("A", 42) + "B"),
		withChecksum("wwadm_" + strings.Repeat("A", 43) + "="),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !ValidAdmin(s) {
			return
		}
		if !adminFormat.MatchString(s) {
			t.Fatalf("ValidAdmin accepted %q, which does not match %s", s, adminFormat)
		}
		secret, err := base64.RawURLEncoding.Strict().DecodeString(s[len("wwadm_") : len(s)-9])
		if err != nil || len(secret) != secretSize {
			t.Fatalf("ValidAdmin accepted %q, whose secret does not decode to %d bytes", s, secretSize)
		}
		if got := formatAdmin([secretSize]byte(secret)); got != s {
			t.Fatalf("ValidAdmin accepted %q, but its canonical form is %q", s, got)
		}
	})
}
