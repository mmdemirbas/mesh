//go:build !windows

package filesync

import "os"

// renameReplace atomically renames src to dst.
// On Unix, os.Rename atomically replaces dst if it exists.
func renameReplace(src, dst string) error {
	return os.Rename(src, dst)
}
