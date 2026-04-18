//go:build darwin

package filesync

import "syscall"

// networkFSTypes lists filesystem type names reported by macOS for network mounts.
var networkFSTypes = map[string]bool{
	"nfs":      true,
	"smbfs":    true,
	"afpfs":    true,
	"webdavfs": true,
}

func classifyStatfs(st *syscall.Statfs_t) (string, bool) {
	// Fstypename is [16]int8; convert to string.
	var buf [16]byte
	n := 0
	for i, c := range st.Fstypename {
		if c == 0 {
			break
		}
		buf[i] = byte(c)
		n = i + 1
	}
	fsType := string(buf[:n])
	return fsType, networkFSTypes[fsType]
}
