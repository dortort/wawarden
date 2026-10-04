//go:build linux || darwin

package listeners

import (
	"net"
	"net/netip"
	"syscall"
	"testing"
)

func TestListenersKeepToOneAddressFamily(t *testing.T) {
	v4 := openSet(t, Spec{Name: "v4", Addr: netip.MustParseAddrPort("0.0.0.0:0"), Handler: text("v4")})
	if got := v4.Inventory()[0].Addr.Addr(); got != netip.IPv4Unspecified() {
		t.Fatalf("an IPv4 wildcard listener is bound to %v, want 0.0.0.0 only", got)
	}

	v6 := openSet(t, Spec{Name: "v6", Addr: netip.MustParseAddrPort("[::1]:0"), Handler: text("v6")})
	conn, err := v6.servers[0].ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var only int
	var sockErr error
	if err := conn.Control(func(fd uintptr) {
		only, sockErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY)
	}); err != nil || sockErr != nil {
		t.Fatalf("IPV6_V6ONLY: %v, %v", err, sockErr)
	}
	if only != 1 {
		t.Fatalf("IPV6_V6ONLY = %d on an IPv6 listener, want 1", only)
	}
}
