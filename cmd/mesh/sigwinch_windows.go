//go:build windows

package main

import "os"

// winchSignal returns a nil channel on Windows (no SIGWINCH support).
// The dashboard still adapts on every tick via term.GetSize.
func winchSignal() (<-chan os.Signal, func()) {
	return nil, func() {} // receiving from nil channel blocks forever, which is correct
}
