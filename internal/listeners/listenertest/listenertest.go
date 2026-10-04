//go:build linux || darwin

// Package listenertest reports the TCP sockets the calling process holds in the listening state.
package listenertest

import (
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"syscall"
)

const probeCeiling = 1 << 16

var fdDirs = []string{"/proc/self/fd", "/dev/fd"}

func ListeningTCP() ([]netip.AddrPort, error) {
	fds, err := openFDs()
	if err != nil {
		return nil, err
	}
	var out []netip.AddrPort
	for _, fd := range fds {
		if t, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || t != syscall.SOCK_STREAM {
			continue
		}
		if !isListening(fd) {
			continue
		}
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		switch a := sa.(type) {
		case *syscall.SockaddrInet4:
			out = append(out, (&net.TCPAddr{IP: a.Addr[:], Port: a.Port}).AddrPort())
		case *syscall.SockaddrInet6:
			out = append(out, (&net.TCPAddr{IP: a.Addr[:], Port: a.Port}).AddrPort())
		}
	}
	slices.SortFunc(out, netip.AddrPort.Compare)
	return out, nil
}

func openFDs() ([]int, error) {
	for _, dir := range fdDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		fds := make([]int, 0, len(entries))
		for _, e := range entries {
			if n, err := strconv.Atoi(e.Name()); err == nil {
				fds = append(fds, n)
			}
		}
		return fds, nil
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return nil, err
	}
	var fds []int
	for fd := range probeCeiling {
		if uint64(fd) >= lim.Cur {
			break
		}
		fds = append(fds, fd)
	}
	return fds, nil
}
