package filesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
)

// observeInode returns the platform-native file identifier for cross-
// checking scan bookkeeping against the filesystem. Uses Unix stat when
// that yields a non-zero value; otherwise opens a handle and reads the
// Windows NT file index via inodeFromFile. Fails the test if neither
// mechanism produces a value so the caller does not silently compare
// against zero.
func observeInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if ino := inodeOf(info); ino != 0 {
		return ino
	}
	f, err := os.Open(path) //nolint:gosec // G304: test-only path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ino := inodeFromFile(f)
	if ino == 0 {
		t.Fatalf("observeInode(%s): platform yielded no identifier", path)
	}
	return ino
}

// TestInodePopulatedDuringScan verifies that the scan records a stable
// file identifier on every indexed file across platforms: Unix reads it
// from stat during the walk, Windows extracts it from the open handle
// during the hash phase (R1 Phase 2 Step 5). The inode is the signal
// R1 Phase 2 uses to detect local renames without a re-transfer, so
// populating it correctly during scan is the foundation for everything
// that follows.
func TestInodePopulatedDuringScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "alpha.txt", "one")
	writeFile(t, dir, "sub/beta.txt", "two")

	idx := newFileIndex()
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"alpha.txt", "sub/beta.txt"} {
		entry, ok := idx.Get(rel)
		if !ok {
			t.Fatalf("entry missing: %s", rel)
		}
		if entry.Inode == 0 {
			t.Errorf("%s: Inode=0, want populated identifier", rel)
			continue
		}
		// Cross-check: the recorded inode must match what the platform
		// reports for the same file on disk. This pins scan behaviour
		// to the real filesystem rather than trusting scan bookkeeping
		// in isolation.
		got := observeInode(t, filepath.Join(dir, rel))
		if got != entry.Inode {
			t.Errorf("%s: scan recorded Inode=%d, platform sees %d", rel, entry.Inode, got)
		}
	}
}

// TestInodeStableAcrossRescan pins the invariant that a rescan of an
// unchanged tree preserves the inode observed on the first pass. This
// matters because rename detection compares inodes across consecutive
// scans — a spurious change would produce false-positive rename
// candidates.
func TestInodeStableAcrossRescan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "stable.txt", "content")

	idx := newFileIndex()
	for pass := range 2 {
		_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	want := observeInode(t, filepath.Join(dir, "stable.txt"))
	entry, _ := idx.Get("stable.txt")
	if entry.Inode != want {
		t.Errorf("after rescan Inode=%d, want %d (fast-path must preserve inode)", entry.Inode, want)
	}
}

// TestInodeBackfillAfterMigration verifies the migration path from a
// pre-Inode version (where every Inode==0): on rescan the entry gains a
// populated Inode without any content change. Unix backfills during the
// fast path using the stat inode; Windows cannot see the NT file index
// until a file handle is opened, so the scan falls through to the hash
// phase for that entry and inodeFromFile populates it. Either route is
// acceptable — what matters is that a subsequent scan has a non-zero
// Inode available for rename detection.
func TestInodeBackfillAfterMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "legacy.txt", "payload")

	idx := newFileIndex()
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-Inode persisted entry: wipe the recorded inode
	// but keep the size/mtime/hash as a real scan would have produced.
	entry, _ := idx.Get("legacy.txt")
	if entry.Inode == 0 {
		t.Fatal("precondition failed: scan should have populated Inode")
	}
	entry.Inode = 0
	idx.Set("legacy.txt", entry)

	// Rescan — either fast-path backfill (Unix) or forced hash-phase
	// inode extraction (Windows) must produce a non-zero Inode.
	_, _, _, _, _, err = idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := idx.Get("legacy.txt")
	if got.Inode == 0 {
		t.Error("scan did not backfill Inode — migration from pre-Inode indexes will not happen until content changes")
	}
	want := observeInode(t, filepath.Join(dir, "legacy.txt"))
	if got.Inode != want {
		t.Errorf("backfilled Inode=%d, want %d (platform-observed)", got.Inode, want)
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
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old/name.txt", "content stays the same")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	oldEntry, _ := idx.Get("old/name.txt")
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

	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	if stats.RenamesDetected != 1 {
		t.Errorf("RenamesDetected=%d, want 1", stats.RenamesDetected)
	}
	if stats.Deletions != 1 {
		t.Errorf("Deletions=%d, want 1 (tombstone still emitted for capability-less peers)", stats.Deletions)
	}

	newEntry, ok := idx.Get("new/name.txt")
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

	tomb, ok := idx.Get("old/name.txt")
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
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "source.bin", "original bytes")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	oldHash := idx.Files()["source.bin"].SHA256
	oldInode := idx.Files()["source.bin"].Inode

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

	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	if stats.RenamesDetected != 1 {
		t.Errorf("RenamesDetected=%d, want 1", stats.RenamesDetected)
	}

	newEntry, _ := idx.Get("target.bin")
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
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "payload")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	if err := os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	if idx.Files()["b.txt"].PrevPath != "a.txt" {
		t.Fatal("precondition failed: second scan must set PrevPath")
	}
	idx.recomputeCache()

	// Third scan: nothing changed on disk. PrevPath must be cleared
	// because the hint has already been emitted.
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	got := idx.Files()["b.txt"].PrevPath
	if got != "" {
		t.Errorf("PrevPath=%q after stable rescan, want empty (hint is single-use)", got)
	}
}

// TestScanRenameIgnoresUnchangedPaths guards against a false
// positive: the tombstone loop must only pair deletions with *new*
// paths. A file that keeps the same path (unchanged or edited) must
// never get its own path tagged as PrevPath.
func TestScanRenameIgnoresUnchangedPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "keep")

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()

	// Edit the file in place — same path, same inode, new content.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep edited payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RenamesDetected != 0 {
		t.Errorf("RenamesDetected=%d on in-place edit, want 0", stats.RenamesDetected)
	}
	if got := idx.Files()["keep.txt"].PrevPath; got != "" {
		t.Errorf("PrevPath=%q for in-place edit, want empty", got)
	}
}

// TestPrevPathRoundTripsThroughProto pins the Step 3 wire contract:
// when the sender has tagged a FileEntry with PrevPath, that hint
// survives marshal via buildIndexExchange and demarshal via
// protoToFileIndex. This is the bridge between local detection and
// peer-visible behaviour.
func TestPrevPathRoundTripsThroughProto(t *testing.T) {
	idx := &FileIndex{
		Sequence: 2,
		files: map[string]FileEntry{
			"old.txt": {Deleted: true, Sequence: 1, MtimeNS: 100},
			"new.txt": {
				Size: 5, MtimeNS: 200, SHA256: testHash("zzz"), Sequence: 2,
				Inode: 42, PrevPath: "old.txt",
			},
		},
	}
	fs := &folderState{index: idx}
	attachSQLiteForTest(t, fs, "f")
	n := &Node{
		deviceID: "dev",
		folders:  map[string]*folderState{"f": fs},
	}

	// Both the delta path and the full path must emit prev_path.
	// Delta path: since>0; full path: since==0.
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
		got, _ := decoded.Get("new.txt")
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
			local: &FileIndex{files: map[string]FileEntry{}},
			remote: &FileIndex{files: map[string]FileEntry{
				"new.txt": {Size: 1, SHA256: testHash("n"), Sequence: 5, PrevPath: "old.txt"},
			}},
			wantHin: "old.txt",
		},
		{
			name: "ancestor-remote-only-change",
			local: &FileIndex{files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("base"), MtimeNS: 100, Sequence: 1},
			}},
			remote: &FileIndex{files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("r"), MtimeNS: 200, Sequence: 6, PrevPath: "was.txt"},
			}},
			baseHsh: map[string]Hash256{"f.txt": testHash("base")},
			wantHin: "was.txt",
		},
		{
			name: "mtime-fallback-remote-newer",
			local: &FileIndex{files: map[string]FileEntry{
				"f.txt": {Size: 1, SHA256: testHash("l"), MtimeNS: 100, Sequence: 1},
			}},
			remote: &FileIndex{files: map[string]FileEntry{
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
	local := &FileIndex{files: map[string]FileEntry{
		"a.txt": {Size: 1, SHA256: testHash("l"), MtimeNS: 100, Sequence: 1},
	}}
	remote := &FileIndex{files: map[string]FileEntry{
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

// newHintRenameFolderState builds a folderState backed by a real os.Root
// and a seeded FileIndex so applyHintRenames can be exercised end-to-end
// against an actual filesystem. Returned state is safe to mutate; the
// root is closed via t.Cleanup.
func newHintRenameFolderState(t *testing.T, dir string, seed map[string]FileEntry) *folderState {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	idx := newFileIndex()
	for path, entry := range seed {
		idx.Set(path, entry)
		if entry.Sequence > idx.Sequence {
			idx.Sequence = entry.Sequence
		}
	}
	idx.Path = dir
	return &folderState{
		index:    idx,
		inFlight: map[string]bool{},
		retries:  retryTracker{},
		root:     root,
	}
}

// TestApplyHintRenamesHappyPath pins the R1 Phase 2 Step 4 receiver
// flow: when the sender tags an ActionDownload with RemotePrevPath and
// a matching ActionDelete is present, the local file is renamed in
// place, the OldPath is tombstoned, and renamedPaths[OldPath] is set so
// the delete action is skipped while the download still runs and can
// take the /delta fast path.
func TestApplyHintRenamesHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old.txt", "original-content")

	fs := newHintRenameFolderState(t, dir, map[string]FileEntry{
		"old.txt": {Size: 16, SHA256: testHash("original-content"), MtimeNS: 100, Sequence: 5, Inode: 42},
	})

	actions := []DiffEntry{
		{Path: "old.txt", Action: ActionDelete},
		{Path: "new.txt", Action: ActionDownload, RemotePrevPath: "old.txt"},
	}
	renamedPaths := map[string]bool{}

	fs.applyHintRenames(context.Background(), "f1", "peer:1", actions, renamedPaths)

	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old.txt still present on disk: err=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatalf("read new.txt: %v", err)
	}
	if string(got) != "original-content" {
		t.Errorf("new.txt content=%q, want %q", got, "original-content")
	}
	if !renamedPaths["old.txt"] {
		t.Errorf("renamedPaths[old.txt]=false, want true (delete must be skipped)")
	}
	if renamedPaths["new.txt"] {
		t.Errorf("renamedPaths[new.txt]=true, want false (download must still run for /delta)")
	}
	oldEntry, ok := fs.index.Get("old.txt")
	if !ok {
		t.Fatalf("old.txt entry removed from index, want tombstone")
	}
	if !oldEntry.Deleted {
		t.Errorf("old.txt entry: Deleted=false, want tombstone")
	}
	if oldEntry.Sequence <= 5 {
		t.Errorf("old.txt Sequence=%d, want >5 (bumped on tombstone)", oldEntry.Sequence)
	}
	if got := fs.metrics.FilesRenamed.Load(); got != 1 {
		t.Errorf("FilesRenamed=%d, want 1", got)
	}
}

// TestApplyHintRenamesSkipsWithoutMatchingDelete guards against stale
// hints: an ActionDownload carrying RemotePrevPath with no matching
// ActionDelete (e.g. the peer already retracted the delete, or this is
// a fresh-device first sync) must leave the local filesystem untouched.
func TestApplyHintRenamesSkipsWithoutMatchingDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old.txt", "untouched")

	fs := newHintRenameFolderState(t, dir, map[string]FileEntry{
		"old.txt": {Size: 9, SHA256: testHash("untouched"), MtimeNS: 100, Sequence: 3, Inode: 42},
	})

	// No ActionDelete for "old.txt" — orphan hint.
	actions := []DiffEntry{
		{Path: "new.txt", Action: ActionDownload, RemotePrevPath: "old.txt"},
	}
	renamedPaths := map[string]bool{}

	fs.applyHintRenames(context.Background(), "f1", "peer:1", actions, renamedPaths)

	if _, err := os.Stat(filepath.Join(dir, "old.txt")); err != nil {
		t.Errorf("old.txt vanished without matching delete: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt was created by hint pass: err=%v", err)
	}
	if len(renamedPaths) != 0 {
		t.Errorf("renamedPaths=%v, want empty", renamedPaths)
	}
	if got := fs.metrics.FilesRenamed.Load(); got != 0 {
		t.Errorf("FilesRenamed=%d, want 0", got)
	}
}

// TestApplyHintRenamesDoesNotClobberExistingNewPath pins drift safety:
// if the target NewPath already exists on disk (e.g. local create raced
// the sync), the hint pass must leave both files alone so the normal
// download flow can surface the conflict.
func TestApplyHintRenamesDoesNotClobberExistingNewPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old.txt", "source")
	writeFile(t, dir, "new.txt", "pre-existing")

	fs := newHintRenameFolderState(t, dir, map[string]FileEntry{
		"old.txt": {Size: 6, SHA256: testHash("source"), MtimeNS: 100, Sequence: 5, Inode: 42},
	})

	actions := []DiffEntry{
		{Path: "old.txt", Action: ActionDelete},
		{Path: "new.txt", Action: ActionDownload, RemotePrevPath: "old.txt"},
	}
	renamedPaths := map[string]bool{}

	fs.applyHintRenames(context.Background(), "f1", "peer:1", actions, renamedPaths)

	if b, err := os.ReadFile(filepath.Join(dir, "old.txt")); err != nil || string(b) != "source" {
		t.Errorf("old.txt disturbed: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "new.txt")); err != nil || string(b) != "pre-existing" {
		t.Errorf("new.txt clobbered: content=%q err=%v", b, err)
	}
	if len(renamedPaths) != 0 {
		t.Errorf("renamedPaths=%v, want empty", renamedPaths)
	}
	if got := fs.metrics.FilesRenamed.Load(); got != 0 {
		t.Errorf("FilesRenamed=%d, want 0", got)
	}
}

// TestApplyHintRenamesFallsBackWhenOldPathMissing covers the case where
// the index claims OldPath exists but it is actually absent from disk
// (e.g. manual user deletion between scan and sync). Rename must fail
// safely: no tombstone, no renamedPaths entry, no metric bump, so the
// normal download still runs and the index reconciles on the next scan.
func TestApplyHintRenamesFallsBackWhenOldPathMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Index says old.txt exists but we never write it to disk.

	fs := newHintRenameFolderState(t, dir, map[string]FileEntry{
		"old.txt": {Size: 3, SHA256: testHash("abc"), MtimeNS: 100, Sequence: 5, Inode: 42},
	})

	actions := []DiffEntry{
		{Path: "old.txt", Action: ActionDelete},
		{Path: "new.txt", Action: ActionDownload, RemotePrevPath: "old.txt"},
	}
	renamedPaths := map[string]bool{}

	fs.applyHintRenames(context.Background(), "f1", "peer:1", actions, renamedPaths)

	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt materialised from a failed rename: err=%v", err)
	}
	if len(renamedPaths) != 0 {
		t.Errorf("renamedPaths=%v, want empty on rename failure", renamedPaths)
	}
	if entry, _ := fs.index.Get("old.txt"); entry.Deleted {
		t.Errorf("old.txt tombstoned despite rename failure")
	}
	if got := fs.metrics.FilesRenamed.Load(); got != 0 {
		t.Errorf("FilesRenamed=%d, want 0 on failure", got)
	}
}

// TestApplyHintRenamesSkipsPathsClaimedByPlanRenames guards composability
// with the earlier planRenames pass: if a path is already in
// renamedPaths, the hint pass must leave it alone so the planRenames
// branch's tombstone and metric remain authoritative.
func TestApplyHintRenamesSkipsPathsClaimedByPlanRenames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old.txt", "stay-put")

	fs := newHintRenameFolderState(t, dir, map[string]FileEntry{
		"old.txt": {Size: 8, SHA256: testHash("stay-put"), MtimeNS: 100, Sequence: 5, Inode: 42},
	})

	actions := []DiffEntry{
		{Path: "old.txt", Action: ActionDelete},
		{Path: "new.txt", Action: ActionDownload, RemotePrevPath: "old.txt"},
	}
	// planRenames already claimed the pair — hint pass must no-op.
	renamedPaths := map[string]bool{"old.txt": true, "new.txt": true}

	fs.applyHintRenames(context.Background(), "f1", "peer:1", actions, renamedPaths)

	if b, err := os.ReadFile(filepath.Join(dir, "old.txt")); err != nil || string(b) != "stay-put" {
		t.Errorf("old.txt disturbed by hint after planRenames claim: content=%q err=%v", b, err)
	}
	if entry, _ := fs.index.Get("old.txt"); entry.Deleted {
		t.Errorf("hint pass tombstoned a planRenames-owned path")
	}
	if got := fs.metrics.FilesRenamed.Load(); got != 0 {
		t.Errorf("FilesRenamed=%d, want 0 (planRenames accounts for its own)", got)
	}
}

// TestIsRenameHintLikely pins the size-gate contract that filters
// inode-reuse false positives out of R1 Phase 2 rename hints. The
// helper rejects inode-matched pairs whose sizes differ by more than
// an order of magnitude in either direction; similar-size pairs pass
// through regardless of absolute size. See the doc comment on
// isRenameHintLikely for the rationale.
func TestIsRenameHintLikely(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		oldSize int64
		newSize int64
		want    bool
	}{
		{"tiny rename is accepted", 7, 7, true},
		{"near-equal small sizes are accepted", 900, 1_000, true},
		{"near-equal large sizes are accepted", 500_000, 480_000, true},
		{"old size zero passes", 0, 1_000_000, true},
		{"new size zero passes", 1_000_000, 0, true},
		{"both zero is accepted", 0, 0, true},
		{"just under 10x shrink is accepted", 900_000, 100_000, true},
		{"just under 10x growth is accepted", 100_000, 900_000, true},
		{"exactly 10x shrink stays inside the gate", 10_000_000, 1_000_000, true},
		{"exactly 10x growth stays inside the gate", 1_000_000, 10_000_000, true},
		{"over 10x shrink is rejected", 11_000_000, 1_000_000, false},
		{"over 10x growth is rejected", 1_000_000, 11_000_000, false},
		{"huge old small new: classic ext4 reuse", 1_000_000_000, 1_000_000, false},
		{"small old huge new: inverse reuse", 1_000, 1_000_000_000, false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := isRenameHintLikely(c.oldSize, c.newSize)
			if got != c.want {
				t.Errorf("isRenameHintLikely(%d, %d) = %v, want %v",
					c.oldSize, c.newSize, got, c.want)
			}
		})
	}
}

// TestScanSkipsRenameHintOnSizeMismatch exercises the scan-level gate
// that protects against inode-reuse false positives. When a deleted
// file and a new file share an inode but the new file's size differs
// from the old by more than an order of magnitude, the hint is
// suppressed — that pattern is almost always an ext4-style inode
// reuse, and emitting a hint for it only buys a wasted block-sig
// round-trip before the receiver falls back to a full download.
//
// Reproducing a genuine kernel inode recycle inside a test is
// platform-dependent and flaky, so this scenario uses a real rename
// followed by an aggressive truncate. The kernel preserves the inode
// across rename; the truncate pushes the size ratio past the 10x
// gate. Either interpretation (real rename with massive edit;
// hypothetical inode reuse) is correctly handled by skipping the
// hint.
func TestScanSkipsRenameHintOnSizeMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Old file: 1 MiB. New file after rename + truncate: 4 bytes.
	// That is a ~262_000x shrink, well past the 10x gate.
	large := make([]byte, 1024*1024)
	for i := range large {
		large[i] = byte(i)
	}
	fullOld := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(fullOld, large, 0o600); err != nil {
		t.Fatal(err)
	}

	idx := newFileIndex()
	if _, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil); err != nil {
		t.Fatal(err)
	}
	idx.recomputeCache()
	if idx.Files()["large.bin"].Inode == 0 {
		t.Fatal("precondition failed: first scan must populate inode")
	}

	// Rename preserves the inode; a fresh write truncates to a size
	// that defeats the ratio gate.
	fullNew := filepath.Join(dir, "small.bin")
	if err := os.Rename(fullOld, fullNew); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullNew, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, newIgnoreMatcher(nil), defaultMaxIndexFiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	if stats.RenamesDetected != 0 {
		t.Errorf("RenamesDetected=%d, want 0 (size-gate must suppress hint)", stats.RenamesDetected)
	}
	if stats.RenameHintsSkipped != 1 {
		t.Errorf("RenameHintsSkipped=%d, want 1", stats.RenameHintsSkipped)
	}
	if got := idx.Files()["small.bin"].PrevPath; got != "" {
		t.Errorf("PrevPath=%q, want empty (gate must clear the hint)", got)
	}
	if tomb, ok := idx.Get("large.bin"); !ok || !tomb.Deleted {
		t.Error("old path tombstone not emitted")
	}
}
