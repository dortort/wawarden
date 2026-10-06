package engine

import (
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

type outcome struct {
	inserted  int
	dropped   []string
	conflicts []ingest.Conflict
}

type applier struct {
	tx    *ingest.Tx
	owner policy.CanonicalChat
	now   time.Time
	out   outcome
}

func (a *applier) drop(reason string) { a.out.dropped = append(a.out.dropped, reason) }

func userChat(c policy.CanonicalChat) bool {
	return c.Kind() == policy.PhoneChat || c.Kind() == policy.LIDChat
}

func normalizeUser(raw string) (policy.CanonicalChat, bool) {
	c, ok := policy.Normalize(raw)
	return c, ok && userChat(c)
}

func (a *applier) message(m Message, origin ingest.Origin) error {
	chat, ok := policy.Normalize(m.Chat)
	if !ok {
		a.drop(dropChat)
		return nil
	}
	sender, ok := normalizeUser(m.Sender)
	if !ok {
		a.drop(dropSender)
		return nil
	}
	if origin == ingest.OriginLive {
		if err := a.learnAlternates(m, chat, sender); err != nil {
			return err
		}
	}
	chat, err := a.tx.Canonical(chat)
	if err != nil {
		return err
	}
	switch m.Kind {
	case KindText, KindMedia, KindOther:
		err = a.insert(m, chat, sender, origin)
	case KindReaction, KindPollUpdate:
		err = a.react(m, chat, sender, origin)
	case KindEdit:
		err = a.edit(m, chat, sender)
	case KindRevoke:
		err = a.revoke(m, chat, sender)
	default:
		a.drop(dropUnknownKind)
		return nil
	}
	if err != nil || m.FromMe || m.PushName == "" {
		return err
	}
	if err := a.tx.SetPushName(sender, m.PushName, m.Timestamp, origin); err != nil {
		return err
	}
	if userChat(chat) {
		return a.tx.SetChatName(chat, m.PushName, ingest.NamePushName, origin)
	}
	return nil
}

func (a *applier) learnAlternates(m Message, chat, sender policy.CanonicalChat) error {
	if err := a.learn(sender, m.SenderAlt, ingest.MappingSenderAlt); err != nil {
		return err
	}
	if m.FromMe && userChat(chat) {
		return a.learn(chat, m.RecipientAlt, ingest.MappingRecipientAlt)
	}
	return nil
}

func (a *applier) learn(primary policy.CanonicalChat, rawAlt string, source ingest.MappingSource) error {
	alt, ok := normalizeUser(rawAlt)
	if !ok {
		return nil
	}
	lid, pn := primary, alt
	if lid.Kind() != policy.LIDChat {
		lid, pn = alt, primary
	}
	if lid.Kind() != policy.LIDChat || pn.Kind() != policy.PhoneChat {
		return nil
	}
	return a.mapping(lid, pn, source)
}

func (a *applier) mapping(lid, pn policy.CanonicalChat, source ingest.MappingSource) error {
	res, err := a.tx.LearnLID(lid, pn, source, a.now)
	if err == nil && res.Outcome == ingest.LIDConflict {
		a.out.conflicts = append(a.out.conflicts, res.Conflict)
	}
	return err
}

func storedKind(k Kind) ingest.Kind {
	switch k {
	case KindText:
		return ingest.KindText
	case KindMedia:
		return ingest.KindMedia
	case KindReaction:
		return ingest.KindReaction
	case KindPollUpdate:
		return ingest.KindPollUpdate
	}
	return ingest.KindOther
}

func (a *applier) row(m Message, chat, sender policy.CanonicalChat, origin ingest.Origin) ingest.Message {
	row := ingest.Message{
		Chat: chat, ID: m.ID, Sender: sender, FromMe: m.FromMe, Origin: origin, Timestamp: m.Timestamp,
		Kind: storedKind(m.Kind), Ingested: a.now,
	}
	if alt, ok := normalizeUser(m.SenderAlt); ok {
		row.SenderAlt = alt
	}
	switch m.Addressing {
	case string(ingest.AddressingPN):
		row.Addressing = ingest.AddressingPN
	case string(ingest.AddressingLID):
		row.Addressing = ingest.AddressingLID
	}
	return row
}

func (a *applier) store(row ingest.Message) error {
	_, inserted, err := a.tx.InsertMessage(row)
	if inserted {
		a.out.inserted++
	}
	return err
}

func (a *applier) insert(m Message, chat, sender policy.CanonicalChat, origin ingest.Origin) error {
	row := a.row(m, chat, sender, origin)
	row.Text, row.MediaType = m.Text, m.MediaType
	if m.Expiration > 0 {
		row.ExpiresAt = m.Timestamp.Add(m.Expiration)
	}
	if m.Reply != nil && m.Reply.ID != "" {
		quote, err := a.quote(m, chat)
		if err != nil {
			return err
		}
		row.Quote = quote
	}
	return a.store(row)
}

func (a *applier) quote(m Message, chat policy.CanonicalChat) (*ingest.Quote, error) {
	same, err := a.sameChat(m.Reply.RemoteJID, chat, m.FromMe)
	if err != nil || !same {
		if err == nil {
			a.drop(dropForeign)
		}
		return nil, err
	}
	quoted, ok := normalizeUser(m.Reply.Participant)
	if !ok {
		return nil, nil
	}
	found, ok, err := a.resolve(chat, m.Reply.ID, quoted)
	if err != nil || !ok {
		return nil, err
	}
	return &ingest.Quote{Ref: found.Ref, Verified: !found.Revoked && found.Text == m.Reply.Text}, nil
}

func (a *applier) resolve(chat policy.CanonicalChat, id string, sender policy.CanonicalChat) (ingest.Found, bool, error) {
	found, ok, err := a.tx.ResolveInChat(chat, id, sender)
	if errors.Is(err, ingest.ErrInvalid) {
		return ingest.Found{}, false, nil
	}
	return found, ok, err
}

func (a *applier) sameChat(rawRemote string, chat policy.CanonicalChat, fromMe bool) (bool, error) {
	if rawRemote == "" {
		return true, nil
	}
	remote, ok := policy.Normalize(rawRemote)
	if !ok {
		return false, nil
	}
	remote, err := a.tx.Canonical(remote)
	if err != nil || remote == chat {
		return err == nil, err
	}
	if !userChat(chat) || fromMe || !a.owner.Valid() {
		return false, nil
	}
	owner, err := a.tx.Canonical(a.owner)
	return err == nil && remote == owner, err
}

func (a *applier) target(m Message, chat, sender policy.CanonicalChat) (ingest.Found, bool, error) {
	key := m.Target
	if key == nil || key.ID == "" {
		a.drop(dropNoTarget)
		return ingest.Found{}, false, nil
	}
	same, err := a.sameChat(key.RemoteJID, chat, m.FromMe)
	if err != nil || !same {
		if err == nil {
			a.drop(dropForeign)
		}
		return ingest.Found{}, false, err
	}
	var original policy.CanonicalChat
	switch {
	case key.FromMe:
		original = sender
	case chat.Kind() == policy.GroupChat:
		if original, same = normalizeUser(key.Participant); !same {
			a.drop(dropNoTarget)
			return ingest.Found{}, false, nil
		}
	case m.FromMe:
		original = chat
	case a.owner.Valid():
		original = a.owner
	default:
		a.drop(dropOwnerUnknown)
		return ingest.Found{}, false, nil
	}
	found, ok, err := a.resolve(chat, key.ID, original)
	if err == nil && !ok {
		a.drop(dropTargetAbsent)
	}
	return found, ok, err
}

func (a *applier) react(m Message, chat, sender policy.CanonicalChat, origin ingest.Origin) error {
	found, ok, err := a.target(m, chat, sender)
	if err != nil || !ok {
		return err
	}
	row := a.row(m, chat, sender, origin)
	row.Quote = &ingest.Quote{Ref: found.Ref, Verified: true}
	return a.store(row)
}

func (a *applier) original(found ingest.Found, sender policy.CanonicalChat) (bool, error) {
	sender, err := a.tx.Canonical(sender)
	return err == nil && sender == found.Sender, err
}

func modifiable(k ingest.Kind) bool {
	return k != ingest.KindReaction && k != ingest.KindPollUpdate
}

func (a *applier) edit(m Message, chat, sender policy.CanonicalChat) error {
	if m.Text == "" || m.Timestamp.IsZero() {
		a.drop(dropInvalid)
		return nil
	}
	found, ok, err := a.target(m, chat, sender)
	if err != nil || !ok {
		return err
	}
	if found.Revoked || !modifiable(found.Kind) {
		a.drop(dropTargetKind)
		return nil
	}
	if same, err := a.original(found, sender); err != nil || !same {
		if err == nil {
			a.drop(dropNotOriginal)
		}
		return err
	}
	if !found.EditedAt.IsZero() && m.Timestamp.UnixMilli() <= found.EditedAt.UnixMilli() {
		a.drop(dropStaleEdit)
		return nil
	}
	return a.tx.ApplyEdit(found.Ref, m.Text, m.Timestamp)
}

func (a *applier) revoke(m Message, chat, sender policy.CanonicalChat) error {
	found, ok, err := a.target(m, chat, sender)
	if err != nil || !ok {
		return err
	}
	same, err := a.original(found, sender)
	if err != nil {
		return err
	}
	if !same {
		if chat.Kind() != policy.GroupChat {
			a.drop(dropNotOriginal)
			return nil
		}
		admin, known, err := a.tx.ParticipantAdmin(chat, sender)
		switch {
		case err != nil:
			return err
		case !known:
			a.drop(dropAdminUnknown)
			return nil
		case !admin:
			a.drop(dropNotAdmin)
			return nil
		}
	}
	return a.tx.ApplyRevoke(found.Ref)
}

func (a *applier) group(g Group, origin ingest.Origin) error {
	chat, ok := policy.Normalize(g.Chat)
	if !ok || chat.Kind() != policy.GroupChat {
		a.drop(dropChat)
		return nil
	}
	if g.Subject != "" {
		if err := a.tx.SetChatName(chat, g.Subject, ingest.NameGroupSubject, origin); err != nil {
			return err
		}
	}
	if len(g.Members) > 0 {
		members, err := a.participants(g.Members)
		if err != nil {
			return err
		}
		if err := a.tx.ReplaceParticipants(chat, members, origin); err != nil {
			return err
		}
	}
	for _, p := range g.Joined {
		if u, ok := normalizeUser(p.User); ok {
			if err := a.tx.SetParticipant(chat, u, p.Admin); err != nil {
				return err
			}
		}
	}
	for _, raw := range g.Left {
		if u, ok := normalizeUser(raw); ok {
			if err := a.tx.RemoveParticipant(chat, u); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *applier) participants(in []Participant) ([]ingest.Participant, error) {
	seen := map[policy.CanonicalChat]bool{}
	var out []ingest.Participant
	for _, p := range in {
		u, ok := normalizeUser(p.User)
		if !ok {
			continue
		}
		canonical, err := a.tx.Canonical(u)
		if err != nil {
			return nil, err
		}
		if !seen[canonical] {
			seen[canonical] = true
			out = append(out, ingest.Participant{User: canonical, Admin: p.Admin})
		}
	}
	return out, nil
}
