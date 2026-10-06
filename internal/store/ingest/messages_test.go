package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func TestOpenCreatesTheSchemaOnce(t *testing.T) {
	opts := testOptions(t)
	s := openWith(t, opts)
	if s.SchemaVersion() != len(migrations) || len(migrations) != 2 {
		t.Fatalf("schema version %d, want %d", s.SchemaVersion(), len(migrations))
	}
	for _, table := range []string{"chats", "chat_aliases", "lid_map", "contacts", "group_participants", "messages", "messages_fts", "history_blobs", "inbox", "sync_state"} {
		if n := scalar[int](t, s, "SELECT count(*) FROM sqlite_schema WHERE name = ?", table); n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	if v := scalar[string](t, s, "SELECT v FROM messages_fts_config WHERE k = 'secure-delete'"); v != "1" {
		t.Fatalf("the full-text index's secure-delete option is %q, want 1", v)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM pragma_table_info('messages') WHERE name = 'raw'"); n != 0 {
		t.Fatal("messages has a raw column")
	}
	insert(t, s, textMessage(t, alice, "M1", alice, "kept across reopening"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again := openWith(t, opts)
	if again.SchemaVersion() != 2 || scalar[int](t, again, "SELECT count(*) FROM messages") != 1 {
		t.Fatal("reopening changed the schema or the data")
	}
	if !again.Healthy() || again.Profile() != ProfileLocal {
		t.Fatal("a reopened store is unhealthy or forgot its profile")
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	opts := testOptions(t)
	s := openWith(t, opts)
	write(t, s, func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, "PRAGMA user_version = 3")
		return err
	})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := Open(t.Context(), opts)
	if r, ok := errors.AsType[*Refusal](err); !ok || r.Reason != "archive_schema_newer" {
		t.Fatalf("Open = %v, want an archive_schema_newer refusal", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := Open(ctx, opts); !errors.As(err, new(*Refusal)) {
		t.Fatalf("Open after a refusal = %v, want the same refusal: the refused Open kept the lock", err)
	}
}

func TestInsertAndResolveInTheChat(t *testing.T) {
	s := openStore(t)
	m := textMessage(t, alice, "3EB0C767D82B6A1A5A26", alice, "hello \u202ethere\u200b")
	m.Timestamp = epoch.Add(time.Minute)
	ref := insert(t, s, m)
	var again Ref
	var inserted bool
	write(t, s, func(tx *Tx) error {
		var err error
		again, inserted, err = tx.InsertMessage(m)
		return err
	})
	if inserted || again != ref {
		t.Fatalf("a duplicate insert = %v, %v, want the existing row and false", again, inserted)
	}
	f, ok := resolve(t, s, alice, m.ID, alice)
	if !ok || f.Ref != ref || f.Text != m.Text || f.Kind != KindText || f.FromMe || f.Revoked || f.Sender != chat(t, alice) || !f.EditedAt.IsZero() {
		t.Fatalf("ResolveInChat = %+v, %v", f, ok)
	}
	for _, miss := range []struct{ chat, id, sender string }{
		{bob, m.ID, alice},
		{alice, m.ID, bob},
		{alice, "3EB0C767D82B6A1A5A27", alice},
		{groupJID, m.ID, alice},
	} {
		if _, ok := resolve(t, s, miss.chat, miss.id, miss.sender); ok {
			t.Fatalf("ResolveInChat found %+v outside its chat, id and sender", miss)
		}
	}
	if d := scalar[string](t, s, "SELECT text_display FROM messages"); d != "hello there" {
		t.Fatalf("text_display = %q, want the display sanitiser's output", d)
	}
	if k, last := scalar[int](t, s, "SELECT kind FROM chats WHERE jid = ?", alice), scalar[int64](t, s, "SELECT last_ts FROM chats WHERE jid = ?", alice); k != 1 || last != ms(m.Timestamp) {
		t.Fatalf("chat kind %d and last_ts %d, want 1 and the message time", k, last)
	}
	if matches(t, s, "hello") != 1 {
		t.Fatal("the full-text index does not find the message")
	}
	ftsIntegrity(t, s)
}

func TestInsertRefusesInvalidMessages(t *testing.T) {
	s := openStore(t)
	valid := func() Message { return textMessage(t, groupJID, "ID1", alice, "text") }
	for name, change := range map[string]func(*Message){
		"zero chat":                func(m *Message) { m.Chat = policy.CanonicalChat{} },
		"empty id":                 func(m *Message) { m.ID = "" },
		"id with a space":          func(m *Message) { m.ID = "a b" },
		"id with a control":        func(m *Message) { m.ID = "a\nb" },
		"overlong id":              func(m *Message) { m.ID = string(bytes.Repeat([]byte("A"), 129)) },
		"zero sender":              func(m *Message) { m.Sender = policy.CanonicalChat{} },
		"group sender":             func(m *Message) { m.Sender = chat(t, group2JID) },
		"group alternate sender":   func(m *Message) { m.SenderAlt = chat(t, group2JID) },
		"unknown origin":           func(m *Message) { m.Origin = "replay" },
		"unknown addressing":       func(m *Message) { m.Addressing = "hosted" },
		"zero time":                func(m *Message) { m.Timestamp = time.Time{} },
		"zero ingest time":         func(m *Message) { m.Ingested = time.Time{} },
		"unknown kind":             func(m *Message) { m.Kind = "status" },
		"reaction with content":    func(m *Message) { m.Kind = KindReaction },
		"poll update with content": func(m *Message) { m.Kind = KindPollUpdate },
		"reaction with a media type": func(m *Message) {
			m.Kind, m.Text, m.MediaType = KindReaction, "", "image/jpeg"
		},
		"media type with a space": func(m *Message) { m.MediaType = "image/ jpeg" },
		"quote without a ref":     func(m *Message) { m.Quote = &Quote{} },
	} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			change(&m)
			err := s.Write(t.Context(), "test.insert", func(tx *Tx) error {
				_, _, err := tx.InsertMessage(m)
				return err
			})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("InsertMessage = %v, want ErrInvalid", err)
			}
		})
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages"); n != 0 {
		t.Fatalf("%d rows after refused inserts", n)
	}
	reaction := valid()
	reaction.Kind, reaction.Text = KindReaction, ""
	insert(t, s, reaction)
	if text := scalar[*string](t, s, "SELECT text FROM messages WHERE kind = 'reaction'"); text != nil {
		t.Fatal("a reaction stored content")
	}
}

func TestQuotesResolveInTheSameChat(t *testing.T) {
	s := openStore(t)
	original := insert(t, s, textMessage(t, groupJID, "ORIG", alice, "the original"))
	elsewhere := insert(t, s, textMessage(t, group2JID, "ORIG", alice, "the original"))
	reply := textMessage(t, groupJID, "REPLY", bob, "a reply")
	reply.Quote = &Quote{Ref: original, Verified: false}
	insert(t, s, reply)
	if ref, verified := scalar[int64](t, s, "SELECT quoted_ref FROM messages WHERE id = 'REPLY'"), scalar[int](t, s, "SELECT quote_verified FROM messages WHERE id = 'REPLY'"); ref != original.seq || verified != 0 {
		t.Fatalf("quoted_ref %d, quote_verified %d, want %d and 0", ref, verified, original.seq)
	}
	forged := textMessage(t, groupJID, "FORGED", bob, "a reply across chats")
	forged.Quote = &Quote{Ref: elsewhere, Verified: true}
	err := s.Write(t.Context(), "test.insert", func(tx *Tx) error {
		_, _, err := tx.InsertMessage(forged)
		return err
	})
	if !errors.Is(err, ErrWrongChat) {
		t.Fatalf("a quote of another chat's message = %v, want ErrWrongChat", err)
	}
	disguised := textMessage(t, groupJID, "DISGUISED", bob, "a reply across chats")
	disguised.Quote = &Quote{Ref: Ref{seq: elsewhere.seq, chat: chat(t, groupJID)}}
	err = s.Write(t.Context(), "test.insert", func(tx *Tx) error {
		_, _, err := tx.InsertMessage(disguised)
		return err
	})
	if !errors.Is(err, ErrWrongChat) {
		t.Fatalf("a reference relabelled with this chat = %v, want ErrWrongChat", err)
	}
}

func TestEditsAndRevokesApplyOnlyInTheirChat(t *testing.T) {
	s := openStore(t)
	ref := insert(t, s, textMessage(t, groupJID, "M1", alice, "first wording"))
	other := insert(t, s, textMessage(t, group2JID, "M1", alice, "untouched"))
	write(t, s, func(tx *Tx) error { return tx.ApplyEdit(ref, "second wording", epoch.Add(time.Minute)) })
	f, _ := resolve(t, s, groupJID, "M1", alice)
	if f.Text != "second wording" || !f.EditedAt.Equal(epoch.Add(time.Minute)) || matches(t, s, "first wording") != 0 || matches(t, s, "second wording") != 1 {
		t.Fatalf("after the edit: %+v", f)
	}
	if edited := scalar[int64](t, s, "SELECT edited_ts FROM messages WHERE seq = ?", ref.seq); edited != ms(epoch.Add(time.Minute)) {
		t.Fatalf("edited_ts = %d", edited)
	}
	relabelled := Ref{seq: other.seq, chat: chat(t, groupJID)}
	for name, apply := range map[string]func(*Tx) error{
		"edit":   func(tx *Tx) error { return tx.ApplyEdit(relabelled, "hijacked", epoch) },
		"revoke": func(tx *Tx) error { return tx.ApplyRevoke(relabelled) },
	} {
		if err := s.Write(t.Context(), "test."+name, apply); !errors.Is(err, ErrWrongChat) {
			t.Fatalf("%s through a relabelled reference = %v, want ErrWrongChat", name, err)
		}
	}
	if f, _ := resolve(t, s, group2JID, "M1", alice); f.Text != "untouched" || f.Revoked {
		t.Fatal("a relabelled reference changed another chat's message")
	}
	write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	f, _ = resolve(t, s, groupJID, "M1", alice)
	if !f.Revoked || f.Text != "" || matches(t, s, "second wording") != 0 {
		t.Fatalf("after the revoke: %+v", f)
	}
	if err := s.Write(t.Context(), "test.edit", func(tx *Tx) error { return tx.ApplyEdit(ref, "too late", epoch) }); !errors.Is(err, ErrRevoked) {
		t.Fatalf("an edit of a revoked message = %v, want ErrRevoked", err)
	}
	if err := s.Write(t.Context(), "test.edit", func(tx *Tx) error { return tx.ApplyEdit(Ref{}, "x", epoch) }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an edit through the zero reference = %v, want ErrInvalid", err)
	}
	if err := s.Write(t.Context(), "test.edit", func(tx *Tx) error {
		return tx.ApplyEdit(Ref{seq: 999, chat: chat(t, groupJID)}, "x", epoch)
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an edit of a missing row = %v, want ErrNotFound", err)
	}
	ftsIntegrity(t, s)
}

func TestAnEditStoresItsDisplayText(t *testing.T) {
	s := openStore(t)
	ref := insert(t, s, textMessage(t, alice, "M1", alice, "first wording"))
	write(t, s, func(tx *Tx) error { return tx.ApplyEdit(ref, "second \u202ewording\u200b", epoch.Add(time.Minute)) })
	if d := scalar[string](t, s, "SELECT text_display FROM messages WHERE seq = ?", ref.seq); d != "second wording" {
		t.Fatalf("text_display after an edit = %q, want the display sanitiser's output of the new text", d)
	}
}

func TestReactionsCannotBeEdited(t *testing.T) {
	s := openStore(t)
	m := textMessage(t, groupJID, "R1", alice, "")
	m.Kind = KindReaction
	ref := insert(t, s, m)
	if err := s.Write(t.Context(), "test.edit", func(tx *Tx) error { return tx.ApplyEdit(ref, "injected", epoch) }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an edit of a reaction = %v, want ErrInvalid", err)
	}
	write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	ftsIntegrity(t, s)
}

func TestPurgeExpired(t *testing.T) {
	s := openStore(t)
	for i, offset := range []time.Duration{-2 * time.Hour, -time.Hour, -time.Minute, time.Hour} {
		m := textMessage(t, alice, "E"+string(rune('0'+i)), alice, "disappearing text")
		m.ExpiresAt = epoch.Add(offset)
		insert(t, s, m)
	}
	insert(t, s, textMessage(t, alice, "KEEP", alice, "permanent text"))
	var purged int
	write(t, s, func(tx *Tx) error {
		var err error
		purged, err = tx.PurgeExpired(epoch, 2)
		return err
	})
	if purged != 2 {
		t.Fatalf("PurgeExpired with a limit of 2 = %d", purged)
	}
	write(t, s, func(tx *Tx) error {
		var err error
		purged, err = tx.PurgeExpired(epoch, 100)
		return err
	})
	if purged != 1 {
		t.Fatalf("the second PurgeExpired = %d, want the last expired row", purged)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages WHERE text IS NOT NULL"); n != 2 {
		t.Fatalf("%d messages keep their text, want the unexpired and the permanent one", n)
	}
	if matches(t, s, "disappearing") != 1 || matches(t, s, "permanent") != 1 {
		t.Fatal("the full-text index does not match the purge")
	}
	if err := s.Write(t.Context(), "test.purge", func(tx *Tx) error { _, err := tx.PurgeExpired(epoch, 0); return err }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("PurgeExpired without a limit = %v", err)
	}
	ftsIntegrity(t, s)
}

func TestBackupThroughTheStore(t *testing.T) {
	s := openStore(t)
	insert(t, s, textMessage(t, alice, "M1", alice, "backed up"))
	var out bytes.Buffer
	var name string
	err := s.Backup(t.Context(), filepath.Join(t.TempDir(), "staging"), func(n string, _ int64, r io.Reader) error {
		name = n
		_, err := io.Copy(&out, r)
		return err
	})
	if err != nil || name != "archive.db" || !bytes.HasPrefix(out.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatalf("Backup = %v, %q of %d bytes", err, name, out.Len())
	}
}
