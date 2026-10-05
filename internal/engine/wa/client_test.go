package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/safego"
	"github.com/dortort/wawarden/internal/store/session"
)

type freshDevices struct{ made atomic.Int32 }

func (d *freshDevices) NewDevice() *store.Device {
	d.made.Add(1)
	return &store.Device{}
}

func refusingDial(t *testing.T) dialFunc {
	return guarded(func(context.Context, string, string) (net.Conn, error) {
		t.Error("a dial passed the guard of a test binary")
		return nil, errNoDial
	})
}

func noDownload(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification, whatsmeow.File) error {
	return errNoDial
}

type rig struct {
	c       *Client
	devices *freshDevices
	logs    *logBuffer
	alerts  *logBuffer

	mu  sync.Mutex
	got []engine.Event
	ack bool
}

const ownerPhone = "+15550100009"

var ownerLID = types.NewJID("100000000000009", types.HiddenUserServer)

func pairedDevice() *store.Device {
	id := types.JID{User: owner.User, Device: 12, Server: types.DefaultUserServer}
	return &store.Device{ID: &id, LID: ownerLID}
}

func newRig(t *testing.T, device *store.Device, configure ...func(*Options)) *rig {
	t.Helper()
	w, logs := newWriter()
	alerts, alertLogs := newLogger(slog.LevelWarn)
	r := &rig{devices: &freshDevices{}, logs: logs, alerts: alertLogs, ack: true}
	opts := Options{Device: device, Devices: r.devices, OwnerPhone: ownerPhone, HistoryMaxBytes: 1 << 20, Writer: w, LogLevel: slog.LevelDebug, Alerts: alerts}
	for _, fn := range configure {
		fn(&opts)
	}
	c, err := newClient(opts, refusingDial(t), noDownload, time.Now)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	r.c = c
	t.Cleanup(c.OnEvent(func(ev engine.Event) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, ev)
		return r.ack
	}))
	return r
}

func (r *rig) events() []engine.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.Event(nil), r.got...)
}

func (r *rig) setAck(ack bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ack = ack
}

func (r *rig) dispatch(evt any) bool {
	r.c.mu.Lock()
	gen := r.c.gen
	r.c.mu.Unlock()
	return r.c.dispatch(gen, evt)
}

func TestNewRefusesIncompleteOptions(t *testing.T) {
	w, _ := newWriter()
	full := Options{Device: &store.Device{}, Devices: &freshDevices{}, HistoryMaxBytes: 1, Writer: w, Alerts: logx.New(w, slog.LevelWarn)}
	for name, change := range map[string]func(*Options){
		"no device":          func(o *Options) { o.Device = nil },
		"no device source":   func(o *Options) { o.Devices = nil },
		"no log writer":      func(o *Options) { o.Writer = nil },
		"no alert logger":    func(o *Options) { o.Alerts = nil },
		"no history cap":     func(o *Options) { o.HistoryMaxBytes = 0 },
		"a history cap over": func(o *Options) { o.HistoryMaxBytes = engine.MaxHistoryMaxBytes + 1 },
	} {
		opts := full
		change(&opts)
		if _, err := New(opts); !errors.Is(err, errOptions) {
			t.Errorf("New with %s = %v, want errOptions", name, err)
		}
	}
	if _, err := New(full); err != nil {
		t.Fatalf("New with complete options: %v", err)
	}
}

func TestTheClientIsBuiltForASupervisedDesktopCompanion(t *testing.T) {
	r := newRig(t, &store.Device{})
	if got := store.DeviceProps.GetPlatformType(); got != waCompanionReg.DeviceProps_DESKTOP {
		t.Errorf("platform %v, want DESKTOP", got)
	}
	if !store.DeviceProps.GetRequireFullSync() {
		t.Error("full sync is not required")
	}
	cli := r.c.current()
	for name, bad := range map[string]bool{
		"auto-reconnect is on":                          cli.EnableAutoReconnect,
		"initial auto-reconnect is on":                  cli.InitialAutoReconnect,
		"the library re-dials after a login request":    !cli.DisableLoginAutoReconnect,
		"automatic history download is on":              !cli.ManualHistorySyncDownload,
		"the automatic history receipt is on":           !cli.DisableManualHistorySyncReceipt,
		"acknowledgements wait for every handler":       cli.SynchronousAck,
		"messages are re-requested from the phone":      cli.AutomaticMessageRerequestFromPhone,
		"sent messages are kept in the device store":    cli.UseRetryMessageStore,
		"reporting tokens are sent":                     cli.SendReportingTokens,
		"no connection-token refresh stub is installed": cli.RefreshCAT == nil,
		"no pre-pair callback is installed":             cli.PrePairCallback == nil,
		"no reconnect hook is installed":                cli.AutoReconnectHook == nil,
	} {
		if bad {
			t.Errorf("%s", name)
		}
	}
	if err := cli.RefreshCAT(t.Context()); !errors.Is(err, errCATRefresh) {
		t.Errorf("the connection-token refresh = %v, want errCATRefresh", err)
	}
	if cli.AutoReconnectHook(errNoDial) {
		t.Error("the reconnect hook allows a reconnect")
	}
	if r.c.Paired() || r.c.Account() != "" {
		t.Fatalf("an unpaired device reports Paired %v, Account %q", r.c.Paired(), r.c.Account())
	}
}

func TestEveryHTTPClientIsBoundedAndIgnoresTheProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	r := newRig(t, &store.Device{})
	for name, client := range map[string]*http.Client{"websocket": r.c.http.websocket, "media": r.c.http.media} {
		if client.Timeout <= 0 || client.CheckRedirect == nil {
			t.Errorf("the %s client has timeout %v and redirect policy %v", name, client.Timeout, client.CheckRedirect != nil)
		}
		rt := client.Transport
		if c, ok := rt.(capped); ok {
			rt = c.next
		} else if name != "websocket" {
			t.Errorf("the %s client's responses are not capped", name)
		}
		checkTransport(t, name, rt)
	}
	if c, ok := r.c.http.media.Transport.(capped); !ok || c.max != (1<<20)+mediaOverhead {
		t.Fatalf("the media cap is %v, want the history cap plus %d bytes", r.c.http.media.Transport, mediaOverhead)
	}
	if err := r.c.http.media.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("the media client follows redirects: %v", err)
	}
}

func TestNoTestBinaryReachesTheNetworkThroughTheClient(t *testing.T) {
	r := newRig(t, pairedDevice())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := r.c.Connect(ctx)
	if !errors.Is(err, errOffline) {
		t.Fatalf("Connect = %v, want the offline refusal", err)
	}
	r.c.Disconnect()
	if err := r.c.AckHistory(ctx, engine.HistoryRef{ID: "3EB0SYNTHETIC"}); err == nil {
		t.Fatal("a history receipt was sent without a connection")
	}
}

func TestConnectHonoursAnEndedContext(t *testing.T) {
	r := newRig(t, pairedDevice())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.c.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect with an ended context = %v", err)
	}
}

func TestThePrePairCallbackAcceptsOnlyTheOwnersNumber(t *testing.T) {
	r := newRig(t, &store.Device{})
	pre := r.c.current().PrePairCallback
	for _, tt := range []struct {
		jid  types.JID
		want bool
	}{
		{types.JID{User: owner.User, Device: 12, Server: types.DefaultUserServer}, true},
		{types.JID{User: peer.User, Device: 12, Server: types.DefaultUserServer}, false},
		{types.JID{User: owner.User, Device: 12, Server: types.HiddenUserServer}, false},
		{types.JID{User: owner.User + "0", Server: types.DefaultUserServer}, false},
		{types.JID{User: "", Server: types.DefaultUserServer}, false},
	} {
		if got := pre(tt.jid, "Synthetic", ""); got != tt.want {
			t.Errorf("the pre-pair callback for %v = %v, want %v", tt.jid, got, tt.want)
		}
	}
	unset := newRig(t, &store.Device{}, func(o *Options) { o.OwnerPhone = "" })
	if unset.c.current().PrePairCallback(types.JID{User: owner.User, Server: types.DefaultUserServer}, "", "") {
		t.Fatal("pairing was accepted while the owner's number is unset")
	}
}

func TestPairPhoneServesOnlyTheOwnerAndWaitsForTheLoginSession(t *testing.T) {
	r := newRig(t, &store.Device{})
	if _, err := r.c.PairPhone(t.Context(), peer.User); !errors.Is(err, errNotOwner) {
		t.Fatalf("PairPhone for another number = %v, want errNotOwner", err)
	}
	unset := newRig(t, &store.Device{}, func(o *Options) { o.OwnerPhone = "" })
	if _, err := unset.c.PairPhone(t.Context(), owner.User); !errors.Is(err, errNotOwner) {
		t.Fatalf("PairPhone with the owner's number unset = %v, want errNotOwner", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.c.PairPhone(ctx, owner.User); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PairPhone before any login session = %v, want the context's deadline", err)
	}
}

func TestQRCodesAreDroppedAndOnlyOpenThePairingWait(t *testing.T) {
	r := newRig(t, &store.Device{})
	_, _, cancel := r.c.prepare()
	defer cancel()
	r.c.mu.Lock()
	qr := r.c.qr
	r.c.mu.Unlock()
	if !r.dispatch(&events.QR{Codes: []string{"2@SYNTHETIC-QR-SECRET", "2@SYNTHETIC-QR-SECRET-2"}}) {
		t.Fatal("the QR event was not acknowledged")
	}
	select {
	case <-qr:
	default:
		t.Fatal("the QR event did not open the pairing wait")
	}
	if !r.dispatch(&events.QR{Codes: []string{"2@SYNTHETIC-QR-SECRET-3"}}) {
		t.Fatal("a second QR event was not acknowledged")
	}
	if len(r.events()) != 0 {
		t.Fatalf("QR events reached the engine: %v", r.events())
	}
	if strings.Contains(r.logs.String()+r.alerts.String(), "SYNTHETIC-QR-SECRET") {
		t.Fatal("QR content reached the logs")
	}
}

func TestPairingEventsUpdateTheDeviceAndReachTheEngine(t *testing.T) {
	r := newRig(t, &store.Device{})
	ad := types.JID{User: owner.User, Device: 12, Server: types.DefaultUserServer}
	if !r.dispatch(&events.PairSuccess{ID: ad, LID: ownerLID, Platform: "android"}) {
		t.Fatal("the pairing event was not acknowledged")
	}
	if !r.c.Paired() || r.c.Account() != owner.User {
		t.Fatalf("after pairing Paired %v, Account %q, want true and the bare digits", r.c.Paired(), r.c.Account())
	}
	if got := r.events(); len(got) != 1 || got[0] != (engine.Paired{JID: owner.String()}) {
		t.Fatalf("engine events %v, want one Paired naming the account without its device", got)
	}

	rejected := newRig(t, &store.Device{})
	rejected.dispatch(&events.PairError{ID: types.JID{User: peer.User, Device: 3, Server: types.DefaultUserServer}, Error: whatsmeow.ErrPairRejectedLocally})
	if got := rejected.events(); len(got) != 1 || got[0] != (engine.PairRejected{}) {
		t.Fatalf("engine events %v, want one PairRejected", got)
	}
	if rejected.c.Paired() {
		t.Fatal("a rejected pairing reports a paired device")
	}
}

func TestAFailedPairingClosesTheAttemptForTheEngine(t *testing.T) {
	for name, err := range map[string]error{
		"an HMAC mismatch":            whatsmeow.ErrPairInvalidDeviceIdentityHMAC,
		"a signature mismatch":        whatsmeow.ErrPairInvalidDeviceSignature,
		"an undecodable identity":     &whatsmeow.PairProtoError{Message: "synthetic", ProtoErr: errNoDial},
		"a device that was not saved": &whatsmeow.PairDatabaseError{Message: "synthetic", DBErr: errNoDial},
		"an unsent confirmation":      fmt.Errorf("failed to send pairing confirmation: %w", errNoDial),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, &store.Device{})
			old := r.c.current()
			if !r.dispatch(&events.PairError{ID: types.JID{User: owner.User, Device: 12, Server: types.DefaultUserServer}, Error: err}) {
				t.Fatal("the pairing failure was not acknowledged")
			}
			if got := r.events(); len(got) != 1 || got[0] != (engine.Disconnected{}) {
				t.Fatalf("engine events %v, want one Disconnected, which the engine takes as the end of the pairing attempt", got)
			}
			if r.c.Paired() || r.c.Account() != "" {
				t.Fatalf("after a failed pairing Paired %v, Account %q", r.c.Paired(), r.c.Account())
			}
			_, _, cancel := r.c.prepare()
			defer cancel()
			if r.c.current() == old || r.devices.made.Load() != 1 {
				t.Fatal("the next attempt does not start from a fresh device")
			}
		})
	}
}

func TestALoginRequestIsADropForTheEngine(t *testing.T) {
	r := newRig(t, pairedDevice())
	_, _, cancel := r.c.prepare()
	defer cancel()
	for range 2 {
		if !r.dispatch(&events.ManualLoginReconnect{}) {
			t.Fatal("the login request was not acknowledged")
		}
	}
	eventually(t, "the engine hears of the drop", func() bool { return len(r.events()) > 0 })
	time.Sleep(50 * time.Millisecond)
	if got := r.events(); len(got) != 1 || got[0] != (engine.Disconnected{}) {
		t.Fatalf("engine events %v, want one Disconnected so that the engine re-dials with its own backoff", got)
	}
	if r.c.current().IsConnected() {
		t.Fatal("the connection that WhatsApp asked to log in again is still open")
	}
}

func TestALoggedOutDeviceIsReplacedByAFreshOneBeforeTheNextConnection(t *testing.T) {
	r := newRig(t, pairedDevice())
	if !r.c.Paired() || r.c.Account() != owner.User {
		t.Fatalf("a stored device reports Paired %v, Account %q", r.c.Paired(), r.c.Account())
	}
	old := r.c.current()
	r.dispatch(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	if r.c.Paired() || r.c.Account() != "" {
		t.Fatalf("after LoggedOut Paired %v, Account %q", r.c.Paired(), r.c.Account())
	}
	if got := r.events(); len(got) != 1 || got[0] != (engine.LoggedOut{}) {
		t.Fatalf("engine events %v, want one LoggedOut", got)
	}
	r.c.mu.Lock()
	oldGen := r.c.gen
	r.c.mu.Unlock()
	_, _, cancel := r.c.prepare()
	defer cancel()
	fresh := r.c.current()
	if fresh == old || fresh.Store.ID != nil || r.devices.made.Load() != 1 {
		t.Fatalf("the next connection reuses the logged-out client (%v), its device is paired (%v), or %d devices were made", fresh == old, fresh.Store.ID, r.devices.made.Load())
	}
	if !r.c.dispatch(oldGen, &events.Connected{}) {
		t.Fatal("an event of the replaced client failed")
	}
	if got := r.events(); len(got) != 1 {
		t.Fatalf("an event of the replaced client reached the engine: %v", got)
	}
}

func TestLogoutRemovesTheLocalDeviceWhenTheServerCannotBeReached(t *testing.T) {
	w, _ := newWriter()
	logger := logx.New(w, slog.LevelInfo)
	sess, err := session.Open(t.Context(), session.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: "local", Logger: logger})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	device := sess.NewDevice()
	id := types.JID{User: peer.User, Device: 12, Server: types.DefaultUserServer}
	device.ID = &id
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{1},
		AccountSignature:    bytes.Repeat([]byte{2}, 64),
		AccountSignatureKey: bytes.Repeat([]byte{3}, 32),
		DeviceSignature:     bytes.Repeat([]byte{4}, 64),
	}
	if err := device.Save(t.Context()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stored, err := sess.Device(t.Context())
	if err != nil || stored.ID == nil {
		t.Fatalf("the saved device was not read back: %v", err)
	}
	alerts, _ := newLogger(slog.LevelWarn)
	c, err := newClient(Options{Device: stored, Devices: sess, OwnerPhone: ownerPhone, HistoryMaxBytes: 1 << 20, Writer: w, Alerts: alerts}, refusingDial(t), noDownload, time.Now)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if !c.Paired() || c.Account() != peer.User {
		t.Fatalf("the stored device reports Paired %v, Account %q", c.Paired(), c.Account())
	}
	if err := c.Logout(t.Context()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if c.Paired() || c.Account() != "" {
		t.Fatal("the device is still reported after Logout")
	}
	after, err := sess.Device(t.Context())
	if err != nil || after.ID != nil {
		t.Fatalf("session.db still holds a device after Logout: %v, %v", after.ID, err)
	}
	if _, _, cancel := c.prepare(); cancel != nil {
		cancel()
	}
	if c.current().Store.ID != nil || c.current().Store.Deleted {
		t.Fatal("the client after Logout does not hold a fresh device")
	}
}

func TestTheEventHandlerReportsTheEnginesAnswer(t *testing.T) {
	r := newRig(t, pairedDevice())
	msg := textMessage(t, peer, peer, "3EB0A1", "synthetic text")
	if !r.dispatch(msg) {
		t.Fatal("a message the engine accepted counts as a failed handler")
	}
	r.setAck(false)
	if r.dispatch(msg) {
		t.Fatal("a message the engine refused does not count as a failed handler, so it would be acknowledged")
	}
	r.c.OnEvent(nil)
	if r.dispatch(msg) {
		t.Fatal("a message without an engine handler was acknowledged")
	}
	r.setAck(true)
	if !r.dispatch(&events.Receipt{}) {
		t.Fatal("an event the adapter ignores counts as a failed handler")
	}
}

func TestAPanicInTheEventHandlerIsRecoveredAndRefusesTheEvent(t *testing.T) {
	logger, logs := newLogger(slog.LevelInfo)
	reg := metrics.NewRegistry()
	safego.Install(logger, reg)
	r := newRig(t, pairedDevice())
	r.c.OnEvent(func(engine.Event) bool { panic("SYNTHETIC-PANIC from 15550100001@s.whatsapp.net") })
	if r.dispatch(textMessage(t, peer, peer, "3EB0P1", "synthetic text")) {
		t.Fatal("a message whose handling panicked was acknowledged")
	}
	if got := logs.events("panic"); len(got) != 1 || got[0]["name"] != "wa.event" || got[0]["panic_type"] != "string" {
		t.Fatalf("panic events %v, want one for wa.event naming only the value's type", got)
	}
	if strings.Contains(logs.String(), "SYNTHETIC-PANIC") || strings.Contains(logs.String(), "15550100001") {
		t.Fatalf("the panic value reached the log:\n%s", logs.String())
	}
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(b.String(), `wawarden_panics_total{name="wa.event"} 1`+"\n") {
		t.Fatalf("the panic was not counted:\n%s", b.String())
	}
}

func TestADeadConnectionIsDroppedAndReported(t *testing.T) {
	r := newRig(t, pairedDevice())
	_, _, cancel := r.c.prepare()
	defer cancel()
	r.dispatch(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: time.Now()})
	r.dispatch(&events.KeepAliveTimeout{ErrorCount: 9, LastSuccess: time.Now().Add(-whatsmeow.KeepAliveMaxFailTime - time.Second)})
	r.dispatch(&events.KeepAliveTimeout{ErrorCount: 10, LastSuccess: time.Now().Add(-whatsmeow.KeepAliveMaxFailTime - time.Second)})
	deadline := time.Now().Add(5 * time.Second)
	for len(r.events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.events(); len(got) != 1 || got[0] != (engine.Disconnected{}) {
		t.Fatalf("engine events %v, want one Disconnected after keepalives failed for longer than the library's limit", got)
	}
}

func TestTheProtocolVersionIsSetAndRead(t *testing.T) {
	r := newRig(t, &store.Device{})
	saved := r.c.Version()
	t.Cleanup(func() { r.c.SetVersion(saved) })
	if saved.IsZero() {
		t.Fatal("the library's built-in version is zero")
	}
	want := engine.Version{2, 3000, saved[2] + 1}
	r.c.SetVersion(want)
	if got := r.c.Version(); got != want {
		t.Fatalf("Version after SetVersion = %v, want %v", got, want)
	}
}

func TestDownloadHistoryStagesThroughTheCap(t *testing.T) {
	ref := engine.HistoryRef{
		ID: "3EB0H1", DirectPath: "/v/t62.7118-24/synthetic", MediaKey: bytes.Repeat([]byte{3}, 32),
		FileSHA256: bytes.Repeat([]byte{1}, 32), FileEncSHA256: bytes.Repeat([]byte{2}, 32), FileLength: 16,
	}
	payload := bytes.Repeat([]byte("h"), 16)
	r := newRig(t, pairedDevice())
	var seen *waE2E.HistorySyncNotification
	r.c.download = func(_ context.Context, _ *whatsmeow.Client, notif *waE2E.HistorySyncNotification, f whatsmeow.File) error {
		seen = notif
		_, err := io.Copy(f, bytes.NewReader(payload))
		return err
	}
	var out bytes.Buffer
	if err := r.c.DownloadHistory(t.Context(), ref, &out); err != nil {
		t.Fatalf("DownloadHistory: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) || seen.GetDirectPath() != ref.DirectPath || !bytes.Equal(seen.GetFileEncSHA256(), ref.FileEncSHA256) {
		t.Fatalf("downloaded %q from %v", out.Bytes(), seen)
	}

	r.c.download = func(_ context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification, f whatsmeow.File) error {
		_, err := io.Copy(f, bytes.NewReader(make([]byte, (1<<20)+mediaOverhead+1)))
		return err
	}
	out.Reset()
	if err := r.c.DownloadHistory(t.Context(), ref, &out); !errors.Is(err, errTooLarge) || out.Len() != 0 {
		t.Fatalf("an oversized download = %v with %d bytes written, want errTooLarge and nothing", err, out.Len())
	}
	r.c.download = func(_ context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification, f whatsmeow.File) error {
		_, err := f.Write(make([]byte, (1<<20)+1))
		return err
	}
	if err := r.c.DownloadHistory(t.Context(), ref, &out); !errors.Is(err, errTooLarge) || out.Len() != 0 {
		t.Fatalf("a decrypted blob over the cap = %v with %d bytes written, want errTooLarge and nothing", err, out.Len())
	}

	called := false
	r.c.download = func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification, whatsmeow.File) error {
		called = true
		return nil
	}
	for name, change := range map[string]func(*engine.HistoryRef){
		"announced over the cap": func(r *engine.HistoryRef) { r.FileLength = (1 << 20) + 1 },
		"a relative path":        func(r *engine.HistoryRef) { r.DirectPath = "v/t62/synthetic" },
		"no media key":           func(r *engine.HistoryRef) { r.MediaKey = nil },
		"a short hash":           func(r *engine.HistoryRef) { r.FileEncSHA256 = r.FileEncSHA256[:31] },
		"no plaintext hash":      func(r *engine.HistoryRef) { r.FileSHA256 = nil },
	} {
		bad := ref
		change(&bad)
		if err := r.c.DownloadHistory(t.Context(), bad, &out); err == nil || called {
			t.Errorf("a reference with %s was downloaded (%v)", name, err)
		}
	}
}

func TestTheProductionDownloaderNeedsAConnection(t *testing.T) {
	r := newRig(t, pairedDevice())
	staged := newCapFile(1<<20, 0)
	err := downloadToFile(t.Context(), r.c.current(), &waE2E.HistorySyncNotification{
		DirectPath: proto.String("/v/t62.7118-24/synthetic"), MediaKey: bytes.Repeat([]byte{3}, 32),
		FileSHA256: bytes.Repeat([]byte{1}, 32), FileEncSHA256: bytes.Repeat([]byte{2}, 32),
	}, staged)
	if err == nil || len(staged.bytes()) != 0 {
		t.Fatalf("a download without a connection = %v, %d bytes", err, len(staged.bytes()))
	}
}
