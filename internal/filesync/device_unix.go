//go:build !windows

package filesync

import (
	"fmt"
	"os"
	"syscall"
)

// folderDeviceID returns the filesystem device ID for the given path.
// On Unix, this is stat.Dev — it identifies the mount point. A change
// in device ID between scans indicates the folder was unmounted and
// (possibly) remounted on a different filesystem.
func folderDeviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported platform: cannot extract device ID")
	}
	return uint64(stat.Dev), nil
}
