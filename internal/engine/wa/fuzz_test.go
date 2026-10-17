package wa

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
)

const maxFuzzMessage = 64 << 10

var messageOptions = proto.UnmarshalOptions{RecursionLimit: messageDepth, DiscardUnknown: true}

var knownKinds = map[engine.Kind]bool{
	engine.KindText: true, engine.KindMedia: true, engine.KindReaction: true, engine.KindPollUpdate: true,
	engine.KindEdit: true, engine.KindRevoke: true, engine.KindOther: true,
}

func quotedImage() *waE2E.Message {
	m := mediaFixtures[0].build(syntheticMedia(4096))
	m.ImageMessage.ContextInfo = &waE2E.ContextInfo{StanzaID: proto.String("3EB0A1"), QuotedMessage: &waE2E.Message{Conversation: proto.String("quoted")}}
	return m
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
		live(f, peer, peer, "3EB0M1", mediaFixtures[0].build(syntheticMedia(4096))),
		live(f, peer, peer, "3EB0M2", mediaFixtures[3].build(syntheticMedia(1<<20))),
		live(f, peer, peer, "3EB0M3", mediaFixtures[9].build(syntheticMedia(512))),
		live(f, peer, peer, "3EB0M4", viewOnceWrapped(mediaFixtures[6].build(syntheticMedia(64)))),
		live(f, peer, peer, "3EB0M5", quotedImage()),
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
	on := capture{max: 100 << 20, retention: time.Hour, sealer: fakeSealer{}, now: func() time.Time { return epoch }}
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
			ev, ok := translate(evt, capture{})
			sealed, captured := translate(evt, on)
			if ok != captured {
				t.Fatalf("translated %v without the capture and %v with it", ok, captured)
			}
			if !ok {
				continue
			}
			if msg, isMessage := sealed.(engine.Message); isMessage {
				checkCapturedMessage(t, ev.(engine.Message), msg)
			} else if !reflect.DeepEqual(ev, sealed) {
				t.Fatalf("the capture changed %+v into %+v", ev, sealed)
			}
			checkPlantedSecrets(t, info, &m, on)
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

func checkCapturedMessage(t *testing.T, plain, sealed engine.Message) {
	t.Helper()
	if !reflect.DeepEqual(withoutReference(sealed), plain) || plain.MediaRef != "" || plain.MediaKind != "" || plain.MediaVariant != "" || plain.MediaBytes != 0 {
		t.Fatalf("the capture changed more than the reference and its plain values: %+v without, %+v with", plain, sealed)
	}
	if sealed.MediaRef == "" {
		if sealed.MediaKind != "" || sealed.MediaVariant != "" || sealed.MediaBytes != 0 {
			t.Fatalf("plain media values without a reference: %+v", sealed)
		}
		return
	}
	pt, err := fakeSealer{}.OpenMedia(sealed.ID, sealed.MediaRef)
	if err != nil {
		t.Fatalf("the reference does not open for its message: %v", err)
	}
	d, ok := parseDescriptor(pt)
	declared, valid := d.declared()
	if !ok || !valid || sealed.Kind != engine.KindMedia || d.kind != sealed.MediaKind || d.variant != sealed.MediaVariant || declared != sealed.MediaBytes ||
		d.chat != sealed.Chat || d.sender != sealed.Sender || d.fromMe != sealed.FromMe {
		t.Fatalf("message %+v carries a reference to %+v", sealed, d)
	}
}

func checkPlantedSecrets(t *testing.T, info types.MessageInfo, m *waE2E.Message, on capture) {
	t.Helper()
	evt := (&events.Message{Info: info, RawMessage: proto.Clone(m).(*waE2E.Message)}).UnwrapRaw()
	part, ok := mediaOf(evt.Message)
	if !ok {
		return
	}
	path, key, sha, enc := "/"+hex.EncodeToString(randomBytes(16)), randomBytes(32), randomBytes(32), randomBytes(32)
	r := part.item.(proto.Message).ProtoReflect()
	fields := r.Descriptor().Fields()
	r.Set(fields.ByName("directPath"), protoreflect.ValueOfString(path))
	r.Set(fields.ByName("mediaKey"), protoreflect.ValueOfBytes(key))
	r.Set(fields.ByName("fileSHA256"), protoreflect.ValueOfBytes(sha))
	r.Set(fields.ByName("fileEncSHA256"), protoreflect.ValueOfBytes(enc))
	for _, mc := range []capture{{}, on} {
		if ev, ok := translate(evt, mc); ok {
			if msg, isMessage := ev.(engine.Message); isMessage {
				noMediaSecrets(t, "a fuzzed message", msg, []byte(path), key, sha, enc)
			}
		}
	}
}

func noMediaSecrets(t *testing.T, where string, m engine.Message, secrets ...[]byte) {
	t.Helper()
	fields := stringsIn(reflect.ValueOf(withoutReference(m)))
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		hexed := hex.EncodeToString(secret)
		for _, form := range []string{string(secret), hexed, strings.ToUpper(hexed), base64.RawStdEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret)} {
			for _, field := range fields {
				if strings.Contains(field, form) {
					t.Fatalf("%s carries media key material outside its reference: %q in %+v", where, field, m)
				}
			}
		}
	}
}

func stringsIn(v reflect.Value) []string {
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Pointer:
		if !v.IsNil() {
			return stringsIn(v.Elem())
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return []string{string(v.Bytes())}
		}
		var out []string
		for i := range v.Len() {
			out = append(out, stringsIn(v.Index(i))...)
		}
		return out
	case reflect.Struct:
		var out []string
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out = append(out, stringsIn(v.Field(i))...)
			}
		}
		return out
	}
	return nil
}

func FuzzMediaDescriptor(f *testing.F) {
	for _, fx := range mediaFixtures {
		media := syntheticMedia(4096)
		part, _ := mediaOf(fx.build(media))
		f.Add(descriptor{directPath: media.path, mediaKey: media.key, fileSHA256: media.sha, fileEncSHA256: media.enc, length: *media.fileLength(),
			kind: part.kind, variant: part.variant, chat: group.String(), sender: victim.String()}.marshal())
	}
	f.Add([]byte{descriptorVersion})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, ok := parseDescriptor(data)
		if !ok {
			return
		}
		if !bytes.Equal(d.marshal(), data) {
			t.Fatalf("parseDescriptor accepted %x, which is not the encoding of %+v", data, d)
		}
		if declared, valid := d.declared(); valid && (declared < 0 || declared > mediaMaxDeclared || !strings.HasPrefix(d.directPath, "/")) {
			t.Fatalf("a valid descriptor %+v declares %d bytes", d, declared)
		}
	})
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
