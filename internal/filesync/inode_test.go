package filesync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInodePopulatedDuringScan verifies that the scan records the
// filesystem inode on every indexed file (Unix) or zero (Windows,
// where Step 1 defers inode extraction to a follow-up). The inode is
// the signal R1 Phase 2 uses to detect local renames without a
// re-transfer, so populating it correctly during scan is the
// foundation for everything that follows.
func TestInodePopulatedDuringScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "alpha.txt", "one")
	writeFile(t, dir, "sub/beta.txt", "two")

	idx := newFileIndex()
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"alpha.txt", "sub/beta.txt"} {
		entry, ok := idx.Files[rel]
		if !ok {
			t.Fatalf("entry missing: %s", rel)
		}
		if runtime.GOOS == "windows" {
			if entry.Inode != 0 {
				t.Errorf("%s: Inode=%d on Windows, want 0 (Step 1 defers Windows population)", rel, entry.Inode)
			}
			continue
		}
		if entry.Inode == 0 {
			t.Errorf("%s: Inode=0, want populated inode", rel)
		}

		// Cross-check: the recorded inode must match an os.Stat of the
		// same file. This pins scan behaviour to the real filesystem
		// rather than trusting scan bookkeeping in isolation.
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		got := inodeOf(info)
		if got != entry.Inode {
			t.Errorf("%s: scan recorded Inode=%d, os.Stat sees %d", rel, entry.Inode, got)
		}
	}
}

// TestInodeStableAcrossRescan pins the invariant that a rescan of an
// unchanged tree preserves the inode observed on the first pass. This
// matters because rename detection compares inodes across consecutive
// scans — a spurious change would produce false-positive rename
// candidates.
func TestInodeStableAcrossRescan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Step 1 defers Windows inode population to a follow-up step")
	}
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "stable.txt", "content")

	idx := newFileIndex()
	for pass := range 2 {
		_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	// Capture the inode we expect from the filesystem.
	info, err := os.Stat(filepath.Join(dir, "stable.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := inodeOf(info)

	entry := idx.Files["stable.txt"]
	if entry.Inode != want {
		t.Errorf("after rescan Inode=%d, want %d (fast-path must preserve inode)", entry.Inode, want)
	}
}

// TestInodeBackfillOnFastPath verifies the migration path: an index
// loaded from a pre-Inode version (where Inode==0) gets its Inode
// populated on the next scan even when the fast-path size+mtime check
// would otherwise skip the entry. Without this, older persisted
// indexes would never gain inode information until the file changed.
func TestInodeBackfillOnFastPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Step 1 defers Windows inode population to a follow-up step")
	}
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "legacy.txt", "payload")

	idx := newFileIndex()
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-Inode persisted entry: wipe the recorded inode
	// but keep the size/mtime/hash as a real scan would have produced.
	entry := idx.Files["legacy.txt"]
	if entry.Inode == 0 {
		t.Fatal("precondition failed: scan should have populated Inode")
	}
	entry.Inode = 0
	idx.Files["legacy.txt"] = entry

	// Rescan — fast path must still backfill the inode.
	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FastPathHits == 0 {
		t.Errorf("FastPathHits=%d, want >0 (unchanged file must take fast path)", stats.FastPathHits)
	}

	got := idx.Files["legacy.txt"]
	if got.Inode == 0 {
		t.Error("fast path did not backfill Inode — migration from pre-Inode indexes will not happen until content changes")
	}
}

// TestInodeOfNilInfo pins the defensive behaviour: inodeOf must not
// panic on a nil FileInfo, and must return 0 so callers can treat the
// value as "unknown" uniformly.
func TestInodeOfNilInfo(t *testing.T) {
	t.Parallel()
	if got := inodeOf(nil); got != 0 {
		t.Errorf("inodeOf(nil)=%d, want 0", got)
	}
}
