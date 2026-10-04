package archtest

import (
	"regexp"
	"slices"
	"strings"
)

var (
	nolintDirective = regexp.MustCompile(`(?i)^//\s*nolint(?::(\S*))?(?:\s|$)`)
	guardLinters    = set("depguard", "forbidigo")
)

var nolintRule = rule{
	name:  "nolint",
	check: checkNolint,
	cases: []snippet{
		{name: "bare and guard-silencing directives", rel: "internal/app/x_test.go", want: 8, src: `package app

//nolint
var a = 1

var b = 1 //nolint // reason

// nolint
var c = 1

//nolint:all
var d = 1

//nolint:
var e = 1

//nolint:depguard
var f = 1

var g = 1 //nolint:gosec,forbidigo // reason

//NOLINT:DEPGUARD
var h = 1
`},
		{name: "named directives and prose", rel: "internal/app/x.go", src: `package app

//nolint:gosec // reason
var a = 1

var b = 1 //nolint:errcheck,gosec

// nolintable prose, and a //nolint mentioned mid-comment
var c = 1

/* nolint */
var d = 1
`},
	},
}

func checkNolint(f *sourceFile) []string {
	var out []string
	for _, group := range f.file.Comments {
		for _, c := range group.List {
			m := nolintDirective.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}
			linters := strings.Split(strings.ToLower(m[1]), ",")
			if m[1] == "" || slices.ContainsFunc(linters, func(l string) bool { return l == "all" || guardLinters[l] }) {
				out = append(out, f.at(c, "a nolint directive must name its linters and may not silence depguard or forbidigo"))
			}
		}
	}
	return out
}
