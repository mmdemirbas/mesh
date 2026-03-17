//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func init() {
	// Enable ANSI escape code processing on Windows cmd.exe so that
	// the tint logger's color output and the live dashboard render correctly.
	enableVirtualTerminalProcessing(os.Stderr)
	enableVirtualTerminalProcessing(os.Stdout)
}

func enableVirtualTerminalProcessing(f *os.File) {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return // not a console (e.g. redirected to file)
	}
	_ = windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
