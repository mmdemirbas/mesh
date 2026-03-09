//go:build windows

package tunnel

import "syscall"

// dialerControlIPQoS returns a net.Dialer.Control function that sets IP_TOS on the socket.
// If tos < 0, returns nil (no control function needed).
// On Windows, IP_TOS is not easily settable via the Go syscall package, so this is a no-op.
func dialerControlIPQoS(tos int) func(network, address string, c syscall.RawConn) error {
	return nil
}
