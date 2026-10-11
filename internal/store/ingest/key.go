package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"log/slog"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	minKey          = 8
	maxKey          = 128
	redactedKey     = "[idempotency key]"
	fingerprintTag  = "wawarden/idempotency/v1\x00"
	fingerprintSize = sha256.Size
)

type Key struct{ s string }

func ParseKey(s string) (Key, bool) {
	if len(s) < minKey || len(s) > maxKey {
		return Key{}, false
	}
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return Key{}, false
		}
	}
	return Key{s: s}, true
}

func (k Key) Valid() bool { return k.s != "" }

func (k Key) Value() string { return k.s }

func (Key) String() string { return redactedKey }

func (Key) GoString() string { return redactedKey }

func (Key) LogValue() slog.Value { return slog.StringValue(redactedKey) }

type ReplyTarget struct {
	ID     string
	Sender policy.CanonicalChat
}

func Fingerprint(chatRef, text string, reply *ReplyTarget) [fingerprintSize]byte {
	b := []byte(fingerprintTag)
	b = lengthPrefixed(b, chatRef)
	b = lengthPrefixed(b, text)
	if reply != nil {
		b = append(b, 1)
		b = lengthPrefixed(b, reply.ID)
		b = lengthPrefixed(b, reply.Sender.JID())
	} else {
		b = append(b, 0)
	}
	return sha256.Sum256(b)
}

func lengthPrefixed(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}
