//go:build !windows

package netutil

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// ListenConfig generates a standard net.ListenConfig that asserts
// SO_REUSEADDR on the socket context, bypassing TIME_WAIT collisions.
func ListenConfig() net.ListenConfig {
	return net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			})
		},
	}
}
