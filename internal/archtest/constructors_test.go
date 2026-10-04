package archtest

import (
	"go/ast"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	chatFile       = "internal/policy/normalize.go"
	grantFile      = "internal/policy/decide.go"
	credentialFile = "internal/policy/admin.go"
)

type sealSite struct{ file, fn string }

var sealFunctions = map[string]sealSite{
	"NewChat":              {chatFile, "Normalize"},
	"NewReadGrant":         {grantFile, "DecideRead"},
	"NewWriteGrant":        {grantFile, "DecideWrite"},
	"NewAdminGrant":        {grantFile, "DecideAdmin"},
	"MatchAdminCredential": {grantFile, "DecideAdmin"},
	"NewAdminCredential":   {credentialFile, "ParseAdminCredential"},
}

var sealRule = rule{
	name:  "seal-constructors",
	check: checkSealConstructors,
	cases: []snippet{
		{name: "the chat constructor outside Normalize", rel: chatFile, want: 3, src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

type thing struct{}

func Normalize(s string) (CanonicalChat, bool) {
	_ = seal.NewReadGrant(s, true, nil)
	return seal.NewChat(s, seal.PhoneChat), true
}

func forge(s string) CanonicalChat { return seal.NewChat(s, seal.LIDChat) }

func (thing) Normalize(s string) CanonicalChat { return seal.NewChat(s, seal.GroupChat) }
`},
		{name: "constructors as values, in function literals and in package variables", rel: grantFile, want: 5, src: `package policy

import (
	"time"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

var mint = seal.NewAdminGrant

var (
	_ = func() WriteGrant { return seal.NewWriteGrant("", nil, true) }
)

func DecideRead(c *Client, now time.Time) (ReadGrant, bool) {
	f := seal.NewReadGrant
	g := func() ReadGrant { return seal.NewReadGrant(c.ID, true, nil) }()
	defer func() { _ = seal.NewReadGrant("", true, nil) }()
	return f(c.ID, g.All(), nil), true
}
`},
		{name: "constructors in functions of the right name in another policy file", rel: "internal/policy/grant.go", want: 2, src: `package policy

import (
	"time"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

func DecideRead(c *Client, now time.Time) (ReadGrant, bool) { return seal.NewReadGrant(c.ID, true, nil), true }

func ParseAdminCredential(s string) (AdminCredential, error) { return seal.NewAdminCredential([32]byte{}), nil }
`},
		{name: "a constructor in a policy test", rel: "internal/policy/x_test.go", want: 1, src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

func Normalize(s string) (CanonicalChat, bool) { return seal.NewChat(s, seal.PhoneChat), true }
`},
		{name: "renamed, dot and blank imports of the seal package", rel: chatFile, want: 3, src: `package policy

import (
	_ "github.com/dortort/wawarden/internal/policy/internal/seal"
	. "github.com/dortort/wawarden/internal/policy/internal/seal"
	s "github.com/dortort/wawarden/internal/policy/internal/seal"
)

func Normalize(jid string) (CanonicalChat, bool) { return s.NewChat(jid, NewChat("", 0).Kind()), true }
`},
		{name: "the seal package imported by a policy subpackage", rel: "internal/policy/sub/x.go", want: 1, src: `package sub

import "github.com/dortort/wawarden/internal/policy/internal/seal"

var _ seal.Chat
`},
		{name: "the seal package imported by package policy's external test", rel: "internal/policy/x_test.go", want: 1, src: `package policy_test

import "github.com/dortort/wawarden/internal/policy/internal/seal"

var _ seal.Chat
`},
		{name: "the seal package imported by another package's test", rel: "internal/api/x_test.go", want: 1, src: `package api

import "github.com/dortort/wawarden/internal/policy/internal/seal"

var _ seal.Chat
`},
		{name: "the chat constructor called in Normalize", rel: chatFile, src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

func Normalize(s string) (CanonicalChat, bool) {
	if s == "" {
		return CanonicalChat{}, false
	}
	if s == "g" {
		return (seal.NewChat)(s, seal.GroupChat), true
	}
	return seal.NewChat(s+"@lid", seal.LIDChat), true
}
`},
		{name: "the grant constructors called in their Decide functions", rel: grantFile, src: `package policy

import (
	"crypto/sha256"
	"time"

	"github.com/dortort/wawarden/internal/policy/internal/seal"
)

func DecideRead(c *Client, now time.Time) (ReadGrant, bool) {
	return seal.NewReadGrant(c.ID, c.ReadAll, nil), true
}

func DecideWrite(c *Client, now time.Time) (WriteGrant, bool) {
	return seal.NewWriteGrant(c.ID, nil, c.AllowFirstContact), true
}

func DecideAdmin(cred AdminCredential, presented string) (AdminGrant, bool) {
	if !seal.MatchAdminCredential(cred, sha256.Sum256([]byte(presented))) {
		return AdminGrant{}, false
	}
	return seal.NewAdminGrant(), true
}
`},
		{name: "the credential constructor called in ParseAdminCredential", rel: credentialFile, src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

func ParseAdminCredential(s string) (AdminCredential, error) {
	var sum [32]byte
	return seal.NewAdminCredential(sum), nil
}
`},
		{name: "types and constants of the seal package elsewhere in policy", rel: "internal/policy/policy.go", src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

type CanonicalChat = seal.Chat

const InvalidChat = seal.InvalidChat

var _ seal.ReadGrant

func f(c seal.Chat) seal.ChatKind { return c.Kind() }
`},
		{name: "the seal package itself", rel: sealDir + "/x_test.go", src: `package seal_test

import s "github.com/dortort/wawarden/internal/policy/internal/seal"

var forge = s.NewChat
`},
		{name: "another package named seal", rel: "internal/policy/x.go", src: `package policy

import "example.com/seal"

var forge = seal.NewChat
`},
	},
}

func checkSealConstructors(f *sourceFile) []string {
	if f.dir == sealDir {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if imp.path != sealPath {
			continue
		}
		if f.dir != policyDir || f.file.Name.Name != "policy" {
			out = append(out, f.at(imp.node, "only package policy may import %q, whose constructors build valid chats and grants", sealPath))
		}
		if imp.node.Name != nil {
			out = append(out, f.at(imp.node, "the seal package imported as %s: import it under its own name, so every reference to a constructor is a selector this rule reads", imp.name))
		}
	}
	called := map[*ast.SelectorExpr]bool{}
	for _, decl := range f.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.CallExpr:
				if sel, p := f.ref(n.Fun); p == sealPath && sealFunctions[sel.Sel.Name] == (sealSite{f.rel, fn.Name.Name}) {
					called[sel] = true
				}
			}
			return true
		})
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if s, p := f.ref(sel); s == nil || p != sealPath {
			return true
		}
		if site, ok := sealFunctions[sel.Sel.Name]; ok && !called[sel] {
			out = append(out, f.at(sel, "seal.%s may only be called directly in the body of %s in %s, outside any function literal: anywhere else, or as a value, it would mint what only that function may", sel.Sel.Name, site.fn, site.file))
		}
		return true
	})
	return out
}

func TestSealFunctionsHaveOneSite(t *testing.T) {
	files, err := moduleFiles(os.DirFS(filepath.Join(moduleRoot(t), sealDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", sealDir, err)
	}
	exported := map[string]bool{}
	for _, f := range files {
		if f.test || f.dir != "." {
			continue
		}
		for _, decl := range f.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					exported[d.Name.Name] = true
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, id := range spec.(*ast.ValueSpec).Names {
						if id.IsExported() {
							exported[id.Name] = true
						}
					}
				}
			}
		}
	}
	got, want := slices.Sorted(maps.Keys(exported)), slices.Sorted(maps.Keys(sealFunctions))
	if !slices.Equal(got, want) {
		t.Fatalf("package seal exports the functions and variables %q, but the seal-constructors rule confines %q: they must match", got, want)
	}
}
