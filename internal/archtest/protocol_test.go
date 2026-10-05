package archtest

import (
	"go/ast"
	"go/token"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const waLogPath = whatsmeowModule + "/util/log"

var bannedProtocolCalls = map[string]string{
	"SendPresence":                   "announces presence, which also turns delivery receipts into visible ones",
	"SendChatPresence":               "sends typing indicators",
	"SubscribePresence":              "subscribes to a contact's presence",
	"MarkRead":                       "sends read receipts",
	"SetForceActiveDeliveryReceipts": "makes delivery receipts visible to senders",
	"DownloadHistorySync":            "writes history payloads of any own device into the session store",
	"SetStatusMessage":               "can end the process inside the protocol library",
	"GetQRChannel":                   "logs the pairing QR codes, which carry the device secret",
	"EnableDecryptedEventBuffer":     "keeps decrypted message plaintext in session.db",
	"SendMexIQ":                      "can end the process inside the protocol library",
	"DangerousInternals":             "reaches raw queries that can end the process and internal handlers that can dial",
	"AddEventHandler":                "registers a handler whose answer the protocol library ignores, so a message the engine refused would be acknowledged",
	"RemoveEventHandler":             "removes the adapter's handler, after which the protocol library acknowledges every event without delivering it",
	"RemoveEventHandlers":            "removes the adapter's handler, after which the protocol library acknowledges every event without delivering it",
	"DefaultContextLogger":           "attaches a logger that bypasses the scrubbing writer",
	"CreateNewsletter":               "is a newsletter call, which can end the process inside the protocol library",
	"FollowNewsletter":               "is a newsletter call, which can end the process inside the protocol library",
	"UnfollowNewsletter":             "is a newsletter call, which can end the process inside the protocol library",
	"GetNewsletterInfo":              "is a newsletter call, which can end the process inside the protocol library",
	"GetNewsletterInfoWithInvite":    "is a newsletter call, which can end the process inside the protocol library",
	"GetNewsletterMessages":          "is a newsletter call, which can end the process inside the protocol library",
	"GetNewsletterMessageUpdates":    "is a newsletter call, which can end the process inside the protocol library",
	"GetSubscribedNewsletters":       "is a newsletter call, which can end the process inside the protocol library",
	"NewsletterMarkViewed":           "is a newsletter call, which can end the process inside the protocol library",
	"NewsletterSendReaction":         "is a newsletter call, which can end the process inside the protocol library",
	"NewsletterSubscribeLiveUpdates": "is a newsletter call, which can end the process inside the protocol library",
	"NewsletterToggleMute":           "is a newsletter call, which can end the process inside the protocol library",
	"UploadNewsletter":               "is a newsletter call, which can end the process inside the protocol library",
	"UploadNewsletterReader":         "is a newsletter call, which can end the process inside the protocol library",
	"SetProxy":                       "routes the protocol library's traffic through a proxy",
	"SetProxyAddress":                "routes the protocol library's traffic through a proxy",
	"SetSOCKSProxy":                  "routes the protocol library's traffic through a proxy",
}

var protocolSettings = set("AutoReconnectHook", "AutomaticMessageRerequestFromPhone", "DisableLoginAutoReconnect", "DisableManualHistorySyncReceipt",
	"EnableAutoReconnect", "InitialAutoReconnect", "ManualHistorySyncDownload", "PrePairCallback", "RefreshCAT", "SendReportingTokens", "SynchronousAck",
	"UseRetryMessageStore")

var (
	bannedProtocolLogging = set("Stdout", "Zerolog")
	bannedProtocolNames   = lowerKeys(bannedProtocolCalls)
	protocolSettingNames  = lowerKeys(protocolSettings)
	protocolNameInText    = regexp.MustCompile(`(?i)\b(` + strings.Join(slices.Sorted(maps.Keys(bannedProtocolCalls)), "|") + "|" + strings.Join(slices.Sorted(maps.Keys(protocolSettings)), "|") + `)\b`)
	jsonEscape            = regexp.MustCompile(`\\u[0-9A-Fa-f]{4}`)
	documentDecoders      = []string{"encoding/gob", "encoding/json", "encoding/xml", "html/template", "net/rpc", "text/template"}
)

const (
	settingWriter   = "installLocked"
	handlerRegistry = "AddEventHandlerWithSuccessStatus"
	handlerFactory  = "handlerFor"
)

func lowerKeys[V any](m map[string]V) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[strings.ToLower(k)] = k
	}
	return out
}

var protocolCallRule = rule{
	name:  "protocol-calls",
	check: checkProtocolCalls,
	cases: []snippet{
		{name: "receipts, presence, the store-writing history download and the QR channel", rel: "internal/engine/wa/x.go", want: 9, src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func f(ctx context.Context, cli *whatsmeow.Client) {
	_ = cli.SendPresence(ctx, types.PresenceAvailable)
	_ = cli.SendChatPresence(ctx, types.EmptyJID, types.ChatPresenceComposing, "")
	_ = cli.SubscribePresence(ctx, types.EmptyJID)
	_ = cli.MarkRead(ctx, nil, cli.LastSuccessfulConnect, types.EmptyJID, types.EmptyJID)
	cli.SetForceActiveDeliveryReceipts(true)
	_, _ = cli.DownloadHistorySync(ctx, nil, true)
	_, _ = cli.GetQRChannel(ctx)
	cli.EnableDecryptedEventBuffer = true
	read := cli.MarkRead
	_ = read
}
`},
		{name: "newsletter calls, the status message, raw GraphQL queries, loggers that bypass the writer and proxies", rel: "internal/engine/wa/x_test.go", want: 12, src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func f(ctx context.Context, cli *whatsmeow.Client, z any) {
	_, _ = cli.GetNewsletterInfo(ctx, cli.Store.GetJID())
	_ = cli.FollowNewsletter(ctx, cli.Store.GetJID())
	_, _ = cli.GetSubscribedNewsletters(ctx)
	_ = cli.NewsletterToggleMute(ctx, cli.Store.GetJID(), true)
	_ = cli.SetStatusMessage(ctx, "")
	_, _ = cli.DangerousInternals().SendMexIQ(ctx, "", nil)
	_ = waLog.Stdout("x", "DEBUG", false)
	_ = waLog.Zerolog
	_ = z.(interface{ DefaultContextLogger() }).DefaultContextLogger
	cli.SetProxyAddress("socks5://127.0.0.1:1080")
}
`},
		{name: "the internals and a handler whose answer is ignored", rel: "internal/engine/wa/x.go", want: 2, src: `package wa

import "go.mau.fi/whatsmeow"

func f(cli *whatsmeow.Client) {
	_ = cli.DangerousInternals()
	_ = cli.AddEventHandler(func(any) {})
}
`},
		{name: "the calls the adapter makes, and its installer building and setting the client and registering its handler", rel: "internal/engine/wa/x.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type Client struct {
	cli *whatsmeow.Client
	gen uint64
}

func (c *Client) handlerFor(gen uint64) whatsmeow.EventHandlerWithSuccessStatus {
	return func(any) bool { return gen == c.gen }
}

func (c *Client) installLocked(device *store.Device) {
	cli := whatsmeow.NewClient(device, waLog.Noop)
	cli.ManualHistorySyncDownload = true
	cli.DisableManualHistorySyncReceipt = true
	cli.EnableAutoReconnect, cli.InitialAutoReconnect = false, false
	cli.AutoReconnectHook = func(error) bool { return false }
	cli.SetWebsocketHTTPClient(nil)
	(cli.SetPreLoginHTTPClient)(nil)
	cli.SetMediaHTTPClient(nil)
	c.gen++
	(cli.AddEventHandlerWithSuccessStatus)(c.handlerFor(c.gen))
	c.cli = cli
}

func f(ctx context.Context, cli *whatsmeow.Client, log waLog.Logger) error {
	_ = proto.Unmarshal
	_ = cli.ManualHistorySyncDownload && !cli.EnableAutoReconnect
	_ = types.NewsletterServer
	_ = waLog.Noop
	log.Infof("x")
	return cli.SendProtocolMessageReceipt(ctx, "", types.ReceiptTypeHistorySync)
}
`},
		{name: "a handler with its answer, a transport set in a test, which the offline-tests rule refuses, and other packages' Stdout", rel: "internal/engine/wa/x_test.go", src: `package wa

import (
	"os"

	"go.mau.fi/whatsmeow"
)

func f(cli *whatsmeow.Client) {
	_ = cli.AddEventHandlerWithSuccessStatus(func(any) bool { return true })
	cli.SetMediaHTTPClient(nil)
	_ = os.Stdout
}
`},
		{name: "handlers removed and transports replaced after the installer", rel: "internal/engine/wa/x.go", want: 7, src: `package wa

import (
	"net/http"

	"go.mau.fi/whatsmeow"
)

type Client struct{ cli *whatsmeow.Client }

func (c *Client) installLocked(cli *whatsmeow.Client) {
	cli.SetMediaHTTPClient(&http.Client{})
	func() { cli.SetWebsocketHTTPClient(&http.Client{}) }()
	c.cli = cli
}

func (c *Client) Disconnect() {
	c.cli.Disconnect()
	c.cli.RemoveEventHandlers()
	_ = c.cli.RemoveEventHandler(1)
}

func (c *Client) DownloadHistory() {
	c.cli.SetMediaHTTPClient(&http.Client{})
	set := c.cli.SetPreLoginHTTPClient
	set(nil)
	_ = (*whatsmeow.Client).SetWebsocketHTTPClient
}

func installLocked(cli *whatsmeow.Client) { cli.SetMediaHTTPClient(nil) }
`},
		{name: "clients built without the constructor, and banned names reached by reflection, decoding or declarations", rel: "internal/engine/wa/x_test.go", want: 12, src: `package wa

import (
	"encoding/json"
	"reflect"

	"go.mau.fi/whatsmeow"
)

type options struct {
	SynchronousAck bool ` + "`json:\"enableDecryptedEventBuffer\"`" + `
}

func markread() {}

func f(cli *whatsmeow.Client, v reflect.Value) {
	_ = &whatsmeow.Client{EnableAutoReconnect: true}
	_ = new(whatsmeow.Client)
	var zero whatsmeow.Client
	_ = zero
	_ = reflect.TypeFor[whatsmeow.Client]()
	_ = json.Unmarshal([]byte(` + "`{\"ManualHistorySyncDownload\":false}`" + `), cli)
	_ = v.MethodByName("Send" + "Presence")
	_ = v.FieldByName("enabledecryptedeventbuffer")
	_ = options{SynchronousAck: true}
}
`},
		{name: "pointers to clients, settings as fields and text near the names", rel: "internal/engine/wa/x.go", src: `package wa

import "go.mau.fi/whatsmeow"

type holder struct{ cli *whatsmeow.Client }

func f(cli *whatsmeow.Client, h holder) []string {
	h.cli = cli
	_ = !cli.SynchronousAck && cli.RefreshCAT != nil
	_ = []*whatsmeow.Client{cli}
	return []string{"manual history sync", "SendPresenceNow", "the read marker", "AddEventHandlerWithSuccessStatus", "markReadable"}
}
`},
		{name: "document decoders in the adapter, settings written outside its installer and a handler that is not its own", rel: "internal/engine/wa/x.go", want: 12, src: `package wa

import (
	"encoding/gob"
	"encoding/json"
	"encoding/xml"
	"net/rpc/jsonrpc"

	"go.mau.fi/whatsmeow"
)

type Client struct{ cli *whatsmeow.Client }

func (c *Client) prepare(flags []bool) {
	c.cli.ManualHistorySyncDownload = false
	c.cli.EnableAutoReconnect, c.cli.InitialAutoReconnect = true, true
	p := &c.cli.DisableLoginAutoReconnect
	*p = false
	for _, c.cli.DisableManualHistorySyncReceipt = range flags {
	}
}

func (c *Client) installLocked(cli *whatsmeow.Client) {
	cli.SynchronousAck = false
	cli.AddEventHandlerWithSuccessStatus(func(any) bool {
		cli.SendReportingTokens = true
		return true
	})
}

func installLocked(cli *whatsmeow.Client) { cli.UseRetryMessageStore = true }
`},
		{name: "event handlers registered other than by the installer as its own handler for the current client", rel: "internal/engine/wa/x.go", want: 9, src: `package wa

import "go.mau.fi/whatsmeow"

type Client struct {
	cli *whatsmeow.Client
	gen uint64
}

func (c *Client) handlerFor(gen uint64) whatsmeow.EventHandlerWithSuccessStatus {
	return func(any) bool { return gen == c.gen }
}

func (c *Client) installLocked(cli *whatsmeow.Client) {
	cli.AddEventHandlerWithSuccessStatus(func(any) bool { return true })
	cli.AddEventHandlerWithSuccessStatus(c.handlerFor(c.gen + 1))
	cli.AddEventHandlerWithSuccessStatus(c.handlerFor(c.gen))
	other := &Client{}
	cli.AddEventHandlerWithSuccessStatus(other.handlerFor(other.gen))
	cli.AddEventHandlerWithSuccessStatus(c.handlerFor(other.gen))
	register := cli.AddEventHandlerWithSuccessStatus
	register(c.handlerFor(c.gen))
	func() { cli.AddEventHandlerWithSuccessStatus(c.handlerFor(c.gen)) }()
}

func (c *Client) prepare() {
	c.cli.AddEventHandlerWithSuccessStatus(c.handlerFor(c.gen))
	_ = (*whatsmeow.Client).AddEventHandlerWithSuccessStatus
}

func installLocked(c *Client, cli *whatsmeow.Client) {
	cli.AddEventHandlerWithSuccessStatus(c.handlerFor(c.gen))
}
`},
		{name: "clients built outside the installer", rel: "internal/engine/wa/x.go", want: 4, src: `package wa

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

type Client struct{ cli *whatsmeow.Client }

func (c *Client) installLocked(device *store.Device) {
	c.cli = whatsmeow.NewClient(device, nil)
	defer func() { c.cli = whatsmeow.NewClient(device, nil) }()
}

func (c *Client) replace(device *store.Device) *whatsmeow.Client {
	build := whatsmeow.NewClient
	_ = build
	return whatsmeow.NewClient(device, nil)
}

func installLocked(device *store.Device) *whatsmeow.Client { return whatsmeow.NewClient(device, nil) }
`},
		{name: "a document decoder in the device store", rel: "internal/store/session/x.go", want: 1, src: `package session

import "encoding/json/v2"
`},
		{name: "templates that call a method by a name built at run time", rel: "internal/engine/wa/x.go", want: 3, src: `package wa

import (
	"context"
	htmltemplate "html/template"
	"io"
	"text/template"
	"text/template/parse"

	"go.mau.fi/whatsmeow"
)

const verb, noun = "Send", "Presence"

func f(ctx context.Context, w io.Writer, cli *whatsmeow.Client) error {
	t := template.Must(template.New("p").Parse("{{.C." + verb + noun + " .Ctx \"available\"}}"))
	_ = htmltemplate.HTMLEscapeString
	_ = parse.NodeAction
	return t.Execute(w, map[string]any{"C": cli, "Ctx": ctx})
}
`},
		{name: "an installer in a test and names spelled through JSON escapes or case folding", rel: "internal/engine/wa/x_test.go", want: 3, src: `package wa

import (
	"encoding/json"

	"go.mau.fi/whatsmeow"
)

type Client struct{}

func (c *Client) installLocked(cli *whatsmeow.Client) {
	cli.SynchronousAck = false
	_ = json.Unmarshal([]byte("{\"Enable\\u0044ecryptedEventBuffer\":true,\"Manual\\u0048istorySyncDownload\":false}"), cli)
	_ = json.Unmarshal([]byte("{\"manualhi\u017ftorysyncdownload\":false}"), cli)
}
`},
		{name: "document decoders and templates in tests and elsewhere", rel: "internal/app/x.go", src: `package app

import (
	"encoding/json"
	"encoding/xml"
	"html/template"
)

var _ = json.Valid([]byte("{\"enable\\u0064\":true}"))
`},
		{name: "the architecture tests' own snippets", rel: archtestDir + "/x_test.go", src: `package archtest

var calls = map[string]string{"SendPresence": "x", "EnableDecryptedEventBuffer": "y", "cli.MarkRead(ctx)": "z"}
`},
	},
}

func checkProtocolCalls(f *sourceFile) []string {
	var out []string
	if !f.test && (within(f.dir, adapterDir) || within(f.dir, sessionDir)) {
		for _, imp := range f.imports {
			if slices.ContainsFunc(documentDecoders, func(d string) bool { return within(imp.path, d) }) {
				out = append(out, f.at(imp.node, "%q reaches the methods and fields of any value, a protocol client and its settings included, by names that a document or template holds: %s and %s decode nothing but protobuf and execute no template", imp.path, adapterDir, sessionDir))
			}
		}
	}
	var writers, closures []ast.Node
	receivers := map[ast.Node]string{}
	for _, d := range f.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && !f.test && f.dir == adapterDir && fd.Name.Name == settingWriter && fd.Recv != nil && fd.Body != nil &&
			len(fd.Recv.List) == 1 && f.isType(fd.Recv.List[0].Type, module+"/"+adapterDir, "Client") {
			writers = append(writers, fd.Body)
			if names := fd.Recv.List[0].Names; len(names) == 1 {
				receivers[fd.Body] = names[0].Name
			}
		}
	}
	var targets []ast.Expr
	var registries, constructors, setters []*ast.SelectorExpr
	registered := map[*ast.SelectorExpr]*ast.CallExpr{}
	setterCalls := map[*ast.SelectorExpr]bool{}
	fields, pointed := map[*ast.Ident]bool{}, map[ast.Expr]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.FuncLit:
			closures = append(closures, e)
		case *ast.CallExpr:
			if sel, ok := ast.Unparen(e.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == handlerRegistry {
				registered[sel] = e
			} else if ok && transportSetters[sel.Sel.Name] {
				setterCalls[sel] = true
			}
		case *ast.AssignStmt:
			targets = append(targets, e.Lhs...)
		case *ast.RangeStmt:
			if e.Tok == token.ASSIGN {
				targets = append(targets, e.Key, e.Value)
			}
		case *ast.IncDecStmt:
			targets = append(targets, e.X)
		case *ast.UnaryExpr:
			if e.Op == token.AND {
				targets = append(targets, e.X)
			}
		case *ast.SelectorExpr:
			fields[e.Sel] = true
			if e.Sel.Name == handlerRegistry {
				registries = append(registries, e)
			}
			if transportSetters[e.Sel.Name] {
				setters = append(setters, e)
			}
			if s, p := f.ref(e); s != nil && p == waLogPath && bannedProtocolLogging[e.Sel.Name] {
				out = append(out, f.at(e, "the protocol library's %s logger bypasses the scrubbing writer: log through the adapter over logx", e.Sel.Name))
			}
			if s, p := f.ref(e); s != nil && p == whatsmeowModule && e.Sel.Name == "NewClient" {
				constructors = append(constructors, e)
			}
			if s, p := f.ref(e); s != nil && p == whatsmeowModule && e.Sel.Name == "Client" && !pointed[e] {
				out = append(out, f.at(e, "a protocol client that is not a pointer can be built by a literal, new or a zero value, without the transports and settings the adapter gives it: only whatsmeow.NewClient in the adapter builds one"))
			}
		case *ast.StarExpr:
			pointed[ast.Unparen(e.X)] = true
		case *ast.Ident:
			if name, banned := bannedProtocolNames[strings.ToLower(e.Name)]; banned {
				out = append(out, f.at(e, "%s %s, so it is never used, named or declared", name, bannedProtocolCalls[name]))
			} else if name, setting := protocolSettingNames[strings.ToLower(e.Name)]; setting && !fields[e] {
				out = append(out, f.at(e, "%s is a protocol client setting that the adapter fixes: it may be named only as a field of a value, so that no literal, declaration or decoded document sets it", name))
			}
		}
		return true
	})
	inside := func(n ast.Node, spans []ast.Node) bool {
		return slices.ContainsFunc(spans, func(s ast.Node) bool { return s.Pos() <= n.Pos() && n.End() <= s.End() })
	}
	for _, target := range targets {
		sel, ok := ast.Unparen(target).(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if name, setting := protocolSettingNames[strings.ToLower(sel.Sel.Name)]; setting && (!inside(sel, writers) || inside(sel, closures)) {
			out = append(out, f.at(sel, "%s is a protocol client setting that only (*Client).%s in %s writes, so nothing undoes it after the client is built", name, settingWriter, adapterDir))
		}
	}
	for _, sel := range constructors {
		if !f.test && (!inside(sel, writers) || inside(sel, closures)) {
			out = append(out, f.at(sel, "only (*Client).%s in %s builds a protocol client, outside any function literal, so every client the adapter holds has its settings, guarded transports and handler", settingWriter, adapterDir))
		}
	}
	for _, sel := range setters {
		if !f.test && (!setterCalls[sel] || !inside(sel, writers) || inside(sel, closures)) {
			out = append(out, f.at(sel, "%s replaces a transport of a protocol client: only (*Client).%s in %s calls it, outside any function literal, so every request of every client goes through the guarded, capped transports", sel.Sel.Name, settingWriter, adapterDir))
		}
	}
	for _, sel := range registries {
		if f.test || !inside(sel, closures) && ownHandler(registered[sel], writers, receivers) {
			continue
		}
		out = append(out, f.at(sel, "the protocol library acknowledges an event only when every handler answers true: only (*Client).%s in %s registers one, outside any function literal, and only as %s(<receiver>.gen) of its own receiver, so the answer is the engine's for the current client", settingWriter, adapterDir, handlerFactory))
	}
	if f.dir == archtestDir {
		return out
	}
	literalRuns(f.file, func(at ast.Node, s string) {
		m := protocolNameInText.FindString(s)
		if m == "" {
			m = protocolNameInText.FindString(jsonUnescaped(s))
		}
		if m != "" {
			out = append(out, f.at(at, "%q spells %s, a banned protocol call or a protocol client setting, which reflection or a decoded document could reach by name", s, m))
		}
	})
	return out
}

func ownHandler(call *ast.CallExpr, writers []ast.Node, receivers map[ast.Node]string) bool {
	if call == nil || len(call.Args) != 1 || call.Ellipsis.IsValid() {
		return false
	}
	recv := ""
	for _, w := range writers {
		if w.Pos() <= call.Pos() && call.End() <= w.End() {
			recv = receivers[w]
		}
	}
	isRecv := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && recv != "" && recv != "_" && id.Name == recv
	}
	inner, ok := ast.Unparen(call.Args[0]).(*ast.CallExpr)
	if !ok || len(inner.Args) != 1 || inner.Ellipsis.IsValid() {
		return false
	}
	factory, ok := ast.Unparen(inner.Fun).(*ast.SelectorExpr)
	if !ok || factory.Sel.Name != handlerFactory || !isRecv(factory.X) {
		return false
	}
	gen, ok := ast.Unparen(inner.Args[0]).(*ast.SelectorExpr)
	return ok && gen.Sel.Name == "gen" && isRecv(gen.X)
}

func jsonUnescaped(s string) string {
	return jsonEscape.ReplaceAllStringFunc(s, func(esc string) string {
		if r, err := strconv.Unquote(`"` + esc + `"`); err == nil {
			return r
		}
		return esc
	})
}

var (
	urlLiteral  = regexp.MustCompile(`(?i)\b(?:https?|wss?)://([^/\s"'?#\\]*)`)
	hostAndPort = regexp.MustCompile(`(?i)(?:^|[^@\w./-])((?:[a-z0-9-]+\.)+[a-z]{2,}):[0-9]{1,5}\b`)
	metaHost    = regexp.MustCompile(`(?i)(?:^|[^@\w.-])((?:[a-z0-9-]+\.)*(?:whatsapp\.(?:com|net)|wa\.me|facebook\.com|fbcdn\.net|fbsbx\.com|instagram\.com|cdninstagram\.com|messenger\.com))\b`)
)

var serverNames = set("s.whatsapp.net", "whatsapp.net")

var networkEntryPoints = set("ConnectContext", "Download", "DownloadAny", "DownloadFB", "DownloadFBToFile", "DownloadMediaWithOnlyPath",
	"DownloadMediaWithOnlyPathToFile", "DownloadMediaWithPath", "DownloadMediaWithPathToFile", "DownloadThumbnail", "DownloadToFile", "FetchAppState",
	"SendMessage", "SendPeerMessage", "Upload", "UploadReader")

var transportSetters = set("SetMediaHTTPClient", "SetPreLoginHTTPClient", "SetWebsocketHTTPClient")

const whatsmeowSocket = whatsmeowModule + "/socket"

var frameSocket = set("FrameSocket", "NewFrameSocket")

var offlineTestRule = rule{
	name:  "offline-tests",
	check: checkOfflineTests,
	cases: []snippet{
		{name: "protocol hosts and network calls in a test", rel: "internal/engine/wa/x_test.go", want: 12, src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
)

const (
	origin = "https://` + webHost + `"
	socket = "wss://` + webHost + `/ws/chat"
	media  = "` + mediaHost + `"
	dial   = "` + webHost + `:443"
	other  = "http://` + documentationAddress + `/x"
	named  = "relay.` + documentationName + `:8443"
)

func f(ctx context.Context, cli *whatsmeow.Client) {
	_, _ = whatsmeow.GetLatestVersion(ctx, nil)
	_ = cli.ConnectContext(ctx)
	_ = cli.DownloadToFile(ctx, nil, nil)
}
`},
		{name: "the client constructor, its frame socket, its transport setters, downloads, uploads and state fetches in a test", rel: "internal/engine/wa/x_test.go", want: 12, src: `package wa

import (
	"context"
	"net/http"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/socket"
)

func f(ctx context.Context, cli *whatsmeow.Client) {
	_ = socket.NewFrameSocket(nil, nil).Connect(ctx)
	_ = (&socket.FrameSocket{URL: socket.URL}).Connect(ctx)
	other := whatsmeow.NewClient(cli.Store, nil)
	build := whatsmeow.NewClient
	_ = build
	other.SetWebsocketHTTPClient(http.DefaultClient)
	other.SetPreLoginHTTPClient(nil)
	other.SetMediaHTTPClient(nil)
	_, _ = cli.Download(ctx, nil)
	_, _ = cli.DownloadAny(ctx, nil)
	_, _ = cli.DownloadMediaWithPath(ctx, "", nil, nil, nil, 0, "", "")
	_, _ = cli.Upload(ctx, nil, whatsmeow.MediaImage)
	_ = cli.FetchAppState(ctx, appstate.WAPatchRegular, false, false)
}
`},
		{name: "the adapter's own methods in a test, whose names the library's client shares, and the library's socket constants", rel: "internal/engine/wa/x_test.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/socket"
)

var origin = socket.Origin

type adapter interface {
	Connect(context.Context) error
	PairPhone(context.Context, string) (string, error)
	Logout(context.Context) error
	DownloadHistory(context.Context) error
	AckHistory(context.Context) error
}

func f(ctx context.Context, a adapter, cli *whatsmeow.Client) bool {
	_ = a.Connect(ctx)
	_, _ = a.PairPhone(ctx, "15550100001")
	_ = a.Logout(ctx)
	_ = a.DownloadHistory(ctx)
	_ = a.AckHistory(ctx)
	return cli.IsConnected()
}
`},
		{name: "identifiers, loopback and reserved names in a test", rel: "internal/engine/wa/x_test.go", src: `package wa

const (
	user    = "15550100001@s.whatsapp.net"
	device  = "15550100001.0:4@lid"
	server  = "s.whatsapp.net"
	bare    = "15550100001@whatsapp.net"
	local   = "http://127.0.0.1:8080/healthz"
	six     = "http://[::1]:8081/"
	example = "https://attacker.example/"
	test    = "https://app.example.test:8443"
	built   = "http://"
	paths   = "go.mau.fi/whatsmeow@v0.0.0/send.go:12 and 15550100004@s.whatsapp.net:0"
)
`},
		{name: "protocol hosts outside a test", rel: "internal/engine/wa/x.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
)

const origin = "https://` + webHost + `"

func f(ctx context.Context, cli *whatsmeow.Client) error {
	other := whatsmeow.NewClient(cli.Store, nil)
	other.SetMediaHTTPClient(nil)
	return cli.ConnectContext(ctx)
}
`},
	},
}

var (
	webHost              = "web." + strings.ToLower("WhatsApp") + ".com"
	mediaHost            = "mmg." + strings.ToLower("WhatsApp") + ".net"
	documentationAddress = "198.51.100." + strings.Repeat("7", 1)
	documentationName    = "corp." + strings.ToLower("INTERNAL")
)

func checkOfflineTests(f *sourceFile) []string {
	if !f.test {
		return nil
	}
	var out []string
	literalRuns(f.file, func(at ast.Node, s string) {
		for _, m := range urlLiteral.FindAllStringSubmatch(s, -1) {
			if host := m[1]; host != "" && !offlineHost(host) {
				out = append(out, f.at(at, "a test names the network host %q: tests reach only loopback and reserved example names", host))
			}
		}
		for _, m := range hostAndPort.FindAllStringSubmatch(s, -1) {
			if !offlineHost(m[1]) {
				out = append(out, f.at(at, "a test names the network host %q: tests reach only loopback and reserved example names", m[1]))
			}
		}
		for _, m := range metaHost.FindAllStringSubmatch(s, -1) {
			if !serverNames[strings.ToLower(m[1])] {
				out = append(out, f.at(at, "a test names the WhatsApp or Meta host %q: no test may contact WhatsApp", m[1]))
			}
		}
	})
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		s, p := f.ref(sel)
		switch {
		case s != nil && p == whatsmeowModule && s.Sel.Name == "GetLatestVersion":
			out = append(out, f.at(sel, "GetLatestVersion fetches from WhatsApp: test the version source through an injected transport"))
		case s != nil && p == whatsmeowModule && s.Sel.Name == "NewClient":
			out = append(out, f.at(sel, "NewClient builds a protocol client whose transports do not dial through the adapter's guard: build test clients with newClient"))
		case s != nil && p == whatsmeowSocket && frameSocket[s.Sel.Name]:
			out = append(out, f.at(sel, "%s builds the protocol library's own WebSocket, which dials WhatsApp without the adapter's guard", s.Sel.Name))
		case transportSetters[sel.Sel.Name]:
			out = append(out, f.at(sel, "%s replaces a transport that dials through the adapter's guard", sel.Sel.Name))
		case networkEntryPoints[sel.Sel.Name]:
			out = append(out, f.at(sel, "%s reaches WhatsApp: tests drive the adapter through hand-built events and injected seams", sel.Sel.Name))
		}
		return true
	})
	return out
}

func offlineHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, ok := strings.Cut(host, "]"); ok && strings.HasPrefix(h, "[") {
		host = strings.TrimPrefix(h, "[")
	} else if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		host = host[:i]
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback()
	}
	if host == "localhost" || host == "example.com" || host == "example.net" || host == "example.org" {
		return true
	}
	for _, suffix := range []string{".test", ".example", ".invalid", ".localhost", ".example.com", ".example.net", ".example.org"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
