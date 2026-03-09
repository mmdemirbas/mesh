package tunnel

import (
	"fmt"
	"strings"
	"syscall"
)

// ipqosValues maps OpenSSH-compatible IPQoS names to IP_TOS byte values.
var ipqosValues = map[string]int{
	// Legacy TOS values
	"lowdelay":    0x10,
	"throughput":  0x08,
	"reliability": 0x04,

	// DSCP classes
	"af11": 0x28, "af12": 0x30, "af13": 0x38,
	"af21": 0x48, "af22": 0x50, "af23": 0x58,
	"af31": 0x68, "af32": 0x70, "af33": 0x78,
	"af41": 0x88, "af42": 0x90, "af43": 0x98,
	"ef": 0xb8,

	// Class selector
	"cs0": 0x00, "cs1": 0x20, "cs2": 0x40, "cs3": 0x60,
	"cs4": 0x80, "cs5": 0xa0, "cs6": 0xc0, "cs7": 0xe0,

	// None
	"none": 0x00,
}

// ParseIPQoS converts an IPQoS name to its numeric IP_TOS value.
// Returns -1 if the value is empty (meaning "don't set").
func ParseIPQoS(name string) (int, error) {
	if name == "" {
		return -1, nil
	}
	v, ok := ipqosValues[strings.ToLower(name)]
	if !ok {
		return 0, fmt.Errorf("unknown ipqos value %q", name)
	}
	return v, nil
}

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
