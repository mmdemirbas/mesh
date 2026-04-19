//go:build windows

package filesync

import "io/fs"

// inodeOf returns a stable file ID for rename detection.
//
// Windows's FindFirstFile-based fs.FileInfo (returned by ReadDir) does not
// include the NT file index — extracting it requires a separate CreateFile
// + GetFileInformationByHandle per entry, which doubles syscall cost on
// large trees. R1 Phase 2 Step 1 therefore returns 0 on Windows, which
// disables sender-side rename detection on that platform. Receivers still
// benefit from the Phase 1 content-hash rename fast path.
//
// A follow-up step will add Windows population by opening a handle during
// the hash phase (where the file is already being read) and calling
// GetFileInformationByHandle to cheaply obtain the file ID alongside the
// content.
func inodeOf(info fs.FileInfo) uint64 {
	return 0
}
