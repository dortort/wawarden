//go:build dev

package fake

import (
	"time"

	"github.com/dortort/wawarden/internal/engine"
)

const (
	server       = "@s.whatsapp.net"
	linkedDevice = "7"

	alice      = "15550100021" + server
	bob        = "15550100022" + server
	carol      = "15550100023" + server
	aliceLID   = "100000000000021@lid"
	bobLID     = "100000000000022@lid"
	group      = "120363000000000021@g.us"
	broadcast  = "status@broadcast"
	newsletter = "120363000000000023@newsletter"

	bootstrapID = "FAKE-HISTORY-BOOTSTRAP"
	recentID    = "FAKE-HISTORY-RECENT"
	device5ID   = "FAKE-HISTORY-DEVICE-5"
	oversizedID = "FAKE-HISTORY-OVERSIZED"
	poisonID    = "FAKE-HISTORY-POISON"

	groupText = "Bob writes to the synthetic group"
	week      = 7 * 24 * time.Hour
)

var synthetic = []string{"15550100021", "15550100022", "15550100023", wrongAccount}

func members(owner string) []engine.Participant {
	return []engine.Participant{{User: owner + server, Admin: true}, {User: alice, Admin: true}, {User: bob}}
}

func bootstrapHistory(owner string, now time.Time) engine.History {
	return engine.History{
		Conversations: []engine.Conversation{
			{Chat: alice, Messages: []engine.Message{
				{ID: "FAKE-H01", Sender: alice, Timestamp: now.Add(-48 * time.Hour), PushName: "Alice (synthetic)", Kind: engine.KindText, Text: "An old message from Alice"},
				{ID: "FAKE-H02", Sender: owner + ":" + linkedDevice + server, FromMe: true, Timestamp: now.Add(-47 * time.Hour), Kind: engine.KindText, Text: "An old reply from the owner"},
			}},
			{Chat: group, Subject: "Synthetic group", Members: members(owner), Messages: []engine.Message{
				{ID: "FAKE-H03", Sender: bob, Timestamp: now.Add(-24 * time.Hour), Kind: engine.KindText, Text: "An old message in the synthetic group"},
			}},
		},
		LIDMappings: []engine.LIDMapping{{PN: bob, LID: bobLID}},
		Contacts:    []engine.Contact{{User: carol, PushName: "Carol (synthetic)"}},
	}
}

func recentHistory(owner string, now time.Time) engine.History {
	return engine.History{Conversations: []engine.Conversation{{Chat: carol, Messages: []engine.Message{
		{ID: "FAKE-H04", Sender: carol, Timestamp: now.Add(-2 * time.Hour), Kind: engine.KindText, Text: "Carol, from the downloaded history"},
		{ID: "FAKE-H05", Sender: owner + ":" + linkedDevice + server, FromMe: true, Timestamp: now.Add(-time.Hour), Kind: engine.KindText, Text: "The owner answers Carol"},
	}}}}
}

func script(owner string, bootstrap []byte) []engine.Event {
	self := owner + ":" + linkedDevice + server
	history := func(sender, id string, ref engine.HistoryRef) engine.HistoryNotification {
		ref.ID = id
		return engine.HistoryNotification{Sender: sender, FromMe: true, Ref: ref}
	}
	text := func(chat, id, sender, body string) engine.Message {
		return engine.Message{Chat: chat, ID: id, Sender: sender, Kind: engine.KindText, Text: body}
	}
	change := func(kind engine.Kind, chat, id, sender string, target engine.Key) engine.Message {
		return engine.Message{Chat: chat, ID: id, Sender: sender, Kind: kind, Target: &target}
	}
	edit := func(chat, id, sender, body string, target engine.Key) engine.Message {
		m := change(engine.KindEdit, chat, id, sender, target)
		m.Text = body
		return m
	}
	first := text(alice, "FAKE-L01", alice, "Hello from the fake engine")
	first.SenderAlt, first.Addressing, first.PushName = aliceLID, "pn", "Alice (synthetic)"
	reply := text(alice, "FAKE-L02", self, "A reply from the owner")
	reply.FromMe, reply.RecipientAlt = true, aliceLID
	quote := text(group, "FAKE-L04", alice, "Alice quotes Bob")
	quote.Reply = &engine.Reply{ID: "FAKE-L03", Participant: bob, Text: groupText}
	forged := text(group, "FAKE-L05", alice, "Alice quotes Bob wrongly")
	forged.Reply = &engine.Reply{ID: "FAKE-L03", Participant: bob, Text: "FORGED: Bob never wrote this"}
	foreign := text(alice, "FAKE-L06", alice, "Alice quotes the group in a direct chat")
	foreign.Reply = &engine.Reply{ID: "FAKE-L03", Participant: bob, RemoteJID: group, Text: groupText}
	byLID := text(group, "FAKE-L07", bobLID, "Bob again, addressed by his LID")
	byLID.SenderAlt, byLID.Addressing = bob, "lid"
	ownRevoke := change(engine.KindRevoke, alice, "FAKE-L10", self, engine.Key{FromMe: true, ID: "FAKE-L02"})
	ownRevoke.FromMe = true
	reaction := change(engine.KindReaction, group, "FAKE-L14", bob, engine.Key{Participant: alice, ID: "FAKE-L04"})
	reaction.Text = "\U0001F44D"
	disappearing := text(alice, "FAKE-L17", alice, "This message disappears after seven days")
	disappearing.Expiration = week
	media := engine.Message{Chat: alice, ID: "FAKE-L18", Sender: alice, Kind: engine.KindMedia, MediaType: "image/jpeg", Text: "A synthetic photo caption"}
	return []engine.Event{
		history(owner+server, bootstrapID, engine.HistoryRef{Inline: bootstrap}),
		engine.Group{Chat: group, Subject: "Synthetic group", Members: members(owner)},
		first,
		reply,
		history(owner+server, recentID, engine.HistoryRef{DirectPath: "/fake/history/recent"}),
		text(group, "FAKE-L03", bob, groupText),
		quote,
		forged,
		foreign,
		byLID,
		engine.Disconnected{},
		edit(alice, "FAKE-L08", alice, "Hello from the fake engine, edited", engine.Key{FromMe: true, ID: "FAKE-L01"}),
		edit(group, "FAKE-L09", bob, "Bob edits what Alice wrote", engine.Key{Participant: alice, ID: "FAKE-L04"}),
		ownRevoke,
		change(engine.KindRevoke, group, "FAKE-L11", alice, engine.Key{Participant: bob, ID: "FAKE-L03"}),
		change(engine.KindRevoke, group, "FAKE-L12", bob, engine.Key{Participant: alice, ID: "FAKE-L05"}),
		change(engine.KindRevoke, alice, "FAKE-L13", alice, engine.Key{RemoteJID: group, FromMe: true, ID: "FAKE-L04"}),
		reaction,
		engine.Message{Chat: group, ID: "FAKE-L15", Sender: bob, Kind: engine.KindOther},
		change(engine.KindPollUpdate, group, "FAKE-L16", alice, engine.Key{Participant: bob, ID: "FAKE-L15"}),
		disappearing,
		media,
		text(broadcast, "FAKE-L19", alice, "A status update"),
		text(newsletter, "FAKE-L20", newsletter, "A newsletter post"),
		engine.Message{Chat: alice, ID: "FAKE-L21", Sender: alice, Kind: engine.KindRevoke},
		engine.Group{Chat: group, Subject: "Synthetic group, renamed", Joined: []engine.Participant{{User: carol}}},
		history(owner+":5"+server, device5ID, engine.HistoryRef{Inline: bootstrap}),
		history(owner+server, oversizedID, engine.HistoryRef{DirectPath: "/fake/history/oversized"}),
		history(owner+server, poisonID, engine.HistoryRef{DirectPath: "/fake/history/poison"}),
	}
}
