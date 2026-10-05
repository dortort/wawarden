package wa

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
)

func decoded(t *testing.T, h *waHistorySync.HistorySync) engine.History {
	t.Helper()
	r := newRig(t, pairedDevice())
	got, err := r.c.Decode(historyBlob(t, h))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return got
}

func TestFixtureHistoryBlobWithALIDMapping(t *testing.T) {
	got := decoded(t, &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{
			conv(peerLID.String(),
				webMessage(peerLID, false, "3EB0HA", &waE2E.Message{Conversation: proto.String("synthetic old text")}, nil),
				webMessage(peerLID, true, "3EB0HB", &waE2E.Message{Conversation: proto.String("synthetic reply")}, nil)),
			{
				ID: proto.String(group.String()), Name: proto.String("Synthetic group"),
				Participant: []*waHistorySync.GroupParticipant{
					{UserJID: proto.String(admin.String()), Rank: waHistorySync.GroupParticipant_ADMIN.Enum()},
					{UserJID: proto.String(victim.String())},
				},
				Messages: []*waHistorySync.HistorySyncMsg{webMessage(group, false, "3EB0HG", &waE2E.Message{Conversation: proto.String("synthetic group text")}, &victim)},
			},
		},
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String(peer.String()), LidJID: proto.String(peerLID.String())}},
		Pushnames:                []*waHistorySync.Pushname{{ID: proto.String(peer.String()), Pushname: proto.String("Synthetic Peer")}, {ID: proto.String(other.String())}},
		ChunkOrder:               proto.Uint32(1),
	})
	if !reflect.DeepEqual(got.LIDMappings, []engine.LIDMapping{{PN: peer.String(), LID: peerLID.String()}}) {
		t.Fatalf("LID mappings %+v", got.LIDMappings)
	}
	if !reflect.DeepEqual(got.Contacts, []engine.Contact{{User: peer.String(), PushName: "Synthetic Peer"}}) {
		t.Fatalf("contacts %+v", got.Contacts)
	}
	if len(got.Conversations) != 2 {
		t.Fatalf("conversations %+v", got.Conversations)
	}
	dm, grp := got.Conversations[0], got.Conversations[1]
	if dm.Chat != peerLID.String() || len(dm.Messages) != 2 || dm.Subject != "" {
		t.Fatalf("direct conversation %+v", dm)
	}
	in, out := dm.Messages[0], dm.Messages[1]
	if in.Kind != engine.KindText || in.Text != "synthetic old text" || in.Sender != peerLID.String() || in.FromMe || in.ID != "3EB0HA" || !in.Timestamp.Equal(epoch) {
		t.Fatalf("incoming history row %+v", in)
	}
	if !out.FromMe || out.Sender != ownerLID.String() || out.Chat != peerLID.String() {
		t.Fatalf("the owner's history row %+v: its sender must be the owner's own identifier on the conversation's server", out)
	}
	if grp.Chat != group.String() || grp.Subject != "Synthetic group" ||
		!reflect.DeepEqual(grp.Members, []engine.Participant{{User: admin.String(), Admin: true}, {User: victim.String()}}) ||
		len(grp.Messages) != 1 || grp.Messages[0].Sender != victim.String() {
		t.Fatalf("group conversation %+v", grp)
	}
}

func TestHistoryRowsNeverFallBackToTheirOwnChat(t *testing.T) {
	got := decoded(t, &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_FULL.Enum(),
		Conversations: []*waHistorySync.Conversation{
			conv("", webMessage(other, false, "3EB0X1", &waE2E.Message{Conversation: proto.String("synthetic misfiled")}, nil)),
			conv("not-a-chat", webMessage(other, false, "3EB0X2", &waE2E.Message{Conversation: proto.String("synthetic misfiled")}, nil)),
			conv(peer.String(), webMessage(other, false, "3EB0X3", &waE2E.Message{Conversation: proto.String("synthetic keyed elsewhere")}, nil)),
		},
	})
	if len(got.Conversations) != 1 {
		t.Fatalf("conversations %+v: one without a usable chat must be skipped, never filed under a row's own key", got.Conversations)
	}
	c := got.Conversations[0]
	if c.Chat != peer.String() || len(c.Messages) != 1 || c.Messages[0].Chat != peer.String() || c.Messages[0].Sender != peer.String() {
		t.Fatalf("a row whose key names another chat = %+v: it belongs to its conversation", c)
	}
}

func TestHistoryEditRowsKeepTheirOwnIdentifier(t *testing.T) {
	edit := &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:           msgKey(peer, false, "3EB0A1", nil),
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String("synthetic, edited")},
	}}}}
	got := decoded(t, &waHistorySync.HistorySync{
		SyncType:      waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{conv(peer.String(), webMessage(peer, false, "3EB0E9", edit, nil))},
	})
	m := got.Conversations[0].Messages[0]
	if m.ID != "3EB0E9" || m.Kind != engine.KindEdit || m.Text != "synthetic, edited" || m.Target == nil || m.Target.ID != "3EB0A1" {
		t.Fatalf("history edit row = %+v (target %+v): it must stay an edit under its own identifier, not become a message colliding with its target", m, m.Target)
	}
}

func TestHistoryRowsCarryTheirExpiry(t *testing.T) {
	row := webMessage(peer, false, "3EB0D9", &waE2E.Message{Conversation: proto.String("synthetic vanishing")}, nil)
	row.Message.EphemeralDuration = proto.Uint32(86400)
	expiring := webMessage(peer, false, "3EB0D8", &waE2E.Message{Conversation: proto.String("synthetic vanishing")}, nil)
	expiring.Message.EphemeralExpirationTimestamp = proto.Uint64(epochSeconds + 3600)
	expired := webMessage(peer, false, "3EB0D7", &waE2E.Message{Conversation: proto.String("synthetic expired")}, nil)
	expired.Message.EphemeralExpirationTimestamp = proto.Uint64(epochSeconds - 3600)
	if epoch.Unix() != epochSeconds {
		t.Fatal("the fixture epoch and its seconds disagree")
	}
	got := decoded(t, &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{conv(peer.String(), row, expiring, expired)}})
	msgs := got.Conversations[0].Messages
	if msgs[0].Expiration != 24*time.Hour || msgs[1].Expiration != time.Hour || msgs[2].Expiration != time.Second {
		t.Fatalf("expirations %v, %v, %v", msgs[0].Expiration, msgs[1].Expiration, msgs[2].Expiration)
	}
}

func TestHistoryWithoutAnOwnIdentityDropsOnlyTheOwnersRows(t *testing.T) {
	r := newRig(t, &store.Device{})
	got, err := r.c.Decode(historyBlob(t, &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{conv(peer.String(),
			webMessage(peer, true, "3EB0O1", &waE2E.Message{Conversation: proto.String("synthetic mine")}, nil),
			webMessage(peer, false, "3EB0O2", &waE2E.Message{Conversation: proto.String("synthetic theirs")}, nil),
			webMessage(group, false, "3EB0O3", nil, nil))},
	}))
	if err != nil || len(got.Conversations[0].Messages) != 1 || got.Conversations[0].Messages[0].ID != "3EB0O2" {
		t.Fatalf("Decode = %+v, %v", got, err)
	}
}

func nested(depth int) []byte {
	b := []byte{}
	for range depth {
		var ctx []byte
		ctx = protowire.AppendTag(ctx, 3, protowire.BytesType)
		ctx = protowire.AppendBytes(ctx, b)
		var ext []byte
		ext = protowire.AppendTag(ext, 17, protowire.BytesType)
		ext = protowire.AppendBytes(ext, ctx)
		var msg []byte
		msg = protowire.AppendTag(msg, 6, protowire.BytesType)
		b = protowire.AppendBytes(msg, ext)
	}
	return b
}

func TestHistoryDecodingIsDepthLimited(t *testing.T) {
	r := newRig(t, pairedDevice())
	for _, tt := range []struct {
		depth int
		ok    bool
	}{{2, true}, {20, false}} {
		msg := nested(tt.depth)
		var web []byte
		web = protowire.AppendTag(web, 1, protowire.BytesType)
		web = protowire.AppendBytes(web, historyBlob(t, msgKey(peer, false, "3EB0N1", nil)))
		web = protowire.AppendTag(web, 2, protowire.BytesType)
		web = protowire.AppendBytes(web, msg)
		var hmsg []byte
		hmsg = protowire.AppendTag(hmsg, 1, protowire.BytesType)
		hmsg = protowire.AppendBytes(hmsg, web)
		var c []byte
		c = protowire.AppendTag(c, 1, protowire.BytesType)
		c = protowire.AppendString(c, peer.String())
		c = protowire.AppendTag(c, 2, protowire.BytesType)
		c = protowire.AppendBytes(c, hmsg)
		blob := historyBlob(t, &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()})
		blob = protowire.AppendTag(blob, 2, protowire.BytesType)
		blob = protowire.AppendBytes(blob, c)
		_, err := r.c.Decode(blob)
		if (err == nil) != tt.ok {
			t.Errorf("Decode of quotes nested %d deep = %v, want success %v", tt.depth, err, tt.ok)
		}
	}
	if _, err := r.c.Decode([]byte("synthetic: not a protocol buffer")); err == nil {
		t.Fatal("Decode accepted garbage")
	}
}

func TestTheStagingFileEnforcesItsCapOnEveryWritePath(t *testing.T) {
	var f any = newCapFile(64, 0)
	if _, ok := f.(io.ReaderFrom); ok {
		t.Fatal("the staging file implements ReadFrom, which io.Copy would prefer over its capped Write")
	}
	if _, ok := f.(io.WriterTo); ok {
		t.Fatal("the staging file implements WriteTo")
	}
	big := bytes.Repeat([]byte("x"), 65)
	if _, err := io.Copy(newCapFile(64, 0), bytes.NewReader(big)); !errors.Is(err, errTooLarge) {
		t.Fatalf("io.Copy from a WriterTo source = %v, want errTooLarge", err)
	}
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	src, err := os.Open(path) //nolint:gosec // G304: the test's own temporary file
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = src.Close() }()
	if _, err := io.Copy(newCapFile(64, 0), src); !errors.Is(err, errTooLarge) {
		t.Fatalf("io.Copy from a file = %v, want errTooLarge", err)
	}
	c := newCapFile(64, 1<<40)
	if cap(c.buf) > 64 {
		t.Fatalf("an announced length over the cap reserved %d bytes", cap(c.buf))
	}
	if _, err := c.WriteAt([]byte("x"), 64); !errors.Is(err, errTooLarge) {
		t.Fatalf("WriteAt past the cap = %v", err)
	}
	if err := c.Truncate(65); !errors.Is(err, errTooLarge) {
		t.Fatalf("Truncate past the cap = %v", err)
	}
	if _, err := c.WriteAt([]byte("x"), -1); !errors.Is(err, errOffset) {
		t.Fatalf("WriteAt at a negative offset = %v", err)
	}
	if _, err := c.Seek(-1, io.SeekStart); !errors.Is(err, errOffset) {
		t.Fatalf("Seek before the start = %v", err)
	}
}

func TestTheStagingFileBehavesLikeAFileForInPlaceDecryption(t *testing.T) {
	f := newCapFile(1<<10, 32)
	if _, err := f.Write([]byte("0123456789abcdef0123456789ABCDEF")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != 32 || fi.IsDir() {
		t.Fatalf("Stat = %v, %v", fi, err)
	}
	tail := make([]byte, 4)
	if _, err := f.ReadAt(tail, 28); err != nil || string(tail) != "CDEF" {
		t.Fatalf("ReadAt = %q, %v", tail, err)
	}
	if pos, err := f.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("Seek = %d, %v", pos, err)
	}
	block := make([]byte, 16)
	if _, err := io.ReadFull(f, block); err != nil || string(block) != "0123456789abcdef" {
		t.Fatalf("ReadFull = %q, %v", block, err)
	}
	if _, err := f.WriteAt(bytes.ToUpper(block), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := f.Truncate(20); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := string(f.bytes()); got != "0123456789ABCDEF0123" {
		t.Fatalf("content %q", got)
	}
	if err := f.Truncate(24); err != nil || string(f.bytes()[20:]) != "\x00\x00\x00\x00" {
		t.Fatalf("growing by Truncate = %q, %v, want zeros", f.bytes()[20:], err)
	}
	if pos, err := f.Seek(-4, io.SeekEnd); err != nil || pos != 20 {
		t.Fatalf("Seek from the end = %d, %v", pos, err)
	}
	if n, err := f.Read(make([]byte, 8)); n != 4 || err != nil {
		t.Fatalf("Read at the end = %d, %v", n, err)
	}
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read past the end = %v, want EOF", err)
	}
	if _, err := f.Seek(0, 7); !errors.Is(err, errOffset) {
		t.Fatalf("Seek with an unknown origin = %v", err)
	}
	if _, err := f.WriteAt([]byte("z"), 30); err != nil || f.bytes()[29] != 0 || f.bytes()[30] != 'z' {
		t.Fatalf("a write past the end leaves %q, %v", f.bytes()[24:], err)
	}
}

const epochSeconds = 1791104400

func webMessage(chat types.JID, fromMe bool, id string, msg *waE2E.Message, participant *types.JID) *waHistorySync.HistorySyncMsg {
	w := &waWeb.WebMessageInfo{
		Key:              msgKey(chat, fromMe, id, participant),
		Message:          msg,
		MessageTimestamp: proto.Uint64(epochSeconds),
		PushName:         proto.String("Synthetic Peer"),
	}
	if participant != nil {
		w.Participant = proto.String(participant.String())
	}
	return &waHistorySync.HistorySyncMsg{Message: w, MsgOrderID: proto.Uint64(1)}
}

func historyBlob(t testing.TB, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

func compress(t testing.TB, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func conv(id string, msgs ...*waHistorySync.HistorySyncMsg) *waHistorySync.Conversation {
	return &waHistorySync.Conversation{ID: proto.String(id), Messages: msgs}
}
