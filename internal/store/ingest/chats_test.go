package ingest

import (
	"errors"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func learn(t *testing.T, s *Store, lid, pn string, source MappingSource) LIDResult {
	t.Helper()
	var res LIDResult
	write(t, s, func(tx *Tx) error {
		var err error
		res, err = tx.LearnLID(chat(t, lid), chat(t, pn), source, epoch)
		return err
	})
	return res
}

func canonical(t *testing.T, s *Store, jid string) string {
	t.Helper()
	var c policy.CanonicalChat
	read(t, s, func(r *Reader) error {
		var err error
		c, err = r.Canonical(chat(t, jid))
		return err
	})
	return c.JID()
}

func snapshot(t *testing.T, s *Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"chats", "chat_aliases", "lid_map", "contacts", "group_participants", "messages"} {
		out[table] = scalar[int](t, s, "SELECT count(*) FROM "+table)
	}
	out["lid chats"] = scalar[int](t, s, "SELECT count(*) FROM chats WHERE jid LIKE '%@lid'")
	out["lid senders"] = scalar[int](t, s, "SELECT count(*) FROM messages WHERE sender_jid LIKE '%@lid'")
	return out
}

func TestLearningAMappingRekeysThePhoneChat(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "M1", alice, "before the mapping"))
	write(t, s, func(tx *Tx) error { return tx.SetChatName(chat(t, alice), "Alice", NamePushName, OriginLive) })
	if res := learn(t, s, aliceLID, alice, MappingSenderAlt); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID = %+v", res)
	}
	if got := canonical(t, s, alice); got != aliceLID {
		t.Fatalf("Canonical(phone) = %q, want the LID", got)
	}
	if got := canonical(t, s, groupJID); got != groupJID {
		t.Fatalf("Canonical(group) = %q", got)
	}
	if kind, name := scalar[int](t, s, "SELECT kind FROM chats WHERE jid = ?", aliceLID), scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", aliceLID); kind != 2 || name != "Alice" {
		t.Fatalf("the re-keyed chat has kind %d and name %q", kind, name)
	}
	if alias := scalar[string](t, s, "SELECT jid FROM chat_aliases WHERE alias = ?", alice); alias != aliceLID {
		t.Fatalf("chat_aliases maps the phone chat to %q", alias)
	}
	if f, ok := resolve(t, s, alice, "M1", alice); !ok || f.Sender.JID() != aliceLID || f.Ref.chat.JID() != aliceLID {
		t.Fatalf("ResolveInChat with the phone identifiers after re-keying = %+v, %v", f, ok)
	}
	if f, ok := resolve(t, s, aliceLID, "M1", aliceLID); !ok || f.Text != "before the mapping" {
		t.Fatalf("ResolveInChat with the LID = %+v, %v", f, ok)
	}
	insert(t, s, textMessage(t, alice, "M2", alice, "after the mapping"))
	if n := scalar[int](t, s, "SELECT count(*) FROM messages WHERE chat_jid = ? AND sender_jid = ?", aliceLID, aliceLID); n != 2 {
		t.Fatalf("%d messages keyed by the LID, want both", n)
	}
	if res := learn(t, s, aliceLID, alice, MappingHistory); res.Outcome != LIDKnown {
		t.Fatalf("learning the same mapping again = %+v, want LIDKnown", res)
	}
	if source := scalar[string](t, s, "SELECT source FROM lid_map"); source != "sender_alt" {
		t.Fatalf("lid_map source = %q, want the first one", source)
	}
}

func TestConflictingMappingsAreRefused(t *testing.T) {
	s := openStore(t)
	learn(t, s, aliceLID, alice, MappingSenderAlt)
	insert(t, s, textMessage(t, bob, "B1", bob, "bob by phone"))
	insert(t, s, textMessage(t, carolLID, "C1", carolLID, "carol by lid"))
	insert(t, s, textMessage(t, carol, "C2", carol, "carol by phone"))
	insert(t, s, textMessage(t, groupJID, "G1", bob, "bob in a group by phone"))
	insert(t, s, textMessage(t, groupJID, "G1", bobLID, "bob in a group by lid"))
	before := snapshot(t, s)
	for _, tt := range []struct {
		name, lid, pn string
		want          Conflict
	}{
		{name: "the LID mapped to another number", lid: aliceLID, pn: bob, want: ConflictContradicts},
		{name: "the number mapped to another LID", lid: bobLID, pn: alice, want: ConflictContradicts},
		{name: "both chats hold messages", lid: carolLID, pn: carol, want: ConflictBothHaveChats},
		{name: "one message under both identities", lid: bobLID, pn: bob, want: ConflictMessageIDs},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if res := learn(t, s, tt.lid, tt.pn, MappingSenderAlt); res.Outcome != LIDConflict || res.Conflict != tt.want {
				t.Fatalf("LearnLID = %+v, want a %s conflict", res, tt.want)
			}
			if after := snapshot(t, s); !equalCounts(after, before) {
				t.Fatalf("a refused mapping changed the archive: %v, was %v", after, before)
			}
		})
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRekeyingMergesAnEmptyChat(t *testing.T) {
	t.Run("the phone chat is empty", func(t *testing.T) {
		s := openStore(t)
		write(t, s, func(tx *Tx) error { return tx.SetChatName(chat(t, alice), "Phone name", NamePushName, OriginLive) })
		insert(t, s, textMessage(t, aliceLID, "L1", aliceLID, "by lid"))
		if res := learn(t, s, aliceLID, alice, MappingRecipientAlt); res.Outcome != LIDLearned {
			t.Fatalf("LearnLID = %+v", res)
		}
		if n, name := scalar[int](t, s, "SELECT count(*) FROM chats"), scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", aliceLID); n != 1 || name != "Phone name" {
			t.Fatalf("%d chats, LID chat named %q, want one chat that took the phone chat's name", n, name)
		}
		if alias := scalar[string](t, s, "SELECT jid FROM chat_aliases WHERE alias = ?", alice); alias != aliceLID {
			t.Fatalf("alias %q", alias)
		}
	})
	t.Run("the LID chat is empty", func(t *testing.T) {
		s := openStore(t)
		write(t, s, func(tx *Tx) error { return tx.SetChatName(chat(t, aliceLID), "LID name", NamePushName, OriginLive) })
		insert(t, s, textMessage(t, alice, "P1", alice, "by phone"))
		if res := learn(t, s, aliceLID, alice, MappingHistory); res.Outcome != LIDLearned {
			t.Fatalf("LearnLID = %+v", res)
		}
		if n, name := scalar[int](t, s, "SELECT count(*) FROM chats"), scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", aliceLID); n != 1 || name != "LID name" {
			t.Fatalf("%d chats, LID chat named %q", n, name)
		}
		if f, ok := resolve(t, s, aliceLID, "P1", aliceLID); !ok || f.Text != "by phone" {
			t.Fatalf("the phone chat's message did not move: %+v, %v", f, ok)
		}
	})
}

func TestRekeyingMovesSendersParticipantsAndContacts(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, groupJID, "G1", bob, "bob in a group"))
	write(t, s, func(tx *Tx) error {
		if err := tx.ReplaceParticipants(chat(t, groupJID), []Participant{{User: chat(t, bob), Admin: true}, {User: chat(t, carol)}}, OriginLive); err != nil {
			return err
		}
		if err := tx.ReplaceParticipants(chat(t, group2JID), []Participant{{User: chat(t, bob)}, {User: chat(t, bobLID), Admin: true}}, OriginLive); err != nil {
			return err
		}
		if err := tx.SetPushName(chat(t, bob), "Bob by phone", epoch.Add(time.Hour), OriginLive); err != nil {
			return err
		}
		return tx.SetPushName(chat(t, bobLID), "Bob by lid", epoch, OriginLive)
	})
	if res := learn(t, s, bobLID, bob, MappingSenderAlt); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID = %+v", res)
	}
	if f, ok := resolve(t, s, groupJID, "G1", bob); !ok || f.Sender.JID() != bobLID {
		t.Fatalf("the group message's sender = %+v, %v, want the LID", f, ok)
	}
	for _, tt := range []struct {
		group string
		admin bool
	}{{groupJID, true}, {group2JID, true}} {
		var admin, known bool
		read(t, s, func(r *Reader) error {
			var err error
			admin, known, err = r.ParticipantAdmin(chat(t, tt.group), chat(t, bob))
			return err
		})
		if !known || admin != tt.admin {
			t.Fatalf("ParticipantAdmin(%s) = %v, %v", tt.group, admin, known)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM group_participants WHERE user_jid = ?", bob); n != 0 {
		t.Fatal("participants still keyed by the phone number")
	}
	if n, name := scalar[int](t, s, "SELECT count(*) FROM contacts"), scalar[string](t, s, "SELECT push_name FROM contacts WHERE jid = ?", bobLID); n != 1 || name != "Bob by phone" {
		t.Fatalf("%d contacts, LID contact named %q, want one keeping the newer push name", n, name)
	}
}

func TestLearnLIDRefusesInvalidInput(t *testing.T) {
	s := openStore(t)
	for name, call := range map[string]func(*Tx) (LIDResult, error){
		"a phone number as the LID": func(tx *Tx) (LIDResult, error) {
			return tx.LearnLID(chat(t, alice), chat(t, bob), MappingSenderAlt, epoch)
		},
		"a LID as the phone number": func(tx *Tx) (LIDResult, error) {
			return tx.LearnLID(chat(t, aliceLID), chat(t, bobLID), MappingSenderAlt, epoch)
		},
		"a group": func(tx *Tx) (LIDResult, error) {
			return tx.LearnLID(chat(t, aliceLID), chat(t, groupJID), MappingSenderAlt, epoch)
		},
		"the zero chat": func(tx *Tx) (LIDResult, error) {
			return tx.LearnLID(policy.CanonicalChat{}, chat(t, alice), MappingSenderAlt, epoch)
		},
		"the protocol library's store as source": func(tx *Tx) (LIDResult, error) {
			return tx.LearnLID(chat(t, aliceLID), chat(t, alice), "whatsmeow_lid_map", epoch)
		},
		"no time": func(tx *Tx) (LIDResult, error) {
			return tx.LearnLID(chat(t, aliceLID), chat(t, alice), MappingSenderAlt, time.Time{})
		},
	} {
		if err := s.Write(t.Context(), "test.learn", func(tx *Tx) error { _, err := call(tx); return err }); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParticipantsAndNames(t *testing.T) {
	s := openStore(t)
	g := chat(t, groupJID)
	write(t, s, func(tx *Tx) error {
		if err := tx.ReplaceParticipants(g, []Participant{{User: chat(t, alice), Admin: true}, {User: chat(t, bob)}}, OriginLive); err != nil {
			return err
		}
		if err := tx.SetParticipant(g, chat(t, carol), true); err != nil {
			return err
		}
		if err := tx.SetParticipant(g, chat(t, alice), false); err != nil {
			return err
		}
		if err := tx.RemoveParticipant(g, chat(t, bob)); err != nil {
			return err
		}
		return tx.SetChatName(g, "A group subject", NameGroupSubject, OriginLive)
	})
	for jid, want := range map[string]struct{ admin, known bool }{alice: {false, true}, bob: {false, false}, carol: {true, true}} {
		var admin, known bool
		read(t, s, func(r *Reader) error {
			var err error
			admin, known, err = r.ParticipantAdmin(g, chat(t, jid))
			return err
		})
		if admin != want.admin || known != want.known {
			t.Errorf("ParticipantAdmin(%s) = %v, %v, want %v, %v", jid, admin, known, want.admin, want.known)
		}
	}
	if source := scalar[string](t, s, "SELECT name_source FROM chats WHERE jid = ?", groupJID); source != "group_subject" {
		t.Fatalf("name_source = %q", source)
	}
	write(t, s, func(tx *Tx) error { return tx.ReplaceParticipants(g, nil, OriginLive) })
	if n := scalar[int](t, s, "SELECT count(*) FROM group_participants"); n != 0 {
		t.Fatalf("%d participants after replacing them with none", n)
	}
	for name, call := range map[string]func(*Tx) error{
		"a push name for a group":     func(tx *Tx) error { return tx.SetChatName(g, "x", NamePushName, OriginLive) },
		"a subject for a direct chat": func(tx *Tx) error { return tx.SetChatName(chat(t, alice), "x", NameGroupSubject, OriginLive) },
		"an unknown name source":      func(tx *Tx) error { return tx.SetChatName(chat(t, alice), "x", "contact_card", OriginLive) },
		"a group's push name":         func(tx *Tx) error { return tx.SetPushName(g, "x", epoch, OriginLive) },
		"a push name without a time":  func(tx *Tx) error { return tx.SetPushName(chat(t, alice), "x", time.Time{}, OriginLive) },
		"participants of a direct chat": func(tx *Tx) error {
			return tx.ReplaceParticipants(chat(t, alice), []Participant{{User: chat(t, bob)}}, OriginLive)
		},
		"a group as participant": func(tx *Tx) error {
			return tx.ReplaceParticipants(g, []Participant{{User: chat(t, group2JID)}}, OriginLive)
		},
		"a participant twice": func(tx *Tx) error {
			return tx.ReplaceParticipants(g, []Participant{{User: chat(t, alice)}, {User: chat(t, alice), Admin: true}}, OriginLive)
		},
		"the zero participant": func(tx *Tx) error { return tx.SetParticipant(g, policy.CanonicalChat{}, true) },
	} {
		if err := s.Write(t.Context(), "test.names", call); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParticipantsAreCanonical(t *testing.T) {
	s := openStore(t)
	learn(t, s, aliceLID, alice, MappingSenderAlt)
	g := chat(t, groupJID)
	write(t, s, func(tx *Tx) error {
		return tx.ReplaceParticipants(g, []Participant{{User: chat(t, alice), Admin: true}}, OriginLive)
	})
	if user := scalar[string](t, s, "SELECT user_jid FROM group_participants"); user != aliceLID {
		t.Fatalf("participant stored as %q, want the LID", user)
	}
	if err := s.Write(t.Context(), "test.participants", func(tx *Tx) error {
		return tx.ReplaceParticipants(g, []Participant{{User: chat(t, alice)}, {User: chat(t, aliceLID)}}, OriginLive)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("one participant under both identities = %v, want ErrInvalid", err)
	}
}

func participantAdmin(t *testing.T, s *Store, group, user string) (admin, known bool) {
	t.Helper()
	read(t, s, func(r *Reader) error {
		var err error
		admin, known, err = r.ParticipantAdmin(chat(t, group), chat(t, user))
		return err
	})
	return admin, known
}

func TestHistoryNeverOverridesLiveMembership(t *testing.T) {
	s := openStore(t)
	g, g2 := chat(t, groupJID), chat(t, group2JID)
	dave := "15550100004@s.whatsapp.net"
	write(t, s, func(tx *Tx) error {
		return tx.ReplaceParticipants(g, []Participant{{User: chat(t, alice), Admin: true}, {User: chat(t, bob)}, {User: chat(t, carol), Admin: true}}, OriginHistory)
	})
	write(t, s, func(tx *Tx) error {
		if err := tx.SetParticipant(g, chat(t, carol), false); err != nil {
			return err
		}
		if err := tx.SetParticipant(g, chat(t, ownerJID), true); err != nil {
			return err
		}
		return tx.RemoveParticipant(g, chat(t, bob))
	})
	write(t, s, func(tx *Tx) error {
		return tx.ReplaceParticipants(g, []Participant{{User: chat(t, alice)}, {User: chat(t, bob), Admin: true}, {User: chat(t, carol), Admin: true}, {User: chat(t, dave), Admin: true}}, OriginHistory)
	})
	for user, want := range map[string]struct{ admin, known bool }{
		alice: {false, true}, bob: {false, false}, carol: {false, true}, dave: {true, true}, ownerJID: {true, true},
	} {
		if admin, known := participantAdmin(t, s, groupJID, user); admin != want.admin || known != want.known {
			t.Errorf("after a stale history list, ParticipantAdmin(%s) = %v, %v, want %v, %v", user, admin, known, want.admin, want.known)
		}
	}
	write(t, s, func(tx *Tx) error {
		if err := tx.ReplaceParticipants(g2, []Participant{{User: chat(t, alice)}}, OriginLive); err != nil {
			return err
		}
		return tx.ReplaceParticipants(g2, []Participant{{User: chat(t, alice), Admin: true}, {User: chat(t, bob), Admin: true}}, OriginHistory)
	})
	if admin, known := participantAdmin(t, s, group2JID, alice); admin || !known {
		t.Error("a history list raised a member of a live member list to admin")
	}
	if _, known := participantAdmin(t, s, group2JID, bob); known {
		t.Error("a history list added a member to a live member list")
	}
	write(t, s, func(tx *Tx) error {
		return tx.ReplaceParticipants(g, []Participant{{User: chat(t, bob), Admin: true}}, OriginLive)
	})
	if admin, known := participantAdmin(t, s, groupJID, bob); !admin || !known {
		t.Error("a live member list did not replace a live removal")
	}
	if _, known := participantAdmin(t, s, groupJID, carol); known {
		t.Error("a live member list kept a member it does not list")
	}
}

func TestHistoryNeverOverridesLiveNames(t *testing.T) {
	s := openStore(t)
	g := chat(t, groupJID)
	write(t, s, func(tx *Tx) error {
		if err := tx.SetChatName(g, "History subject", NameGroupSubject, OriginHistory); err != nil {
			return err
		}
		if err := tx.SetChatName(g, "Newer history subject", NameGroupSubject, OriginHistory); err != nil {
			return err
		}
		if err := tx.SetChatName(chat(t, alice), "Alice live", NamePushName, OriginLive); err != nil {
			return err
		}
		if err := tx.SetChatName(chat(t, alice), "Alice from history", NamePushName, OriginHistory); err != nil {
			return err
		}
		if err := tx.SetPushName(chat(t, alice), "Alice live", epoch, OriginLive); err != nil {
			return err
		}
		if err := tx.SetPushName(chat(t, alice), "Alice from history", epoch.Add(time.Hour), OriginHistory); err != nil {
			return err
		}
		if err := tx.SetPushName(chat(t, bob), "Bob from history", epoch, OriginHistory); err != nil {
			return err
		}
		return tx.SetPushName(chat(t, bob), "Bob from newer history", epoch, OriginHistory)
	})
	if name := scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", groupJID); name != "Newer history subject" {
		t.Fatalf("subject %q, want history to replace a subject history set", name)
	}
	if name := scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", alice); name != "Alice live" {
		t.Fatalf("chat name %q, want the live one", name)
	}
	if name := scalar[string](t, s, "SELECT push_name FROM contacts WHERE jid = ?", alice); name != "Alice live" {
		t.Fatalf("push name %q, want the live one", name)
	}
	if name := scalar[string](t, s, "SELECT push_name FROM contacts WHERE jid = ?", bob); name != "Bob from newer history" {
		t.Fatalf("push name %q, want history to replace a push name history set", name)
	}
	write(t, s, func(tx *Tx) error {
		if err := tx.SetChatName(g, "Live subject", NameGroupSubject, OriginLive); err != nil {
			return err
		}
		return tx.SetChatName(g, "Stale subject", NameGroupSubject, OriginHistory)
	})
	if name := scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", groupJID); name != "Live subject" {
		t.Fatalf("subject %q, want the live one", name)
	}
	for name, call := range map[string]func(*Tx) error{
		"a chat name":   func(tx *Tx) error { return tx.SetChatName(g, "x", NameGroupSubject, "contact_card") },
		"a push name":   func(tx *Tx) error { return tx.SetPushName(chat(t, alice), "x", epoch, "") },
		"a member list": func(tx *Tx) error { return tx.ReplaceParticipants(g, nil, "server") },
	} {
		if err := s.Write(t.Context(), "test.origin", call); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s of an unknown origin = %v, want ErrInvalid", name, err)
		}
	}
}

func TestRekeyingPrefersLiveFacts(t *testing.T) {
	s := openStore(t)
	g := chat(t, groupJID)
	write(t, s, func(tx *Tx) error {
		if err := tx.ReplaceParticipants(g, []Participant{{User: chat(t, bobLID), Admin: true}, {User: chat(t, carolLID), Admin: true}}, OriginHistory); err != nil {
			return err
		}
		if err := tx.SetParticipant(g, chat(t, bob), false); err != nil {
			return err
		}
		if err := tx.RemoveParticipant(g, chat(t, carol)); err != nil {
			return err
		}
		if err := tx.SetChatName(chat(t, bob), "Bob live", NamePushName, OriginLive); err != nil {
			return err
		}
		if err := tx.SetChatName(chat(t, bobLID), "Bob from history", NamePushName, OriginHistory); err != nil {
			return err
		}
		if err := tx.SetPushName(chat(t, bob), "Bob live", epoch, OriginLive); err != nil {
			return err
		}
		return tx.SetPushName(chat(t, bobLID), "Bob from history", epoch.Add(time.Hour), OriginHistory)
	})
	for _, m := range [][2]string{{bobLID, bob}, {carolLID, carol}} {
		if res := learn(t, s, m[0], m[1], MappingSenderAlt); res.Outcome != LIDLearned {
			t.Fatalf("LearnLID(%s) = %+v", m[0], res)
		}
	}
	if admin, known := participantAdmin(t, s, groupJID, bobLID); admin || !known {
		t.Error("rekeying kept a history admin flag over a live demotion")
	}
	if _, known := participantAdmin(t, s, groupJID, carolLID); known {
		t.Error("rekeying kept a history member over a live removal")
	}
	if name := scalar[string](t, s, "SELECT name FROM chats WHERE jid = ?", bobLID); name != "Bob live" {
		t.Fatalf("chat name %q after rekeying, want the live one", name)
	}
	if name := scalar[string](t, s, "SELECT push_name FROM contacts WHERE jid = ?", bobLID); name != "Bob live" {
		t.Fatalf("push name %q after rekeying, want the live one", name)
	}
}

func TestMigratingKeepsEarlierFactsLive(t *testing.T) {
	opts := testOptions(t)
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:1]
	s := openWith(t, opts)
	migrations = all
	write(t, s, func(tx *Tx) error {
		for _, q := range []string{
			"INSERT INTO chats (jid, kind, name, name_source) VALUES ('" + groupJID + "', 3, 'Subject', 'group_subject')",
			"INSERT INTO group_participants (group_jid, user_jid, is_admin) VALUES ('" + groupJID + "', '" + alice + "', 0)",
			"INSERT INTO contacts (jid, push_name, name_source, updated_ts) VALUES ('" + alice + "', 'Alice', 'push_name', 0)",
		} {
			if _, err := tx.q.ExecContext(tx.ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = openWith(t, opts)
	if s.SchemaVersion() != len(all) {
		t.Fatalf("schema version %d after migrating, want %d", s.SchemaVersion(), len(all))
	}
	write(t, s, func(tx *Tx) error {
		if err := tx.ReplaceParticipants(chat(t, groupJID), []Participant{{User: chat(t, alice), Admin: true}}, OriginHistory); err != nil {
			return err
		}
		if err := tx.SetChatName(chat(t, groupJID), "Stale", NameGroupSubject, OriginHistory); err != nil {
			return err
		}
		return tx.SetPushName(chat(t, alice), "Stale", epoch, OriginHistory)
	})
	if admin, known := participantAdmin(t, s, groupJID, alice); admin || !known {
		t.Error("history raised a participant recorded before the migration to admin")
	}
	if got := scalar[string](t, s, "SELECT c.name || ' ' || k.push_name FROM chats c, contacts k WHERE c.jid = ? AND k.jid = ?", groupJID, alice); got != "Subject Alice" {
		t.Fatalf("names %q, want those recorded before the migration", got)
	}
}
