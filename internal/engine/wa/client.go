// Package wa is the only user of the WhatsApp protocol library: it implements the engine's client boundary over a whatsmeow client, translates the library's events into the engine's plain data, guards pairing and caps downloads.
package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	pairDisplayName  = "Chrome (Linux)"
	pairWait         = 30 * time.Second
	maxHistoryLength = engine.MaxHistoryMaxBytes
	websocketTimeout = 30 * time.Second
	mediaTimeout     = 2 * time.Minute
)

var (
	errOptions      = errors.New("wa: a device, a device source, loggers and a history cap from 1 byte to 256 MiB are required")
	errNotOwner     = errors.New("wa: pairing is allowed only for the owner's number")
	errNoPairing    = errors.New("wa: the connection offered no pairing session in time")
	errCATRefresh   = errors.New("wa: connection tokens are not refreshed")
	errStoreRemoval = errors.New("wa: the local device could not be removed")
)

var versionMu sync.Mutex

var configureDevice = sync.OnceFunc(func() {
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.DeviceProps.RequireFullSync = proto.Bool(true)
})

type Devices interface {
	NewDevice() *store.Device
}

type Options struct {
	Device          *store.Device
	Devices         Devices
	OwnerPhone      string
	HistoryMaxBytes int64
	Logger          *slog.Logger
	Alerts          *slog.Logger
	UnsafeDebug     time.Duration
}

type downloader func(ctx context.Context, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification, dst whatsmeow.File) error

func downloadToFile(ctx context.Context, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification, dst whatsmeow.File) error {
	return cli.DownloadToFile(ctx, notif, dst)
}

type Client struct {
	devices  Devices
	owner    string
	max      int64
	log      logger
	http     httpClients
	download downloader

	mu       sync.Mutex
	cli      *whatsmeow.Client
	gen      uint64
	stale    bool
	paired   bool
	account  string
	handler  func(engine.Event) bool
	qr       chan struct{}
	qrSeen   bool
	dropping bool
	cancel   context.CancelFunc
}

var (
	_ engine.Client         = (*Client)(nil)
	_ engine.HistoryDecoder = (*Client)(nil)
)

func New(opts Options) (*Client, error) {
	return newClient(opts, guarded(systemDial()), downloadToFile, time.Now)
}

func newClient(opts Options, dial dialFunc, download downloader, now func() time.Time) (*Client, error) {
	if opts.Device == nil || opts.Devices == nil || opts.Logger == nil || opts.Alerts == nil ||
		opts.HistoryMaxBytes <= 0 || opts.HistoryMaxBytes > engine.MaxHistoryMaxBytes {
		return nil, errOptions
	}
	configureDevice()
	owner, _ := policy.OwnerDigits(opts.OwnerPhone)
	c := &Client{
		devices:  opts.Devices,
		owner:    owner,
		max:      opts.HistoryMaxBytes,
		log:      logger{out: opts.Logger, module: "whatsmeow", gate: newDebugGate(opts.UnsafeDebug, now, opts.Alerts)},
		http:     newHTTPClients(dial, opts.HistoryMaxBytes+mediaOverhead),
		download: download,
	}
	routeSignalLogs(logger{out: opts.Logger, module: "libsignal", gate: c.log.gate})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installLocked(opts.Device)
	if opts.Device.ID != nil {
		c.paired, c.account = true, account(*opts.Device.ID)
	}
	return c, nil
}

func account(jid types.JID) string {
	if jid.Server != types.DefaultUserServer {
		return ""
	}
	return jid.User
}

func (c *Client) installLocked(device *store.Device) {
	cli := whatsmeow.NewClient(device, c.log)
	cli.EnableAutoReconnect = false
	cli.InitialAutoReconnect = false
	cli.AutoReconnectHook = func(error) bool { return false }
	cli.ManualHistorySyncDownload = true
	cli.DisableManualHistorySyncReceipt = true
	cli.AutomaticMessageRerequestFromPhone = false
	cli.UseRetryMessageStore = false
	cli.SendReportingTokens = false
	cli.SynchronousAck = false
	cli.RefreshCAT = func(context.Context) error { return errCATRefresh }
	cli.PrePairCallback = c.prePair
	cli.SetWebsocketHTTPClient(c.http.websocket)
	cli.SetPreLoginHTTPClient(c.http.websocket)
	cli.SetMediaHTTPClient(c.http.media)
	c.gen++
	gen := c.gen
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool { return c.dispatch(gen, evt) })
	c.cli, c.stale = cli, false
}

func (c *Client) prePair(jid types.JID, _, _ string) bool {
	return c.owner != "" && jid.Server == types.DefaultUserServer && jid.User == c.owner
}

func (c *Client) current() *whatsmeow.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli
}

func (c *Client) dispatch(gen uint64, evt any) bool {
	c.mu.Lock()
	live := gen == c.gen
	c.mu.Unlock()
	if !live {
		return true
	}
	switch e := evt.(type) {
	case *events.QR:
		c.sawQR(gen)
		return true
	case *events.KeepAliveTimeout:
		if time.Since(e.LastSuccess) >= whatsmeow.KeepAliveMaxFailTime {
			c.dropDeadConnection(gen)
		}
		return true
	case *events.PairSuccess:
		c.update(gen, func() { c.paired, c.account = true, account(e.ID) })
	case *events.PairError:
		c.update(gen, func() { c.stale = true })
	case *events.LoggedOut:
		c.update(gen, func() { c.paired, c.account, c.stale = false, "", true })
	}
	ev, ok := translate(evt)
	if !ok {
		return true
	}
	return c.deliver(ev)
}

func (c *Client) update(gen uint64, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen == c.gen {
		fn()
	}
}

func (c *Client) deliver(ev engine.Event) bool {
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	return h != nil && h(ev)
}

func (c *Client) sawQR(gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen == c.gen && c.qr != nil && !c.qrSeen {
		c.qrSeen = true
		close(c.qr)
	}
}

func (c *Client) dropDeadConnection(gen uint64) {
	c.mu.Lock()
	if gen != c.gen || c.dropping {
		c.mu.Unlock()
		return
	}
	c.dropping = true
	cli := c.cli
	c.mu.Unlock()
	safego.Go("wa.keepalive", func() {
		cli.Disconnect()
		c.deliver(engine.Disconnected{})
	})
}

func (c *Client) OnEvent(handler func(engine.Event) bool) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.handler = nil
	}
}

func (c *Client) prepare() (*whatsmeow.Client, context.Context, context.CancelFunc) {
	c.mu.Lock()
	var old *whatsmeow.Client
	if c.stale {
		old = c.cli
		c.installLocked(c.devices.NewDevice())
		c.paired, c.account = false, ""
	}
	if c.cancel != nil {
		c.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.qr, c.qrSeen, c.dropping = make(chan struct{}), false, false
	cli := c.cli
	c.mu.Unlock()
	if old != nil {
		old.Disconnect()
	}
	return cli, ctx, cancel
}

func (c *Client) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cli, conn, cancel := c.prepare()
	var mu sync.Mutex
	finished := false
	done := make(chan struct{})
	safego.Go("wa.connect", func() {
		select {
		case <-ctx.Done():
			mu.Lock()
			if !finished {
				cancel()
			}
			mu.Unlock()
		case <-done:
		}
	})
	err := cli.ConnectContext(conn)
	mu.Lock()
	finished = true
	mu.Unlock()
	close(done)
	if err != nil {
		cancel()
		return err
	}
	return nil
}

func (c *Client) Disconnect() {
	c.mu.Lock()
	cli, cancel := c.cli, c.cancel
	c.cancel = nil
	c.mu.Unlock()
	cli.Disconnect()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) Paired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paired
}

func (c *Client) Account() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paired {
		return ""
	}
	return c.account
}

func (c *Client) PairPhone(ctx context.Context, digits string) (string, error) {
	if c.owner == "" || digits != c.owner {
		return "", errNotOwner
	}
	c.mu.Lock()
	cli, qr := c.cli, c.qr
	c.mu.Unlock()
	timer := time.NewTimer(pairWait)
	defer timer.Stop()
	select {
	case <-qr:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", errNoPairing
	}
	return cli.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, pairDisplayName)
}

func (c *Client) Logout(ctx context.Context) error {
	cli := c.current()
	if err := cli.Logout(ctx); err != nil {
		cli.Disconnect()
		if err := cli.Store.Delete(ctx); err != nil {
			return fmt.Errorf("%w: %w", errStoreRemoval, err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli == cli {
		c.paired, c.account, c.stale = false, "", true
	}
	return nil
}

func (c *Client) Version() engine.Version {
	versionMu.Lock()
	defer versionMu.Unlock()
	return engine.Version(store.GetWAVersion())
}

func (c *Client) SetVersion(v engine.Version) {
	versionMu.Lock()
	defer versionMu.Unlock()
	store.SetWAVersion(store.WAVersionContainer(v))
}

func (c *Client) AckHistory(ctx context.Context, ref engine.HistoryRef) error {
	return c.current().SendProtocolMessageReceipt(ctx, ref.ID, types.ReceiptTypeHistorySync)
}

func (c *Client) DownloadHistory(ctx context.Context, ref engine.HistoryRef, dst io.Writer) error {
	if !strings.HasPrefix(ref.DirectPath, "/") || len(ref.MediaKey) == 0 || len(ref.FileEncSHA256) != 32 || len(ref.FileSHA256) != 32 {
		return errHistoryRef
	}
	if ref.FileLength > maxHistoryLength || int64(ref.FileLength) > c.max {
		return errTooLarge
	}
	notif := &waE2E.HistorySyncNotification{
		DirectPath:    proto.String(ref.DirectPath),
		MediaKey:      ref.MediaKey,
		FileSHA256:    ref.FileSHA256,
		FileEncSHA256: ref.FileEncSHA256,
		FileLength:    proto.Uint64(ref.FileLength),
	}
	staged := newCapFile(c.max+mediaOverhead, int64(ref.FileLength))
	if err := c.download(ctx, c.current(), notif, staged); err != nil {
		return err
	}
	if int64(len(staged.bytes())) > c.max {
		return errTooLarge
	}
	_, err := dst.Write(staged.bytes())
	return err
}

func (c *Client) Decode(blob []byte) (engine.History, error) {
	return decodeHistory(c.current(), blob)
}

func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type httpClients struct {
	websocket *http.Client
	media     *http.Client
}

func newHTTPClients(dial dialFunc, mediaMax int64) httpClients {
	return httpClients{
		websocket: &http.Client{Transport: newTransport(dial), Timeout: websocketTimeout, CheckRedirect: refuseRedirects},
		media:     &http.Client{Transport: capped{next: newTransport(dial), max: mediaMax}, Timeout: mediaTimeout, CheckRedirect: refuseRedirects},
	}
}
