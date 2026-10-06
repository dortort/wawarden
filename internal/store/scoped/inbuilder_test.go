package scoped_test

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const scopedTemplate = "SELECT 1 FROM messages m LEFT JOIN contact_names n ON n.jid = m.sender_jid{scope n.jid} WHERE m.chat_jid = ?1 AND m.ts < ?2{scope m.chat_jid}"

var setClause = regexp.MustCompile(`^SELECT 1 FROM messages m LEFT JOIN contact_names n ON n\.jid = m\.sender_jid AND n\.jid IN \((\?\d+(?:,\?\d+)*)\) WHERE m\.chat_jid = \?1 AND m\.ts < \?2 AND m\.chat_jid IN \((\?\d+(?:,\?\d+)*)\)$`)

func checkClause(t tb, g policy.ReadGrant, want []string) {
	t.Helper()
	text, args, err := scoped.Expand(g, scopedTemplate, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	switch {
	case !g.Valid() || !g.All() && len(want) == 0:
		if text != strings.ReplaceAll(strings.ReplaceAll(scopedTemplate, "{scope n.jid}", " AND 0"), "{scope m.chat_jid}", " AND 0") || len(args) != 0 {
			t.Fatalf("an empty or invalid grant gave %q, %q, want AND 0 and no argument", text, args)
		}
	case g.All():
		if text != strings.ReplaceAll(strings.ReplaceAll(scopedTemplate, "{scope n.jid}", " AND 1"), "{scope m.chat_jid}", " AND 1") || len(args) != 0 {
			t.Fatalf("an all-chats grant gave %q, %q, want AND 1 and no argument", text, args)
		}
	default:
		m := setClause.FindStringSubmatch(text)
		var placeholders []string
		for i := range want {
			placeholders = append(placeholders, "?"+strconv.Itoa(3+i))
		}
		if m == nil || m[1] != strings.Join(placeholders, ",") || m[2] != m[1] {
			t.Fatalf("a grant of %d chats gave %q", len(want), text)
		}
		got := make([]string, len(args))
		for i, a := range args {
			got[i] = a.(string)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("arguments %q, want the sorted distinct chats %q", got, want)
		}
	}
}

func TestTheINBuilderHasThreeCases(t *testing.T) {
	checkClause(t, policy.ReadGrant{}, nil)
	checkClause(t, grant(t), nil)
	checkClause(t, grantAll(t), nil)
	checkClause(t, grant(t, groupJID, alice, bobLID, alice), []string{bobLID, groupJID, alice})
	revoked, _ := policy.DecideRead(&policy.Client{ID: "client01", ReadAll: true, Revoked: true, ExpiresAt: epoch.Add(time.Hour)}, epoch)
	checkClause(t, revoked, nil)
	if _, _, err := scoped.Expand(grantAll(t), "SELECT 1 FROM messages", 0); err == nil {
		t.Fatal("Expand accepted a query without a scope clause")
	}
}

func TestOnlyTheReviewedScalarsRunWithoutAScope(t *testing.T) {
	s := openStore(t)
	for _, query := range []string{"SELECT text FROM messages LIMIT 1", scoped.Queries["selectMessage"], scoped.Queries["selectChat"]} {
		if err := s.Scoped().Scalar(grantAll(t), t.Context(), query); !errors.Is(err, scoped.ErrUnscoped) {
			t.Errorf("a single-row read of %q = %v, want it refused", query, err)
		}
	}
	for _, name := range []string{"selectTopChange", "selectTopSeq"} {
		if err := s.Scoped().Scalar(grant(t), t.Context(), scoped.Queries[name]); err != nil {
			t.Errorf("%s = %v, want the reviewed scalar to run", name, err)
		}
	}
}

func chatGenerator() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		digits := rapid.StringMatching(`[1-9][0-9]{5,14}`).Draw(t, "digits")
		return digits + rapid.SampledFrom([]string{"@s.whatsapp.net", "@lid", "@c.us"}).Draw(t, "server")
	})
}

func TestTheINBuilderBindsTheSortedDistinctChatsOfAnyGrant(t *testing.T) {
	cases := map[string]int{}
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.IntRange(0, 3).Draw(t, "kind")
		var g policy.ReadGrant
		var want []string
		switch kind {
		case 0:
		case 1:
			g, _ = policy.DecideRead(&policy.Client{ID: "client01", ReadAll: true, ExpiresAt: epoch.Add(time.Hour)}, epoch)
		default:
			jids := rapid.SliceOfN(chatGenerator(), 0, 40).Draw(t, "chats")
			read := map[policy.CanonicalChat]struct{}{}
			for _, jid := range jids {
				c, ok := policy.Normalize(jid)
				if ok {
					read[c] = struct{}{}
					want = append(want, c.JID())
				}
			}
			slices.Sort(want)
			want = slices.Compact(want)
			g, _ = policy.DecideRead(&policy.Client{ID: "client01", Read: read, ExpiresAt: epoch.Add(time.Hour)}, epoch)
		}
		switch {
		case kind == 1:
			cases["all"]++
		case len(want) == 0:
			cases["empty"]++
		default:
			cases["set"]++
		}
		checkClause(t, g, want)
	})
	for _, c := range []string{"all", "empty", "set"} {
		if cases[c] == 0 {
			t.Errorf("no check drew the %s case: %v", c, cases)
		}
	}
}

func FuzzINBuilderText(f *testing.F) {
	f.Add("15550100001@s.whatsapp.net\n100000000000001@lid\n120363000000000001@g.us")
	f.Add("x'; DROP TABLE messages;--\n15550100001@s.whatsapp.net")
	f.Add("")
	f.Fuzz(func(t *testing.T, input string) {
		read := map[policy.CanonicalChat]struct{}{}
		var want []string
		for _, jid := range strings.Split(input, "\n") {
			if c, ok := policy.Normalize(jid); ok {
				read[c] = struct{}{}
				want = append(want, c.JID())
			}
		}
		slices.Sort(want)
		want = slices.Compact(want)
		g, _ := policy.DecideRead(&policy.Client{ID: "client01", Read: read, ExpiresAt: epoch.Add(time.Hour)}, epoch)
		text, args, err := scoped.Expand(g, scopedTemplate, 2)
		if len(want) > scoped.MaxGrantChats {
			if !errors.Is(err, scoped.ErrGrantTooLarge) {
				t.Fatalf("a grant of %d chats = %v", len(want), err)
			}
			return
		}
		for _, jid := range want {
			if strings.Contains(text, jid) {
				t.Fatalf("the SQL text %q carries the chat %q", text, jid)
			}
		}
		if len(args) != len(want) {
			t.Fatalf("%d arguments for %d chats", len(args), len(want))
		}
		checkClause(t, g, want)
	})
}
