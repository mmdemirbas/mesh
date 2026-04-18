//go:build !windows

package filesync

import "os"

// renameReplace atomically renames src to dst.
// On Unix, os.Rename atomically replaces dst if it exists.
func renameReplace(src, dst string) error {
	return os.Rename(src, dst)
}

// renameReplaceRoot atomically renames src to dst within an os.Root.
// L5: prevents symlink TOCTOU by using Root-relative operations.
func renameReplaceRoot(root *os.Root, src, dst string) error {
	return root.Rename(src, dst)
}
