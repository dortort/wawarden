// Package policy decides what an authenticated caller may do and mints the grants that prove it.
package policy

import (
	"time"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

type CanonicalChat = seal.Chat

type Client struct {
	ID                string
	Name              string
	ReadAll           bool
	Read              map[CanonicalChat]struct{}
	Write             map[CanonicalChat]struct{}
	ExpiresAt         time.Time
	Revoked           bool
	AllowFirstContact bool
}
