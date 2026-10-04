package archtest

import "go/ast"

var chatRenderingMethods = set("String", "GoString", "Format", "MarshalText", "MarshalJSON", "MarshalBinary", "LogValue", "Error")

var chatMethodRule = rule{
	name:  "chat-methods",
	check: checkChatMethods,
	cases: []snippet{
		{name: "rendering methods on the chat type", rel: chatFile, want: 8, src: `package policy

import (
	"fmt"
	"log/slog"
)

func (c CanonicalChat) String() string                  { return c.jid }
func (c *CanonicalChat) GoString() string               { return c.jid }
func (CanonicalChat) Format(fmt.State, rune)            {}
func (c (CanonicalChat)) MarshalText() ([]byte, error)  { return nil, nil }
func (c *(CanonicalChat)) MarshalJSON() ([]byte, error) { return nil, nil }
func (CanonicalChat) MarshalBinary() ([]byte, error)    { return nil, nil }
func (c CanonicalChat) LogValue() slog.Value            { return slog.StringValue(c.jid) }
func (c CanonicalChat) Error() string                   { return c.jid }
`},
		{name: "a rendering method declared in a policy test", rel: "internal/policy/x_test.go", want: 1, src: `package policy

func (c CanonicalChat) String() string { return c.jid }
`},
		{name: "aliases of the chat type in a policy test", rel: "internal/policy/x_test.go", want: 3, src: `package policy

type cc = CanonicalChat

type (
	pc = *CanonicalChat
	nc = (CanonicalChat)
)

func (c cc) String() string   { return "" }
func (p pc) GoString() string { return "" }
`},
		{name: "an alias of the chat type in a non-test policy file", rel: "internal/policy/grant.go", want: 1, src: `package policy

type cc = CanonicalChat
`},
		{name: "defined types and other aliases in a policy test", rel: "internal/policy/x_test.go", src: `package policy

type dc CanonicalChat

type (
	rg = ReadGrant
	s  = string
)

func (d dc) String() string { return "" }
`},
		{name: "an alias of the chat type in another package's test", rel: "internal/api/x_test.go", src: `package api

import "github.com/dortort/wawarden/internal/policy"

type cc = policy.CanonicalChat
`},
		{name: "accessors, other types' methods and functions", rel: chatFile, src: `package policy

type ChatKind uint8

func (k ChatKind) String() string      { return "" }
func (c CanonicalChat) JID() string    { return c.jid }
func (c CanonicalChat) Kind() ChatKind { return 0 }
func (c CanonicalChat) Valid() bool    { return c.ok }
func String(c CanonicalChat) string    { return "" }
`},
		{name: "another package's chat type", rel: "internal/api/x.go", src: `package api

type CanonicalChat struct{}

func (CanonicalChat) String() string { return "" }
`},
	},
}

func checkChatMethods(f *sourceFile) []string {
	var out []string
	for _, decl := range f.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 && chatRenderingMethods[d.Name.Name] && f.isType(d.Recv.List[0].Type, policyPath, chatType) {
				out = append(out, f.at(d.Name, "policy.CanonicalChat declares %s, which formatting, logging or encoding calls to print a chat identifier: read the identifier only through JID", d.Name.Name))
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && f.dir == policyDir && ts.Assign.IsValid() && f.isType(ts.Type, policyPath, chatType) {
					out = append(out, f.at(ts, "type %s is an alias of policy.CanonicalChat, so a method declared on it, in any file of the package, is a method of CanonicalChat that this rule does not see", ts.Name.Name))
				}
			}
		}
	}
	return out
}
