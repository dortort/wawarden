package listenertest

import "syscall"

func isListening(fd int) bool {
	v, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	return err == nil && v != 0
}
