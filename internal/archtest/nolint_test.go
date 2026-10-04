package archtest

import (
	"regexp"
	"slices"
	"strings"
)

var (
	nolintDirective = regexp.MustCompile(`^nolint( |:|$)`)
	silencedLinters = set("", "all", "depguard", "forbidigo")
)

var nolintRule = rule{
	name:  "nolint",
	check: checkNolint,
	cases: []snippet{
		{name: "bare and guard-silencing directives", rel: "internal/app/x_test.go", want: 14, src: `package app

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

//nolint:gosec, depguard
var i = 1

//nolint:gosec ,depguard
var j = 1

var k = 1 //nolint:gosec,forbidigo// reason

///nolint
var l = 1

// //nolint:depguard
var m = 1

//nolint:allx
var n = 1
`},
		{name: "named directives and prose", rel: "internal/app/x.go", src: `package app

//nolint:gosec // reason
var a = 1

var b = 1 //nolint:errcheck,gosec

// nolintable prose, and a //nolint mentioned mid-comment
var c = 1

/* nolint */
var d = 1

// nolint:gosec, errcheck // reason
var e = 1

var f = 1 //nolint:gosec// depguard and forbidigo are named only in the reason
`},
	},
}

func checkNolint(f *sourceFile) []string {
	var out []string
	for _, group := range f.file.Comments {
		for _, c := range group.List {
			text := strings.ToLower(strings.TrimLeft(c.Text, "/ "))
			if !nolintDirective.MatchString(text) {
				continue
			}
			names, named := strings.CutPrefix(text, "nolint:")
			names, _, _ = strings.Cut(names, "//")
			if !named || strings.HasPrefix(names, "all") || slices.ContainsFunc(strings.Split(names, ","), func(l string) bool { return silencedLinters[strings.TrimSpace(l)] }) {
				out = append(out, f.at(c, "a nolint directive must name its linters and may not silence depguard or forbidigo"))
			}
		}
	}
	return out
}
