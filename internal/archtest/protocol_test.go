package archtest

import (
	"go/ast"
	"net/netip"
	"regexp"
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
}

var bannedProtocolLogging = set("Stdout", "Zerolog")

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
		{name: "newsletter calls, the status message, raw GraphQL queries and loggers that bypass the writer", rel: "internal/engine/wa/x_test.go", want: 9, src: `package wa

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
}
`},
		{name: "the internals that reach raw queries outside a test", rel: "internal/engine/wa/x.go", want: 1, src: `package wa

import "go.mau.fi/whatsmeow"

func f(cli *whatsmeow.Client) { _ = cli.DangerousInternals() }
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
		{name: "the internals in a test, and other packages' Stdout", rel: "internal/engine/wa/x_test.go", src: `package wa

import (
	"os"

	"go.mau.fi/whatsmeow"
)

func f(cli *whatsmeow.Client) {
	_ = cli.DangerousInternals().DispatchEvent(nil)
	_ = os.Stdout
}
`},
	},
}

func checkProtocolCalls(f *sourceFile) []string {
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if why, banned := bannedProtocolCalls[name]; banned {
			out = append(out, f.at(sel, "%s %s, so it is never used", name, why))
		}
		if s, p := f.ref(sel); s != nil && p == waLogPath && bannedProtocolLogging[name] {
			out = append(out, f.at(sel, "the protocol library's %s logger bypasses the scrubbing writer: log through the adapter over logx", name))
		}
		if name == "DangerousInternals" && !f.test {
			out = append(out, f.at(sel, "DangerousInternals reaches raw queries that can end the process inside the protocol library; only tests may use it, to dispatch hand-built events"))
		}
		return true
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
