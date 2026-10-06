package wa

import (
	"bytes"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	epoch     = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	owner     = types.NewJID("15550100009", types.DefaultUserServer)
	peer      = types.NewJID("15550100001", types.DefaultUserServer)
	peerLID   = types.NewJID("100000000000001", types.HiddenUserServer)
	admin     = types.NewJID("15550100002", types.DefaultUserServer)
	victim    = types.NewJID("15550100003", types.DefaultUserServer)
	other     = types.NewJID("15550100004", types.DefaultUserServer)
	group     = types.NewJID("120363000000000001", types.GroupServer)
	channel   = types.NewJID("120363000000000002", types.NewsletterServer)
	companion = types.JID{User: owner.User, Device: 5, Server: types.DefaultUserServer}
)

func wire[M proto.Message](t testing.TB, m M) M {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := m.ProtoReflect().New().Interface().(M)
	if err := (proto.UnmarshalOptions{RecursionLimit: messageDepth}).Unmarshal(b, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return out
}

func msgKey(chat types.JID, fromMe bool, id string, participant *types.JID) *waCommon.MessageKey {
	k := &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(fromMe), ID: proto.String(id)}
	if participant != nil {
		k.Participant = proto.String(participant.String())
	}
	return k
}

func live(t testing.TB, chat, sender types.JID, id string, raw *waE2E.Message) *events.Message {
	t.Helper()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer},
			ID:            id,
			Timestamp:     epoch,
			PushName:      "Synthetic Peer",
		},
		RawMessage: wire(t, raw),
	}
	return evt.UnwrapRaw()
}

func textMessage(t testing.TB, chat, sender types.JID, id, body string) *events.Message {
	return live(t, chat, sender, id, &waE2E.Message{Conversation: proto.String(body)})
}

func contactEvent(t testing.TB, jid types.JID, action *waSyncAction.ContactAction) *events.Contact {
	t.Helper()
	return &events.Contact{JID: jid, Timestamp: epoch, Action: wire(t, action)}
}

func editMessage(t testing.TB, chat, sender types.JID, id string, key *waCommon.MessageKey, body string) *events.Message {
	evt := live(t, chat, sender, id, editContent(key, body))
	evt.Info.Edit = types.EditAttributeMessageEdit
	return evt
}

func editContent(key *waCommon.MessageKey, body string) *waE2E.Message {
	return &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:           key,
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String(body)},
		TimestampMS:   proto.Int64(epoch.UnixMilli()),
	}}}}
}

func resent(t testing.TB, cli *whatsmeow.Client, chat types.JID, fromMe bool, id string, raw *waE2E.Message) *events.Message {
	t.Helper()
	web := wire(t, &waWeb.WebMessageInfo{
		Key:              msgKey(chat, fromMe, id, nil),
		Message:          raw,
		MessageTimestamp: proto.Uint64(epochSeconds),
		PushName:         proto.String("Synthetic Peer"),
	})
	evt, err := cli.ParseWebMessage(types.EmptyJID, web)
	if err != nil {
		t.Fatalf("ParseWebMessage: %v", err)
	}
	evt.UnavailableRequestID = "3EB0RQ1"
	return evt
}

func revokeMessage(t testing.TB, chat, sender types.JID, id string, key *waCommon.MessageKey, edit types.EditAttribute) *events.Message {
	evt := live(t, chat, sender, id, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: key}})
	evt.Info.Edit = edit
	return evt
}

func historyNotification(t testing.TB, sender types.JID, id string, inline []byte) *events.Message {
	n := &waE2E.HistorySyncNotification{
		FileSHA256:    bytes.Repeat([]byte{1}, 32),
		FileEncSHA256: bytes.Repeat([]byte{2}, 32),
		MediaKey:      bytes.Repeat([]byte{3}, 32),
		FileLength:    proto.Uint64(1 << 20),
		DirectPath:    proto.String("/v/t62.7118-24/synthetic"),
		SyncType:      waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum(),
		ChunkOrder:    proto.Uint32(1),
	}
	if inline != nil {
		n.DirectPath, n.FileLength = nil, nil
		n.InitialHistBootstrapInlinePayload = inline
	}
	evt := live(t, owner, sender, id, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:                    waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: n,
	}})
	evt.Info.IsFromMe = true
	return evt
}
