//go:build !windows

package main

import "os"

// openFDCount returns the number of open file descriptors for this process.
// Returns -1 if the count cannot be determined.
func openFDCount() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}
