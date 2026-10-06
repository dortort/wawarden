package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/ratelimit"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	actionMe       = "rest.me"
	actionChats    = "rest.chats"
	actionChat     = "rest.chat"
	actionMessages = "rest.messages"
	actionSearch   = "rest.search"
	actionChanges  = "rest.changes"
	actionMessage  = "rest.message"

	endpointChats    = "chats"
	endpointMessages = "messages"
	endpointSearch   = "search"
	endpointChanges  = "changes"
	allChats         = "*"

	DefaultReadsPerMinute    = 600
	DefaultSearchesPerMinute = 60
	readBurst                = 60
	searchBurst              = 6

	defaultPageLimit = 50
	maxPageBytes     = 256 << 10
	envelopeBytes    = 1 << 10
	maxTextBytes     = 32 << 10

	originOwner = "owner"
	originPeer  = "peer"

	stateDisconnected = "disconnected"
	readTimeLayout    = "2006-01-02T15:04:05.000Z07:00"
)

var (
	errNotFound  = &codedError{status: http.StatusNotFound, code: codeNotFound}
	errBadCursor = &codedError{status: http.StatusBadRequest, code: codeInvalidCursor}
	errBusy      = &codedError{status: http.StatusServiceUnavailable, code: codeBusy, retryAfter: time.Second}
	errNoClient  = errors.New("api: a read route ran without an authenticated client")
)

var sessionStates = []string{"unpaired", "connecting", "connected", stateDisconnected}

type ReadArchive interface {
	Chats(g policy.ReadGrant, ctx context.Context, pos scoped.ChatPosition, limit int) (scoped.ChatPage, error)
	Chat(g policy.ReadGrant, ctx context.Context, ref string) (scoped.Chat, bool, error)
	Messages(g policy.ReadGrant, ctx context.Context, ref string, pos scoped.MessagePosition, dir scoped.Direction, limit int) (scoped.MessagePage, bool, error)
	Message(g policy.ReadGrant, ctx context.Context, chat policy.CanonicalChat, id string, sender policy.CanonicalChat) (scoped.Message, bool, error)
	Changes(g policy.ReadGrant, ctx context.Context, ref string, pos scoped.ChangePosition, limit int) (scoped.ChangePage, bool, error)
	Search(g policy.ReadGrant, ctx context.Context, query scoped.Query, ref string, pos scoped.SearchPosition, limit int) (scoped.SearchPage, bool, error)
}

type ReadLimits struct {
	ReadsPerMinute    int
	SearchesPerMinute int
}

type reads struct {
	archive  ReadArchive
	cursors  *cursor.Sealer
	refs     *cursor.Sealer
	searches *ratelimit.Limiter
	session  func() string
}

type readHandler func(context.Context, policy.ReadGrant, *Request) (dto.Response, policy.CanonicalChat, error)

func readBudgets(l ReadLimits, now func() time.Time) (reads, searches *ratelimit.Limiter) {
	perRead := cmp.Or(l.ReadsPerMinute, DefaultReadsPerMinute)
	perSearch := cmp.Or(l.SearchesPerMinute, DefaultSearchesPerMinute)
	return ratelimit.New(perRead, min(readBurst, perRead), now), ratelimit.New(perSearch, min(searchBurst, perSearch), now)
}

func registerReads(rt *router, s *reads) {
	rt.read("GET /v1/me", noted(actionMe, s.me))
	rt.read("GET /v1/chats", noted(actionChats, s.chats))
	rt.read("GET /v1/chats/{ref}", noted(actionChat, s.chat))
	rt.read("GET /v1/chats/{ref}/messages", noted(actionMessages, s.messages))
	rt.read("GET /v1/search", noted(actionSearch, s.search))
	rt.read("GET /v1/changes", noted(actionChanges, s.changes))
	rt.read("GET /v1/messages/{mref}", noted(actionMessage, s.message))
}

func noted(action string, h readHandler) func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
	return func(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, error) {
		ignoreBody(r.w, r.req)
		resp, chat, err := h(ctx, g, r)
		if errors.Is(err, scoped.ErrBusy) {
			err = errBusy
		}
		if n := noteFrom(ctx); n != nil {
			n.action, n.chat = action, chat
			if refusal, ok := errors.AsType[*codedError](err); ok {
				n.reason = refusal.code
			}
		}
		return resp, err
	}
}

func (s *reads) me(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	if _, err := params(r); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	c := clientFrom(ctx)
	if c == nil {
		return nil, policy.CanonicalChat{}, errNoClient
	}
	return dto.Me{
		Client: dto.MeClient{
			ID: c.ID, Name: c.Name, ExpiresAt: timeText(c.ExpiresAt),
			Read:  dto.ReadScope{All: g.All(), Chats: scopeChats(g.Chats())},
			Write: dto.WriteScope{Chats: scopeChats(c.Write)},
		},
		Session: s.state(),
	}, policy.CanonicalChat{}, nil
}

func (s *reads) chats(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	q, err := params(r, "cursor", "limit")
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	limit, err := pageLimit(q)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	binding := cursor.Binding{Client: g.Client(), Endpoint: endpointChats, Filter: allChats}
	var pos scoped.ChatPosition
	if text, ok := q["cursor"]; ok {
		p, err := s.cursors.OpenCursor(binding, text)
		if err != nil || p[1] < 1 || p[2] < 0 || p[2] > 1 {
			return nil, policy.CanonicalChat{}, errBadCursor
		}
		pos = scoped.ChatPosition{LastTS: p[0], Row: p[1], Undated: p[2] == 1}
	}
	page, err := s.archive.Chats(g, ctx, pos, limit)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	items := make([]dto.Chat, 0, len(page.Chats))
	for _, c := range page.Chats {
		items = append(items, chatItem(c))
	}
	kept, cut, err := fit(items)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	out := dto.ChatPage{Chats: kept, Truncated: cut, Session: s.state()}
	if cut || page.More {
		next := page.Next
		if cut {
			next = page.Chats[len(kept)-1].Position
		}
		undated := int64(0)
		if next.Undated {
			undated = 1
		}
		if out.Next, err = s.seal(binding, cursor.Position{next.LastTS, next.Row, undated}); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
	}
	return out, policy.CanonicalChat{}, nil
}

func (s *reads) chat(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	if _, err := params(r); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	c, found, err := s.archive.Chat(g, ctx, r.req.PathValue("ref"))
	if err := missing(found, err); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	return chatItem(c), c.Chat, nil
}

func (s *reads) messages(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	q, err := params(r, "cursor", "limit")
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	limit, err := pageLimit(q)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	ref := r.req.PathValue("ref")
	binding := cursor.Binding{Client: g.Client(), Endpoint: endpointMessages, Filter: ref}
	var pos scoped.MessagePosition
	if text, ok := q["cursor"]; ok {
		p, err := s.cursors.OpenCursor(binding, text)
		if err != nil || p[1] < 1 || p[2] != int64(scoped.Older) {
			return nil, policy.CanonicalChat{}, s.cursorRefused(ctx, g, ref)
		}
		pos = scoped.MessagePosition{TS: p[0], Seq: p[1]}
	}
	page, found, err := s.archive.Messages(g, ctx, ref, pos, scoped.Older, limit)
	if err := missing(found, err); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	kept, cut, err := s.messageItems(g.Client(), page.Messages)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	out := dto.MessagePage{Messages: kept, Truncated: cut, Session: s.state()}
	if cut || page.More {
		next := page.Next
		if cut {
			next = page.Messages[len(kept)-1].Position
		}
		if out.Next, err = s.seal(binding, cursor.Position{next.TS, next.Seq, int64(scoped.Older)}); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
	}
	return out, page.Chat.Chat, nil
}

func (s *reads) search(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	if wait, ok := s.searches.Allow(g.Client()); !ok {
		return nil, policy.CanonicalChat{}, &codedError{status: http.StatusTooManyRequests, code: codeRateLimited, retryAfter: wait}
	}
	q, err := params(r, "q", "chat", "cursor", "limit")
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	limit, err := pageLimit(q)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	text, ok := q["q"]
	if !ok {
		return nil, policy.CanonicalChat{}, errBadQuery
	}
	query, err := scoped.ParseQuery(text)
	if err != nil {
		return nil, policy.CanonicalChat{}, errBadQuery
	}
	ref := q["chat"]
	binding := cursor.Binding{Client: g.Client(), Endpoint: endpointSearch, Filter: cmp.Or(ref, allChats), Query: query.Expression()}
	var pos scoped.SearchPosition
	if text, ok := q["cursor"]; ok {
		p, err := s.cursors.OpenCursor(binding, text)
		if err != nil || p[0] < 1 || p[1] != 0 || p[2] != 0 {
			return nil, policy.CanonicalChat{}, s.cursorRefused(ctx, g, ref)
		}
		pos = scoped.SearchPosition{Upper: p[0]}
	}
	page, found, err := s.archive.Search(g, ctx, query, ref, pos, limit)
	if err := missing(found, err); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	kept, cut, err := s.messageItems(g.Client(), page.Messages)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	out := dto.SearchPage{Messages: kept, More: cut || page.More, Truncated: cut, Session: s.state()}
	if out.More {
		next := page.Next
		if cut {
			next = scoped.SearchPosition{Upper: page.Messages[len(kept)-1].Position.Seq}
		}
		if out.Next, err = s.seal(binding, cursor.Position{next.Upper}); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
	}
	return out, policy.CanonicalChat{}, nil
}

func (s *reads) changes(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	q, err := params(r, "since", "chat", "limit")
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	limit, err := pageLimit(q)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	ref := q["chat"]
	binding := cursor.Binding{Client: g.Client(), Endpoint: endpointChanges, Filter: cmp.Or(ref, allChats)}
	var pos scoped.ChangePosition
	if since, ok := q["since"]; ok {
		if pos, ok = s.changePosition(binding, since); !ok {
			return nil, policy.CanonicalChat{}, s.cursorRefused(ctx, g, ref)
		}
	}
	page, found, err := s.archive.Changes(g, ctx, ref, pos, limit)
	if err := missing(found, err); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	kept, cut, err := s.messageItems(g.Client(), page.Messages)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	next := page.Next
	if cut {
		next = page.Messages[len(kept)-1].Change
	}
	out := dto.ChangePage{Messages: kept, More: cut || page.More, Truncated: cut, Session: s.state()}
	if out.Next, err = s.seal(binding, cursor.Position{next.ChangeSeq, next.Since}); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	return out, policy.CanonicalChat{}, nil
}

func (s *reads) message(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
	if _, err := params(r); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	ref, err := s.refs.OpenRef(g.Client(), r.req.PathValue("mref"))
	if err != nil {
		return nil, policy.CanonicalChat{}, errNotFound
	}
	chat, chatOK := policy.Normalize(ref.Chat)
	sender, senderOK := policy.Normalize(ref.Sender)
	if !chatOK || !senderOK {
		return nil, policy.CanonicalChat{}, errNotFound
	}
	m, found, err := s.archive.Message(g, ctx, chat, ref.ID, sender)
	if err := missing(found, err); err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	out, err := s.messageItem(g.Client(), m)
	if err != nil {
		return nil, policy.CanonicalChat{}, err
	}
	return out, m.Chat, nil
}

func (s *reads) changePosition(binding cursor.Binding, since string) (scoped.ChangePosition, bool) {
	if strings.Contains(since, ":") {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return scoped.ChangePosition{}, false
		}
		return scoped.ChangePosition{Since: max(t.UnixMilli(), 0)}, true
	}
	p, err := s.cursors.OpenCursor(binding, since)
	if err != nil || p[0] < 0 || p[1] < 0 || p[2] != 0 {
		return scoped.ChangePosition{}, false
	}
	return scoped.ChangePosition{ChangeSeq: p[0], Since: p[1]}, true
}

func (s *reads) cursorRefused(ctx context.Context, g policy.ReadGrant, ref string) error {
	if ref == "" {
		return errBadCursor
	}
	_, found, err := s.archive.Chat(g, ctx, ref)
	switch {
	case err != nil:
		return err
	case !found:
		return errNotFound
	}
	return errBadCursor
}

func missing(found bool, err error) error {
	if err == nil && !found {
		return errNotFound
	}
	return err
}

func (s *reads) seal(b cursor.Binding, p cursor.Position) (*string, error) {
	text, err := s.cursors.Cursor(b, p)
	if err != nil {
		return nil, err
	}
	return &text, nil
}

func (s *reads) state() dto.Session {
	state := s.session()
	if !slices.Contains(sessionStates, state) {
		state = stateDisconnected
	}
	return dto.Session{State: state}
}

func (s *reads) messageItems(client string, rows []scoped.Message) ([]dto.Message, bool, error) {
	items := make([]dto.Message, 0, len(rows))
	for _, m := range rows {
		item, err := s.messageItem(client, m)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	return fit(items)
}

func (s *reads) messageItem(client string, m scoped.Message) (dto.Message, error) {
	ref, err := s.refs.SealRef(client, cursor.Ref{Chat: m.Chat.JID(), ID: m.ID, Sender: m.Sender.JID()})
	if err != nil {
		return dto.Message{}, err
	}
	var reply *string
	if m.ReplyID != "" {
		quoted, err := s.refs.SealRef(client, cursor.Ref{Chat: m.Chat.JID(), ID: m.ReplyID, Sender: m.ReplySender.JID()})
		if err != nil {
			return dto.Message{}, err
		}
		reply = &quoted
	}
	text, textCut := capText(m.Text)
	display, displayCut := capText(m.TextDisplay)
	out := dto.Message{
		Ref: ref, Chat: m.ChatRef, Sender: dto.Sender{ID: m.Sender.JID(), Name: optional(cmp.Or(m.SavedName, m.PushName))},
		FromMe: m.FromMe, TS: readTime(m.At), Kind: m.Kind, MediaType: optional(m.MediaType), ReplyTo: reply,
		QuoteVerified: m.QuoteVerified, EditedAt: optionalReadTime(m.EditedAt), Revoked: m.Revoked, Origin: originPeer, Untrusted: true,
	}
	if m.FromMe {
		out.Origin = originOwner
	}
	if !m.Revoked {
		out.Text, out.TextDisplay, out.TextTruncated = optional(text), optional(display), textCut || displayCut
	}
	return out, nil
}

func chatItem(c scoped.Chat) dto.Chat {
	return dto.Chat{
		ID: c.Ref, Kind: chatKind(c.Chat.Kind()), Name: optional(c.Name), NameSource: optional(c.NameSource), LastMessageAt: optionalReadTime(c.LastAt),
	}
}

func scopeChats(set map[policy.CanonicalChat]struct{}) []dto.ScopeChat {
	out := make([]dto.ScopeChat, 0, len(set))
	for c := range set {
		if c.Valid() {
			out = append(out, dto.ScopeChat{ID: c.JID(), Kind: chatKind(c.Kind())})
		}
	}
	slices.SortFunc(out, func(a, b dto.ScopeChat) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func fit[T dto.Chat | dto.Message](items []T) ([]T, bool, error) {
	room := maxPageBytes - envelopeBytes
	for i, item := range items {
		size, err := dto.Size(item)
		if err != nil {
			return nil, false, err
		}
		if room -= size + 1; room < 0 && i > 0 {
			return items[:i], true, nil
		}
	}
	return items, false, nil
}

func capText(s string) (string, bool) {
	size := 0
	for i, r := range s {
		size += escapedSize(r)
		if size > maxTextBytes {
			return s[:i], true
		}
	}
	return s, false
}

// An upper bound of what encoding/json writes for the rune: HTML-sensitive, control and invalid input becomes a six-byte escape.
func escapedSize(r rune) int {
	switch {
	case r < 0x20, r == '<', r == '>', r == '&', r == ' ', r == ' ', r == utf8.RuneError:
		return 6
	case r == '"', r == '\\':
		return 2
	}
	return utf8.RuneLen(r)
}

func params(r *Request, allowed ...string) (map[string]string, error) {
	values, err := url.ParseQuery(r.req.URL.RawQuery)
	if err != nil {
		return nil, errBadQuery
	}
	out := make(map[string]string, len(values))
	for key, v := range values {
		if !slices.Contains(allowed, key) || len(v) != 1 || v[0] == "" {
			return nil, errBadQuery
		}
		out[key] = v[0]
	}
	return out, nil
}

func pageLimit(q map[string]string) (int, error) {
	v, ok := q["limit"]
	if !ok {
		return defaultPageLimit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v || n < 1 || n > scoped.MaxLimit {
		return 0, errBadQuery
	}
	return n, nil
}

func readTime(t time.Time) string { return t.UTC().Format(readTimeLayout) }

func optionalReadTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := readTime(t)
	return &s
}
