//go:build linux

package filesync

import "syscall"

// Network filesystem magic numbers from statfs(2).
const (
	nfsMagic  = 0x6969     // NFS_SUPER_MAGIC
	smbMagic  = 0x517B     // SMB_SUPER_MAGIC
	cifsMagic = 0xFF534D42 // CIFS_MAGIC_NUMBER
	fuseMagic = 0x65735546 // FUSE_SUPER_MAGIC
)

func classifyStatfs(st *syscall.Statfs_t) (string, bool) {
	switch st.Type {
	case nfsMagic:
		return "nfs", true
	case smbMagic:
		return "smb", true
	case cifsMagic:
		return "cifs", true
	case fuseMagic:
		return "fuse", true
	default:
		return "", false
	}
}
