//go:build windows

package filesync

import "golang.org/x/sys/windows"

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
