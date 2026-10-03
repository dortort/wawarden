package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/token"
)

const syntheticAdminToken = "wwadm_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8_c307c63e"

func mustCredential(t *testing.T, s string) AdminCredential {
	t.Helper()
	c, err := ParseAdminCredential(s)
	if err != nil {
		t.Fatalf("ParseAdminCredential: %v", err)
	}
	return c
}

func TestDecideAdmin(t *testing.T) {
	for name, tok := range map[string]string{"fixed token": syntheticAdminToken, "generated token": token.NewAdmin()} {
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
		for _, d := range "0123456789abcdef" {
			if byte(d) == configured[i] {
				continue
			}
			nearMiss := mustCredential(t, configured[:i]+string(d)+configured[i+1:])
			if g, ok := DecideAdmin(nearMiss, tok); ok || !reflect.ValueOf(g).IsZero() {
				t.Fatalf("DecideAdmin() accepted the token against a credential differing at hex digit %d", i)
			}
		}
	}
}

func TestDecideAdminDeniesTheEmptyTokenEvenWhenItsHashIsConfigured(t *testing.T) {
	empty := sha256.Sum256(nil)
	cred := mustCredential(t, hex.EncodeToString(empty[:]))
	if _, ok := DecideAdmin(cred, ""); ok {
		t.Fatal("DecideAdmin() accepted the empty token")
	}
}

func TestParseAdminCredential(t *testing.T) {
	valid := token.Hash(syntheticAdminToken)
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
			if _, ok := DecideAdmin(c, syntheticAdminToken); ok {
				t.Fatal("a credential that failed to parse admitted a token")
			}
		})
	}
}
