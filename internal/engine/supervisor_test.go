package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
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
	v := h.versions(versionResult{v: newer})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateUnpaired, "")
	if h.client.count("connect") != 0 || v.count() != 0 || len(h.client.history()) != 0 {
		t.Fatalf("an unpaired client made calls %v and %d version fetches without admin pair: it must make no connection at all", h.client.history(), v.count())
	}
	if h.counter("wawarden_paired") != 0 || h.counter("wawarden_connected") != 0 {
		t.Fatal("gauges are not 0 while unpaired")
	}
	if u := h.alerts("unpaired"); len(u) != 1 || u[0]["level"] != "WARN" {
		t.Fatalf("unpaired events %v, want exactly one warning", u)
	}
	if s := h.logs.events("engine_state"); len(s) != 0 {
		t.Fatalf("engine_state events %v: the engine starts unpaired, so its state does not change", s)
	}

	budget := newSupRig(t)
	budget.client.setPaired(false)
	bv := budget.versions(versionResult{v: newer})
	budget.s.begin(t.Context(), MaxRecentStarts+1)
	budget.steps()
	budget.want(StateDisconnected, ReasonRestartBudget)
	if u := budget.alerts("unpaired"); len(u) != 1 || u[0]["level"] != "WARN" {
		t.Fatalf("unpaired events %v at a start held by the restart budget without a device, want exactly one warning", u)
	}
	if d := budget.alerts("disconnected"); len(d) != 1 || d[0]["reason"] != string(ReasonRestartBudget) {
		t.Fatalf("disconnected events %v, want the restart budget reported too", d)
	}
	if len(budget.client.history()) != 0 || bv.count() != 0 {
		t.Fatalf("calls %v and %d version fetches at a start held by the restart budget", budget.client.history(), bv.count())
	}
	paired := newSupRig(t)
	paired.versions(versionResult{v: current})
	paired.s.begin(t.Context(), 1)
	if len(paired.alerts("unpaired")) != 0 {
		t.Fatal("a paired start reported unpaired")
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
	h.disconnectedLast()
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
	h.disconnectedLast()
}

func (h *supRig) disconnectedLast() {
	h.t.Helper()
	if calls := h.client.history(); len(calls) == 0 || calls[len(calls)-1] != "disconnect" {
		h.t.Fatalf("calls %v: a client left outdated must end disconnected, so that nothing reconnects it behind the supervisor", calls)
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

func TestPermanentDisconnectsWhileConnectingWaitForTheAdmin(t *testing.T) {
	for _, tt := range []struct {
		event  Event
		reason Reason
	}{
		{StreamReplaced{}, ReasonReplaced},
		{TemporaryBan{Expire: time.Hour}, ReasonTemporaryBan},
		{CATRefreshFailed{}, ReasonCATRefresh},
		{ConnectFailure{Code: 409}, ReasonConnectFailure},
	} {
		for _, dialing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dialing=%v", tt.reason, dialing), func(t *testing.T) {
				h := newSupRig(t)
				h.versions(versionResult{v: current})
				if dialing {
					h.emitDuringFirstConnect(tt.event)
				}
				h.s.begin(t.Context(), 1)
				h.steps()
				if !dialing {
					h.want(StateConnecting, "")
					h.deliver(tt.event)
				}
				h.deliver(Connected{}, Disconnected{})
				h.want(StateDisconnected, tt.reason)
				if h.steps() != 0 || h.client.count("connect") != 1 || h.client.isConnected() {
					t.Fatalf("calls %v: want the one connection closed and no other", h.client.history())
				}
				if a := h.alerts("disconnected"); len(a) != 1 || a[0]["reason"] != string(tt.reason) {
					t.Fatalf("disconnected alerts %v", a)
				}
				if h.counter("wawarden_connected") != 0 {
					t.Fatal("the Connected gauge is not 0")
				}
				if err := h.s.reconnect(); err != nil {
					t.Fatalf("Reconnect = %v", err)
				}
				h.steps()
				h.deliver(Connected{})
				h.want(StateConnected, "")
			})
		}
	}
}

func TestOutdatedWhileConnectingRefreshesTheVersion(t *testing.T) {
	for _, dialing := range []bool{false, true} {
		t.Run(fmt.Sprintf("dialing=%v newer", dialing), func(t *testing.T) {
			h := newSupRig(t)
			v := h.versions(versionResult{v: current}, versionResult{v: newer})
			if dialing {
				h.emitDuringFirstConnect(ClientOutdated{})
			}
			h.s.begin(t.Context(), 1)
			h.steps()
			if !dialing {
				h.want(StateConnecting, "")
				h.deliver(ClientOutdated{})
				h.steps()
			}
			calls := h.client.history()
			if v.count() != 2 || h.client.Version() != newer || slices.Contains(calls, "set_version_while_connected") {
				t.Fatalf("%d fetches, version %v, calls %v", v.count(), h.client.Version(), calls)
			}
			i := slices.Index(calls, "set_version")
			if i < 1 || calls[i-1] != "disconnect" || calls[len(calls)-1] != "connect" || h.client.count("connect") != 2 {
				t.Fatalf("calls %v, want a disconnect, the version, then a second connect", calls)
			}
			h.deliver(Connected{})
			h.want(StateConnected, "")
		})
		t.Run(fmt.Sprintf("dialing=%v none newer", dialing), func(t *testing.T) {
			h := newSupRig(t)
			v := h.versions(versionResult{v: current})
			if dialing {
				h.emitDuringFirstConnect(ClientOutdated{})
			}
			h.s.begin(t.Context(), 1)
			h.steps()
			if !dialing {
				h.deliver(ClientOutdated{})
				h.steps()
			}
			h.want(StateDisconnected, ReasonOutdated)
			if v.count() != 5 || h.client.count("connect") != 1 || h.client.count("set_version") != 0 {
				t.Fatalf("%d fetches, calls %v", v.count(), h.client.history())
			}
			h.disconnectedLast()
			if a := h.alerts("disconnected"); len(a) != 1 || a[0]["reason"] != string(ReasonOutdated) {
				t.Fatalf("disconnected alerts %v", a)
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

func TestLosingTheDeviceReportsUnpairedOnce(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	if u := h.alerts("unpaired"); len(u) != 0 {
		t.Fatalf("unpaired events %v while paired", u)
	}
	h.client.setPaired(false)
	h.deliver(LoggedOut{}, StreamReplaced{}, Disconnected{})
	h.want(StateDisconnected, ReasonLoggedOut)
	if u := h.alerts("unpaired"); len(u) != 1 || u[0]["level"] != "WARN" || h.counter("wawarden_paired") != 0 {
		t.Fatalf("unpaired events %v, Paired gauge %v: losing the device reports it once and drops the gauge", u, h.counter("wawarden_paired"))
	}
	h.client.setPaired(true)
	h.deliver(Paired{JID: ownerDev})
	h.client.setPaired(false)
	h.deliver(LoggedOut{})
	if u := h.alerts("unpaired"); len(u) != 2 {
		t.Fatalf("unpaired events %v, want one for each time the device was lost", u)
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
	h.clock.advance(stableAfter)
	h.deliver(Disconnected{})
	before := len(h.clock.slept())
	h.steps()
	if got := h.clock.slept()[before:]; !slices.Equal(got, []time.Duration{time.Second}) {
		t.Fatalf("after a connection that stayed up the backoff restarted at %v, want 1s", got)
	}
}

func TestConnectionsThatDropSoonKeepTheBackoffGrowing(t *testing.T) {
	for _, dialing := range []bool{false, true} {
		t.Run(fmt.Sprintf("connected while dialing=%v", dialing), func(t *testing.T) {
			h := newSupRig(t)
			h.connectedNow()
			if dialing {
				h.client.onConnect = func() { h.client.emit(Connected{}) }
			}
			for range 11 {
				h.clock.advance(stableAfter - time.Millisecond)
				h.deliver(Disconnected{})
				h.want(StateConnecting, "")
				h.steps()
				if !dialing {
					h.deliver(Connected{})
				}
				h.want(StateConnected, "")
			}
			var want []time.Duration
			for _, d := range []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300} {
				want = append(want, d*time.Second/2)
			}
			if got := h.clock.slept(); !slices.Equal(got, want) {
				t.Fatalf("waits %v, want %v", got, want)
			}
			if h.client.count("connect") != 12 {
				t.Fatalf("calls %v", h.client.history())
			}
			h.clock.advance(stableAfter)
			h.deliver(Disconnected{})
			before := len(h.clock.slept())
			h.steps()
			if got := h.clock.slept()[before:]; !slices.Equal(got, []time.Duration{time.Second}) {
				t.Fatalf("after a connection that stayed up a minute the backoff restarted at %v, want 1s", got)
			}
		})
	}
}

func TestTheFirstConnectionAfterPairingStartsTheBackoffAfresh(t *testing.T) {
	h := newSupRig(t)
	h.connectedNow()
	for range 8 {
		h.deliver(Disconnected{})
		h.steps()
		h.deliver(Connected{})
	}
	h.client.setPaired(false)
	h.deliver(LoggedOut{})
	h.want(StateDisconnected, ReasonLoggedOut)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.client.pairAs(ownerDev)
	h.deliver(Paired{JID: ownerDev}, Disconnected{})
	h.want(StateConnecting, "")
	before := len(h.clock.slept())
	h.steps()
	if got := h.clock.slept()[before:]; !slices.Equal(got, []time.Duration{time.Second}) {
		t.Fatalf("the reconnect after pairing waited %v, want the backoff's first 1s", got)
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

func TestBackoffAddsTheJitterToHalfTheDelay(t *testing.T) {
	s := &supervisor{jitter: func(d time.Duration) time.Duration { return d / 3 }}
	for i, secs := range []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300} {
		d := secs * time.Second
		if got, want := s.backoff(i+1), d/2+(d-d/2)/3; got != want {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, want)
		}
	}
	if got, want := s.backoff(50), 150*time.Second+50*time.Second; got != want {
		t.Fatalf("backoff at the cap = %v, want %v", got, want)
	}
}

func TestNewInstallsARandomJitter(t *testing.T) {
	r := newHistRig(t)
	o := r.opts
	o.Jitter, o.Metrics = nil, metrics.NewRegistry()
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := map[time.Duration]bool{}
	for range 100 {
		d := e.sup.backoff(5)
		if d < 16*time.Second || d >= 32*time.Second {
			t.Fatalf("backoff(5) = %v, want within [16s, 32s)", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatalf("100 draws of backoff(5) all gave %v", seen)
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

type panicFirst struct {
	panicked bool
	then     VersionSource
}

func (p *panicFirst) Latest(ctx context.Context) (Version, error) {
	if !p.panicked {
		p.panicked = true
		panic("synthetic canary version source panic")
	}
	return p.then.Latest(ctx)
}

func TestPanicsInTheClientAreFailedAttempts(t *testing.T) {
	h := newSupRig(t)
	h.reportPanics()
	h.s.versions = &panicFirst{then: &fakeVersions{results: []versionResult{{v: current}}}}
	var once sync.Once
	h.client.onConnect = func() { once.Do(func() { panic("synthetic canary connect panic") }) }
	h.s.begin(t.Context(), 1)
	h.steps()
	if h.client.count("connect") != 2 || !slices.Equal(h.clock.slept(), []time.Duration{time.Second, time.Second}) {
		t.Fatalf("calls %v waits %v, want a second fetch and a second connect, each after 1s", h.client.history(), h.clock.slept())
	}
	if len(h.logs.events("version_refresh_failed")) != 1 || len(h.logs.events("connect_failed")) != 1 {
		t.Fatalf("the panics were not failed attempts: %s", h.logs)
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
	if h.counter("wawarden_panics_total", "name", "engine.supervisor") != 2 || strings.Contains(h.logs.String(), "canary") {
		t.Fatalf("panics counted %v; log %s", h.counter("wawarden_panics_total", "name", "engine.supervisor"), h.logs)
	}
}

func recovered(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

func TestPanicsInTheClientWhilePairingAreFailedAttempts(t *testing.T) {
	h := newSupRig(t)
	h.reportPanics()
	h.client.setPaired(false)
	var connectOnce, pairOnce sync.Once
	h.client.onConnect = func() { connectOnce.Do(func() { panic("synthetic canary pairing connect panic") }) }
	h.client.onPair = func() { pairOnce.Do(func() { panic("synthetic canary pairing code panic") }) }
	for _, call := range []string{"Connect", "PairPhone"} {
		var err error
		recovered(func() { _, err = h.s.pair(t.Context()) })
		if !errors.Is(err, ErrPairFailed) {
			t.Fatalf("Pair with a panicking %s = %v, want ErrPairFailed", call, err)
		}
	}
	h.want(StateConnecting, "")
	if code, err := h.s.pair(t.Context()); err != nil || code == "" {
		t.Fatalf("Pair after the panics = %q, %v", code, err)
	}
	if h.client.count("connect") != 2 || h.client.count("pair:15550100009") != 2 {
		t.Fatalf("calls %v, want a second dial after the panicking one and a code from the open connection", h.client.history())
	}
	if len(h.logs.events("pair_failed")) != 2 || h.counter("wawarden_panics_total", "name", "engine.supervisor") != 2 || strings.Contains(h.logs.String(), "canary") {
		t.Fatalf("panics counted %v; log %s", h.counter("wawarden_panics_total", "name", "engine.supervisor"), h.logs)
	}
}

func TestAPanicInTheClientWhileReconnectingStillRedials(t *testing.T) {
	h := newSupRig(t)
	h.reportPanics()
	h.versions(versionResult{v: current})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateConnecting, "")
	var once sync.Once
	h.client.onDisconnect = func() { once.Do(func() { panic("synthetic canary disconnect panic") }) }
	err := errors.New("synthetic: reconnect did not return")
	recovered(func() { err = h.s.reconnect() })
	if err != nil {
		t.Fatalf("Reconnect with a panicking Disconnect = %v", err)
	}
	if h.steps() == 0 || h.client.count("connect") != 2 {
		t.Fatalf("calls %v, want a new dial after the panicking disconnect", h.client.history())
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
	if h.counter("wawarden_panics_total", "name", "engine.supervisor") != 1 {
		t.Fatalf("panics counted %v", h.counter("wawarden_panics_total", "name", "engine.supervisor"))
	}
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
	if u := h.alerts("unpaired"); len(u) != 0 {
		t.Fatalf("unpaired alerts %v at a start held by the restart budget with a device", u)
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

func TestFailedPairingAttemptsCountTowardTheLimit(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	h.client.connectErrs = []error{errors.New("synthetic: unreachable")}
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairFailed) {
		t.Fatalf("Pair with a failing connect = %v, want ErrPairFailed", err)
	}
	h.client.pairErr = errors.New("synthetic: refused")
	for range 2 {
		if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairFailed) {
			t.Fatalf("Pair with a failing request = %v, want ErrPairFailed", err)
		}
	}
	h.client.pairErr = nil
	if _, err := h.s.pair(t.Context()); !errors.Is(err, ErrPairRateLimited) {
		t.Fatalf("a fourth attempt after three failures = %v, want ErrPairRateLimited", err)
	}
	if h.client.count("connect") != 2 || h.client.count("pair:15550100009") != 2 {
		t.Fatalf("calls %v", h.client.history())
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
	h.client.pairAs(bobDev)
	h.deliver(Paired{JID: bobDev})
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
	h.client.pairAs(ownerDev)
	h.deliver(Paired{JID: ownerDev}, Connected{})
	h.want(StateConnected, "")
	if h.client.count("logout") != 0 || len(h.alerts("pair_rejected")) != 0 {
		t.Fatalf("the owner's own pairing was rejected: %v", h.client.history())
	}
}

func TestPairingEventsOutOfOrder(t *testing.T) {
	t.Run("a late PairRejected leaves the owner's device connected", func(t *testing.T) {
		h := newSupRig(t)
		h.client.oneSocket = true
		h.connectedNow()
		h.deliver(PairRejected{})
		h.want(StateConnected, "")
		if !h.s.accepting() || len(h.alerts("pair_rejected")) != 0 || h.counter("wawarden_connected") != 1 {
			t.Fatalf("accepting %v, pair_rejected alerts %v", h.s.accepting(), h.alerts("pair_rejected"))
		}
		if err := h.s.reconnect(); !errors.Is(err, ErrAlreadyConnected) {
			t.Fatalf("Reconnect = %v, want ErrAlreadyConnected", err)
		}
		if h.steps() != 0 || h.client.count("connect") != 1 {
			t.Fatalf("calls %v", h.client.history())
		}
	})
	t.Run("a late PairRejected leaves a stored device waiting for the admin", func(t *testing.T) {
		h := newSupRig(t)
		h.connectedNow()
		h.deliver(StreamReplaced{}, PairRejected{})
		h.want(StateDisconnected, ReasonReplaced)
		if len(h.alerts("pair_rejected")) != 0 {
			t.Fatalf("pair_rejected alerts %v", h.alerts("pair_rejected"))
		}
	})
	for _, order := range [][]Event{{Paired{JID: ownerDev}, Connected{}}, {Connected{}, Paired{JID: ownerDev}}} {
		t.Run(fmt.Sprintf("pairing then %T and %T", order[0], order[1]), func(t *testing.T) {
			h := newSupRig(t)
			h.client.setPaired(false)
			if _, err := h.s.pair(t.Context()); err != nil {
				t.Fatalf("Pair = %v", err)
			}
			h.client.pairAs(ownerDev)
			h.deliver(order...)
			h.want(StateConnected, "")
			if !h.s.accepting() || h.counter("wawarden_connected") != 1 || h.steps() != 0 {
				t.Fatalf("accepting %v, connected gauge %v, calls %v", h.s.accepting(), h.counter("wawarden_connected"), h.client.history())
			}
		})
	}
	t.Run("Paired for the owner while connected", func(t *testing.T) {
		h := newSupRig(t)
		h.connectedNow()
		h.deliver(Paired{JID: ownerDev})
		h.want(StateConnected, "")
		if h.counter("wawarden_connected") != 1 || h.steps() != 0 || h.client.count("connect") != 1 {
			t.Fatalf("connected gauge %v, calls %v", h.counter("wawarden_connected"), h.client.history())
		}
	})
}

func (f *fakeClient) failLogouts(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logoutErrs = nil
	for range n {
		f.logoutErrs = append(f.logoutErrs, errors.New("synthetic: error sending logout request"))
	}
}

func TestARejectedDeviceIsLoggedOutUntilItIsGone(t *testing.T) {
	h := newSupRig(t)
	clock := newGatedClock()
	h.s.clock = clock
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.client.failLogouts(2)
	h.client.pairAs(bobDev)
	h.deliver(Paired{JID: bobDev})
	eventually(t, "the first logout failed and its retry waits", func() bool { return clock.waiting() == 1 })
	if st := h.s.status(); st != (Status{State: StateUnpaired, Paired: true}) {
		t.Fatalf("status %+v while the rejected device is still stored", st)
	}
	if err := h.s.reconnect(); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Reconnect with a rejected device = %v, want ErrNotPaired", err)
	}
	h.deliver(Connected{}, Disconnected{}, Connected{})
	h.want(StateUnpaired, "")
	if h.steps() != 0 || h.client.count("connect") != 1 {
		t.Fatalf("calls %v: the rejected device was connected", h.client.history())
	}
	clock.advance(time.Second)
	eventually(t, "the second logout failed and its retry waits", func() bool {
		return len(h.alerts("logout_failed")) == 2 && clock.waiting() == 1
	})
	clock.advance(2 * time.Second)
	h.client.waitFor(t, "logout")
	eventually(t, "the logout loop ends", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return !h.s.loggingOut
	})
	for i, a := range h.alerts("logout_failed") {
		if a["level"] != "WARN" || a["attempt"] != float64(i+1) || a["error_type"] != "*errors.errorString" {
			t.Fatalf("logout_failed alerts %v", h.alerts("logout_failed"))
		}
	}
	if a := h.alerts("pair_rejected"); len(a) != 1 || a[0]["stage"] != "after_pairing" {
		t.Fatalf("pair_rejected alerts %v", a)
	}
	if h.counter("wawarden_paired") != 0 {
		t.Fatal("the Paired gauge is not 0 after the rejected device was logged out")
	}
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair after the rejected device was logged out = %v", err)
	}
	if strings.Contains(h.logs.String(), "15550100002") {
		t.Fatal("the rejected account reached the log")
	}
}

func TestTheOwnersPairingEndsARejection(t *testing.T) {
	h := newSupRig(t)
	clock := newGatedClock()
	h.s.clock = clock
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.client.failLogouts(1)
	h.client.pairAs(bobDev)
	h.deliver(Paired{JID: bobDev})
	eventually(t, "the logout failed and its retry waits", func() bool { return clock.waiting() == 1 })
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair after the rejected device disappeared = %v", err)
	}
	h.client.pairAs(ownerDev)
	h.deliver(Paired{JID: ownerDev}, Connected{})
	h.want(StateConnected, "")
	if !h.s.accepting() {
		t.Fatal("the owner's traffic is not accepted after the owner paired")
	}
	clock.advance(time.Second)
	eventually(t, "the logout loop ends", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return !h.s.loggingOut
	})
	if h.client.count("logout") != 0 || h.client.count("logout_failed") != 1 || !h.client.Paired() {
		t.Fatalf("calls %v: the owner's device was logged out", h.client.history())
	}
	h.want(StateConnected, "")
}

func (h *supRig) refusesTheStoredDevice(connects int) {
	h.t.Helper()
	h.want(StateDisconnected, ReasonOwnerMismatch)
	h.s.mu.Lock()
	loggingOut := h.s.loggingOut
	h.s.mu.Unlock()
	if h.steps() != 0 || h.client.count("connect") != connects || loggingOut || h.client.count("logout") != 0 || h.client.count("logout_failed") != 0 {
		h.t.Fatalf("calls %v: another account's stored device was connected or logged out", h.client.history())
	}
	mismatches := 0
	for _, a := range h.alerts("disconnected") {
		if a["reason"] == string(ReasonOwnerMismatch) && a["level"] == "WARN" {
			mismatches++
		}
	}
	if mismatches != 1 || len(h.alerts("pair_rejected")) != 0 {
		h.t.Fatalf("disconnected alerts %v, pair_rejected alerts %v", h.alerts("disconnected"), h.alerts("pair_rejected"))
	}
	if h.s.accepting() {
		h.t.Fatal("traffic of another account's stored device is accepted")
	}
	alerts := len(h.alerts("disconnected"))
	if err := h.s.reconnect(); !errors.Is(err, ErrOwnerMismatch) {
		h.t.Fatalf("Reconnect = %v, want ErrOwnerMismatch", err)
	}
	h.want(StateDisconnected, ReasonOwnerMismatch)
	if h.steps() != 0 || h.client.count("connect") != connects || !h.client.Paired() || len(h.alerts("disconnected")) != alerts {
		h.t.Fatalf("calls %v after the refused reconnect", h.client.history())
	}
	if h.counter("wawarden_paired") != 1 || h.counter("wawarden_connected") != 0 {
		h.t.Fatal("the gauges do not show a paired, disconnected engine")
	}
	if account := accountOf(h.client.Account()); account != "" && strings.Contains(h.logs.String(), account) {
		h.t.Fatal("the stored account reached the log")
	}
}

func TestTheStoredDeviceMustBeTheOwnersBeforeEveryConnect(t *testing.T) {
	t.Run("the owner's device connects", func(t *testing.T) {
		h := newSupRig(t)
		h.client.pairAs(ownerDev)
		h.versions(versionResult{v: current})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.deliver(Connected{})
		h.want(StateConnected, "")
		if h.client.count("connect") != 1 || len(h.alerts("disconnected")) != 0 {
			t.Fatalf("calls %v, disconnected alerts %v", h.client.history(), h.alerts("disconnected"))
		}
	})
	t.Run("at the start", func(t *testing.T) {
		h := newSupRig(t)
		h.client.pairAs(bobDev)
		h.versions(versionResult{v: current})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.refusesTheStoredDevice(0)
	})
	t.Run("without an account", func(t *testing.T) {
		h := newSupRig(t)
		h.client.pairAs("@s.whatsapp.net")
		h.versions(versionResult{v: current})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.refusesTheStoredDevice(0)
	})
	t.Run("before the version is fetched", func(t *testing.T) {
		h := newSupRig(t)
		h.client.pairAs(bobDev)
		v := h.versions(versionResult{err: errors.New("synthetic: offline")})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.refusesTheStoredDevice(0)
		if v.count() != 0 || len(h.alerts("disconnected")) != 1 {
			t.Fatalf("%d fetches, disconnected alerts %v", v.count(), h.alerts("disconnected"))
		}
	})
	t.Run("on an explicit reconnect while outdated", func(t *testing.T) {
		h := newSupRig(t)
		v := h.versions(versionResult{err: errors.New("synthetic: offline")})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.want(StateDisconnected, ReasonOutdated)
		h.client.pairAs(bobDev)
		if err := h.s.reconnect(); !errors.Is(err, ErrOwnerMismatch) {
			t.Fatalf("Reconnect while outdated with another account's device = %v, want ErrOwnerMismatch", err)
		}
		h.refusesTheStoredDevice(0)
		if v.count() != 4 {
			t.Fatalf("%d fetches, want only the 4 of the start", v.count())
		}
	})
	t.Run("at the start before the restart budget", func(t *testing.T) {
		h := newSupRig(t)
		h.client.pairAs(bobDev)
		v := h.versions(versionResult{v: current})
		h.s.begin(t.Context(), MaxRecentStarts+1)
		h.refusesTheStoredDevice(0)
		for _, a := range h.alerts("disconnected") {
			if a["reason"] == string(ReasonRestartBudget) {
				t.Fatalf("disconnected alerts %v: the restart budget hid the owner mismatch", h.alerts("disconnected"))
			}
		}
		if v.count() != 0 {
			t.Fatalf("%d fetches, want none", v.count())
		}
	})
	t.Run("on an explicit reconnect under the restart budget", func(t *testing.T) {
		h := newSupRig(t)
		v := h.versions(versionResult{v: current})
		h.s.begin(t.Context(), MaxRecentStarts+1)
		h.want(StateDisconnected, ReasonRestartBudget)
		h.client.pairAs(bobDev)
		if err := h.s.reconnect(); !errors.Is(err, ErrOwnerMismatch) {
			t.Fatalf("Reconnect under the restart budget with another account's device = %v, want ErrOwnerMismatch", err)
		}
		h.refusesTheStoredDevice(0)
		if v.count() != 0 {
			t.Fatalf("%d fetches, want none", v.count())
		}
	})
	for _, account := range []string{"155501000091", "1555010000", ownerDev, owner} {
		t.Run("not "+account, func(t *testing.T) {
			h := newSupRig(t)
			h.client.account = account
			h.versions(versionResult{v: current})
			h.s.begin(t.Context(), 1)
			h.steps()
			h.refusesTheStoredDevice(0)
		})
	}
	t.Run("on an explicit reconnect", func(t *testing.T) {
		h := newSupRig(t)
		h.connectedNow()
		h.deliver(StreamReplaced{})
		h.client.pairAs(bobDev)
		if err := h.s.reconnect(); !errors.Is(err, ErrOwnerMismatch) {
			t.Fatalf("Reconnect with another account's device = %v, want ErrOwnerMismatch", err)
		}
		h.refusesTheStoredDevice(1)
	})
	t.Run("after a drop", func(t *testing.T) {
		h := newSupRig(t)
		h.connectedNow()
		h.client.pairAs(bobDev)
		h.deliver(Disconnected{})
		h.steps()
		h.refusesTheStoredDevice(1)
	})
	t.Run("not without an owner's number", func(t *testing.T) {
		h := newSupRig(t, withOwner(""))
		h.client.pairAs(bobDev)
		h.versions(versionResult{v: current})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.deliver(Connected{})
		h.want(StateConnected, "")
		if h.client.count("connect") != 1 || h.client.count("logout") != 0 || len(h.alerts("disconnected")) != 0 || len(h.alerts("pair_rejected")) != 0 {
			t.Fatalf("calls %v: without an owner's number the stored device is connected unchecked", h.client.history())
		}
	})
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

func TestAPairingThatFailedInTheClientCanBeStartedAgain(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.deliver(Disconnected{})
	h.want(StateUnpaired, "")
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("the second Pair = %v", err)
	}
	h.want(StateConnecting, "")
	if h.client.count("connect") != 2 || h.client.count("pair:15550100009") != 2 {
		t.Fatalf("calls %v, want the second pairing to dial again before it asks for a code", h.client.history())
	}
}

func TestALoginRequestAfterPairingIsRedialledByTheSupervisor(t *testing.T) {
	h := newSupRig(t)
	h.client.setPaired(false)
	if _, err := h.s.pair(t.Context()); err != nil {
		t.Fatalf("Pair = %v", err)
	}
	h.client.pairAs(ownerDev)
	h.deliver(Paired{JID: ownerDev}, Disconnected{})
	h.want(StateConnecting, "")
	if h.steps() == 0 || h.client.count("connect") != 2 {
		t.Fatalf("calls %v, want the supervisor to dial the paired device again", h.client.history())
	}
	if got := h.clock.slept(); len(got) != 1 || got[0] <= 0 {
		t.Fatalf("waits %v, want one backoff delay before the new dial", got)
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
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
	connects := h.client.count("connect")
	h.versions(versionResult{v: current})
	if err := h.s.reconnect(); err != nil {
		t.Fatalf("Reconnect when outdated = %v", err)
	}
	h.steps()
	h.want(StateDisconnected, ReasonOutdated)
	if h.client.count("connect") != connects {
		t.Fatalf("a version the server called outdated was used again: %v", h.client.history())
	}
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

func TestReconnectWhileConnectingReplacesTheConnection(t *testing.T) {
	t.Run("waiting for Connected", func(t *testing.T) {
		h := newSupRig(t)
		h.client.oneSocket = true
		h.versions(versionResult{v: current})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.want(StateConnecting, "")
		if err := h.s.reconnect(); err != nil {
			t.Fatalf("Reconnect = %v", err)
		}
		h.steps()
		h.deliver(Connected{})
		h.want(StateConnected, "")
		if got := h.client.history(); !slices.Equal(got, []string{"connect", "disconnect", "connect"}) {
			t.Fatalf("calls %v, want the first connection closed before the second", got)
		}
		if h.counter("wawarden_connected") != 1 {
			t.Fatal("the Connected gauge is not 1")
		}
	})
	t.Run("while dialing", func(t *testing.T) {
		h := newSupRig(t)
		h.client.oneSocket = true
		var once sync.Once
		h.client.onConnect = func() {
			once.Do(func() {
				if err := h.s.reconnect(); err != nil {
					t.Errorf("Reconnect = %v", err)
				}
			})
		}
		h.versions(versionResult{v: current})
		h.s.begin(t.Context(), 1)
		h.steps()
		h.deliver(Connected{})
		h.want(StateConnected, "")
		if got := h.client.history(); !slices.Equal(got, []string{"connect", "disconnect", "disconnect", "connect"}) {
			t.Fatalf("calls %v, want the dial that lost its race closed before the next", got)
		}
	})
}

func TestReconnectAfterAFailedStartTakesAnEqualVersion(t *testing.T) {
	h := newSupRig(t)
	h.versions(versionResult{err: errors.New("synthetic: offline")})
	h.s.begin(t.Context(), 1)
	h.steps()
	h.want(StateDisconnected, ReasonOutdated)
	h.versions(versionResult{v: current})
	if err := h.s.reconnect(); err != nil {
		t.Fatalf("Reconnect = %v", err)
	}
	h.steps()
	if got := h.client.history(); !slices.Equal(got, []string{"disconnect", "connect"}) {
		t.Fatalf("calls %v, want the disconnect of the failed start, then one connect with the current version", got)
	}
	h.deliver(Connected{})
	h.want(StateConnected, "")
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
