package policy

import (
	"strings"
	"unique"
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

type ChatKind uint8

const (
	InvalidChat ChatKind = iota
	PhoneChat
	LIDChat
	GroupChat
)

func Normalize(s string) (CanonicalChat, bool) {
	if len(s) > maxChatInput {
		return CanonicalChat{}, false
	}
	local, server, _ := strings.Cut(s, "@")
	var user string
	var ok bool
	switch server {
	case phoneServer, legacyPhoneServer:
		user, ok = userPart(local)
		server = phoneServer
	case lidServer:
		user, ok = userPart(local)
	case groupServer:
		user, ok = local, groupPart(local)
	}
	if !ok {
		return CanonicalChat{}, false
	}
	return CanonicalChat{jid: unique.Make(user + "@" + server), ok: true}, true
}

func (c CanonicalChat) Valid() bool { return c.ok }

func (c CanonicalChat) JID() string {
	if !c.ok {
		return ""
	}
	return c.jid.Value()
}

func (c CanonicalChat) Kind() ChatKind {
	if !c.ok {
		return InvalidChat
	}
	jid := c.jid.Value()
	switch jid[strings.LastIndexByte(jid, '@')+1:] {
	case phoneServer:
		return PhoneChat
	case lidServer:
		return LIDChat
	case groupServer:
		return GroupChat
	}
	return InvalidChat
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
