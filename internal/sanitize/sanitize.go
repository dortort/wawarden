// Package sanitize removes the characters that let third-party text hide, reorder or rewrite what a reader or a terminal shows.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	tagFirst = 0xE0000
	tagLast  = 0xE007F
)

func Display(text string) string {
	return rewrite(text, func(r rune) (rune, bool) {
		return r, !hidden(r) || r == '\n' || r == '\t'
	})
}

func Terminal(s string) string {
	return rewrite(s, func(r rune) (rune, bool) {
		if hidden(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return utf8.RuneError, true
		}
		return r, true
	})
}

func hidden(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r >= tagFirst && r <= tagLast
}

func rewrite(s string, mapRune func(rune) (rune, bool)) string {
	var b strings.Builder
	copied := false
	for i, r := range s {
		out, keep := mapRune(r)
		valid := r != utf8.RuneError || encodedReplacement(s[i:])
		if keep && out == r && valid {
			if copied {
				b.WriteRune(r)
			}
			continue
		}
		if !copied {
			b.Grow(len(s))
			b.WriteString(s[:i])
			copied = true
		}
		if keep {
			b.WriteRune(out)
		}
	}
	if !copied {
		return s
	}
	return b.String()
}

func encodedReplacement(s string) bool {
	_, size := utf8.DecodeRuneInString(s)
	return size == utf8.RuneLen(utf8.RuneError)
}
