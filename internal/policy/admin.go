package policy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"strings"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

const (
	adminTokenPrefix = "wwadm_"
	adminSecretSize  = 32
)

var ErrAdminCredential = errors.New("policy: the admin credential must be a SHA-256 written as 64 hexadecimal characters")

type AdminCredential = seal.AdminCredential

func ParseAdminCredential(s string) (AdminCredential, error) {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return AdminCredential{}, ErrAdminCredential
	}
	var sum [sha256.Size]byte
	if _, err := hex.Decode(sum[:], []byte(s)); err != nil {
		return AdminCredential{}, ErrAdminCredential
	}
	return seal.NewAdminCredential(sum), nil
}

func WellFormedAdminToken(s string) bool {
	i := strings.LastIndexByte(s, '_')
	if i < 0 {
		return false
	}
	body, sum := s[:i], s[i+1:]
	encoded, ok := strings.CutPrefix(body, adminTokenPrefix)
	if !ok {
		return false
	}
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil &&
		len(secret) == adminSecretSize &&
		base64.RawURLEncoding.EncodeToString(secret) == encoded &&
		sum == hex.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE([]byte(body))))
}
