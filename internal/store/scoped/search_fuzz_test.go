package scoped_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

var searchCorpus = []string{
	"alpha beta gamma",
	"alpha only",
	"beta only",
	`abc" OR "secret`,
	"abc secret",
	"the secret word",
	`say "hello" there`,
	"text:column filter",
	"{text}:braces",
	"NEAR(alpha beta)",
	"star* caret^ minus- back\\slash",
	"MiXeD CaSe Words",
	"naïve café",
	"שלום עולם",
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func FuzzSearchQuery(f *testing.F) {
	for _, seed := range []string{"alpha", "alpha beta", `abc" OR "secret`, `"secret"`, "NEAR(alpha", "text:column", "{text}:braces", "star*", "^caret",
		"secret\x00", `abc"`, `""""`, "MIXED case", "café", "שלום", "alpha AND beta", "alpha OR beta", "-minus", `back\slash`} {
		f.Add(seed)
	}
	s, err := ingest.Open(f.Context(), testOptions(f.TempDir()))
	if err != nil {
		f.Fatalf("Open: %v", err)
	}
	f.Cleanup(func() { _ = s.Close() })
	if err := s.Write(f.Context(), "test.corpus", func(tx *ingest.Tx) error {
		for i, text := range searchCorpus {
			if _, _, err := tx.InsertMessage(message(f, alice, "C"+strconv.Itoa(i), alice, text, epoch)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		f.Fatalf("Write: %v", err)
	}
	g := grantAll(f)
	f.Fuzz(func(t *testing.T, input string) {
		q, err := scoped.ParseQuery(input)
		if err != nil {
			if !errors.Is(err, scoped.ErrInvalidQuery) {
				t.Fatalf("ParseQuery(%q) = %v, want ErrInvalidQuery", input, err)
			}
			return
		}
		terms := strings.Fields(input)
		phrases := strings.Split(q.Expression(), " AND ")
		if len(phrases) != len(terms) {
			t.Fatalf("expression %q for %d terms", q.Expression(), len(terms))
		}
		for i, phrase := range phrases {
			if len(phrase) < 2 || phrase[0] != '"' || phrase[len(phrase)-1] != '"' {
				t.Fatalf("phrase %q of %q is not quoted", phrase, q.Expression())
			}
			if inner := phrase[1 : len(phrase)-1]; strings.Contains(strings.ReplaceAll(inner, `""`, ""), `"`) || strings.ReplaceAll(inner, `""`, `"`) != terms[i] {
				t.Fatalf("phrase %q of %q is not the quoted term %q", phrase, q.Expression(), terms[i])
			}
		}
		page, ok, err := s.Scoped().Search(g, t.Context(), q, "", scoped.SearchPosition{}, scoped.MaxLimit)
		if err != nil || !ok || page.More {
			t.Fatalf("Search(%q) = %+v, %v, %v", q.Expression(), page, ok, err)
		}
		if !isASCII(input) {
			return
		}
		var want []string
		for i := len(searchCorpus) - 1; i >= 0; i-- {
			if !slices.ContainsFunc(terms, func(term string) bool { return !strings.Contains(asciiLower(searchCorpus[i]), asciiLower(term)) }) {
				want = append(want, "C"+strconv.Itoa(i))
			}
		}
		if got := ids(page.Messages); !slices.Equal(got, want) {
			t.Fatalf("Search(%q) found %q, want the rows holding every term: %q", q.Expression(), got, want)
		}
	})
}
