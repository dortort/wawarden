package engine

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	current = Version{2, 3000, 100}
	newer   = Version{2, 3000, 200}
	older   = Version{2, 3000, 50}
)

type supRig struct {
	*rig
	s *supervisor
}

func newSupRig(t *testing.T, options ...option) *supRig {
	t.Helper()
	r := &supRig{rig: newRig(t, options...)}
	r.s = newSupervisor(r.opts)
	r.s.ctx = t.Context()
	r.client.OnEvent(func(ev Event) bool {
		r.s.handle(ev)
		return true
	})
	return r
}

func (h *supRig) versions(results ...versionResult) *fakeVersions {
	v := &fakeVersions{results: results}
	h.s.versions = v
	return v
}

func (h *supRig) steps() int {
	n := 0
	for h.s.step(h.t.Context()) {
		n++
		if n > 100 {
			h.t.Fatal("the supervisor kept stepping")
		}
	}
	return n
}

func (h *supRig) want(state State, reason Reason) {
	h.t.Helper()
	if st := h.s.status(); st.State != state || st.Reason != reason {
		h.t.Fatalf("status %+v, want %s(%s)", st, state, reason)
	}
}

func (h *supRig) deliver(evs ...Event) {
	for _, ev := range evs {
		h.s.handle(ev)
	}
}

func (h *supRig) connectedNow() {
	h.t.Helper()
	h.versions(versionResult{v: current})
	h.s.begin(h.t.Context(), 1)
	h.steps()
	h.deliver(Connected{})
	h.want(StateConnected, "")
}

func TestStartRefreshesTheVersionThenConnects(t *testing.T) {
	h := newSupRig(t)
	h.versions(versionResult{v: newer})
	h.s.begin(t.Context(), 1)
	h.want(StateConnecting, "")
	h.steps()
	if got, want := h.client.history(), []string{"disconnect", "set_version", "connect"}; !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if h.client.Version() != newer {
		t.Fatalf("version %v, want %v", h.client.Version(), newer)
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
	if h.counter("wawarden_paired") != 1 || h.counter("wawarden_connected") != 1 {
		t.Fatal("the Paired and Connected gauges are not 1 while connected")
	}
	if len(h.logs.events("version_updated")) != 1 {
		t.Fatalf("no version_updated event: %s", h.logs)
	}
}

func TestStartKeepsAnEqualVersion(t *testing.T) {
	h := newSupRig(t)
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), 1)
	h.steps()
	if got := h.client.history(); !slices.Equal(got, []string{"connect"}) {
		t.Fatalf("calls %v, want only a connect", got)
	}
}

func TestStartLeavesAnUnpairedClientUnpaired(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	h.versions(versionResult{v: newer})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateUnpaired, "")
	if h.client.count("connect") != 0 {
		t.Fatal("an unpaired client was connected without admin pair")
	}
	if h.counter("wawarden_paired") != 0 || h.counter("wawarden_connected") != 0 {
		t.Fatal("gauges are not 0 while unpaired")
	}
}

func TestVersionRefreshRetriesThenStaysOutdated(t *testing.T) {
	h := newSupRig(t)
	v := h.versions(versionResult{err: errors.New("synthetic: offline")})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateDisconnected, ReasonOutdated)
	if v.count() != 4 || !slices.Equal(h.clock.slept(), []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("%d fetches after waits %v, want 4 after 1s, 2s and 4s", v.count(), h.clock.slept())
	}
	if h.client.count("connect") != 0 || h.client.count("set_version") != 0 {
		t.Fatalf("calls %v, want none", h.client.history())
	}
	if a := h.alerts("disconnected"); len(a) != 1 || a[0]["reason"] != "outdated" || a[0]["level"] != "WARN" {
		t.Fatalf("disconnected alerts %v", a)
	}
	if len(h.logs.events("version_refresh_failed")) != 4 {
		t.Fatalf("version_refresh_failed events: %s", h.logs)
	}
	if h.steps() != 0 {
		t.Fatal("the supervisor retried on its own after giving up")
	}
}

func TestVersionRefreshRecoversAfterAFailure(t *testing.T) {
	h := newSupRig(t)
	h.versions(versionResult{err: errors.New("synthetic: offline")}, versionResult{v: newer})
	h.s.begin(t.Context(), 1)
	h.steps()
	if !slices.Equal(h.clock.slept(), []time.Duration{time.Second}) || h.client.count("connect") != 1 || h.client.Version() != newer {
		t.Fatalf("waits %v calls %v", h.clock.slept(), h.client.history())
	}
}

func TestOutdatedClientRefusesTheSameOrALowerVersion(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	h.versions(versionResult{v: current}, versionResult{v: older}, versionResult{v: Version{}})
	h.deliver(ClientOutdated{})
	h.want(StateConnecting, "")
	h.steps()
	h.want(StateDisconnected, ReasonOutdated)
	if h.client.count("set_version") != 0 || h.client.Version() != current {
		t.Fatalf("calls %v, version %v", h.client.history(), h.client.Version())
	}
}

func TestOutdatedClientTakesANewerVersionWhileDisconnected(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	h.versions(versionResult{v: newer})
	h.deliver(ClientOutdated{})
	h.steps()
	calls := h.client.history()
	if slices.Contains(calls, "set_version_while_connected") {
		t.Fatalf("the version was set while connected: %v", calls)
	}
	i := slices.Index(calls, "set_version")
	if i < 1 || calls[i-1] != "disconnect" || calls[len(calls)-1] != "connect" {
		t.Fatalf("calls %v, want a disconnect, the version, then a connect", calls)
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
}

func TestPermanentDisconnectsWaitForTheAdmin(t *testing.T) {
	for _, tt := range []struct {
		event  Event
		reason Reason
	}{
		{StreamReplaced{}, ReasonReplaced},
		{TemporaryBan{Expire: time.Hour}, ReasonTemporaryBan},
		{CATRefreshFailed{}, ReasonCATRefresh},
		{ConnectFailure{Code: 409}, ReasonConnectFailure},
		{LoggedOut{}, ReasonLoggedOut},
	} {
		t.Run(string(tt.reason), func(t *testing.T) {
			h := newSupRig(t)
			h.connectedNow()
			connects := h.client.count("connect")
			if tt.reason == ReasonLoggedOut {
				h.client.setPaired(false)
			}
			h.deliver(tt.event, Disconnected{}, Connected{}, Disconnected{})
			h.want(StateDisconnected, tt.reason)
			if h.steps() != 0 || h.client.count("connect") != connects {
				t.Fatalf("the supervisor reconnected on its own: %v", h.client.history())
			}
			if a := h.alerts("disconnected"); len(a) != 1 || a[0]["reason"] != string(tt.reason) {
				t.Fatalf("disconnected alerts %v", a)
			}
			if h.counter("wawarden_connected") != 0 {
				t.Fatal("the Connected gauge is not 0")
			}
			err := h.s.reconnect()
			if tt.reason == ReasonLoggedOut {
				if !errors.Is(err, ErrNotPaired) {
					t.Fatalf("Reconnect after logout = %v, want ErrNotPaired", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconnect = %v", err)
			}
			h.steps()
			h.deliver(Connected{})
			h.want(StateConnected, "")
			if h.client.count("connect") != connects+1 {
				t.Fatalf("calls %v, want one more connect", h.client.history())
			}
		})
	}
}

func TestLoggedOutWinsInEitherOrder(t *testing.T) {
	for _, order := range [][]Event{{LoggedOut{}, StreamReplaced{}}, {StreamReplaced{}, LoggedOut{}}, {Disconnected{}, LoggedOut{}}} {
		h := newSupRig(t)
		h.connectedNow()
		h.deliver(order...)
		h.want(StateDisconnected, ReasonLoggedOut)
		if h.steps() != 0 {
			t.Fatalf("%T: a step ran after the logout", order)
		}
	}
}

func TestOrdinaryDropsReconnectWithCappedBackoff(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	failures := make([]error, 12)
	for i := range failures {
		failures[i] = errors.New("synthetic: unreachable")
	}
	h.client.mu.Lock()
	h.client.connectErrs = failures
	h.client.mu.Unlock()
	h.deliver(Disconnected{})
	h.want(StateConnecting, "")
	h.steps()
	var want []time.Duration
	for _, d := range []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300, 300, 300} {
		want = append(want, d*time.Second/2)
	}
	if got := h.clock.slept(); !slices.Equal(got, want) {
		t.Fatalf("waits %v, want %v", got, want)
	}
	if len(h.logs.events("connect_failed")) != 12 {
		t.Fatalf("connect_failed events: %d", len(h.logs.events("connect_failed")))
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
	h.deliver(Disconnected{})
	before := len(h.clock.slept())
	h.steps()
	if got := h.clock.slept()[before:]; !slices.Equal(got, []time.Duration{time.Second}) {
		t.Fatalf("after a success the backoff restarted at %v, want 1s", got)
	}
}

func TestBackoffStaysWithinItsBounds(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a fixed seed makes the property test reproducible
	for range 2000 {
		attempt := 1 + r.IntN(80)
		pick := r.Int64N(4)
		s := &supervisor{jitter: func(d time.Duration) time.Duration {
			switch pick {
			case 0:
				return -d
			case 1:
				return 2 * d
			case 2:
				return d
			}
			return time.Duration(r.Int64N(int64(d) + 1))
		}}
		ceiling := reconnectBase
		for i := 1; i < attempt && ceiling < reconnectCap; i++ {
			ceiling *= 2
		}
		ceiling = min(ceiling, reconnectCap)
		if d := s.backoff(attempt); d < ceiling/2 || d >= ceiling {
			t.Fatalf("backoff(%d) = %v, want within [%v, %v)", attempt, d, ceiling/2, ceiling)
		}
	}
}

func TestAConnectionThatLostItsRaceIsClosed(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	h.deliver(Disconnected{})
	h.client.onConnect = func() { h.client.emit(StreamReplaced{}) }
	h.steps()
	h.want(StateDisconnected, ReasonReplaced)
	if h.client.isConnected() {
		t.Fatalf("a connection that completed after StreamReplaced stays open: %v", h.client.history())
	}
}

func (h *supRig) emitDuringFirstConnect(evs ...Event) {
	var once sync.Once
	h.client.onConnect = func() {
		once.Do(func() {
			for _, ev := range evs {
				h.client.emit(ev)
			}
		})
	}
}

func TestADropWhileConnectingReconnects(t *testing.T) {
	h := newSupRig(t)
	h.emitDuringFirstConnect(Disconnected{})
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), 1)
	h.steps()
	if h.client.count("connect") != 2 || !slices.Equal(h.clock.slept(), []time.Duration{time.Second}) {
		t.Fatalf("calls %v waits %v, want a second connect after the backoff", h.client.history(), h.clock.slept())
	}
	h.want(StateConnecting, "")
	h.deliver(Connected{})
	h.want(StateConnected, "")
}

func TestADropWhileConnectingReconnectsInTheRunLoop(t *testing.T) {
	h := newSupRig(t)
	clock := newGatedClock()
	h.s.clock = clock
	h.emitDuringFirstConnect(Disconnected{})
	h.versions(versionResult{v: current})
	h.s.start(t.Context(), 1)
	h.client.waitFor(t, "connect")
	eventually(t, "the reconnect waits for its backoff", func() bool { return clock.waiting() == 1 })
	clock.advance(time.Second)
	h.client.waitFor(t, "connect")
	h.client.emit(Connected{})
	eventually(t, "the second connection is connected", func() bool { return h.s.status().State == StateConnected })
}

func TestConnectFailuresRetry(t *testing.T) {
	h := newSupRig(t)
	h.client.connectErrs = []error{errors.New("synthetic"), errors.New("synthetic"), nil}
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), 1)
	h.steps()
	if h.client.count("connect") != 3 || !slices.Equal(h.clock.slept(), []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("calls %v waits %v", h.client.history(), h.clock.slept())
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
}

func TestRestartBudgetStartsDisconnected(t *testing.T) {
	h := newSupRig(t)
	v := h.versions(versionResult{v: newer})
	h.s.begin(t.Context(), MaxRecentStarts+1)
	h.want(StateDisconnected, ReasonRestartBudget)
	if h.steps() != 0 || v.count() != 0 || len(h.client.history()) != 0 {
		t.Fatalf("calls %v, fetches %d", h.client.history(), v.count())
	}
	if a := h.alerts("disconnected"); len(a) != 1 || a[0]["reason"] != "restart_budget" {
		t.Fatalf("disconnected alerts %v", a)
	}
	if err := h.s.reconnect(); err != nil {
		t.Fatalf("Reconnect = %v", err)
	}
	h.steps()
	if h.client.count("connect") != 1 {
		t.Fatalf("calls %v", h.client.history())
	}

	h = newSupRig(t)
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), MaxRecentStarts)
	h.steps()
	if h.client.count("connect") != 1 {
		t.Fatalf("five starts in the window refused the connect: %v", h.client.history())
	}
}

func TestPairingRefusals(t *testing.T) {
	h := newSupRig(t)
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("Pair while paired = %v, want ErrAlreadyPaired", err)
	}
	for _, phone := range []string{"", "15550100009", "+0155501000", "+155", "+1555010000912345", "+1555O100009"} {
		h := newSupRig(t, withOwner(phone))
		h.client.setPaired(false)
		if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrOwnerPhoneMissing) {
			t.Fatalf("Pair with owner phone %q = %v, want ErrOwnerPhoneMissing", phone, err)
		}
		if len(h.client.history()) != 0 {
			t.Fatalf("calls %v", h.client.history())
		}
	}
}

func TestPairingIsRateLimited(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	for i := range 3 {
		code, err := h.s.pair(t.Context())
		if err != nil || code != "SYNT-HETC" {
			t.Fatalf("attempt %d = %q, %v", i+1, code, err)
		}
		h.clock.advance(10 * time.Minute)
	}
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairRateLimited) {
		t.Fatalf("fourth attempt = %v, want ErrPairRateLimited", err)
	}
	h.clock.advance(31 * time.Minute)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("after the first attempt left the window: %v", err)
	}
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairRateLimited) {
		t.Fatalf("a fifth attempt within the hour = %v", err)
	}
	if h.client.count("connect") != 1 || h.client.count("pair:15550100009") != 4 {
		t.Fatalf("calls %v, want one connect and four pairing requests for the owner", h.client.history())
	}
	if strings.Contains(h.logs.String(), "SYNT-HETC") {
		t.Fatal("the pairing code reached the log")
	}
}

func TestPairingFailureIsFixed(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	h.client.pairErr = errors.New("synthetic: 15550100009 refused")
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairFailed) || strings.Contains(err.Error(), "15550100009") {
		t.Fatalf("Pair = %v, want ErrPairFailed without the client's text", err)
	}
	if strings.Contains(h.logs.String(), "15550100009") {
		t.Fatal("the owner's number reached the log")
	}
	h = newSupRig(t)
	h.client.setPaired(false)
	h.client.connectErrs = []error{errors.New("synthetic")}
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairFailed) {
		t.Fatalf("Pair = %v, want ErrPairFailed", err)
	}
	h.want(StateUnpaired, "")
}

func TestPairedAccountMustBeTheOwner(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.client.setPaired(true)
	h.deliver(Paired{JID: "15550100002:3@s.whatsapp.net"})
	h.client.waitFor(t, "logout")
	h.want(StateUnpaired, "")
	if a := h.alerts("pair_rejected"); len(a) != 1 || a[0]["stage"] != "after_pairing" {
		t.Fatalf("pair_rejected alerts %v", a)
	}
	if strings.Contains(h.logs.String(), "15550100002") {
		t.Fatal("the rejected account reached the log")
	}

	h = newSupRig(t)
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.deliver(PairRejected{})
	h.want(StateUnpaired, "")
	if a := h.alerts("pair_rejected"); len(a) != 1 || a[0]["stage"] != "before_save" {
		t.Fatalf("pair_rejected alerts %v", a)
	}

	h = newSupRig(t)
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.client.setPaired(true)
	h.deliver(Paired{JID: "15550100009:12@s.whatsapp.net"}, Connected{})
	h.want(StateConnected, "")
	if h.client.count("logout") != 0 || len(h.alerts("pair_rejected")) != 0 {
		t.Fatalf("the owner's own pairing was rejected: %v", h.client.history())
	}
}

func TestUnpairedDropDuringPairingReturnsToUnpaired(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.want(StateConnecting, "")
	h.deliver(Disconnected{})
	h.want(StateUnpaired, "")
	if h.steps() != 0 {
		t.Fatal("an unpaired client is reconnected")
	}
}

func TestReconnect(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	if err := h.s.reconnect(); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("Reconnect while connected = %v", err)
	}
	h.versions(versionResult{err: errors.New("synthetic")})
	h.deliver(ClientOutdated{})
	h.steps()
	h.want(StateDisconnected, ReasonOutdated)
	h.versions(versionResult{v: newer})
	if err := h.s.reconnect(); err != nil {
		t.Fatalf("Reconnect when outdated = %v", err)
	}
	h.steps()
	if h.client.Version() != newer || h.client.history()[len(h.client.history())-1] != "connect" {
		t.Fatalf("calls %v", h.client.history())
	}
	h.client.setPaired(false)
	h.deliver(LoggedOut{})
	if err := h.s.reconnect(); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Reconnect when unpaired = %v", err)
	}
}

func TestStopEndsEveryTransition(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	h.s.stop()
	h.want(StateDisconnected, ReasonShutdown)
	h.deliver(Connected{}, StreamReplaced{}, Disconnected{})
	h.want(StateDisconnected, ReasonShutdown)
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Pair after stop = %v", err)
	}
	if err := h.s.reconnect(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Reconnect after stop = %v", err)
	}
	if len(h.alerts("disconnected")) != 0 || h.client.history()[len(h.client.history())-1] != "disconnect" {
		t.Fatalf("alerts %v calls %v", h.alerts("disconnected"), h.client.history())
	}
}

func permutations(events []Event) [][]Event {
	var out [][]Event
	var perms func([]Event, int)
	perms = func(evs []Event, k int) {
		if k == len(evs) {
			out = append(out, slices.Clone(evs))
			return
		}
		for i := k; i < len(evs); i++ {
			evs[k], evs[i] = evs[i], evs[k]
			perms(evs, k+1)
			evs[k], evs[i] = evs[i], evs[k]
		}
	}
	perms(slices.Clone(events), 0)
	return out
}

func TestOutOfOrderEventsKeepTheStickyState(t *testing.T) {
	orders := permutations([]Event{StreamReplaced{}, Disconnected{}, Disconnected{}, Connected{}})
	if len(orders) != 24 {
		t.Fatalf("%d orders tried", len(orders))
	}
	for _, evs := range orders {
		h := newSupRig(t)
		h.connectedNow()
		h.deliver(evs...)
		h.steps()
		h.want(StateDisconnected, ReasonReplaced)
		if h.client.isConnected() || h.steps() != 0 {
			t.Fatalf("order %v reconnected after StreamReplaced: %v", fmt.Sprint(evs), h.client.history())
		}
	}
}

func TestEveryDropIsFollowedByAConnect(t *testing.T) {
	for _, events := range [][]Event{{Disconnected{}, Connected{}}, {Disconnected{}, Disconnected{}, Connected{}}, {Disconnected{}, Connected{}, Connected{}}} {
		for _, evs := range permutations(events) {
			h := newSupRig(t)
			h.connectedNow()
			h.deliver(evs...)
			h.steps()
			h.want(StateConnecting, "")
			if h.client.count("connect") != 2 || h.counter("wawarden_connected") != 0 {
				t.Fatalf("order %v: calls %v, connected gauge %v, want a second connect and the gauge at 0", fmt.Sprint(evs), h.client.history(), h.counter("wawarden_connected"))
			}
			h.deliver(Connected{})
			h.want(StateConnected, "")
		}
	}
}

func TestALateConnectedDuringTheBackoffIsIgnored(t *testing.T) {
	h := newSupRig(t)
	clock := newGatedClock()
	h.s.clock = clock
	h.versions(versionResult{v: current})
	h.s.start(t.Context(), 1)
	h.client.waitFor(t, "connect")
	h.client.emit(Connected{})
	eventually(t, "the engine is connected", func() bool { return h.s.status().State == StateConnected })
	h.client.emit(Disconnected{})
	eventually(t, "the reconnect waits for its backoff", func() bool { return clock.waiting() == 1 })
	h.client.emit(Connected{})
	h.want(StateConnecting, "")
	clock.advance(time.Second)
	h.client.waitFor(t, "connect")
	if h.counter("wawarden_connected") != 0 {
		t.Fatal("the Connected gauge is 1 while the engine reconnects")
	}
	h.client.emit(Connected{})
	eventually(t, "the new connection is connected", func() bool { return h.s.status().State == StateConnected })
}

func TestConnectedBeforeConnectReturnsCounts(t *testing.T) {
	h := newSupRig(t)
	h.emitDuringFirstConnect(Connected{})
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateConnected, "")
	if h.client.count("connect") != 1 || h.counter("wawarden_connected") != 1 {
		t.Fatalf("calls %v, connected gauge %v", h.client.history(), h.counter("wawarden_connected"))
	}

	h = newSupRig(t)
	h.emitDuringFirstConnect(Connected{}, Disconnected{})
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateConnecting, "")
	if h.client.count("connect") != 2 {
		t.Fatalf("calls %v, want a reconnect after the drop", h.client.history())
	}
}

func TestConcurrentEventsAndCommands(t *testing.T) {
	h := newSupRig(t)
	h.versions(versionResult{v: current})
	h.s.start(t.Context(), 1)
	h.client.waitFor(t, "connect")
	all := []Event{Connected{}, Disconnected{}, StreamReplaced{}, TemporaryBan{}, ClientOutdated{}, CATRefreshFailed{}, ConnectFailure{}, PairRejected{}}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(g), 7)) //nolint:gosec // G404: a fixed seed makes the interleaving test reproducible
			for range 200 {
				switch n := r.IntN(len(all) + 2); {
				case n < len(all):
					h.client.emit(all[n])
				case n == len(all):
					_ = h.s.reconnect()
				default:
					_ = h.s.status()
				}
			}
		}()
	}
	wg.Wait()
	h.s.stop()
	h.want(StateDisconnected, ReasonShutdown)
	if h.counter("wawarden_connected") != 0 {
		t.Fatal("the Connected gauge is not 0 after stop")
	}
}
