package policy

import (
	"maps"
	"reflect"
	"testing"
	"time"
)

var (
	now        = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	future     = now.Add(90 * 24 * time.Hour)
	chatA      = testChat("chat-a")
	chatB      = testChat("chat-b")
	chatC      = testChat("chat-c")
	knownChats = []CanonicalChat{chatA, chatB, chatC}
)

func testChat(jid string) CanonicalChat { return CanonicalChat{jid: jid, ok: true} }

func set(chats ...CanonicalChat) map[CanonicalChat]struct{} {
	s := make(map[CanonicalChat]struct{}, len(chats))
	for _, c := range chats {
		s[c] = struct{}{}
	}
	return s
}

type shape struct {
	name   string
	client func() *Client
}

func clientShapes() []shape {
	return []shape{
		{name: "nil", client: func() *Client { return nil }},
		{name: "empty", client: func() *Client { return &Client{ID: "c-empty", ExpiresAt: future} }},
		{name: "read-all", client: func() *Client { return &Client{ID: "c-all", ReadAll: true, ExpiresAt: future} }},
		{name: "read-set", client: func() *Client { return &Client{ID: "c-read", Read: set(chatA, chatB), ExpiresAt: future} }},
		{name: "write-set", client: func() *Client { return &Client{ID: "c-write", Write: set(chatA), ExpiresAt: future} }},
		{name: "read+write", client: func() *Client {
			return &Client{ID: "c-rw", Read: set(chatA, chatB), Write: set(chatA), AllowFirstContact: true, ExpiresAt: future}
		}},
		{name: "read-all+write", client: func() *Client {
			return &Client{ID: "c-all-w", ReadAll: true, Write: set(chatA), ExpiresAt: future}
		}},
		{name: "sets holding the zero chat", client: func() *Client {
			return &Client{ID: "c-zero", Read: set(CanonicalChat{}, chatA), Write: set(CanonicalChat{}, chatA), ExpiresAt: future}
		}},
		{name: "write set holding only the zero chat", client: func() *Client {
			return &Client{ID: "c-zero-w", Write: set(CanonicalChat{}), ExpiresAt: future}
		}},
		{name: "never expiring", client: func() *Client { return &Client{ID: "c-zero-exp", ReadAll: true, Read: set(chatA), Write: set(chatA)} }},
		{name: "expired", client: func() *Client {
			return &Client{ID: "c-expired", Read: set(chatA), Write: set(chatA), ExpiresAt: now.Add(-time.Second)}
		}},
		{name: "revoked", client: func() *Client {
			return &Client{ID: "c-revoked", Read: set(chatA), Write: set(chatA), ExpiresAt: future, Revoked: true}
		}},
	}
}

func TestZeroChatIsNeverAllowed(t *testing.T) {
	for _, s := range clientShapes() {
		t.Run(s.name, func(t *testing.T) {
			r, _ := DecideRead(s.client(), now)
			if r.Allows(CanonicalChat{}) {
				t.Fatal("the read grant allows the zero-value chat")
			}
			if _, ok := r.Chats()[CanonicalChat{}]; ok {
				t.Fatal("the read grant's chat set holds the zero-value chat")
			}
			w, _ := DecideWrite(s.client(), now)
			if w.Allows(CanonicalChat{}) {
				t.Fatal("the write grant allows the zero-value chat")
			}
		})
	}
}

func TestDecideRead(t *testing.T) {
	tests := []struct {
		name    string
		client  *Client
		now     time.Time
		wantOK  bool
		wantAll bool
		allowed []CanonicalChat
	}{
		{name: "nil client", client: nil, now: now},
		{name: "read-all", client: &Client{ID: "c1", ReadAll: true, ExpiresAt: future}, now: now, wantOK: true, wantAll: true, allowed: knownChats},
		{name: "read-set", client: &Client{ID: "c2", Read: set(chatA, chatB), ExpiresAt: future}, now: now, wantOK: true, allowed: []CanonicalChat{chatA, chatB}},
		{name: "empty read set allows nothing", client: &Client{ID: "c3", ExpiresAt: future}, now: now, wantOK: true},
		{name: "write chats grant no reads", client: &Client{ID: "c4", Write: set(chatC), ExpiresAt: future}, now: now, wantOK: true},
		{name: "zero chat in the set is dropped", client: &Client{ID: "c5", Read: set(CanonicalChat{}, chatB), ExpiresAt: future}, now: now, wantOK: true, allowed: []CanonicalChat{chatB}},
		{name: "revoked", client: &Client{ID: "c6", ReadAll: true, ExpiresAt: future, Revoked: true}, now: now},
		{name: "never expiring", client: &Client{ID: "c7", ReadAll: true}, now: now},
		{name: "never expiring, decided before year one", client: &Client{ID: "c7", ReadAll: true}, now: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "expired", client: &Client{ID: "c8", ReadAll: true, ExpiresAt: now.Add(-time.Nanosecond)}, now: now},
		{name: "zero decision time", client: &Client{ID: "c9", ReadAll: true, ExpiresAt: future}, now: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, ok := DecideRead(tt.client, tt.now)
			if ok != tt.wantOK || g.Valid() != tt.wantOK {
				t.Fatalf("DecideRead() ok = %v, Valid() = %v, want %v", ok, g.Valid(), tt.wantOK)
			}
			if !ok {
				if !reflect.ValueOf(g).IsZero() {
					t.Fatalf("a denied read decision returned a non-zero grant %+v", g)
				}
				return
			}
			if g.Client() != tt.client.ID {
				t.Fatalf("Client() = %q, want %q", g.Client(), tt.client.ID)
			}
			if g.All() != tt.wantAll {
				t.Fatalf("All() = %v, want %v", g.All(), tt.wantAll)
			}
			want := set(tt.allowed...)
			for _, chat := range knownChats {
				_, w := want[chat]
				if got := g.Allows(chat); got != w {
					t.Fatalf("Allows(%s) = %v, want %v", chat.jid, got, w)
				}
			}
			if !tt.wantAll && !maps.Equal(g.Chats(), want) {
				t.Fatalf("Chats() = %v, want %v", g.Chats(), want)
			}
			if tt.wantAll && len(g.Chats()) != 0 {
				t.Fatalf("Chats() = %v for a read-all grant, want an empty set", g.Chats())
			}
		})
	}
}

func TestDecideWrite(t *testing.T) {
	tests := []struct {
		name             string
		client           *Client
		now              time.Time
		wantOK           bool
		wantFirstContact bool
		allowed          []CanonicalChat
	}{
		{name: "nil client", client: nil, now: now},
		{name: "write-set", client: &Client{ID: "c1", Write: set(chatA, chatB), ExpiresAt: future}, now: now, wantOK: true, allowed: []CanonicalChat{chatA, chatB}},
		{name: "first contact carried", client: &Client{ID: "c2", Read: set(chatA), Write: set(chatA), AllowFirstContact: true, ExpiresAt: future}, now: now, wantOK: true, wantFirstContact: true, allowed: []CanonicalChat{chatA}},
		{name: "read chats grant no writes", client: &Client{ID: "c3", Read: set(chatA, chatB), Write: set(chatC), ExpiresAt: future}, now: now, wantOK: true, allowed: []CanonicalChat{chatC}},
		{name: "empty write set", client: &Client{ID: "c4", Read: set(chatA), ExpiresAt: future}, now: now},
		{name: "write set holding only the zero chat", client: &Client{ID: "c5", Write: set(CanonicalChat{}), ExpiresAt: future}, now: now},
		{name: "zero chat in the set is dropped", client: &Client{ID: "c6", Write: set(CanonicalChat{}, chatB), ExpiresAt: future}, now: now, wantOK: true, allowed: []CanonicalChat{chatB}},
		{name: "read-all with a write chat", client: &Client{ID: "c7", ReadAll: true, Write: set(chatA), ExpiresAt: future}, now: now},
		{name: "read-all without write chats", client: &Client{ID: "c8", ReadAll: true, ExpiresAt: future}, now: now},
		{name: "revoked", client: &Client{ID: "c9", Write: set(chatA), ExpiresAt: future, Revoked: true}, now: now},
		{name: "never expiring", client: &Client{ID: "c10", Write: set(chatA)}, now: now},
		{name: "never expiring, decided before year one", client: &Client{ID: "c10", Write: set(chatA)}, now: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "expired", client: &Client{ID: "c11", Write: set(chatA), ExpiresAt: now.Add(-time.Nanosecond)}, now: now},
		{name: "zero decision time", client: &Client{ID: "c12", Write: set(chatA), ExpiresAt: future}, now: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, ok := DecideWrite(tt.client, tt.now)
			if ok != tt.wantOK || g.Valid() != tt.wantOK {
				t.Fatalf("DecideWrite() ok = %v, Valid() = %v, want %v", ok, g.Valid(), tt.wantOK)
			}
			if !ok {
				if !reflect.ValueOf(g).IsZero() {
					t.Fatalf("a denied write decision returned a non-zero grant %+v", g)
				}
				return
			}
			if g.Client() != tt.client.ID {
				t.Fatalf("Client() = %q, want %q", g.Client(), tt.client.ID)
			}
			if g.AllowFirstContact() != tt.wantFirstContact {
				t.Fatalf("AllowFirstContact() = %v, want %v", g.AllowFirstContact(), tt.wantFirstContact)
			}
			want := set(tt.allowed...)
			for _, chat := range knownChats {
				_, w := want[chat]
				if got := g.Allows(chat); got != w {
					t.Fatalf("Allows(%s) = %v, want %v", chat.jid, got, w)
				}
			}
		})
	}
}

func TestExpiryIsExclusiveAtTheBoundary(t *testing.T) {
	expires := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		now    time.Time
		wantOK bool
	}{
		{name: "one nanosecond before", now: expires.Add(-time.Nanosecond), wantOK: true},
		{name: "the boundary instant", now: expires},
		{name: "the boundary instant in another zone", now: expires.In(time.FixedZone("UTC+3", 3*60*60))},
		{name: "one nanosecond after", now: expires.Add(time.Nanosecond)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{ID: "c1", Read: set(chatA), Write: set(chatA), ExpiresAt: expires}
			if _, ok := DecideRead(c, tt.now); ok != tt.wantOK {
				t.Fatalf("DecideRead() ok = %v, want %v", ok, tt.wantOK)
			}
			if _, ok := DecideWrite(c, tt.now); ok != tt.wantOK {
				t.Fatalf("DecideWrite() ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestRevokedClientIsDenied(t *testing.T) {
	c := &Client{ID: "c1", Read: set(chatA), Write: set(chatA), AllowFirstContact: true, ExpiresAt: future}
	if _, ok := DecideRead(c, now); !ok {
		t.Fatal("DecideRead() denied the client before revocation")
	}
	if _, ok := DecideWrite(c, now); !ok {
		t.Fatal("DecideWrite() denied the client before revocation")
	}
	c.Revoked = true
	if g, ok := DecideRead(c, now); ok || g.Allows(chatA) {
		t.Fatal("DecideRead() granted a revoked client")
	}
	if g, ok := DecideWrite(c, now); ok || g.Allows(chatA) {
		t.Fatal("DecideWrite() granted a revoked client")
	}
}

func TestReadAllWithAWriteChatDeniesWrites(t *testing.T) {
	c := &Client{ID: "c1", ReadAll: true, Write: set(chatA), AllowFirstContact: true, ExpiresAt: future}
	if g, ok := DecideWrite(c, now); ok || g.Valid() || g.Allows(chatA) {
		t.Fatalf("DecideWrite() = %+v, %v for a read-all client with a write chat, want a denial", g, ok)
	}
}

func TestGrantsAreSnapshots(t *testing.T) {
	c := &Client{ID: "c1", Read: set(chatA), Write: set(chatB), AllowFirstContact: true, ExpiresAt: future}
	r, ok := DecideRead(c, now)
	if !ok {
		t.Fatal("DecideRead() denied a live client")
	}
	w, ok := DecideWrite(c, now)
	if !ok {
		t.Fatal("DecideWrite() denied a live client")
	}

	delete(c.Read, chatA)
	c.Read[chatC] = struct{}{}
	delete(c.Write, chatB)
	c.Write[chatC] = struct{}{}
	c.ID = "c2"
	c.ReadAll = true
	c.AllowFirstContact = false
	c.Revoked = true
	c.ExpiresAt = now.Add(-time.Hour)

	exported := r.Chats()
	delete(exported, chatA)
	exported[chatC] = struct{}{}

	if !r.Valid() || r.Client() != "c1" || r.All() {
		t.Fatalf("read grant changed after the client was mutated: valid %v, client %q, all %v", r.Valid(), r.Client(), r.All())
	}
	if !r.Allows(chatA) || r.Allows(chatC) {
		t.Fatalf("read grant follows the client's or the exported set: Allows(a) = %v, Allows(c) = %v", r.Allows(chatA), r.Allows(chatC))
	}
	if !maps.Equal(r.Chats(), set(chatA)) {
		t.Fatalf("Chats() = %v, want only chat-a", r.Chats())
	}
	if !w.Valid() || w.Client() != "c1" || !w.AllowFirstContact() {
		t.Fatalf("write grant changed after the client was mutated: valid %v, client %q, first contact %v", w.Valid(), w.Client(), w.AllowFirstContact())
	}
	if !w.Allows(chatB) || w.Allows(chatC) {
		t.Fatalf("write grant follows the client's set: Allows(b) = %v, Allows(c) = %v", w.Allows(chatB), w.Allows(chatC))
	}

	c.Read, c.Write = nil, nil
	if !r.Allows(chatA) || !w.Allows(chatB) {
		t.Fatal("grants changed after the client's sets were replaced")
	}
}

func TestZeroValueGrantsAreInvalid(t *testing.T) {
	chats := append([]CanonicalChat{{}}, knownChats...)

	var r ReadGrant
	if r.Valid() || r.All() || r.Client() != "" || len(r.Chats()) != 0 {
		t.Fatalf("zero ReadGrant: valid %v, all %v, client %q, chats %v", r.Valid(), r.All(), r.Client(), r.Chats())
	}
	var w WriteGrant
	if w.Valid() || w.AllowFirstContact() || w.Client() != "" {
		t.Fatalf("zero WriteGrant: valid %v, first contact %v, client %q", w.Valid(), w.AllowFirstContact(), w.Client())
	}
	for _, chat := range chats {
		if r.Allows(chat) || w.Allows(chat) {
			t.Fatalf("a zero-value grant allows %q", chat.jid)
		}
	}
	if (AdminGrant{}).Valid() {
		t.Fatal("zero AdminGrant is valid")
	}
}

func TestGrantsWithoutTheOkFlagAllowNothing(t *testing.T) {
	chats := append([]CanonicalChat{{}}, knownChats...)

	reads := []struct {
		name string
		g    ReadGrant
	}{
		{name: "read-all", g: ReadGrant{client: "c1", all: true}},
		{name: "read-set", g: ReadGrant{client: "c1", chats: set(chatA, chatB)}},
		{name: "read-all with a set holding the zero chat", g: ReadGrant{client: "c1", all: true, chats: set(chatA, CanonicalChat{})}},
	}
	for _, tt := range reads {
		t.Run("ReadGrant "+tt.name, func(t *testing.T) {
			if tt.g.Valid() || tt.g.All() || len(tt.g.Chats()) != 0 {
				t.Fatalf("Valid() = %v, All() = %v, Chats() = %v, want false, false, empty", tt.g.Valid(), tt.g.All(), tt.g.Chats())
			}
			for _, chat := range chats {
				if tt.g.Allows(chat) {
					t.Fatalf("Allows(%q) = true", chat.jid)
				}
			}
		})
	}

	writes := []struct {
		name string
		g    WriteGrant
	}{
		{name: "write-set with first contact", g: WriteGrant{client: "c1", chats: set(chatA, chatB), allowFirstContact: true}},
		{name: "set holding the zero chat", g: WriteGrant{client: "c1", chats: set(chatA, CanonicalChat{}), allowFirstContact: true}},
	}
	for _, tt := range writes {
		t.Run("WriteGrant "+tt.name, func(t *testing.T) {
			if tt.g.Valid() || tt.g.AllowFirstContact() {
				t.Fatalf("Valid() = %v, AllowFirstContact() = %v, want false, false", tt.g.Valid(), tt.g.AllowFirstContact())
			}
			for _, chat := range chats {
				if tt.g.Allows(chat) {
					t.Fatalf("Allows(%q) = true", chat.jid)
				}
			}
		})
	}
}

func TestValidGrantsRefuseTheZeroChatEvenWhenTheirSetHoldsIt(t *testing.T) {
	c := &Client{ID: "c1", Read: set(chatA), Write: set(chatA), ExpiresAt: future}
	r, ok := DecideRead(c, now)
	if !ok {
		t.Fatal("DecideRead() denied a live client")
	}
	w, ok := DecideWrite(c, now)
	if !ok {
		t.Fatal("DecideWrite() denied a live client")
	}

	r.chats[CanonicalChat{}] = struct{}{}
	w.chats[CanonicalChat{}] = struct{}{}

	if r.Allows(CanonicalChat{}) || w.Allows(CanonicalChat{}) {
		t.Fatalf("a valid grant allows the zero-value chat: read %v, write %v", r.Allows(CanonicalChat{}), w.Allows(CanonicalChat{}))
	}
	if !r.Allows(chatA) || !w.Allows(chatA) {
		t.Fatal("the grants no longer allow their own chat")
	}
}

func TestDecisionTypesHaveNoExportedFields(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[CanonicalChat](),
		reflect.TypeFor[ReadGrant](),
		reflect.TypeFor[WriteGrant](),
		reflect.TypeFor[AdminGrant](),
		reflect.TypeFor[AdminCredential](),
	} {
		t.Run(typ.Name(), func(t *testing.T) {
			if typ.Kind() != reflect.Struct {
				t.Fatalf("%s is a %s, want a struct", typ, typ.Kind())
			}
			for f := range typ.Fields() {
				if f.IsExported() {
					t.Fatalf("%s has the exported field %s", typ, f.Name)
				}
			}
			ok, found := typ.FieldByName("ok")
			if !found || ok.Type.Kind() != reflect.Bool {
				t.Fatalf("%s lacks the unexported bool field ok", typ)
			}
		})
	}
}
