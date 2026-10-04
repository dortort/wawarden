//go:build linux || darwin

package listeners

import (
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/listeners/listenertest"
)

func TestOpenReleasesEarlierListenersOnFailure(t *testing.T) {
	taken := openSet(t, Spec{Name: "taken", Addr: loopback, Handler: text("x")})
	busy := taken.Inventory()[0].Addr
	listenertest.Require(t, busy)
	logger, _ := newLogger(t)
	_, err := Open(t.Context(), logger, []Spec{
		{Name: "first", Addr: loopback, Handler: text("x")},
		{Name: "second", Addr: loopback, Handler: text("x")},
		{Name: "clash", Addr: busy, Handler: text("x")},
	})
	if err == nil || !strings.Contains(err.Error(), "clash") {
		t.Fatalf("Open = %v, want an error naming the clashing listener", err)
	}
	listenertest.Require(t, busy)
}
