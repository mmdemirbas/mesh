//go:build windows

package netutil

import (
	"context"
	"net"
	"syscall"
)

// SO_EXCLUSIVEADDRUSE prevents another process from binding to the same port.
// On Windows, SO_REUSEADDR allows port hijacking — any process can steal a
// bound port. SO_EXCLUSIVEADDRUSE is the correct option for security.
const soExclusiveAddrUse = ^int(4) // -5, 0xFFFFFFFB

// ListenReusable creates a TCP listener with SO_EXCLUSIVEADDRUSE to prevent
// port hijacking on Windows.
func ListenReusable(ctx context.Context, network, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}

	if network == "tcp" && address == "" {
		return lc.Listen(ctx, network, "0.0.0.0:0")
	}

	return lc.Listen(ctx, network, address)
}
