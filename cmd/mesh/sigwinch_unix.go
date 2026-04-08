//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// winchSignal returns a channel that receives a value on terminal resize (SIGWINCH),
// and a stop function that deregisters the signal handler.
func winchSignal() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}
