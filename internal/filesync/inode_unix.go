//go:build !windows

package filesync

import (
	"io/fs"
	"os"
	"syscall"
)

// inodeOf returns the filesystem inode number for the file described by
// info. The inode is stable across renames on the same filesystem and is
// the signal R1 uses to detect local renames without re-transferring
// content.
//
// Returns 0 when the inode cannot be extracted (e.g. info.Sys() is not a
// *syscall.Stat_t — which should not happen on Unix in practice but is
// handled defensively). A zero inode means "rename detection disabled for
// this entry" throughout the filesync code.
func inodeOf(info fs.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Ino)
}

// inodeFromFile is the Windows-only counterpart that extracts the inode
// from an already-open file handle. Unix reads the inode from the walk
// phase via Stat, so this helper returns 0 here and the scan keeps the
// walk-captured value. Kept as a no-op so scan code can call it
// unconditionally without a platform branch.
func inodeFromFile(_ *os.File) uint64 {
	return 0
}
