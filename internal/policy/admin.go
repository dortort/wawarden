package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var ErrAdminCredential = errors.New("policy: the admin credential must be a SHA-256 written as 64 hexadecimal characters")

type AdminCredential struct {
	sum [sha256.Size]byte
	ok  bool
}

func ParseAdminCredential(s string) (AdminCredential, error) {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return AdminCredential{}, ErrAdminCredential
	}
	var c AdminCredential
	if _, err := hex.Decode(c.sum[:], []byte(s)); err != nil {
		return AdminCredential{}, ErrAdminCredential
	}
	c.ok = true
	return c, nil
}
