package seal

import (
	"crypto/sha256"
	"testing"
	"unique"
)

var (
	chatA  = NewChat("15550100001@s.whatsapp.net", PhoneChat)
	chatB  = NewChat("100000000000001@lid", LIDChat)
	chatC  = NewChat("120363000000000001@g.us", GroupChat)
	probes = []Chat{{}, chatA, chatB, chatC}
)

func set(chats ...Chat) map[Chat]struct{} {
	s := make(map[Chat]struct{}, len(chats))
	for _, c := range chats {
		s[c] = struct{}{}
	}
	return s
}

func TestInvalidChatsExposeNothing(t *testing.T) {
	for name, c := range map[string]Chat{
		"zero value":                 {},
		"identifier without ok flag": {jid: unique.Make("15550100001@s.whatsapp.net"), kind: PhoneChat},
	} {
		t.Run(name, func(t *testing.T) {
			if c.Valid() || c.JID() != "" || c.Kind() != InvalidChat {
				t.Fatalf("Valid() = %v, JID() = %q, Kind() = %d, want false, empty, %d", c.Valid(), c.JID(), c.Kind(), InvalidChat)
			}
		})
	}
}

func TestGrantsWithoutTheOkFlagAllowNothing(t *testing.T) {
	reads := []struct {
		name string
		g    ReadGrant
	}{
		{name: "read-all", g: ReadGrant{client: "c1", all: true}},
		{name: "read-set", g: ReadGrant{client: "c1", chats: set(chatA, chatB)}},
		{name: "read-all with a set holding the zero chat", g: ReadGrant{client: "c1", all: true, chats: set(chatA, Chat{})}},
	}
	for _, tt := range reads {
		t.Run("ReadGrant "+tt.name, func(t *testing.T) {
			if tt.g.Valid() || tt.g.All() || len(tt.g.Chats()) != 0 {
				t.Fatalf("Valid() = %v, All() = %v, Chats() = %v, want false, false, empty", tt.g.Valid(), tt.g.All(), tt.g.Chats())
			}
			for _, chat := range probes {
				if tt.g.Allows(chat) {
					t.Fatalf("Allows(%q) = true", chat.JID())
				}
			}
		})
	}

	writes := []struct {
		name string
		g    WriteGrant
	}{
		{name: "write-set with first contact", g: WriteGrant{client: "c1", chats: set(chatA, chatB), allowFirstContact: true}},
		{name: "set holding the zero chat", g: WriteGrant{client: "c1", chats: set(chatA, Chat{}), allowFirstContact: true}},
	}
	for _, tt := range writes {
		t.Run("WriteGrant "+tt.name, func(t *testing.T) {
			if tt.g.Valid() || tt.g.AllowFirstContact() {
				t.Fatalf("Valid() = %v, AllowFirstContact() = %v, want false, false", tt.g.Valid(), tt.g.AllowFirstContact())
			}
			for _, chat := range probes {
				if tt.g.Allows(chat) {
					t.Fatalf("Allows(%q) = true", chat.JID())
				}
			}
		})
	}
}

func TestValidGrantsRefuseTheZeroChatEvenWhenTheirSetHoldsIt(t *testing.T) {
	r := NewReadGrant("c1", false, set(chatA, Chat{}))
	w := NewWriteGrant("c1", set(chatA, Chat{}), false)
	if r.Allows(Chat{}) || w.Allows(Chat{}) {
		t.Fatalf("a valid grant allows the zero-value chat: read %v, write %v", r.Allows(Chat{}), w.Allows(Chat{}))
	}
	if !r.Allows(chatA) || !w.Allows(chatA) {
		t.Fatal("the grants no longer allow their own chat")
	}
	if all := NewReadGrant("c1", true, nil); all.Allows(Chat{}) || !all.Allows(chatC) {
		t.Fatalf("a read-all grant: Allows(zero) = %v, Allows(chat) = %v, want false, true", all.Allows(Chat{}), all.Allows(chatC))
	}
}

func TestCredentialsMatchOnlyTheirDigestAndOnlyWithTheOkFlag(t *testing.T) {
	sum := sha256.Sum256([]byte("presented"))
	other := sum
	other[len(other)-1] ^= 1
	if !MatchAdminCredential(NewAdminCredential(sum), sum) {
		t.Fatal("a credential does not match its own digest")
	}
	for name, c := range map[string]AdminCredential{
		"zero value":                    {},
		"digest without its ok flag":    {sum: sum},
		"credential for another digest": NewAdminCredential(other),
	} {
		t.Run(name, func(t *testing.T) {
			if MatchAdminCredential(c, sum) {
				t.Fatal("the credential matched the digest")
			}
		})
	}
}
