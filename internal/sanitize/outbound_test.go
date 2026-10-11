package sanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

var refusedOutboundRanges = [][2]rune{
	{0x00, 0x08}, {0x0b, 0x1f}, {0x7f, 0x9f}, {0x061c, 0x061c}, {0x200e, 0x200f},
	{0x2028, 0x202e}, {0x2066, 0x2069}, {0xfeff, 0xfeff}, {0xe0000, 0xe007f},
}

func outboundModel(s string) bool {
	if !utf8.ValidString(s) || len(s) == 0 || len(s) > 8192 || utf8.RuneCountInString(s) > 4096 {
		return false
	}
	for _, r := range s {
		for _, span := range refusedOutboundRanges {
			if r >= span[0] && r <= span[1] {
				return false
			}
		}
	}
	return true
}

func TestOutbound(t *testing.T) {
	euro := "\u20ac"
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"plain text", "hello", true},
		{"a newline and a tab", "line one\n\tline two", true},
		{"a single character", "x", true},
		{"whitespace only", "   ", true},
		{"zero-width joiner", "\U0001F468" + zwj + "\U0001F469", true},
		{"zero-width non-joiner", "a\u200cb", true},
		{"variation selector 16", "\u2764\ufe0f", true},
		{"a flag", "\U0001F1EB\U0001F1F7", true},
		{"zero-width space", "a\u200bb", true},
		{"soft hyphen", "a\u00adb", true},
		{"word joiner", "a\u2060b", true},
		{"the encoded replacement character", "a" + ff, true},
		{"4096 runes", strings.Repeat("a", 4096), true},
		{"4097 runes", strings.Repeat("a", 4097), false},
		{"8192 bytes", strings.Repeat(euro, 2730) + "ab", true},
		{"8193 bytes", strings.Repeat(euro, 2731), false},
		{"4096 two-byte runes", strings.Repeat("\u00e9", 4096), true},
		{"empty", "", false},
		{"invalid UTF-8", "a\xffb", false},
		{"a truncated sequence", "a\xe2\x82", false},
		{"NUL", "a\x00b", false},
		{"a carriage return", "a\rb", false},
		{"a vertical tab", "a\vb", false},
		{"escape", esc + "[31m", false},
		{"bell", bel, false},
		{"DEL", "a\x7fb", false},
		{"a C1 control", csi1, false},
		{"the last C1 control", "a\u009fb", false},
		{"no-break space after C1", "a\u00a0b", true},
		{"arabic letter mark", "a\u061cb", false},
		{"left-to-right mark", "a\u200eb", false},
		{"right-to-left mark", "a\u200fb", false},
		{"line separator", "a\u2028b", false},
		{"paragraph separator", "a\u2029b", false},
		{"left-to-right embedding", "a\u202ab", false},
		{"pop directional formatting", "a" + pdf + "b", false},
		{"right-to-left override", rlo + "txt.exe", false},
		{"left-to-right isolate", "a\u2066b", false},
		{"pop directional isolate", "a\u2069b", false},
		{"byte order mark", "\ufeffhello", false},
		{"a tag character", "\U0001F3F4\U000E0067\U000E007F", false},
		{"the first tag character", "a\U000E0000", false},
		{"past the tag block", "a\U000E0080", true},
	}
	for _, tt := range tests {
		if got := Outbound(tt.text); got != tt.want {
			t.Errorf("%s: Outbound = %v, want %v", tt.name, got, tt.want)
		}
		if got := outboundModel(tt.text); got != tt.want {
			t.Errorf("%s: the model says %v, want %v", tt.name, got, tt.want)
		}
	}
}

func FuzzOutboundText(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "line\nwith\ttab", "a\rb", esc + "[0m", csi1, rlo + "x" + pdf, "\u2066x\u2069", "\ufeff",
		"\U0001F468" + zwj + "\U0001F469", "\u2764\ufe0f", "\U0001F3F4\U000E0067\U000E007F", "\xff", strings.Repeat("a", 4097),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Outbound(s)
		if got != outboundModel(s) {
			t.Fatalf("Outbound(%+q) = %v, the model says %v", s, got, !got)
		}
		if got && (len(s) > 8192 || utf8.RuneCountInString(s) > 4096 || s == "") {
			t.Fatalf("Outbound(%+q) accepted text outside the bounds", s)
		}
	})
}
