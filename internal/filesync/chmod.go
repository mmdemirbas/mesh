package filesync

import "os"

// chmodFileRoot sets the mode of name within root using fchmod on an
// open file descriptor, avoiding the path-based TOCTOU race that
// affects Root.Chmod on Unix (Go documents this for Root.Chmod /
// Root.Chown / Root.Chtimes; see also CVE-2026-32282 / golang/go#78293
// for the Linux-specific symlink-swap variant).
//
// Root.Open re-resolves the path under the root's confinement, so a
// symlink-swap can still redirect the fd to a different file within
// the root — but never to a path outside it (Root rejects symlinks
// that escape). Once the fd is open, *os.File.Chmod calls fchmod(2),
// which operates on the kernel inode behind the fd and ignores any
// subsequent path mutation. The race window in the path-based
// Root.Chmod (replace target with a symlink between path resolution
// and the chmod syscall) is therefore closed.
//
// O_RDONLY needs read permission on the file. This is satisfied for
// every file mesh installs (download/conflict/rename targets all have
// at minimum 0600 after the rename), so the open never fails on a
// permission lookup. Callers continue to ignore the error — a chmod
// failure is a UX issue, not a correctness one.
func chmodFileRoot(root *os.Root, name string, mode os.FileMode) error {
	f, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}
