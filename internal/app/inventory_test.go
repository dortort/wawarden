//go:build linux || darwin

package app

import (
	"errors"
	"io"
	"net/netip"
	"testing"

	"github.com/dortort/wawarden/internal/listeners/listenertest"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/token"
)

func inventoryAddrs(a *App) []netip.AddrPort {
	var out []netip.AddrPort
	for _, b := range a.Inventory() {
		out = append(out, b.Addr)
	}
	return out
}

func TestProcessListenersAreExactlyTheInventory(t *testing.T) {
	tests := []struct {
		name       string
		adminToken string
		count      int
	}{
		{name: "without an admin hash", count: 2},
		{name: "with an admin hash", adminToken: token.NewAdmin(), count: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listenertest.Require(t)
			a, _, stop := start(t, testConfig(t, tt.adminToken), noClients{})
			if len(a.Inventory()) != tt.count {
				t.Fatalf("inventory = %+v, want %d listeners", a.Inventory(), tt.count)
			}
			listenertest.Require(t, inventoryAddrs(a)...)
			if err := stop(); err != nil {
				t.Fatalf("Run = %v", err)
			}
			listenertest.Require(t)
		})
	}
}

func TestHealthListenerIsRefusedBeyondLoopback(t *testing.T) {
	for _, health := range []string{"192.0.2.1:8081", "[2001:db8::1]:8081"} {
		t.Run(health, func(t *testing.T) {
			cfg := testConfig(t, "")
			cfg.HealthListen = netip.MustParseAddrPort(health)
			a, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, idle())
			if err == nil {
				t.Cleanup(func() { _ = run(t, a)() })
			}
			if !errors.Is(err, errHealthNotLoopback) {
				t.Fatalf("newAppWith = %v, want the non-loopback health address refused before anything is bound", err)
			}
			listenertest.Require(t)
		})
	}
}

func TestFailedStartLeavesNoListener(t *testing.T) {
	a, _, stop := start(t, testConfig(t, token.NewAdmin()), noClients{})
	cfg := testConfig(t, token.NewAdmin())
	cfg.HealthListen = addr(t, a, "health")
	if _, err := newAppWith(t.Context(), cfg, logx.NewWriter(io.Discard), noClients{}, idle()); err == nil {
		t.Fatal("newAppWith bound an address already in use")
	}
	listenertest.Require(t, inventoryAddrs(a)...)
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	listenertest.Require(t)
}
