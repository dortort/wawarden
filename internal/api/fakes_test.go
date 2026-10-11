package api

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const sealKeyID = "0a1b2c3d"

func testSealers(t testing.TB) (cursors, refs *cursor.Sealer) {
	t.Helper()
	var err error
	if cursors, err = cursor.New(bytes.Repeat([]byte{0x11}, 32), sealKeyID); err != nil {
		t.Fatalf("cursor.New: %v", err)
	}
	if refs, err = cursor.NewRef(bytes.Repeat([]byte{0x22}, 32), sealKeyID); err != nil {
		t.Fatalf("cursor.NewRef: %v", err)
	}
	return cursors, refs
}

func testClientDeps(t testing.TB, auth Authenticator, now func() time.Time) ClientDeps {
	t.Helper()
	cursors, refs := testSealers(t)
	return ClientDeps{
		Authenticator: auth, Metrics: metrics.NewRegistry(), Now: now, Archive: &fakeArchive{}, Audit: &fakeAuditor{},
		Session: func() string { return "connected" }, Cursors: cursors, Refs: refs,
	}
}

type fakeAuditor struct {
	mu     sync.Mutex
	events []ReadEvent
	fail   bool
}

func (a *fakeAuditor) Record(_ context.Context, e ReadEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("synthetic audit failure")
	}
	a.events = append(a.events, e)
	return nil
}

func (a *fakeAuditor) recorded() []ReadEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.events)
}

type fakeArchive struct {
	mu       sync.Mutex
	chats    []scoped.Chat
	messages []scoped.Message
	err      error
	calls    []string
}

func (f *fakeArchive) call(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.err
}

func (f *fakeArchive) trace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeArchive) chat(g policy.ReadGrant, ref string) (scoped.Chat, bool) {
	for _, c := range f.chats {
		if c.Ref == ref && g.Allows(c.Chat) {
			return c, true
		}
	}
	return scoped.Chat{}, false
}

func (f *fakeArchive) inScope(g policy.ReadGrant, ref string) []scoped.Message {
	var out []scoped.Message
	for _, m := range f.messages {
		if g.Allows(m.Chat) && (ref == "" || m.ChatRef == ref) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeArchive) Chats(g policy.ReadGrant, _ context.Context, pos scoped.ChatPosition, limit int) (scoped.ChatPage, error) {
	if err := f.call("Chats"); err != nil {
		return scoped.ChatPage{}, err
	}
	var page scoped.ChatPage
	for _, c := range f.chats {
		if g.Allows(c.Chat) && (pos.Row == 0 || c.Position.Row < pos.Row) {
			page.Chats = append(page.Chats, c)
		}
	}
	if len(page.Chats) > limit {
		page.Chats, page.More = page.Chats[:limit], true
		page.Next = page.Chats[limit-1].Position
	}
	return page, nil
}

func (f *fakeArchive) Chat(g policy.ReadGrant, _ context.Context, ref string) (scoped.Chat, bool, error) {
	if err := f.call("Chat"); err != nil {
		return scoped.Chat{}, false, err
	}
	c, ok := f.chat(g, ref)
	return c, ok, nil
}

func (f *fakeArchive) Messages(g policy.ReadGrant, _ context.Context, ref string, pos scoped.MessagePosition, _ scoped.Direction, limit int) (scoped.MessagePage, bool, error) {
	if err := f.call("Messages"); err != nil {
		return scoped.MessagePage{}, false, err
	}
	c, ok := f.chat(g, ref)
	if !ok {
		return scoped.MessagePage{}, false, nil
	}
	page := scoped.MessagePage{Chat: c}
	for _, m := range f.inScope(g, ref) {
		if pos.Seq == 0 || m.Position.Seq < pos.Seq {
			page.Messages = append(page.Messages, m)
		}
	}
	if len(page.Messages) > limit {
		page.Messages, page.More = page.Messages[:limit], true
		page.Next = page.Messages[limit-1].Position
	}
	return page, true, nil
}

func (f *fakeArchive) Message(g policy.ReadGrant, _ context.Context, chat policy.CanonicalChat, id string, sender policy.CanonicalChat) (scoped.Message, bool, error) {
	if err := f.call("Message"); err != nil {
		return scoped.Message{}, false, err
	}
	if !g.Allows(chat) {
		return scoped.Message{}, false, nil
	}
	for _, m := range f.messages {
		if m.Chat == chat && m.ID == id && m.Sender == sender {
			return m, true, nil
		}
	}
	return scoped.Message{}, false, nil
}

func (f *fakeArchive) Changes(g policy.ReadGrant, _ context.Context, ref string, pos scoped.ChangePosition, limit int) (scoped.ChangePage, bool, error) {
	if err := f.call("Changes"); err != nil {
		return scoped.ChangePage{}, false, err
	}
	if _, ok := f.chat(g, ref); ref != "" && !ok {
		return scoped.ChangePage{}, false, nil
	}
	rows := f.inScope(g, ref)
	slices.SortFunc(rows, func(a, b scoped.Message) int { return int(a.Change.ChangeSeq - b.Change.ChangeSeq) })
	page := scoped.ChangePage{Next: pos}
	for _, m := range rows {
		if m.Change.ChangeSeq > pos.ChangeSeq && m.At.UnixMilli() >= pos.Since {
			page.Messages = append(page.Messages, m)
			page.Next = m.Change
		}
	}
	if len(page.Messages) > limit {
		page.Messages, page.More = page.Messages[:limit], true
		page.Next = page.Messages[limit-1].Change
	}
	return page, true, nil
}

func (f *fakeArchive) Search(g policy.ReadGrant, _ context.Context, query scoped.Query, ref string, pos scoped.SearchPosition, limit int) (scoped.SearchPage, bool, error) {
	if err := f.call("Search"); err != nil {
		return scoped.SearchPage{}, false, err
	}
	if _, ok := f.chat(g, ref); ref != "" && !ok {
		return scoped.SearchPage{}, false, nil
	}
	term := strings.Trim(strings.SplitN(query.Expression(), " AND ", 2)[0], `"`)
	var page scoped.SearchPage
	for _, m := range f.inScope(g, ref) {
		if (pos.Upper == 0 || m.Position.Seq < pos.Upper) && !m.Revoked && strings.Contains(m.Text, term) {
			page.Messages = append(page.Messages, m)
		}
	}
	if len(page.Messages) > limit {
		page.Messages, page.More = page.Messages[:limit], true
		page.Next = scoped.SearchPosition{Upper: page.Messages[limit-1].Position.Seq}
	}
	return page, true, nil
}
