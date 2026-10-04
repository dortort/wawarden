package archtest

import (
	"go/ast"
	"go/token"
	"net"
	"net/netip"
	"strconv"
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
	"strconv"
)

var (
	_ = "0.0.0.0:0"
	_ = "[::]:8080"
	_ = "WAWARDEN_LISTEN=0.0.0.0:9"
	_ = "::"
	_ = "0.0.0.0"
	_ = "[::]"
	_ = "::ffff:0.0.0.0"
	_ = "0.0.0." + "0:0"
	_ = "[::]" + (":" + "8080")
	_ = "0.0.0.0:" + strconv.Itoa(8080)
	_ = netip.IPv4Unspecified()
	_ = netip.IPv6Unspecified
	_ = net.IPv4zero
	_ = net.IPv6unspecified
	_ = net.IPv4(0, 0, 0, 0x0)
	_ = netip.AddrPortFrom(netip.AddrFrom4([4]byte{}), 0)
	_ = netip.AddrFrom16([16]byte{0, 0})
	_ = netip.AddrFromSlice([]byte{0, 0, 0, 0})
	_ = netip.AddrFromSlice(net.IP{0, 0, 0, 0})
	_ = net.IP([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
)
`

var wildcardRule = rule{
	name:  "test-wildcard-addresses",
	check: wildcardAddresses(wildcardAllowances),
	cases: []snippet{
		{name: "wildcard addresses in a test", rel: "internal/app/x_test.go", want: 20, src: "package app\n" + everyWildcard},
		{name: "loopback and specific addresses in a test", rel: "internal/app/x_test.go", src: `package app

import (
	"net"
	"net/netip"
)

var (
	_ = "127.0.0.1:0"
	_ = "[::1]:0"
	_ = "localhost:0"
	_ = "192.0.2.1:80"
	_ = "fe80::1"
	_ = "a::b"
	_ = "std::string"
	_ = "version 0.0.0"
	_ = "0" + ":" + "0"
	_ = "127.0.0." + "1:0"
	_ = "0.0.0." + "1"
	_ = netip.IPv6Loopback()
	_ = net.IPv4(127, 0, 0, 1)
	_ = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	_ = netip.AddrFrom16([16]byte{15: 1})
	_ = netip.AddrFromSlice([]byte{})
	_ = net.IP{0, 0, 0}
	_ = [4]byte{}
	_ = make(net.IP, 4)
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
		literalRuns(f.file, func(at ast.Node, s string) {
			if unspecifiedAddress(s) {
				found = append(found, f.at(at, "a string literal names a wildcard address: tests listen on loopback only"))
			}
		})
		ast.Inspect(f.file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if s, p := f.ref(sel); s != nil && unspecifiedValues[p][s.Sel.Name] {
					found = append(found, f.at(sel, "%s.%s is a wildcard address: tests listen on loopback only", p, s.Sel.Name))
				}
			}
			if f.zeroAddress(n) {
				found = append(found, f.at(n, "an address built from zero bytes is a wildcard address: tests listen on loopback only"))
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
		if host, _, err := net.SplitHostPort(tok); err == nil {
			tok = host
		}
		tok = strings.TrimSuffix(strings.TrimPrefix(tok, "["), "]")
		if a, err := netip.ParseAddr(tok); err == nil && a.Unmap().IsUnspecified() {
			return true
		}
	}
	return false
}

func (f *sourceFile) zeroAddress(n ast.Node) bool {
	ipBytes := func(e ast.Expr) bool {
		lit, ok := ast.Unparen(e).(*ast.CompositeLit)
		return ok && (len(lit.Elts) == net.IPv4len || len(lit.Elts) == net.IPv6len) && zeroInts(lit.Elts)
	}
	switch n := n.(type) {
	case *ast.CompositeLit:
		return f.isType(n.Type, "net", "IP") && ipBytes(n)
	case *ast.CallExpr:
		sel, p := f.ref(n.Fun)
		if sel == nil || len(n.Args) == 0 {
			return false
		}
		arg, _ := ast.Unparen(n.Args[0]).(*ast.CompositeLit)
		switch p + "." + sel.Sel.Name {
		case "net.IPv4":
			return len(n.Args) == net.IPv4len && zeroInts(n.Args)
		case "net/netip.AddrFrom4", "net/netip.AddrFrom16":
			return arg != nil && zeroInts(arg.Elts)
		case "net/netip.AddrFromSlice", "net.IP":
			return arg != nil && !f.isType(arg.Type, "net", "IP") && ipBytes(arg)
		}
	}
	return false
}

func zeroInts(es []ast.Expr) bool {
	for _, e := range es {
		v, ok := ast.Unparen(e).(*ast.BasicLit)
		if !ok || v.Kind != token.INT {
			return false
		}
		if n, err := strconv.ParseUint(v.Value, 0, 64); err != nil || n != 0 {
			return false
		}
	}
	return true
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
