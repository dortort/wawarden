package archtest

import (
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func unscopedForbidigoPatterns(t *testing.T, config string) []*regexp.Regexp {
	t.Helper()
	var out []*regexp.Regexp
	lines := strings.Split(config, "\n")
	for i, line := range lines {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		expr, ok := strings.CutPrefix(line[indent:], "- pattern: ")
		if !ok {
			continue
		}
		scoped := false
		for _, field := range lines[i+1:] {
			if len(field)-len(strings.TrimLeft(field, " ")) <= indent {
				break
			}
			scoped = scoped || strings.HasPrefix(strings.TrimSpace(field), "pkg:")
		}
		if scoped {
			continue
		}
		re, err := regexp.Compile(strings.TrimSpace(expr))
		if err != nil {
			t.Fatalf("forbidigo pattern %q: %v", expr, err)
		}
		out = append(out, re)
	}
	return out
}

func TestUnscopedForbidigoPatterns(t *testing.T) {
	const config = `    forbidigo:
      forbid:
        - pattern: ^http\.Get$
          pkg: ^net/http$
          msg: scoped
        - pattern: \.Run$
          msg: unscoped
        - pattern: ^os\.Exit$
`
	var got []string
	for _, re := range unscopedForbidigoPatterns(t, config) {
		got = append(got, re.String())
	}
	if want := []string{`\.Run$`, `^os\.Exit$`}; !slices.Equal(got, want) {
		t.Fatalf("unscoped patterns %q, want %q", got, want)
	}
}

func TestLintBansListenAndServeOnAnyReceiver(t *testing.T) {
	data, err := fs.ReadFile(os.DirFS(moduleRoot(t)), ".golangci.yml")
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	patterns := unscopedForbidigoPatterns(t, string(data))
	for _, text := range []string{
		"http.ListenAndServe",
		"http.ListenAndServeTLS",
		"http.Server.ListenAndServe",
		"http.Server.ListenAndServeTLS",
		"app.embedded.ListenAndServe",
		"app.embedded.ListenAndServeTLS",
	} {
		if !slices.ContainsFunc(patterns, func(re *regexp.Regexp) bool { return re.MatchString(text) }) {
			t.Errorf("no forbidigo pattern without a pkg constraint matches %q, so lint does not ban ListenAndServe on every receiver, including one promoted through embedding", text)
		}
	}
}
