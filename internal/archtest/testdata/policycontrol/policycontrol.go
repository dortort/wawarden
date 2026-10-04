package policy

import (
	"time"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

var fixtureChats = map[CanonicalChat]struct{}{{}: {}}

func fixtureControl(c *Client, now time.Time) (ReadGrant, WriteGrant, bool) {
	chat, ok := Normalize("15550100001@s.whatsapp.net")
	r, _ := DecideRead(c, now)
	w, _ := DecideWrite(c, now)
	var zero seal.Chat
	return r, w, ok && live(c, now) && len(validChats(fixtureChats)) == 0 && chat != zero && chat.Kind() == PhoneChat
}
