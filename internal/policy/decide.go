package policy

import (
	"crypto/sha256"
	"time"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

func DecideRead(c *Client, now time.Time) (ReadGrant, bool) {
	if !live(c, now) {
		return ReadGrant{}, false
	}
	var chats map[CanonicalChat]struct{}
	if !c.ReadAll {
		chats = validChats(c.Read)
	}
	return seal.NewReadGrant(c.ID, c.ReadAll, chats), true
}

func DecideWrite(c *Client, now time.Time) (WriteGrant, bool) {
	if !live(c, now) || c.ReadAll {
		return WriteGrant{}, false
	}
	chats := validChats(c.Write)
	if len(chats) == 0 {
		return WriteGrant{}, false
	}
	return seal.NewWriteGrant(c.ID, chats, c.AllowFirstContact), true
}

func DecideAdmin(cred AdminCredential, presented string) (AdminGrant, bool) {
	if !WellFormedAdminToken(presented) || !seal.MatchAdminCredential(cred, sha256.Sum256([]byte(presented))) {
		return AdminGrant{}, false
	}
	return seal.NewAdminGrant(), true
}

func live(c *Client, now time.Time) bool {
	return c != nil && !c.Revoked && !now.IsZero() && !c.ExpiresAt.IsZero() && now.Before(c.ExpiresAt)
}

func validChats(set map[CanonicalChat]struct{}) map[CanonicalChat]struct{} {
	out := make(map[CanonicalChat]struct{}, len(set))
	for chat := range set {
		if chat.Valid() {
			out[chat] = struct{}{}
		}
	}
	return out
}
