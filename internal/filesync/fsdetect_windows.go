//go:build windows

package filesync

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// detectNetworkFS reports whether path resides on a network filesystem.
func detectNetworkFS(path string) (string, bool) {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return "", false
	}
	// GetDriveType needs a trailing backslash (e.g. "C:\").
	root := vol + `\`
	dt := windows.GetDriveType(windows.StringToUTF16Ptr(root))
	if dt == windows.DRIVE_REMOTE {
		return "remote", true
	}
	return "", false
}
