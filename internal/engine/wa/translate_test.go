package wa

import (
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
)

func translated(t *testing.T, evt any) engine.Event {
	t.Helper()
	ev, ok := translate(evt)
	if !ok {
		t.Fatalf("%T was not translated", evt)
	}
	return ev
}

func asMessage(t *testing.T, evt any) engine.Message {
	t.Helper()
	m, ok := translated(t, evt).(engine.Message)
	if !ok {
		t.Fatalf("%T did not translate to a message", evt)
	}
	return m
}

func TestFixtureText(t *testing.T) {
	got := asMessage(t, textMessage(t, peer, peer, "3EB0A1", "synthetic hello"))
	want := engine.Message{Chat: peer.String(), ID: "3EB0A1", Sender: peer.String(), Timestamp: epoch, PushName: "Synthetic Peer", Kind: engine.KindText, Text: "synthetic hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("text = %+v, want %+v", got, want)
	}
}

func TestFixtureEdit(t *testing.T) {
	evt := editMessage(t, peer, peer, "3EB0E1", msgKey(owner, true, "3EB0A1", nil), "synthetic hello, edited")
	if !evt.IsEdit {
		t.Fatal("the fixture is not unwrapped as an edit")
	}
	got := asMessage(t, evt)
	if got.Kind != engine.KindEdit || got.ID != "3EB0E1" || got.Text != "synthetic hello, edited" || got.Timestamp != epoch ||
		got.Target == nil || *got.Target != (engine.Key{RemoteJID: owner.String(), FromMe: true, ID: "3EB0A1"}) {
		t.Fatalf("edit = %+v (target %+v)", got, got.Target)
	}
}

func TestFixtureOwnRevoke(t *testing.T) {
	evt := revokeMessage(t, peer, owner, "3EB0R1", msgKey(peer, true, "3EB0M7", nil), types.EditAttributeSenderRevoke)
	evt.Info.IsFromMe = true
	got := asMessage(t, evt)
	if got.Kind != engine.KindRevoke || !got.FromMe || got.Sender != owner.String() || got.Target == nil ||
		*got.Target != (engine.Key{RemoteJID: peer.String(), FromMe: true, ID: "3EB0M7"}) {
		t.Fatalf("own revoke = %+v (target %+v)", got, got.Target)
	}
}

func TestFixtureAdminRevoke(t *testing.T) {
	got := asMessage(t, revokeMessage(t, group, admin, "3EB0R2", msgKey(group, false, "3EB0G9", &victim), types.EditAttributeAdminRevoke))
	if got.Kind != engine.KindRevoke || got.Chat != group.String() || got.Sender != admin.String() || got.Target == nil ||
		*got.Target != (engine.Key{RemoteJID: group.String(), ID: "3EB0G9", Participant: victim.String()}) {
		t.Fatalf("admin revoke = %+v (target %+v)", got, got.Target)
	}
}

func TestFixtureCrossChatRevokeKeepsTheForeignChatForTheEngineToRefuse(t *testing.T) {
	got := asMessage(t, revokeMessage(t, group, admin, "3EB0R3", msgKey(peer, false, "3EB0A1", &peer), types.EditAttributeAdminRevoke))
	if got.Kind != engine.KindRevoke || got.Chat != group.String() || got.Target == nil || got.Target.RemoteJID != peer.String() {
		t.Fatalf("cross-chat revoke = %+v (target %+v): the key's chat must reach the engine, which never follows it", got, got.Target)
	}
}

func TestFixtureReaction(t *testing.T) {
	evt := live(t, group, victim, "3EB0X1", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: msgKey(group, false, "3EB0G9", &admin), Text: proto.String("\U0001F44D"), SenderTimestampMS: proto.Int64(epoch.UnixMilli()),
	}})
	got := asMessage(t, evt)
	if got.Kind != engine.KindReaction || got.Text != "" || got.Target == nil || *got.Target != (engine.Key{RemoteJID: group.String(), ID: "3EB0G9", Participant: admin.String()}) {
		t.Fatalf("reaction = %+v (target %+v), want its target without its content", got, got.Target)
	}
	enc := asMessage(t, live(t, group, victim, "3EB0X2", &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{
		TargetMessageKey: msgKey(group, false, "3EB0G9", &admin), EncPayload: []byte("ciphertext"), EncIV: []byte("0123456789ab"),
	}}))
	if enc.Kind != engine.KindReaction || enc.Target == nil || enc.Target.ID != "3EB0G9" {
		t.Fatalf("encrypted reaction = %+v", enc)
	}
}

func TestFixturePollUpdate(t *testing.T) {
	got := asMessage(t, live(t, group, victim, "3EB0V1", &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
		PollCreationMessageKey: msgKey(group, false, "3EB0P1", &admin),
		Vote:                   &waE2E.PollEncValue{EncPayload: []byte("ciphertext"), EncIV: []byte("0123456789ab")},
		Metadata:               &waE2E.PollUpdateMessageMetadata{},
		SenderTimestampMS:      proto.Int64(epoch.UnixMilli()),
	}}))
	if got.Kind != engine.KindPollUpdate || got.Text != "" || got.Target == nil || got.Target.ID != "3EB0P1" || got.Target.Participant != admin.String() {
		t.Fatalf("poll update = %+v (target %+v)", got, got.Target)
	}
}

func TestFixtureDisappearing(t *testing.T) {
	evt := live(t, peer, peer, "3EB0D1", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("synthetic, vanishing"),
			ContextInfo: &waE2E.ContextInfo{Expiration: proto.Uint32(604800), EphemeralSettingTimestamp: proto.Int64(epoch.Unix())},
		},
	}}})
	if !evt.IsEphemeral {
		t.Fatal("the fixture is not unwrapped as ephemeral")
	}
	got := asMessage(t, evt)
	if got.Kind != engine.KindText || got.Text != "synthetic, vanishing" || got.Expiration != 7*24*time.Hour {
		t.Fatalf("disappearing = %+v, want a week's expiration", got)
	}
}

func TestFixtureForgedQuote(t *testing.T) {
	got := asMessage(t, live(t, peer, peer, "3EB0Q1", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("synthetic reply"),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID: proto.String("3EB0A1"), Participant: proto.String(peer.String()),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("FORGED original text")},
		},
	}}))
	if got.Reply == nil || *got.Reply != (engine.Reply{ID: "3EB0A1", Participant: peer.String(), Text: "FORGED original text"}) {
		t.Fatalf("forged quote = %+v, want the quoted text passed on for verification against the stored original", got.Reply)
	}
}

func TestFixtureForeignRemoteJID(t *testing.T) {
	got := asMessage(t, live(t, peer, peer, "3EB0Q2", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:  proto.String("synthetic caption"),
		Mimetype: proto.String("image/jpeg"),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID: proto.String("3EB0A1"), Participant: proto.String(other.String()), RemoteJID: proto.String(other.String()),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("secret from another chat")},
		},
	}}))
	if got.Kind != engine.KindMedia || got.MediaType != "image/jpeg" || got.Text != "synthetic caption" || got.Reply == nil || got.Reply.RemoteJID != other.String() {
		t.Fatalf("foreign RemoteJID = %+v (reply %+v)", got, got.Reply)
	}
}

func TestFixtureStatusBroadcastAndNewsletterReachTheEngineUnderTheirOwnChats(t *testing.T) {
	status := live(t, types.StatusBroadcastJID, peer, "3EB0S1", &waE2E.Message{Conversation: proto.String("synthetic status")})
	status.Info.IsGroup = true
	list := live(t, types.NewJID("1696000000", types.BroadcastServer), peer, "3EB0S2", &waE2E.Message{Conversation: proto.String("synthetic list")})
	news := live(t, channel, channel, "3EB0N1", &waE2E.Message{Conversation: proto.String("synthetic channel post")})
	news.Info.ServerID = 4711
	for name, evt := range map[string]*events.Message{"status": status, "broadcast list": list, "newsletter": news} {
		got := asMessage(t, evt)
		if got.Chat != evt.Info.Chat.String() {
			t.Errorf("%s = %+v: the chat must stay what WhatsApp named, for the engine to drop", name, got)
		}
	}
}

func TestFixtureHistoryNotifications(t *testing.T) {
	primary, ok := translated(t, historyNotification(t, owner, "3EB0H1", nil)).(engine.HistoryNotification)
	if !ok || !primary.FromMe || primary.Sender != owner.String() || primary.Ref.ID != "3EB0H1" || primary.Ref.DirectPath != "/v/t62.7118-24/synthetic" ||
		primary.Ref.FileLength != 1<<20 || len(primary.Ref.MediaKey) != 32 || len(primary.Ref.FileEncSHA256) != 32 || primary.Ref.Inline != nil {
		t.Fatalf("history notification from device 0 = %+v", primary)
	}
	other, ok := translated(t, historyNotification(t, companion, "3EB0H2", nil)).(engine.HistoryNotification)
	if !ok || other.Sender != companion.String() {
		t.Fatalf("history notification from device 5 = %+v: the device must reach the engine, which drops it", other)
	}
}

func TestFixtureInlineBootstrapPayload(t *testing.T) {
	inline := []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01}
	got, ok := translated(t, historyNotification(t, owner, "3EB0H3", inline)).(engine.HistoryNotification)
	if !ok || string(got.Ref.Inline) != string(inline) || got.Ref.DirectPath != "" {
		t.Fatalf("inline bootstrap = %+v, want the payload and no download", got)
	}
}

func TestFixtureTheRevokeZeroValueTrap(t *testing.T) {
	untyped := live(t, peer, peer, "3EB0Z1", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: msgKey(peer, false, "3EB0A1", nil)}})
	if untyped.Message.GetProtocolMessage().GetType() != waE2E.ProtocolMessage_REVOKE {
		t.Fatal("the library no longer reads an absent type as REVOKE; this trap needs another look")
	}
	if ev, ok := translate(untyped); ok {
		t.Fatalf("a protocol message without a type became %+v", ev)
	}
	other := live(t, peer, peer, "3EB0Z2", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(), EphemeralExpiration: proto.Uint32(86400)}})
	if ev, ok := translate(other); ok {
		t.Fatalf("a protocol message of another type became %+v", ev)
	}
	plain := asMessage(t, textMessage(t, peer, peer, "3EB0Z3", "not a revoke"))
	if plain.Kind != engine.KindText {
		t.Fatalf("a plain message became %s", plain.Kind)
	}
}

func TestIdentifiersKeepTheirDevicePartForTheEngineToStrip(t *testing.T) {
	sender := types.JID{User: peer.User, Device: 7, Server: types.DefaultUserServer}
	alt := types.JID{User: peerLID.User, Device: 7, Server: types.HiddenUserServer}
	evt := textMessage(t, group, sender, "3EB0K1", "synthetic")
	evt.Info.SenderAlt, evt.Info.AddressingMode = alt, types.AddressingModePN
	got := asMessage(t, evt)
	if got.Sender != "15550100001:7@s.whatsapp.net" || got.SenderAlt != "100000000000001:7@lid" || got.Addressing != "pn" {
		t.Fatalf("identifiers = %q, %q, %q", got.Sender, got.SenderAlt, got.Addressing)
	}
	fromMe := textMessage(t, peer, owner, "3EB0K2", "synthetic")
	fromMe.Info.IsFromMe, fromMe.Info.RecipientAlt = true, peerLID
	if got := asMessage(t, fromMe); got.RecipientAlt != peerLID.String() || got.SenderAlt != "" {
		t.Fatalf("a message of the owner's = %+v", got)
	}
}

func TestMessagesWithoutContentAreAcknowledgedAndDropped(t *testing.T) {
	for name, raw := range map[string]*waE2E.Message{
		"a sender key distribution": {SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String(group.String())}},
		"context only":              {MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: []byte("secret")}},
		"empty":                     {},
	} {
		if ev, ok := translate(live(t, group, peer, "3EB0C1", raw)); ok {
			t.Errorf("%s became %+v", name, ev)
		}
	}
	if ev, ok := translate(&events.Message{Info: types.MessageInfo{ID: "3EB0C2"}}); ok {
		t.Fatalf("a message without content became %+v", ev)
	}
	got := asMessage(t, live(t, peer, peer, "3EB0C3", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(1)}}))
	if got.Kind != engine.KindOther || got.Text != "" {
		t.Fatalf("a location = %+v, want a row of kind other without text", got)
	}
}

func TestMediaTypesAreReducedToAPlainMIMEEssence(t *testing.T) {
	for _, tt := range []struct {
		msg  *waE2E.Message
		want string
	}{
		{&waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus")}}, "audio/ogg"},
		{&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String("Application/PDF"), Caption: proto.String("synthetic doc")}}, "application/pdf"},
		{&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("<script>")}}, "video"},
		{&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, "sticker"},
		{&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/" + string(make([]byte, 80)))}}, "image"},
	} {
		got := asMessage(t, live(t, peer, peer, "3EB0F1", tt.msg))
		if got.Kind != engine.KindMedia || got.MediaType != tt.want {
			t.Errorf("media %v = %s %q, want %q", tt.msg, got.Kind, got.MediaType, tt.want)
		}
	}
}

func TestConnectionAndPairingEventsTranslate(t *testing.T) {
	for _, tt := range []struct {
		in   any
		want engine.Event
	}{
		{&events.Connected{}, engine.Connected{}},
		{&events.Disconnected{}, engine.Disconnected{}},
		{&events.ClientOutdated{}, engine.ClientOutdated{}},
		{&events.StreamReplaced{}, engine.StreamReplaced{}},
		{&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, engine.LoggedOut{}},
		{&events.CATRefreshError{}, engine.CATRefreshFailed{}},
		{&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour}, engine.TemporaryBan{Expire: time.Hour}},
		{&events.ConnectFailure{Reason: events.ConnectFailureReason(409)}, engine.ConnectFailure{Code: 409}},
	} {
		if got := translated(t, tt.in); got != tt.want {
			t.Errorf("%T = %+v, want %+v", tt.in, got, tt.want)
		}
	}
	for _, ignored := range []any{&events.StreamError{Code: "999"}, &events.Receipt{}, &events.Presence{}, &events.HistorySync{}, &events.QR{Codes: []string{"2@x"}}} {
		if ev, ok := translate(ignored); ok {
			t.Errorf("%T became %+v", ignored, ev)
		}
	}
}

func TestGroupEventsTranslate(t *testing.T) {
	change, ok := translated(t, &events.GroupInfo{
		JID: group, Timestamp: epoch, Name: &types.GroupName{Name: "Synthetic group"},
		Join: []types.JID{peer}, Leave: []types.JID{other}, Promote: []types.JID{admin}, Demote: []types.JID{victim},
	}).(engine.Group)
	want := engine.Group{
		Chat: group.String(), Subject: "Synthetic group", Timestamp: epoch, Left: []string{other.String()},
		Joined: []engine.Participant{{User: peer.String()}, {User: victim.String()}, {User: admin.String(), Admin: true}},
	}
	if !ok || !reflect.DeepEqual(change, want) {
		t.Fatalf("group change = %+v, want %+v", change, want)
	}
	if ev, ok := translate(&events.GroupInfo{JID: group, Announce: &types.GroupAnnounce{IsAnnounce: true}}); ok {
		t.Fatalf("a group change the archive does not keep became %+v", ev)
	}
	joined, ok := translated(t, &events.JoinedGroup{GroupInfo: types.GroupInfo{
		JID: group, GroupName: types.GroupName{Name: "Synthetic group"},
		Participants: []types.GroupParticipant{{JID: admin, IsSuperAdmin: true}, {JID: peerLID, IsAdmin: true}, {JID: victim}},
	}}).(engine.Group)
	if !ok || joined.Subject != "Synthetic group" || !reflect.DeepEqual(joined.Members, []engine.Participant{{User: admin.String(), Admin: true}, {User: peerLID.String(), Admin: true}, {User: victim.String()}}) {
		t.Fatalf("joined group = %+v", joined)
	}
}
