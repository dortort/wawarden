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

const (
	maxOutboundRunes = 4096
	maxOutboundBytes = 8192
)

func Outbound(text string) bool {
	if text == "" || len(text) > maxOutboundBytes || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxOutboundRunes {
		return false
	}
	for _, r := range text {
		if refusedOutbound(r) {
			return false
		}
	}
	return true
}

func refusedOutbound(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r >= tagFirst && r <= tagLast:
		return true
	}
	switch r {
	case 0x061c, 0x200e, 0x200f, 0x2028, 0x2029, 0xfeff:
		return true
	}
	return false
}
