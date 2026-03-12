//go:build !windows

package netutil

import (
	"context"
	"net"
	"syscall"
)

// ListenReusable creates a TCP listener that automatically asserts SO_REUSEADDR.
// This eliminates OS-level TIME_WAIT collisions when rapidly re-binding local ports.
func ListenReusable(ctx context.Context, network, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}
}
