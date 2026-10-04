//go:build linux || darwin

package listeners

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/listeners/listenertest"
)

func requireProcessListeners(t *testing.T, want ...netip.AddrPort) {
	t.Helper()
	got, err := listenertest.ListeningTCP()
	if err != nil {
		t.Fatalf("ListeningTCP: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the process listens on %v, want exactly %v", got, want)
	}
}

func TestOpenReleasesEarlierListenersOnFailure(t *testing.T) {
	taken := openSet(t, Spec{Name: "taken", Addr: loopback, Handler: text("x")})
	busy := taken.Inventory()[0].Addr
	requireProcessListeners(t, busy)
	logger, _ := newLogger(t)
	_, err := Open(t.Context(), logger, []Spec{
		{Name: "first", Addr: loopback, Handler: text("x")},
		{Name: "second", Addr: loopback, Handler: text("x")},
		{Name: "clash", Addr: busy, Handler: text("x")},
	})
	if err == nil || !strings.Contains(err.Error(), "clash") {
		t.Fatalf("Open = %v, want an error naming the clashing listener", err)
	}
	requireProcessListeners(t, busy)
}
