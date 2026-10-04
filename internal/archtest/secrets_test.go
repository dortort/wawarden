package archtest

import (
	"go/ast"
	"maps"
	"slices"
	"strings"
	"testing"
)

const tokenDir = "internal/token"

var variableTimeEquality = map[string]map[string]bool{
	"bytes":   set("Compare", "Equal", "EqualFold"),
	"reflect": set("DeepEqual"),
	"slices":  set("Compare", "CompareFunc", "Equal", "EqualFunc"),
	"strings": set("Compare", "EqualFold"),
}

var credentialDigestFields = set("sum")

var secretComparisonRule = rule{
	name:  "secret-comparisons",
	check: checkSecretComparisons,
	cases: []snippet{
		{name: "the admin digest compared in variable time", rel: "internal/policy/decide.go", want: 9, src: `package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

func DecideAdmin(cred AdminCredential, presented string) (AdminGrant, bool) {
	sum := sha256.Sum256([]byte(presented))
	if sum != cred.sum || !bytes.Equal(sum[:], cred.sum[:]) || string(sum[:]) != string(cred.sum[:]) {
		return AdminGrant{}, false
	}
	if hex.EncodeToString(cred.sum[:]) != hex.EncodeToString(sum[:]) {
		return AdminGrant{}, false
	}
	for i := range cred.sum {
		if sum[i] != cred.sum[i] {
			return AdminGrant{}, false
		}
	}
	stored, _ := cred.sum, &cred.sum
	return AdminGrant{ok: stored == sum}, true
}
`},
		{name: "a partial digest compared through crypto/subtle", rel: "internal/policy/decide.go", want: 4, src: `package policy

import (
	"crypto/sha256"
	"crypto/subtle"
)

func DecideAdmin(cred AdminCredential, presented string) (AdminGrant, bool) {
	sum := sha256.Sum256([]byte(presented))
	for i := 1; i <= len(sum); i++ {
		if subtle.ConstantTimeCompare(sum[:i], cred.sum[:i]) != 1 {
			return AdminGrant{}, false
		}
	}
	ok := subtle.ConstantTimeCompare(sum[:1:1], cred.sum[0:1:1]) == 1 &&
		subtle.ConstantTimeCompare(sum[1:], cred.sum[1:]) == 1 &&
		subtle.ConstantTimeCompare(sum[:], (cred.sum[:len(sum)])) == 1
	return AdminGrant{ok: ok}, true
}
`},
		{name: "variable-time equality helpers in token", rel: "internal/token/x.go", want: 10, src: `package token

import (
	b "bytes"
	"reflect"
	"slices"
	"strings"
)

func f(x, y []byte, s, t string) bool {
	return b.Equal(x, y) || b.EqualFold(x, y) || b.Compare(x, y) == 0 ||
		slices.Equal(x, y) || slices.Compare(x, y) == 0 ||
		slices.EqualFunc(x, y, func(p, q byte) bool { return p == q }) ||
		slices.CompareFunc(x, y, func(p, q byte) int { return int(p) - int(q) }) == 0 ||
		strings.EqualFold(s, t) || strings.Compare(s, t) == 0 || reflect.DeepEqual(x, y)
}
`},
		{name: "another package's ConstantTimeCompare in a policy subpackage", rel: "internal/policy/sub/x.go", want: 1, src: `package sub

import "example.com/subtle"

func f(cred struct{ sum [32]byte }, sum [32]byte) bool {
	return subtle.ConstantTimeCompare(sum[:], cred.sum[:]) == 1
}
`},
		{name: "the digest compared through crypto/subtle", rel: "internal/policy/decide.go", src: `package policy

import (
	"crypto/sha256"
	ct "crypto/subtle"
	"encoding/hex"
	"strings"
)

func DecideAdmin(cred AdminCredential, presented string) (AdminGrant, bool) {
	sum := sha256.Sum256([]byte(presented))
	if ct.ConstantTimeCompare(sum[:], (cred.sum)[:]) != 1 || ct.ConstantTimeCompare(cred.sum[:], cred.sum[:]) != 1 {
		return AdminGrant{}, false
	}
	return AdminGrant{ok: true}, true
}

func credential(sum [32]byte) AdminCredential { return AdminCredential{sum: sum, ok: true} }

func checksum(body, sum string) bool {
	_, ok := strings.CutPrefix(body, "wwadm_")
	return ok && sum == hex.EncodeToString([]byte(body))
}
`},
		{name: "comparisons outside policy and token, and in their tests", rel: "internal/api/x.go", src: `package api

import (
	"bytes"
	"strings"
)

func f(a, b struct{ sum []byte }, scheme string) bool {
	return bytes.Equal(a.sum, b.sum) && strings.EqualFold(scheme, "Bearer")
}
`},
		{name: "a policy test", rel: "internal/policy/x_test.go", src: `package policy

import "bytes"

func f(a, b AdminCredential) bool { return bytes.Equal(a.sum[:], b.sum[:]) && a.sum == b.sum }
`},
	},
}

func checkSecretComparisons(f *sourceFile) []string {
	if f.test || !within(f.dir, policyDir) && !within(f.dir, tokenDir) {
		return nil
	}
	var out []string
	constantTime := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if sel, p := f.ref(n.Fun); sel == nil || p != "crypto/subtle" || sel.Sel.Name != "ConstantTimeCompare" {
				break
			}
			for _, arg := range n.Args {
				arg = ast.Unparen(arg)
				if s, ok := arg.(*ast.SliceExpr); ok && s.Low == nil && s.High == nil && s.Max == nil {
					arg = ast.Unparen(s.X)
				}
				if field, ok := arg.(*ast.SelectorExpr); ok {
					constantTime[field] = true
				}
			}
		case *ast.SelectorExpr:
			if sel, p := f.ref(n); sel != nil {
				if variableTimeEquality[p][sel.Sel.Name] {
					out = append(out, f.at(n, "%s.%s compares in variable time: compare secrets with crypto/subtle.ConstantTimeCompare", p, sel.Sel.Name))
				}
				break
			}
			if credentialDigestFields[n.Sel.Name] && !constantTime[n] {
				out = append(out, f.at(n, "the admin credential's digest is read other than whole, as an argument of crypto/subtle.ConstantTimeCompare, where it could be compared in variable time"))
			}
		}
		return true
	})
	return out
}

func TestCredentialDigestFields(t *testing.T) {
	declared := declarations(t, moduleRoot(t), policyDir)
	if !declared["AdminCredential"] {
		t.Fatal("package policy no longer declares AdminCredential, so the secret-comparisons rule guards no digest: update it")
	}
	fields := map[string]bool{}
	for name := range declared {
		if field, ok := strings.CutPrefix(name, "AdminCredential."); ok && field != "ok" && field != "_" {
			fields[field] = true
		}
	}
	if !maps.Equal(fields, credentialDigestFields) {
		t.Fatalf("AdminCredential declares %q besides its ok flag, but the secret-comparisons rule confines %q: they must match",
			slices.Sorted(maps.Keys(fields)), slices.Sorted(maps.Keys(credentialDigestFields)))
	}
}
