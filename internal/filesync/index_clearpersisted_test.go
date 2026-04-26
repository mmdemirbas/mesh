package filesync

import "testing"

// TestClearPersisted_KeepsPathsAddedAfterSnapshot pins the C1 fix from
// the 2026-04-26 deep review. persistFolder snapshots DirtyPaths, drops
// the index lock for the SQLite commit, then must clear ONLY the paths
// it just persisted. The blanket ClearDirty would silently drop a path
// that another goroutine marked dirty during the commit window, leaving
// SQLite permanently behind the in-memory index — peers would never see
// the row, the dirty flag stays cleared, and no error fires.
func TestClearPersisted_KeepsPathsAddedAfterSnapshot(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()

	a := FileEntry{Size: 1, MtimeNS: 1, SHA256: testHash("a"), Sequence: 1}
	b := FileEntry{Size: 1, MtimeNS: 2, SHA256: testHash("b"), Sequence: 2}
	if err := idx.Set("a.txt", a); err != nil {
		t.Fatalf("Set a.txt: %v", err)
	}
	if err := idx.Set("b.txt", b); err != nil {
		t.Fatalf("Set b.txt: %v", err)
	}

	// Snapshot reflects what persistFolder is about to commit.
	dirty, deleted := idx.DirtyPaths()
	snap := idx.clone()
	snap.dirty = dirty
	snap.deleted = deleted

	// Concurrent path C marked dirty AFTER the snapshot, BEFORE the
	// post-commit clear. Models the scan / sync goroutine that
	// mutates the live index while the SQLite write is in flight.
	c := FileEntry{Size: 1, MtimeNS: 3, SHA256: testHash("c"), Sequence: 3}
	if err := idx.Set("c.txt", c); err != nil {
		t.Fatalf("Set c.txt: %v", err)
	}

	idx.ClearPersisted(snap, dirty, deleted)

	stillDirty, _ := idx.DirtyPaths()
	if _, ok := stillDirty["c.txt"]; !ok {
		t.Fatal("c.txt was added to dirty AFTER the snapshot; ClearPersisted must not remove it")
	}
	if _, ok := stillDirty["a.txt"]; ok {
		t.Error("a.txt was in the snapshot and unchanged after; should be cleared")
	}
	if _, ok := stillDirty["b.txt"]; ok {
		t.Error("b.txt was in the snapshot and unchanged after; should be cleared")
	}
}

// TestClearPersisted_KeepsPathMutatedDuringCommit covers the second
// failure mode the fix has to handle: a path that was both in the
// snapshot AND mutated again before the post-commit clear. The new
// in-memory value has not yet reached SQLite, so the dirty marker
// must stay set until a future persist cycle commits the new value.
func TestClearPersisted_KeepsPathMutatedDuringCommit(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()

	v1 := FileEntry{Size: 1, MtimeNS: 1, SHA256: testHash("v1"), Sequence: 1}
	if err := idx.Set("a.txt", v1); err != nil {
		t.Fatalf("Set v1: %v", err)
	}
	dirty, deleted := idx.DirtyPaths()
	snap := idx.clone()
	snap.dirty = dirty
	snap.deleted = deleted

	// Live mutation while the commit was in flight.
	v2 := FileEntry{Size: 2, MtimeNS: 2, SHA256: testHash("v2"), Sequence: 2}
	if err := idx.Set("a.txt", v2); err != nil {
		t.Fatalf("Set v2: %v", err)
	}

	idx.ClearPersisted(snap, dirty, deleted)

	stillDirty, _ := idx.DirtyPaths()
	if _, ok := stillDirty["a.txt"]; !ok {
		t.Fatal("a.txt was modified between snapshot and clear; the new value is unpersisted, so dirty must stay set")
	}
}
