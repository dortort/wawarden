//go:build linux || darwin

package listenertest

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dortort/wawarden/internal/listeners"
)

func listening(t *testing.T) []netip.AddrPort {
	t.Helper()
	got, err := ListeningTCP()
	if err != nil {
		t.Fatalf("ListeningTCP: %v", err)
	}
	return got
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func listenIPv6(t *testing.T) netip.AddrPort {
	t.Helper()
	s, err := listeners.Open(t.Context(), slog.New(slog.DiscardHandler), []listeners.Spec{{Name: "v6", Addr: netip.MustParseAddrPort("[::1]:0"), Handler: http.NotFoundHandler()}})
	if err != nil {
		t.Fatalf("listen on IPv6 loopback: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s.Inventory()[0].Addr
}

func TestListeningTCP(t *testing.T) {
	for _, mode := range []struct {
		name string
		dirs []string
	}{
		{name: "dir", dirs: fdDirs},
		{name: "probe", dirs: nil},
	} {
		t.Run(mode.name, func(t *testing.T) {
			saved := fdDirs
			fdDirs = mode.dirs
			t.Cleanup(func() { fdDirs = saved })

			base := listening(t)
			first := listen(t)
			second := listen(t)
			v6 := listenIPv6(t)

			udp, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen udp: %v", err)
			}
			t.Cleanup(func() { _ = udp.Close() })

			unix, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "s"))
			if err != nil {
				t.Fatalf("listen unix: %v", err)
			}
			t.Cleanup(func() { _ = unix.Close() })

			client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp4", first.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
			server, err := first.Accept()
			if err != nil {
				t.Fatalf("accept: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			want := append(slices.Clone(base), first.Addr().(*net.TCPAddr).AddrPort(), second.Addr().(*net.TCPAddr).AddrPort(), v6)
			slices.SortFunc(want, netip.AddrPort.Compare)
			if got := listening(t); !slices.Equal(got, want) {
				t.Fatalf("listening = %v, want %v: UDP, unix-domain and connected sockets must not appear", got, want)
			}
			slices.Reverse(want)
			Require(t, want...)

			if err := second.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			want = append(slices.Clone(base), first.Addr().(*net.TCPAddr).AddrPort(), v6)
			slices.SortFunc(want, netip.AddrPort.Compare)
			if got := listening(t); !slices.Equal(got, want) {
				t.Fatalf("after closing a listener, listening = %v, want %v", got, want)
			}
		})
	}
}
