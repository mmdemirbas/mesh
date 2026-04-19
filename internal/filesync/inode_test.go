package filesync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
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

// TestScanDetectsRenameSameContent pins the R1 Phase 2 scan-level
// rename detection for a content-unchanged move. The sender tags the
// new entry with PrevPath; peers use that hint to apply a local
// rename instead of re-downloading the bytes.
func TestScanDetectsRenameSameContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows inode extraction lands in a later step")
	}
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old/name.txt", "content stays the same")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	oldEntry := idx.Files["old/name.txt"]
	if oldEntry.Inode == 0 {
		t.Fatal("precondition failed: first scan must populate inode")
	}

	// Rename the file in place. The kernel preserves the inode.
	if err := os.MkdirAll(filepath.Join(dir, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "old/name.txt"), filepath.Join(dir, "new/name.txt")); err != nil {
		t.Fatal(err)
	}

	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	if stats.RenamesDetected != 1 {
		t.Errorf("RenamesDetected=%d, want 1", stats.RenamesDetected)
	}
	if stats.Deletions != 1 {
		t.Errorf("Deletions=%d, want 1 (tombstone still emitted for capability-less peers)", stats.Deletions)
	}

	newEntry, ok := idx.Files["new/name.txt"]
	if !ok {
		t.Fatal("new path missing from index")
	}
	if newEntry.PrevPath != "old/name.txt" {
		t.Errorf("PrevPath=%q, want %q", newEntry.PrevPath, "old/name.txt")
	}
	if newEntry.Inode != oldEntry.Inode {
		t.Errorf("Inode=%d on new path, want %d (kernel preserves inode on rename)", newEntry.Inode, oldEntry.Inode)
	}
	if newEntry.SHA256 != oldEntry.SHA256 {
		t.Error("content hash changed across rename, unexpected")
	}

	tomb, ok := idx.Files["old/name.txt"]
	if !ok || !tomb.Deleted {
		t.Error("old path tombstone not emitted")
	}
}

// TestScanDetectsRenameWithEdit pins the R1 Phase 2 big-bandwidth-win
// case: a rename plus content edit. Without the hint the receiver
// would re-download the full file; with the hint it can rename
// locally and run /delta against the old content to move only the
// changed blocks.
func TestScanDetectsRenameWithEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows inode extraction lands in a later step")
	}
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "source.bin", "original bytes")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	oldHash := idx.Files["source.bin"].SHA256
	oldInode := idx.Files["source.bin"].Inode

	// Rename and edit in-place. The edit must be observable via mtime
	// or size so the scan does not take the fast path.
	if err := os.Rename(filepath.Join(dir, "source.bin"), filepath.Join(dir, "target.bin")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "target.bin"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" plus addition"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	if stats.RenamesDetected != 1 {
		t.Errorf("RenamesDetected=%d, want 1", stats.RenamesDetected)
	}

	newEntry := idx.Files["target.bin"]
	if newEntry.PrevPath != "source.bin" {
		t.Errorf("PrevPath=%q, want %q", newEntry.PrevPath, "source.bin")
	}
	if newEntry.Inode != oldInode {
		t.Errorf("inode=%d, want %d (rename preserves inode)", newEntry.Inode, oldInode)
	}
	if newEntry.SHA256 == oldHash {
		t.Error("hash unchanged despite content edit — scan did not re-hash")
	}
}

// TestScanRenameHintClearedOnRescan pins that PrevPath is a
// single-use hint. Once the receiver has had a chance to apply the
// rename, re-emitting the hint on every subsequent scan would bloat
// the index exchange with stale data.
func TestScanRenameHintClearedOnRescan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows inode extraction lands in a later step")
	}
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "payload")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	if err := os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles); err != nil {
		t.Fatal(err)
	}
	if idx.Files["b.txt"].PrevPath != "a.txt" {
		t.Fatal("precondition failed: second scan must set PrevPath")
	}
	idx.recomputeCache()

	// Third scan: nothing changed on disk. PrevPath must be cleared
	// because the hint has already been emitted.
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles); err != nil {
		t.Fatal(err)
	}
	got := idx.Files["b.txt"].PrevPath
	if got != "" {
		t.Errorf("PrevPath=%q after stable rescan, want empty (hint is single-use)", got)
	}
}

// TestScanRenameIgnoresUnchangedPaths guards against a false
// positive: the tombstone loop must only pair deletions with *new*
// paths. A file that keeps the same path (unchanged or edited) must
// never get its own path tagged as PrevPath.
func TestScanRenameIgnoresUnchangedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows inode extraction lands in a later step")
	}
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "keep")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()

	// Edit the file in place — same path, same inode, new content.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep edited payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RenamesDetected != 0 {
		t.Errorf("RenamesDetected=%d on in-place edit, want 0", stats.RenamesDetected)
	}
	if got := idx.Files["keep.txt"].PrevPath; got != "" {
		t.Errorf("PrevPath=%q for in-place edit, want empty", got)
	}
}

// TestPrevPathRoundTripsThroughProto pins the Step 3 wire contract:
// when the sender has tagged a FileEntry with PrevPath, that hint
// survives marshal via buildIndexExchange and demarshal via
// protoToFileIndex. This is the bridge between local detection and
// peer-visible behaviour.
func TestPrevPathRoundTripsThroughProto(t *testing.T) {
	t.Parallel()
	idx := &FileIndex{
		Sequence: 2,
		Files: map[string]FileEntry{
			"old.txt": {Deleted: true, Sequence: 1, MtimeNS: 100},
			"new.txt": {
				Size: 5, MtimeNS: 200, SHA256: testHash("zzz"), Sequence: 2,
				Inode: 42, PrevPath: "old.txt",
			},
		},
	}
	idx.rebuildSeqIndex()
	n := &Node{
		deviceID: "dev",
		folders: map[string]*folderState{
			"f": {index: idx},
		},
	}

	// Both the delta path (seqIndex) and the full path must emit
	// prev_path. The delta path goes through buildIndexExchange when
	// sinceSequence>0 and seqIndex is non-empty; the full path is
	// sinceSequence==0.
	for _, since := range []int64{0, 1} {
		exch := n.buildIndexExchange("f", since)
		var newInfo *pb.FileInfo
		for _, f := range exch.GetFiles() {
			if f.GetPath() == "new.txt" {
				newInfo = f
			}
		}
		if newInfo == nil {
			t.Fatalf("since=%d: new.txt missing from exchange", since)
		}
		if newInfo.GetPrevPath() != "old.txt" {
			t.Errorf("since=%d: proto PrevPath=%q, want %q", since, newInfo.GetPrevPath(), "old.txt")
		}

		decoded := protoToFileIndex(exch)
		got := decoded.Files["new.txt"]
		if got.PrevPath != "old.txt" {
			t.Errorf("since=%d: decoded PrevPath=%q, want %q", since, got.PrevPath, "old.txt")
		}
	}
}

// TestDiffPropagatesPrevPath pins that the receiver's diff() carries
// the sender's hint onto the resulting ActionDownload so the sync
// loop can consume it. Covers the three code paths that emit
// ActionDownload: missing locally, ancestor-driven remote-only
// change, and mtime fallback.
func TestDiffPropagatesPrevPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		local   *FileIndex
		remote  *FileIndex
		baseHsh map[string]Hash256
		lastSyn int64
		wantHin string
	}{
		{
			name:  "missing-locally",
			local: &FileIndex{Files: map[string]FileEntry{}},
			remote: &FileIndex{Files: map[string]FileEntry{
				"new.txt": {Size: 1, SHA256: testHash("n"), Sequence: 5, PrevPath: "old.txt"},
			}},
			wantHin: "old.txt",
		},
		{
			name: "ancestor-remote-only-change",
			local: &FileIndex{Files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("base"), MtimeNS: 100, Sequence: 1},
			}},
			remote: &FileIndex{Files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("r"), MtimeNS: 200, Sequence: 6, PrevPath: "was.txt"},
			}},
			baseHsh: map[string]Hash256{"f.txt": testHash("base")},
			wantHin: "was.txt",
		},
		{
			name: "mtime-fallback-remote-newer",
			local: &FileIndex{Files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("l"), MtimeNS: 100, Sequence: 1},
			}},
			remote: &FileIndex{Files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("r"), MtimeNS: 200, Sequence: 6, PrevPath: "earlier.txt"},
			}},
			lastSyn: 150, // local mtime 100 <= 150 → download path
			wantHin: "earlier.txt",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.local.diff(tc.remote, 0, tc.lastSyn, tc.baseHsh, "send-receive")
			if len(got) != 1 {
				t.Fatalf("got %d actions, want 1", len(got))
			}
			if got[0].Action != ActionDownload {
				t.Fatalf("Action=%v, want ActionDownload", got[0].Action)
			}
			if got[0].RemotePrevPath != tc.wantHin {
				t.Errorf("RemotePrevPath=%q, want %q", got[0].RemotePrevPath, tc.wantHin)
			}
		})
	}
}

// TestDiffOmitsPrevPathFromNonDownloads ensures the hint never leaks
// onto non-download actions — only ActionDownload uses it, and
// attaching it to a Delete or Conflict would confuse downstream
// consumers that assume the field is action-specific.
func TestDiffOmitsPrevPathFromNonDownloads(t *testing.T) {
	t.Parallel()
	local := &FileIndex{Files: map[string]FileEntry{
		"a.txt": {Size: 1, SHA256: testHash("l"), MtimeNS: 100, Sequence: 1},
	}}
	remote := &FileIndex{Files: map[string]FileEntry{
		"a.txt": {Deleted: true, Sequence: 7, PrevPath: "stale.txt"},
	}}
	// lastSeenSeq>0 and local not modified since lastSync → delete propagates.
	got := local.diff(remote, 3, 200, nil, "send-receive")
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1", len(got))
	}
	if got[0].Action != ActionDelete {
		t.Fatalf("Action=%v, want ActionDelete", got[0].Action)
	}
	if got[0].RemotePrevPath != "" {
		t.Errorf("RemotePrevPath=%q on ActionDelete, want empty", got[0].RemotePrevPath)
	}
}
