// Package cursor seals pagination positions and message references to one client and one request shape, so a caller can hand them back but never read, forge or reuse them elsewhere.
package cursor

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	MaxCursor = 256
	MaxRef    = 640
	RefPrefix = "m1_"

	version      = 0x01
	idSize       = 4
	headerSize   = 1 + idSize
	keySize      = 32
	positionSize = 3 * 8
	nonceSize    = 12
	refFields    = 3
	cursorLabel  = "cursor"
	refLabel     = "mref"
	nonceLabel   = "wawarden/mref-nonce/v1"
)

var (
	ErrInvalid = errors.New("invalid cursor")
	errKey     = errors.New("cursor: the key is not 32 bytes with an 8-hex-digit key id")
	errRefSize = errors.New("cursor: the message reference would exceed its length cap")
)

var encoding = base64.RawURLEncoding.Strict()

type Sealer struct {
	aead     cipher.AEAD
	fixed    cipher.AEAD
	nonceKey []byte
	id       [idSize]byte
}

type Binding struct {
	Client   string
	Endpoint string
	Filter   string
	Query    string
}

type Position [3]int64

type Ref struct {
	Chat   string
	ID     string
	Sender string
}

func New(key []byte, keyID string) (*Sealer, error) {
	id, err := hex.DecodeString(keyID)
	if err != nil || len(id) != idSize || len(key) != keySize {
		return nil, errKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	fixed, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	derive := hmac.New(sha256.New, key)
	derive.Write([]byte(nonceLabel))
	s := &Sealer{aead: aead, fixed: fixed, nonceKey: derive.Sum(nil)}
	copy(s.id[:], id)
	return s, nil
}

func (s *Sealer) Cursor(b Binding, p Position) (string, error) {
	pt, err := binary.Append(nil, binary.BigEndian, p)
	if err != nil {
		return "", err
	}
	return s.seal(pt, b.fields())
}

func (s *Sealer) OpenCursor(b Binding, text string) (Position, error) {
	pt, err := s.open(text, MaxCursor, b.fields())
	if err != nil || len(pt) != positionSize {
		return Position{}, ErrInvalid
	}
	var p Position
	if _, err := binary.Decode(pt, binary.BigEndian, &p); err != nil {
		return Position{}, ErrInvalid
	}
	return p, nil
}

func (s *Sealer) SealRef(client string, r Ref) (string, error) {
	if s == nil {
		return "", ErrInvalid
	}
	var pt []byte
	for _, f := range [refFields]string{r.Chat, r.ID, r.Sender} {
		pt = binary.AppendUvarint(pt, uint64(len(f)))
		pt = append(pt, f...)
	}
	mac := hmac.New(sha256.New, s.nonceKey)
	mac.Write(additional(nil, []string{client}))
	mac.Write(pt)
	nonce := mac.Sum(nil)[:nonceSize]
	header := s.header()
	text := encoding.EncodeToString(s.fixed.Seal(append(header, nonce...), nonce, pt, additional(header, []string{client, refLabel})))
	if len(RefPrefix)+len(text) > MaxRef {
		return "", errRefSize
	}
	return RefPrefix + text, nil
}

func (s *Sealer) OpenRef(client, text string) (Ref, error) {
	body, ok := strings.CutPrefix(text, RefPrefix)
	if !ok || len(text) > MaxRef {
		return Ref{}, ErrInvalid
	}
	pt, err := s.open(body, MaxRef, []string{client, refLabel})
	if err != nil {
		return Ref{}, ErrInvalid
	}
	var f [refFields]string
	for i := range f {
		n, used := binary.Uvarint(pt)
		if used <= 0 {
			return Ref{}, ErrInvalid
		}
		if pt = pt[used:]; n > uint64(len(pt)) {
			return Ref{}, ErrInvalid
		}
		f[i], pt = string(pt[:n]), pt[n:]
	}
	if len(pt) != 0 {
		return Ref{}, ErrInvalid
	}
	return Ref{Chat: f[0], ID: f[1], Sender: f[2]}, nil
}

func (b Binding) fields() []string {
	query := ""
	if b.Query != "" {
		sum := sha256.Sum256([]byte(b.Query))
		query = string(sum[:])
	}
	return []string{cursorLabel, b.Client, b.Endpoint, b.Filter, query}
}

func (s *Sealer) seal(pt []byte, fields []string) (string, error) {
	if s == nil {
		return "", ErrInvalid
	}
	header := s.header()
	ad := additional(header, fields)
	return encoding.EncodeToString(s.aead.Seal(header, nil, pt, ad)), nil //nolint:gosec // G407: NewGCMWithRandomNonce draws a fresh random nonce and requires a nil one
}

func (s *Sealer) header() []byte { return []byte{version, s.id[0], s.id[1], s.id[2], s.id[3]} }

func (s *Sealer) open(text string, limit int, fields []string) ([]byte, error) {
	if s == nil || len(text) > limit {
		return nil, ErrInvalid
	}
	raw, err := encoding.DecodeString(text)
	// The decoder skips CR and LF anywhere, so only a canonical re-encoding is accepted.
	if err != nil || encoding.EncodeToString(raw) != text || len(raw) < headerSize+s.aead.Overhead() {
		return nil, ErrInvalid
	}
	if raw[0] != version || !bytes.Equal(raw[1:headerSize], s.id[:]) {
		return nil, ErrInvalid
	}
	pt, err := s.aead.Open(nil, nil, raw[headerSize:], additional(raw[:headerSize], fields))
	if err != nil {
		return nil, ErrInvalid
	}
	return pt, nil
}

func additional(header []byte, fields []string) []byte {
	ad := append([]byte(nil), header...)
	for _, f := range fields {
		ad = binary.AppendUvarint(ad, uint64(len(f)))
		ad = append(ad, f...)
	}
	return ad
}
