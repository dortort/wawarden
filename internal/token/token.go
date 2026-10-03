// Package token generates, checks and hashes admin bearer tokens.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strings"
)

const (
	adminPrefix = "wwadm_"
	secretSize  = 32
)

func NewAdmin() string {
	var secret [secretSize]byte
	rand.Read(secret[:])
	return formatAdmin(secret)
}

func ValidAdmin(s string) bool {
	i := strings.LastIndexByte(s, '_')
	if i < 0 {
		return false
	}
	body, sum := s[:i], s[i+1:]
	encoded, ok := strings.CutPrefix(body, adminPrefix)
	if !ok {
		return false
	}
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil &&
		len(secret) == secretSize &&
		base64.RawURLEncoding.EncodeToString(secret) == encoded &&
		sum == checksum(body)
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
