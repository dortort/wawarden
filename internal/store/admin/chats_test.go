package admin_test

import (
	"testing"

	"github.com/dortort/wawarden/internal/store/ingest"
)

func TestChatsListsNamesIdentifiersAndReferences(t *testing.T) {
	f := open(t)
	f.seed(t, phoneA, phoneB, groupG, groupH)
	err := f.store.Write(t.Context(), "test.names", func(tx *ingest.Tx) error {
		if err := tx.SetChatName(chat(t, groupG), "Synthetic Book Club", ingest.NameGroupSubject, ingest.OriginLive); err != nil {
			return err
		}
		return tx.SetChatName(chat(t, phoneA), "Test Person", ingest.NamePushName, ingest.OriginLive)
	})
	if err != nil {
		t.Fatalf("SetChatName: %v", err)
	}
	all, truncated, err := f.store.Admin().Chats(t.Context(), "", 10)
	if err != nil || truncated || len(all) != 4 {
		t.Fatalf("Chats = %d chats, %v, %v", len(all), truncated, err)
	}
	refs := map[string]string{}
	for _, c := range all {
		if len(c.Ref) != 32 || !c.Chat.Valid() {
			t.Fatalf("chat %+v has no reference or identifier", c)
		}
		refs[c.Chat.JID()] = c.Ref
	}
	tests := []struct {
		match string
		want  []string
	}{
		{match: "book club", want: []string{groupG}},
		{match: "PERSON", want: []string{phoneA}},
		{match: "0100002", want: []string{phoneB}},
		{match: "@g.us", want: []string{groupG, groupH}},
		{match: refs[groupH], want: []string{groupH}},
		{match: "absent", want: nil},
	}
	for _, tt := range tests {
		got, _, err := f.store.Admin().Chats(t.Context(), tt.match, 10)
		if err != nil {
			t.Fatalf("Chats(%q): %v", tt.match, err)
		}
		var jids []string
		for _, c := range got {
			jids = append(jids, c.Chat.JID())
		}
		if len(jids) != len(tt.want) {
			t.Fatalf("Chats(%q) = %v, want %v", tt.match, jids, tt.want)
		}
		for _, w := range tt.want {
			found := false
			for _, j := range jids {
				found = found || j == w
			}
			if !found {
				t.Fatalf("Chats(%q) = %v, want %v", tt.match, jids, tt.want)
			}
		}
	}
	two, truncated, err := f.store.Admin().Chats(t.Context(), "", 2)
	if err != nil || !truncated || len(two) != 2 {
		t.Fatalf("Chats with a limit of 2 = %d chats, truncated %v, %v", len(two), truncated, err)
	}
}
