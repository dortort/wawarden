package admin_test

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

func TestWriteHoldersCountsLiveClientsThatMayWrite(t *testing.T) {
	f := open(t)
	f.seed(t, phoneA, phoneB, groupG)
	holders := func(at time.Time) int {
		t.Helper()
		n, err := f.clients.WriteHolders(t.Context(), at)
		if err != nil {
			t.Fatalf("WriteHolders: %v", err)
		}
		return n
	}
	if n := holders(start); n != 0 {
		t.Fatalf("WriteHolders of no clients = %d", n)
	}
	f.create(t, policy.ClientSpec{Name: "reader", Read: []string{phoneA}})
	f.create(t, policy.ClientSpec{Name: "everything", AllChats: true})
	f.create(t, policy.ClientSpec{Name: "writer", Read: []string{phoneA, groupG}, Write: []string{phoneA}})
	f.create(t, policy.ClientSpec{Name: "short writer", Read: []string{groupG}, Write: []string{groupG}, ExpiresInDays: 1})
	revoked, _ := f.create(t, policy.ClientSpec{Name: "revoked", Read: []string{phoneB}, Write: []string{phoneB}})
	if n := holders(start); n != 3 {
		t.Fatalf("WriteHolders = %d, want the three writers", n)
	}
	if _, err := f.clients.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	for _, tt := range []struct {
		at   time.Time
		want int
	}{
		{start, 2},
		{start.Add(24*time.Hour - time.Millisecond), 2},
		{start.Add(24 * time.Hour), 1},
		{start.Add(90 * 24 * time.Hour), 0},
		{time.Time{}, 0},
	} {
		if n := holders(tt.at); n != tt.want {
			t.Fatalf("WriteHolders at %v = %d, want %d", tt.at, n, tt.want)
		}
	}
}

var chatReference = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestAFirstContactClientGivesItsUnseenChatsAReference(t *testing.T) {
	f := open(t)
	f.seed(t, phoneA)
	const (
		fresh    = "15550100007@s.whatsapp.net"
		freshLID = "100000000000007@lid"
		learned  = "100000000000008@lid"
	)
	if _, _, err := f.clients.Create(t.Context(), policy.ClientSpec{Name: "group", Read: []string{groupH}, Write: []string{groupH}, AllowFirstContact: true}); !errors.Is(err, policy.ErrWriteChatUnknown) {
		t.Fatalf("Create with an unseen group = %v, want ErrWriteChatUnknown", err)
	}
	c, _ := f.create(t, policy.ClientSpec{Name: "first contact", Read: []string{phoneA, fresh, freshLID, groupG}, Write: []string{phoneA, fresh, freshLID}, AllowFirstContact: true})
	known := map[string]bool{}
	for _, w := range c.Write {
		known[w.Chat.JID()] = w.Known
	}
	if len(known) != 3 || !known[phoneA] || known[fresh] || known[freshLID] {
		t.Fatalf("the create answer marks the write chats %v, want only the seen one known", known)
	}
	d := rawOpen(t, f.copyArchive(t))
	kinds := map[string]int{}
	refs := map[string]bool{}
	rows, err := d.QueryContext(t.Context(), "SELECT jid, kind, ref FROM chats")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for rows.Next() {
		var jid, ref string
		var kind int
		if err := rows.Scan(&jid, &kind, &ref); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if !chatReference.MatchString(ref) || refs[ref] {
			t.Fatalf("chat %s has the reference %q", jid, ref)
		}
		kinds[jid], refs[ref] = kind, true
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(kinds) != 3 || kinds[phoneA] != 1 || kinds[fresh] != 1 || kinds[freshLID] != 2 {
		t.Fatalf("chats %v, want the seen chat and one placeholder per unseen direct write chat", kinds)
	}
	got, err := f.clients.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, w := range got.Write {
		if !w.Known {
			t.Fatalf("write chat %s is unknown after the create", w.Chat.JID())
		}
	}
	err = f.store.Write(t.Context(), "test.learn", func(tx *ingest.Tx) error {
		res, err := tx.LearnLID(chat(t, learned), chat(t, fresh), ingest.MappingSenderAlt, start)
		if err == nil && (res.Outcome != ingest.LIDLearned || !res.Rescoped) {
			t.Errorf("LearnLID = %+v, want the placeholder re-keyed with the client's scope", res)
		}
		return err
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err = f.clients.Get(t.Context(), c.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	write := map[string]bool{}
	for _, w := range got.Write {
		write[w.Chat.JID()] = w.Known
	}
	if len(write) != 3 || !write[learned] || !write[phoneA] || !write[freshLID] {
		t.Fatalf("write chats after the mapping %v, want the placeholder's LID known in place of its number", write)
	}
}
