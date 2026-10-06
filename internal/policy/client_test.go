package policy

import (
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	specPhone = "15550100001@s.whatsapp.net"
	specLID   = "100000000000001@lid"
	specGroup = "120363000000000001@g.us"
)

func manyChats(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "1555010" + strconv.Itoa(1000+i) + "@s.whatsapp.net"
	}
	return out
}

func TestValidateClientSpecRefusals(t *testing.T) {
	tests := []struct {
		name string
		spec ClientSpec
		now  time.Time
		want *SpecError
	}{
		{name: "empty name", spec: ClientSpec{Read: []string{specPhone}}, want: ErrNameInvalid},
		{name: "name of 65 characters", spec: ClientSpec{Name: strings.Repeat("é", 65), Read: []string{specPhone}}, want: ErrNameInvalid},
		{name: "name with a newline", spec: ClientSpec{Name: "agent\nx", Read: []string{specPhone}}, want: ErrNameInvalid},
		{name: "name with an escape", spec: ClientSpec{Name: "agent\x1b[31m", Read: []string{specPhone}}, want: ErrNameInvalid},
		{name: "name with a C1 control", spec: ClientSpec{Name: "agent\u0085", Read: []string{specPhone}}, want: ErrNameInvalid},
		{name: "name not UTF-8", spec: ClientSpec{Name: "agent\xff", Read: []string{specPhone}}, want: ErrNameInvalid},
		{name: "expiry of 366 days", spec: ClientSpec{Name: "a", Read: []string{specPhone}, ExpiresInDays: 366}, want: ErrExpiryOutOfRange},
		{name: "negative expiry", spec: ClientSpec{Name: "a", Read: []string{specPhone}, ExpiresInDays: -1}, want: ErrExpiryOutOfRange},
		{name: "no clock", spec: ClientSpec{Name: "a", Read: []string{specPhone}}, now: time.Time{}, want: ErrExpiryOutOfRange},
		{name: "257 read chats", spec: ClientSpec{Name: "a", Read: manyChats(257)}, want: ErrTooManyChats},
		{name: "257 write chats", spec: ClientSpec{Name: "a", Read: manyChats(256), Write: manyChats(257)}, want: ErrTooManyChats},
		{name: "invalid read chat", spec: ClientSpec{Name: "a", Read: []string{"+15550100001"}}, want: ErrChatInvalid},
		{name: "invalid write chat", spec: ClientSpec{Name: "a", Read: []string{specPhone}, Write: []string{"status@broadcast"}}, want: ErrChatInvalid},
		{name: "all chats with a write chat", spec: ClientSpec{Name: "a", AllChats: true, Write: []string{specPhone}}, want: ErrAllChatsWithWrite},
		{name: "all chats with read and write chats", spec: ClientSpec{Name: "a", AllChats: true, Read: []string{specPhone}, Write: []string{specPhone}}, want: ErrAllChatsWithWrite},
		{name: "all chats with a read chat", spec: ClientSpec{Name: "a", AllChats: true, Read: []string{specPhone}}, want: ErrReadScopeConflict},
		{name: "no read scope", spec: ClientSpec{Name: "a"}, want: ErrReadScopeMissing},
		{name: "write only", spec: ClientSpec{Name: "a", Write: []string{specPhone}}, want: ErrReadScopeMissing},
		{name: "write chat not readable", spec: ClientSpec{Name: "a", Read: []string{specPhone}, Write: []string{specGroup}}, want: ErrWriteNotReadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := now
			if tt.name == "no clock" {
				at = tt.now
			}
			c, err := ValidateClientSpec(tt.spec, at)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ValidateClientSpec() error %v, want %v", err, tt.want)
			}
			if c.Name != "" || c.Read != nil || c.Write != nil || !c.ExpiresAt.IsZero() {
				t.Fatalf("a refused spec returned a client: %+v", c)
			}
			var typed *SpecError
			if !errors.As(err, &typed) || typed.Code() != tt.want.code {
				t.Fatalf("the error %v carries no code %q", err, tt.want.code)
			}
		})
	}
}

func TestValidateClientSpecAccepts(t *testing.T) {
	tests := []struct {
		name        string
		spec        ClientSpec
		read, write map[CanonicalChat]struct{}
		all         bool
		days        int
	}{
		{name: "default expiry", spec: ClientSpec{Name: "reader", Read: []string{specPhone}}, read: set(chatA), days: 90},
		{name: "one day", spec: ClientSpec{Name: "r", Read: []string{specPhone}, ExpiresInDays: 1}, read: set(chatA), days: 1},
		{name: "longest expiry", spec: ClientSpec{Name: strings.Repeat("é", 64), Read: []string{specPhone}, ExpiresInDays: 365}, read: set(chatA), days: 365},
		{name: "all chats", spec: ClientSpec{Name: "all", AllChats: true}, all: true, days: 90},
		{
			name: "write subset of read, forms normalised and repeated",
			spec: ClientSpec{Name: "writer", Read: []string{specPhone, "15550100001:3@c.us", specLID, specGroup}, Write: []string{"15550100001@c.us", specGroup}, AllowFirstContact: true},
			read: set(chatA, chatB, chatC), write: set(chatA, chatC), days: 90,
		},
		{name: "256 chats in each set", spec: ClientSpec{Name: "many", Read: manyChats(256), Write: manyChats(256)}, days: 90},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ValidateClientSpec(tt.spec, now)
			if err != nil {
				t.Fatalf("ValidateClientSpec(): %v", err)
			}
			if c.ID != "" || c.Name != tt.spec.Name || c.ReadAll != tt.all || c.Revoked || c.AllowFirstContact != tt.spec.AllowFirstContact {
				t.Fatalf("client %+v does not reflect the spec %+v", c, tt.spec)
			}
			if want := now.Add(time.Duration(tt.days) * 24 * time.Hour); !c.ExpiresAt.Equal(want) {
				t.Fatalf("expires at %v, want %v", c.ExpiresAt, want)
			}
			if tt.read != nil && !maps.Equal(c.Read, tt.read) || tt.write != nil && !maps.Equal(c.Write, tt.write) {
				t.Fatalf("sets %v and %v, want %v and %v", c.Read, c.Write, tt.read, tt.write)
			}
			if tt.name == "256 chats in each set" && (len(c.Read) != 256 || len(c.Write) != 256) {
				t.Fatalf("%d read and %d write chats, want 256 each", len(c.Read), len(c.Write))
			}
			if _, ok := DecideRead(&c, now); !ok {
				t.Fatal("an accepted client is not live")
			}
			if _, ok := DecideRead(&c, c.ExpiresAt); ok {
				t.Fatal("an accepted client is live at its expiry")
			}
		})
	}
}

func TestWritableChat(t *testing.T) {
	tests := []struct {
		chat         CanonicalChat
		known, allow bool
		want         bool
	}{
		{chat: chatA, known: true, want: true},
		{chat: chatC, known: true, want: true},
		{chat: chatA, allow: true, want: true},
		{chat: chatB, allow: true, want: true},
		{chat: chatC, allow: true, want: false},
		{chat: chatA, want: false},
		{chat: CanonicalChat{}, known: true, allow: true, want: false},
	}
	for _, tt := range tests {
		if got := WritableChat(tt.chat, tt.known, tt.allow); got != tt.want {
			t.Fatalf("WritableChat(%q, known %v, first contact %v) = %v, want %v", tt.chat.JID(), tt.known, tt.allow, got, tt.want)
		}
	}
}
