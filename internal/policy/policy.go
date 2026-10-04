// Package policy decides what an authenticated caller may do and mints the grants that prove it.
package policy

import (
	"time"
	"unique"
)

type CanonicalChat struct {
	jid unique.Handle[string]
	ok  bool
}

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
