package policy

import (
	"strings"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

const (
	maxChatInput      = 128
	maxUserDigits     = 24
	maxAgentDigits    = 3
	maxAgent          = 255
	maxDeviceDigits   = 5
	maxDevice         = 65535
	phoneServer       = "s.whatsapp.net"
	legacyPhoneServer = "c.us"
	lidServer         = "lid"
	groupServer       = "g.us"
)

type ChatKind = seal.ChatKind

const (
	InvalidChat = seal.InvalidChat
	PhoneChat   = seal.PhoneChat
	LIDChat     = seal.LIDChat
	GroupChat   = seal.GroupChat
)

func Normalize(s string) (CanonicalChat, bool) {
	if len(s) > maxChatInput {
		return CanonicalChat{}, false
	}
	local, server, _ := strings.Cut(s, "@")
	var user string
	var kind ChatKind
	var ok bool
	switch server {
	case phoneServer, legacyPhoneServer:
		user, ok = userPart(local)
		server, kind = phoneServer, PhoneChat
	case lidServer:
		user, ok = userPart(local)
		kind = LIDChat
	case groupServer:
		user, ok = local, groupPart(local)
		kind = GroupChat
	}
	if !ok {
		return CanonicalChat{}, false
	}
	return seal.NewChat(user+"@"+server, kind), true
}

func userPart(local string) (string, bool) {
	head, device, hasDevice := strings.Cut(local, ":")
	user, agent, hasAgent := strings.Cut(head, ".")
	ok := digits(user, maxUserDigits) &&
		(!hasDevice || number(device, maxDeviceDigits, maxDevice)) &&
		(!hasAgent || hasDevice && number(agent, maxAgentDigits, maxAgent))
	return user, ok
}

func groupPart(local string) bool {
	creator, created, legacy := strings.Cut(local, "-")
	return digits(creator, maxUserDigits) && (!legacy || digits(created, maxUserDigits))
}

func digits(s string, most int) bool {
	if s == "" || len(s) > most {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func number(s string, most, limit int) bool {
	if !digits(s, most) {
		return false
	}
	n := 0
	for i := range len(s) {
		n = n*10 + int(s[i]-'0')
	}
	return n <= limit
}
