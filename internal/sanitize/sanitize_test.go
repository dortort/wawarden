package sanitize

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

const (
	esc  = "\x1b"
	bel  = "\x07"
	csi1 = "\u009b"
	osc1 = "\u009d"
	st   = esc + `\`
	rlo  = "\u202e"
	pdf  = "\u202c"
	zwj  = "\u200d"
	ff   = "\ufffd"
)

func TestDisplay(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "plain text", in: "Synthetic message, nothing to remove.", want: "Synthetic message, nothing to remove."},
		{name: "newline and tab kept", in: "line one\nline two\tcolumn", want: "line one\nline two\tcolumn"},
		{name: "carriage return", in: "line one\r\nline two\roverwritten", want: "line one\nline twooverwritten"},
		{name: "NUL, BEL, backspace, form feed and DEL", in: "a\x00b" + bel + "c\bd\fe\x7ff", want: "abcdef"},
		{name: "CSI colour sequence", in: esc + "[31mred" + esc + "[0m", want: "[31mred[0m"},
		{name: "CSI that clears the screen", in: esc + "[2J" + esc + "[Hhome", want: "[2J[Hhome"},
		{name: "C1 CSI", in: csi1 + "31mred", want: "31mred"},
		{name: "OSC 8 hyperlink", in: esc + "]8;;https://example.test/" + st + "click" + esc + "]8;;" + st, want: "]8;;https://example.test/\\click]8;;\\"},
		{name: "OSC 52 clipboard write ended by BEL", in: esc + "]52;c;c3ludGhldGlj" + bel + "after", want: "]52;c;c3ludGhldGljafter"},
		{name: "C1 OSC ended by C1 ST", in: osc1 + "52;c;c3ludGhldGlj\u009cafter", want: "52;c;c3ludGhldGljafter"},
		{name: "every C1 control", in: "a\u0080\u0085\u008d\u009fb", want: "ab"},
		{name: "right-to-left override", in: "invoice_" + rlo + "fdp.exe" + pdf, want: "invoice_fdp.exe"},
		{name: "embeddings and overrides", in: "\u202aa\u202bb\u202cc\u202dd\u202ee", want: "abcde"},
		{name: "isolates", in: "\u2066a\u2067b\u2068c\u2069", want: "abc"},
		{name: "directional marks", in: "a\u200eb\u200fc\u061cd", want: "abcd"},
		{name: "zero-width characters", in: "pay\u200bpal\u200cand\u2060joined", want: "paypalandjoined"},
		{name: "byte-order mark", in: "\ufeffstart and\ufeffmiddle", want: "start andmiddle"},
		{name: "other format characters", in: "soft\u00adhyphen\u2061\u180e\ufff9x", want: "softhyphenx"},
		{name: "tag characters", in: "visible\U000E0001\U000E0069\U000E0067\U000E006E\U000E007F", want: "visible"},
		{name: "unassigned tag code points", in: "a\U000E0000b\U000E0002c\U000E001Fd", want: "abcd"},
		{name: "emoji ZWJ sequence", in: "\U0001F468" + zwj + "\U0001F469" + zwj + "\U0001F467", want: "\U0001F468\U0001F469\U0001F467"},
		{name: "emoji keycap, skin tone and flag", in: "1\ufe0f\u20e3 \U0001F44D\U0001F3FD \U0001F1EB\U0001F1F7", want: "1\ufe0f\u20e3 \U0001F44D\U0001F3FD \U0001F1EB\U0001F1F7"},
		{name: "emoji subdivision flag", in: "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F", want: "\U0001F3F4"},
		{name: "line and paragraph separators kept", in: "a\u2028b\u2029c", want: "a\u2028b\u2029c"},
		{name: "invalid UTF-8", in: "a\xffb\xe2\x80c", want: "a" + ff + "b" + ff + ff + "c"},
		{name: "encoded replacement character kept", in: "a" + ff + "b", want: "a" + ff + "b"},
		{name: "surrogate half encoded in UTF-8", in: "a\xed\xa0\x80b", want: "a" + ff + ff + ff + "b"},
		{name: "empty", in: "", want: ""},
		{name: "only removable characters", in: esc + "\u200b" + rlo + "\U000E0041", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Display(tt.in); got != tt.want {
				t.Fatalf("Display(%+q) = %+q, want %+q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTerminal(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "plain text", in: "status: connected", want: "status: connected"},
		{name: "newline, tab and carriage return", in: "a\nb\tc\rd", want: "a" + ff + "b" + ff + "c" + ff + "d"},
		{name: "CSI colour sequence", in: esc + "[31mred" + esc + "[0m", want: ff + "[31mred" + ff + "[0m"},
		{name: "C1 CSI", in: csi1 + "2J", want: ff + "2J"},
		{name: "OSC 8 hyperlink", in: esc + "]8;;https://example.test/" + st + "click" + esc + "]8;;" + st, want: ff + "]8;;https://example.test/" + ff + "\\click" + ff + "]8;;" + ff + "\\"},
		{name: "OSC 52 clipboard write ended by BEL", in: esc + "]52;c;c3ludGhldGlj" + bel, want: ff + "]52;c;c3ludGhldGlj" + ff},
		{name: "C1 OSC ended by C1 ST", in: osc1 + "52;c;eA==\u009c", want: ff + "52;c;eA==" + ff},
		{name: "DEL and NUL", in: "a\x7fb\x00c", want: "a" + ff + "b" + ff + "c"},
		{name: "right-to-left override", in: "name" + rlo + "txt.exe", want: "name" + ff + "txt.exe"},
		{name: "isolates and marks", in: "\u2066a\u2069\u200e\u061c", want: ff + "a" + ff + ff + ff},
		{name: "zero-width characters and BOM", in: "\ufeffa\u200bb\u200cc\u2060", want: ff + "a" + ff + "b" + ff + "c" + ff},
		{name: "tag characters", in: "x\U000E0001\U000E0041\U000E0000", want: "x" + ff + ff + ff},
		{name: "line and paragraph separators", in: "a\u2028b\u2029c", want: "a" + ff + "b" + ff + "c"},
		{name: "emoji ZWJ sequence", in: "\U0001F468" + zwj + "\U0001F469", want: "\U0001F468" + ff + "\U0001F469"},
		{name: "emoji keycap and flag", in: "1\ufe0f\u20e3 \U0001F1EB\U0001F1F7", want: "1\ufe0f\u20e3 \U0001F1EB\U0001F1F7"},
		{name: "invalid UTF-8", in: "a\xffb\xc3", want: "a" + ff + "b" + ff},
		{name: "encoded replacement character kept", in: ff, want: ff},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Terminal(tt.in); got != tt.want {
				t.Fatalf("Terminal(%+q) = %+q, want %+q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanTextIsReturnedWithoutCopying(t *testing.T) {
	tests := []struct {
		name  string
		f     func(string) string
		clean string
	}{
		{name: "Display", f: Display, clean: strings.Repeat("Synthetic text, emoji \U0001F44D\U0001F3FD 1\ufe0f\u20e3, a tab\tand a line\n", 8)},
		{name: "Terminal", f: Terminal, clean: strings.Repeat("Synthetic text, emoji \U0001F44D\U0001F3FD 1\ufe0f\u20e3. ", 8)},
	}
	for _, tt := range tests {
		if got := tt.f(tt.clean); got != tt.clean {
			t.Fatalf("%s changed clean text to %+q", tt.name, got)
		}
		if allocs := testing.AllocsPerRun(100, func() { _ = tt.f(tt.clean) }); allocs != 0 {
			t.Fatalf("%s allocated %v times for text it leaves unchanged", tt.name, allocs)
		}
	}
}

func forbiddenInDisplay(r rune) bool {
	return r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r >= 0xE0000 && r <= 0xE007F)
}

func forbiddenInTerminal(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r >= 0xE0000 && r <= 0xE007F
}

func FuzzSanitize(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain",
		"line\nwith\ttab\r",
		esc + "[31mred" + esc + "[0m",
		esc + "]8;;https://example.test/" + st + "click" + esc + "]8;;" + st,
		esc + "]52;c;c3ludGhldGlj" + bel,
		csi1 + osc1 + "\u009c",
		rlo + "txt.exe" + pdf,
		"\U0001F468" + zwj + "\U0001F469" + zwj + "\U0001F467",
		"\U0001F3F4\U000E0067\U000E0062\U000E007F",
		"\ufeff\u200b\u2028\u2029",
		"\xff\xe2\x80" + ff,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		check := func(name string, out string, forbidden func(rune) bool) {
			if !utf8.ValidString(out) {
				t.Fatalf("%s(%+q) = %+q, not valid UTF-8", name, s, out)
			}
			for _, r := range out {
				if forbidden(r) {
					t.Fatalf("%s(%+q) = %+q, which holds %U", name, s, out, r)
				}
			}
		}
		display := Display(s)
		check("Display", display, forbiddenInDisplay)
		if again := Display(display); again != display {
			t.Fatalf("Display is not idempotent: %+q, then %+q", display, again)
		}
		if utf8.RuneCountInString(display) > utf8.RuneCountInString(s) {
			t.Fatalf("Display(%+q) = %+q, longer than its input", s, display)
		}
		terminal := Terminal(s)
		check("Terminal", terminal, forbiddenInTerminal)
		if again := Terminal(terminal); again != terminal {
			t.Fatalf("Terminal is not idempotent: %+q, then %+q", terminal, again)
		}
		if got, want := utf8.RuneCountInString(terminal), utf8.RuneCountInString(s); got != want {
			t.Fatalf("Terminal(%+q) = %+q holds %d runes, want one for each of the input's %d", s, terminal, got, want)
		}
		if utf8.ValidString(s) && !strings.ContainsFunc(s, forbiddenInDisplay) && display != s {
			t.Fatalf("Display(%+q) = %+q changed text that holds nothing to remove", s, display)
		}
		if utf8.ValidString(s) && !strings.ContainsFunc(s, forbiddenInTerminal) && terminal != s {
			t.Fatalf("Terminal(%+q) = %+q changed text that holds nothing to replace", s, terminal)
		}
	})
}
