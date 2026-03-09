//go:build !windows

package tunnel

import "syscall"

// dialerControlIPQoS returns a net.Dialer.Control function that sets IP_TOS on the socket.
// If tos < 0, returns nil (no control function needed).
func dialerControlIPQoS(tos int) func(network, address string, c syscall.RawConn) error {
	if tos < 0 {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		var setErr error
		err := c.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, tos)
		})
		if err != nil {
			return err
		}
		return setErr
	}
}
