package wa

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
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

func onDemandNotification(t testing.TB, sender types.JID, id, request string, inline []byte) *events.Message {
	evt := historyNotification(t, sender, id, inline)
	n := evt.Message.GetProtocolMessage().GetHistorySyncNotification()
	n.SyncType = waE2E.HistorySyncType_ON_DEMAND.Enum()
	n.PeerDataRequestSessionID = proto.String(request)
	return evt
}

var errSyntheticSeal = errors.New("synthetic: the sealer refuses")

type fakeSealer struct{ refuse bool }

func (s fakeSealer) SealMedia(id string, pt []byte) (string, error) {
	if s.refuse {
		return "", errSyntheticSeal
	}
	return "sealed." + hex.EncodeToString([]byte(id)) + "." + hex.EncodeToString(pt), nil
}

func (fakeSealer) OpenMedia(id, text string) ([]byte, error) {
	body, ok := strings.CutPrefix(text, "sealed."+hex.EncodeToString([]byte(id))+".")
	if !ok {
		return nil, errSyntheticSeal
	}
	return hex.DecodeString(body)
}

func randomBytes(n int64) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

type mediaFields struct {
	path          string
	key, sha, enc []byte
	length        int64
}

func (f mediaFields) fileLength() *uint64 {
	if f.length < 0 {
		return nil
	}
	return proto.Uint64(uint64(f.length))
}

func syntheticMedia(length int64) mediaFields {
	return mediaFields{path: "/example/" + hex.EncodeToString(randomBytes(8)), key: randomBytes(32), sha: randomBytes(32), enc: randomBytes(32), length: length}
}

type mediaFixture struct {
	name     string
	kind     string
	variant  string
	build    func(mediaFields) *waE2E.Message
	viewOnce func(*waE2E.Message)
}

var mediaFixtures = []mediaFixture{
	{name: "an image", kind: "image", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String("synthetic caption"),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}, viewOnce: func(m *waE2E.Message) { m.ImageMessage.ViewOnce = proto.Bool(true) }},
	{name: "a video", kind: "video", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}, viewOnce: func(m *waE2E.Message) { m.VideoMessage.ViewOnce = proto.Bool(true) }},
	{name: "a GIF", kind: "video", variant: "gif", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), GifPlayback: proto.Bool(true),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}, viewOnce: func(m *waE2E.Message) { m.VideoMessage.ViewOnce = proto.Bool(true) }},
	{name: "a video note", kind: "video", variant: "video_note", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}, viewOnce: func(m *waE2E.Message) { m.PtvMessage.ViewOnce = proto.Bool(true) }},
	{name: "an audio", kind: "audio", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/mpeg"),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}, viewOnce: func(m *waE2E.Message) { m.AudioMessage.ViewOnce = proto.Bool(true) }},
	{name: "a voice note", kind: "audio", variant: "voice_note", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus"), PTT: proto.Bool(true),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}, viewOnce: func(m *waE2E.Message) { m.AudioMessage.ViewOnce = proto.Bool(true) }},
	{name: "a document", kind: "document", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"), FileName: proto.String("synthetic.pdf"),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}},
	{name: "a sticker", kind: "sticker", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}},
	{name: "an animated sticker", kind: "sticker", variant: "animated", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"), IsAnimated: proto.Bool(true),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}},
	{name: "a Lottie sticker", kind: "sticker", variant: "lottie", build: func(f mediaFields) *waE2E.Message {
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("application/was"), IsAnimated: proto.Bool(true), IsLottie: proto.Bool(true),
			DirectPath: proto.String(f.path), MediaKey: f.key, FileSHA256: f.sha, FileEncSHA256: f.enc, FileLength: f.fileLength()}}
	}},
}

func viewOnceWrapped(m *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: m}}
}
