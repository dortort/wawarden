package archtest

import (
	"go/ast"
	"maps"
	"net/netip"
	"regexp"
	"slices"
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
		{name: "the calls the adapter makes", rel: "internal/engine/wa/x.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func f(ctx context.Context, cli *whatsmeow.Client, log waLog.Logger) error {
	cli.ManualHistorySyncDownload = true
	cli.DisableManualHistorySyncReceipt = true
	cli.EnableAutoReconnect = false
	_ = types.NewsletterServer
	_ = waLog.Noop
	log.Infof("x")
	return cli.SendProtocolMessageReceipt(ctx, "", types.ReceiptTypeHistorySync)
}
`},
		{name: "a handler with its answer, and other packages' Stdout", rel: "internal/engine/wa/x_test.go", src: `package wa

import (
	"os"

	"go.mau.fi/whatsmeow"
)

func f(cli *whatsmeow.Client) {
	_ = cli.AddEventHandlerWithSuccessStatus(func(any) bool { return true })
	_ = os.Stdout
}
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
	cli.SynchronousAck = false
	cli.RefreshCAT = nil
	_ = (*whatsmeow.Client).AddEventHandlerWithSuccessStatus
	_ = []*whatsmeow.Client{cli}
	return []string{"manual history sync", "SendPresenceNow", "the read marker", "AddEventHandlerWithSuccessStatus", "markReadable"}
}
`},
		{name: "the architecture tests' own snippets", rel: archtestDir + "/x_test.go", src: `package archtest

var calls = map[string]string{"SendPresence": "x", "EnableDecryptedEventBuffer": "y", "cli.MarkRead(ctx)": "z"}
`},
	},
}

func checkProtocolCalls(f *sourceFile) []string {
	var out []string
	fields, pointed := map[*ast.Ident]bool{}, map[ast.Expr]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			fields[e.Sel] = true
			if s, p := f.ref(e); s != nil && p == waLogPath && bannedProtocolLogging[e.Sel.Name] {
				out = append(out, f.at(e, "the protocol library's %s logger bypasses the scrubbing writer: log through the adapter over logx", e.Sel.Name))
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
	if f.dir == archtestDir {
		return out
	}
	literalRuns(f.file, func(at ast.Node, s string) {
		if m := protocolNameInText.FindString(s); m != "" {
			out = append(out, f.at(at, "%q spells %s, a banned protocol call or a protocol client setting, which reflection or a decoded document could reach by name", s, m))
		}
	})
	return out
}

var (
	urlLiteral  = regexp.MustCompile(`(?i)\b(?:https?|wss?)://([^/\s"'?#\\]*)`)
	hostAndPort = regexp.MustCompile(`(?i)(?:^|[^@\w./-])((?:[a-z0-9-]+\.)+[a-z]{2,}):[0-9]{1,5}\b`)
	metaHost    = regexp.MustCompile(`(?i)(?:^|[^@\w.-])((?:[a-z0-9-]+\.)*(?:whatsapp\.(?:com|net)|wa\.me|facebook\.com|fbcdn\.net|fbsbx\.com|instagram\.com|cdninstagram\.com|messenger\.com))\b`)
)

var serverNames = set("s.whatsapp.net", "whatsapp.net")

var networkEntryPoints = set("ConnectContext", "DownloadToFile", "DownloadMediaWithPathToFile", "SendMessage", "SendPeerMessage")

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

func f(ctx context.Context, cli *whatsmeow.Client) error { return cli.ConnectContext(ctx) }
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
		if s, p := f.ref(sel); s != nil && p == whatsmeowModule && s.Sel.Name == "GetLatestVersion" {
			out = append(out, f.at(sel, "GetLatestVersion fetches from WhatsApp: test the version source through an injected transport"))
		} else if networkEntryPoints[sel.Sel.Name] {
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
