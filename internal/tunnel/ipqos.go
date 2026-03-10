package tunnel

import (
	"fmt"
	"strings"
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

// ParseIPQoS converts an IPQoS string to numeric IP_TOS values.
// OpenSSH supports up to two space-separated values: interactive and non-interactive.
// If one value is provided, both return the same value.
func ParseIPQoS(name string) (interactive int, nonInteractive int, err error) {
	if name == "" {
		return -1, -1, nil
	}
	parts := strings.Fields(name)
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("invalid ipqos value: expected 1 or 2 parts")
	}

	parseSingle := func(s string) (int, error) {
		v, ok := ipqosValues[strings.ToLower(s)]
		if !ok {
			return 0, fmt.Errorf("unknown ipqos value %q", s)
		}
		return v, nil
	}

	interactive, err = parseSingle(parts[0])
	if err != nil {
		return 0, 0, err
	}

	nonInteractive = interactive
	if len(parts) == 2 {
		nonInteractive, err = parseSingle(parts[1])
		if err != nil {
			return 0, 0, err
		}
	}

	return interactive, nonInteractive, nil
}
