package scoped_test

import (
	"cmp"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const ownerJID = "15550100099@s.whatsapp.net"

type sentMessage struct {
	chat, id, sender string
	canary           string
}

type propClient struct {
	id      string
	all     bool
	revoked bool
}

type scopeSet struct {
	all   bool
	chats map[string]bool
}

func (s scopeSet) has(jid string) bool { return s.all || s.chats[jid] }

type tally struct {
	mu     sync.Mutex
	runs   int
	counts map[string]int
}

func (c *tally) add(seen map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs++
	for k, v := range seen {
		if v {
			c.counts[k]++
		}
	}
}

func (c *tally) require(t *testing.T, features ...string) {
	t.Logf("coverage over %d checks: %v", c.runs, c.counts)
	if c.runs < 100 {
		return
	}
	for _, f := range features {
		if c.counts[f] == 0 {
			t.Errorf("no check covered %q: tune the generators", f)
		}
	}
}

func canary(k int) string { return "zq" + strconv.Itoa(k) + "x" }

const savedName = "Synthetic Saved"

func checkSavedNames(t *rapid.T, a *archive, id string, set scopeSet, saved []string, rows []scoped.Message) {
	for _, m := range rows {
		sender := m.Sender.JID()
		want := ""
		if slices.Contains(saved, sender) && set.has(sender) {
			want = savedName
		}
		if m.SavedName != want {
			t.Fatalf("%s read the saved name %q of %s, want %q", id, m.SavedName, sender, want)
		}
		a.seen["saved name shown"] = a.seen["saved name shown"] || want != ""
		a.seen["saved name hidden"] = a.seen["saved name hidden"] || want == "" && slices.Contains(saved, sender)
	}
}

func scopeOfClient(t *rapid.T, s *ingest.Store, c propClient) scopeSet {
	if c.revoked {
		return scopeSet{}
	}
	if c.all {
		return scopeSet{all: true}
	}
	rows, err := s.Scoped().Column(t.Context(), "SELECT chat_jid FROM client_read_chats WHERE client_id = ?", c.id)
	if err != nil {
		t.Fatalf("client chats: %v", err)
	}
	set := scopeSet{chats: map[string]bool{}}
	for _, jid := range rows {
		set.chats[jid] = true
	}
	return set
}

func grantOf(t *rapid.T, c propClient, set scopeSet) policy.ReadGrant {
	read := map[policy.CanonicalChat]struct{}{}
	for jid := range set.chats {
		read[chat(t, jid)] = struct{}{}
	}
	g, _ := policy.DecideRead(&policy.Client{ID: c.id, ReadAll: c.all, Revoked: c.revoked, Read: read, ExpiresAt: epoch.Add(24 * time.Hour)}, epoch)
	return g
}

func dump(t *rapid.T, s *ingest.Store) ([]scoped.DumpChat, []scoped.DumpMessage) {
	chats, messages, err := s.Scoped().Dump(t.Context())
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	return chats, messages
}

func visibleSeqs(set scopeSet, messages []scoped.DumpMessage) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range messages {
		if set.has(m.Chat) {
			out[m.Seq] = true
		}
	}
	return out
}

type archive struct {
	store   *ingest.Store
	clients []propClient
	sent    []sentMessage
	dead    []string
	live    map[string]bool
	seen    map[string]bool
}

func buildArchive(t *rapid.T, dir string) *archive {
	var idents, users []string
	var preferLID []bool
	for i := range rapid.IntRange(0, 2).Draw(t, "persons") {
		pn, lid := "1555010010"+strconv.Itoa(i)+"@s.whatsapp.net", "10000000000010"+strconv.Itoa(i)+"@lid"
		idents, users = append(idents, pn, lid), append(users, pn, lid)
		preferLID = append(preferLID, rapid.Bool().Draw(t, "prefers the LID"))
	}
	if rapid.Bool().Draw(t, "group") {
		idents = append(idents, "120363000000000100@g.us")
	}
	s, err := ingest.Open(t.Context(), testOptions(dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a := &archive{store: s, live: map[string]bool{}, seen: map[string]bool{}}
	for i := range rapid.IntRange(1, 3).Draw(t, "clients") {
		c := propClient{id: "client0" + strconv.Itoa(i)}
		var read []string
		switch rapid.IntRange(0, 3).Draw(t, "client kind") {
		case 0:
			c.all = true
		case 1:
			c.revoked = true
			fallthrough
		default:
			for p := 0; p < len(users); p += 2 {
				switch rapid.IntRange(0, 3).Draw(t, "identities read") {
				case 1:
					read = append(read, users[p])
				case 2:
					read = append(read, users[p+1])
				case 3:
					read = append(read, users[p], users[p+1])
				}
			}
			if len(idents) > len(users) && rapid.Bool().Draw(t, "reads the group") {
				read = append(read, idents[len(idents)-1])
			}
			if rapid.Bool().Draw(t, "an unseen chat") {
				read = append(read, "15550100077@s.whatsapp.net")
			}
		}
		addClient(t, s, c, read)
		a.clients = append(a.clients, c)
	}
	k := 0
	var batch []func(*ingest.Tx) error
	flush := func() {
		if len(batch) == 0 {
			return
		}
		fns := batch
		batch = nil
		if err := s.Write(t.Context(), "test.ops", func(tx *ingest.Tx) error {
			for _, fn := range fns {
				if err := fn(tx); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if rapid.Bool().Draw(t, "the owner saved a name for themself") {
		batch = append(batch, func(tx *ingest.Tx) error { return tx.SetContactName(chat(t, ownerJID), savedName, "", epoch) })
	}
	ops := rapid.IntRange(4, 40).Draw(t, "operations")
	for i := range ops {
		if len(idents) == 0 {
			break
		}
		switch op := rapid.IntRange(0, 9).Draw(t, "operation"); {
		case op <= 4:
			c := rapid.SampledFrom(idents).Draw(t, "chat")
			if i := slices.Index(users, c); i >= 0 && rapid.IntRange(0, 7).Draw(t, "the preferred identity") > 0 {
				if preferLID[i/2] {
					c = users[i|1]
				} else {
					c = users[i&^1]
				}
			}
			sender := ownerJID
			fromMe := true
			switch {
			case strings.HasSuffix(c, "@g.us") && len(users) > 0 && rapid.Bool().Draw(t, "from a member"):
				sender, fromMe = rapid.SampledFrom(users).Draw(t, "member"), false
			case !strings.HasSuffix(c, "@g.us") && rapid.Bool().Draw(t, "from the peer"):
				sender, fromMe = c, false
			}
			k++
			m := sentMessage{chat: c, id: "m" + strconv.Itoa(i), sender: sender, canary: canary(k)}
			a.sent, a.live[m.canary] = append(a.sent, m), true
			msg := message(t, c, m.id, sender, "text "+m.canary+" end", epoch.Add(time.Duration(rapid.IntRange(0, 4).Draw(t, "minute"))*time.Minute))
			msg.FromMe = fromMe
			batch = append(batch, func(tx *ingest.Tx) error { _, _, err := tx.InsertMessage(msg); return err })
		case op <= 6 && len(a.sent) > 0:
			idx := rapid.IntRange(0, len(a.sent)-1).Draw(t, "target")
			revoke := op == 6
			k++
			next := canary(k)
			batch = append(batch, func(tx *ingest.Tx) error {
				target := &a.sent[idx]
				f, ok, err := tx.ResolveInChat(chat(t, target.chat), target.id, chat(t, target.sender))
				if err != nil || !ok || f.Revoked {
					return err
				}
				a.live[target.canary] = false
				a.dead = append(a.dead, target.canary)
				if revoke {
					a.seen["revoke"] = true
					return tx.ApplyRevoke(f.Ref)
				}
				a.seen["edit"] = true
				target.canary, a.live[next] = next, true
				return tx.ApplyEdit(f.Ref, "edited "+next+" end", epoch.Add(time.Hour))
			})
		case op <= 8 && len(users) > 0 && 4*i >= ops:
			flush()
			p := rapid.IntRange(0, len(users)/2-1).Draw(t, "person")
			a.learn(t, users[2*p+1], users[2*p])
		default:
			c := rapid.SampledFrom(idents).Draw(t, "touched chat")
			source := ingest.NamePushName
			if strings.HasSuffix(c, "@g.us") {
				source = ingest.NameGroupSubject
			} else if rapid.Bool().Draw(t, "saves a contact name") {
				batch = append(batch, func(tx *ingest.Tx) error { return tx.SetContactName(chat(t, c), savedName, "", epoch) })
			}
			batch = append(batch, func(tx *ingest.Tx) error { return tx.SetChatName(chat(t, c), "Synthetic", source, ingest.OriginLive) })
		}
	}
	flush()
	return a
}

func addClient(t *rapid.T, s *ingest.Store, c propClient, read []string) {
	revoked := "NULL"
	if c.revoked {
		revoked = "2"
	}
	all := "0"
	if c.all {
		all = "1"
	}
	stmts := []string{"INSERT INTO clients (id, name, token_hash, read_all, created_at, expires_at, revoked_at) VALUES ('" + c.id + "', '" + c.id + "', zeroblob(32), " + all + ", 1, 2, " + revoked + ")"}
	for _, jid := range read {
		stmts = append(stmts, "INSERT INTO client_read_chats (client_id, chat_jid) VALUES ('"+c.id+"', '"+jid+"')")
	}
	if err := s.Scoped().Exec(t.Context(), stmts...); err != nil {
		t.Fatalf("add a client: %v", err)
	}
}

func (a *archive) learn(t *rapid.T, lid, pn string) {
	_, before := dump(t, a.store)
	all := visibleSeqs(scopeSet{all: true}, before)
	scopes := make([]scopeSet, len(a.clients))
	for i, c := range a.clients {
		scopes[i] = scopeOfClient(t, a.store, c)
	}
	var res ingest.LIDResult
	if err := a.store.Write(t.Context(), "test.learn", func(tx *ingest.Tx) error {
		var err error
		res, err = tx.LearnLID(chat(t, lid), chat(t, pn), ingest.MappingSenderAlt, epoch)
		return err
	}); err != nil {
		t.Fatalf("LearnLID: %v", err)
	}
	switch {
	case res.Outcome == ingest.LIDLearned && res.Rescoped:
		a.seen["re-key moved a scope"] = true
	case res.Outcome == ingest.LIDLearned:
		a.seen["re-key"] = true
	case res.Conflict == ingest.ConflictScopedChat:
		a.seen["re-key refused for a scope"] = true
	}
	_, after := dump(t, a.store)
	for i, c := range a.clients {
		was := visibleSeqs(scopes[i], before)
		for seq := range visibleSeqs(scopeOfClient(t, a.store, c), after) {
			if all[seq] && !was[seq] {
				t.Fatalf("learning %s for %s (%+v) let %s see message %d, which it could not see before", lid, pn, res, c.id, seq)
			}
		}
	}
}

func chatOrder(a, b scoped.DumpChat) int {
	if a.Dated != b.Dated {
		if a.Dated {
			return -1
		}
		return 1
	}
	return cmp.Or(cmp.Compare(b.LastTS, a.LastTS), cmp.Compare(b.Row, a.Row))
}

func TestPropertyScopedReadsMatchTheOracle(t *testing.T) {
	parent := t.TempDir()
	cov := &tally{counts: map[string]int{}}
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp(parent, "archive")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		a := buildArchive(t, dir)
		defer func() { _ = a.store.Close(); _ = os.RemoveAll(dir) }()
		chats, messages := dump(t, a.store)
		saved, err := a.store.Scoped().Column(t.Context(), "SELECT jid FROM contact_names")
		if err != nil {
			t.Fatalf("saved names: %v", err)
		}
		a.seen["dead canary"] = len(a.dead) > 0
		for _, c := range a.clients {
			set := scopeOfClient(t, a.store, c)
			g := grantOf(t, c, set)
			vis := visibleSeqs(set, messages)
			if !c.all && len(vis) > 0 && len(vis) < len(messages) {
				a.seen["a client sees some messages and not others"] = true
			}
			limit := rapid.IntRange(1, 5).Draw(t, "limit")
			checkChats(t, a, g, set, chats, limit)
			rows := checkMessages(t, a, g, set, chats, messages, limit)
			rows = append(rows, checkChanges(t, a, g, set, messages, limit)...)
			checkSavedNames(t, a, c.id, set, saved, rows)
			for _, m := range rows {
				for _, d := range a.dead {
					if strings.Contains(m.Text, d) || strings.Contains(m.TextDisplay, d) {
						t.Fatalf("%s read the revoked or edited text %s", c.id, d)
					}
				}
			}
			checkSearch(t, a, g, set, messages)
			checkMessage(t, a, g, set, messages)
		}
		cov.add(a.seen)
	})
	cov.require(t, "re-key moved a scope", "re-key refused for a scope", "a client sees some messages and not others", "revoke", "edit", "dead canary",
		"changes from a time skipped a change", "changes from a time kept a later change dated before it", "saved name shown", "saved name hidden")
}

func checkChats(t *rapid.T, a *archive, g policy.ReadGrant, set scopeSet, chats []scoped.DumpChat, limit int) {
	var want []string
	sorted := slices.Clone(chats)
	slices.SortFunc(sorted, chatOrder)
	for _, c := range sorted {
		if set.has(c.JID) {
			want = append(want, c.Ref)
		}
	}
	var got []string
	var pos scoped.ChatPosition
	for range 100 {
		page, err := a.store.Scoped().Chats(g, t.Context(), pos, limit)
		if err != nil {
			t.Fatalf("Chats: %v", err)
		}
		for _, c := range page.Chats {
			got = append(got, c.Ref)
		}
		if !page.More {
			break
		}
		pos = page.Next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Chats = %q, want %q", got, want)
	}
}

func checkMessages(t *rapid.T, a *archive, g policy.ReadGrant, set scopeSet, chats []scoped.DumpChat, messages []scoped.DumpMessage, limit int) []scoped.Message {
	var rows []scoped.Message
	var denied []string
	for _, c := range chats {
		dir := rapid.SampledFrom([]scoped.Direction{scoped.Older, scoped.Newer}).Draw(t, "direction")
		var want []int64
		of := slices.DeleteFunc(slices.Clone(messages), func(m scoped.DumpMessage) bool { return m.Chat != c.JID })
		slices.SortFunc(of, func(x, y scoped.DumpMessage) int { return cmp.Or(cmp.Compare(y.TS, x.TS), cmp.Compare(y.Seq, x.Seq)) })
		if dir == scoped.Newer {
			slices.Reverse(of)
		}
		for _, m := range of {
			want = append(want, m.Seq)
		}
		var got []int64
		var pos scoped.MessagePosition
		for range 100 {
			page, found, err := a.store.Scoped().Messages(g, t.Context(), c.Ref, pos, dir, limit)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			if found != set.has(c.JID) {
				t.Fatalf("Messages of %s found %v, want %v", c.JID, found, set.has(c.JID))
			}
			if !found {
				if len(page.Messages) != 0 || page.Chat != (scoped.Chat{}) {
					t.Fatalf("a chat out of scope returned %+v", page)
				}
				denied = append(denied, c.Ref)
				break
			}
			for _, m := range page.Messages {
				got = append(got, m.Position.Seq)
			}
			rows = append(rows, page.Messages...)
			if !page.More {
				break
			}
			pos = page.Next
		}
		if set.has(c.JID) && !slices.Equal(got, want) {
			t.Fatalf("Messages of %s in direction %d = %v, want %v", c.JID, dir, got, want)
		}
	}
	if len(denied) > 0 {
		checkDeniedEqualsMissing(t, a, g, denied[0])
	}
	return rows
}

func checkDeniedEqualsMissing(t *rapid.T, a *archive, g policy.ReadGrant, ref string) {
	var traces [2][]string
	var results [2]string
	for i, probe := range []string{ref, strings.Repeat("f", 32)} {
		r := a.store.Scoped().Traced(func(q string) { traces[i] = append(traces[i], q) })
		c, ok, err := r.Chat(g, t.Context(), probe)
		page, ok2, err2 := r.Messages(g, t.Context(), probe, scoped.MessagePosition{}, scoped.Older, 5)
		results[i] = strconv.FormatBool(ok) + strconv.FormatBool(ok2) + strconv.FormatBool(c == scoped.Chat{}) + strconv.Itoa(len(page.Messages))
		if err != nil || err2 != nil {
			t.Fatalf("a chat out of scope: %v, %v", err, err2)
		}
	}
	if results[0] != results[1] || !slices.Equal(traces[0], traces[1]) {
		t.Fatalf("an out-of-scope chat and a missing chat differ: %q, %q", results, traces)
	}
}

func checkChanges(t *rapid.T, a *archive, g policy.ReadGrant, set scopeSet, messages []scoped.DumpMessage, limit int) []scoped.Message {
	of := slices.DeleteFunc(slices.Clone(messages), func(m scoped.DumpMessage) bool { return !set.has(m.Chat) })
	slices.SortFunc(of, func(x, y scoped.DumpMessage) int { return cmp.Compare(x.ChangeSeq, y.ChangeSeq) })
	var pos scoped.ChangePosition
	if rapid.Bool().Draw(t, "changes from a time") {
		pos.Since = epoch.Add(time.Duration(rapid.IntRange(0, 5).Draw(t, "since minute")) * time.Minute).UnixMilli()
		first := slices.IndexFunc(of, func(m scoped.DumpMessage) bool { return m.TS >= pos.Since })
		if first < 0 {
			first = len(of)
		}
		if first > 0 && first < len(of) {
			a.seen["changes from a time skipped a change"] = true
		}
		of = of[first:]
		if slices.ContainsFunc(of, func(m scoped.DumpMessage) bool { return m.TS < pos.Since }) {
			a.seen["changes from a time kept a later change dated before it"] = true
		}
	}
	var want, got []int64
	for _, m := range of {
		want = append(want, m.Seq)
	}
	var rows []scoped.Message
	for range 100 {
		page, _, err := a.store.Scoped().Changes(g, t.Context(), "", pos, limit)
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		for _, m := range page.Messages {
			if m.Revoked && (m.Text != "" || m.TextDisplay != "") {
				t.Fatalf("a revoked change carries text: %+v", m)
			}
			got = append(got, m.Position.Seq)
		}
		rows = append(rows, page.Messages...)
		if !page.More {
			break
		}
		pos = page.Next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Changes = %v, want %v", got, want)
	}
	return rows
}

func checkSearch(t *rapid.T, a *archive, g policy.ReadGrant, set scopeSet, messages []scoped.DumpMessage) {
	terms := slices.Clone(a.dead)
	live := slices.Sorted(maps.Keys(a.live))
	live = slices.DeleteFunc(live, func(k string) bool { return !a.live[k] })
	if len(live) > 0 {
		terms = append(terms, rapid.SampledFrom(live).Draw(t, "live canary"))
	}
	for _, term := range terms {
		q, err := scoped.ParseQuery(term)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", term, err)
		}
		var want, got []int64
		for _, m := range messages {
			if set.has(m.Chat) && !m.Revoked && strings.Contains(m.Text, term) {
				want = append(want, m.Seq)
			}
		}
		slices.SortFunc(want, func(x, y int64) int { return cmp.Compare(y, x) })
		page, _, err := a.store.Scoped().Search(g, t.Context(), q, "", scoped.SearchPosition{}, scoped.MaxLimit)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		for _, m := range page.Messages {
			got = append(got, m.Position.Seq)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("Search(%s) = %v, want %v", term, got, want)
		}
		if !a.live[term] && len(got) > 0 {
			t.Fatalf("Search found the revoked or edited text %s", term)
		}
	}
}

func checkMessage(t *rapid.T, a *archive, g policy.ReadGrant, set scopeSet, messages []scoped.DumpMessage) {
	if len(a.sent) == 0 {
		return
	}
	for range 3 {
		m := rapid.SampledFrom(a.sent).Draw(t, "sent message")
		i := slices.IndexFunc(messages, func(d scoped.DumpMessage) bool { return d.ID == m.id })
		if i < 0 {
			t.Fatalf("message %s is not in the archive", m.id)
		}
		row, found, err := a.store.Scoped().Message(g, t.Context(), chat(t, m.chat), m.id, chat(t, m.sender))
		if err != nil || found != set.has(messages[i].Chat) {
			t.Fatalf("Message(%s) = %v, %v, want found %v", m.id, found, err, set.has(messages[i].Chat))
		}
		if found && (row.Position.Seq != messages[i].Seq || row.Text != messages[i].Text && !row.Revoked) {
			t.Fatalf("Message(%s) = %+v, want %+v", m.id, row, messages[i])
		}
	}
}
