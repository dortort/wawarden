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

func yamlValue(config string, keys ...string) (string, bool) {
	parent, child := -1, 0
	for line := range strings.Lines(config) {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= parent {
			return "", false
		}
		if child < 0 {
			child = indent
		}
		key, value, _ := strings.Cut(text, ":")
		if indent != child || key != keys[0] {
			continue
		}
		if len(keys) == 1 {
			return strings.TrimSpace(value), true
		}
		keys = keys[1:]
		parent, child = indent, -1
	}
	return "", false
}

func TestYAMLValue(t *testing.T) {
	const config = `linters:
  settings:
    exclusions:
      generated: lax
  exclusions:
    # comment
    rules:
      - path: x
        generated: strict
    generated: disable
formatters:
  enable:
    - gofmt
other:
  exclusions:
    generated: lax
`
	for _, tt := range []struct {
		keys  []string
		want  string
		found bool
	}{
		{keys: []string{"linters", "exclusions", "generated"}, want: "disable", found: true},
		{keys: []string{"linters", "settings", "exclusions", "generated"}, want: "lax", found: true},
		{keys: []string{"formatters", "exclusions", "generated"}},
		{keys: []string{"exclusions", "generated"}},
	} {
		if got, found := yamlValue(config, tt.keys...); got != tt.want || found != tt.found {
			t.Errorf("yamlValue(%q) = %q, %v, want %q, %v", tt.keys, got, found, tt.want, tt.found)
		}
	}
}

func TestLintChecksGeneratedFiles(t *testing.T) {
	data, err := fs.ReadFile(os.DirFS(moduleRoot(t)), ".golangci.yml")
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	for _, section := range []string{"linters", "formatters"} {
		if got, _ := yamlValue(string(data), section, "exclusions", "generated"); got != "disable" {
			t.Errorf("%s.exclusions.generated is %q, want disable: otherwise a \"Code generated ... DO NOT EDIT.\" header switches off every check for its file", section, got)
		}
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
