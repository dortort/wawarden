package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var canonicalJID = regexp.MustCompile(`^(?:[0-9]{1,24}@(?:s\.whatsapp\.net|lid)|[0-9]{1,24}(?:-[0-9]{1,24})?@g\.us)$`)

var acceptedForm = regexp.MustCompile(`^([0-9]{1,24})(?:(?:\.([0-9]{1,3}))?:([0-9]{1,5}))?@(s\.whatsapp\.net|c\.us|lid)$|^([0-9]{1,24}(?:-[0-9]{1,24})?)@g\.us$`)

var canonicalKinds = map[string]ChatKind{"s.whatsapp.net": PhoneChat, "lid": LIDChat, "g.us": GroupChat}

var normalizeCases = []struct {
	name string
	in   string
	want string
	kind ChatKind
}{
	{name: "phone number", in: "15550100001@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "legacy phone server", in: "15550100001@c.us", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "phone device", in: "15550100001:7@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "phone device zero", in: "15550100001:0@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "phone agent and device", in: "15550100001.1:7@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "agent zero", in: "15550100001.0:0@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "largest agent and device", in: "15550100001.255:65535@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "device with leading zeros", in: "15550100001:00007@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "agent with leading zeros", in: "15550100001.001:7@s.whatsapp.net", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "legacy server with device", in: "15550100001:7@c.us", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "legacy server with agent and device", in: "15550100001.1:7@c.us", want: "15550100001@s.whatsapp.net", kind: PhoneChat},
	{name: "one-digit user", in: "1@s.whatsapp.net", want: "1@s.whatsapp.net", kind: PhoneChat},
	{name: "24-digit user", in: strings.Repeat("1", 24) + "@s.whatsapp.net", want: strings.Repeat("1", 24) + "@s.whatsapp.net", kind: PhoneChat},
	{name: "LID", in: "100000000000001@lid", want: "100000000000001@lid", kind: LIDChat},
	{name: "LID device", in: "100000000000001:12@lid", want: "100000000000001@lid", kind: LIDChat},
	{name: "LID agent and device", in: "100000000000001.1:12@lid", want: "100000000000001@lid", kind: LIDChat},
	{name: "24-digit LID", in: strings.Repeat("9", 24) + "@lid", want: strings.Repeat("9", 24) + "@lid", kind: LIDChat},
	{name: "group", in: "120363000000000001@g.us", want: "120363000000000001@g.us", kind: GroupChat},
	{name: "legacy group", in: "15550100001-1600000000@g.us", want: "15550100001-1600000000@g.us", kind: GroupChat},
	{name: "one-digit group", in: "1@g.us", want: "1@g.us", kind: GroupChat},
	{name: "shortest legacy group", in: "1-2@g.us", want: "1-2@g.us", kind: GroupChat},
	{name: "longest legacy group", in: strings.Repeat("1", 24) + "-" + strings.Repeat("2", 24) + "@g.us", want: strings.Repeat("1", 24) + "-" + strings.Repeat("2", 24) + "@g.us", kind: GroupChat},

	{name: "empty", in: ""},
	{name: "bare word", in: "abc"},
	{name: "bare number", in: "15550100001"},
	{name: "bare server", in: "s.whatsapp.net"},
	{name: "at sign alone", in: "@"},
	{name: "three parts", in: "a@b@c"},
	{name: "repeated at sign", in: "15550100001@@s.whatsapp.net"},
	{name: "server repeated after a second at sign", in: "15550100001@s.whatsapp.net@s.whatsapp.net"},
	{name: "empty server", in: "15550100001@"},
	{name: "empty user", in: "@s.whatsapp.net"},
	{name: "empty LID user", in: "@lid"},
	{name: "empty group", in: "@g.us"},
	{name: "letters in the user", in: "abc@s.whatsapp.net"},
	{name: "hexadecimal user", in: "0x1@s.whatsapp.net"},
	{name: "25-digit user", in: strings.Repeat("1", 25) + "@s.whatsapp.net"},
	{name: "25-digit LID", in: strings.Repeat("1", 25) + "@lid"},
	{name: "device 70000", in: "15550100001:70000@s.whatsapp.net"},
	{name: "device 65536", in: "15550100001:65536@s.whatsapp.net"},
	{name: "device -5", in: "15550100001:-5@s.whatsapp.net"},
	{name: "device +5", in: "15550100001:+5@s.whatsapp.net"},
	{name: "six-digit device", in: "15550100001:000007@s.whatsapp.net"},
	{name: "empty device", in: "15550100001:@s.whatsapp.net"},
	{name: "two devices", in: "15550100001:1:2@s.whatsapp.net"},
	{name: "agent 300", in: "15550100001.300:1@s.whatsapp.net"},
	{name: "agent 256", in: "15550100001.256:1@s.whatsapp.net"},
	{name: "four-digit agent", in: "15550100001.0001:1@s.whatsapp.net"},
	{name: "empty agent", in: "15550100001.:1@s.whatsapp.net"},
	{name: "agent without a device", in: "15550100001.1@s.whatsapp.net"},
	{name: "agent after the device", in: "15550100001:1.1@s.whatsapp.net"},
	{name: "two agents", in: "15550100001.1.1:1@s.whatsapp.net"},
	{name: "LID device 70000", in: "100000000000001:70000@lid"},
	{name: "LID agent 300", in: "100000000000001.300:1@lid"},
	{name: "group device", in: "120363000000000001:1@g.us"},
	{name: "group agent and device", in: "120363000000000001.1:1@g.us"},
	{name: "hyphen in a phone user", in: "15550100001-1600000000@s.whatsapp.net"},
	{name: "hyphen in a LID", in: "100000000000001-1@lid"},
	{name: "two hyphens in a group", in: "1-2-3@g.us"},
	{name: "leading hyphen in a group", in: "-1@g.us"},
	{name: "trailing hyphen in a group", in: "1-@g.us"},
	{name: "25-digit group", in: strings.Repeat("1", 25) + "@g.us"},
	{name: "25-digit legacy group timestamp", in: "1-" + strings.Repeat("2", 25) + "@g.us"},
	{name: "upper-case server", in: "15550100001@S.WHATSAPP.NET"},
	{name: "mixed-case server", in: "15550100001@s.WhatsApp.net"},
	{name: "upper-case legacy server", in: "15550100001@C.US"},
	{name: "upper-case LID server", in: "100000000000001@LID"},
	{name: "upper-case group server", in: "120363000000000001@G.US"},
	{name: "leading space", in: " 15550100001@s.whatsapp.net"},
	{name: "trailing space", in: "15550100001@s.whatsapp.net "},
	{name: "space before the at sign", in: "15550100001 @s.whatsapp.net"},
	{name: "tab", in: "15550100001\t@s.whatsapp.net"},
	{name: "trailing newline", in: "15550100001@s.whatsapp.net\n"},
	{name: "embedded newline", in: "1555\n0100001@s.whatsapp.net"},
	{name: "carriage return", in: "15550100001@s.whatsapp.net\r"},
	{name: "embedded NUL", in: "15550100001\x00@s.whatsapp.net"},
	{name: "trailing NUL", in: "15550100001@s.whatsapp.net\x00"},
	{name: "Arabic-Indic digits", in: "١٥٥٥٠١٠٠٠٠١@s.whatsapp.net"},
	{name: "full-width digits", in: "１５５５０１０００００１@s.whatsapp.net"},
	{name: "full-width at sign", in: "15550100001＠s.whatsapp.net"},
	{name: "invalid UTF-8", in: "15550100001\xff@s.whatsapp.net"},
	{name: "129-byte input", in: strings.Repeat("1", 129-len("@s.whatsapp.net")) + "@s.whatsapp.net"},
	{name: "status broadcast", in: "status@broadcast"},
	{name: "broadcast list", in: "1600000000@broadcast"},
	{name: "phone user on the broadcast server", in: "15550100001@broadcast"},
	{name: "newsletter", in: "120363000000000001@newsletter"},
	{name: "hosted", in: "15550100001@hosted"},
	{name: "hosted LID", in: "100000000000001@hosted.lid"},
	{name: "bot", in: "15550100001@bot"},
	{name: "messenger", in: "15550100001@msgr"},
	{name: "interop", in: "15550100001@interop"},
	{name: "server without its subdomain", in: "15550100001@whatsapp.net"},
	{name: "server with a trailing dot", in: "15550100001@s.whatsapp.net."},
	{name: "server with a suffix", in: "120363000000000001@g.us.example"},
}

func mustNormalize(s string) CanonicalChat {
	c, ok := Normalize(s)
	if !ok {
		panic("Normalize rejected " + s)
	}
	return c
}

func normalizeModel(s string) (string, bool) {
	if len(s) > 128 {
		return "", false
	}
	m := acceptedForm.FindStringSubmatch(s)
	switch {
	case m == nil:
		return "", false
	case m[5] != "":
		return m[5] + "@g.us", true
	}
	agent, _ := strconv.Atoi("0" + m[2])
	device, _ := strconv.Atoi("0" + m[3])
	if agent > 255 || device > 65535 {
		return "", false
	}
	server := m[4]
	if server == "c.us" {
		server = "s.whatsapp.net"
	}
	return m[1] + "@" + server, true
}

func checkCanonical(t *testing.T, c CanonicalChat) {
	t.Helper()
	jid := c.JID()
	user, server, _ := strings.Cut(jid, "@")
	if !c.Valid() || strings.Count(jid, "@") != 1 || strings.ContainsAny(user, ":.") {
		t.Fatalf("Normalize returned %q (valid %v), want exactly one @ and no : or . before it", jid, c.Valid())
	}
	if kind, ok := canonicalKinds[server]; !ok || c.Kind() != kind {
		t.Fatalf("Normalize returned %q of kind %d, want one of the servers %v and its kind", jid, c.Kind(), canonicalKinds)
	}
	if !canonicalJID.MatchString(jid) {
		t.Fatalf("Normalize returned %q, which does not match %s", jid, canonicalJID)
	}
	if again, ok := Normalize(jid); !ok || again != c {
		t.Fatalf("Normalize(%q) = %q, %v, want the same chat back", jid, again.JID(), ok)
	}
}

func TestNormalize(t *testing.T) {
	for _, tt := range normalizeCases {
		t.Run(tt.name, func(t *testing.T) {
			c, ok := Normalize(tt.in)
			if ok != (tt.want != "") || c.Valid() != ok {
				t.Fatalf("Normalize(%q) ok = %v, Valid() = %v, want %v", tt.in, ok, c.Valid(), tt.want != "")
			}
			if !ok {
				if c != (CanonicalChat{}) {
					t.Fatalf("Normalize(%q) rejected the input but returned %+v, want the zero value", tt.in, c)
				}
				return
			}
			if c.JID() != tt.want || c.Kind() != tt.kind {
				t.Fatalf("Normalize(%q) = %q of kind %d, want %q of kind %d", tt.in, c.JID(), c.Kind(), tt.want, tt.kind)
			}
		})
	}
}

func TestNormalizeIsCanonicalAndIdempotent(t *testing.T) {
	for _, tt := range normalizeCases {
		if tt.want == "" {
			continue
		}
		t.Run(tt.name, func(t *testing.T) { checkCanonical(t, mustNormalize(tt.in)) })
	}
}

func TestNormalizeGivesEveryFormOfAnIdentifierOneChat(t *testing.T) {
	forms := map[string][]string{
		"15550100001@s.whatsapp.net": {"15550100001@c.us", "15550100001:7@s.whatsapp.net", "15550100001.1:7@c.us", "15550100001:00000@s.whatsapp.net"},
		"100000000000001@lid":        {"100000000000001:12@lid", "100000000000001.255:65535@lid"},
	}
	for canonical, others := range forms {
		want := mustNormalize(canonical)
		chats := set(want)
		for _, form := range others {
			c := mustNormalize(form)
			if c != want {
				t.Fatalf("Normalize(%q) = %q, want %q", form, c.JID(), want.JID())
			}
			chats[c] = struct{}{}
		}
		if len(chats) != 1 {
			t.Fatalf("the forms of %q make %d map keys, want 1", canonical, len(chats))
		}
	}
	if mustNormalize("15550100001@s.whatsapp.net") == mustNormalize("15550100001@lid") {
		t.Fatal("a phone-number chat and a LID chat with the same digits are the same chat")
	}
}

func TestInvalidChatsExposeNothing(t *testing.T) {
	for name, c := range map[string]CanonicalChat{
		"zero value": {},
	} {
		t.Run(name, func(t *testing.T) {
			if c.Valid() || c.JID() != "" || c.Kind() != InvalidChat {
				t.Fatalf("Valid() = %v, JID() = %q, Kind() = %d, want false, empty, %d", c.Valid(), c.JID(), c.Kind(), InvalidChat)
			}
		})
	}
}

func TestPrintedChatsHideTheirIdentifier(t *testing.T) {
	for _, jid := range []string{"15550100001@s.whatsapp.net", "100000000000001@lid", "120363000000000001@g.us", "15550100001-1600000000@g.us"} {
		t.Run(jid, func(t *testing.T) {
			c := mustNormalize(jid)
			client := &Client{ID: "c1", Read: set(c), Write: set(c)}
			var out []string
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
				out = append(out, fmt.Sprintf(verb, c), fmt.Sprintf(verb, &c), fmt.Sprintf(verb, []CanonicalChat{c}), fmt.Sprintf(verb, client))
			}
			out = append(out, fmt.Sprint(c), fmt.Sprintln(c, &c), fmt.Errorf("chat %v", c).Error())
			var logs bytes.Buffer
			slog.New(slog.NewTextHandler(&logs, nil)).Info("read", "chat", c, "client", client)
			slog.New(slog.NewJSONHandler(&logs, nil)).Info("read", "chat", c, "client", client)
			encoded, err := json.Marshal(map[string]any{"chat": c, "chats": []CanonicalChat{c}})
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			out = append(out, logs.String(), string(encoded))
			user, _, _ := strings.Cut(jid, "@")
			for _, s := range out {
				if strings.Contains(s, "@") || strings.Contains(s, user) {
					t.Fatalf("printing a chat wrote its identifier %q: %s", jid, s)
				}
			}
		})
	}
}

func TestNormalizeAllocations(t *testing.T) {
	tests := []struct {
		name string
		in   string
		most float64
	}{
		{name: "phone number with agent and device", in: "15550100001.1:7@c.us", most: 1},
		{name: "legacy group", in: "15550100001-1600000000@g.us", most: 1},
		{name: "one-mebibyte user", in: strings.Repeat("1", 1<<20) + "@s.whatsapp.net"},
		{name: "one-mebibyte server", in: "15550100001@" + strings.Repeat("s", 1<<20)},
		{name: "out-of-range device", in: "15550100001:70000@s.whatsapp.net"},
		{name: "three parts", in: "a@b@c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if n := testing.AllocsPerRun(100, func() { Normalize(tt.in) }); n > tt.most {
				t.Fatalf("Normalize allocated %v times per call, want at most %v", n, tt.most)
			}
		})
	}
}

func FuzzNormalize(f *testing.F) {
	for _, tt := range normalizeCases {
		f.Add(tt.in)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, ok := Normalize(s)
		want, wantOK := normalizeModel(s)
		if ok != wantOK || c.JID() != want {
			t.Fatalf("Normalize(%q) = %q, %v, but the accepted forms give %q, %v", s, c.JID(), ok, want, wantOK)
		}
		if !ok {
			if c != (CanonicalChat{}) {
				t.Fatalf("Normalize(%q) rejected the input but returned %+v, want the zero value", s, c)
			}
			return
		}
		if len(s) > 128 {
			t.Fatalf("Normalize accepted a %d-byte input", len(s))
		}
		checkCanonical(t, c)
	})
}
