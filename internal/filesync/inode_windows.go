//go:build windows

package filesync

import (
	"io/fs"
	"os"
	"syscall"
)

// inodeOf returns a stable file ID for rename detection.
//
// Windows's FindFirstFile-based fs.FileInfo (returned by ReadDir) does not
// include the NT file index, so the walk phase cannot extract it cheaply.
// Step 5 pairs this with inodeFromFile: the hash phase already opens the
// file, and GetFileInformationByHandle on the existing handle yields the
// NT file index without a second CreateFile. Files that hit the fast path
// (size+mtime unchanged) keep the previously-recorded inode.
func inodeOf(_ fs.FileInfo) uint64 {
	return 0
}

// inodeFromFile extracts the NT file index from an open handle by calling
// GetFileInformationByHandle. The high and low 32-bit halves are packed
// into a uint64 the same way folderDeviceID packs the volume serial.
//
// Returns 0 when the call fails or the file was opened without a valid
// handle. A zero inode means "rename detection disabled for this entry"
// throughout the filesync code.
func inodeFromFile(f *os.File) uint64 {
	if f == nil {
		return 0
	}
	h := syscall.Handle(f.Fd())
	if h == syscall.InvalidHandle {
		return 0
	}
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}
