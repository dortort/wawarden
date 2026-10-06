package scoped_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const missingRef = "ffffffffffffffffffffffffffffffff"

var sealedTokens = regexp.MustCompile(`m1_[A-Za-z0-9_-]+|"next":"[A-Za-z0-9_-]*"`)

type httpClients struct {
	mu      sync.Mutex
	clients map[string]*policy.Client
}

func (h *httpClients) Authenticate(_ context.Context, presented string) (*policy.Client, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clients[presented]
	return c, ok, nil
}

func (h *httpClients) set(token string, c *policy.Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[token] = c
}

type discardAudit struct{}

func (discardAudit) Record(context.Context, api.ReadEvent) error { return nil }

type storeAudit struct{ audit *admin.Audit }

func (s storeAudit) Record(ctx context.Context, e api.ReadEvent) error {
	return s.audit.Record(ctx, admin.Event(e))
}

func syntheticMaster(t *testing.T) *keys.Master {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(90 + i)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, r := keys.LoadFile(path)
	if r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	return m
}

type httpHarness struct {
	handler http.Handler
	clients *httpClients
	mu      sync.Mutex
	sql     []string
	bodies  [][]byte
}

func clientHandler(t tb, reader api.ReadArchive, auth api.Authenticator, audit api.Auditor) http.Handler {
	t.Helper()
	cursors, err := cursor.New(bytes.Repeat([]byte{0x31}, 32), "0a1b2c3d")
	if err != nil {
		t.Fatalf("cursor.New: %v", err)
	}
	refs, err := cursor.New(bytes.Repeat([]byte{0x32}, 32), "0a1b2c3d")
	if err != nil {
		t.Fatalf("cursor.New: %v", err)
	}
	var ticks atomic.Int64
	return api.NewClientHandler(api.ClientDeps{
		Authenticator: auth, Metrics: metrics.NewRegistry(), Now: func() time.Time { return epoch.Add(time.Duration(ticks.Add(1)) * time.Second) },
		Archive: reader, Audit: audit,
		Session: func() string { return "connected" }, Cursors: cursors, Refs: refs,
		Limits: api.ReadLimits{ReadsPerMinute: 1 << 30, SearchesPerMinute: 1 << 30},
	})
}

func newHTTPHarness(t *rapid.T, a *archive) *httpHarness {
	h := &httpHarness{clients: &httpClients{clients: map[string]*policy.Client{}}}
	h.handler = clientHandler(t, a.store.Scoped().Traced(func(q string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sql = append(h.sql, q)
	}), h.clients, discardAudit{})
	return h
}

func TestHTTPReadsAnswerBusyInsteadOfQueueing(t *testing.T) {
	type holder func(s *ingest.Store, held chan<- struct{}, release <-chan struct{}) error
	for name, hold := range map[string]holder{
		"every read slot held": func(s *ingest.Store, held chan<- struct{}, release <-chan struct{}) error {
			return s.Scoped().Hold(grantAll(t), context.Background(), held, release)
		},
		"a write held past the read deadline": func(s *ingest.Store, held chan<- struct{}, release <-chan struct{}) error {
			return s.Scoped().HoldWrite(context.Background(), held, release)
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions(t.TempDir())
			opts.ReadSlots, opts.ReadTimeout, opts.Master, opts.AuditOut = 1, 100*time.Millisecond, syntheticMaster(t), &syncBuffer{}
			s := openWith(t, opts)
			auth := &httpClients{clients: map[string]*policy.Client{"synthetic-token": {ID: "clientaa", ReadAll: true, ExpiresAt: epoch.AddDate(1, 0, 0)}}}
			h := clientHandler(t, s.Scoped(), auth, storeAudit{s.Audit()})
			get := func() *httptest.ResponseRecorder {
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/chats", nil)
				r.Header.Set("Authorization", "Bearer synthetic-token")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				return rec
			}
			held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() { done <- hold(s, held, release) }()
			<-held
			start, answered := time.Now(), make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- get() }()
			select {
			case rec := <-answered:
				if took := time.Since(start); took > 5*time.Second {
					t.Fatalf("the busy answer took %v", took)
				}
				if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"error":"busy"}` || rec.Header().Get("Retry-After") != "1" {
					t.Fatalf("a read while %s = %d %s, Retry-After %q", name, rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
				}
			case <-time.After(30 * time.Second):
				close(release)
				<-done
				t.Fatalf("a read while %s waited until it was released", name)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("the held call = %v", err)
			}
			rec := get()
			for range 50 {
				if rec.Code != http.StatusServiceUnavailable {
					break
				}
				rec = get()
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("a read after the release = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func (h *httpHarness) get(t *rapid.T, token, target string) (*httptest.ResponseRecorder, []string) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	h.mu.Lock()
	h.sql = nil
	h.mu.Unlock()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, rec.Body.Bytes())
	return rec, h.sql
}

type httpMessage struct {
	Ref  string  `json:"mref"`
	Chat string  `json:"chat"`
	Text *string `json:"text"`
}

type httpPage struct {
	Chats []struct {
		ID string `json:"id"`
	} `json:"chats"`
	Messages []httpMessage `json:"messages"`
	Next     *string       `json:"next"`
	More     bool          `json:"more"`
}

func (h *httpHarness) page(t *rapid.T, token, target string) httpPage {
	rec, _ := h.get(t, token, target)
	var p httpPage
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body.String())
	}
	return p
}

func (h *httpHarness) walk(t *rapid.T, token, target, key string, limit int) []httpMessage {
	var out []httpMessage
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	next := ""
	for range 1000 {
		q := sep + "limit=" + strconv.Itoa(limit)
		if next != "" {
			q += "&" + key + "=" + url.QueryEscape(next)
		}
		p := h.page(t, token, target+q)
		out = append(out, p.Messages...)
		if p.Next == nil || key == "since" && !p.More {
			return out
		}
		next = *p.Next
	}
	t.Fatalf("GET %s did not end", target)
	return nil
}

func texts(ms []httpMessage) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Text == nil {
			out = append(out, "")
		} else {
			out = append(out, *m.Text)
		}
	}
	return out
}

func expected(messages []scoped.DumpMessage, keep func(scoped.DumpMessage) bool) []string {
	var rows []scoped.DumpMessage
	for _, m := range messages {
		if keep(m) {
			rows = append(rows, m)
		}
	}
	slices.SortFunc(rows, func(a, b scoped.DumpMessage) int { return cmp.Or(cmp.Compare(b.TS, a.TS), cmp.Compare(b.Seq, a.Seq)) })
	out := make([]string, 0, len(rows))
	for _, m := range rows {
		if m.Revoked {
			out = append(out, "")
		} else {
			out = append(out, m.Text)
		}
	}
	return out
}

func clientOf(t *rapid.T, c propClient, set scopeSet) *policy.Client {
	read := map[policy.CanonicalChat]struct{}{}
	for jid := range set.chats {
		read[chat(t, jid)] = struct{}{}
	}
	return &policy.Client{ID: c.id, ReadAll: c.all, Revoked: c.revoked, Read: read, ExpiresAt: epoch.AddDate(10, 0, 0)}
}

func TestPropertyHTTPReadsMatchTheOracle(t *testing.T) {
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
		h := newHTTPHarness(t, a)
		a.seen["dead canary"] = len(a.dead) > 0
		refOf := map[string]string{}
		for _, c := range chats {
			refOf[c.JID] = c.Ref
		}
		for _, c := range a.clients {
			token := "synthetic-token-" + c.id
			set := scopeOfClient(t, a.store, c)
			h.clients.set(token, clientOf(t, c, set))
			if c.revoked {
				denied, sql := h.get(t, token, "/v1/chats")
				unknown, _ := h.get(t, token, "/v1/no-such-route")
				if denied.Code != http.StatusNotFound || !bytes.Equal(denied.Body.Bytes(), unknown.Body.Bytes()) || !maps.EqualFunc(denied.Header(), unknown.Header(), slices.Equal) || len(sql) != 0 {
					t.Fatalf("a revoked client's listing = %d %s after %q", denied.Code, denied.Body.String(), sql)
				}
				continue
			}
			limit := rapid.IntRange(1, 5).Draw(t, "limit")
			checkHTTPChats(t, h, token, set, chats, limit)
			cursors := map[string]string{}
			for _, dc := range chats {
				target := "/v1/chats/" + dc.Ref + "/messages"
				if !set.has(dc.JID) {
					requireDeniedEqualsMissing(t, h, token, "/v1/chats/"+dc.Ref, "/v1/chats/"+missingRef)
					requireDeniedEqualsMissing(t, h, token, target, "/v1/chats/"+missingRef+"/messages")
					requireDeniedEqualsMissing(t, h, token, "/v1/changes?chat="+dc.Ref, "/v1/changes?chat="+missingRef)
					requireDeniedEqualsMissing(t, h, token, "/v1/changes?chat="+dc.Ref+"&since=x", "/v1/changes?chat="+missingRef+"&since=x")
					requireDeniedEqualsMissing(t, h, token, target+"?cursor=x", "/v1/chats/"+missingRef+"/messages?cursor=x")
					requireDeniedEqualsMissing(t, h, token, "/v1/search?q=text&chat="+dc.Ref, "/v1/search?q=text&chat="+missingRef)
					requireDeniedEqualsMissing(t, h, token, "/v1/search?q=text&chat="+dc.Ref+"&cursor=x", "/v1/search?q=text&chat="+missingRef+"&cursor=x")
					a.seen["http denied chat"] = true
					continue
				}
				got := h.walk(t, token, target, "cursor", limit)
				want := expected(messages, func(m scoped.DumpMessage) bool { return m.Chat == dc.JID })
				if !slices.Equal(texts(got), want) {
					t.Fatalf("%s read %s as %q, want %q", c.id, target, texts(got), want)
				}
				for _, m := range got {
					if m.Chat != dc.Ref {
						t.Fatalf("%s read a message of %s under %s", c.id, m.Chat, dc.Ref)
					}
					if rec, _ := h.get(t, token, "/v1/messages/"+m.Ref); rec.Code != http.StatusOK {
						t.Fatalf("%s could not open its own reference: %d", c.id, rec.Code)
					}
				}
				if first := h.page(t, token, target+"?limit=1"); first.Next != nil {
					cursors[dc.Ref] = *first.Next
					a.seen["http message cursor"] = true
				}
			}
			changes := h.walk(t, token, "/v1/changes", "since", limit)
			want := expected(messages, func(m scoped.DumpMessage) bool { return set.has(m.Chat) })
			if got := texts(changes); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
				t.Fatalf("%s changes = %q, want %q", c.id, got, want)
			}
			checkHTTPSearch(t, h, a, token, set, messages, refOf)
			checkHTTPReplay(t, h, a, token, c, set, cursors, refOf)
		}
		for _, body := range h.bodies {
			unsealed := sealedTokens.ReplaceAll(body, nil)
			for _, d := range a.dead {
				if bytes.Contains(unsealed, []byte(d)) {
					t.Fatalf("a response carries the revoked or edited text %s: %s", d, body)
				}
			}
		}
		cov.add(a.seen)
	})
	cov.require(t, "http denied chat", "http message cursor", "http replay narrowed", "http search hit", "dead canary")
}

func checkHTTPChats(t *rapid.T, h *httpHarness, token string, set scopeSet, chats []scoped.DumpChat, limit int) {
	var got []string
	next := ""
	for range 1000 {
		target := "/v1/chats?limit=" + strconv.Itoa(limit)
		if next != "" {
			target += "&cursor=" + url.QueryEscape(next)
		}
		p := h.page(t, token, target)
		for _, c := range p.Chats {
			got = append(got, c.ID)
		}
		if p.Next == nil {
			break
		}
		next = *p.Next
	}
	ordered := slices.Clone(chats)
	slices.SortFunc(ordered, chatOrder)
	var want []string
	for _, c := range ordered {
		if set.has(c.JID) {
			want = append(want, c.Ref)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chats = %q, want %q", got, want)
	}
}

func checkHTTPSearch(t *rapid.T, h *httpHarness, a *archive, token string, set scopeSet, messages []scoped.DumpMessage, refOf map[string]string) {
	for _, c := range slices.Sorted(maps.Keys(a.live)) {
		var holder *scoped.DumpMessage
		for i := range messages {
			if !messages[i].Revoked && strings.Contains(messages[i].Text, c) {
				holder = &messages[i]
			}
		}
		got := h.walk(t, token, "/v1/search?q="+c, "cursor", scoped.MaxLimit)
		inScope := holder != nil && set.has(holder.Chat)
		switch {
		case inScope && (len(got) != 1 || got[0].Chat != refOf[holder.Chat] || got[0].Text == nil || !strings.Contains(*got[0].Text, c)):
			t.Fatalf("search %s = %+v, want the one message of %s", c, got, holder.Chat)
		case !inScope && len(got) != 0:
			t.Fatalf("search %s = %+v, want nothing", c, got)
		case inScope:
			a.seen["http search hit"] = true
		}
	}
}

func checkHTTPReplay(t *rapid.T, h *httpHarness, a *archive, token string, c propClient, set scopeSet, cursors map[string]string, refOf map[string]string) {
	narrowed := scopeSet{chats: map[string]bool{}}
	for _, jid := range slices.Sorted(maps.Keys(refOf)) {
		if set.has(jid) && rapid.Bool().Draw(t, "kept after the change") {
			narrowed.chats[jid] = true
		}
	}
	changes := h.page(t, token, "/v1/changes?limit=1")
	search := h.page(t, token, "/v1/search?q=text&limit=1")
	h.clients.set(token, clientOf(t, propClient{id: c.id}, narrowed))
	allowed := map[string]bool{}
	for jid := range narrowed.chats {
		allowed[refOf[jid]] = true
	}
	for ref, next := range cursors {
		target := "/v1/chats/" + ref + "/messages?limit=1&cursor=" + url.QueryEscape(next)
		rec, _ := h.get(t, token, target)
		if allowed[ref] {
			if rec.Code != http.StatusOK {
				t.Fatalf("replay of a cursor for a kept chat = %d %s", rec.Code, rec.Body.String())
			}
			continue
		}
		missing, _ := h.get(t, token, "/v1/chats/"+missingRef+"/messages")
		if rec.Code != http.StatusNotFound || !bytes.Equal(rec.Body.Bytes(), missing.Body.Bytes()) {
			t.Fatalf("replay of a cursor for a removed chat = %d %s", rec.Code, rec.Body.String())
		}
		a.seen["http replay narrowed"] = true
	}
	for _, p := range []struct {
		target string
		next   *string
	}{{"/v1/changes?limit=200&since=", changes.Next}, {"/v1/search?q=text&limit=200&cursor=", search.Next}} {
		if p.next == nil {
			continue
		}
		for _, m := range h.page(t, token, p.target+url.QueryEscape(*p.next)).Messages {
			if !allowed[m.Chat] {
				t.Fatalf("a replayed cursor returned a message of %s after the grant narrowed", m.Chat)
			}
		}
	}
	h.clients.set(token, clientOf(t, c, set))
}

func requireDeniedEqualsMissing(t *rapid.T, h *httpHarness, token, denied, missing string) {
	d, dSQL := h.get(t, token, denied)
	m, mSQL := h.get(t, token, missing)
	if d.Code != http.StatusNotFound || d.Code != m.Code || !bytes.Equal(d.Body.Bytes(), m.Body.Bytes()) || !maps.EqualFunc(d.Header(), m.Header(), slices.Equal) {
		t.Fatalf("GET %s = %d %s %v, GET %s = %d %s %v", denied, d.Code, d.Body.String(), d.Header(), missing, m.Code, m.Body.String(), m.Header())
	}
	if !slices.Equal(dSQL, mSQL) || len(dSQL) == 0 {
		t.Fatalf("GET %s issued %q, GET %s issued %q", denied, dSQL, missing, mSQL)
	}
}
