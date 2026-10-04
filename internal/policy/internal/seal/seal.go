// Package seal declares the chat, grant and credential types whose values only its constructors can build.
package seal

import (
	"crypto/sha256"
	"crypto/subtle"
	"maps"
	"unique"
)

type ChatKind uint8

const (
	InvalidChat ChatKind = iota
	PhoneChat
	LIDChat
	GroupChat
)

type Chat struct {
	jid  unique.Handle[string]
	kind ChatKind
	ok   bool
}

func NewChat(jid string, kind ChatKind) Chat {
	return Chat{jid: unique.Make(jid), kind: kind, ok: true}
}

func (c Chat) Valid() bool { return c.ok }

func (c Chat) JID() string {
	if !c.ok {
		return ""
	}
	return c.jid.Value()
}

func (c Chat) Kind() ChatKind {
	if !c.ok {
		return InvalidChat
	}
	return c.kind
}

type ReadGrant struct {
	client string
	all    bool
	chats  map[Chat]struct{}
	ok     bool
}

func NewReadGrant(client string, all bool, chats map[Chat]struct{}) ReadGrant {
	return ReadGrant{client: client, all: all, chats: chats, ok: true}
}

func (g ReadGrant) Client() string { return g.client }
func (g ReadGrant) Valid() bool    { return g.ok }
func (g ReadGrant) All() bool      { return g.ok && g.all }

func (g ReadGrant) Chats() map[Chat]struct{} {
	if !g.ok {
		return nil
	}
	return maps.Clone(g.chats)
}

func (g ReadGrant) Allows(chat Chat) bool {
	if !g.ok || !chat.ok {
		return false
	}
	if g.all {
		return true
	}
	_, ok := g.chats[chat]
	return ok
}

type WriteGrant struct {
	client            string
	chats             map[Chat]struct{}
	allowFirstContact bool
	ok                bool
}

func NewWriteGrant(client string, chats map[Chat]struct{}, allowFirstContact bool) WriteGrant {
	return WriteGrant{client: client, chats: chats, allowFirstContact: allowFirstContact, ok: true}
}

func (g WriteGrant) Client() string          { return g.client }
func (g WriteGrant) Valid() bool             { return g.ok }
func (g WriteGrant) AllowFirstContact() bool { return g.ok && g.allowFirstContact }

func (g WriteGrant) Allows(chat Chat) bool {
	if !g.ok || !chat.ok {
		return false
	}
	_, ok := g.chats[chat]
	return ok
}

type AdminGrant struct {
	ok bool
}

func NewAdminGrant() AdminGrant { return AdminGrant{ok: true} }

func (g AdminGrant) Valid() bool { return g.ok }

type AdminCredential struct {
	_   [0]func() // incomparable, so == cannot stand in for crypto/subtle
	sum [sha256.Size]byte
	ok  bool
}

func NewAdminCredential(sum [sha256.Size]byte) AdminCredential {
	return AdminCredential{sum: sum, ok: true}
}

func MatchAdminCredential(c AdminCredential, sum [sha256.Size]byte) bool {
	return c.ok && subtle.ConstantTimeCompare(sum[:], c.sum[:]) == 1
}
