package policy

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/token"
)

var adminFormat = regexp.MustCompile(`^wwadm_[A-Za-z0-9_-]{43}_[0-9a-f]{8}$`)

func withChecksum(body string) string {
	return fmt.Sprintf("%s_%08x", body, crc32.ChecksumIEEE([]byte(body)))
}

func adminTokenFor(secret [adminSecretSize]byte) string {
	return withChecksum("wwadm_" + base64.RawURLEncoding.EncodeToString(secret[:]))
}

func knownAdminTokens() map[string]string {
	var sequential, allOnes [adminSecretSize]byte
	for i := range sequential {
		sequential[i] = byte(i)
		allOnes[i] = 0xff
	}
	return map[string]string{
		"sequential secret":          adminTokenFor(sequential),
		"secret full of underscores": adminTokenFor(allOnes),
		"generated token":            token.NewAdmin(),
	}
}

func mustCredential(t *testing.T, s string) AdminCredential {
	t.Helper()
	c, err := ParseAdminCredential(s)
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	return c
}

func TestDecideAdmin(t *testing.T) {
	for name, tok := range knownAdminTokens() {
		t.Run(name, func(t *testing.T) { checkDecideAdmin(t, tok) })
	}
}

func checkDecideAdmin(t *testing.T, tok string) {
	cred := mustCredential(t, token.Hash(tok))
	upperCred := mustCredential(t, strings.ToUpper(token.Hash(tok)))

	for _, c := range []AdminCredential{cred, upperCred} {
		g, ok := DecideAdmin(c, tok)
		if !ok || !g.Valid() {
			t.Fatalf("DecideAdmin() denied the configured token: ok %v, valid %v", ok, g.Valid())
		}
	}

	flipped := []byte(tok)
	for i, b := range flipped {
		if 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' {
			flipped[i] ^= 0x20
			break
		}
	}
	rejected := []struct {
		name      string
		cred      AdminCredential
		presented string
	}{
		{name: "empty token", cred: cred, presented: ""},
		{name: "zero credential", cred: AdminCredential{}, presented: tok},
		{name: "credential holding the hash without its ok flag", cred: AdminCredential{sum: sha256.Sum256([]byte(tok))}, presented: tok},
		{name: "truncated by one", cred: cred, presented: tok[:len(tok)-1]},
		{name: "truncated to the prefix", cred: cred, presented: tok[:len("wwadm_")]},
		{name: "extended", cred: cred, presented: tok + "0"},
		{name: "trailing newline", cred: cred, presented: tok + "\n"},
		{name: "leading space", cred: cred, presented: " " + tok},
		{name: "upper case", cred: cred, presented: strings.ToUpper(tok)},
		{name: "one letter's case flipped", cred: cred, presented: string(flipped)},
		{name: "another valid token", cred: cred, presented: token.NewAdmin()},
		{name: "the hash itself", cred: cred, presented: token.Hash(tok)},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if g, ok := DecideAdmin(tt.cred, tt.presented); ok || !reflect.ValueOf(g).IsZero() {
				t.Fatalf("DecideAdmin() = %+v, %v, want a denial", g, ok)
			}
		})
	}

	for i := range len(tok) {
		nearMiss := []byte(tok)
		nearMiss[i] ^= 0x01
		if _, ok := DecideAdmin(cred, string(nearMiss)); ok {
			t.Fatalf("DecideAdmin() accepted a token differing from the configured one at byte %d", i)
		}
	}

	configured := token.Hash(tok)
	for i := range len(configured) {
		for _, d := range []byte("0123456789abcdef") {
			if d == configured[i] {
				continue
			}
			nearMiss := mustCredential(t, configured[:i]+string(d)+configured[i+1:])
			if g, ok := DecideAdmin(nearMiss, tok); ok || !reflect.ValueOf(g).IsZero() {
				t.Fatalf("DecideAdmin() accepted the token against a credential differing at hex digit %d", i)
			}
		}
	}
}

func TestDecideAdminDeniesMalformedTokensEvenWhenTheirHashIsConfigured(t *testing.T) {
	tok := knownAdminTokens()["sequential secret"]
	canonical, sum := tok[len("wwadm_"):len(tok)-9], tok[len(tok)-8:]
	tests := []struct {
		name      string
		presented string
	}{
		{name: "empty", presented: ""},
		{name: "passphrase", presented: "correct horse battery staple"},
		{name: "hexadecimal string", presented: strings.Repeat("0123456789abcdef", 4)},
		{name: "upper case", presented: strings.ToUpper(tok)},
		{name: "trailing newline", presented: tok + "\n"},
		{name: "upper-case checksum", presented: "wwadm_" + canonical + "_" + strings.ToUpper(sum)},
		{name: "checksum of another body", presented: "wwadm_" + canonical + "_" + knownAdminTokens()["secret full of underscores"][len(tok)-8:]},
		{name: "client prefix with its checksum", presented: withChecksum("ww_" + canonical)},
		{name: "short secret with its checksum", presented: withChecksum("wwadm_" + canonical[:42])},
		{name: "padded secret with its checksum", presented: withChecksum("wwadm_" + canonical + "=")},
		{name: "non-canonical secret with its checksum", presented: withChecksum("wwadm_" + canonical[:42] + "9")},
		{name: "newline inside the secret with its checksum", presented: withChecksum("wwadm_" + canonical[:20] + "\n" + canonical[20:])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred := mustCredential(t, token.Hash(tt.presented))
			if g, ok := DecideAdmin(cred, tt.presented); ok || !reflect.ValueOf(g).IsZero() {
				t.Fatalf("DecideAdmin() = %+v, %v for a malformed token whose hash is configured, want a denial", g, ok)
			}
		})
	}
}

func TestParseAdminCredential(t *testing.T) {
	tok := token.NewAdmin()
	valid := token.Hash(tok)
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "63 characters", in: valid[:63]},
		{name: "65 characters", in: valid + "0"},
		{name: "128 characters", in: valid + valid},
		{name: "trailing newline", in: valid + "\n"},
		{name: "newline in place of a digit", in: valid[:63] + "\n"},
		{name: "leading space", in: " " + valid[1:]},
		{name: "0x prefix", in: "0x" + valid[2:]},
		{name: "non-hex letter", in: valid[:63] + "g"},
		{name: "colon separated", in: valid[:31] + ":" + valid[32:]},
		{name: "non-ASCII", in: valid[:62] + "é"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseAdminCredential(tt.in)
			if !errors.Is(err, ErrAdminCredential) {
				t.Fatalf("ParseAdminCredential() error = %v, want ErrAdminCredential", err)
			}
			if !reflect.ValueOf(c).IsZero() {
				t.Fatal("ParseAdminCredential() returned a non-zero credential with its error")
			}
			if tt.in != "" && strings.Contains(err.Error(), tt.in) {
				t.Fatalf("the error echoes its input: %v", err)
			}
			if _, ok := DecideAdmin(c, tok); ok {
				t.Fatal("a credential that failed to parse admitted a token")
			}
		})
	}
}

func TestWellFormedAdminTokenAcceptsKnownAndGeneratedTokens(t *testing.T) {
	for name, tok := range knownAdminTokens() {
		if !adminFormat.MatchString(tok) || !wellFormedAdminToken(tok) {
			t.Fatalf("%s %q: wellFormedAdminToken() = false", name, tok)
		}
	}
	for range 2000 {
		if tok := token.NewAdmin(); !wellFormedAdminToken(tok) {
			t.Fatalf("wellFormedAdminToken(%q) = false for a generated token", tok)
		}
	}
}

func TestWellFormedAdminTokenRejectsEverySingleCharacterChange(t *testing.T) {
	tokens := []string{token.NewAdmin()}
	for _, tok := range knownAdminTokens() {
		tokens = append(tokens, tok)
	}
	for _, tok := range tokens {
		for i := range len(tok) {
			for b := range 256 {
				if byte(b) == tok[i] {
					continue
				}
				mutated := tok[:i] + string([]byte{byte(b)}) + tok[i+1:]
				if wellFormedAdminToken(mutated) {
					t.Fatalf("wellFormedAdminToken accepted %q, which differs from %q at byte %d", mutated, tok, i)
				}
			}
		}
	}
}

func TestWellFormedAdminTokenRejects(t *testing.T) {
	tok := knownAdminTokens()["sequential secret"]
	canonical, sum := tok[len("wwadm_"):len(tok)-9], tok[len(tok)-8:]
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
		{name: "uppercase checksum", in: "wwadm_" + canonical + "_" + strings.ToUpper(sum)},
		{name: "short checksum", in: "wwadm_" + canonical + "_" + sum[:7]},
		{name: "long checksum", in: "wwadm_" + canonical + "_" + sum + "0"},
		{name: "truncated", in: tok[:len(tok)-1]},
		{name: "extended", in: tok + "0"},
		{name: "leading space", in: " " + tok},
		{name: "trailing newline", in: tok + "\n"},
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
		{name: "checksum of another body", in: "wwadm_" + canonical + "_" + knownAdminTokens()["secret full of underscores"][len(tok)-8:]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if wellFormedAdminToken(tt.in) {
				t.Fatalf("wellFormedAdminToken(%q) = true, want false", tt.in)
			}
		})
	}
}

func FuzzWellFormedAdminToken(f *testing.F) {
	known := knownAdminTokens()
	sequential := known["sequential secret"]
	for _, seed := range []string{
		"",
		"wwadm_",
		"wwadm__",
		sequential,
		known["secret full of underscores"],
		strings.ToUpper(sequential),
		sequential + "\n",
		sequential[:len(sequential)-1],
		withChecksum("wwadm_" + strings.Repeat("A", 42) + "B"),
		withChecksum("wwadm_" + strings.Repeat("A", 43) + "="),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !wellFormedAdminToken(s) {
			return
		}
		if !adminFormat.MatchString(s) {
			t.Fatalf("wellFormedAdminToken accepted %q, which does not match %s", s, adminFormat)
		}
		secret, err := base64.RawURLEncoding.Strict().DecodeString(s[len("wwadm_") : len(s)-9])
		if err != nil || len(secret) != adminSecretSize {
			t.Fatalf("wellFormedAdminToken accepted %q, whose secret does not decode to %d bytes", s, adminSecretSize)
		}
		if got := adminTokenFor([adminSecretSize]byte(secret)); got != s {
			t.Fatalf("wellFormedAdminToken accepted %q, but its canonical form is %q", s, got)
		}
	})
}
