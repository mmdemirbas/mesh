//go:build windows

package netutil

import (
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// ListenConfig generates a standard net.ListenConfig that asserts
// SO_REUSEADDR on the socket context, bypassing TIME_WAIT collisions.
func ListenConfig() net.ListenConfig {
	return net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
			})
		},
	}
}
