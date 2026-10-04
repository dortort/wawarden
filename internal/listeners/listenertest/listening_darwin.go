package listenertest

import "syscall"

const (
	tcpConnectionInfo = 0x106
	tcpStateListen    = 1
)

// SO_ACCEPTCONN fails with ENOPROTOOPT for TCP sockets on darwin; TCP_CONNECTION_INFO's first byte is the TCP state.
func isListening(fd int) bool {
	v, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_TCP, tcpConnectionInfo)
	return err == nil && v&0xff == tcpStateListen
}
