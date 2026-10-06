package engine

import (
	"database/sql"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/sanitize"
	"github.com/dortort/wawarden/internal/store/ingest"
)

func dm(id, from, body string) Message {
	m := text(from, id, from, body)
	m.PushName = "Alice Example"
	return m
}

func fromOwner(chatJID, id, body string) Message {
	m := text(chatJID, id, "15550100009:3@s.whatsapp.net", body)
	m.FromMe = true
	return m
}

func inGroup(id, sender, body string) Message { return text(group, id, sender, body) }

func change(kind Kind, chatJID, id, sender string, target Key) Message {
	return Message{Chat: chatJID, ID: id, Sender: sender, Timestamp: epoch.Add(time.Minute), Kind: kind, Target: &target}
}

func members(admins ...string) Group {
	g := Group{Chat: group, Subject: "Synthetic Group", Timestamp: epoch}
	for _, u := range []string{alice, bob, carol} {
		admin := false
		for _, a := range admins {
			admin = admin || a == u
		}
		g.Members = append(g.Members, Participant{User: u, Admin: admin})
	}
	return g
}

func TestFixtureText(t *testing.T) {
	r := newPipeRig(t)
	body := "hello \u202eevil\u200b\U000E0041 world"
	m := dm("3EB0000000000000000001", alice, body)
	m.Sender = "15550100001:7@s.whatsapp.net"
	r.ingest(m)
	f := r.must(alice, m.ID, alice)
	if f.Text != body || f.FromMe || f.Revoked {
		t.Fatalf("found %+v", f)
	}
	if r.counter("wawarden_messages_ingested_total") != 1 {
		t.Fatal("the ingested counter did not count the message")
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT text_display FROM messages"); got != sanitize.Display(body) || got == body {
		t.Fatalf("text_display = %q", got)
	}
	if got := query[string](t, db, "SELECT origin || from_me || kind FROM messages"); got != "live0text" {
		t.Fatalf("origin, from_me and kind = %q", got)
	}
	if got := query[string](t, db, "SELECT push_name || ' ' || name_source FROM contacts WHERE jid = ?", alice); got != "Alice Example push_name" {
		t.Fatalf("contact = %q", got)
	}
	if got := query[string](t, db, "SELECT name || ' ' || name_source FROM chats WHERE jid = ?", alice); got != "Alice Example push_name" {
		t.Fatalf("chat name = %q", got)
	}
	if n := query[int](t, db, "SELECT count(*) FROM inbox"); n != 0 {
		t.Fatalf("%d inbox rows left", n)
	}
}

func TestFixtureFromMe(t *testing.T) {
	r := newPipeRig(t)
	m := fromOwner(alice, "3EB0000000000000000002", "sent from the phone")
	m.PushName = "Owner Example"
	r.ingest(m)
	f := r.must(alice, m.ID, owner)
	if !f.FromMe {
		t.Fatal("from_me was not stored")
	}
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM contacts"); n != 0 {
		t.Fatal("the owner's own push name was stored as a contact")
	}
	if name := query[sql.NullString](t, db, "SELECT name FROM chats WHERE jid = ?", alice); name.Valid {
		t.Fatalf("the chat was named after the owner: %q", name.String)
	}
}

func TestFixtureEdit(t *testing.T) {
	r := newPipeRig(t)
	orig := dm("M1", alice, "first words")
	edit := change(KindEdit, alice, "M2", alice, Key{RemoteJID: owner, FromMe: true, ID: "M1"})
	edit.Text = "second words"
	r.ingest(orig, edit)
	if f := r.must(alice, "M1", alice); f.Text != "second words" {
		t.Fatalf("text after the edit = %q", f.Text)
	}
	if _, ok := r.find(alice, "M2", alice); ok {
		t.Fatal("the edit was stored as a message of its own")
	}
	db := r.inspect()
	if got := query[int64](t, db, "SELECT edited_ts FROM messages"); got != edit.Timestamp.UnixMilli() {
		t.Fatalf("edited_ts = %d", got)
	}
}

func TestFixtureStaleEditIsDropped(t *testing.T) {
	r := newPipeRig(t)
	newer := change(KindEdit, alice, "E2", alice, Key{FromMe: true, ID: "M1"})
	newer.Timestamp, newer.Text = epoch.Add(2*time.Minute), "second edit wording"
	older := change(KindEdit, alice, "E1", alice, Key{FromMe: true, ID: "M1"})
	older.Timestamp, older.Text = epoch.Add(time.Minute), "first edit wording"
	r.ingest(dm("M1", alice, "original wording"), newer, older)
	if f := r.must(alice, "M1", alice); f.Text != "second edit wording" || !f.EditedAt.Equal(newer.Timestamp) {
		t.Fatalf("after an older edit arrived late: %+v", f)
	}
	r.ingest(newer)
	if r.dropped(dropStaleEdit) != 2 || len(r.logs.events("ingest_failed")) != 0 {
		t.Fatalf("stale edit drops %v, failures %d", r.dropped(dropStaleEdit), len(r.logs.events("ingest_failed")))
	}
}

func TestFixtureEditByAnotherSenderIsRefused(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(inGroup("M1", alice, "alice wrote this"))
	forged := change(KindEdit, group, "M2", bob, Key{RemoteJID: group, Participant: alice, ID: "M1"})
	forged.Text = "bob rewrote it"
	own := change(KindEdit, group, "M3", bob, Key{RemoteJID: group, FromMe: true, ID: "M1"})
	own.Text = "bob rewrote it"
	empty := change(KindEdit, group, "M4", alice, Key{FromMe: true, ID: "M1"})
	r.ingest(forged, own, empty)
	if f := r.must(group, "M1", alice); f.Text != "alice wrote this" {
		t.Fatalf("text = %q", f.Text)
	}
	if r.dropped(dropNotOriginal) != 1 || r.dropped(dropTargetAbsent) != 1 || r.dropped(dropInvalid) != 1 {
		t.Fatalf("drops: not original %v, target unknown %v, invalid %v", r.dropped(dropNotOriginal), r.dropped(dropTargetAbsent), r.dropped(dropInvalid))
	}
}

func TestFixtureOwnRevoke(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(dm("M1", alice, "regret"), fromOwner(alice, "M2", "mine"))
	r.ingest(change(KindRevoke, alice, "M3", alice, Key{RemoteJID: owner, FromMe: true, ID: "M1"}))
	ownerRevoke := change(KindRevoke, alice, "M4", "15550100009:3@s.whatsapp.net", Key{RemoteJID: alice, FromMe: true, ID: "M2"})
	ownerRevoke.FromMe = true
	r.ingest(ownerRevoke)
	for _, id := range []string{"M1", "M2"} {
		sender := alice
		if id == "M2" {
			sender = owner
		}
		if f := r.must(alice, id, sender); !f.Revoked || f.Text != "" {
			t.Fatalf("%s after its sender's revoke: %+v", id, f)
		}
	}
}

func TestFixtureDirectChatChangesNeedTheOriginalSender(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(fromOwner(alice, "M1", "the owner's words"), dm("M2", alice, "alice's words"))
	byOwner := func(kind Kind, id, body string, target Key) Message {
		m := fromOwner(alice, id, body)
		m.Kind, m.Target, m.Timestamp = kind, &target, epoch.Add(time.Minute)
		return m
	}
	aliceEdit := change(KindEdit, alice, "X3", alice, Key{RemoteJID: owner, ID: "M1"})
	aliceEdit.Text = "alice rewrote the owner's words"
	r.ingest(
		change(KindRevoke, alice, "X1", alice, Key{RemoteJID: owner, ID: "M1"}),
		byOwner(KindRevoke, "X2", "", Key{RemoteJID: alice, ID: "M2"}),
		aliceEdit,
		byOwner(KindEdit, "X4", "the owner rewrote alice's words", Key{RemoteJID: alice, ID: "M2"}),
	)
	if f := r.must(alice, "M1", owner); f.Revoked || f.Text != "the owner's words" {
		t.Fatalf("alice changed the owner's message: %+v", f)
	}
	if f := r.must(alice, "M2", alice); f.Revoked || f.Text != "alice's words" {
		t.Fatalf("the owner changed alice's message: %+v", f)
	}
	if r.dropped(dropNotOriginal) != 4 {
		t.Fatalf("not original sender drops %v, want 4", r.dropped(dropNotOriginal))
	}
}

func TestFixtureDirectChatKeysNameTheOwnerOrTheChat(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(fromOwner(alice, "M1", "owner's message"), dm("M2", alice, "alice's message"))
	r.ingest(change(KindReaction, alice, "R1", alice, Key{RemoteJID: owner, ID: "M1"}))
	reply := fromOwner(alice, "R2", "")
	reply.Kind, reply.Target = KindReaction, &Key{RemoteJID: alice, ID: "M2"}
	r.ingest(reply)
	r.ingest(change(KindRevoke, alice, "R3", alice, Key{RemoteJID: bob, FromMe: true, ID: "M2"}))
	if _, ok := r.find(alice, "R1", alice); !ok {
		t.Fatal("alice's reaction to the owner's message, keyed by the owner, was dropped")
	}
	if _, ok := r.find(alice, "R2", owner); !ok {
		t.Fatal("the owner's reaction to alice's message, keyed by alice, was dropped")
	}
	if f := r.must(alice, "M2", alice); f.Revoked || r.dropped(dropForeign) != 1 {
		t.Fatalf("a key naming a third chat was followed: %+v", f)
	}
}

func TestFixtureAdminRevoke(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(members(carol), inGroup("M1", alice, "spam"))
	r.ingest(change(KindRevoke, group, "M2", "15550100003:2@s.whatsapp.net", Key{RemoteJID: group, Participant: alice, ID: "M1"}))
	if f := r.must(group, "M1", alice); !f.Revoked {
		t.Fatal("an admin's revoke was not applied")
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT name || ' ' || name_source FROM chats WHERE jid = ?", group); got != "Synthetic Group group_subject" {
		t.Fatalf("group name = %q", got)
	}
}

func TestFixtureAdminRevokeNeedsARecordedAdmin(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(members(carol), inGroup("M1", alice, "kept"))
	r.ingest(change(KindRevoke, group, "M2", bob, Key{RemoteJID: group, Participant: alice, ID: "M1"}))
	r.ingest(change(KindRevoke, group, "M3", "15550100004@s.whatsapp.net", Key{RemoteJID: group, Participant: alice, ID: "M1"}))
	r.ingest(change(KindRevoke, alice, "M4", bob, Key{Participant: alice, ID: "M1"}))
	if f := r.must(group, "M1", alice); f.Revoked {
		t.Fatal("a revoke by a non-admin was applied")
	}
	if r.dropped(dropNotAdmin) != 1 || r.dropped(dropAdminUnknown) != 1 || r.dropped(dropTargetAbsent) != 1 {
		t.Fatalf("drops: not admin %v, admin unknown %v, target unknown %v", r.dropped(dropNotAdmin), r.dropped(dropAdminUnknown), r.dropped(dropTargetAbsent))
	}
	r.ingest(Group{Chat: group, Left: []string{carol}, Joined: []Participant{{User: bob, Admin: true}}, Timestamp: epoch})
	r.ingest(change(KindRevoke, group, "M5", carol, Key{RemoteJID: group, Participant: alice, ID: "M1"}))
	if f := r.must(group, "M1", alice); f.Revoked || r.dropped(dropAdminUnknown) != 2 {
		t.Fatal("a former admin's revoke was applied")
	}
	r.ingest(change(KindRevoke, group, "M6", bob, Key{RemoteJID: group, Participant: alice, ID: "M1"}))
	if f := r.must(group, "M1", alice); !f.Revoked {
		t.Fatal("a promoted admin's revoke was refused")
	}
}

func TestFixtureCrossChatRevoke(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(inGroup("M1", alice, "in group one"))
	r.ingest(change(KindRevoke, group, "M2", alice, Key{RemoteJID: group2, FromMe: true, ID: "M1"}))
	r.ingest(change(KindRevoke, group2, "M3", alice, Key{RemoteJID: group, FromMe: true, ID: "M1"}))
	r.ingest(change(KindRevoke, group2, "M4", alice, Key{FromMe: true, ID: "M1"}))
	r.ingest(change(KindRevoke, group, "M5", alice, Key{RemoteJID: "status@broadcast", FromMe: true, ID: "M1"}))
	if f := r.must(group, "M1", alice); f.Revoked {
		t.Fatal("a revoke from or naming another chat was applied")
	}
	if r.dropped(dropForeign) != 3 || r.dropped(dropTargetAbsent) != 1 {
		t.Fatalf("drops: foreign %v, target unknown %v", r.dropped(dropForeign), r.dropped(dropTargetAbsent))
	}
}

func TestFixtureReaction(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(inGroup("M1", alice, "react to me"))
	react := change(KindReaction, group, "R1", bob, Key{RemoteJID: group, Participant: alice, ID: "M1"})
	react.Text = "\U0001F44D"
	orphan := change(KindReaction, group, "R2", bob, Key{RemoteJID: group, Participant: alice, ID: "M9"})
	r.ingest(react, orphan)
	if f := r.must(group, "R1", bob); f.Kind != "reaction" || f.Text != "" {
		t.Fatalf("reaction row %+v", f)
	}
	if _, ok := r.find(group, "R2", bob); ok || r.dropped(dropTargetAbsent) != 1 {
		t.Fatal("a reaction to an unknown message was stored")
	}
	db := r.inspect()
	if got := query[int](t, db, "SELECT count(*) FROM messages a JOIN messages b ON a.quoted_ref = b.seq WHERE a.id = 'R1' AND b.id = 'M1' AND a.text IS NULL"); got != 1 {
		t.Fatal("the reaction does not point at its target")
	}
}

func TestFixturePollUpdate(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(Message{Chat: group, ID: "P1", Sender: alice, Timestamp: epoch, Kind: KindOther})
	vote := change(KindPollUpdate, group, "V1", bob, Key{RemoteJID: group, Participant: alice, ID: "P1"})
	vote.Text = "ciphertext that is never stored"
	r.ingest(vote)
	if f := r.must(group, "V1", bob); f.Kind != "poll_update" || f.Text != "" {
		t.Fatalf("poll update row %+v", f)
	}
	edit := change(KindEdit, group, "E1", bob, Key{FromMe: true, ID: "V1"})
	edit.Text = "a vote is never edited"
	r.ingest(edit)
	if r.dropped(dropTargetKind) != 1 {
		t.Fatal("an edit of a poll update was not refused")
	}
}

func TestFixtureDisappearing(t *testing.T) {
	r := newPipeRig(t)
	m := dm("M1", alice, "this vanishes")
	m.Expiration = 7 * 24 * time.Hour
	r.ingest(m, dm("M2", alice, "this stays"))
	r.clock.advance(7*24*time.Hour - time.Second)
	r.p.sweep(t.Context())
	if f := r.must(alice, "M1", alice); f.Text != "this vanishes" {
		t.Fatal("a message was purged before it expired")
	}
	r.clock.advance(time.Second)
	r.p.sweep(t.Context())
	if f := r.must(alice, "M1", alice); f.Text != "" {
		t.Fatalf("an expired message kept its text: %+v", f)
	}
	if f := r.must(alice, "M2", alice); f.Text != "this stays" {
		t.Fatal("a message without a timer was purged")
	}
	db := r.inspect()
	if got := query[int64](t, db, "SELECT expires_at FROM messages WHERE id = 'M1'"); got != epoch.Add(m.Expiration).UnixMilli() {
		t.Fatalf("expires_at = %d", got)
	}
	if n := query[int](t, db, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '\"vanishes\"'"); n != 0 {
		t.Fatal("the expired text is still in the full-text index")
	}
}

func TestSweepPurgesInBatches(t *testing.T) {
	r := newPipeRig(t)
	for i := range sweepBatch + 5 {
		m := dm("M"+strconv.Itoa(i), alice, "short lived "+strconv.Itoa(i))
		m.Expiration = time.Hour
		r.deliver(m)
	}
	r.drain()
	r.clock.advance(2 * time.Hour)
	r.p.sweep(t.Context())
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM messages WHERE text IS NOT NULL"); n != 0 {
		t.Fatalf("%d expired messages kept their text", n)
	}
}

func TestFixtureForgedQuote(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(inGroup("M1", alice, "the original"))
	forged := inGroup("M2", bob, "replying")
	forged.Reply = &Reply{ID: "M1", Participant: alice, Text: "FORGED"}
	honest := inGroup("M3", carol, "replying too")
	honest.Reply = &Reply{ID: "M1", Participant: alice, RemoteJID: group, Text: "the original"}
	missing := inGroup("M4", carol, "replying to nothing")
	missing.Reply = &Reply{ID: "M9", Participant: alice, Text: "the original"}
	r.ingest(forged, honest, missing)
	db := r.inspect()
	rows, err := db.QueryContext(t.Context(), "SELECT id, quoted_ref IS NOT NULL, coalesce(quote_verified, -1) FROM messages WHERE id != 'M1' ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	want := map[string][2]int{"M2": {1, 0}, "M3": {1, 1}, "M4": {0, -1}}
	for rows.Next() {
		var id string
		var ref, verified int
		if err := rows.Scan(&id, &ref, &verified); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got := [2]int{ref, verified}; got != want[id] {
			t.Errorf("%s: quoted %d verified %d, want %v", id, ref, verified, want[id])
		}
	}
	if n := query[int](t, db, "SELECT count(*) FROM messages WHERE text LIKE '%FORGED%' OR text_display LIKE '%FORGED%'"); n != 0 {
		t.Fatal("quoted text was stored")
	}
}

func TestFixtureForeignRemoteJIDInAQuote(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(inGroup("M1", alice, "group one"), text(group2, "M1", alice, "group two"))
	foreign := text(group2, "M2", bob, "private reply")
	foreign.Reply = &Reply{ID: "M1", Participant: alice, RemoteJID: group, Text: "group one"}
	r.ingest(foreign)
	f := r.must(group2, "M2", bob)
	if f.Text != "private reply" || r.dropped(dropForeign) != 1 {
		t.Fatalf("message %+v, foreign drops %v", f, r.dropped(dropForeign))
	}
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM messages WHERE quoted_ref IS NOT NULL"); n != 0 {
		t.Fatal("a quote naming another chat was linked")
	}
}

func TestFixtureStatusBroadcastAndNewsletterAreDropped(t *testing.T) {
	r := newPipeRig(t)
	for _, c := range []string{"status@broadcast", "1700000000@broadcast", "120363000000000003@newsletter", "13135550002@bot", "15550100001@hosted", ""} {
		m := text(c, "S1", alice, "never stored")
		r.deliver(m)
	}
	r.deliver(text(alice, "S2", "abc@lid", "unknown sender"), Group{Chat: "status@broadcast", Subject: "x", Timestamp: epoch})
	r.drain()
	if r.dropped(dropChat) != 7 || r.dropped(dropSender) != 1 {
		t.Fatalf("chat drops %v, sender drops %v", r.dropped(dropChat), r.dropped(dropSender))
	}
	db := r.inspect()
	if n := query[int](t, db, "SELECT (SELECT count(*) FROM messages) + (SELECT count(*) FROM chats)"); n != 0 {
		t.Fatalf("%d rows stored for dropped chats", n)
	}
}

func TestFixtureRevokeZeroValueTrap(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(dm("M1", alice, "must survive"))
	trap := Message{Chat: alice, ID: "M2", Sender: alice, Timestamp: epoch, Target: &Key{RemoteJID: owner, FromMe: true, ID: "M1"}}
	plain := dm("M3", alice, "a plain message")
	r.ingest(trap, plain)
	if f := r.must(alice, "M1", alice); f.Revoked || f.Text != "must survive" {
		t.Fatalf("a message without a kind revoked its target: %+v", f)
	}
	if r.dropped(dropUnknownKind) != 1 {
		t.Fatal("the kindless event was not counted")
	}
	if f := r.must(alice, "M3", alice); f.Kind != "text" {
		t.Fatalf("plain message %+v", f)
	}
}

func TestFixtureLIDLearnedFromServerAssertedAlternates(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(dm("M1", alice, "before the mapping"))
	m := dm("M2", aliceLID, "after")
	m.SenderAlt = "15550100001:4@s.whatsapp.net"
	m.Addressing = "lid"
	r.ingest(m)
	history := text(bobLID, "M3", bobLID, "from history")
	history.SenderAlt = bob
	r.write(func(tx *ingest.Tx) error {
		a := &applier{tx: tx, owner: r.p.owner, now: epoch}
		return a.message(history, ingest.OriginHistory)
	})
	sent := fromOwner(bobLID, "M4", "to bob")
	sent.RecipientAlt = bob
	r.ingest(sent)
	if f := r.must(aliceLID, "M1", aliceLID); f.Text != "before the mapping" {
		t.Fatalf("the phone-keyed chat was not re-keyed: %+v", f)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT pn || ' ' || source FROM lid_map WHERE lid = ?", aliceLID); got != alice+" sender_alt" {
		t.Fatalf("alice's mapping = %q", got)
	}
	if got := query[string](t, db, "SELECT jid FROM chat_aliases WHERE alias = ?", alice); got != aliceLID {
		t.Fatalf("alias = %q", got)
	}
	if got := query[string](t, db, "SELECT pn || ' ' || source FROM lid_map WHERE lid = ?", bobLID); got != bob+" recipient_alt" {
		t.Fatalf("bob's mapping = %q", got)
	}
	if got := query[string](t, db, "SELECT addressing_mode || ' ' || sender_alt FROM messages WHERE id = 'M2'"); got != "lid "+alice {
		t.Fatalf("addressing and sender_alt = %q", got)
	}
}

func TestFixtureRecipientAltIsLearnedOnlyFromTheOwnersMessages(t *testing.T) {
	r := newPipeRig(t)
	m := dm("M1", aliceLID, "incoming")
	m.RecipientAlt = owner
	r.ingest(m)
	r.must(aliceLID, "M1", aliceLID)
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM lid_map"); n != 0 {
		t.Fatalf("%d mappings learned from the recipient alternate of an incoming message", n)
	}
}

func TestFixtureAGroupKeyNamingTheOwnerIsForeign(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(inGroup("M1", alice, "kept"))
	r.ingest(change(KindRevoke, group, "M2", alice, Key{RemoteJID: owner, FromMe: true, ID: "M1"}))
	if f := r.must(group, "M1", alice); f.Revoked || f.Text != "kept" || r.dropped(dropForeign) != 1 {
		t.Fatalf("a group key naming the owner was followed: %+v, foreign drops %v", f, r.dropped(dropForeign))
	}
}

func TestFixtureEditAfterARevokeIsDroppedOnce(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(dm("M1", alice, "revoked first"), change(KindRevoke, alice, "M2", alice, Key{FromMe: true, ID: "M1"}))
	edit := change(KindEdit, alice, "M3", alice, Key{FromMe: true, ID: "M1"})
	edit.Timestamp, edit.Text = epoch.Add(2*time.Minute), "edited after the revoke"
	r.ingest(edit)
	if f := r.must(alice, "M1", alice); !f.Revoked || f.Text != "" {
		t.Fatalf("after the edit %+v", f)
	}
	if r.dropped(dropTargetKind) != 1 || len(r.logs.events("ingest_failed")) != 0 || len(r.alerts("quarantine")) != 0 || len(r.clock.slept()) != 0 {
		t.Fatalf("target kind drops %v, failures %d, quarantines %d, waits %v", r.dropped(dropTargetKind), len(r.logs.events("ingest_failed")), len(r.alerts("quarantine")), r.clock.slept())
	}
}

func TestFixtureRekeyConflictIsRefused(t *testing.T) {
	r := newPipeRig(t)
	first := dm("M1", aliceLID, "mapped")
	first.SenderAlt = alice
	r.ingest(first)
	contradiction := dm("M2", aliceLID, "contradicts")
	contradiction.SenderAlt = bob
	r.ingest(contradiction)
	if r.counter("wawarden_rekey_conflicts_total", "conflict", "mapping_contradicts") != 1 {
		t.Fatal("the contradicting mapping was not counted")
	}
	if a := r.alerts("rekey_conflict"); len(a) != 1 || a[0]["conflict"] != "mapping_contradicts" {
		t.Fatalf("rekey_conflict alerts %v", a)
	}
	r.must(aliceLID, "M2", aliceLID)
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM lid_map"); n != 1 {
		t.Fatalf("%d mappings, want only the first", n)
	}
}

func TestFixtureGroupMembership(t *testing.T) {
	r := newPipeRig(t)
	g := members(alice)
	g.Members = append(g.Members, Participant{User: "15550100001:3@s.whatsapp.net"}, Participant{User: "not a user"}, Participant{User: group2})
	r.ingest(g, Group{Chat: group, Joined: []Participant{{User: "15550100004@s.whatsapp.net"}}, Left: []string{bob, "nobody"}, Timestamp: epoch})
	r.ingest(Group{Chat: alice, Subject: "not a group", Timestamp: epoch})
	db := r.inspect()
	if got := query[string](t, db, "SELECT group_concat(user_jid || ':' || is_admin, ',') FROM (SELECT * FROM group_participants WHERE present = 1 ORDER BY user_jid)"); got != alice+":1,"+carol+":0,15550100004@s.whatsapp.net:0" {
		t.Fatalf("participants = %q", got)
	}
	if r.dropped(dropChat) != 1 {
		t.Fatal("a group event for a direct chat was not dropped")
	}
}

func TestResolutionNeverLeavesTheEventsChat(t *testing.T) {
	for seed := range uint64(3) {
		rng := rand.New(rand.NewPCG(seed, 99)) //nolint:gosec // G404: a fixed seed makes the property test reproducible
		r := newPipeRig(t)
		senders := []string{alice, bob, carol}
		r.ingest(members(carol), Group{Chat: group2, Members: []Participant{{User: alice, Admin: true}, {User: bob, Admin: true}, {User: carol, Admin: true}}, Timestamp: epoch})
		for i := range 20 {
			r.deliver(inGroup("M"+strconv.Itoa(i), senders[i%3], "message "+strconv.Itoa(i)), text(group2, "M"+strconv.Itoa(i), senders[i%3], "other "+strconv.Itoa(i)))
		}
		r.drain()
		kinds := []Kind{KindRevoke, KindEdit, KindReaction, KindPollUpdate}
		for i := range 100 {
			ev := change(kinds[rng.IntN(len(kinds))], group2, "X"+strconv.Itoa(i), senders[rng.IntN(3)], Key{
				RemoteJID: []string{"", group, group2, alice, owner}[rng.IntN(5)], FromMe: rng.IntN(2) == 0,
				ID: "M" + strconv.Itoa(rng.IntN(20)), Participant: senders[rng.IntN(3)],
			})
			ev.Text = "changed"
			r.deliver(ev)
		}
		r.drain()
		for i := range 20 {
			f := r.must(group, "M"+strconv.Itoa(i), senders[i%3])
			if f.Revoked || f.Text != "message "+strconv.Itoa(i) {
				t.Fatalf("seed %d: an event delivered in another chat changed %+v", seed, f)
			}
		}
		if r.dropped(dropForeign) == 0 {
			t.Fatalf("seed %d: no event named another chat", seed)
		}
	}
}

func TestFixtureDirectChatKeysNeedTheOwner(t *testing.T) {
	r := newPipeRig(t, withOwner(""))
	r.ingest(fromOwner(alice, "M1", "owner's message"))
	r.ingest(change(KindReaction, alice, "R1", alice, Key{ID: "M1"}), change(KindReaction, alice, "R2", alice, Key{RemoteJID: owner, ID: "M1"}))
	if _, ok := r.find(alice, "R1", alice); ok {
		t.Fatal("a key naming the owner was resolved without a configured owner")
	}
	if r.dropped(dropOwnerUnknown) != 1 || r.dropped(dropForeign) != 1 {
		t.Fatalf("drops: owner unknown %v, foreign %v", r.dropped(dropOwnerUnknown), r.dropped(dropForeign))
	}
}
