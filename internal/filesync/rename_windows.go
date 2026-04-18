//go:build windows

package filesync

import (
	"log/slog"
	"os"

	"golang.org/x/sys/windows"
)

// renameReplace atomically renames src to dst.
// On Windows, os.Rename fails when dst exists. MoveFileEx with
// MOVEFILE_REPLACE_EXISTING performs an atomic replace on NTFS,
// eliminating the crash window between remove and rename (B16).
func renameReplace(src, dst string) error {
	return windows.MoveFileEx(
		windows.StringToUTF16Ptr(src),
		windows.StringToUTF16Ptr(dst),
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

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
