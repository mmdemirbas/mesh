//go:build windows

package filesync

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInodeFromFileExtractsNTFileIndex pins R1 Phase 2 Step 5: opening
// a handle and calling GetFileInformationByHandle yields a non-zero NT
// file index, and two opens of the same path return the same id. This
// is the Windows equivalent of the Unix stat inode — the signal used by
// rename detection.
func TestInodeFromFileExtractsNTFileIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	if err := os.WriteFile(path, []byte("windows"), 0644); err != nil {
		t.Fatal(err)
	}

	f1, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ino1 := inodeFromFile(f1)
	_ = f1.Close()

	if ino1 == 0 {
		t.Fatal("inodeFromFile returned 0 on Windows — Step 5 population failed")
	}

	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ino2 := inodeFromFile(f2)
	_ = f2.Close()

	if ino1 != ino2 {
		t.Errorf("inode differs across opens: %d vs %d (must be stable)", ino1, ino2)
	}
}

// TestInodeFromFileZeroOnNil guards the defensive branch: a nil handle
// must not panic and must return 0 so callers can treat it uniformly as
// "no id available".
func TestInodeFromFileZeroOnNil(t *testing.T) {
	t.Parallel()
	if got := inodeFromFile(nil); got != 0 {
		t.Errorf("inodeFromFile(nil)=%d, want 0", got)
	}
}
