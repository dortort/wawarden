// Package token generates, parses and hashes admin and client bearer tokens, and compares client token digests in constant time.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strings"
)

const (
	adminPrefix = "wwadm_"
	secretSize  = 32

	clientPrefix  = "ww_"
	clientIDSize  = 5
	clientIDLen   = 8
	clientLen     = 64
	clientIDEnd   = len(clientPrefix) + clientIDLen
	clientBodyEnd = clientLen - 9
)

var clientIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func NewAdmin() string {
	var secret [secretSize]byte
	rand.Read(secret[:])
	return formatAdmin(secret)
}

func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func formatAdmin(secret [secretSize]byte) string {
	body := adminPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	return body + "_" + checksum(body)
}

func checksum(body string) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(body)))
}

func NewClient() (id, full string) {
	var raw [clientIDSize]byte
	var secret [secretSize]byte
	rand.Read(raw[:])
	rand.Read(secret[:])
	return formatClient(raw, secret)
}

func formatClient(raw [clientIDSize]byte, secret [secretSize]byte) (id, full string) {
	id = clientIDEncoding.EncodeToString(raw[:])
	body := clientPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret[:])
	return id, body + "_" + checksum(body)
}

func ParseClient(s string) (id string, ok bool) {
	if len(s) != clientLen || !strings.HasPrefix(s, clientPrefix) || s[clientIDEnd] != '_' || s[clientBodyEnd] != '_' {
		return "", false
	}
	id, encoded := s[len(clientPrefix):clientIDEnd], s[clientIDEnd+1:clientBodyEnd]
	raw, idErr := clientIDEncoding.DecodeString(id)
	secret, secretErr := base64.RawURLEncoding.DecodeString(encoded)
	if idErr != nil || secretErr != nil || len(raw) != clientIDSize || len(secret) != secretSize ||
		clientIDEncoding.EncodeToString(raw) != id || base64.RawURLEncoding.EncodeToString(secret) != encoded ||
		checksum(s[:clientBodyEnd]) != s[clientBodyEnd+1:] {
		return "", false
	}
	return id, true
}

func WellFormedClientID(id string) bool {
	raw, err := clientIDEncoding.DecodeString(id)
	return err == nil && len(raw) == clientIDSize && clientIDEncoding.EncodeToString(raw) == id
}

func StoredDigest(s string) []byte {
	digest := sha256.Sum256([]byte(s))
	return digest[:]
}

type Digest struct {
	_   [0]func()
	sum [sha256.Size]byte
}

func DigestOf(s string) Digest { return Digest{sum: sha256.Sum256([]byte(s))} }

func ParseDigest(b []byte) (Digest, bool) {
	if len(b) != sha256.Size {
		return Digest{}, false
	}
	var stored [sha256.Size]byte
	copy(stored[:], b)
	return Digest{sum: stored}, true
}

func RandomDigest() Digest {
	var random [sha256.Size]byte
	rand.Read(random[:])
	return Digest{sum: random}
}

func Match(stored, presented Digest) bool {
	return subtle.ConstantTimeCompare(stored.sum[:], presented.sum[:]) == 1
}
