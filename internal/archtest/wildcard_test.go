package archtest

import (
	"go/ast"
	"net/netip"
	"strings"
	"testing"
	"unicode"
)

type wildcardAllowance struct {
	count  int
	reason string
}

const archtestDir = "internal/archtest"

var wildcardAllowances = map[string]wildcardAllowance{
	"cmd/wawarden/run_test.go":          {count: 1, reason: "a health address that serve refuses before it binds anything"},
	"internal/app/app_test.go":          {count: 2, reason: "binds 0.0.0.0 on an ephemeral port to prove a non-loopback client or admin listener logs a warning; no other non-loopback address is bindable on every host"},
	"internal/config/config_test.go":    {count: 12, reason: "addresses the validator accepts or refuses; package config never binds"},
	"internal/listeners/family_test.go": {count: 3, reason: "binds 0.0.0.0 on an ephemeral port to prove an IPv4 wildcard listener takes IPv4 only, which only a wildcard bind can show"},
}

var unspecifiedValues = map[string]map[string]bool{
	"net":       set("IPv4zero", "IPv6zero", "IPv6unspecified"),
	"net/netip": set("IPv4Unspecified", "IPv6Unspecified"),
}

const everyWildcard = `
import (
	"net"
	"net/netip"
)

var (
	_ = "0.0.0.0:0"
	_ = "[::]:8080"
	_ = "WAWARDEN_LISTEN=0.0.0.0:9"
	_ = "::"
	_ = "0.0.0.0"
	_ = netip.IPv4Unspecified()
	_ = netip.IPv6Unspecified
	_ = net.IPv4zero
	_ = net.IPv6unspecified
)
`

var wildcardRule = rule{
	name:  "test-wildcard-addresses",
	check: wildcardAddresses(wildcardAllowances),
	cases: []snippet{
		{name: "wildcard addresses in a test", rel: "internal/app/x_test.go", want: 9, src: "package app\n" + everyWildcard},
		{name: "loopback and specific addresses in a test", rel: "internal/app/x_test.go", src: `package app

import "net/netip"

var (
	_ = "127.0.0.1:0"
	_ = "[::1]:0"
	_ = "localhost:0"
	_ = "192.0.2.1:80"
	_ = "fe80::1"
	_ = "a::b"
	_ = "std::string"
	_ = netip.IPv6Loopback()
)
`},
		{name: "wildcard addresses outside tests", rel: "internal/config/x.go", src: "package config\n" + everyWildcard},
		{name: "the architecture tests' own snippets", rel: archtestDir + "/x_test.go", src: "package archtest\n" + everyWildcard},
	},
}

func wildcardAddresses(allowed map[string]wildcardAllowance) func(*sourceFile) []string {
	return func(f *sourceFile) []string {
		if !f.test || f.dir == archtestDir {
			return nil
		}
		var found []string
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if s, ok := stringLit(n); ok && unspecifiedAddress(s) {
					found = append(found, f.at(n, "a string literal names a wildcard address: tests listen on loopback only"))
				}
			case *ast.SelectorExpr:
				if sel, p := f.ref(n); sel != nil && unspecifiedValues[p][sel.Sel.Name] {
					found = append(found, f.at(n, "%s.%s is a wildcard address: tests listen on loopback only", p, sel.Sel.Name))
				}
			}
			return true
		})
		a, listed := allowed[f.rel]
		switch {
		case !listed:
			return found
		case len(found) != a.count:
			return []string{f.at(f.file, "%d wildcard addresses, the allow-list expects %d (%s): %q", len(found), a.count, a.reason, found)}
		}
		return nil
	}
}

func unspecifiedAddress(s string) bool {
	tokens := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(".:[]%", r)
	})
	for _, tok := range tokens {
		if ap, err := netip.ParseAddrPort(tok); err == nil && ap.Addr().IsUnspecified() {
			return true
		}
		if a, err := netip.ParseAddr(tok); err == nil && a.IsUnspecified() {
			return true
		}
	}
	return false
}

func TestWildcardAllowance(t *testing.T) {
	check := wildcardAddresses(map[string]wildcardAllowance{"internal/app/x_test.go": {count: 2, reason: "synthetic"}})
	for _, tt := range []struct {
		name string
		rel  string
		src  string
		want int
	}{
		{name: "listed with the expected count", rel: "internal/app/x_test.go", src: `"0.0.0.0:0", "[::]:0"`},
		{name: "listed with one more", rel: "internal/app/x_test.go", src: `"0.0.0.0:0", "[::]:0", "0.0.0.0:1"`, want: 1},
		{name: "listed with one fewer", rel: "internal/app/x_test.go", src: `"0.0.0.0:0"`, want: 1},
		{name: "not listed", rel: "internal/app/y_test.go", src: `"0.0.0.0:0", "[::]:0"`, want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseSource(tt.rel, []byte("package app\n\nvar _ = []string{"+tt.src+"}\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := check(f); len(got) != tt.want {
				t.Fatalf("%d findings, want %d: %q", len(got), tt.want, got)
			}
		})
	}
}
