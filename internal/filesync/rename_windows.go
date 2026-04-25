//go:build windows

package filesync

import (
	"log/slog"
	"os"
)

// renameReplaceRoot renames src to dst within an os.Root.
// L5: prevents symlink TOCTOU by using Root-relative operations.
// On Windows, Root.Rename fails when dst exists. Remove first then rename.
// This is not atomic (unlike MoveFileEx) but is safe from path traversal.
func renameReplaceRoot(root *os.Root, src, dst string) error {
	if err := root.Remove(dst); err != nil && !os.IsNotExist(err) {
		slog.Debug("renameReplaceRoot: pre-rename remove failed", "dst", dst, "error", err)
	}
	return root.Rename(src, dst)
}
