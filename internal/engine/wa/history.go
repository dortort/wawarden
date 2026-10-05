package wa

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
)

const (
	historyDepth  = messageDepth + 4
	mediaOverhead = 32
)

var historyOptions = proto.UnmarshalOptions{RecursionLimit: historyDepth, DiscardUnknown: true}

var (
	errTooLarge   = errors.New("wa: the history blob is larger than WAWARDEN_HISTORY_MAX_BYTES")
	errOffset     = errors.New("wa: negative offset")
	errHistoryRef = errors.New("wa: the history notification lacks a download path, media key or hash")
)

func decodeHistory(cli *whatsmeow.Client, blob []byte) (engine.History, error) {
	var h waHistorySync.HistorySync
	if err := historyOptions.Unmarshal(blob, &h); err != nil {
		return engine.History{}, fmt.Errorf("wa: decode the history blob: %w", err)
	}
	var out engine.History
	for _, m := range h.GetPhoneNumberToLidMappings() {
		out.LIDMappings = append(out.LIDMappings, engine.LIDMapping{PN: m.GetPnJID(), LID: m.GetLidJID()})
	}
	for _, p := range h.GetPushnames() {
		if p.GetID() != "" && p.GetPushname() != "" {
			out.Contacts = append(out.Contacts, engine.Contact{User: p.GetID(), PushName: p.GetPushname()})
		}
	}
	for _, c := range h.GetConversations() {
		if pn, lid := c.GetPnJID(), c.GetLidJID(); pn != "" && lid != "" {
			out.LIDMappings = append(out.LIDMappings, engine.LIDMapping{PN: pn, LID: lid})
		}
		if conv, ok := conversation(cli, c); ok {
			out.Conversations = append(out.Conversations, conv)
		}
	}
	return out, nil
}

func conversation(cli *whatsmeow.Client, c *waHistorySync.Conversation) (engine.Conversation, bool) {
	chat, err := types.ParseJID(c.GetID())
	if err != nil || chat.User == "" || chat.Server == "" {
		return engine.Conversation{}, false
	}
	conv := engine.Conversation{Chat: chat.String()}
	if chat.Server == types.GroupServer {
		conv.Subject = c.GetName()
		for _, p := range c.GetParticipant() {
			rank := p.GetRank()
			conv.Members = append(conv.Members, engine.Participant{User: p.GetUserJID(), Admin: rank == waHistorySync.GroupParticipant_ADMIN || rank == waHistorySync.GroupParticipant_SUPERADMIN})
		}
	}
	for _, hm := range c.GetMessages() {
		if m, ok := historyMessage(cli, chat, hm.GetMessage()); ok {
			conv.Messages = append(conv.Messages, m)
		}
	}
	return conv, true
}

func historyMessage(cli *whatsmeow.Client, chat types.JID, web *waWeb.WebMessageInfo) (engine.Message, bool) {
	if web.GetMessage() == nil {
		return engine.Message{}, false
	}
	evt, err := cli.ParseWebMessage(chat, web)
	if err != nil {
		return engine.Message{}, false
	}
	evt.Info.ID = web.GetKey().GetID()
	evt.UnwrapRaw()
	if evt.Message.GetProtocolMessage().GetHistorySyncNotification() != nil {
		return engine.Message{}, false
	}
	m, ok := content(evt.Info, evt.Message)
	if !ok {
		return engine.Message{}, false
	}
	m.Chat = chat.String()
	if m.Expiration == 0 {
		m.Expiration = historyExpiration(web, m.Timestamp)
	}
	return m, true
}

func historyExpiration(web *waWeb.WebMessageInfo, sent time.Time) time.Duration {
	if at := web.GetEphemeralExpirationTimestamp(); at > 0 && at <= uint64(maxUnixSeconds) {
		return max(time.Unix(int64(at), 0).Sub(sent), time.Second)
	}
	return time.Duration(web.GetEphemeralDuration()) * time.Second
}

const maxUnixSeconds = 1 << 40

type capFile struct {
	buf []byte
	off int64
	max int64
}

func newCapFile(limit int64, hint int64) *capFile {
	return &capFile{buf: make([]byte, 0, min(hint+mediaOverhead, limit)), max: limit}
}

func (f *capFile) bytes() []byte { return f.buf }

func (f *capFile) Write(p []byte) (int, error) {
	n, err := f.WriteAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *capFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errOffset
	}
	end := off + int64(len(p))
	if end > f.max {
		return 0, errTooLarge
	}
	f.grow(end)
	copy(f.buf[off:], p)
	return len(p), nil
}

func (f *capFile) grow(end int64) {
	if end <= int64(len(f.buf)) {
		return
	}
	if end > int64(cap(f.buf)) {
		next := make([]byte, len(f.buf), max(end, min(2*int64(cap(f.buf)), f.max)))
		copy(next, f.buf)
		f.buf = next
	}
	old := len(f.buf)
	f.buf = f.buf[:end]
	clear(f.buf[old:])
}

func (f *capFile) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	if n > 0 {
		return n, nil
	}
	return n, err
}

func (f *capFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errOffset
	}
	if off >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *capFile) Seek(offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.off
	case io.SeekEnd:
		base = int64(len(f.buf))
	default:
		return 0, errOffset
	}
	if base+offset < 0 {
		return 0, errOffset
	}
	f.off = base + offset
	return f.off, nil
}

func (f *capFile) Truncate(size int64) error {
	switch {
	case size < 0:
		return errOffset
	case size > f.max:
		return errTooLarge
	case size <= int64(len(f.buf)):
		f.buf = f.buf[:size]
	default:
		f.grow(size)
	}
	return nil
}

func (f *capFile) Stat() (fs.FileInfo, error) { return fileInfo(len(f.buf)), nil }

type fileInfo int64

func (fileInfo) Name() string       { return "history" }
func (i fileInfo) Size() int64      { return int64(i) }
func (fileInfo) Mode() fs.FileMode  { return 0o600 }
func (fileInfo) ModTime() time.Time { return time.Time{} }
func (fileInfo) IsDir() bool        { return false }
func (fileInfo) Sys() any           { return nil }
