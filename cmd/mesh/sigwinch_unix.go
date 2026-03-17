//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// winchSignal returns a channel that receives a value on terminal resize (SIGWINCH).
func winchSignal() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch
}
