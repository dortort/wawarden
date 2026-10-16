package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func TestOpenCreatesTheSchemaOnce(t *testing.T) {
	opts := testOptions(t)
	s := openWith(t, opts)
	if s.SchemaVersion() != 5 || LatestSchemaVersion() != s.SchemaVersion() {
		t.Fatalf("schema version %d, latest %d, want 5", s.SchemaVersion(), LatestSchemaVersion())
	}
	for _, table := range []string{"chats", "chat_aliases", "lid_map", "contacts", "group_participants", "messages", "messages_fts", "history_blobs", "inbox", "sync_state",
		"contact_names", "clients", "client_read_chats", "client_write_chats", "audit", "media_refs", "revoke_tombstones"} {
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
	if again.SchemaVersion() != 5 || scalar[int](t, again, "SELECT count(*) FROM messages") != 1 {
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
		_, err := tx.q.ExecContext(tx.ctx, "PRAGMA user_version = 6")
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
	for _, kind := range []Kind{KindReaction, KindPollUpdate} {
		m := textMessage(t, groupJID, "R-"+string(kind), alice, "")
		m.Kind = kind
		ref := insert(t, s, m)
		if err := s.Write(t.Context(), "test.edit", func(tx *Tx) error { return tx.ApplyEdit(ref, "injected", epoch) }); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an edit of a %s = %v, want ErrInvalid", kind, err)
		}
		write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	}
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

const refRetention = 30 * 24 * time.Hour

func mediaMessage(t *testing.T, chatJID, id, senderJID, caption, sealed string) Message {
	t.Helper()
	m := textMessage(t, chatJID, id, senderJID, caption)
	m.Kind, m.MediaType = KindMedia, "image/jpeg"
	m.Media = &Media{Ref: sealed, Kind: MediaImage, Declared: 4096, Retention: refRetention}
	return m
}

func insertNew(t *testing.T, s *Store, m Message) (Ref, bool) {
	t.Helper()
	var ref Ref
	var inserted bool
	write(t, s, func(tx *Tx) error {
		var err error
		ref, inserted, err = tx.InsertMessage(m)
		return err
	})
	return ref, inserted
}

func storedRef(t *testing.T, s *Store, ref Ref) string {
	t.Helper()
	return scalar[string](t, s, "SELECT coalesce((SELECT ref || ' ' || kind || ' ' || coalesce(variant, '-') || ' ' || declared || ' ' || expires_at || ' ' || added_at FROM media_refs WHERE seq = ?), '')", ref.seq)
}

func refRow(sealed string, kind MediaKind, variant string, declared int64, expires, added time.Time) string {
	if variant == "" {
		variant = "-"
	}
	return fmt.Sprintf("%s %s %s %d %d %d", sealed, kind, variant, declared, ms(expires), ms(added))
}

func TestInsertRefusesInvalidMediaReferences(t *testing.T) {
	s := openStore(t)
	for name, change := range map[string]func(*Message){
		"a reference on a text message":      func(m *Message) { m.Kind = KindText },
		"a reference on an unknown kind":     func(m *Message) { m.Kind = KindOther },
		"an empty reference":                 func(m *Message) { m.Media.Ref = "" },
		"an overlong reference":              func(m *Message) { m.Media.Ref = strings.Repeat("A", maxMediaRef+1) },
		"a reference with a space":           func(m *Message) { m.Media.Ref = "sealed reference" },
		"a reference with a control":         func(m *Message) { m.Media.Ref = "sealed\nreference" },
		"a reference beyond ASCII":           func(m *Message) { m.Media.Ref = "sealedé" },
		"an unknown media kind":              func(m *Message) { m.Media.Kind = "contact" },
		"an empty media kind":                func(m *Message) { m.Media.Kind = "" },
		"an unknown variant":                 func(m *Message) { m.Media.Variant = "panorama" },
		"a gif that is not a video":          func(m *Message) { m.Media.Variant = "gif" },
		"a voice note that is not audio":     func(m *Message) { m.Media.Kind, m.Media.Variant = MediaVideo, "voice_note" },
		"a video note that is not a video":   func(m *Message) { m.Media.Kind, m.Media.Variant = MediaSticker, "video_note" },
		"an animation that is not sticker":   func(m *Message) { m.Media.Kind, m.Media.Variant = MediaDocument, "animated" },
		"a lottie that is not a sticker":     func(m *Message) { m.Media.Kind, m.Media.Variant = MediaAudio, "lottie" },
		"a negative declared size":           func(m *Message) { m.Media.Declared = -1 },
		"a declared size beyond 2 GiB":       func(m *Message) { m.Media.Declared = maxMediaDeclared + 1 },
		"no retention":                       func(m *Message) { m.Media.Retention = 0 },
		"a negative retention":               func(m *Message) { m.Media.Retention = -time.Hour },
		"a reaction carrying a reference":    func(m *Message) { m.Kind, m.Text, m.MediaType = KindReaction, "", "" },
		"a poll update carrying a reference": func(m *Message) { m.Kind, m.Text, m.MediaType = KindPollUpdate, "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			m := mediaMessage(t, groupJID, "ID1", alice, "caption", "sealed-reference")
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
	if n := scalar[int](t, s, "SELECT count(*) FROM messages") + scalar[int](t, s, "SELECT count(*) FROM media_refs"); n != 0 {
		t.Fatalf("%d rows after refused inserts", n)
	}
	for i, media := range []Media{
		{Ref: strings.Repeat("~", maxMediaRef), Kind: MediaVideo, Variant: "video_note", Declared: maxMediaDeclared, Retention: time.Millisecond * 2},
		{Ref: "!", Kind: MediaAudio, Variant: "voice_note", Retention: refRetention},
		{Ref: "a", Kind: MediaSticker, Variant: "lottie", Retention: refRetention},
		{Ref: "b", Kind: MediaSticker, Variant: "animated", Retention: refRetention},
		{Ref: "c", Kind: MediaVideo, Variant: "gif", Retention: refRetention},
		{Ref: "d", Kind: MediaDocument, Retention: refRetention},
	} {
		m := mediaMessage(t, groupJID, fmt.Sprintf("OK%d", i), alice, "", "unused")
		m.Media = &media
		m.Ingested = epoch.Add(-time.Millisecond)
		if _, inserted := insertNew(t, s, m); !inserted {
			t.Fatalf("the valid reference %d was not stored", i)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM media_refs"); n != 6 {
		t.Fatalf("%d references after six valid inserts, want 6", n)
	}
}

func TestAReferenceIsWrittenWithANewRowOnly(t *testing.T) {
	s := openStore(t)
	m := mediaMessage(t, groupJID, "M1", alice, "a caption", "sealed-first")
	m.Media.Kind, m.Media.Variant, m.Media.Declared = MediaVideo, "gif", maxMediaDeclared
	ref, inserted := insertNew(t, s, m)
	if want := refRow("sealed-first", MediaVideo, "gif", maxMediaDeclared, epoch.Add(refRetention), epoch); !inserted || storedRef(t, s, ref) != want {
		t.Fatalf("a new media message stored the reference %q, want %q", storedRef(t, s, ref), want)
	}
	again := mediaMessage(t, groupJID, "M1", alice, "a caption", "sealed-second")
	again.Ingested = epoch.Add(time.Minute)
	if dup, inserted := insertNew(t, s, again); inserted || dup != ref {
		t.Fatalf("a duplicate insert = %v, %v, want the stored row and false", dup, inserted)
	}
	if got := storedRef(t, s, ref); !strings.HasPrefix(got, "sealed-first ") {
		t.Fatalf("a duplicate replaced the stored reference with %q", got)
	}
	bare := mediaMessage(t, groupJID, "M2", alice, "", "unused")
	bare.Media = nil
	bareRef, _ := insertNew(t, s, bare)
	if got := storedRef(t, s, bareRef); got != "" {
		t.Fatalf("a message without a reference stored %q", got)
	}
	fill := mediaMessage(t, groupJID, "M2", alice, "", "sealed-fill")
	fill.Ingested = epoch.Add(time.Hour)
	if _, inserted := insertNew(t, s, fill); inserted {
		t.Fatal("a duplicate that fills a reference reported a new row")
	}
	if want := refRow("sealed-fill", MediaImage, "", 4096, epoch.Add(refRetention), epoch.Add(time.Hour)); storedRef(t, s, bareRef) != want {
		t.Fatalf("the duplicate filled %q, want %q", storedRef(t, s, bareRef), want)
	}
	text := insert(t, s, textMessage(t, groupJID, "T1", alice, "plain words"))
	if _, inserted := insertNew(t, s, mediaMessage(t, groupJID, "T1", alice, "plain words", "sealed-text")); inserted || storedRef(t, s, text) != "" {
		t.Fatal("a duplicate gave a reference to a stored message that is not media")
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM media_refs"); n != 2 {
		t.Fatalf("%d references, want the first and the filled one", n)
	}
}

func TestADuplicateNeverRestoresAReference(t *testing.T) {
	s := openStore(t)
	revoked := insert(t, s, mediaMessage(t, groupJID, "R1", alice, "", "sealed-revoked"))
	write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(revoked) })
	if _, inserted := insertNew(t, s, mediaMessage(t, groupJID, "R1", alice, "", "sealed-revoked")); inserted || storedRef(t, s, revoked) != "" {
		t.Fatal("a duplicate restored the reference of a revoked message")
	}
	expiring := mediaMessage(t, groupJID, "E1", alice, "", "unused")
	expiring.Media, expiring.ExpiresAt = nil, epoch.Add(time.Hour)
	expired := insert(t, s, expiring)
	late := mediaMessage(t, groupJID, "E1", alice, "", "sealed-expired")
	late.Ingested = epoch.Add(time.Hour)
	if _, inserted := insertNew(t, s, late); inserted || storedRef(t, s, expired) != "" {
		t.Fatal("a duplicate without an expiry gave a reference to a stored message that has expired")
	}
	if got := scalar[int](t, s, "SELECT count(*) FROM messages WHERE revoked = 0 AND media_type IS NOT NULL AND id = 'R1'"); got != 0 {
		t.Fatal("a duplicate restored the media type of a revoked message")
	}
}

func TestAReferenceExpiresWithItsRetentionOrItsMessage(t *testing.T) {
	for _, tt := range []struct {
		name               string
		ts, expiry, stored time.Time
		want               time.Time
	}{
		{name: "a message without an expiry", ts: epoch, want: epoch.Add(refRetention)},
		{name: "a message that expires first", ts: epoch, expiry: epoch.Add(time.Hour), want: epoch.Add(time.Hour)},
		{name: "a message that expires later", ts: epoch, expiry: epoch.Add(refRetention + time.Hour), want: epoch.Add(refRetention)},
		{name: "a history message near its retention", ts: epoch.Add(-refRetention + time.Minute), want: epoch.Add(time.Minute)},
		{name: "a history message past its retention", ts: epoch.Add(-refRetention)},
		{name: "a message expiring as it arrives", ts: epoch, expiry: epoch},
		{name: "a duplicate of an older stored message", ts: epoch, stored: epoch.Add(-refRetention + time.Hour), want: epoch.Add(time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			m := mediaMessage(t, groupJID, "M1", alice, "", "sealed")
			m.Timestamp, m.ExpiresAt = tt.ts, tt.expiry
			if !tt.stored.IsZero() {
				first := m
				first.Timestamp, first.Media = tt.stored, nil
				insert(t, s, first)
			}
			ref, _ := insertNew(t, s, m)
			got := storedRef(t, s, ref)
			if tt.want.IsZero() {
				if got != "" {
					t.Fatalf("a reference already past its expiry was stored as %q", got)
				}
				return
			}
			if want := refRow("sealed", MediaImage, "", 4096, tt.want, epoch); got != want {
				t.Fatalf("the reference is %q, want %q", got, want)
			}
		})
	}
}

func TestARevokeDeletesTheReferenceAndTheMediaType(t *testing.T) {
	s := openStore(t)
	ref := insert(t, s, mediaMessage(t, groupJID, "M1", alice, "first caption", "sealed-revoked"))
	kept := insert(t, s, mediaMessage(t, groupJID, "M2", alice, "other caption", "sealed-kept"))
	write(t, s, func(tx *Tx) error { return tx.ApplyEdit(ref, "second caption", epoch.Add(time.Minute)) })
	if !strings.HasPrefix(storedRef(t, s, ref), "sealed-revoked ") || scalar[string](t, s, "SELECT media_type FROM messages WHERE seq = ?", ref.seq) != "image/jpeg" {
		t.Fatal("a caption edit dropped the reference or the media type")
	}
	before := scalar[int](t, s, "SELECT max(change_seq) FROM messages")
	write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	write(t, s, func(tx *Tx) error { return tx.ApplyRevoke(ref) })
	if got := storedRef(t, s, ref); got != "" {
		t.Fatalf("the revoked message keeps the reference %q", got)
	}
	if mt := scalar[*string](t, s, "SELECT media_type FROM messages WHERE seq = ?", ref.seq); mt != nil {
		t.Fatalf("the revoked message keeps the media type %q", *mt)
	}
	if !strings.HasPrefix(storedRef(t, s, kept), "sealed-kept ") || scalar[string](t, s, "SELECT media_type FROM messages WHERE seq = ?", kept.seq) != "image/jpeg" {
		t.Fatal("the revoke reached another message's reference or media type")
	}
	if after := scalar[int](t, s, "SELECT change_seq FROM messages WHERE seq = ?", ref.seq); after <= before {
		t.Fatalf("the revoke left the change number at %d, want it after %d", after, before)
	}
	ftsIntegrity(t, s)
}

func TestPurgeExpiredClearsTheMediaTypeOfCaptionlessMedia(t *testing.T) {
	s := openStore(t)
	for _, row := range []struct {
		id, caption string
		media       bool
		expiry      time.Duration
	}{
		{"C1", "captioned", true, time.Hour},
		{"T1", "text only", false, time.Hour},
		{"N1", "", true, time.Hour},
		{"N2", "", true, 30 * time.Minute},
		{"N3", "", true, 3 * time.Hour},
		{"N4", "", true, 0},
	} {
		m := mediaMessage(t, alice, row.id, alice, row.caption, "sealed-"+row.id)
		if !row.media {
			m = textMessage(t, alice, row.id, alice, row.caption)
		}
		if row.expiry > 0 {
			m.ExpiresAt = epoch.Add(row.expiry)
		}
		insert(t, s, m)
	}
	now := epoch.Add(2 * time.Hour)
	for _, want := range []int{3, 1, 0} {
		var purged int
		write(t, s, func(tx *Tx) error {
			var err error
			purged, err = tx.PurgeExpired(now, 3)
			return err
		})
		if purged != want {
			t.Fatalf("PurgeExpired with a limit of 3 = %d, want %d", purged, want)
		}
	}
	if got := scalar[string](t, s, "SELECT group_concat(id, ',') FROM (SELECT id FROM messages WHERE media_type IS NOT NULL ORDER BY id)"); got != "N3,N4" {
		t.Fatalf("the messages keeping a media type are %q, want the unexpired N3 and N4", got)
	}
	if got := scalar[string](t, s, "SELECT group_concat(ref, ',') FROM (SELECT ref FROM media_refs ORDER BY ref)"); got != "sealed-N3,sealed-N4" {
		t.Fatalf("the references kept are %q, want the unexpired ones", got)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages WHERE text IS NOT NULL"); n != 0 {
		t.Fatalf("%d messages keep their text", n)
	}
	for query, index := range map[string]string{selectExpired: "messages_expiring", selectExpiredMedia: "messages_media_expiring"} {
		if plan := queryPlan(t, s, query, ms(now), 10); !strings.Contains(plan, "INDEX "+index+" ") || strings.Contains(plan, "SCAN") {
			t.Errorf("%q plans as %q, want a search of %s", query, plan, index)
		}
	}
	ftsIntegrity(t, s)
}

func purgeRefs(t *testing.T, s *Store, now time.Time, retention time.Duration, on bool, limit int) int {
	t.Helper()
	var n int
	write(t, s, func(tx *Tx) error {
		var err error
		n, err = tx.PurgeMediaRefs(now, retention, on, limit)
		return err
	})
	return n
}

func TestPurgeMediaRefsRemovesExpiredAndAgedReferences(t *testing.T) {
	s := openStore(t)
	captionless := mediaMessage(t, alice, "A", alice, "", "sealed-A")
	captionless.ExpiresAt = epoch.Add(time.Hour)
	insert(t, s, captionless)
	insert(t, s, mediaMessage(t, alice, "B", alice, "a caption", "sealed-B"))
	later := mediaMessage(t, alice, "C", alice, "", "sealed-C")
	later.Timestamp, later.Ingested = epoch.Add(10*24*time.Hour), epoch.Add(10*24*time.Hour)
	insert(t, s, later)
	kept := func() string {
		return scalar[string](t, s, "SELECT coalesce(group_concat(ref, ','), '') FROM (SELECT ref FROM media_refs ORDER BY ref)")
	}
	if n := purgeRefs(t, s, epoch.Add(time.Hour-time.Millisecond), refRetention, true, 10); n != 0 || kept() != "sealed-A,sealed-B,sealed-C" {
		t.Fatalf("a purge before any expiry removed %d references, keeping %q", n, kept())
	}
	if n := purgeRefs(t, s, epoch.Add(time.Hour), refRetention, true, 10); n != 1 || kept() != "sealed-B,sealed-C" {
		t.Fatalf("a purge at the captionless message's expiry removed %d, keeping %q", n, kept())
	}
	if n := purgeRefs(t, s, epoch.Add(refRetention), refRetention, true, 10); n != 1 || kept() != "sealed-C" {
		t.Fatalf("a purge at the end of the retention removed %d, keeping %q", n, kept())
	}
	if n := purgeRefs(t, s, epoch.Add(refRetention), 20*24*time.Hour, true, 10); n != 1 || kept() != "" {
		t.Fatalf("a purge with a shorter retention removed %d, keeping %q", n, kept())
	}
	for i := range 5 {
		m := mediaMessage(t, alice, fmt.Sprintf("X%d", i), alice, "", "sealed-X")
		m.ExpiresAt = epoch.Add(time.Hour)
		insert(t, s, m)
	}
	for _, want := range []int{2, 2, 1, 0} {
		if n := purgeRefs(t, s, epoch.Add(2*time.Hour), refRetention, true, 2); n != want {
			t.Fatalf("a purge with a limit of 2 removed %d, want %d", n, want)
		}
	}
	for query, index := range map[string]string{selectExpiredRefs: "media_refs_expiry", selectAgedRefs: "media_refs_added", selectAllRefs: "SCAN media_refs",
		selectExpiredTombstones: "revoke_tombstones_expiry", selectOldestTombstones: "revoke_tombstones_expiry"} {
		args := []any{ms(epoch), 10}
		if query == selectOldestTombstones || query == selectAllRefs {
			args = args[1:]
		}
		if plan := queryPlan(t, s, query, args...); !strings.Contains(plan, index) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%q plans as %q, want %s in order", query, plan, index)
		}
	}
	for name, purge := range map[string]func(*Tx) error{
		"no time":          func(tx *Tx) error { _, err := tx.PurgeMediaRefs(time.Time{}, refRetention, true, 10); return err },
		"no retention":     func(tx *Tx) error { _, err := tx.PurgeMediaRefs(epoch, 0, true, 10); return err },
		"no limit":         func(tx *Tx) error { _, err := tx.PurgeMediaRefs(epoch, refRetention, true, 0); return err },
		"a negative limit": func(tx *Tx) error { _, err := tx.PurgeMediaRefs(epoch, refRetention, false, -1); return err },
	} {
		if err := s.Write(t.Context(), "test.purge", purge); !errors.Is(err, ErrInvalid) {
			t.Errorf("PurgeMediaRefs with %s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestPurgeMediaRefsRemovesEveryReferenceWhileDownloadsAreOff(t *testing.T) {
	s := openStore(t)
	for i := range 3 {
		insert(t, s, mediaMessage(t, alice, fmt.Sprintf("M%d", i), alice, "", fmt.Sprintf("sealed-%d", i)))
	}
	write(t, s, func(tx *Tx) error {
		return tx.PutRevokeTombstone(chat(t, alice), "GONE", chat(t, alice), epoch.Add(-8*24*time.Hour))
	})
	for _, want := range []int{2, 2, 0} {
		if n := purgeRefs(t, s, epoch, refRetention, false, 2); n != want {
			t.Fatalf("a purge while downloads are off removed %d, want %d", n, want)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM media_refs") + scalar[int](t, s, "SELECT count(*) FROM revoke_tombstones"); n != 0 {
		t.Fatalf("%d references or tombstones survive a purge while downloads are off", n)
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM messages WHERE media_type = 'image/jpeg'"); n != 3 {
		t.Fatalf("%d messages keep their media type, want all three: turning downloads off removes only references", n)
	}
}

func TestExpireMediaRefMovesTheExpiryForward(t *testing.T) {
	s := openStore(t)
	learn(t, s, aliceLID, alice, MappingSenderAlt)
	ref := insert(t, s, mediaMessage(t, groupJID, "M1", alice, "", "sealed-1"))
	other := insert(t, s, mediaMessage(t, groupJID, "M2", alice, "", "sealed-2"))
	expire := func(id, sender string, at time.Time) {
		t.Helper()
		write(t, s, func(tx *Tx) error { return tx.ExpireMediaRef(chat(t, groupJID), id, chat(t, sender), at) })
	}
	expiry := func(r Ref) int64 {
		return scalar[int64](t, s, "SELECT expires_at FROM media_refs WHERE seq = ?", r.seq)
	}
	expire("M1", alice, epoch.Add(time.Hour))
	expire("M1", aliceLID, epoch.Add(2*time.Hour))
	expire("M1", bob, epoch)
	expire("ABSENT", alice, epoch)
	if expiry(ref) != ms(epoch.Add(time.Hour)) || expiry(other) != ms(epoch.Add(refRetention)) {
		t.Fatalf("expiries %d and %d, want the first moved to an hour and the second kept", expiry(ref), expiry(other))
	}
	if n := purgeRefs(t, s, epoch.Add(time.Hour), refRetention, true, 10); n != 1 || storedRef(t, s, ref) != "" || storedRef(t, s, other) == "" {
		t.Fatalf("the purge after an expiry removed %d references", n)
	}
	for name, call := range map[string]func(*Tx) error{
		"no time":        func(tx *Tx) error { return tx.ExpireMediaRef(chat(t, groupJID), "M2", chat(t, alice), time.Time{}) },
		"an invalid id":  func(tx *Tx) error { return tx.ExpireMediaRef(chat(t, groupJID), "M 2", chat(t, alice), epoch) },
		"a group sender": func(tx *Tx) error { return tx.ExpireMediaRef(chat(t, groupJID), "M2", chat(t, group2JID), epoch) },
	} {
		if err := s.Write(t.Context(), "test.expire", call); !errors.Is(err, ErrInvalid) {
			t.Errorf("ExpireMediaRef with %s = %v, want ErrInvalid", name, err)
		}
	}
}

func tombstone(t *testing.T, s *Store, chatJID, id, senderJID string, at time.Time) {
	t.Helper()
	write(t, s, func(tx *Tx) error { return tx.PutRevokeTombstone(chat(t, chatJID), id, chat(t, senderJID), at) })
}

func TestALateCopyOfARevokedMessageIsStoredRevoked(t *testing.T) {
	for _, origin := range []Origin{OriginLive, OriginHistory} {
		t.Run(string(origin), func(t *testing.T) {
			s := openStore(t)
			learn(t, s, aliceLID, alice, MappingSenderAlt)
			tombstone(t, s, groupJID, "LATE", alice, epoch)
			if got := scalar[string](t, s, "SELECT chat_jid || ' ' || id || ' ' || sender_jid || ' ' || expires_at FROM revoke_tombstones"); got != fmt.Sprintf("%s LATE %s %d", groupJID, aliceLID, ms(epoch.Add(7*24*time.Hour))) {
				t.Fatalf("the tombstone is %q, want it keyed by the canonical sender with a 7-day expiry", got)
			}
			other := mediaMessage(t, groupJID, "LATE", bob, "from another sender", "sealed-other")
			other.Origin = origin
			otherRef, inserted := insertNew(t, s, other)
			if f, _ := resolve(t, s, groupJID, "LATE", bob); !inserted || f.Revoked || f.Text != "from another sender" || storedRef(t, s, otherRef) == "" {
				t.Fatalf("a copy from another sender was stored as %+v", f)
			}
			if n := scalar[int](t, s, "SELECT count(*) FROM revoke_tombstones"); n != 1 {
				t.Fatal("a copy from another sender consumed the tombstone")
			}
			late := mediaMessage(t, groupJID, "LATE", alice, "the late caption", "sealed-late")
			late.Origin, late.Ingested = origin, epoch.Add(time.Hour)
			ref, inserted := insertNew(t, s, late)
			if !inserted {
				t.Fatal("the late copy was not stored")
			}
			f, ok := resolve(t, s, groupJID, "LATE", aliceLID)
			if !ok || f.Ref != ref || !f.Revoked || f.Text != "" || f.Kind != KindMedia {
				t.Fatalf("the late copy was stored as %+v, want it revoked without text", f)
			}
			if got := scalar[string](t, s, "SELECT coalesce(text, '-') || coalesce(text_display, '-') || coalesce(media_type, '-') FROM messages WHERE seq = ?", ref.seq); got != "---" {
				t.Fatalf("the late copy stored %q, want no text, display text or media type", got)
			}
			if storedRef(t, s, ref) != "" || matches(t, s, "late caption") != 0 {
				t.Fatal("the late copy stored a reference or indexed its caption")
			}
			if n := scalar[int](t, s, "SELECT count(*) FROM revoke_tombstones"); n != 0 {
				t.Fatal("the late copy left its tombstone")
			}
			if cs := scalar[int](t, s, "SELECT change_seq FROM messages WHERE seq = ?", ref.seq); cs != scalar[int](t, s, "SELECT max(change_seq) FROM messages") {
				t.Fatal("the late copy has no change number")
			}
			late.Ingested = epoch.Add(2 * time.Hour)
			if _, inserted := insertNew(t, s, late); inserted || storedRef(t, s, ref) != "" {
				t.Fatal("a second copy restored the reference")
			}
			if f, _ := resolve(t, s, groupJID, "LATE", alice); !f.Revoked || f.Text != "" {
				t.Fatalf("a second copy undid the revoke: %+v", f)
			}
			ftsIntegrity(t, s)
		})
	}
}

func TestATombstoneDoesNotTouchAStoredMessage(t *testing.T) {
	s := openStore(t)
	ref := insert(t, s, mediaMessage(t, alice, "M1", alice, "stored first", "sealed-1"))
	tombstone(t, s, alice, "M1", alice, epoch)
	if _, inserted := insertNew(t, s, mediaMessage(t, alice, "M1", alice, "stored first", "sealed-1")); inserted {
		t.Fatal("a duplicate was stored again")
	}
	if f, _ := resolve(t, s, alice, "M1", alice); f.Revoked || f.Text != "stored first" || storedRef(t, s, ref) == "" {
		t.Fatalf("a tombstone changed a stored message: %+v", f)
	}
}

func TestTombstonesExpireAfterSevenDays(t *testing.T) {
	s := openStore(t)
	tombstone(t, s, alice, "OLD", alice, epoch)
	tombstone(t, s, alice, "OLD", alice, epoch.Add(-time.Hour))
	if got := scalar[int64](t, s, "SELECT expires_at FROM revoke_tombstones"); got != ms(epoch.Add(tombstoneLife)) {
		t.Fatalf("an older revoke moved the tombstone's expiry to %d", got)
	}
	tombstone(t, s, alice, "OLD", alice, epoch.Add(time.Hour))
	end := epoch.Add(tombstoneLife + time.Hour)
	if got := scalar[int64](t, s, "SELECT expires_at FROM revoke_tombstones"); got != ms(end) {
		t.Fatalf("a newer revoke left the tombstone's expiry at %d, want %d", got, ms(end))
	}
	late := textMessage(t, alice, "OLD", alice, "after the tombstone expired")
	late.Ingested = end
	ref, _ := insertNew(t, s, late)
	if f, _ := resolve(t, s, alice, "OLD", alice); f.Ref != ref || f.Revoked || f.Text != "after the tombstone expired" {
		t.Fatalf("an expired tombstone revoked a late copy: %+v", f)
	}
	if n := purgeRefs(t, s, end.Add(-time.Millisecond), refRetention, true, 10); n != 0 {
		t.Fatalf("a purge before the expiry removed %d tombstones", n)
	}
	if n := purgeRefs(t, s, end, refRetention, true, 10); n != 1 || scalar[int](t, s, "SELECT count(*) FROM revoke_tombstones") != 0 {
		t.Fatalf("a purge at the expiry removed %d tombstones", n)
	}
	for name, call := range map[string]func(*Tx) error{
		"no time":        func(tx *Tx) error { return tx.PutRevokeTombstone(chat(t, alice), "M1", chat(t, alice), time.Time{}) },
		"an invalid id":  func(tx *Tx) error { return tx.PutRevokeTombstone(chat(t, alice), "", chat(t, alice), epoch) },
		"a group sender": func(tx *Tx) error { return tx.PutRevokeTombstone(chat(t, groupJID), "M1", chat(t, group2JID), epoch) },
		"no chat":        func(tx *Tx) error { return tx.PutRevokeTombstone(policy.CanonicalChat{}, "M1", chat(t, alice), epoch) },
	} {
		if err := s.Write(t.Context(), "test.tombstone", call); !errors.Is(err, ErrInvalid) {
			t.Errorf("PutRevokeTombstone with %s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestTombstonesAreCappedAtTheOldest100000(t *testing.T) {
	s := openStore(t)
	if err := execAll(t, s, fmt.Sprintf("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d) "+
		"INSERT INTO revoke_tombstones (chat_jid, id, sender_jid, expires_at) SELECT '%s', 'T' || i, '%s', %d + i FROM n", tombstoneCap+5, groupJID, alice, ms(epoch))); err != nil {
		t.Fatalf("seed the tombstones: %v", err)
	}
	if n := purgeRefs(t, s, epoch, refRetention, true, 3); n != 3 {
		t.Fatalf("a purge with a limit of 3 removed %d tombstones beyond the cap", n)
	}
	if n := purgeRefs(t, s, epoch, refRetention, false, 100); n != 2 {
		t.Fatalf("a purge while downloads are off removed %d tombstones beyond the cap, want the last 2", n)
	}
	if n := purgeRefs(t, s, epoch, refRetention, true, 100); n != 0 {
		t.Fatalf("a purge at the cap removed %d tombstones", n)
	}
	if n, oldest := scalar[int](t, s, "SELECT count(*) FROM revoke_tombstones"), scalar[string](t, s, "SELECT id FROM revoke_tombstones ORDER BY expires_at LIMIT 1"); n != tombstoneCap || oldest != "T6" {
		t.Fatalf("%d tombstones remain, the oldest %s, want %d from T6", n, oldest, tombstoneCap)
	}
}
