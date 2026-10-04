package archtest

import "go/ast"

const (
	sealFile = sealDir + "/seal.go"
	sealChat = "Chat"
)

var chatRenderingMethods = set("String", "GoString", "Format", "MarshalText", "MarshalJSON", "MarshalBinary", "LogValue", "Error")

var chatMethodRule = rule{
	name:  "chat-methods",
	check: checkChatMethods,
	cases: []snippet{
		{name: "rendering methods on the chat type", rel: sealFile, want: 8, src: `package seal

import (
	"fmt"
	"log/slog"
)

func (c Chat) String() string                  { return c.JID() }
func (c *Chat) GoString() string               { return c.JID() }
func (Chat) Format(fmt.State, rune)            {}
func (c (Chat)) MarshalText() ([]byte, error)  { return nil, nil }
func (c *(Chat)) MarshalJSON() ([]byte, error) { return nil, nil }
func (Chat) MarshalBinary() ([]byte, error)    { return nil, nil }
func (c Chat) LogValue() slog.Value            { return slog.StringValue(c.JID()) }
func (c Chat) Error() string                   { return c.JID() }
`},
		{name: "a rendering method declared in a seal test", rel: sealDir + "/x_test.go", want: 1, src: `package seal

func (c Chat) String() string { return c.JID() }
`},
		{name: "aliases of the chat type in a seal test", rel: sealDir + "/x_test.go", want: 3, src: `package seal

type cc = Chat

type (
	pc = *Chat
	nc = (Chat)
)

func (c cc) String() string   { return "" }
func (p pc) GoString() string { return "" }
`},
		{name: "an alias of the chat type in a non-test seal file", rel: sealFile, want: 1, src: `package seal

type cc = Chat
`},
		{name: "defined types and other aliases in a seal test", rel: sealDir + "/x_test.go", src: `package seal

type dc Chat

type (
	rg = ReadGrant
	s  = string
)

func (d dc) String() string { return "" }
`},
		{name: "aliases of the chat type in package policy, where the compiler refuses methods on them", rel: "internal/policy/policy.go", src: `package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

type CanonicalChat = seal.Chat

type cc = CanonicalChat
`},
		{name: "an alias of the chat type in another package's test", rel: "internal/api/x_test.go", src: `package api

import "github.com/dortort/wawarden/internal/policy"

type cc = policy.CanonicalChat
`},
		{name: "accessors, other types' methods and functions", rel: sealFile, src: `package seal

type ChatKind uint8

func (k ChatKind) String() string   { return "" }
func (c Chat) JID() string          { return "" }
func (c Chat) Kind() ChatKind       { return 0 }
func (c Chat) Valid() bool          { return c.ok }
func String(c Chat) string          { return "" }
func NewChat(string, ChatKind) Chat { return Chat{} }
`},
		{name: "another package's chat type", rel: "internal/api/x.go", src: `package api

type Chat struct{}

func (Chat) String() string { return "" }
`},
	},
}

func checkChatMethods(f *sourceFile) []string {
	var out []string
	for _, decl := range f.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 && chatRenderingMethods[d.Name.Name] && f.isType(d.Recv.List[0].Type, sealPath, sealChat) {
				out = append(out, f.at(d.Name, "seal.Chat, which package policy exports as CanonicalChat, declares %s, which formatting, logging or encoding calls to print a chat identifier: read the identifier only through JID", d.Name.Name))
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && f.dir == sealDir && ts.Assign.IsValid() && f.isType(ts.Type, sealPath, sealChat) {
					out = append(out, f.at(ts, "type %s is an alias of seal.Chat, so a method declared on it, in any file of package seal, is a method of the chat type that this rule does not see", ts.Name.Name))
				}
			}
		}
	}
	return out
}
