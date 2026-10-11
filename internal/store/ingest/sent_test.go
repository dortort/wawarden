package ingest

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func TestTheFirstContactRule(t *testing.T) {
	s := openStore(t)
	g := grantFor(t, writerID, false, alice, bob, carol, aliceLID, groupJID)
	firstContact := func(k, chatJID string) error {
		t.Helper()
		_, err := s.ReserveSend(g, t.Context(), request(t, k, chatJID, "synthetic", epoch.Add(time.Duration(len(k))*time.Hour)))
		return err
	}
	if err := firstContact("first-01", bob); !errors.Is(err, ErrFirstContact) || ledger(t, s, writerID, "first-01") != "none" {
		t.Fatalf("a chat with no traffic = %v, want ErrFirstContact and no row", err)
	}
	own := textMessage(t, bob, "OWN1", ownerJID, "synthetic own")
	own.FromMe = true
	insert(t, s, own)
	if err := firstContact("first-01", bob); !errors.Is(err, ErrFirstContact) {
		t.Fatalf("a chat with only the account's own messages = %v, want ErrFirstContact", err)
	}
	insert(t, s, Message{Chat: chat(t, bob), ID: "R1", Sender: chat(t, bob), Origin: OriginLive, Timestamp: epoch, Kind: KindReaction, Ingested: epoch})
	if err := firstContact("first-01", bob); err != nil {
		t.Fatalf("a chat with a peer reaction = %v, want it admitted", err)
	}
	history := textMessage(t, carol, "H1", carol, "synthetic history")
	history.Origin = OriginHistory
	insert(t, s, history)
	if err := firstContact("first-002", carol); err != nil {
		t.Fatalf("a chat with peer history = %v, want it admitted", err)
	}
	if err := firstContact("first-0003", groupJID); err != nil {
		t.Fatalf("a group without traffic = %v, want it admitted", err)
	}
	insert(t, s, textMessage(t, aliceLID, "L1", aliceLID, "synthetic lid inbound"))
	if err := firstContact("first-00004", alice); !errors.Is(err, ErrFirstContact) {
		t.Fatalf("a number whose inbound sits under an unmerged LID = %v, want ErrFirstContact", err)
	}
	if err := firstContact("first-000005", aliceLID); err != nil {
		t.Fatalf("a LID chat with inbound = %v, want it admitted", err)
	}
	if res := learn(t, s, aliceLID, alice, MappingSenderAlt); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID = %+v", res)
	}
	if err := firstContact("first-0000006", alice); err != nil {
		t.Fatalf("a number mapped to a LID with inbound = %v, want it admitted", err)
	}
	allowed := grantFor(t, otherID, true, "15550100004@s.whatsapp.net")
	fresh := request(t, "first-allow1", "15550100004@s.whatsapp.net", "synthetic", epoch.Add(30*time.Hour))
	reserved(t, s, allowed, fresh)
	fresh.Now = fresh.Now.Add(time.Second)
	if res := replay(t, s, grantFor(t, otherID, false, "15550100004@s.whatsapp.net"), fresh); res.Kind != InProgress {
		t.Fatalf("a replay under a grant without first contact = %+v, want it answered from the ledger", res)
	}
}

func historyCopy(t *testing.T, chatJID, id, senderJID string) Message {
	t.Helper()
	m := textMessage(t, chatJID, id, senderJID, "synthetic sent text")
	m.FromMe, m.Origin = true, OriginHistory
	return m
}

func sentBy(t *testing.T, s *Store, id string) string {
	t.Helper()
	return scalar[string](t, s, "SELECT group_concat(chat_jid || ' ' || sender_jid || ' ' || origin || ' ' || coalesce(sent_by, '-'), ',') FROM messages WHERE id = ?", id)
}

func TestTheSentRowDedupsAgainstItsHistoryCopy(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "IN1", alice, "synthetic inbound"))
	insert(t, s, textMessage(t, bobLID, "IN2", bobLID, "synthetic inbound"))
	g := grantFor(t, writerID, false, alice, bobLID)

	pn := request(t, "dedup-key1", alice, "synthetic", epoch)
	pn.MsgID = "S1"
	insert(t, s, historyCopy(t, alice, "S1", ownerJID))
	reserved(t, s, g, pn)
	if row := acked(t, s, g, pn, ownerJID, epoch); row.Inserted || row.Chat != chat(t, alice) || row.Sender != chat(t, ownerJID) {
		t.Fatalf("SettleSend after a history copy = %+v, want the copy kept", row)
	}
	if got := sentBy(t, s, "S1"); got != alice+" "+ownerJID+" history "+writerID {
		t.Fatalf("message S1 = %q, want the history copy marked as sent by the client", got)
	}

	lid := request(t, "dedup-key2", bobLID, "synthetic", epoch.Add(time.Minute))
	lid.MsgID = "S2"
	insert(t, s, historyCopy(t, bobLID, "S2", ownLID))
	reserved(t, s, g, lid)
	if row := acked(t, s, g, lid, ownLID, epoch.Add(time.Minute)); row.Inserted {
		t.Fatalf("SettleSend after a history copy in a LID chat = %+v", row)
	}
	if got := sentBy(t, s, "S2"); got != bobLID+" "+ownLID+" history "+writerID {
		t.Fatalf("message S2 = %q", got)
	}

	fresh := request(t, "dedup-key3", alice, "synthetic", epoch.Add(2*time.Minute))
	fresh.MsgID = "S3"
	reserved(t, s, g, fresh)
	if row := acked(t, s, g, fresh, ownerJID, epoch.Add(2*time.Minute)); !row.Inserted {
		t.Fatalf("SettleSend of a new message = %+v, want it inserted", row)
	}
	var inserted bool
	write(t, s, func(tx *Tx) error {
		var err error
		_, inserted, err = tx.InsertMessage(historyCopy(t, alice, "S3", ownerJID))
		return err
	})
	if got := sentBy(t, s, "S3"); inserted || got != alice+" "+ownerJID+" live "+writerID {
		t.Fatalf("message S3 after its history copy = %q (inserted %v), want the sent row unchanged", got, inserted)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '\"synthetic sent\"'"); n != 3 {
		t.Fatalf("%d indexed sent messages, want 3", n)
	}
}

func TestASecondClientNeverTakesOverASentMark(t *testing.T) {
	s := inboundStore(t)
	first, second := grantFor(t, writerID, false, alice), grantFor(t, otherID, false, alice)
	mine := request(t, "mark-key-01", alice, "synthetic", epoch)
	mine.MsgID = "S1"
	reserved(t, s, first, mine)
	if row := acked(t, s, first, mine, ownerJID, epoch); !row.Inserted {
		t.Fatalf("SettleSend of a new message = %+v, want it inserted", row)
	}
	theirs := request(t, "mark-key-02", alice, "synthetic", epoch.Add(time.Minute))
	theirs.MsgID = "S1"
	reserved(t, s, second, theirs)
	if row := acked(t, s, second, theirs, ownerJID, epoch.Add(time.Minute)); row.Inserted {
		t.Fatalf("SettleSend of a stored message id = %+v, want the stored row kept", row)
	}
	if got := sentBy(t, s, "S1"); got != alice+" "+ownerJID+" live "+writerID {
		t.Fatalf("message S1 = %q, want it still marked as sent by the first client", got)
	}
	theirs.Now = epoch.Add(2 * time.Minute)
	if res, err := s.ReserveSend(second, t.Context(), theirs); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the second client's replay = %+v, %v, want ErrNotFound rather than the first client's row", res, err)
	}
	mine.Now = epoch.Add(2 * time.Minute)
	if res := replay(t, s, first, mine); res.Kind != ReplayedDone || res.Sent.ID != "S1" || res.Sent.Sender != chat(t, ownerJID) {
		t.Fatalf("the first client's replay = %+v", res)
	}
}

func TestTheOwnPairMapsOwnSendersAlike(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "IN1", alice, "synthetic inbound"))
	insert(t, s, historyCopy(t, bob, "B1", ownerJID))
	if res := learn(t, s, ownLID, ownerJID, MappingSelf); res.Outcome != LIDLearned {
		t.Fatalf("LearnLID with the self source = %+v", res)
	}
	if res := learn(t, s, ownLID, ownerJID, MappingSelf); res.Outcome != LIDKnown {
		t.Fatalf("a repeated self mapping = %+v, want LIDKnown", res)
	}
	if got := sentBy(t, s, "B1"); got != bob+" "+ownLID+" history -" {
		t.Fatalf("an own message after the self mapping = %q, want its sender re-keyed", got)
	}
	g := grantFor(t, writerID, false, alice)
	in := request(t, "own-pair-01", alice, "synthetic", epoch)
	in.MsgID = "S1"
	insert(t, s, historyCopy(t, alice, "S1", ownLID))
	reserved(t, s, g, in)
	if row := acked(t, s, g, in, ownerJID, epoch); row.Inserted || row.Sender != chat(t, ownLID) {
		t.Fatalf("SettleSend with the own number = %+v, want the history copy under the own LID", row)
	}
	if got := sentBy(t, s, "S1"); got != alice+" "+ownLID+" history "+writerID {
		t.Fatalf("message S1 = %q", got)
	}
	in.Now = epoch.Add(time.Minute)
	if res := replay(t, s, g, in); res.Kind != ReplayedDone || res.Sent.Sender != chat(t, ownLID) || res.Sent.ID != "S1" {
		t.Fatalf("replay = %+v", res)
	}
}

func TestASentReplyQuotesItsTarget(t *testing.T) {
	s := openStore(t)
	target := insert(t, s, textMessage(t, alice, "IN1", alice, "synthetic inbound"))
	g := grantFor(t, writerID, false, alice)
	in := request(t, "reply-key-1", alice, "synthetic", epoch)
	in.MsgID = "S1"
	reserved(t, s, g, in)
	reply := &ReplyTarget{ID: "IN1", Sender: chat(t, alice)}
	if _, err := s.SettleSend(g, t.Context(), Settlement{Key: in.Key, Outcome: SendAcked, Now: epoch, AckedAt: epoch, Sent: sentText(t, alice, "S1", ownerJID, epoch), Reply: reply}); err != nil {
		t.Fatalf("SettleSend: %v", err)
	}
	if got := scalar[string](t, s, "SELECT quoted_ref || ' ' || quote_verified FROM messages WHERE id = 'S1'"); got != strconv.FormatInt(target.seq, 10)+" 1" {
		t.Fatalf("the reply quotes %q, want message %d, verified", got, target.seq)
	}
	gone := request(t, "reply-key-2", alice, "synthetic", epoch.Add(time.Minute))
	gone.MsgID = "S2"
	reserved(t, s, g, gone)
	if _, err := s.SettleSend(g, t.Context(), Settlement{Key: gone.Key, Outcome: SendAcked, Now: gone.Now, AckedAt: gone.Now, Sent: sentText(t, alice, "S2", ownerJID, gone.Now), Reply: &ReplyTarget{ID: "MISSING", Sender: chat(t, alice)}}); err != nil {
		t.Fatalf("SettleSend: %v", err)
	}
	if got := scalar[string](t, s, "SELECT coalesce(quoted_ref, '-') || coalesce(quote_verified, '-') FROM messages WHERE id = 'S2'"); got != "--" {
		t.Fatalf("a reply to a missing message quotes %q", got)
	}
}

func TestRecordSentRefusesWhatTheGrantOrTheShapeDoesNotAllow(t *testing.T) {
	s := openStore(t)
	g := grantFor(t, writerID, false, alice)
	for name, tt := range map[string]struct {
		g    policy.WriteGrant
		edit func(*Message)
		want error
	}{
		"a zero grant":             {policy.WriteGrant{}, func(*Message) {}, ErrNotAllowed},
		"a chat outside the grant": {grantFor(t, writerID, false, bob), func(*Message) {}, ErrNotAllowed},
		"a peer message":           {g, func(m *Message) { m.FromMe = false }, ErrInvalid},
		"a history message":        {g, func(m *Message) { m.Origin = OriginHistory }, ErrInvalid},
		"a media message":          {g, func(m *Message) { m.Kind = KindMedia }, ErrInvalid},
		"a preset quote":           {g, func(m *Message) { m.Quote = &Quote{} }, ErrInvalid},
		"no sender":                {g, func(m *Message) { m.Sender = policy.CanonicalChat{} }, ErrInvalid},
	} {
		m := sentText(t, alice, "S1", ownerJID, epoch)
		tt.edit(&m)
		err := s.Write(t.Context(), "test.record", func(tx *Tx) error {
			_, err := tx.RecordSent(tt.g, m, nil)
			return err
		})
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: RecordSent = %v, want %v", name, err, tt.want)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages"); n != 0 {
		t.Fatalf("refused records stored %d messages", n)
	}
}
