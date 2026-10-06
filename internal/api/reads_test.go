package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	keyGroup  = "synthetic-group-reader"
	keyAlice  = "synthetic-alice-reader"
	keyAll    = "synthetic-all-reader"
	keyGroup2 = "synthetic-second-group-reader"

	groupRef = "11111111111111111111111111111111"
	aliceRef = "22222222222222222222222222222222"
	bobRef   = "33333333333333333333333333333333"
	noRef    = "44444444444444444444444444444444"
)

var (
	groupChat = syntheticChat("120363000000000001@g.us")
	aliceChat = syntheticChat("15550100001@s.whatsapp.net")
	bobChat   = syntheticChat("15550100002@s.whatsapp.net")
)

func syntheticChat(jid string) policy.CanonicalChat {
	c, ok := policy.Normalize(jid)
	if !ok {
		panic("synthetic chat refused: " + jid)
	}
	return c
}

func readClient(id string, read ...policy.CanonicalChat) *policy.Client {
	set := map[policy.CanonicalChat]struct{}{}
	for _, c := range read {
		set[c] = struct{}{}
	}
	return &policy.Client{ID: id, Name: id + "-name", Read: set, ExpiresAt: testNow.Add(24 * time.Hour)}
}

type readFixture struct {
	handler http.Handler
	archive *fakeArchive
	audit   *fakeAuditor
	clock   *clock
	clients fakeAuthenticator
	cursors *cursor.Sealer
	refs    *cursor.Sealer
	state   string
}

func syntheticMessage(chat policy.CanonicalChat, ref string, seq int64, sender policy.CanonicalChat) scoped.Message {
	at := testNow.Add(time.Duration(seq) * time.Minute)
	return scoped.Message{
		Chat: chat, ChatRef: ref, ID: "SYNTHETIC" + strconv.FormatInt(seq, 10), Sender: sender, PushName: "Push " + strconv.FormatInt(seq, 10),
		At: at, Kind: "text", Text: "synthetic text " + strconv.FormatInt(seq, 10), TextDisplay: "synthetic text " + strconv.FormatInt(seq, 10),
		Position: scoped.MessagePosition{TS: at.UnixMilli(), Seq: seq}, Change: scoped.ChangePosition{ChangeSeq: seq},
	}
}

func newReadFixture(t *testing.T) *readFixture {
	t.Helper()
	f := &readFixture{audit: &fakeAuditor{}, clock: newClock(), state: "connected"}
	writer := readClient("client-alice", aliceChat)
	writer.Write = map[policy.CanonicalChat]struct{}{aliceChat: {}}
	f.clients = fakeAuthenticator{
		keyGroup:  readClient("client-group", groupChat),
		keyGroup2: readClient("client-group2", groupChat),
		keyAlice:  writer,
		keyAll:    {ID: "client-all", Name: "client-all-name", ReadAll: true, ExpiresAt: testNow.Add(24 * time.Hour)},
	}
	f.archive = &fakeArchive{chats: []scoped.Chat{
		{Chat: groupChat, Ref: groupRef, Name: "Synthetic Group", NameSource: "group_subject", LastAt: testNow, Position: scoped.ChatPosition{LastTS: testNow.UnixMilli(), Row: 3}},
		{Chat: aliceChat, Ref: aliceRef, Name: "Alice", NameSource: "push_name", Position: scoped.ChatPosition{Row: 2, Undated: true}},
		{Chat: bobChat, Ref: bobRef, Position: scoped.ChatPosition{Row: 1, Undated: true}},
	}}
	for seq := int64(10); seq >= 1; seq-- {
		f.archive.messages = append(f.archive.messages, syntheticMessage(groupChat, groupRef, seq, aliceChat))
	}
	f.archive.messages = append(f.archive.messages, syntheticMessage(aliceChat, aliceRef, 11, aliceChat), syntheticMessage(bobChat, bobRef, 12, bobChat))
	deps := testClientDeps(t, f.clients, f.clock.now)
	deps.Archive, deps.Audit = f.archive, f.audit
	deps.Session = func() string { return f.state }
	f.cursors, f.refs = deps.Cursors, deps.Refs
	f.handler = NewClientHandler(deps)
	return f
}

func (f *readFixture) get(t *testing.T, key, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(f.handler, newRequest(t, http.MethodGet, target, bearer(key)))
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

type sessionView struct {
	State string `json:"state"`
}

type messageView struct {
	Ref    string `json:"mref"`
	Chat   string `json:"chat"`
	Sender struct {
		ID   string  `json:"id"`
		Name *string `json:"name"`
	} `json:"sender"`
	FromMe        bool    `json:"from_me"`
	TS            string  `json:"ts"`
	Text          *string `json:"text"`
	TextDisplay   *string `json:"text_display"`
	TextTruncated bool    `json:"text_truncated"`
	ReplyTo       *string `json:"reply_to"`
	QuoteVerified bool    `json:"quote_verified"`
	Revoked       bool    `json:"revoked"`
	Origin        string  `json:"origin"`
	Untrusted     bool    `json:"untrusted"`
}

type pageView struct {
	Chats []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"chats"`
	Messages  []messageView `json:"messages"`
	Next      *string       `json:"next"`
	More      bool          `json:"more"`
	Truncated bool          `json:"truncated"`
	Session   sessionView   `json:"session"`
}

func TestMeDescribesTheClientsOwnScope(t *testing.T) {
	f := newReadFixture(t)
	rec := f.get(t, keyAlice, "/v1/me")
	want := `{"client":{"id":"client-alice","name":"client-alice-name","expires_at":"2026-01-03T03:04:05Z","read":{"all":false,"chats":[{"id":"15550100001@s.whatsapp.net","kind":"phone"}]},"write":{"chats":[{"id":"15550100001@s.whatsapp.net","kind":"phone"}]}},"session":{"state":"connected"}}`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("GET /v1/me = %d %s, want %s", rec.Code, rec.Body.String(), want)
	}
	requireSecurityHeaders(t, rec.Header())
	f.state = "logged_out_by_a_bug"
	rec = f.get(t, keyAll, "/v1/me")
	want = `{"client":{"id":"client-all","name":"client-all-name","expires_at":"2026-01-03T03:04:05Z","read":{"all":true,"chats":[]},"write":{"chats":[]}},"session":{"state":"disconnected"}}`
	if rec.Body.String() != want {
		t.Fatalf("GET /v1/me = %s, want %s: an unknown session state reads as disconnected", rec.Body.String(), want)
	}
}

func TestChatsListOnlyTheGrantAndPageWithSealedCursors(t *testing.T) {
	f := newReadFixture(t)
	page := decodeBody[pageView](t, f.get(t, keyAll, "/v1/chats?limit=2"))
	if len(page.Chats) != 2 || page.Chats[0].ID != groupRef || page.Chats[1].ID != aliceRef || page.Next == nil || page.Truncated || page.Session.State != "connected" {
		t.Fatalf("first page = %+v", page)
	}
	if strings.Contains(*page.Next, "15550100") || len(*page.Next) > cursor.MaxCursor {
		t.Fatalf("cursor %q is not opaque and short", *page.Next)
	}
	page = decodeBody[pageView](t, f.get(t, keyAll, "/v1/chats?limit=2&cursor="+*page.Next))
	if len(page.Chats) != 1 || page.Chats[0].ID != bobRef || page.Next != nil {
		t.Fatalf("second page = %+v", page)
	}
	page = decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats"))
	if len(page.Chats) != 1 || page.Chats[0].ID != groupRef || page.Chats[0].Kind != "group" {
		t.Fatalf("scoped listing = %+v", page)
	}
	rec := f.get(t, keyGroup, "/v1/chats/"+groupRef)
	want := `{"id":"11111111111111111111111111111111","kind":"group","name":"Synthetic Group","name_source":"group_subject","last_message_at":"2026-01-02T03:04:05.000Z"}`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("GET /v1/chats/{ref} = %d %s, want %s", rec.Code, rec.Body.String(), want)
	}
}

func TestQueryParsingIsStrict(t *testing.T) {
	f := newReadFixture(t)
	targets := map[string][]string{
		"/v1/me":                              {"limit=5", "x=1"},
		"/v1/chats":                           {"limit=0", "limit=201", "limit=+5", "limit=05", "limit=five", "limit=-1", "limit=", "limit=5&limit=6", "cursor=", "sort=asc", "%zz=1", "q=abc"},
		"/v1/chats/" + groupRef:               {"limit=5"},
		"/v1/chats/" + groupRef + "/messages": {"limit=999", "cursor=a&cursor=b", "chat=" + groupRef},
		"/v1/search":                          {"", "q=ab", "q=abc&q=abd", "q=" + strings.Repeat("a", 129), "q=abc%00def", "q=" + url.QueryEscape("abc \x01 def"), "q=" + url.QueryEscape("\xff\xfe\xfd"), "q=abc&chat=", "q=abc&since=x", "q=abc&limit=0"},
		"/v1/changes":                         {"since=", "chat=", "cursor=x", "limit=201", "q=abc"},
		"/v1/messages/m1_x":                   {"limit=1"},
	}
	for target, queries := range targets {
		for _, q := range queries {
			t.Run(target+"?"+q, func(t *testing.T) {
				f.archive.trace()
				f.clock.advance(time.Minute)
				rec := f.get(t, keyAll, target+"?"+q)
				requireError(t, rec, http.StatusBadRequest, codeInvalidQuery)
				if calls := f.archive.trace(); len(calls) != 0 {
					t.Fatalf("a malformed query reached the archive: %v", calls)
				}
			})
		}
	}
}

func TestEveryNotFoundIsByteIdentical(t *testing.T) {
	f := newReadFixture(t)
	reference := f.get(t, keyGroup, "/v1/no-such-route")
	requireError(t, reference, http.StatusNotFound, codeNotFound)
	foreignRef, err := f.refs.SealRef("client-alice", cursor.Ref{Chat: groupChat.JID(), ID: "SYNTHETIC1", Sender: aliceChat.JID()})
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	outOfScopeRef, err := f.refs.SealRef("client-group", cursor.Ref{Chat: aliceChat.JID(), ID: "SYNTHETIC11", Sender: aliceChat.JID()})
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	missingRef, err := f.refs.SealRef("client-group", cursor.Ref{Chat: groupChat.JID(), ID: "SYNTHETIC99", Sender: aliceChat.JID()})
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	unnormalRef, err := f.refs.SealRef("client-group", cursor.Ref{Chat: "not-a-chat", ID: "SYNTHETIC1", Sender: aliceChat.JID()})
	if err != nil {
		t.Fatalf("SealRef: %v", err)
	}
	staleCursor := "AQoLDA0OAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	targets := map[string][]string{
		"chat":            {"/v1/chats/" + aliceRef, "/v1/chats/" + noRef, "/v1/chats/x", "/v1/chats/%00", "/v1/chats/" + strings.Repeat("f", 4096)},
		"messages":        {"/v1/chats/" + aliceRef + "/messages", "/v1/chats/" + noRef + "/messages", "/v1/chats/x/messages"},
		"stale cursor":    {"/v1/chats/" + aliceRef + "/messages?cursor=" + staleCursor, "/v1/chats/" + noRef + "/messages?cursor=" + staleCursor},
		"search filter":   {"/v1/search?q=synthetic&chat=" + aliceRef, "/v1/search?q=synthetic&chat=" + noRef, "/v1/search?q=synthetic&chat=x", "/v1/search?q=synthetic&chat=*"},
		"search cursor":   {"/v1/search?q=synthetic&chat=" + aliceRef + "&cursor=" + staleCursor, "/v1/search?q=synthetic&chat=" + noRef + "&cursor=garbage"},
		"changes filter":  {"/v1/changes?chat=" + aliceRef, "/v1/changes?chat=" + noRef, "/v1/changes?chat=x"},
		"changes since":   {"/v1/changes?chat=" + aliceRef + "&since=" + staleCursor, "/v1/changes?chat=" + noRef + "&since=2026-13-01T00:00:00Z"},
		"message":         {"/v1/messages/" + foreignRef, "/v1/messages/" + outOfScopeRef, "/v1/messages/" + missingRef, "/v1/messages/" + unnormalRef, "/v1/messages/garbage", "/v1/messages/m1_", "/v1/messages/" + foreignRef[:len(foreignRef)-2]},
		"wrong prefix":    {"/v1/messages/" + strings.TrimPrefix(missingRef, cursor.RefPrefix)},
		"unrouted method": {"/v1/chats/" + groupRef + "/messages/extra"},
	}
	for name, list := range targets {
		for i, target := range list {
			t.Run(fmt.Sprintf("%s/%d", name, i), func(t *testing.T) {
				requireSameResponse(t, f.get(t, keyGroup, target), reference)
			})
		}
	}
	f.clients["synthetic-expired-between"] = &policy.Client{ID: "client-expired", ReadAll: true, ExpiresAt: testNow}
	requireSameResponse(t, f.get(t, "synthetic-expired-between", "/v1/chats"), reference)
}

func TestDeniedAndMissingTakeTheSameArchivePath(t *testing.T) {
	f := newReadFixture(t)
	for _, tt := range []struct{ denied, missing string }{
		{"/v1/chats/" + aliceRef, "/v1/chats/" + noRef},
		{"/v1/chats/" + aliceRef + "/messages", "/v1/chats/" + noRef + "/messages"},
		{"/v1/chats/" + aliceRef + "/messages?cursor=garbage", "/v1/chats/" + noRef + "/messages?cursor=garbage"},
		{"/v1/search?q=synthetic&chat=" + aliceRef, "/v1/search?q=synthetic&chat=" + noRef},
		{"/v1/changes?chat=" + aliceRef + "&since=garbage", "/v1/changes?chat=" + noRef + "&since=garbage"},
	} {
		f.archive.trace()
		denied := f.get(t, keyGroup, tt.denied)
		deniedCalls := f.archive.trace()
		missing := f.get(t, keyGroup, tt.missing)
		missingCalls := f.archive.trace()
		requireSameResponse(t, denied, missing)
		if !slices.Equal(deniedCalls, missingCalls) || len(deniedCalls) == 0 {
			t.Fatalf("%s took %v, %s took %v", tt.denied, deniedCalls, tt.missing, missingCalls)
		}
	}
}

func TestMessagesCarryTheFixedShape(t *testing.T) {
	f := newReadFixture(t)
	f.archive.messages[0].FromMe = true
	f.archive.messages[1].Revoked, f.archive.messages[1].Text, f.archive.messages[1].TextDisplay = true, "", ""
	f.archive.messages[2].SavedName = "Saved Alice"
	f.archive.messages[3].ReplyID, f.archive.messages[3].ReplySender, f.archive.messages[3].QuoteVerified = "SYNTHETIC1", aliceChat, true
	f.archive.messages[4].Text = ""
	page := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=5"))
	if len(page.Messages) != 5 || page.Next == nil || page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	for i, m := range page.Messages {
		if !m.Untrusted || m.Chat != groupRef || m.Sender.ID != aliceChat.JID() {
			t.Fatalf("message %d = %+v", i, m)
		}
		if want := "peer"; i != 0 && m.Origin != want {
			t.Fatalf("message %d origin = %q, want %q", i, m.Origin, want)
		}
	}
	if m := page.Messages[0]; !m.FromMe || m.Origin != "owner" || m.TS != "2026-01-02T03:14:05.000Z" {
		t.Fatalf("own message = %+v", m)
	}
	if m := page.Messages[1]; !m.Revoked || m.Text != nil || m.TextDisplay != nil {
		t.Fatalf("revoked message = %+v", m)
	}
	if m := page.Messages[2]; m.Sender.Name == nil || *m.Sender.Name != "Saved Alice" {
		t.Fatalf("saved name = %+v", m.Sender)
	}
	if m := page.Messages[1]; m.Sender.Name == nil || *m.Sender.Name != "Push 9" {
		t.Fatalf("push name = %+v", m.Sender)
	}
	if m := page.Messages[4]; m.Text != nil || m.TextDisplay == nil {
		t.Fatalf("empty text = %+v, want null text and a display text", m)
	}
	reply := page.Messages[3]
	if reply.ReplyTo == nil || !reply.QuoteVerified {
		t.Fatalf("reply = %+v", reply)
	}
	quoted := decodeBody[messageView](t, f.get(t, keyGroup, "/v1/messages/"+*reply.ReplyTo))
	if quoted.Text == nil || *quoted.Text != "synthetic text 1" || quoted.ReplyTo != nil {
		t.Fatalf("quoted message = %+v", quoted)
	}
	self := decodeBody[messageView](t, f.get(t, keyGroup, "/v1/messages/"+page.Messages[2].Ref))
	if self.Text == nil || *self.Text != "synthetic text 8" {
		t.Fatalf("message by reference = %+v", self)
	}
	next := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=5&cursor="+*page.Next))
	if len(next.Messages) != 5 || *next.Messages[0].Text != "synthetic text 5" || next.Next != nil {
		t.Fatalf("second page = %+v", next)
	}
}

func TestInvalidCursorsAreRefusedAfterTheScopeCheck(t *testing.T) {
	f := newReadFixture(t)
	page := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=2"))
	other := decodeBody[pageView](t, f.get(t, keyAll, "/v1/chats?limit=1"))
	for name, target := range map[string]string{
		"garbage":                   "/v1/chats/" + groupRef + "/messages?cursor=garbage",
		"another client's cursor":   "/v1/chats/" + groupRef + "/messages?cursor=" + *page.Next,
		"another endpoint's cursor": "/v1/chats/" + groupRef + "/messages?cursor=" + *other.Next,
		"chats with garbage":        "/v1/chats?cursor=garbage",
		"search with garbage":       "/v1/search?q=synthetic&cursor=garbage",
		"changes with garbage":      "/v1/changes?since=garbage",
		"changes with a bad time":   "/v1/changes?since=2026-01-02T03:04:05",
	} {
		t.Run(name, func(t *testing.T) {
			key := keyGroup2
			if !strings.Contains(name, "another client") {
				key = keyGroup
			}
			requireError(t, f.get(t, key, target), http.StatusBadRequest, codeInvalidCursor)
		})
	}
	search := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/search?q=synthetic&limit=1"))
	if search.Next == nil || !search.More {
		t.Fatalf("search page = %+v", search)
	}
	requireError(t, f.get(t, keyGroup, "/v1/search?q=synthetix&limit=1&cursor="+*search.Next), http.StatusBadRequest, codeInvalidCursor)
	requireError(t, f.get(t, keyGroup, "/v1/search?q=synthetic&chat="+groupRef+"&limit=1&cursor="+*search.Next), http.StatusBadRequest, codeInvalidCursor)
	if more := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/search?q=synthetic&limit=1&cursor="+*search.Next)); len(more.Messages) != 1 || *more.Messages[0].Text != "synthetic text 9" {
		t.Fatalf("second search page = %+v", more)
	}
	messages := decodeBody[pageView](t, f.get(t, keyAll, "/v1/chats/"+groupRef+"/messages?limit=1"))
	changes := decodeBody[pageView](t, f.get(t, keyAll, "/v1/changes?chat="+groupRef+"&since=2030-01-01T00:00:00Z"))
	if messages.Next == nil || changes.Next == nil {
		t.Fatalf("messages page = %+v, changes page = %+v", messages, changes)
	}
	for name, target := range map[string]string{
		"a messages cursor on another chat":    "/v1/chats/" + aliceRef + "/messages?cursor=" + *messages.Next,
		"a changes cursor on another chat":     "/v1/changes?chat=" + aliceRef + "&since=" + *changes.Next,
		"a changes cursor without its chat":    "/v1/changes?since=" + *changes.Next,
		"a changes cursor on its chat's pages": "/v1/chats/" + groupRef + "/messages?cursor=" + *changes.Next,
		"a messages cursor on its chat's feed": "/v1/changes?chat=" + groupRef + "&since=" + *messages.Next,
	} {
		t.Run(name, func(t *testing.T) {
			requireError(t, f.get(t, keyAll, target), http.StatusBadRequest, codeInvalidCursor)
		})
	}
}

func TestReplaysUnderAChangedGrantNeverWidenScope(t *testing.T) {
	f := newReadFixture(t)
	page := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=2"))
	all := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/changes?limit=2"))
	search := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/search?q=synthetic&limit=1"))
	mref := page.Messages[0].Ref
	reference := f.get(t, keyGroup, "/v1/no-such-route")

	f.clients[keyGroup] = readClient("client-group", bobChat)
	for _, target := range []string{
		"/v1/chats/" + groupRef + "/messages?limit=2&cursor=" + *page.Next,
		"/v1/messages/" + mref,
	} {
		requireSameResponse(t, f.get(t, keyGroup, target), reference)
	}
	for _, target := range []string{"/v1/changes?limit=2&since=" + *all.Next, "/v1/search?q=synthetic&limit=200&cursor=" + *search.Next} {
		got := decodeBody[pageView](t, f.get(t, keyGroup, target))
		for _, m := range got.Messages {
			if m.Chat != bobRef {
				t.Fatalf("%s replayed under the new grant returned %+v", target, m)
			}
		}
	}
}

func TestChangesStartFromATimeAndAlwaysCarryANextCursor(t *testing.T) {
	f := newReadFixture(t)
	since := testNow.Add(8 * time.Minute).Format(time.RFC3339)
	page := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/changes?since="+since))
	if len(page.Messages) != 3 || *page.Messages[0].Text != "synthetic text 8" || page.Next == nil || page.More {
		t.Fatalf("changes since %s = %+v", since, page)
	}
	tail := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/changes?since="+*page.Next))
	if len(tail.Messages) != 0 || tail.Next == nil {
		t.Fatalf("tail = %+v, want no rows and a cursor to poll again", tail)
	}
	f.archive.messages[0].Change.ChangeSeq = 50
	tail = decodeBody[pageView](t, f.get(t, keyGroup, "/v1/changes?since="+*tail.Next))
	if len(tail.Messages) != 1 || *tail.Messages[0].Text != "synthetic text 10" {
		t.Fatalf("tail after an edit = %+v", tail)
	}
	if early := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/changes?since=1900-01-01T00:00:00Z&limit=200")); len(early.Messages) != 10 {
		t.Fatalf("changes since 1900 = %d rows, want all 10", len(early.Messages))
	}
}

func TestReadBudgetIsChargedBeforeRouting(t *testing.T) {
	f := newReadFixture(t)
	for i := range readBurst {
		target := "/v1/me"
		if i%2 == 1 {
			target = "/v1/chats/" + noRef
		}
		if rec := f.get(t, keyGroup, target); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	rows := len(f.audit.recorded())
	rec := f.get(t, keyGroup, "/v1/no-such-route")
	requireError(t, rec, http.StatusTooManyRequests, codeRateLimited)
	requireSecurityHeaders(t, rec.Header())
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := len(f.audit.recorded()); got != rows {
		t.Fatalf("a request refused by the read budget wrote %d audit rows", got-rows)
	}
	if rec := f.get(t, keyGroup2, "/v1/me"); rec.Code != http.StatusOK {
		t.Fatalf("a second client was refused: %d", rec.Code)
	}
	if rec := serve(f.handler, newRequest(t, http.MethodGet, "/v1/me", bearer("synthetic-unknown-key"))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown key = %d, want 401 from authentication, not the budget", rec.Code)
	}
	f.clock.advance(100 * time.Millisecond)
	if rec := f.get(t, keyGroup, "/v1/me"); rec.Code != http.StatusOK {
		t.Fatalf("one interval later = %d, want 200", rec.Code)
	}
	requireError(t, f.get(t, keyGroup, "/v1/me"), http.StatusTooManyRequests, codeRateLimited)
}

func TestSearchIsChargedToBothBudgets(t *testing.T) {
	f := newReadFixture(t)
	for range searchBurst {
		if rec := f.get(t, keyGroup, "/v1/search?q=synthetic"); rec.Code != http.StatusOK {
			t.Fatalf("search = %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := f.get(t, keyGroup, "/v1/search?q=synthetic")
	requireError(t, rec, http.StatusTooManyRequests, codeRateLimited)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if events := f.audit.recorded(); events[len(events)-1].Reason != codeRateLimited || events[len(events)-1].Action != actionSearch {
		t.Fatalf("last audit event = %+v", events[len(events)-1])
	}
	for i := range readBurst - searchBurst - 1 {
		if rec := f.get(t, keyGroup, "/v1/me"); rec.Code != http.StatusOK {
			t.Fatalf("read %d after the searches = %d", i+1, rec.Code)
		}
	}
	requireError(t, f.get(t, keyGroup, "/v1/me"), http.StatusTooManyRequests, codeRateLimited)
}

func TestBusyReadsAnswerAtOnce(t *testing.T) {
	f := newReadFixture(t)
	f.archive.err = fmt.Errorf("%w: synthetic", scoped.ErrBusy)
	for _, target := range []string{"/v1/chats", "/v1/chats/" + groupRef, "/v1/search?q=synthetic", "/v1/changes"} {
		rec := f.get(t, keyGroup, target)
		requireError(t, rec, http.StatusServiceUnavailable, codeBusy)
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("%s: Retry-After = %q, want 1", target, got)
		}
	}
	f.archive.err = fmt.Errorf("synthetic failure naming %s", groupChat.JID())
	rec := f.get(t, keyGroup, "/v1/chats")
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
}

func TestAuditRecordsEveryRoutedRequest(t *testing.T) {
	f := newReadFixture(t)
	mref := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=1")).Messages[0].Ref
	f.get(t, keyGroup, "/v1/chats/"+aliceRef)
	f.get(t, keyGroup, "/v1/chats?limit=0")
	f.get(t, keyGroup, "/v1/chats?cursor=garbage")
	f.get(t, keyGroup, "/v1/messages/"+mref)
	f.get(t, keyGroup, "/v1/no-such-route")
	serve(f.handler, newRequest(t, http.MethodPost, "/v1/me", bearer(keyGroup)))
	got := f.audit.recorded()
	want := []ReadEvent{
		{Action: actionMessages, Chat: groupChat, OK: true, Reason: outcomeOK},
		{Action: actionChat, Reason: codeNotFound},
		{Action: actionChats, Reason: codeInvalidQuery},
		{Action: actionChats, Reason: codeInvalidCursor},
		{Action: actionMessage, Chat: groupChat, OK: true, Reason: outcomeOK},
		{Action: actionUnrouted, Reason: codeNotFound},
		{Action: actionUnrouted, Reason: codeMethodNotAllowed},
	}
	if len(got) != len(want) {
		t.Fatalf("audit = %+v, want %d events", got, len(want))
	}
	for i := range want {
		want[i].At, want[i].Client, want[i].Peer = testNow, "client-group", "192.0.2.1"
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAFailedAuditFailsTheRequestClosed(t *testing.T) {
	f := newReadFixture(t)
	f.audit.fail = true
	for _, target := range []string{"/v1/chats/" + groupRef + "/messages", "/v1/chats/" + aliceRef, "/v1/no-such-route", "/v1/search?q=synthetic"} {
		rec := f.get(t, keyGroup, target)
		requireError(t, rec, http.StatusInternalServerError, codeInternal)
		requireSecurityHeaders(t, rec.Header())
		if rec.Header().Get("Retry-After") != "" {
			t.Fatalf("%s: a refused audit kept Retry-After", target)
		}
	}
	for range searchBurst {
		f.get(t, keyAll, "/v1/search?q=synthetic")
	}
	rec := f.get(t, keyAll, "/v1/search?q=synthetic")
	requireError(t, rec, http.StatusInternalServerError, codeInternal)
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("a refused audit of a rate-limited search kept Retry-After")
	}
}

func TestTextAndPagesAreCut(t *testing.T) {
	f := newReadFixture(t)
	escaped := strings.Repeat("<", 40<<10)
	f.archive.messages = nil
	for seq := int64(200); seq >= 1; seq-- {
		m := syntheticMessage(groupChat, groupRef, seq, aliceChat)
		m.Text, m.TextDisplay = strings.Repeat("é", 20<<10)+"tail", escaped
		f.archive.messages = append(f.archive.messages, m)
	}
	rec := f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=200")
	if rec.Body.Len() > maxPageBytes {
		t.Fatalf("page = %d bytes, above %d", rec.Body.Len(), maxPageBytes)
	}
	page := decodeBody[pageView](t, rec)
	if !page.Truncated || page.Next == nil || len(page.Messages) == 0 || len(page.Messages) >= 200 {
		t.Fatalf("page holds %d messages, truncated %v, next %v", len(page.Messages), page.Truncated, page.Next)
	}
	for _, m := range page.Messages {
		text, _ := json.Marshal(*m.Text)
		display, _ := json.Marshal(*m.TextDisplay)
		if !m.TextTruncated || len(text) > maxTextBytes+2 || len(display) > maxTextBytes+2 || strings.HasSuffix(*m.Text, "tail") {
			t.Fatalf("text %d and display %d encoded bytes, truncated %v", len(text), len(display), m.TextTruncated)
		}
	}
	next := decodeBody[pageView](t, f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages?limit=200&cursor="+*page.Next))
	if want := "2026-01-02T" + testNow.Add(time.Duration(200-len(page.Messages))*time.Minute).Format("15:04:05") + ".000Z"; next.Messages[0].TS != want {
		t.Fatalf("the next page starts at %s, want the first dropped message at %s", next.Messages[0].TS, want)
	}
	single := strings.Repeat(" ", 40<<10)
	f.archive.messages = []scoped.Message{syntheticMessage(groupChat, groupRef, 1, aliceChat)}
	f.archive.messages[0].Text, f.archive.messages[0].TextDisplay = single, single
	rec = f.get(t, keyGroup, "/v1/chats/"+groupRef+"/messages")
	if one := decodeBody[pageView](t, rec); len(one.Messages) != 1 || one.Truncated || !one.Messages[0].TextTruncated {
		t.Fatalf("a single heavy message = %d messages, truncated %v", len(one.Messages), one.Truncated)
	}
}

func FuzzReadQuery(f *testing.F) {
	for _, seed := range []string{"", "limit=50", "q=abc&chat=x&cursor=y&limit=1", "limit=1&limit=2", "%zz", "a;b", "q=%00"} {
		f.Add(seed)
	}
	allowed := []string{"q", "chat", "cursor", "limit"}
	f.Fuzz(func(t *testing.T, raw string) {
		r := &Request{req: httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/search", nil)}
		r.req.URL.RawQuery = raw
		q, err := params(r, allowed...)
		if err != nil {
			if err != errBadQuery {
				t.Fatalf("params refused with %v", err)
			}
			return
		}
		values, err := url.ParseQuery(raw)
		if err != nil || len(values) != len(q) {
			t.Fatalf("params accepted %q as %v", raw, q)
		}
		for key, v := range q {
			if !slices.Contains(allowed, key) || v == "" || len(values[key]) != 1 || values[key][0] != v {
				t.Fatalf("params accepted %q with %s=%q", raw, key, v)
			}
		}
		if n, err := pageLimit(q); err == nil && (n < 1 || n > scoped.MaxLimit) {
			t.Fatalf("pageLimit accepted %d", n)
		}
	})
}
