package wa

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
)

const maxFuzzMessage = 64 << 10

var messageOptions = proto.UnmarshalOptions{RecursionLimit: messageDepth, DiscardUnknown: true}

var knownKinds = map[engine.Kind]bool{
	engine.KindText: true, engine.KindMedia: true, engine.KindReaction: true, engine.KindPollUpdate: true,
	engine.KindEdit: true, engine.KindRevoke: true, engine.KindOther: true,
}

func FuzzMessageBytes(f *testing.F) {
	for _, evt := range []*events.Message{
		textMessage(f, peer, peer, "3EB0A1", "synthetic"),
		editMessage(f, peer, peer, "3EB0E1", msgKey(owner, true, "3EB0A1", nil), "synthetic edit"),
		revokeMessage(f, group, admin, "3EB0R2", msgKey(group, false, "3EB0G9", &victim), types.EditAttributeAdminRevoke),
		historyNotification(f, owner, "3EB0H1", nil),
		historyNotification(f, owner, "3EB0H2", []byte{1, 2, 3}),
		live(f, peer, peer, "3EB0Q1", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("synthetic reply"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("3EB0A1"), Expiration: proto.Uint32(86400), QuotedMessage: &waE2E.Message{Conversation: proto.String("quoted")}},
		}}),
		live(f, group, victim, "3EB0X1", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: msgKey(group, false, "3EB0G9", &admin), Text: proto.String("+")}}),
		live(f, peer, peer, "3EB0Z1", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: msgKey(peer, false, "3EB0A1", nil)}}),
	} {
		b, err := proto.Marshal(evt.RawMessage)
		if err != nil {
			f.Fatalf("Marshal: %v", err)
		}
		f.Add(b)
	}
	infos := []types.MessageInfo{
		{MessageSource: types.MessageSource{Chat: peer, Sender: peer}, ID: "3EB0F1", Timestamp: epoch},
		{MessageSource: types.MessageSource{Chat: group, Sender: types.JID{User: victim.User, Device: 2, Server: types.DefaultUserServer}, IsGroup: true}, ID: "3EB0F2", Timestamp: epoch},
		{MessageSource: types.MessageSource{Chat: owner, Sender: owner, IsFromMe: true}, ID: "3EB0F3", Timestamp: epoch},
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFuzzMessage {
			return
		}
		var m waE2E.Message
		if messageOptions.Unmarshal(data, &m) != nil {
			return
		}
		for _, info := range infos {
			evt := (&events.Message{Info: info, RawMessage: &m}).UnwrapRaw()
			ev, ok := translate(evt)
			if !ok {
				continue
			}
			switch e := ev.(type) {
			case engine.Message:
				checkFuzzedMessage(t, info, evt, e, len(data))
			case engine.HistoryNotification:
				if e.FromMe != info.IsFromMe || e.Sender != info.Sender.String() || e.Ref.ID != info.ID || len(e.Ref.Inline) > len(data) {
					t.Fatalf("history notification %+v from %+v", e, info)
				}
			default:
				t.Fatalf("a message became %T", ev)
			}
		}
	})
}

func checkFuzzedMessage(t *testing.T, info types.MessageInfo, evt *events.Message, e engine.Message, size int) {
	t.Helper()
	if !knownKinds[e.Kind] || e.Chat != info.Chat.String() || e.ID != info.ID || e.Sender != info.Sender.String() || e.FromMe != info.IsFromMe {
		t.Fatalf("message %+v from %+v", e, info)
	}
	if len(e.Text) > size || len(e.MediaType) > maxMediaType || e.Reply != nil && len(e.Reply.Text) > size {
		t.Fatalf("content longer than its input: %d, %d, %+v > %d", len(e.Text), len(e.MediaType), e.Reply, size)
	}
	pm := evt.Message.GetProtocolMessage()
	switch e.Kind {
	case engine.KindRevoke:
		if pm == nil || pm.Type == nil || *pm.Type != waE2E.ProtocolMessage_REVOKE {
			t.Fatalf("a revoke from a message that sets no REVOKE type: %v", evt.Message)
		}
	case engine.KindEdit:
		if pm == nil || pm.Type == nil || *pm.Type != waE2E.ProtocolMessage_MESSAGE_EDIT {
			t.Fatalf("an edit from a message that sets no MESSAGE_EDIT type: %v", evt.Message)
		}
	}
	if e.Expiration < 0 {
		t.Fatalf("negative expiration %v", e.Expiration)
	}
}

func FuzzNormalizeAgainstParseJID(f *testing.F) {
	for _, s := range []string{
		"15550100001@s.whatsapp.net", "15550100001:4@s.whatsapp.net", "15550100001.0:4@lid", "15550100001.255:65535@lid",
		"120363000000000001@g.us", "15550100001-1696000000@g.us", "15550100001@c.us", "status@broadcast", "120363000000000002@newsletter",
		"a@b@c", "15550100001:70000@s.whatsapp.net", "15550100001.300:1@lid", "", "abc", "15550100001@S.WHATSAPP.NET", "0@s.whatsapp.net",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, ok := policy.Normalize(s)
		if !ok {
			return
		}
		in, err := types.ParseJID(s)
		if err != nil {
			t.Fatalf("Normalize accepted %q, which the protocol library cannot parse: %v", s, err)
		}
		out, err := types.ParseJID(c.JID())
		if err != nil {
			t.Fatalf("the protocol library cannot parse Normalize(%q) = %q: %v", s, c.JID(), err)
		}
		server := in.Server
		if server == types.LegacyUserServer {
			server = types.DefaultUserServer
		}
		if out.User != in.User || out.Server != server {
			t.Fatalf("Normalize(%q) = %q, but the protocol library reads the input as user %q on %q", s, c.JID(), in.User, in.Server)
		}
		if out.Device != 0 || out.RawAgent != 0 || out.Integrator != 0 {
			t.Fatalf("Normalize(%q) = %q, which the protocol library reads with a device or agent: %+v", s, c.JID(), out)
		}
		if out.String() != c.JID() {
			t.Fatalf("Normalize(%q) = %q, which the protocol library writes as %q", s, c.JID(), out.String())
		}
	})
}
