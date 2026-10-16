package scoped_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	sealedText = "SYNTHETIC-SEALED-1"
	refLife    = 30 * 24 * time.Hour
)

func mediaClient(t tb, all bool, jids ...string) policy.ReadGrant {
	t.Helper()
	read := map[policy.CanonicalChat]struct{}{}
	for _, jid := range jids {
		read[chat(t, jid)] = struct{}{}
	}
	g, ok := policy.DecideRead(&policy.Client{ID: "client01", Read: read, ReadAll: all, AllowMedia: true, ExpiresAt: epoch.Add(24 * time.Hour)}, epoch)
	if !ok || !g.Media() {
		t.Fatalf("DecideRead refused a live client with media")
	}
	return g
}

func mediaGrant(t tb, jids ...string) policy.ReadGrant {
	t.Helper()
	return mediaClient(t, false, jids...)
}

func mediaGrantAll(t tb) policy.ReadGrant {
	t.Helper()
	return mediaClient(t, true)
}

func mediaMessage(t tb, chatJID, id, senderJID string, at time.Time) ingest.Message {
	t.Helper()
	m := message(t, chatJID, id, senderJID, "a synthetic caption", at)
	m.Kind, m.MediaType = ingest.KindMedia, "image/jpeg"
	m.Media = &ingest.Media{Ref: sealedText, Kind: ingest.MediaImage, Declared: 4096, Retention: refLife}
	return m
}

func attachment(t *testing.T, r *scoped.Reader, g policy.ReadGrant, chatJID, id, senderJID string, now time.Time) (scoped.Attachment, bool) {
	t.Helper()
	a, found, err := r.Attachment(g, t.Context(), chat(t, chatJID), id, chat(t, senderJID), now)
	if err != nil {
		t.Fatalf("Attachment(%s, %s) = %v", chatJID, id, err)
	}
	return a, found
}

func requireUnusable(t *testing.T, name string, a scoped.Attachment, found bool, chatJID, id, senderJID string) {
	t.Helper()
	want := scoped.Attachment{Chat: chat(t, chatJID), ID: id, Sender: chat(t, senderJID)}
	if !found || a != want {
		t.Fatalf("%s = %+v, %v, want the message found with no usable reference", name, a, found)
	}
}

func TestAnAttachmentInScopeCarriesItsReference(t *testing.T) {
	s := openStore(t)
	video := mediaMessage(t, alice, "V", alice, epoch)
	video.MediaType = "video/mp4"
	video.Media.Kind, video.Media.Variant, video.Media.Declared = ingest.MediaVideo, "gif", 8192
	insert(t, s, mediaMessage(t, alice, "I", alice, epoch), video)
	for _, tt := range []struct {
		id, kind, variant string
		declared          int64
	}{{"I", "image", "", 4096}, {"V", "video", "gif", 8192}} {
		a, found := attachment(t, s.Scoped(), mediaGrant(t, alice), alice, tt.id, alice, epoch)
		if !found || !a.Usable || a.Ref.Sealed() != sealedText || a.Kind != tt.kind || a.Variant != tt.variant || a.Declared != tt.declared ||
			!a.Expires.Equal(epoch.Add(refLife)) || a.Chat.JID() != alice || a.ID != tt.id || a.Sender.JID() != alice {
			t.Fatalf("Attachment(%s) = %+v, %v", tt.id, a, found)
		}
	}
	write(t, s, func(tx *ingest.Tx) error {
		_, err := tx.LearnLID(chat(t, aliceLID), chat(t, alice), ingest.MappingSenderAlt, epoch)
		return err
	})
	for _, c := range []string{alice, aliceLID} {
		a, found := attachment(t, s.Scoped(), mediaGrant(t, aliceLID), c, "I", alice, epoch)
		if !found || !a.Usable || a.Chat.JID() != aliceLID || a.Sender.JID() != aliceLID || a.Ref.Sealed() != sealedText {
			t.Fatalf("Attachment through %s = %+v, %v, want the canonical chat and sender", c, a, found)
		}
	}
}

func TestEveryAttachmentDenialIsTheSameNotFound(t *testing.T) {
	s := openStore(t)
	insert(t, s, mediaMessage(t, alice, "M", alice, epoch), mediaMessage(t, bob, "B", bob, epoch))
	grants := []struct {
		name  string
		grant policy.ReadGrant
		media bool
		scope bool
	}{
		{"media for every chat", mediaGrantAll(t), true, true},
		{"media for the chat", mediaGrant(t, alice), true, true},
		{"media for another chat", mediaGrant(t, bob), true, false},
		{"every chat without media", grantAll(t), false, true},
		{"the chat without media", grant(t, alice), false, true},
		{"the zero grant", policy.ReadGrant{}, false, false},
	}
	probes := []struct {
		name, id string
		exists   bool
	}{{"a message with a reference", "M", true}, {"a missing message", "missing", false}, {"a garbage id", "bad id", false}}
	var denied [][]string
	for _, g := range grants {
		for _, p := range probes {
			r, log := traced(s)
			a, found, err := r.Attachment(g.grant, t.Context(), chat(t, alice), p.id, chat(t, alice), epoch)
			if err != nil {
				t.Fatalf("%s, %s = %v", g.name, p.name, err)
			}
			if want := g.media && g.scope && p.exists; found != want || want && !a.Usable || !want && a != (scoped.Attachment{}) {
				t.Fatalf("%s, %s = %+v, %v, want found %v", g.name, p.name, a, found, want)
			}
			if !g.media || !g.scope {
				for _, q := range *log {
					if strings.Contains(q, "FROM messages") {
						t.Fatalf("%s, %s reached the messages table: %q", g.name, p.name, q)
					}
				}
				if p.id != "bad id" {
					denied = append(denied, *log)
				}
			}
		}
	}
	if len(denied) != 8 || len(denied[0]) == 0 {
		t.Fatalf("denials = %q", denied)
	}
	for _, trace := range denied[1:] {
		if !slices.Equal(trace, denied[0]) {
			t.Fatalf("a grant without media, a chat out of scope and a missing message issued different SQL: %q", denied)
		}
	}
}

func TestAnAttachmentReadRefusesInvalidInputBeforeAnyQuery(t *testing.T) {
	s := openStore(t)
	insert(t, s, mediaMessage(t, alice, "M", alice, epoch))
	r, log := traced(s)
	if _, _, err := r.Attachment(mediaGrantAll(t), t.Context(), chat(t, alice), "M", chat(t, alice), time.Time{}); !errors.Is(err, scoped.ErrInvalid) {
		t.Fatalf("Attachment without a time = %v, want ErrInvalid", err)
	}
	for _, bad := range []struct {
		chat   policy.CanonicalChat
		id     string
		sender policy.CanonicalChat
	}{{policy.CanonicalChat{}, "M", chat(t, alice)}, {chat(t, alice), "bad id", chat(t, alice)}, {chat(t, alice), "", chat(t, alice)},
		{chat(t, alice), "M", chat(t, groupJID)}, {chat(t, alice), "M", policy.CanonicalChat{}}} {
		if a, found, err := r.Attachment(mediaGrantAll(t), t.Context(), bad.chat, bad.id, bad.sender, epoch); found || err != nil || a != (scoped.Attachment{}) {
			t.Fatalf("Attachment with invalid input = %+v, %v, %v", a, found, err)
		}
	}
	if len(*log) != 0 {
		t.Fatalf("invalid input reached the database: %q", *log)
	}
}

func TestAnAttachmentPastItsExpiryIsNotUsable(t *testing.T) {
	s := openStore(t)
	brief := mediaMessage(t, alice, "D", alice, epoch)
	brief.ExpiresAt = epoch.Add(time.Hour)
	insert(t, s, mediaMessage(t, alice, "M", alice, epoch), brief, mediaMessage(t, alice, "X", alice, epoch))
	g := mediaGrant(t, alice)
	for _, tt := range []struct {
		id     string
		expiry time.Time
	}{{"M", epoch.Add(refLife)}, {"D", epoch.Add(time.Hour)}} {
		if a, found := attachment(t, s.Scoped(), g, alice, tt.id, alice, tt.expiry.Add(-time.Millisecond)); !found || !a.Usable || !a.Expires.Equal(tt.expiry) {
			t.Fatalf("Attachment(%s) before its expiry = %+v, %v", tt.id, a, found)
		}
		for _, now := range []time.Time{tt.expiry, tt.expiry.Add(time.Hour)} {
			a, found := attachment(t, s.Scoped(), g, alice, tt.id, alice, now)
			requireUnusable(t, "Attachment("+tt.id+") at "+now.String(), a, found, alice, tt.id, alice)
		}
	}
	write(t, s, func(tx *ingest.Tx) error {
		return tx.ExpireMediaRef(chat(t, alice), "X", chat(t, alice), epoch.Add(time.Minute))
	})
	a, found := attachment(t, s.Scoped(), g, alice, "X", alice, epoch.Add(time.Minute))
	requireUnusable(t, "Attachment after the reference was expired", a, found, alice, "X", alice)
}

func TestAMessageWithoutAReferenceIsFoundButNotUsable(t *testing.T) {
	s := openStore(t)
	viewOnce := mediaMessage(t, alice, "O", alice, epoch)
	viewOnce.Media = nil
	late := mediaMessage(t, alice, "L", alice, epoch)
	late.Ingested = epoch.Add(refLife)
	insert(t, s, message(t, alice, "T", alice, "plain text", epoch), viewOnce, late)
	for _, id := range []string{"T", "O", "L"} {
		a, found := attachment(t, s.Scoped(), mediaGrant(t, alice), alice, id, alice, epoch)
		requireUnusable(t, "Attachment("+id+")", a, found, alice, id, alice)
	}
}

func TestARevokedAttachmentIsNotUsable(t *testing.T) {
	s := openStore(t)
	refs := insert(t, s, mediaMessage(t, alice, "A", alice, epoch), mediaMessage(t, alice, "R", alice, epoch), mediaMessage(t, alice, "N", alice, epoch),
		mediaMessage(t, alice, "K", alice, epoch))
	exec(t, s, "UPDATE messages SET revoked = 1 WHERE id = 'R'", "UPDATE messages SET media_type = NULL WHERE id = 'N'", "UPDATE messages SET kind = 'text' WHERE id = 'K'")
	if kept, err := s.Scoped().Column(t.Context(), "SELECT ref FROM media_refs ORDER BY seq"); err != nil || len(kept) != 4 {
		t.Fatalf("the references = %q, %v, want every reference still stored", kept, err)
	}
	write(t, s, func(tx *ingest.Tx) error { return tx.ApplyRevoke(refs[0]) })
	for _, id := range []string{"A", "R", "N", "K"} {
		a, found := attachment(t, s.Scoped(), mediaGrantAll(t), alice, id, alice, epoch)
		requireUnusable(t, "Attachment("+id+")", a, found, alice, id, alice)
	}
}

func TestAnAttachmentWithACorruptKindIsRefused(t *testing.T) {
	s := openStore(t)
	insert(t, s, mediaMessage(t, alice, "M", alice, epoch))
	exec(t, s, "PRAGMA ignore_check_constraints = 1", "UPDATE media_refs SET kind = 'archive'", "PRAGMA ignore_check_constraints = 0")
	if a, found, err := s.Scoped().Attachment(mediaGrantAll(t), t.Context(), chat(t, alice), "M", chat(t, alice), epoch); err == nil || found || a != (scoped.Attachment{}) {
		t.Fatalf("Attachment with a corrupt kind = %+v, %v, %v", a, found, err)
	}
}

func TestARevokedOrExpiredRowShowsNoMediaType(t *testing.T) {
	s := openStore(t)
	brief := mediaMessage(t, alice, "E", alice, epoch.Add(time.Minute))
	brief.ExpiresAt = epoch.Add(time.Hour)
	insert(t, s, mediaMessage(t, alice, "R", alice, epoch), brief, mediaMessage(t, alice, "L", alice, epoch.Add(2*time.Minute)))
	exec(t, s, "UPDATE messages SET revoked = 1 WHERE id = 'R'")
	write(t, s, func(tx *ingest.Tx) error {
		_, err := tx.PurgeExpired(epoch.Add(2*time.Hour), 10)
		return err
	})
	if kept, err := s.Scoped().Column(t.Context(), "SELECT media_type FROM messages WHERE id = 'R'"); err != nil || len(kept) != 1 || kept[0] != "image/jpeg" {
		t.Fatalf("the revoked row's media type = %q, %v, want it still stored", kept, err)
	}
	ref := refOf(t, s, alice)
	page, _, err := s.Scoped().Messages(grantAll(t), t.Context(), ref, scoped.MessagePosition{}, scoped.Older, 5)
	if err != nil || len(page.Messages) != 3 {
		t.Fatalf("Messages = %+v, %v", page, err)
	}
	changes, _, err := s.Scoped().Changes(grantAll(t), t.Context(), "", scoped.ChangePosition{}, 5)
	if err != nil || len(changes.Messages) != 3 {
		t.Fatalf("Changes = %+v, %v", changes, err)
	}
	q, err := scoped.ParseQuery("synthetic caption")
	if err != nil {
		t.Fatal(err)
	}
	found, _, err := s.Scoped().Search(grantAll(t), t.Context(), q, "", scoped.SearchPosition{}, 5)
	if err != nil || len(found.Messages) != 1 || found.Messages[0].ID != "L" {
		t.Fatalf("Search = %+v, %v", found, err)
	}
	rows := map[string][]scoped.Message{"Messages": page.Messages, "Changes": changes.Messages, "Search": found.Messages}
	for _, id := range []string{"R", "E", "L"} {
		one, ok, err := s.Scoped().Message(grantAll(t), t.Context(), chat(t, alice), id, chat(t, alice))
		if err != nil || !ok {
			t.Fatalf("Message(%s) = %v, %v", id, ok, err)
		}
		rows["Message"] = append(rows["Message"], one)
	}
	for name, ms := range rows {
		for _, m := range ms {
			if want := map[string]string{"L": "image/jpeg"}[m.ID]; m.MediaType != want {
				t.Errorf("%s: %s media type = %q, want %q", name, m.ID, m.MediaType, want)
			}
		}
	}
}

func TestAnAttachmentReadNeverQueues(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.ReadSlots = 1
	s := openWith(t, opts)
	insert(t, s, mediaMessage(t, alice, "M", alice, epoch))
	held, release := make(chan struct{}), make(chan struct{})
	done := holdUntilHeld(t, held, release, func() error { return s.Scoped().Hold(grantAll(t), context.Background(), held, release) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, found, err := s.Scoped().Attachment(mediaGrantAll(t), ctx, chat(t, alice), "M", chat(t, alice), epoch)
	took := time.Since(start)
	close(release)
	if !errors.Is(err, scoped.ErrBusy) || found || took > 5*time.Second {
		t.Fatalf("Attachment while every slot is held = %v, %v after %v, want ErrBusy at once", found, err, took)
	}
	if err := <-done; err != nil {
		t.Fatalf("the held read = %v", err)
	}
	if a, found := attachment(t, s.Scoped(), mediaGrantAll(t), alice, "M", alice, epoch); !found || !a.Usable {
		t.Fatalf("Attachment after the slot was freed = %+v, %v", a, found)
	}
}
