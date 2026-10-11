package policy

import (
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestMayContact(t *testing.T) {
	tests := []struct {
		chat           CanonicalChat
		inbound, allow bool
		want           bool
	}{
		{chat: chatA, want: false},
		{chat: chatA, inbound: true, want: true},
		{chat: chatA, allow: true, want: true},
		{chat: chatB, want: false},
		{chat: chatB, inbound: true, want: true},
		{chat: chatB, allow: true, want: true},
		{chat: chatC, want: true},
		{chat: chatC, inbound: true, allow: true, want: true},
		{chat: CanonicalChat{}, inbound: true, allow: true, want: false},
	}
	for _, tt := range tests {
		if got := MayContact(tt.chat, tt.inbound, tt.allow); got != tt.want {
			t.Fatalf("MayContact(%q, inbound %v, first contact %v) = %v, want %v", tt.chat.JID(), tt.inbound, tt.allow, got, tt.want)
		}
	}
}

func TestWriteHolderMatchesDecideWriteForEveryShape(t *testing.T) {
	for _, s := range clientShapes() {
		for _, at := range []time.Time{{}, now, future, future.Add(-time.Nanosecond)} {
			_, ok := DecideWrite(s.client(), at)
			if got := WriteHolder(s.client(), at); got != ok {
				t.Fatalf("%s at %v: WriteHolder = %v, DecideWrite ok = %v", s.name, at, got, ok)
			}
		}
	}
}

var specChats = []string{
	"15550100001@s.whatsapp.net", "+1 555 0100 002", "100000000000001@lid", "120363000000000001@g.us",
	"15550100003:7@s.whatsapp.net", "status@broadcast", "not a chat", "",
}

func drawClient(t *rapid.T) *Client {
	if rapid.IntRange(0, 9).Draw(t, "nil") == 0 {
		return nil
	}
	pool := append([]CanonicalChat{{}}, knownChats...)
	write := set(rapid.SliceOfN(rapid.SampledFrom(pool), 0, 4).Draw(t, "write")...)
	var expires time.Time
	if rapid.Bool().Draw(t, "expiring") {
		expires = now.Add(time.Duration(rapid.Int64Range(-int64(48*time.Hour), int64(48*time.Hour)).Draw(t, "expiry")))
	}
	return &Client{ID: "aaaaaaaa", ReadAll: rapid.Bool().Draw(t, "read_all"), Write: write, ExpiresAt: expires,
		Revoked: rapid.Bool().Draw(t, "revoked"), AllowFirstContact: rapid.Bool().Draw(t, "first_contact")}
}

func drawTime(t *rapid.T, label string) time.Time {
	if rapid.IntRange(0, 9).Draw(t, label+"_zero") == 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(rapid.Int64Range(-int64(48*time.Hour), int64(400*24*time.Hour)).Draw(t, label)))
}

func TestAcceptedSpecsWriteOnlyWhatTheyReadAndHoldWritesAsDecided(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		spec := ClientSpec{
			Name:              rapid.SampledFrom([]string{"agent", "", "bad\x00name"}).Draw(t, "name"),
			Read:              rapid.SliceOfN(rapid.SampledFrom(specChats), 0, 4).Draw(t, "read"),
			Write:             rapid.SliceOfN(rapid.SampledFrom(specChats), 0, 4).Draw(t, "write"),
			AllChats:          rapid.Bool().Draw(t, "all_chats"),
			AllowFirstContact: rapid.Bool().Draw(t, "first_contact"),
			ExpiresInDays:     rapid.IntRange(-1, 400).Draw(t, "days"),
		}
		c, err := ValidateClientSpec(spec, now)
		if spec.AllChats && len(spec.Write) > 0 && err == nil {
			t.Fatalf("a client reading all chats was accepted with write chats %q", spec.Write)
		}
		if err == nil {
			for chat := range c.Write {
				if _, ok := c.Read[chat]; !ok || c.ReadAll {
					t.Fatalf("an accepted client writes %s outside its read chats", chat.JID())
				}
			}
			c.ID, c.Revoked = "aaaaaaaa", rapid.Bool().Draw(t, "revoked")
			at := drawTime(t, "spec_at")
			if _, ok := DecideWrite(&c, at); WriteHolder(&c, at) != ok {
				t.Fatalf("WriteHolder = %v for an accepted client, DecideWrite ok = %v", !ok, ok)
			}
		}
		other, at := drawClient(t), drawTime(t, "at")
		if _, ok := DecideWrite(other, at); WriteHolder(other, at) != ok {
			t.Fatalf("WriteHolder = %v, DecideWrite ok = %v", !ok, ok)
		}
	})
}
