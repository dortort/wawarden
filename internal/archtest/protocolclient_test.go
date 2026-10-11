package archtest

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
)

var protocolClientCalls = set("AddEventHandlerWithSuccessStatus", "BuildHistorySyncRequest", "ConnectContext", "Disconnect", "DownloadToFile",
	"GenerateMessageID", "IsConnected", "IsLoggedIn", "Logout", "PairPhone", "ParseWebMessage", "SendMessage", "SendPeerMessage",
	"SendProtocolMessageReceipt", "SetMediaHTTPClient", "SetPreLoginHTTPClient", "SetWebsocketHTTPClient")

var confinedProtocolCalls = map[string]string{"SendMessage": "transmitMessage", "SendPeerMessage": "transmitPeerMessage"}

var protocolClientMethods = sync.OnceValues(func() (map[string]bool, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", whatsmeowModule).Output()
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return nil, fmt.Errorf("go list -m %s printed no directory", whatsmeowModule)
	}
	fsys := os.DirFS(dir)
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	methods := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		if file.Name.Name != "whatsmeow" {
			continue
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || !fd.Name.IsExported() {
				continue
			}
			recv := ast.Unparen(fd.Recv.List[0].Type)
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = ast.Unparen(star.X)
			}
			if id, ok := recv.(*ast.Ident); ok && id.Name == "Client" {
				methods[fd.Name.Name] = true
			}
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no method of %s.Client found in %s", whatsmeowModule, dir)
	}
	return methods, nil
})

var protocolClientCallRule = rule{
	name:  "protocol-client-calls",
	check: checkProtocolClientCalls,
	cases: []snippet{
		{name: "library calls the adapter does not make, a send outside its transmitter, and the names reached by value, expression, interface or embedding", rel: "internal/engine/wa/x.go", want: 9, src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type grouper interface {
	CreateGroup(context.Context, whatsmeow.ReqCreateGroup) (*types.GroupInfo, error)
}

type wrapped struct{ *whatsmeow.Client }

func transmitMessage(ctx context.Context, cli *whatsmeow.Client, to types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	later := func() { _, _ = cli.SendMessage(ctx, to, msg) }
	later()
	_ = cli.SendMessage
	return cli.SendMessage(ctx, to, msg)
}

func broadcast(ctx context.Context, cli *whatsmeow.Client, w wrapped, jids []types.JID, msg *waE2E.Message) {
	for _, jid := range jids {
		_, _ = cli.SendMessage(ctx, jid, msg)
	}
	_, _ = cli.CreateGroup(ctx, whatsmeow.ReqCreateGroup{})
	_ = cli.LeaveGroup(ctx, jids[0])
	_ = (*whatsmeow.Client).CreateGroup
	_ = w.LeaveGroup(ctx, jids[0])
	send := cli.SendPeerMessage
	_, _ = send(ctx, msg)
}
`},
		{name: "a transmitter in a subpackage of the adapter", rel: "internal/engine/wa/sub/x.go", want: 1, src: `package sub

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

func transmitPeerMessage(ctx context.Context, cli *whatsmeow.Client, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	return cli.SendPeerMessage(ctx, msg)
}
`},
		{name: "the calls the adapter makes, its transmitters and its own methods whose names the library's client shares", rel: "internal/engine/wa/x.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type Client struct{ cli *whatsmeow.Client }

type socketState interface {
	IsConnected() bool
	IsLoggedIn() bool
}

func transmitMessage(ctx context.Context, cli *whatsmeow.Client, to types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return cli.SendMessage(ctx, to, msg, extra)
}

func transmitPeerMessage(ctx context.Context, cli *whatsmeow.Client, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	return (cli.SendPeerMessage)(ctx, msg)
}

func (c *Client) Connect(ctx context.Context) error { return c.cli.ConnectContext(ctx) }

func (c *Client) Disconnect() { c.cli.Disconnect() }

func (c *Client) Logout(ctx context.Context) error { return c.cli.Logout(ctx) }

func (c *Client) PairPhone(ctx context.Context, phone string) (string, error) {
	return c.cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "x")
}

func (c *Client) DownloadHistory() {}

func (c *Client) Send() {}

func (c *Client) Backfill() {}

func f(ctx context.Context, s socketState, cli *whatsmeow.Client, web any) {
	if s.IsConnected() && s.IsLoggedIn() && cli.IsConnected() && cli.IsLoggedIn() {
		_ = cli.BuildHistorySyncRequest(&types.MessageInfo{}, 50)
		_ = cli.GenerateMessageID()
	}
	_ = cli.SendProtocolMessageReceipt(ctx, "", types.ReceiptTypeHistorySync)
	_ = cli.DownloadToFile
	_ = cli.ParseWebMessage
	_ = web
}
`},
		{name: "library calls in the adapter's tests, which the offline-tests rule fences, and the names outside the adapter", rel: "internal/engine/wa/x_test.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
)

func f(ctx context.Context, cli *whatsmeow.Client) {
	_, _ = cli.CreateGroup(ctx, whatsmeow.ReqCreateGroup{})
	_ = cli.SendMessage
}
`},
		{name: "the engine's interface, whose names the library's client shares", rel: "internal/engine/x.go", src: `package engine

import "context"

type Client interface {
	Connect(context.Context) error
	Download(context.Context) error
	SendMessage(context.Context) error
}
`},
	},
}

func checkProtocolClientCalls(f *sourceFile) []string {
	if f.test || !within(f.dir, adapterDir) {
		return nil
	}
	methods, err := protocolClientMethods()
	if err != nil {
		return []string{f.at(f.file, "read the methods of the protocol client: %v", err)}
	}
	declared := map[*ast.Ident]bool{}
	transmitters := map[string]ast.Node{}
	for _, d := range f.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		switch {
		case !ok:
		case fd.Recv != nil:
			declared[fd.Name] = true
		case f.dir == adapterDir && fd.Body != nil:
			transmitters[fd.Name.Name] = fd.Body
		}
	}
	var closures []ast.Node
	called := map[*ast.Ident]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.FuncLit:
			closures = append(closures, e)
		case *ast.CallExpr:
			if sel, ok := ast.Unparen(e.Fun).(*ast.SelectorExpr); ok {
				called[sel.Sel] = true
			}
		}
		return true
	})
	inside := func(n ast.Node, span ast.Node) bool {
		return span != nil && span.Pos() <= n.Pos() && n.End() <= span.End()
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || !methods[id.Name] || declared[id] {
			return true
		}
		if !protocolClientCalls[id.Name] {
			out = append(out, f.at(id, "%s is a method of the protocol client that the adapter does not call: the adapter calls only the reviewed methods that %s lists", id.Name, archtestDir))
		} else if fn, ok := confinedProtocolCalls[id.Name]; ok && (!called[id] || !inside(id, transmitters[fn]) || slices.ContainsFunc(closures, func(c ast.Node) bool { return inside(id, c) })) {
			out = append(out, f.at(id, "%s sends to WhatsApp: only %s in %s calls it, directly and outside any function literal, so every send takes the engine's one send path", id.Name, fn, adapterDir))
		}
		return true
	})
	return out
}

func TestProtocolClientCallsAreTheLibrarys(t *testing.T) {
	methods, err := protocolClientMethods()
	if err != nil {
		t.Fatalf("read the methods of the protocol client: %v", err)
	}
	for name := range protocolClientCalls {
		if !methods[name] {
			t.Errorf("the reviewed protocol calls name %s, which is not a method of %s.Client", name, whatsmeowModule)
		}
	}
	for name, fn := range confinedProtocolCalls {
		if !protocolClientCalls[name] {
			t.Errorf("%s is confined to %s but is not among the reviewed protocol calls", name, fn)
		}
	}
	for name := range bannedProtocolCalls {
		if methods[name] && protocolClientCalls[name] {
			t.Errorf("%s is both banned and among the reviewed protocol calls", name)
		}
	}
}
