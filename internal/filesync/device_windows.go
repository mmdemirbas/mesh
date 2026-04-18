//go:build windows

package filesync

import (
	"fmt"
	"os"
	"syscall"
)

// folderDeviceID returns the volume serial number for the given path.
// On Windows, this serves the same purpose as Unix stat.Dev — it identifies
// the volume and changes if the folder is remounted on a different volume.
func folderDeviceID(path string) (uint64, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("invalid path: %w", err)
	}

	var info syscall.ByHandleFileInformation
	h, err := syscall.CreateFile(pathp,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS, // required for directories
		0)
	if err != nil {
		// Fallback: just stat — cannot get volume serial.
		if _, statErr := os.Stat(path); statErr != nil {
			return 0, statErr
		}
		return 0, fmt.Errorf("cannot open directory for device ID: %w", err)
	}
	defer syscall.CloseHandle(h)

	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, fmt.Errorf("GetFileInformationByHandle: %w", err)
	}
	return uint64(info.VolumeSerialNumber), nil
}
