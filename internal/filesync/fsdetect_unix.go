//go:build !windows

package filesync

import "syscall"

// detectNetworkFS reports whether path resides on a network filesystem.
// Returns the filesystem type name and true if network, empty and false otherwise.
func detectNetworkFS(path string) (string, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", false
	}
	return classifyStatfs(&st)
}
