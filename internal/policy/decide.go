package policy

import (
	"crypto/sha256"
	"crypto/subtle"
	"time"
)

func DecideRead(c *Client, now time.Time) (ReadGrant, bool) {
	if !live(c, now) {
		return ReadGrant{}, false
	}
	g := ReadGrant{client: c.ID, all: c.ReadAll, ok: true}
	if !c.ReadAll {
		g.chats = validChats(c.Read)
	}
	return g, true
}

func DecideWrite(c *Client, now time.Time) (WriteGrant, bool) {
	if !live(c, now) || c.ReadAll {
		return WriteGrant{}, false
	}
	chats := validChats(c.Write)
	if len(chats) == 0 {
		return WriteGrant{}, false
	}
	return WriteGrant{client: c.ID, chats: chats, allowFirstContact: c.AllowFirstContact, ok: true}, true
}

func DecideAdmin(cred AdminCredential, presented string) (AdminGrant, bool) {
	if !cred.ok || presented == "" {
		return AdminGrant{}, false
	}
	sum := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(sum[:], cred.sum[:]) != 1 {
		return AdminGrant{}, false
	}
	return AdminGrant{ok: true}, true
}

func live(c *Client, now time.Time) bool {
	return c != nil && !c.Revoked && !now.IsZero() && !c.ExpiresAt.IsZero() && now.Before(c.ExpiresAt)
}

func validChats(set map[CanonicalChat]struct{}) map[CanonicalChat]struct{} {
	out := make(map[CanonicalChat]struct{}, len(set))
	for chat := range set {
		if chat.ok {
			out[chat] = struct{}{}
		}
	}
	return out
}
