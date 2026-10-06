package scoped_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

func allChats(t *testing.T, r *scoped.Reader, g policy.ReadGrant, limit int) []string {
	t.Helper()
	var out []string
	var pos scoped.ChatPosition
	for range 100 {
		page, err := r.Chats(g, t.Context(), pos, limit)
		if err != nil {
			t.Fatalf("Chats: %v", err)
		}
		if len(page.Chats) > limit {
			t.Fatalf("a page of %d chats, over the limit of %d", len(page.Chats), limit)
		}
		for _, c := range page.Chats {
			out = append(out, c.Chat.JID())
		}
		if !page.More {
			return out
		}
		pos = page.Next
	}
	t.Fatal("Chats never ended")
	return nil
}

func TestChatsListNewestFirstThenTheChatsWithoutMessages(t *testing.T) {
	s := openStore(t)
	insert(t, s, message(t, alice, "A", alice, "alice", epoch.Add(time.Minute)), message(t, bob, "B", bob, "bob", epoch.Add(3*time.Minute)),
		message(t, groupJID, "G", alice, "group", epoch.Add(2*time.Minute)))
	write(t, s, func(tx *ingest.Tx) error {
		if err := tx.SetChatName(chat(t, carol), "Carol", ingest.NamePushName, ingest.OriginLive); err != nil {
			return err
		}
		return tx.SetChatName(chat(t, bobLID), "Bob", ingest.NamePushName, ingest.OriginLive)
	})
	want := []string{bob, groupJID, alice, bobLID, carol}
	for limit := 1; limit <= 6; limit++ {
		if got := allChats(t, s.Scoped(), grantAll(t), limit); !slices.Equal(got, want) {
			t.Fatalf("all chats in pages of %d = %q, want %q", limit, got, want)
		}
	}
	if got := allChats(t, s.Scoped(), grant(t, alice, carol, "15550100009@s.whatsapp.net"), 1); !slices.Equal(got, []string{alice, carol}) {
		t.Fatalf("chats of a grant = %q, want alice's and carol's", got)
	}
	if got := allChats(t, s.Scoped(), grant(t), 5); len(got) != 0 {
		t.Fatalf("chats of an empty grant = %q", got)
	}
	if got := allChats(t, s.Scoped(), policy.ReadGrant{}, 5); len(got) != 0 {
		t.Fatalf("chats of the zero grant = %q", got)
	}
	page, err := s.Scoped().Chats(grantAll(t), t.Context(), scoped.ChatPosition{}, 5)
	if err != nil || page.Chats[0].Ref != refOf(t, s, bob) || page.Chats[0].Name != "" || !page.Chats[0].LastAt.Equal(epoch.Add(3*time.Minute)) ||
		page.Chats[4].Name != "Carol" || page.Chats[4].NameSource != "push_name" || !page.Chats[4].LastAt.IsZero() {
		t.Fatalf("Chats = %+v, %v", page, err)
	}
}

func TestReadsRefuseAnInvalidLimit(t *testing.T) {
	s := openStore(t)
	r, g := s.Scoped(), grantAll(t)
	q, err := scoped.ParseQuery("abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, -1, scoped.MaxLimit + 1} {
		_, err1 := r.Chats(g, t.Context(), scoped.ChatPosition{}, limit)
		_, _, err2 := r.Messages(g, t.Context(), "x", scoped.MessagePosition{}, scoped.Older, limit)
		_, _, err3 := r.Search(g, t.Context(), q, "", scoped.SearchPosition{}, limit)
		_, _, err4 := r.Changes(g, t.Context(), "", scoped.ChangePosition{}, limit)
		for _, err := range []error{err1, err2, err3, err4} {
			if !errors.Is(err, scoped.ErrInvalid) {
				t.Fatalf("a read with limit %d = %v, want ErrInvalid", limit, err)
			}
		}
	}
	if _, _, err := r.Messages(g, t.Context(), "x", scoped.MessagePosition{}, scoped.Direction(2), 1); !errors.Is(err, scoped.ErrInvalid) {
		t.Fatalf("an unknown direction = %v", err)
	}
	if _, _, err := r.Search(g, t.Context(), scoped.Query{}, "", scoped.SearchPosition{}, 1); !errors.Is(err, scoped.ErrInvalid) {
		t.Fatalf("the zero query = %v", err)
	}
	for _, pos := range []scoped.ChangePosition{{ChangeSeq: -1}, {Since: -1}} {
		if _, _, err := r.Changes(g, t.Context(), "", pos, 1); !errors.Is(err, scoped.ErrInvalid) {
			t.Fatalf("the change position %+v = %v, want ErrInvalid", pos, err)
		}
	}
}

func traced(s *ingest.Store) (*scoped.Reader, *[]string) {
	var log []string
	return s.Scoped().Traced(func(q string) { log = append(log, q) }), &log
}

func TestAChatOutOfScopeAndAMissingChatAreTheSame(t *testing.T) {
	s := openStore(t)
	insert(t, s, message(t, alice, "A", alice, "alice", epoch), message(t, bob, "B", bob, "bob", epoch))
	ref := refOf(t, s, alice)
	if c, ok, err := s.Scoped().Chat(grant(t, alice), t.Context(), ref); err != nil || !ok || c.Chat.JID() != alice {
		t.Fatalf("Chat in scope = %+v, %v, %v", c, ok, err)
	}
	var traces [][]string
	for _, probe := range []string{ref, strings.Repeat("0", 32), "garbage"} {
		r, log := traced(s)
		c, ok, err := r.Chat(grant(t, bob), t.Context(), probe)
		if err != nil || ok || c != (scoped.Chat{}) {
			t.Fatalf("Chat(%q) out of scope = %+v, %v, %v", probe, c, ok, err)
		}
		page, ok, err := r.Messages(grant(t, bob), t.Context(), probe, scoped.MessagePosition{}, scoped.Older, 5)
		if err != nil || ok || len(page.Messages) != 0 {
			t.Fatalf("Messages(%q) out of scope = %+v, %v, %v", probe, page, ok, err)
		}
		traces = append(traces, *log)
	}
	if !slices.Equal(traces[0], traces[1]) || !slices.Equal(traces[0], traces[2]) || len(traces[0]) != 2 {
		t.Fatalf("an out-of-scope chat and missing chats issued different SQL: %q", traces)
	}
}

func TestMessagesPageInBothDirectionsAcrossEqualTimes(t *testing.T) {
	s := openStore(t)
	var batch []ingest.Message
	for i := range 9 {
		batch = append(batch, message(t, alice, "M"+strconv.Itoa(i), alice, "text "+strconv.Itoa(i), epoch.Add(time.Duration(i%3)*time.Minute)))
	}
	insert(t, s, batch...)
	insert(t, s, message(t, bob, "B", bob, "bob", epoch))
	want := []string{"M8", "M5", "M2", "M7", "M4", "M1", "M6", "M3", "M0"}
	ref := refOf(t, s, alice)
	for _, dir := range []scoped.Direction{scoped.Older, scoped.Newer} {
		for limit := 1; limit <= 10; limit++ {
			var got []string
			var pos scoped.MessagePosition
			for {
				page, ok, err := s.Scoped().Messages(grant(t, alice), t.Context(), ref, pos, dir, limit)
				if err != nil || !ok || page.Chat.Chat.JID() != alice {
					t.Fatalf("Messages = %+v, %v, %v", page, ok, err)
				}
				got = append(got, ids(page.Messages)...)
				if !page.More {
					break
				}
				pos = page.Next
			}
			w := slices.Clone(want)
			if dir == scoped.Newer {
				slices.Reverse(w)
			}
			if !slices.Equal(got, w) {
				t.Fatalf("direction %d in pages of %d = %q, want %q", dir, limit, got, w)
			}
		}
	}
}

func TestMessageRowsCarryTheirState(t *testing.T) {
	s := openStore(t)
	refs := insert(t, s, message(t, alice, "A", alice, "original", epoch), message(t, alice, "B", alice, "to revoke", epoch.Add(time.Minute)))
	quoted := message(t, alice, "C", "15550100009@s.whatsapp.net", "a reply", epoch.Add(2*time.Minute))
	quoted.FromMe, quoted.Quote = true, &ingest.Quote{Ref: refs[0], Verified: true}
	insert(t, s, quoted)
	write(t, s, func(tx *ingest.Tx) error {
		if err := tx.ApplyEdit(refs[0], "edited \u202etext", epoch.Add(3*time.Minute)); err != nil {
			return err
		}
		return tx.ApplyRevoke(refs[1])
	})
	page, _, err := s.Scoped().Messages(grant(t, alice), t.Context(), refOf(t, s, alice), scoped.MessagePosition{}, scoped.Older, 5)
	if err != nil || len(page.Messages) != 3 {
		t.Fatalf("Messages = %+v, %v", page, err)
	}
	c, b, a := page.Messages[0], page.Messages[1], page.Messages[2]
	if !c.FromMe || c.ReplyID != "A" || c.ReplySender.JID() != alice || !c.QuoteVerified || c.Text != "a reply" || c.Kind != "text" || c.ChatRef != refOf(t, s, alice) {
		t.Errorf("the reply = %+v", c)
	}
	if !b.Revoked || b.Text != "" || b.TextDisplay != "" {
		t.Errorf("the revoked message = %+v", b)
	}
	if a.Text != "edited \u202etext" || a.TextDisplay == a.Text || !a.EditedAt.Equal(epoch.Add(3*time.Minute)) || a.Sender.JID() != alice || !a.At.Equal(epoch) {
		t.Errorf("the edited message = %+v", a)
	}
}

func TestOneMessageResolvesThroughTheLIDMapAndTheGrant(t *testing.T) {
	s := openStore(t)
	insert(t, s, message(t, alice, "A", alice, "by phone", epoch))
	write(t, s, func(tx *ingest.Tx) error {
		_, err := tx.LearnLID(chat(t, aliceLID), chat(t, alice), ingest.MappingSenderAlt, epoch)
		return err
	})
	for _, c := range []string{alice, aliceLID} {
		m, ok, err := s.Scoped().Message(grant(t, aliceLID), t.Context(), chat(t, c), "A", chat(t, alice))
		if err != nil || !ok || m.Chat.JID() != aliceLID || m.Sender.JID() != aliceLID || m.Text != "by phone" {
			t.Fatalf("Message(%s) = %+v, %v, %v", c, m, ok, err)
		}
	}
	r, log := traced(s)
	for _, probe := range []struct{ chat, id string }{{aliceLID, "A"}, {bobLID, "A"}, {aliceLID, "missing"}} {
		if m, ok, err := r.Message(grant(t, alice, bob), t.Context(), chat(t, probe.chat), probe.id, chat(t, alice)); err != nil || ok {
			t.Fatalf("Message(%v) out of scope = %+v, %v, %v", probe, m, ok, err)
		}
	}
	for _, q := range *log {
		if strings.Contains(q, "FROM messages") {
			t.Fatalf("a message out of scope reached the messages table: %q", q)
		}
	}
	for _, bad := range []struct {
		chat   policy.CanonicalChat
		id     string
		sender policy.CanonicalChat
	}{{policy.CanonicalChat{}, "A", chat(t, alice)}, {chat(t, aliceLID), "bad id", chat(t, alice)}, {chat(t, aliceLID), "A", chat(t, groupJID)}} {
		if _, ok, err := s.Scoped().Message(grantAll(t), t.Context(), bad.chat, bad.id, bad.sender); ok || err != nil {
			t.Fatalf("Message with invalid input = %v, %v", ok, err)
		}
	}
}

func TestSavedNamesShowOnlyForAChatInScope(t *testing.T) {
	s := openStore(t)
	insert(t, s, message(t, groupJID, "G", bob, "in the group", epoch))
	write(t, s, func(tx *ingest.Tx) error { return tx.SetPushName(chat(t, bob), "Bob says", epoch, ingest.OriginLive) })
	exec(t, s, "INSERT INTO contact_names (jid, full_name, first_name, updated_ts) VALUES ('"+bob+"', 'Bob Saved', 'Bob', 1)")
	ref := refOf(t, s, groupJID)
	for _, tt := range []struct {
		g    policy.ReadGrant
		want string
	}{{grant(t, groupJID), ""}, {grant(t, groupJID, alice), ""}, {grant(t, groupJID, bob), "Bob Saved"}, {grantAll(t), "Bob Saved"}} {
		page, _, err := s.Scoped().Messages(tt.g, t.Context(), ref, scoped.MessagePosition{}, scoped.Older, 5)
		if err != nil || len(page.Messages) != 1 || page.Messages[0].PushName != "Bob says" || page.Messages[0].SavedName != tt.want {
			t.Fatalf("Messages = %+v, %v, want the saved name %q", page, err, tt.want)
		}
	}
}

func searchAll(t *testing.T, r *scoped.Reader, g policy.ReadGrant, text, ref string, limit int) []string {
	t.Helper()
	q, err := scoped.ParseQuery(text)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", text, err)
	}
	var out []string
	var pos scoped.SearchPosition
	for range 1000 {
		page, ok, err := r.Search(g, t.Context(), q, ref, pos, limit)
		if err != nil || !ok {
			t.Fatalf("Search = %v, %v", ok, err)
		}
		out = append(out, ids(page.Messages)...)
		if !page.More {
			return out
		}
		pos = page.Next
	}
	t.Fatal("Search never ended")
	return nil
}

func TestSearchFindsTextInScopeNewestFirstAcrossWindows(t *testing.T) {
	s := openStore(t)
	refs := insert(t, s, message(t, alice, "A1", alice, "the needle here", epoch), message(t, bob, "B1", bob, "a needle for bob", epoch),
		message(t, alice, "A2", alice, "needle to revoke", epoch), message(t, alice, "A3", alice, "old needle text", epoch))
	write(t, s, func(tx *ingest.Tx) error {
		if err := tx.ApplyRevoke(refs[2]); err != nil {
			return err
		}
		return tx.ApplyEdit(refs[3], "new haystack text", epoch.Add(time.Minute))
	})
	exec(t, s,
		"INSERT INTO messages (seq, chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display) VALUES (30000, '"+alice+"', 'A4', '"+alice+"', 0, 'live', 1, 'text', 'needle far', 'needle far')",
		"INSERT INTO messages_fts (rowid, text) VALUES (30000, 'needle far')",
		"INSERT INTO messages (seq, chat_jid, id, sender_jid, from_me, origin, ts, kind, text, text_display) VALUES (170000, '"+alice+"', 'A5', '"+alice+"', 0, 'live', 1, 'text', 'needle farther', 'needle farther')",
		"INSERT INTO messages_fts (rowid, text) VALUES (170000, 'needle farther')")
	for limit := 1; limit <= 4; limit++ {
		if got := searchAll(t, s.Scoped(), grant(t, alice), "NEEDLE", "", limit); !slices.Equal(got, []string{"A5", "A4", "A1"}) {
			t.Fatalf("search in pages of %d = %q", limit, got)
		}
	}
	if got := searchAll(t, s.Scoped(), grantAll(t), "needle", refOf(t, s, bob), 5); !slices.Equal(got, []string{"B1"}) {
		t.Fatalf("search in bob's chat = %q", got)
	}
	if got := searchAll(t, s.Scoped(), grantAll(t), "old needle", "", 5); len(got) != 0 {
		t.Fatalf("an edited message's old text was found: %q", got)
	}
	if got := searchAll(t, s.Scoped(), grantAll(t), "revoke", "", 5); len(got) != 0 {
		t.Fatalf("a revoked message's text was found: %q", got)
	}
	if got := searchAll(t, s.Scoped(), grantAll(t), "haystack", "", 5); !slices.Equal(got, []string{"A3"}) {
		t.Fatalf("an edited message's new text = %q", got)
	}
	q, _ := scoped.ParseQuery("needle")
	if _, ok, err := s.Scoped().Search(grant(t, alice), t.Context(), q, refOf(t, s, bob), scoped.SearchPosition{}, 5); ok || err != nil {
		t.Fatalf("search in a chat out of scope = %v, %v, want not found", ok, err)
	}
	page, _, err := s.Scoped().Search(grant(t, alice), t.Context(), q, "", scoped.SearchPosition{}, 5)
	if err != nil || len(page.Messages) != 1 || !page.More || page.Next.Upper != 170001-scoped.Window*5 {
		t.Fatalf("the first page = %+v, %v, want one message and more after five windows", page, err)
	}
}

func TestChangesFollowTheChangeNumbers(t *testing.T) {
	s := openStore(t)
	refs := insert(t, s, message(t, alice, "A1", alice, "one", epoch), message(t, bob, "B1", bob, "two", epoch), message(t, alice, "A2", alice, "three", epoch))
	write(t, s, func(tx *ingest.Tx) error { return tx.ApplyRevoke(refs[0]) })
	exec(t, s, "UPDATE messages SET change_seq = 150000 WHERE id = 'A2'")
	collectChanges := func(g policy.ReadGrant, ref string, limit int) ([]string, []scoped.Message) {
		var got []string
		var rows []scoped.Message
		var pos scoped.ChangePosition
		for range 100 {
			page, ok, err := s.Scoped().Changes(g, t.Context(), ref, pos, limit)
			if err != nil || !ok {
				t.Fatalf("Changes = %v, %v", ok, err)
			}
			got = append(got, ids(page.Messages)...)
			rows = append(rows, page.Messages...)
			if !page.More {
				return got, rows
			}
			pos = page.Next
		}
		t.Fatal("Changes never ended")
		return nil, nil
	}
	for limit := 1; limit <= 3; limit++ {
		got, rows := collectChanges(grant(t, alice), "", limit)
		if !slices.Equal(got, []string{"A1", "A2"}) || !rows[0].Revoked || rows[0].Text != "" || rows[1].Change.ChangeSeq != 150000 {
			t.Fatalf("changes in pages of %d = %q, %+v", limit, got, rows)
		}
	}
	if got, _ := collectChanges(grantAll(t), refOf(t, s, bob), 5); !slices.Equal(got, []string{"B1"}) {
		t.Fatalf("changes of bob's chat = %q", got)
	}
	if _, ok, err := s.Scoped().Changes(grant(t, alice), t.Context(), refOf(t, s, bob), scoped.ChangePosition{}, 5); ok || err != nil {
		t.Fatalf("changes of a chat out of scope = %v, %v, want not found", ok, err)
	}
	page, _, err := s.Scoped().Changes(grant(t, alice), t.Context(), "", scoped.ChangePosition{ChangeSeq: 5}, 5)
	if err != nil || len(page.Messages) != 0 || !page.More || page.Next.ChangeSeq != 5+5*scoped.Window {
		t.Fatalf("a page across empty windows = %+v, %v", page, err)
	}
}

func TestChangesFromATimeStartAtTheFirstChangeOfAMessageDatedFromIt(t *testing.T) {
	s := openStore(t)
	refs := insert(t, s, message(t, alice, "A1", alice, "one", epoch), message(t, bob, "B1", bob, "two", epoch.Add(2*time.Minute)),
		message(t, alice, "A2", alice, "three", epoch.Add(time.Minute)), message(t, bob, "B0", bob, "zero", epoch))
	exec(t, s, "UPDATE messages SET change_seq = 150000 WHERE id = 'A2'")
	write(t, s, func(tx *ingest.Tx) error { return tx.ApplyEdit(refs[0], "edited", epoch.Add(time.Hour)) })
	exec(t, s, "UPDATE messages SET change_seq = 120000 WHERE id = 'B0'")
	since := func(d time.Duration) scoped.ChangePosition {
		return scoped.ChangePosition{Since: epoch.Add(d).UnixMilli()}
	}
	collect := func(g policy.ReadGrant, ref string, pos scoped.ChangePosition, limit int) ([]string, scoped.ChangePosition, int) {
		var got []string
		for calls := 1; calls <= 100; calls++ {
			page, ok, err := s.Scoped().Changes(g, t.Context(), ref, pos, limit)
			if err != nil || !ok {
				t.Fatalf("Changes = %v, %v", ok, err)
			}
			got = append(got, ids(page.Messages)...)
			pos = page.Next
			if !page.More {
				return got, pos, calls
			}
		}
		t.Fatal("Changes never ended")
		return nil, pos, 0
	}
	for limit := 1; limit <= 3; limit++ {
		got, _, calls := collect(grant(t, alice), "", since(time.Minute), limit)
		if !slices.Equal(got, []string{"A2", "A1"}) || calls < 2 {
			t.Fatalf("alice's changes from a minute in, in pages of %d = %q in %d calls, want A2 then the edit of A1 after more than five windows", limit, got, calls)
		}
	}
	if got, _, _ := collect(grant(t, bob), "", since(time.Minute), 5); !slices.Equal(got, []string{"B1", "B0"}) {
		t.Fatalf("bob's changes from a minute in = %q", got)
	}
	if got, _, _ := collect(grantAll(t), "", since(time.Minute), 5); !slices.Equal(got, []string{"B1", "B0", "A2", "A1"}) {
		t.Fatalf("all changes from a minute in = %q", got)
	}
	if got, _, _ := collect(grantAll(t), "", since(0), 5); !slices.Equal(got, []string{"B1", "B0", "A2", "A1"}) {
		t.Fatalf("all changes from the first message = %q", got)
	}
	got, idle, _ := collect(grantAll(t), "", since(3*time.Minute), 5)
	gotChat, idleChat, _ := collect(grantAll(t), refOf(t, s, bob), since(3*time.Minute), 5)
	if len(got) != 0 || len(gotChat) != 0 || idle.Since != since(3*time.Minute).Since || idleChat.Since != idle.Since {
		t.Fatalf("changes from after every message = %q, %q, next %+v, %+v, want none and still waiting for the time", got, gotChat, idle, idleChat)
	}
	write(t, s, func(tx *ingest.Tx) error { return tx.ApplyEdit(refs[0], "edited again", epoch.Add(2*time.Hour)) })
	insert(t, s, message(t, bob, "B2", bob, "four", epoch.Add(4*time.Minute)), message(t, alice, "A3", alice, "five", epoch.Add(5*time.Minute)))
	write(t, s, func(tx *ingest.Tx) error { return tx.ApplyRevoke(refs[1]) })
	if got, _, _ := collect(grantAll(t), "", idle, 5); !slices.Equal(got, []string{"B2", "A3", "B1"}) {
		t.Fatalf("changes once a message is dated from the time = %q, want B2 and what changed after it", got)
	}
	if got, _, _ := collect(grantAll(t), refOf(t, s, bob), idleChat, 5); !slices.Equal(got, []string{"B2", "B1"}) {
		t.Fatalf("changes of bob's chat once a message is dated from the time = %q", got)
	}
	if _, ok, err := s.Scoped().Changes(grant(t, alice), t.Context(), refOf(t, s, bob), since(time.Minute), 5); ok || err != nil {
		t.Fatalf("changes from a time of a chat out of scope = %v, %v, want not found", ok, err)
	}
}

func TestParseQuery(t *testing.T) {
	for in, want := range map[string]string{
		"abc":                             `"abc"`,
		"  hello   world ":                `"hello" AND "world"`,
		`say "hi"`:                        `"say" AND """hi"""`,
		`abc" XOR "hidden`:                `"abc""" AND "XOR" AND """hidden"`,
		"NEAR(abc text:x {text}:y ^abc*":  `"NEAR(abc" AND "text:x" AND "{text}:y" AND "^abc*"`,
		"שָׁלוֹם":                         `"שָׁלוֹם"`,
		"aaa bbb ccc ddd eee fff ggg hhh": `"aaa" AND "bbb" AND "ccc" AND "ddd" AND "eee" AND "fff" AND "ggg" AND "hhh"`,
		"\u00a0abc\u2003":                 `"abc"`,
		"日本語":                             `"日本語"`,
		strings.Repeat("x", 128):          `"` + strings.Repeat("x", 128) + `"`,
	} {
		q, err := scoped.ParseQuery(in)
		if err != nil || q.Expression() != want {
			t.Errorf("ParseQuery(%q) = %q, %v, want %q", in, q.Expression(), err, want)
		}
	}
	for _, in := range []string{"", "ab", "abc de", "  ", strings.Repeat("x", 129), "abc\x00def", "abc\tdef", "abc\ndef", "abc\u0085def", "abc\x7f",
		"\xff\xfeabc", "aaa bbb ccc ddd eee fff ggg hhh iii", "👍🏽", "日本"} {
		if q, err := scoped.ParseQuery(in); !errors.Is(err, scoped.ErrInvalidQuery) {
			t.Errorf("ParseQuery(%q) = %q, %v, want ErrInvalidQuery", in, q.Expression(), err)
		}
	}
}

func TestReadsNeverQueue(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.ReadSlots = 1
	s := openWith(t, opts)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- s.Scoped().Hold(grantAll(t), context.Background(), held, release) }()
	<-held
	if _, err := s.Scoped().Chats(grantAll(t), t.Context(), scoped.ChatPosition{}, 5); !errors.Is(err, scoped.ErrBusy) {
		t.Fatalf("a read while every slot is held = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the held read = %v", err)
	}
	if _, err := s.Scoped().Chats(grantAll(t), t.Context(), scoped.ChatPosition{}, 5); err != nil {
		t.Fatalf("a read after the slot was freed = %v", err)
	}
}

func TestAReadPastItsDeadlineIsBusy(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.ReadTimeout = 300 * time.Millisecond
	s := openWith(t, opts)
	if err := s.Scoped().Stall(grantAll(t), t.Context()); !errors.Is(err, scoped.ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a read past its deadline = %v, want ErrBusy", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Scoped().Stall(grantAll(t), ctx); errors.Is(err, scoped.ErrBusy) {
		t.Fatalf("a read its caller cancelled = %v, want no ErrBusy", err)
	}
}

func TestAGrantOfMoreChatsThanOneQueryBindsIsRefused(t *testing.T) {
	s := openStore(t)
	jids := make([]string, scoped.MaxGrantChats+1)
	for i := range jids {
		jids[i] = strconv.Itoa(15550200000+i) + "@s.whatsapp.net"
	}
	if _, err := s.Scoped().Chats(grant(t, jids[:scoped.MaxGrantChats]...), t.Context(), scoped.ChatPosition{}, 5); err != nil {
		t.Fatalf("a grant of %d chats = %v", scoped.MaxGrantChats, err)
	}
	if _, err := s.Scoped().Chats(grant(t, jids...), t.Context(), scoped.ChatPosition{}, 5); !errors.Is(err, scoped.ErrGrantTooLarge) {
		t.Fatalf("a grant of %d chats = %v, want ErrGrantTooLarge", len(jids), err)
	}
}

func TestAnEmptyScopeEndsTheQueryBeforeItsFirstRow(t *testing.T) {
	s := openStore(t)
	insert(t, s, message(t, alice, "A", alice, "a row", epoch))
	const probe = "SELECT m.seq FROM messages m WHERE abs(m.seq * 0 - 9223372036854775807 - 1) > 0{scope m.chat_jid}"
	for _, g := range []policy.ReadGrant{{}, grant(t)} {
		if n, err := s.Scoped().Probe(g, t.Context(), probe); err != nil || n != 0 {
			t.Fatalf("an empty scope = %d rows, %v, want no row visited", n, err)
		}
	}
	if _, err := s.Scoped().Probe(grantAll(t), t.Context(), probe); err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("the control visited no row: %v", err)
	}
	if _, err := s.Scoped().Probe(grantAll(t), t.Context(), "SELECT 1 FROM messages"); err == nil {
		t.Fatal("a query without a scope clause ran")
	}
}
