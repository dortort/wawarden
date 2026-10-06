package scoped

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	minQueryBytes = 3
	maxQueryBytes = 128
	maxTerms      = 8
	minTermRunes  = 3
)

var ErrInvalidQuery = errors.New("scoped: a search query is 3 to 128 bytes of UTF-8 text without control characters, in 1 to 8 terms of at least 3 characters each")

type Query struct {
	match string
}

func ParseQuery(s string) (Query, error) {
	if len(s) > maxQueryBytes || !utf8.ValidString(s) {
		return Query{}, ErrInvalidQuery
	}
	s = norm.NFC.String(s)
	if len(s) < minQueryBytes || len(s) > maxQueryBytes || strings.ContainsFunc(s, unicode.IsControl) {
		return Query{}, ErrInvalidQuery
	}
	terms := strings.Fields(s)
	if len(terms) == 0 || len(terms) > maxTerms {
		return Query{}, ErrInvalidQuery
	}
	phrases := make([]string, len(terms))
	for i, term := range terms {
		if utf8.RuneCountInString(term) < minTermRunes {
			return Query{}, ErrInvalidQuery
		}
		phrases[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return Query{match: strings.Join(phrases, " AND ")}, nil
}

func (q Query) Expression() string { return q.match }
