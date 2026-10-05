package wa

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/dortort/wawarden/internal/engine"
)

var mimeEssence = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$`)

const (
	maxMediaType = 64
	messageDepth = 24
)

func translate(evt any) (engine.Event, bool) {
	switch e := evt.(type) {
	case *events.Message:
		return message(e)
	case *events.GroupInfo:
		return groupChange(e)
	case *events.JoinedGroup:
		return joinedGroup(e)
	case *events.Connected:
		return engine.Connected{}, true
	case *events.Disconnected:
		return engine.Disconnected{}, true
	case *events.ClientOutdated:
		return engine.ClientOutdated{}, true
	case *events.StreamReplaced:
		return engine.StreamReplaced{}, true
	case *events.LoggedOut:
		return engine.LoggedOut{}, true
	case *events.CATRefreshError:
		return engine.CATRefreshFailed{}, true
	case *events.TemporaryBan:
		return engine.TemporaryBan{Expire: e.Expire}, true
	case *events.ConnectFailure:
		return engine.ConnectFailure{Code: int(e.Reason)}, true
	case *events.PairSuccess:
		return engine.Paired{JID: types.NewJID(e.ID.User, e.ID.Server).String()}, true
	case *events.PairError:
		if errors.Is(e.Error, whatsmeow.ErrPairRejectedLocally) {
			return engine.PairRejected{}, true
		}
	}
	return nil, false
}

func message(e *events.Message) (engine.Event, bool) {
	if e.Message == nil {
		return nil, false
	}
	if n := e.Message.GetProtocolMessage().GetHistorySyncNotification(); n != nil {
		return engine.HistoryNotification{Sender: e.Info.Sender.String(), FromMe: e.Info.IsFromMe, Ref: historyRef(e.Info.ID, n)}, true
	}
	m, ok := content(e.Info, e.Message)
	if !ok {
		return nil, false
	}
	return m, true
}

func historyRef(id string, n *waE2E.HistorySyncNotification) engine.HistoryRef {
	return engine.HistoryRef{
		ID:            id,
		DirectPath:    n.GetDirectPath(),
		MediaKey:      n.GetMediaKey(),
		FileSHA256:    n.GetFileSHA256(),
		FileEncSHA256: n.GetFileEncSHA256(),
		FileLength:    n.GetFileLength(),
		Inline:        n.GetInitialHistBootstrapInlinePayload(),
	}
}

func content(info types.MessageInfo, m *waE2E.Message) (engine.Message, bool) {
	out := engine.Message{
		Chat:       info.Chat.String(),
		ID:         info.ID,
		Sender:     info.Sender.String(),
		FromMe:     info.IsFromMe,
		Timestamp:  info.Timestamp,
		PushName:   info.PushName,
		Addressing: string(info.AddressingMode),
	}
	if !info.SenderAlt.IsEmpty() {
		out.SenderAlt = info.SenderAlt.String()
	}
	if !info.RecipientAlt.IsEmpty() {
		out.RecipientAlt = info.RecipientAlt.String()
	}
	if pm := m.GetProtocolMessage(); pm != nil {
		return protocol(out, pm)
	}
	switch {
	case m.GetReactionMessage() != nil:
		out.Kind, out.Target = engine.KindReaction, key(m.GetReactionMessage().GetKey())
		return out, true
	case m.GetEncReactionMessage() != nil:
		out.Kind, out.Target = engine.KindReaction, key(m.GetEncReactionMessage().GetTargetMessageKey())
		return out, true
	case m.GetPollUpdateMessage() != nil:
		out.Kind, out.Target = engine.KindPollUpdate, key(m.GetPollUpdateMessage().GetPollCreationMessageKey())
		return out, true
	}
	if kind, mime, ok := media(m); ok {
		out.Kind, out.MediaType, out.Text = engine.KindMedia, mediaType(mime, kind), text(m)
	} else if m.Conversation != nil || m.GetExtendedTextMessage() != nil {
		out.Kind, out.Text = engine.KindText, text(m)
	} else if hasContent(m) {
		out.Kind = engine.KindOther
	} else {
		return engine.Message{}, false
	}
	if ci := contextInfo(m); ci != nil {
		if id := ci.GetStanzaID(); id != "" {
			out.Reply = &engine.Reply{ID: id, Participant: ci.GetParticipant(), RemoteJID: ci.GetRemoteJID(), Text: text(ci.GetQuotedMessage())}
		}
		if exp := ci.GetExpiration(); exp > 0 {
			out.Expiration = time.Duration(exp) * time.Second
		}
	}
	return out, true
}

func protocol(out engine.Message, pm *waE2E.ProtocolMessage) (engine.Message, bool) {
	if pm.Type == nil {
		return engine.Message{}, false
	}
	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		out.Kind, out.Target = engine.KindRevoke, key(pm.GetKey())
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		out.Kind, out.Target, out.Text = engine.KindEdit, key(pm.GetKey()), text(pm.GetEditedMessage())
	default:
		return engine.Message{}, false
	}
	return out, true
}

func key(k *waCommon.MessageKey) *engine.Key {
	if k == nil {
		return nil
	}
	return &engine.Key{RemoteJID: k.GetRemoteJID(), FromMe: k.GetFromMe(), ID: k.GetID(), Participant: k.GetParticipant()}
}

func text(m *waE2E.Message) string {
	switch {
	case m == nil:
		return ""
	case m.Conversation != nil:
		return m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption()
	case m.GetPtvMessage() != nil:
		return m.GetPtvMessage().GetCaption()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption()
	}
	return ""
}

func media(m *waE2E.Message) (kind, mime string, ok bool) {
	switch {
	case m.GetImageMessage() != nil:
		return "image", m.GetImageMessage().GetMimetype(), true
	case m.GetVideoMessage() != nil:
		return "video", m.GetVideoMessage().GetMimetype(), true
	case m.GetPtvMessage() != nil:
		return "video", m.GetPtvMessage().GetMimetype(), true
	case m.GetAudioMessage() != nil:
		return "audio", m.GetAudioMessage().GetMimetype(), true
	case m.GetDocumentMessage() != nil:
		return "document", m.GetDocumentMessage().GetMimetype(), true
	case m.GetStickerMessage() != nil:
		return "sticker", m.GetStickerMessage().GetMimetype(), true
	}
	return "", "", false
}

func mediaType(mime, kind string) string {
	essence, _, _ := strings.Cut(mime, ";")
	essence = strings.ToLower(strings.TrimSpace(essence))
	if len(essence) <= maxMediaType && mimeEssence.MatchString(essence) {
		return essence
	}
	return kind
}

var protocolOnly = map[protoreflect.Name]bool{"senderKeyDistributionMessage": true, "messageContextInfo": true}

func hasContent(m *waE2E.Message) bool {
	found := false
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		found = !protocolOnly[fd.Name()]
		return !found
	})
	return found
}

type contextual interface{ GetContextInfo() *waE2E.ContextInfo }

func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	var ci *waE2E.ContextInfo
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return true
		}
		if c, ok := v.Message().Interface().(contextual); ok {
			ci = c.GetContextInfo()
		}
		return ci == nil
	})
	return ci
}

func groupChange(e *events.GroupInfo) (engine.Event, bool) {
	g := engine.Group{Chat: e.JID.String(), Timestamp: e.Timestamp}
	if e.Name != nil {
		g.Subject = e.Name.Name
	}
	for _, j := range e.Join {
		g.Joined = append(g.Joined, engine.Participant{User: j.String()})
	}
	for _, j := range e.Demote {
		g.Joined = append(g.Joined, engine.Participant{User: j.String()})
	}
	for _, j := range e.Promote {
		g.Joined = append(g.Joined, engine.Participant{User: j.String(), Admin: true})
	}
	for _, j := range e.Leave {
		g.Left = append(g.Left, j.String())
	}
	if g.Subject == "" && len(g.Joined) == 0 && len(g.Left) == 0 {
		return nil, false
	}
	return g, true
}

func joinedGroup(e *events.JoinedGroup) (engine.Event, bool) {
	g := engine.Group{Chat: e.JID.String(), Subject: e.Name}
	for _, p := range e.Participants {
		g.Members = append(g.Members, engine.Participant{User: p.JID.String(), Admin: p.IsAdmin || p.IsSuperAdmin})
	}
	if g.Subject == "" && len(g.Members) == 0 {
		return nil, false
	}
	return g, true
}
