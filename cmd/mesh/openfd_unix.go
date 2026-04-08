//go:build !windows

package main

import "os"

// openFDCount returns the number of open file descriptors for this process.
// Returns -1 if the count cannot be determined.
func openFDCount() int {
	f, err := os.Open("/dev/fd")
	if err != nil {
		return -1
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return -1
	}
	return len(names)
}
