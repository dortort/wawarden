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
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !chatRenderingMethods[fn.Name.Name] {
			continue
		}
		if f.isType(fn.Recv.List[0].Type, policyPath, chatType) {
			out = append(out, f.at(fn.Name, "policy.CanonicalChat declares %s, which formatting, logging or encoding calls to print a chat identifier: read the identifier only through JID", fn.Name.Name))
		}
	}
	return out
}
