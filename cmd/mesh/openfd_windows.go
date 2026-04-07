//go:build windows

package main

// openFDCount returns -1 on Windows where FD counting is not available.
func openFDCount() int {
	return -1
}
