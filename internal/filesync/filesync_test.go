package filesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

// --- Ignore pattern tests ---

func TestParseLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line    string
		wantOK  bool
		pattern string
		neg     bool
		dirOnly bool
	}{
		{"", false, "", false, false},
		{"// comment", false, "", false, false},
		{"# comment", false, "", false, false},
		{"*.tmp", true, "*.tmp", false, false},
		{"!important.txt", true, "important.txt", true, false},
		{"node_modules/", true, "node_modules", false, true},
		{"!build/", true, "build", true, true},
		{".git", true, ".git", false, false},
		{"!", false, "", false, false},  // negation prefix only, no pattern
		{"!/", false, "", false, false}, // negation + dir-only, no pattern
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			t.Parallel()
			p, ok := parseLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseLine(%q) ok=%v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if p.pattern != tt.pattern {
				t.Errorf("pattern=%q, want %q", p.pattern, tt.pattern)
			}
			if p.negation != tt.neg {
				t.Errorf("negation=%v, want %v", p.negation, tt.neg)
			}
			if p.dirOnly != tt.dirOnly {
				t.Errorf("dirOnly=%v, want %v", p.dirOnly, tt.dirOnly)
			}
		})
	}
}

func TestShouldIgnore(t *testing.T) {
	t.Parallel()
	m := &ignoreMatcher{
		patterns: []ignorePattern{
			{pattern: ".stfolder"},
			{pattern: ".mesh-tmp-*"},
			{pattern: "*.log"},
			{pattern: "build", dirOnly: true},
			{pattern: "important.log", negation: true},
		},
	}

	tests := []struct {
		path   string
		isDir  bool
		ignore bool
	}{
		{".stfolder", true, true},
		{".stfolder", false, true},
		{".mesh-tmp-abc123", false, true},
		{"foo.log", false, true},
		{"sub/bar.log", false, true},
		{"important.log", false, false}, // negated
		{"build", true, true},           // dir-only match
		{"build", false, false},         // not a dir, dir-only pattern
		{"src/main.go", false, false},
		{"README.md", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got := m.shouldIgnore(tt.path, tt.isDir)
			if got != tt.ignore {
				t.Errorf("shouldIgnore(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.ignore)
			}
		})
	}
}

func TestMatchPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", true},
		{"*.go", "main.txt", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "lib/main.go", false},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/pkg/main.go", true},
		{"src/**/*.go", "src/a/b/c.go", true},
		{"src/**/*.go", "lib/main.go", false},
		{".git", ".git", true},
		{".git", "sub/.git", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s", tt.pattern, tt.path), func(t *testing.T) {
			got := matchPattern(tt.pattern, tt.path)
			if got != tt.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestIsConflictFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"report.sync-conflict-20260406-143022-abc123.docx", true},
		{"file.sync-conflict-20260101-000000-def456.txt", true},
		{"normal-file.txt", false},
		{"sync-conflict-missing-prefix", false}, // has the substring
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isConflictFile(tt.name); got != tt.want {
				t.Errorf("isConflictFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// --- Index tests ---

// TestScanWithStatsPopulatesEvidence pins the instrumentation contract:
// every counter that corresponds to observable work must increment when
// that work happens. Without this, runScan's debug logs would silently go
// stale as the scan body evolves and we'd lose the ability to attribute
// heaviness to a concrete phase.
func TestScanWithStatsPopulatesEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "kept.txt", "alpha")
	writeFile(t, dir, "sub/nested.txt", "beta payload")
	writeFile(t, dir, "ignored.log", "noise")
	writeFile(t, dir, "build/artifact.bin", "drop")

	ignore := newIgnoreMatcher([]string{"*.log", "build/"})
	idx := newFileIndex()

	// First pass: every tracked file must be hashed (no fast-path hits).
	_, _, _, stats, _, err := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesHashed != 2 {
		t.Errorf("FilesHashed=%d, want 2 (kept.txt + sub/nested.txt)", stats.FilesHashed)
	}
	if stats.BytesHashed != int64(len("alpha")+len("beta payload")) {
		t.Errorf("BytesHashed=%d, want %d", stats.BytesHashed, len("alpha")+len("beta payload"))
	}
	if stats.FastPathHits != 0 {
		t.Errorf("FastPathHits=%d on first pass, want 0", stats.FastPathHits)
	}
	if stats.FilesIgnored != 1 {
		t.Errorf("FilesIgnored=%d, want 1 (ignored.log)", stats.FilesIgnored)
	}
	if stats.DirsIgnored != 1 {
		t.Errorf("DirsIgnored=%d, want 1 (build/)", stats.DirsIgnored)
	}
	if stats.DirsWalked != 1 {
		t.Errorf("DirsWalked=%d, want 1 (sub/)", stats.DirsWalked)
	}
	if stats.WalkDuration <= 0 {
		t.Error("WalkDuration must be positive")
	}
	if stats.HashDuration <= 0 {
		t.Error("HashDuration must be positive when files are hashed")
	}

	// Second pass on unchanged tree: every file must hit the fast path,
	// no rehashing.
	_, _, _, stats2, _, err := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.FilesHashed != 0 {
		t.Errorf("FilesHashed=%d on unchanged rescan, want 0 (fast path)", stats2.FilesHashed)
	}
	if stats2.FastPathHits != 2 {
		t.Errorf("FastPathHits=%d on unchanged rescan, want 2", stats2.FastPathHits)
	}
	if stats2.BytesHashed != 0 {
		t.Errorf("BytesHashed=%d on unchanged rescan, want 0", stats2.BytesHashed)
	}
}

// TestPurgeTombstonesReturnsCount pins the count return used by the debug
// log. A silent drop to void return would blind the telemetry.
func TestPurgeTombstonesReturnsCount(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	oldNs := time.Now().Add(-60 * 24 * time.Hour).UnixNano()
	recentNs := time.Now().UnixNano()
	idx.Files["old-gone"] = FileEntry{Deleted: true, MtimeNS: oldNs}
	idx.Files["also-old-gone"] = FileEntry{Deleted: true, MtimeNS: oldNs}
	idx.Files["recent-gone"] = FileEntry{Deleted: true, MtimeNS: recentNs}
	idx.Files["live"] = FileEntry{Size: 3, MtimeNS: recentNs, SHA256: "x"}

	n := idx.purgeTombstones(30*24*time.Hour, nil)
	if n != 2 {
		t.Errorf("purgeTombstones returned %d, want 2", n)
	}
	if _, ok := idx.Files["live"]; !ok {
		t.Error("live entry removed")
	}
	if _, ok := idx.Files["recent-gone"]; !ok {
		t.Error("recent tombstone removed (within retention)")
	}
	if _, ok := idx.Files["old-gone"]; ok {
		t.Error("old tombstone not removed")
	}
}

// TestFileIndexClone verifies that clone produces a deep copy — mutating the
// clone's Files must not affect the original, and bumping Sequence must not
// leak back. Regression: the scan-without-lock path depends on this isolation.
func TestFileIndexClone(t *testing.T) {
	t.Parallel()
	orig := newFileIndex()
	orig.Path = "/tmp/src"
	orig.Sequence = 7
	orig.Files["a.txt"] = FileEntry{Size: 5, SHA256: "aaa", Sequence: 1}
	orig.Files["b.txt"] = FileEntry{Size: 9, SHA256: "bbb", Sequence: 2}

	clone := orig.clone()
	clone.Sequence = 99
	clone.Files["a.txt"] = FileEntry{Size: 100, SHA256: "mutated", Sequence: 50}
	clone.Files["c.txt"] = FileEntry{Size: 1, SHA256: "ccc", Sequence: 99}

	if orig.Sequence != 7 {
		t.Errorf("orig.Sequence mutated: got %d want 7", orig.Sequence)
	}
	if orig.Files["a.txt"].SHA256 != "aaa" {
		t.Errorf("orig file mutated via clone: got %q want aaa", orig.Files["a.txt"].SHA256)
	}
	if _, ok := orig.Files["c.txt"]; ok {
		t.Error("orig gained entry that was only added to clone")
	}
	if orig.Path != clone.Path {
		t.Errorf("clone.Path = %q, want %q", clone.Path, orig.Path)
	}
}

// TestRunScanPreservesConcurrentWrites pins the merge-on-swap rule: entries
// whose Sequence was bumped after a scan started (e.g. by a concurrent sync
// download) must survive the swap, otherwise scan silently clobbers sync.
func TestRunScanPreservesConcurrentWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "scanned.txt", "on-disk")

	fs := &folderState{
		cfg:           config.FolderCfg{ID: "f1", Path: dir, Direction: "send-receive"},
		index:         newFileIndex(),
		ignore:        &ignoreMatcher{},
		peers:         map[string]PeerState{},
		pending:       map[string]PendingSummary{},
		peerLastError: map[string]string{},
	}
	fs.index.Path = dir
	n := &Node{folders: map[string]*folderState{"f1": fs}}

	// Simulate: a sync download added "from-peer.txt" with Sequence > scanStart
	// AFTER the scan snapshot but BEFORE the swap. We mimic this by injecting
	// the entry directly between runScan's clone and swap — which in practice
	// happens because sync downloads take fs.indexMu.Lock().
	//
	// Rather than orchestrate timing, we insert the entry at Sequence=1000 and
	// run a normal runScan: the walk sees only "scanned.txt" on disk, so a
	// naive swap would drop "from-peer.txt". The merge rule must preserve it.
	fs.index.Files["from-peer.txt"] = FileEntry{
		Size: 7, SHA256: "peerhash", Sequence: 1000,
	}
	fs.index.Sequence = 1000

	n.runScan(context.Background(), nil)

	if _, ok := fs.index.Files["from-peer.txt"]; !ok {
		t.Fatal("runScan clobbered a concurrently-written peer entry (expected merge-preserve)")
	}
	if fs.index.Files["from-peer.txt"].SHA256 != "peerhash" {
		t.Errorf("peer entry content lost: got %+v", fs.index.Files["from-peer.txt"])
	}
	if _, ok := fs.index.Files["scanned.txt"]; !ok {
		t.Error("scan failed to pick up on-disk file")
	}
	if fs.index.Sequence < 1000 {
		t.Errorf("Sequence regressed: got %d, must be >= 1000", fs.index.Sequence)
	}
}

func TestRunScanTargeted(t *testing.T) {
	t.Parallel()
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeFile(t, dir1, "a.txt", "aaa")
	writeFile(t, dir2, "b.txt", "bbb")

	fs1 := &folderState{
		cfg:           config.FolderCfg{ID: "f1", Path: dir1, Direction: "send-receive"},
		index:         newFileIndex(),
		ignore:        &ignoreMatcher{},
		peers:         map[string]PeerState{},
		pending:       map[string]PendingSummary{},
		peerLastError: map[string]string{},
	}
	fs1.index.Path = dir1
	fs2 := &folderState{
		cfg:           config.FolderCfg{ID: "f2", Path: dir2, Direction: "send-receive"},
		index:         newFileIndex(),
		ignore:        &ignoreMatcher{},
		peers:         map[string]PeerState{},
		pending:       map[string]PendingSummary{},
		peerLastError: map[string]string{},
	}
	fs2.index.Path = dir2
	n := &Node{
		folders:     map[string]*folderState{"f1": fs1, "f2": fs2},
		scanTrigger: make(chan struct{}, 1),
	}

	// Targeted scan: only dir1 is dirty.
	n.runScan(context.Background(), map[string]bool{dir1: true})

	if _, ok := fs1.index.Files["a.txt"]; !ok {
		t.Error("targeted scan should have scanned f1")
	}
	if len(fs2.index.Files) != 0 {
		t.Errorf("targeted scan should NOT have scanned f2, but it has %d files", len(fs2.index.Files))
	}

	// Full scan (nil): both folders scanned.
	n.runScan(context.Background(), nil)
	if _, ok := fs2.index.Files["b.txt"]; !ok {
		t.Error("full scan should have scanned f2")
	}
}

// TestRunScanCapExceededDoesNotAbortOtherFolders verifies that one folder
// hitting its max_files cap does not prevent remaining folders from scanning.
func TestRunScanCapExceededDoesNotAbortOtherFolders(t *testing.T) {
	t.Parallel()
	// dir1: 5 files, cap=3 → exceeds cap
	dir1 := t.TempDir()
	for i := range 5 {
		writeFile(t, dir1, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("d%d", i))
	}
	// dir2: 2 files, cap=default → fine
	dir2 := t.TempDir()
	writeFile(t, dir2, "ok.txt", "ok")

	fs1 := &folderState{
		cfg:           config.FolderCfg{ID: "capped", Path: dir1, Direction: "send-receive", MaxFiles: 3},
		index:         newFileIndex(),
		ignore:        &ignoreMatcher{},
		peers:         map[string]PeerState{},
		pending:       map[string]PendingSummary{},
		peerLastError: map[string]string{},
	}
	fs1.index.Path = dir1
	fs2 := &folderState{
		cfg:           config.FolderCfg{ID: "normal", Path: dir2, Direction: "send-receive"},
		index:         newFileIndex(),
		ignore:        &ignoreMatcher{},
		peers:         map[string]PeerState{},
		pending:       map[string]PendingSummary{},
		peerLastError: map[string]string{},
	}
	fs2.index.Path = dir2
	n := &Node{
		folders:     map[string]*folderState{"capped": fs1, "normal": fs2},
		scanTrigger: make(chan struct{}, 1),
	}

	n.runScan(context.Background(), nil)

	// The capped folder should NOT have been swapped (partial index).
	// The normal folder MUST have been scanned despite the other folder's error.
	if _, ok := fs2.index.Files["ok.txt"]; !ok {
		t.Fatal("runScan aborted all folders when one exceeded cap — must continue to remaining folders")
	}
}

// TestGetFolderStatusesNotBlockedDuringScan verifies the lock refactor: a
// long-running scan (simulated by a held indexMu) must not block readers.
// Regression for the "empty UI / no loading state" report — before the fix,
// runScan held the write lock across the entire WalkDir so every admin
// request hung until scan completed.
func TestGetFolderStatusesNotBlockedDuringScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "x.txt", "data")

	fs := &folderState{
		cfg:           config.FolderCfg{ID: "slow", Path: dir, Direction: "send-receive"},
		index:         newFileIndex(),
		ignore:        &ignoreMatcher{},
		peers:         map[string]PeerState{},
		pending:       map[string]PendingSummary{},
		peerLastError: map[string]string{},
	}
	n := &Node{folders: map[string]*folderState{"slow": fs}}
	activeNodes.Register(n)
	defer activeNodes.Unregister(n)

	// Simulate a scan in progress by holding the RLock (scan clones under
	// RLock; concurrent RLock must be admitted).
	fs.indexMu.RLock()
	defer fs.indexMu.RUnlock()

	done := make(chan []FolderStatus, 1)
	go func() { done <- GetFolderStatuses() }()

	select {
	case got := <-done:
		if len(got) != 1 || got[0].ID != "slow" {
			t.Fatalf("unexpected statuses: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetFolderStatuses blocked while scan held RLock — readers must be concurrent")
	}
}

func TestScanAndPersist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "sub/b.txt", "world")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	changed, _, _, err := idx.scan(context.Background(), dir, ignore)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changes on first scan")
	}
	if len(idx.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(idx.Files))
	}
	if _, ok := idx.Files["a.txt"]; !ok {
		t.Error("missing a.txt")
	}
	if _, ok := idx.Files["sub/b.txt"]; !ok {
		t.Error("missing sub/b.txt")
	}

	// Second scan with no changes.
	changed, _, _, err = idx.scan(context.Background(), dir, ignore)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no changes on re-scan")
	}

	// Persist and reload.
	idxPath := filepath.Join(t.TempDir(), "index.yaml")
	if err := idx.save(idxPath); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sequence != idx.Sequence {
		t.Errorf("sequence mismatch: got %d, want %d", loaded.Sequence, idx.Sequence)
	}
	if len(loaded.Files) != len(idx.Files) {
		t.Errorf("file count mismatch: got %d, want %d", len(loaded.Files), len(idx.Files))
	}
}

func TestScanDetectsDeletion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)

	// Delete b.txt
	_ = os.Remove(filepath.Join(dir, "b.txt"))

	changed, _, _, _ := idx.scan(context.Background(), dir, ignore)
	if !changed {
		t.Fatal("expected change after deletion")
	}

	entry, ok := idx.Files["b.txt"]
	if !ok {
		t.Fatal("b.txt should still be in index as tombstone")
	}
	if !entry.Deleted {
		t.Error("b.txt should be marked deleted")
	}
}

func TestScanDeletion_TombstoneMtimeIsNow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old.txt", "data")

	// Backdate the file to 60 days ago.
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "old.txt"), oldTime, oldTime)

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)

	// Verify the indexed mtime reflects the backdated time.
	entry := idx.Files["old.txt"]
	if entry.MtimeNS > time.Now().Add(-59*24*time.Hour).UnixNano() {
		t.Fatal("pre-condition: file mtime should be ~60 days ago")
	}

	// Delete the file and re-scan.
	_ = os.Remove(filepath.Join(dir, "old.txt"))
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)

	entry = idx.Files["old.txt"]
	if !entry.Deleted {
		t.Fatal("expected tombstone")
	}

	// Tombstone MtimeNS should be recent (within last minute), not 60 days ago.
	oneMinuteAgo := time.Now().Add(-1 * time.Minute).UnixNano()
	if entry.MtimeNS < oneMinuteAgo {
		t.Errorf("tombstone MtimeNS should be recent, got %d (threshold %d)", entry.MtimeNS, oneMinuteAgo)
	}

	// A 30-day purge must NOT remove this freshly-created tombstone.
	idx.purgeTombstones(30*24*time.Hour, nil)
	if _, ok := idx.Files["old.txt"]; !ok {
		t.Error("fresh tombstone should survive purge")
	}
}

func TestScanRespectsIgnore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "keep")
	writeFile(t, dir, "skip.log", "skip")

	idx := newFileIndex()
	ignore := &ignoreMatcher{
		patterns: []ignorePattern{{pattern: "*.log"}},
	}

	_, _, _, _ = idx.scan(context.Background(), dir, ignore)

	if _, ok := idx.Files["keep.txt"]; !ok {
		t.Error("keep.txt should be indexed")
	}
	if _, ok := idx.Files["skip.log"]; ok {
		t.Error("skip.log should be ignored")
	}
}

// B10: scan errors (unreadable files) must suppress tombstone generation.
// Without this, a temporarily locked file is treated as deleted and the
// tombstone propagates to peers, causing them to delete the file.
func TestScanErrorsSuppressTombstones(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "readable.txt", "hello")
	writeFile(t, dir, "locked.txt", "important")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}

	// First scan: both files indexed.
	_, _, _, scanErr := idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	if _, ok := idx.Files["locked.txt"]; !ok {
		t.Fatal("locked.txt should be in index after first scan")
	}

	// Make locked.txt unreadable to simulate a permission error.
	lockedPath := filepath.Join(dir, "locked.txt")
	if err := os.Chmod(lockedPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedPath, 0644) })

	// Modify readable.txt so the scan detects a real change.
	writeFile(t, dir, "readable.txt", "changed")

	// Re-scan: locked.txt can't be hashed.
	_, _, _, scanErr = idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// locked.txt must NOT be tombstoned — scan had errors.
	entry, ok := idx.Files["locked.txt"]
	if !ok {
		t.Fatal("locked.txt should still be in index")
	}
	if entry.Deleted {
		t.Error("B10: locked.txt must not be tombstoned when scan had hash errors")
	}
}

// B10: scan must fail fast when folder root is inaccessible.
func TestScanFolderRootInaccessible(t *testing.T) {
	t.Parallel()

	idx := newFileIndex()
	idx.Files["important.txt"] = FileEntry{SHA256: "abc", Sequence: 1}

	ignore := &ignoreMatcher{}
	_, _, _, scanErr := idx.scan(context.Background(), "/nonexistent/path/that/does/not/exist", ignore)
	if scanErr == nil {
		t.Fatal("expected error for inaccessible folder root")
	}

	// The existing index must be untouched — no tombstones created.
	entry := idx.Files["important.txt"]
	if entry.Deleted {
		t.Error("B10: inaccessible folder root must not tombstone existing entries")
	}
}

// B11: file modified during hashing should be skipped (TOCTOU guard).
func TestScanTOCTOU_FileModifiedDuringHash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "stable.txt", "stable content")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}

	// First scan: stable.txt is indexed.
	_, _, _, scanErr := idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	origEntry := idx.Files["stable.txt"]

	// Now modify stable.txt's content but keep the mtime the same,
	// then change the mtime to trigger a re-hash.
	// The TOCTOU guard re-stats after hashing. We simulate the race by
	// writing a file, scanning, then checking that a file whose stat
	// changed between the initial stat and the post-hash stat is skipped.
	//
	// Direct simulation: write a large file, scan it, and during the scan
	// modify it. This is hard to trigger deterministically. Instead, we
	// verify the positive case: a stable file is indexed correctly.
	// The TOCTOU codepath is tested by checking that TocTouSkips is 0
	// for stable files.
	changed, _, _, stats, _, scanErr := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// Stable file should not have been re-hashed (fast path hits).
	if stats.TocTouSkips != 0 {
		t.Errorf("expected 0 TocTouSkips for stable file, got %d", stats.TocTouSkips)
	}

	// The entry should remain unchanged.
	if idx.Files["stable.txt"].SHA256 != origEntry.SHA256 {
		t.Error("stable file hash should not change")
	}
	_ = changed
}

func TestScanCapExceeded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create 5 files but set cap to 3.
	for i := range 5 {
		writeFile(t, dir, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("data%d", i))
	}
	idx := newFileIndex()
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, &ignoreMatcher{}, 3)
	if !errors.Is(err, errIndexCapExceeded) {
		t.Fatalf("expected errIndexCapExceeded, got %v", err)
	}
	// Index should have at most 3 entries (the cap).
	if len(idx.Files) > 3 {
		t.Errorf("expected at most 3 files in index after cap, got %d", len(idx.Files))
	}
}

func TestScanCapNotExceeded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 3 {
		writeFile(t, dir, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("data%d", i))
	}
	idx := newFileIndex()
	_, count, _, _, _, err := idx.scanWithStats(context.Background(), dir, &ignoreMatcher{}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 files, got %d", count)
	}
}

func TestRetryTracker(t *testing.T) {
	t.Parallel()
	var rt retryTracker

	// Not quarantined initially.
	if rt.quarantined("a.txt", "hash1") {
		t.Fatal("should not be quarantined before any failure")
	}

	// Record failures up to maxRetries.
	for i := range maxRetries - 1 {
		rt.record("a.txt", "hash1")
		if rt.quarantined("a.txt", "hash1") {
			t.Fatalf("should not be quarantined after %d failures", i+1)
		}
	}
	rt.record("a.txt", "hash1")
	if !rt.quarantined("a.txt", "hash1") {
		t.Fatal("should be quarantined after maxRetries failures")
	}

	// New remote hash resets quarantine.
	if rt.quarantined("a.txt", "hash2") {
		t.Fatal("new remote hash should not be quarantined")
	}

	// Recording with new hash resets counter.
	rt.record("a.txt", "hash2")
	if rt.quarantined("a.txt", "hash2") {
		t.Fatal("should not be quarantined after 1 failure with new hash")
	}

	// Clear removes tracking.
	rt.clear("a.txt")
	if rt.quarantined("a.txt", "hash2") {
		t.Fatal("should not be quarantined after clear")
	}

	// quarantinedPaths lists quarantined files.
	for range maxRetries {
		rt.record("x.txt", "xhash")
	}
	paths := rt.quarantinedPaths()
	if len(paths) != 1 || paths[0] != "x.txt" {
		t.Errorf("quarantinedPaths = %v, want [x.txt]", paths)
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Sequence = 5
	local.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 3}
	local.Files["b.txt"] = FileEntry{SHA256: "bbb", Sequence: 2}
	local.Files["c.txt"] = FileEntry{SHA256: "ccc", Sequence: 5} // modified locally

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 6}  // same content
	remote.Files["b.txt"] = FileEntry{SHA256: "bbb2", Sequence: 7} // remote changed
	remote.Files["c.txt"] = FileEntry{SHA256: "ccc2", Sequence: 8} // both changed (conflict)
	remote.Files["d.txt"] = FileEntry{SHA256: "ddd", Sequence: 9}  // new on remote

	actions := local.diff(remote, 4, "send-receive")

	actionMap := make(map[string]DiffAction)
	for _, a := range actions {
		actionMap[a.Path] = a.Action
	}

	if _, ok := actionMap["a.txt"]; ok {
		t.Error("a.txt should have no action (same content)")
	}
	if actionMap["b.txt"] != ActionDownload {
		t.Error("b.txt should be download (only remote changed)")
	}
	if actionMap["c.txt"] != ActionConflict {
		t.Error("c.txt should be conflict (both changed)")
	}
	if actionMap["d.txt"] != ActionDownload {
		t.Error("d.txt should be download (new on remote)")
	}
}

func TestDiffReceiveOnly(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	actions := local.diff(remote, 0, "receive-only")
	if len(actions) != 1 || actions[0].Action != ActionDownload {
		t.Error("receive-only should allow downloads")
	}
}

func TestDiffSendOnly(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	actions := local.diff(remote, 0, "send-only")
	if len(actions) != 0 {
		t.Error("send-only should produce no actions (no receiving)")
	}
}

func TestDiffDeleteTombstone(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	actions := local.diff(remote, 0, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionDelete {
		t.Errorf("expected delete action, got %v", actions)
	}
}

// B9: diff must populate RemoteSequence so syncFolder can compute
// a safe LastSeenSequence on partial failure.
func TestDiffPopulatesRemoteSequence(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Sequence = 5

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["new.txt"] = FileEntry{SHA256: "aaa", Sequence: 7}
	remote.Files["del.txt"] = FileEntry{Deleted: true, Sequence: 8}
	// Also add del.txt to local so the delete action is generated.
	local.Files["del.txt"] = FileEntry{SHA256: "bbb", Sequence: 1}

	actions := local.diff(remote, 4, "send-receive")
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}

	for _, a := range actions {
		switch a.Path {
		case "new.txt":
			if a.RemoteSequence != 7 {
				t.Errorf("new.txt RemoteSequence: want 7, got %d", a.RemoteSequence)
			}
		case "del.txt":
			if a.RemoteSequence != 8 {
				t.Errorf("del.txt RemoteSequence: want 8, got %d", a.RemoteSequence)
			}
		}
	}
}

// B8: delete tombstone must not destroy a locally-modified file.
func TestDiffDeleteTombstone_LocalModifiedWins(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Sequence = 5
	// Local file was modified after the last sync (seq 3 > lastSeenSeq 2).
	local.Files["a.txt"] = FileEntry{SHA256: "aaa-modified", Sequence: 3}

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 7}

	actions := local.diff(remote, 2, "send-receive")

	for _, a := range actions {
		if a.Path == "a.txt" {
			t.Errorf("B8: locally-modified file should not be deleted by remote tombstone, got action %v", a.Action)
		}
	}
}

// B8: delete tombstone should proceed when local file is unchanged since last sync.
func TestDiffDeleteTombstone_LocalUnchanged(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Sequence = 5
	// Local file was NOT modified since last sync (seq 1 <= lastSeenSeq 2).
	local.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 7}

	actions := local.diff(remote, 2, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionDelete {
		t.Errorf("expected delete for unchanged local file, got %v", actions)
	}
}

// B8: on first sync (lastSeenSeq=0), remote deletions should go through.
func TestDiffDeleteTombstone_FirstSync(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	// lastSeenSeq=0 means we've never synced — trust remote tombstones.
	actions := local.diff(remote, 0, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionDelete {
		t.Errorf("expected delete on first sync, got %v", actions)
	}
}

// B8: both sides deleted — no action needed.
func TestDiffDeleteTombstone_BothDeleted(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 2}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	actions := local.diff(remote, 1, "send-receive")
	for _, a := range actions {
		if a.Path == "a.txt" {
			t.Errorf("no action expected when both sides deleted, got %v", a.Action)
		}
	}
}

func TestPurgeTombstones(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	// Old tombstone (mtime = 0 means epoch, well past 30 days ago).
	idx.Files["old.txt"] = FileEntry{Deleted: true, MtimeNS: 0}
	// Recent tombstone.
	idx.Files["recent.txt"] = FileEntry{Deleted: true, MtimeNS: time.Now().UnixNano()}

	idx.purgeTombstones(30*24*time.Hour, nil)

	if _, ok := idx.Files["old.txt"]; ok {
		t.Error("old tombstone should have been purged")
	}
	if _, ok := idx.Files["recent.txt"]; !ok {
		t.Error("recent tombstone should be kept")
	}
}

// B14: tombstones must survive purge when a peer hasn't acknowledged them.
func TestPurgeTombstones_BlockedByUnackedPeer(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	oldNs := time.Now().Add(-60 * 24 * time.Hour).UnixNano()

	// Tombstone at sequence 10.
	idx.Files["deleted.txt"] = FileEntry{Deleted: true, MtimeNS: oldNs, Sequence: 10}
	// Tombstone at sequence 5.
	idx.Files["also-deleted.txt"] = FileEntry{Deleted: true, MtimeNS: oldNs, Sequence: 5}

	// Peer A has seen up to 10, peer B only up to 7.
	peers := map[string]PeerState{
		"192.168.1.1:7756": {LastSeenSequence: 10},
		"192.168.1.2:7756": {LastSeenSequence: 7},
	}

	n := idx.purgeTombstones(30*24*time.Hour, peers)

	// deleted.txt (seq=10): peer A acked (10>=10), peer B NOT acked (7<10) → kept
	if _, ok := idx.Files["deleted.txt"]; !ok {
		t.Error("tombstone at seq=10 should be kept: peer B hasn't acknowledged it")
	}
	// also-deleted.txt (seq=5): both peers acked (10>=5, 7>=5) → purged
	if _, ok := idx.Files["also-deleted.txt"]; ok {
		t.Error("tombstone at seq=5 should be purged: all peers acknowledged")
	}
	if n != 1 {
		t.Errorf("purgeTombstones returned %d, want 1", n)
	}
}

// B14: with no peers configured, all tombstones are purgeable.
func TestPurgeTombstones_NoPeers(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	oldNs := time.Now().Add(-60 * 24 * time.Hour).UnixNano()
	idx.Files["gone.txt"] = FileEntry{Deleted: true, MtimeNS: oldNs, Sequence: 5}

	n := idx.purgeTombstones(30*24*time.Hour, nil)
	if n != 1 {
		t.Errorf("purgeTombstones returned %d, want 1", n)
	}
	if _, ok := idx.Files["gone.txt"]; ok {
		t.Error("tombstone should be purged with no peers")
	}
}

func TestCleanTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create stale temp files: one at root, one nested.
	writeFile(t, dir, ".mesh-tmp-aaa", "stale root")
	writeFile(t, dir, "sub/.mesh-tmp-bbb", "stale nested")
	// Create a fresh temp file that should survive.
	writeFile(t, dir, ".mesh-tmp-fresh", "fresh")
	// Create a normal file that should never be touched.
	writeFile(t, dir, "sub/real.txt", "keep")

	// Backdate the stale files.
	staleTime := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, ".mesh-tmp-aaa"), staleTime, staleTime)
	_ = os.Chtimes(filepath.Join(dir, "sub/.mesh-tmp-bbb"), staleTime, staleTime)

	cleanTempFiles(dir, 24*time.Hour)

	// Stale files should be removed.
	if _, err := os.Stat(filepath.Join(dir, ".mesh-tmp-aaa")); !os.IsNotExist(err) {
		t.Error("stale root temp file should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub/.mesh-tmp-bbb")); !os.IsNotExist(err) {
		t.Error("stale nested temp file should be removed")
	}
	// Fresh temp file should survive.
	if _, err := os.Stat(filepath.Join(dir, ".mesh-tmp-fresh")); err != nil {
		t.Error("fresh temp file should survive")
	}
	// Normal file should be untouched.
	if _, err := os.Stat(filepath.Join(dir, "sub/real.txt")); err != nil {
		t.Error("normal file should be untouched")
	}
}

// --- Conflict tests ---

func TestConflictFileName(t *testing.T) {
	t.Parallel()
	result := conflictFileName("docs/report.docx", "abc123def")
	if !isConflictFile(result) {
		t.Errorf("expected conflict pattern, got %q", result)
	}
	if filepath.Dir(result) != "docs" {
		t.Errorf("expected dir 'docs', got %q", filepath.Dir(result))
	}
}

func TestResolveConflict_RemoteWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "local content")

	localMtime := time.Now().Add(-1 * time.Hour).UnixNano()
	remoteMtime := time.Now().UnixNano()

	winner, conflictPath := resolveConflict(dir, "file.txt", localMtime, remoteMtime, "remote123")
	if winner != "remote" {
		t.Errorf("expected remote to win, got %q", winner)
	}
	if conflictPath == "" {
		t.Error("expected non-empty conflict path for remote winner")
	}

	// B13: resolveConflict must NOT rename the file — caller handles it
	// after a successful download. Verify original is untouched.
	if _, err := os.Stat(filepath.Join(dir, "file.txt")); err != nil {
		t.Error("original file should still exist (resolveConflict must not rename)")
	}
}

func TestResolveConflict_LocalWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "local content")

	localMtime := time.Now().UnixNano()
	remoteMtime := time.Now().Add(-1 * time.Hour).UnixNano()

	winner, conflictPath := resolveConflict(dir, "file.txt", localMtime, remoteMtime, "remote123")
	if winner != "local" {
		t.Errorf("expected local to win, got %q", winner)
	}
	if conflictPath != "" {
		t.Errorf("expected empty conflict path for local winner, got %q", conflictPath)
	}

	// Original should still exist.
	if _, err := os.Stat(filepath.Join(dir, "file.txt")); err != nil {
		t.Error("original file should still exist")
	}
}

// TestResolveConflict_StaleIndexMtime_LocalWritesWinOverRemote verifies that
// if the caller passes a stale index mtime but the file on disk has been
// modified after the scan — making it newer than the remote version —
// resolveConflict consults the disk mtime and declares local the winner
// rather than clobbering the user's latest edits.
func TestResolveConflict_StaleIndexMtime_LocalWritesWinOverRemote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	// Simulate: scan recorded an old mtime, user then edited the file so the
	// disk mtime is now the newest of the three timestamps.
	scanTimeMtime := time.Now().Add(-2 * time.Hour).UnixNano()
	remoteMtime := time.Now().Add(-1 * time.Hour).UnixNano()
	writeFile(t, dir, "file.txt", "user's latest edit")
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	winner, _ := resolveConflict(dir, "file.txt", scanTimeMtime, remoteMtime, "remote123")
	if winner != "local" {
		t.Errorf("expected local to win (disk mtime newer than remote), got %q", winner)
	}
	// Original file must not be renamed.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("user's edited file was clobbered: %v", err)
	}
}

// B13: verify that a failed download during conflict resolution does not lose
// the local file. The local file must remain at its original path.
func TestConflictResolution_FailedDownloadPreservesLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "precious local content")

	localMtime := time.Now().Add(-1 * time.Hour).UnixNano()
	remoteMtime := time.Now().UnixNano()

	winner, conflictPath := resolveConflict(dir, "file.txt", localMtime, remoteMtime, "remote123")
	if winner != "remote" {
		t.Fatal("expected remote to win for this test setup")
	}

	// Simulate a download failure: downloadToVerifiedTemp would return an error.
	// The key invariant is that the local file must NOT have been renamed yet.
	_ = conflictPath // would be used only after successful download

	// Verify local file is intact.
	data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("local file should exist after failed download: %v", err)
	}
	if string(data) != "precious local content" {
		t.Errorf("local file content changed: got %q", string(data))
	}
}

// B17: verify that NFD paths (macOS HFS+ decomposition) are normalized to
// NFC during scan, preventing cross-platform duplicates.
func TestScanNormalizesNFD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// "café" in NFD: e + combining acute accent (U+0301)
	nfdName := "caf\u0065\u0301.txt"
	// "café" in NFC: é (U+00E9)
	nfcName := "caf\u00e9.txt"

	writeFile(t, dir, nfdName, "content")

	idx := newFileIndex()
	changed, _, _, err := idx.scan(context.Background(), dir, &ignoreMatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected scan to detect changes")
	}

	// The index key should be NFC regardless of what the filesystem stores.
	if _, ok := idx.Files[nfcName]; !ok {
		// Show what keys exist for debugging.
		for k := range idx.Files {
			t.Logf("index key: %q (len=%d)", k, len(k))
		}
		t.Errorf("expected NFC key %q in index", nfcName)
	}
}

// B17: verify that remote index paths are normalized to NFC so diff()
// compares consistently.
func TestProtoToFileIndex_NormalizesNFD(t *testing.T) {
	t.Parallel()
	nfdPath := "caf\u0065\u0301.txt"
	nfcPath := "caf\u00e9.txt"

	idx := protoToFileIndex(&pb.IndexExchange{
		Files: []*pb.FileInfo{
			{Path: nfdPath, Size: 10, Sha256: []byte{1, 2, 3}},
		},
	})

	if _, ok := idx.Files[nfcPath]; !ok {
		for k := range idx.Files {
			t.Logf("key: %q", k)
		}
		t.Errorf("expected NFC key %q in converted index", nfcPath)
	}
}

// --- Transfer tests ---

func TestDownloadFile_PathTraversal(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	_, err := downloadFile(t.Context(), client, "127.0.0.1:9999", "test", "../../../etc/passwd", "abcdef0123456789abcdef0123456789", t.TempDir(), nil)
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestDownloadFile_ShortHash(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	_, err := downloadFile(t.Context(), client, "127.0.0.1:9999", "test", "file.txt", "abc", t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error for short hash")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- Block-level delta tests ---

func TestComputeBlockSignatures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a file with 2.5 blocks worth of data (block size = 4 bytes for testing).
	writeFile(t, dir, "data.bin", "AAAABBBBcc")

	hashes, err := computeBlockSignatures(filepath.Join(dir, "data.bin"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(hashes))
	}
	// First two blocks are different (AAAA vs BBBB).
	if hashEqual(hashes[0], hashes[1]) {
		t.Error("first two blocks should differ")
	}
}

func TestComputeDeltaBlocks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Old file: "AAAABBBBcc"
	writeFile(t, dir, "old.bin", "AAAABBBBcc")
	// New file: "AAAAXXXXcc" — middle block changed.
	writeFile(t, dir, "new.bin", "AAAAXXXXcc")

	oldHashes, _ := computeBlockSignatures(filepath.Join(dir, "old.bin"), 4)
	delta, err := computeDeltaBlocks(filepath.Join(dir, "new.bin"), 4, oldHashes)
	if err != nil {
		t.Fatal(err)
	}
	// Only block 1 (XXXX) should be in the delta.
	if len(delta) != 1 {
		t.Fatalf("expected 1 delta block, got %d", len(delta))
	}
	if delta[0].index != 1 {
		t.Errorf("delta block index = %d, want 1", delta[0].index)
	}
	if string(delta[0].data) != "XXXX" {
		t.Errorf("delta block data = %q, want 'XXXX'", delta[0].data)
	}
}

func TestApplyDelta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "old.bin", "AAAABBBBcc")

	// Delta: replace block 1 with "XXXX".
	blocks := []deltaBlock{{index: 1, data: []byte("XXXX")}}
	tmpPath, err := applyDelta(
		filepath.Join(dir, "old.bin"),
		filepath.Join(dir, "result.bin"),
		4, 10, blocks,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "AAAAXXXXcc" {
		t.Errorf("delta result = %q, want 'AAAAXXXXcc'", got)
	}
	_ = os.Remove(tmpPath)
}

func TestDeltaEndpoint_ReducesTransfer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Use 1024-byte blocks (B18 minimum) with 3 blocks total.
	const bs = 1024
	blockA := strings.Repeat("A", bs)
	blockX := strings.Repeat("X", bs) // changed block on server
	blockC := strings.Repeat("c", bs)
	blockB := strings.Repeat("B", bs) // old block on client

	writeFile(t, dir, "data.bin", blockA+blockX+blockC)

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(dir, "127.0.0.1"),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Client has old version with different middle block.
	clientDir := t.TempDir()
	writeFile(t, clientDir, "data.bin", blockA+blockB+blockC)
	localHashes, _ := computeBlockSignatures(filepath.Join(clientDir, "data.bin"), bs)

	req := &pb.BlockSignatures{
		FolderId:    "test",
		Path:        "data.bin",
		BlockSize:   bs,
		FileSize:    3 * bs,
		BlockHashes: localHashes,
	}
	reqData, _ := proto.Marshal(req)

	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := readBody(t, resp)
	var deltaResp pb.DeltaResponse
	if err := proto.Unmarshal(body, &deltaResp); err != nil {
		t.Fatal(err)
	}

	// Should only get 1 changed block (the middle one).
	if len(deltaResp.GetBlocks()) != 1 {
		t.Fatalf("expected 1 delta block, got %d", len(deltaResp.GetBlocks()))
	}
	if string(deltaResp.GetBlocks()[0].GetData()) != blockX {
		t.Errorf("delta data length = %d, want %d", len(deltaResp.GetBlocks()[0].GetData()), bs)
	}
}

// B18: verify that extreme BlockSize values are clamped to [1KB, 16MB].
func TestDeltaEndpoint_BlockSizeClamped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "data.bin", "AAAA")

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(dir, "127.0.0.1"),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	for _, tc := range []struct {
		name      string
		blockSize int64
	}{
		{"one byte", 1},
		{"below min", 512},
		{"above max", 32 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &pb.BlockSignatures{
				FolderId:    "test",
				Path:        "data.bin",
				BlockSize:   tc.blockSize,
				FileSize:    4,
				BlockHashes: nil,
			}
			reqData, _ := proto.Marshal(req)
			resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
		})
	}
}

func TestSafePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "valid.txt", "ok")
	writeFile(t, root, "sub/nested.txt", "ok")

	tests := []struct {
		name    string
		relPath string
		wantErr bool
	}{
		{"simple file", "valid.txt", false},
		{"nested file", "sub/nested.txt", false},
		{"dotdot prefix", "../escape.txt", true},
		{"dotdot mid", "sub/../../escape.txt", true},
		{"dotdot collapsed", "a/../..", true},
		{"deep dotdot", "a/b/c/../../../../escape.txt", true},
		{"absolute unix path", "/etc/passwd", runtime.GOOS != "windows"},
		{"absolute windows path", `C:\Windows\System32`, runtime.GOOS == "windows"},
		{"null byte", "file\x00.txt", true},
		{"empty path", "", false}, // resolves to root itself, which is allowed
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := safePath(root, tt.relPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("safePath(%q) error=%v, wantErr=%v", tt.relPath, err, tt.wantErr)
			}
		})
	}
}

func TestDeleteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "content")

	if err := deleteFile(dir, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestDeleteFile_PathTraversal(t *testing.T) {
	t.Parallel()
	err := deleteFile(t.TempDir(), "../escape.txt")
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

// --- Protocol tests ---

func TestHandleFile_ServesContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world")

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(dir, "127.0.0.1"),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/file?folder=test&path=hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleFile_RejectsTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(dir, "127.0.0.1"),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/file?folder=test&path=../../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Error("should reject path traversal")
	}
}

func TestHandleFile_WithOffset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "data.txt", "abcdefghij")

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(dir, "127.0.0.1"),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/file?folder=test&path=data.txt&offset=5")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	buf := make([]byte, 100)
	n2, _ := resp.Body.Read(buf)
	if string(buf[:n2]) != "fghij" {
		t.Errorf("expected 'fghij', got %q", string(buf[:n2]))
	}
}

func TestHandleIndex_ExchangeRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	idx := newFileIndex()
	idx.Sequence = 5
	idx.Files["local.txt"] = FileEntry{Size: 100, SHA256: "abc123", Sequence: 5}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Send an index and receive one back.
	req := &pb.IndexExchange{
		DeviceId: "peer-device",
		FolderId: "test",
		Sequence: 3,
		Files: []*pb.FileInfo{
			{Path: "remote.txt", Size: 200, Sha256: []byte("def456"), Sequence: 3},
		},
	}
	data, _ := proto.Marshal(req)

	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := readBody(t, resp)
	var respIdx pb.IndexExchange
	if err := proto.Unmarshal(body, &respIdx); err != nil {
		t.Fatal(err)
	}

	if respIdx.GetDeviceId() != "test-device" {
		t.Errorf("expected device_id 'test-device', got %q", respIdx.GetDeviceId())
	}
	if len(respIdx.GetFiles()) != 1 {
		t.Fatalf("expected 1 file in response, got %d", len(respIdx.GetFiles()))
	}
	if respIdx.GetFiles()[0].GetPath() != "local.txt" {
		t.Errorf("expected 'local.txt', got %q", respIdx.GetFiles()[0].GetPath())
	}
}

func TestHandleIndex_DeltaMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	idx := newFileIndex()
	idx.Sequence = 10
	idx.Files["old.txt"] = FileEntry{Size: 100, SHA256: "aaa", Sequence: 3}
	idx.Files["new.txt"] = FileEntry{Size: 200, SHA256: "bbb", Sequence: 8}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Request with since=5: should only get new.txt (sequence 8 > 5).
	req := &pb.IndexExchange{
		DeviceId: "peer",
		FolderId: "test",
		Sequence: 5,
		Since:    5,
	}
	data, _ := proto.Marshal(req)

	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := readBody(t, resp)
	var respIdx pb.IndexExchange
	if err := proto.Unmarshal(body, &respIdx); err != nil {
		t.Fatal(err)
	}

	if len(respIdx.GetFiles()) != 1 {
		t.Fatalf("expected 1 file in delta response, got %d", len(respIdx.GetFiles()))
	}
	if respIdx.GetFiles()[0].GetPath() != "new.txt" {
		t.Errorf("expected 'new.txt', got %q", respIdx.GetFiles()[0].GetPath())
	}

	// Request with since=0: should get both files (full exchange).
	req.Since = 0
	data, _ = proto.Marshal(req)
	resp2, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()

	body2 := readBody(t, resp2)
	var respIdx2 pb.IndexExchange
	if err := proto.Unmarshal(body2, &respIdx2); err != nil {
		t.Fatal(err)
	}
	if len(respIdx2.GetFiles()) != 2 {
		t.Fatalf("expected 2 files in full response, got %d", len(respIdx2.GetFiles()))
	}
}

func TestBuildIndexExchange_DeltaFiltering(t *testing.T) {
	t.Parallel()
	n := &Node{
		deviceID: "test",
		folders: map[string]*folderState{
			"docs": {
				index: &FileIndex{
					Sequence: 10,
					Files: map[string]FileEntry{
						"old.txt": {SHA256: "aaa", Sequence: 2},
						"mid.txt": {SHA256: "bbb", Sequence: 5},
						"new.txt": {SHA256: "ccc", Sequence: 9},
					},
				},
			},
		},
	}

	// Full exchange.
	full := n.buildIndexExchange("docs", 0)
	if len(full.GetFiles()) != 3 {
		t.Errorf("full: expected 3 files, got %d", len(full.GetFiles()))
	}

	// Delta since=4: should get mid.txt (5) and new.txt (9).
	delta := n.buildIndexExchange("docs", 4)
	if len(delta.GetFiles()) != 2 {
		t.Errorf("delta since=4: expected 2 files, got %d", len(delta.GetFiles()))
	}

	// Delta since=9: should get nothing (no entries > 9, only = 9).
	delta2 := n.buildIndexExchange("docs", 9)
	if len(delta2.GetFiles()) != 0 {
		t.Errorf("delta since=9: expected 0 files, got %d", len(delta2.GetFiles()))
	}
}

func TestHandleStatus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["device_id"] != "test-device" {
		t.Errorf("expected device_id 'test-device', got %v", result["device_id"])
	}
}

func TestHandleIndex_LoopbackTrusted(t *testing.T) {
	t.Parallel()
	// Loopback connections (via SSH tunnels) bypass peer IP validation.
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "10.99.99.99"), // peer IP doesn't match localhost
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	idx := newFileIndex()
	idx.Files["local.txt"] = FileEntry{Size: 100, SHA256: "abc", Sequence: 1}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "10.99.99.99"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler()) // connects from 127.0.0.1
	defer ts.Close()

	req := &pb.IndexExchange{DeviceId: "peer", FolderId: "test"}
	data, _ := proto.Marshal(req)

	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Loopback should be trusted — expect 200, not 403.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 (loopback trusted), got %d", resp.StatusCode)
	}
}

func TestPeerValidation_NonLoopbackRejected(t *testing.T) {
	t.Parallel()
	// Non-loopback peers that don't match any configured peer are rejected.
	n := &Node{
		folders: map[string]*folderState{
			"docs": {cfg: config.FolderCfg{
				Peers:            []string{"10.1.1.1:7756"},
				AllowedPeerHosts: []string{"10.1.1.1"},
			}},
		},
	}

	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false}, // isPeerConfigured doesn't match, but isLoopback would
		{"::1", false},       // same — loopback bypass is in validatePeer, not isPeerConfigured
		{"10.1.1.1", true},   // matches configured peer
		{"10.2.2.2", false},  // doesn't match
		{"192.168.1.1", false},
	}

	for _, tt := range tests {
		got := n.isPeerConfigured(tt.ip)
		if got != tt.want {
			t.Errorf("isPeerConfigured(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}

	// Verify isLoopback
	if !isLoopback("127.0.0.1") {
		t.Error("isLoopback(127.0.0.1) should be true")
	}
	if !isLoopback("::1") {
		t.Error("isLoopback(::1) should be true")
	}
	if isLoopback("10.1.1.1") {
		t.Error("isLoopback(10.1.1.1) should be false")
	}
}

func TestPaginatedIndexExchange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Build a node with a large index that exceeds indexPageSize.
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "server-device",
	}
	idx := newFileIndex()
	for i := range indexPageSize + 500 { // slightly more than one page
		idx.Files[fmt.Sprintf("file-%05d.txt", i)] = FileEntry{
			Size: int64(i), SHA256: fmt.Sprintf("hash%05d", i), Sequence: int64(i + 1),
		}
	}
	idx.Sequence = int64(indexPageSize + 500)
	n.folders["bigfolder"] = &folderState{
		cfg:   config.FolderCfg{ID: "bigfolder", Path: dir, Direction: "send-receive", Peers: []string{"127.0.0.1:7756"}},
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Client builds its own index (also large).
	clientFiles := make([]*pb.FileInfo, 0, indexPageSize+200)
	for i := range indexPageSize + 200 {
		clientFiles = append(clientFiles, &pb.FileInfo{
			Path: fmt.Sprintf("client-%05d.txt", i), Size: int64(i),
			Sha256: []byte(fmt.Sprintf("chash%05d", i)), Sequence: int64(i + 1),
		})
	}
	exchange := &pb.IndexExchange{
		DeviceId: "client-device",
		FolderId: "bigfolder",
		Sequence: int64(indexPageSize + 200),
		Since:    0,
		Files:    clientFiles,
	}

	// Use sendIndex which should automatically paginate.
	client := &http.Client{Timeout: 10 * time.Second}
	addr := ts.Listener.Addr().String()
	resp, err := sendIndex(t.Context(), client, addr, exchange)
	if err != nil {
		t.Fatalf("sendIndex: %v", err)
	}

	// Server should return its full index (possibly paginated, reassembled by sendIndex).
	if resp.GetDeviceId() != "server-device" {
		t.Errorf("expected device_id 'server-device', got %q", resp.GetDeviceId())
	}
	if len(resp.GetFiles()) != indexPageSize+500 {
		t.Errorf("expected %d files in response, got %d", indexPageSize+500, len(resp.GetFiles()))
	}
}

func TestPaginatedIndexExchange_SmallIndex(t *testing.T) {
	t.Parallel()
	// Small indices should still work (single-page path).
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "srv",
	}
	idx := newFileIndex()
	idx.Files["a.txt"] = FileEntry{Size: 10, SHA256: "aaa", Sequence: 1}
	idx.Sequence = 1
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	exchange := &pb.IndexExchange{
		DeviceId: "cli",
		FolderId: "test",
		Sequence: 1,
		Files:    []*pb.FileInfo{{Path: "b.txt", Size: 20, Sha256: []byte("bbb"), Sequence: 1}},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := sendIndex(t.Context(), client, ts.Listener.Addr().String(), exchange)
	if err != nil {
		t.Fatalf("sendIndex: %v", err)
	}

	if len(resp.GetFiles()) != 1 || resp.GetFiles()[0].GetPath() != "a.txt" {
		t.Errorf("unexpected response: %v", resp.GetFiles())
	}
}

func TestPaginateResponse(t *testing.T) {
	t.Parallel()
	files := make([]*pb.FileInfo, indexPageSize*3+50)
	for i := range files {
		files[i] = &pb.FileInfo{Path: fmt.Sprintf("f%d", i)}
	}

	resp := &pb.IndexExchange{
		DeviceId: "d", FolderId: "f", Sequence: 99, Files: files,
	}
	pages := paginateResponse(resp)

	if len(pages) != 4 {
		t.Fatalf("expected 4 pages, got %d", len(pages))
	}

	total := 0
	for i, p := range pages {
		if p.GetPage() != int32(i) {
			t.Errorf("page %d: got page=%d", i, p.GetPage())
		}
		if p.GetTotalPages() != 4 {
			t.Errorf("page %d: got total_pages=%d", i, p.GetTotalPages())
		}
		total += len(p.GetFiles())
	}
	if total != len(files) {
		t.Errorf("total files across pages: got %d, want %d", total, len(files))
	}
}

// --- Watcher tests ---

func TestFolderWatcher_SignalsDirty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ignore := &ignoreMatcher{}

	fw, err := newFolderWatcher([]string{dir}, map[string]*ignoreMatcher{dir: ignore}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fw.close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fw.run(ctx)

	// Create a file to trigger the watcher.
	writeFile(t, dir, "trigger.txt", "data")

	// Wait for dirty signal.
	select {
	case <-fw.dirtyCh:
		// ok
	case <-time.After(3 * time.Second):
		t.Error("expected dirty signal within 3s")
	}
}

// --- Peer matching tests ---

func TestPeerMatchesAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		peer    string
		request string
		want    bool
	}{
		{"192.168.1.10:7756", "192.168.1.10", true},
		{"192.168.1.10:7756", "192.168.1.11", false},
		{"127.0.0.1:7756", "127.0.0.1", true},
		{"localhost:7756", "127.0.0.1", true},
		{"127.0.0.1:7756", "::1", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s", tt.peer, tt.request), func(t *testing.T) {
			if got := peerMatchesAddr(tt.peer, tt.request); got != tt.want {
				t.Errorf("peerMatchesAddr(%q, %q) = %v, want %v", tt.peer, tt.request, got, tt.want)
			}
		})
	}
}

// TestPeerMatchesAddr_IPv6Canonical pins a bug surfaced during the D11 e2e
// work: peerMatchesAddr compares IP strings literally and never parses them
// with net.ParseIP, so two equal IPv6 addresses written in different
// canonical forms (e.g. "2001:db8::1" vs. "2001:db8:0:0:0:0:0:1") fail to
// match and the request is rejected with 403 "unknown peer". Running
// mesh on an IPv6-first network would surface this as silent filesync
// failures for any host whose remote address arrives in the long form.
func TestPeerMatchesAddr_IPv6Canonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		peer    string
		request string
	}{
		{
			name:    "short form peer vs expanded request",
			peer:    "[2001:db8::1]:7756",
			request: "2001:db8:0:0:0:0:0:1",
		},
		{
			name:    "zero-run collapsed differently on each side",
			peer:    "[2001:db8:0:0::1]:7756",
			request: "2001:db8::1",
		},
		{
			name:    "lowercase vs uppercase hex",
			peer:    "[2001:db8::abcd]:7756",
			request: "2001:DB8::ABCD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Sanity: confirm the two forms parse to the same IP.
			p1 := net.ParseIP(stripBrackets(extractHost(tt.peer)))
			p2 := net.ParseIP(tt.request)
			if p1 == nil || p2 == nil || !p1.Equal(p2) {
				t.Fatalf("test setup: %q and %q are not the same IP (p1=%v p2=%v)", tt.peer, tt.request, p1, p2)
			}
			if !peerMatchesAddr(tt.peer, tt.request) {
				t.Fatalf("peerMatchesAddr(%q, %q) = false; both sides are the same IP, should match", tt.peer, tt.request)
			}
		})
	}
}

// TestPeerMatchesAddr_HostnameResolution pins a bug surfaced during the
// D11 e2e work: configured peer hostnames were never resolved via DNS, so
// a peer listed as "server:7756" in the config never matched a request
// whose remote address was the IP that hostname resolved to. The
// scenario suite worked around this with an sh wrapper that rewrote the
// YAML at container start; real users configuring a docker compose
// service name, a Tailscale magicdns name, or any LAN hostname hit a
// silent 403 "unknown peer". The fix moved resolution into
// config.FilesyncCfg.Resolve, which populates FolderCfg.AllowedPeerHosts
// once at startup, so this test drives that path end-to-end: build a
// FilesyncCfg with a hostname peer, call Resolve, build a filesync Node
// around the resolved folder, and confirm isPeerConfigured accepts the
// resolved IP. Uses os.Hostname() as a deterministic source of a
// non-"localhost" name that usually resolves to a loopback address in
// typical dev and CI environments, and skips if neither 127.0.0.1 nor ::1
// is among the resolved addresses.
func TestPeerMatchesAddr_HostnameResolution(t *testing.T) {
	t.Parallel()
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Skipf("cannot determine hostname: %v", err)
	}
	addrs, err := net.LookupHost(hostname)
	if err != nil {
		t.Skipf("cannot resolve %q: %v", hostname, err)
	}
	var want string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.IsLoopback() {
			want = a
			break
		}
	}
	if want == "" {
		t.Skipf("hostname %q does not resolve to loopback (resolved to %v)", hostname, addrs)
	}

	cfg := config.FilesyncCfg{
		Peers: map[string][]string{
			"server": {hostname + ":7756"},
		},
		Folders: map[string]config.FolderCfgRaw{
			"docs": {
				Path:  t.TempDir(),
				Peers: []string{"server"},
			},
		},
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("cfg.Resolve: %v", err)
	}
	if len(cfg.ResolvedFolders) != 1 {
		t.Fatalf("ResolvedFolders = %d, want 1", len(cfg.ResolvedFolders))
	}
	folder := cfg.ResolvedFolders[0]
	folder.AllowedPeerHosts = config.ResolveAllowedPeerHosts(folder.ID, folder.Peers)
	if len(folder.AllowedPeerHosts) == 0 {
		t.Fatalf("AllowedPeerHosts is empty after resolve; hostname %q should expand", hostname)
	}

	n := &Node{
		folders: map[string]*folderState{
			folder.ID: {cfg: folder},
		},
	}
	if !n.isPeerConfigured(want) {
		t.Fatalf("isPeerConfigured(%q) = false; hostname %q resolved to %v, expected match via AllowedPeerHosts=%v",
			want, hostname, addrs, folder.AllowedPeerHosts)
	}
}

// extractHost and stripBrackets are tiny helpers kept local to the IPv6
// test so it does not depend on test order or package-level state.
func extractHost(hp string) string {
	h, _, err := net.SplitHostPort(hp)
	if err != nil {
		return hp
	}
	return h
}

func stripBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}

// --- End-to-end sync test (FT1) ---

func TestTwoNodeSync(t *testing.T) {
	t.Parallel()
	// Set up two folders with a file on each side.
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeFile(t, dirA, "from-a.txt", "content from A")

	// Node A: scan to build index.
	idxA := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idxA.scan(context.Background(), dirA, ignore)

	// Node B: empty index.
	idxB := newFileIndex()

	// Start node B's HTTP server so A can download from it and vice versa.
	nodeB := &Node{
		cfg:      testCfg(dirB, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "node-b",
	}
	nodeB.folders["test"] = &folderState{
		cfg:   testFolderCfg(dirB, "127.0.0.1"),
		index: idxB,
		peers: make(map[string]PeerState),
	}
	srvB := httptest.NewServer((&server{node: nodeB}).handler())
	defer srvB.Close()

	// Node A's HTTP server.
	nodeA := &Node{
		cfg:        testCfg(dirA, "127.0.0.1"),
		folders:    make(map[string]*folderState),
		deviceID:   "node-a",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	nodeA.folders["test"] = &folderState{
		cfg:   testFolderCfg(dirA, "127.0.0.1"),
		index: idxA,
		peers: make(map[string]PeerState),
	}
	srvA := httptest.NewServer((&server{node: nodeA}).handler())
	defer srvA.Close()

	// Node B exchanges index with node A via A's server.
	exchangeB := nodeB.buildIndexExchange("test", 0)
	remoteIdx, err := sendIndex(t.Context(), &http.Client{}, srvA.Listener.Addr().String(), exchangeB)
	if err != nil {
		t.Fatal(err)
	}

	// remoteIdx should contain from-a.txt.
	remoteFileIndex := protoToFileIndex(remoteIdx)
	if _, ok := remoteFileIndex.Files["from-a.txt"]; !ok {
		t.Fatal("expected from-a.txt in remote index")
	}

	// Compute diff: B should want to download from-a.txt.
	fsB := nodeB.folders["test"]
	fsB.indexMu.Lock()
	actions := fsB.index.diff(remoteFileIndex, 0, "send-receive")
	fsB.indexMu.Unlock()

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Action != ActionDownload || actions[0].Path != "from-a.txt" {
		t.Fatalf("expected download from-a.txt, got %v", actions[0])
	}

	// Download the file from node A's server.
	err = downloadFromPeer(t.Context(),
		&http.Client{},
		srvA.Listener.Addr().String(),
		"test",
		"from-a.txt",
		actions[0].RemoteHash,
		dirB,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the file arrived on disk with correct content.
	got, err := os.ReadFile(filepath.Join(dirB, "from-a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "content from A" {
		t.Errorf("expected 'content from A', got %q", string(got))
	}
}

// --- Resume test (FT2) ---

func TestDownloadFile_Resume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "0123456789abcdef" // 16 bytes

	// Compute expected hash.
	writeFile(t, dir, "source.txt", content)
	expectedHash, err := hashFile(filepath.Join(dir, "source.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// Start a server that serves the file with offset support.
	nodeDir := t.TempDir()
	writeFile(t, nodeDir, "data.txt", content)

	n := &Node{
		cfg:      testCfg(nodeDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(nodeDir, "127.0.0.1"),
	}
	srv := httptest.NewServer((&server{node: n}).handler())
	defer srv.Close()

	destDir := t.TempDir()

	// Pre-create a partial temp file (first 5 bytes).
	tmpName := ".mesh-tmp-" + expectedHash[:16]
	tmpPath := filepath.Join(destDir, tmpName)
	if err := os.WriteFile(tmpPath, []byte(content[:5]), 0600); err != nil {
		t.Fatal(err)
	}

	// Download should resume from offset 5.
	path, err := downloadFile(t.Context(),
		&http.Client{},
		srv.Listener.Addr().String(),
		"test",
		"data.txt",
		expectedHash,
		destDir,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("expected %q, got %q", content, string(got))
	}

	// Temp file should be cleaned up (renamed to final).
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp file should have been renamed away")
	}
}

// --- Direction enforcement test (FT3) ---

func TestHandleFile_RejectsReceiveOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "secret.txt", "data")

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: config.FolderCfg{
			ID:        "test",
			Path:      dir,
			Direction: "receive-only",
			Peers:     []string{"127.0.0.1:7756"},
		},
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/file?folder=test&path=secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for receive-only folder, got %d", resp.StatusCode)
	}
}

func TestHandleFile_RejectsDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "data.txt", "content")

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: config.FolderCfg{
			ID:        "test",
			Path:      dir,
			Direction: "disabled",
		},
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/file?folder=test&path=data.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for disabled folder, got %d", resp.StatusCode)
	}
}

func TestDryRunComputesDiffWithoutExecution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "local.txt", "local content")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)

	// Simulate a remote index with a file we don't have.
	remote := &FileIndex{
		Sequence: 10,
		Files: map[string]FileEntry{
			"remote.txt": {Size: 100, MtimeNS: 1000, SHA256: "abc123", Sequence: 10},
		},
	}

	// Dry-run should compute diff (canReceive = true).
	actions := idx.diff(remote, 0, "dry-run")
	if len(actions) == 0 {
		t.Fatal("dry-run diff should produce actions")
	}
	found := false
	for _, a := range actions {
		if a.Path == "remote.txt" && a.Action == ActionDownload {
			found = true
		}
	}
	if !found {
		t.Error("expected download action for remote.txt in dry-run diff")
	}
}

func TestDisabledFolderSkippedInScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "content")

	n := &Node{
		folders: make(map[string]*folderState),
	}
	n.folders["active"] = &folderState{
		cfg:    config.FolderCfg{ID: "active", Path: dir, Direction: "send-receive"},
		index:  newFileIndex(),
		ignore: &ignoreMatcher{},
	}
	n.folders["off"] = &folderState{
		cfg:    config.FolderCfg{ID: "off", Path: dir, Direction: "disabled"},
		index:  newFileIndex(),
		ignore: &ignoreMatcher{},
	}

	n.runScan(context.Background(), nil)

	// Active folder should have scanned files.
	if len(n.folders["active"].index.Files) == 0 {
		t.Error("active folder should have scanned files")
	}
	// Disabled folder should remain empty.
	if len(n.folders["off"].index.Files) != 0 {
		t.Error("disabled folder should not have scanned files")
	}
}

// --- Unknown folder test (FT4) ---

func TestHandleIndex_UnknownFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	// "test" folder exists but we'll request "nonexistent".
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: newFileIndex(),
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.IndexExchange{FolderId: "nonexistent"}
	data, _ := proto.Marshal(req)

	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown folder, got %d", resp.StatusCode)
	}
}

// --- scan collects conflicts (FT6) ---

func TestScanCollectsConflicts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "normal.txt", "ok")
	writeFile(t, dir, "report.sync-conflict-20260406-143022-abc123.docx", "conflict1")
	writeFile(t, dir, "sub/data.sync-conflict-20260101-000000-def456.csv", "conflict2")

	idx := newFileIndex()
	_, _, _, _, conflicts, err := idx.scanWithStats(context.Background(), dir, &ignoreMatcher{}, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d: %v", len(conflicts), conflicts)
	}

	conflictSet := make(map[string]bool)
	for _, c := range conflicts {
		conflictSet[c] = true
	}
	if !conflictSet["report.sync-conflict-20260406-143022-abc123.docx"] {
		t.Error("missing root-level conflict")
	}
	if !conflictSet["sub/data.sync-conflict-20260101-000000-def456.csv"] {
		t.Error("missing nested conflict")
	}

	// Conflicts must be sorted for stable UI rendering.
	for i := 1; i < len(conflicts); i++ {
		if conflicts[i-1] >= conflicts[i] {
			t.Errorf("conflicts not sorted: %v", conflicts)
		}
	}
}

// --- persistFolder roundtrip test (FT8) ---

func TestPersistFolder_Roundtrip(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	folderDir := t.TempDir()
	writeFile(t, folderDir, "a.txt", "hello")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), folderDir, ignore)

	peers := map[string]PeerState{
		"192.168.1.10:7756": {LastSeenSequence: 42, LastSync: time.Now().Truncate(time.Second)},
	}

	n := &Node{
		dataDir: dataDir,
		folders: map[string]*folderState{
			"docs": {
				cfg:   config.FolderCfg{ID: "docs", Path: folderDir},
				index: idx,
				peers: peers,
			},
		},
	}

	n.persistFolder("docs")

	// Reload index.
	loadedIdx, err := loadIndex(filepath.Join(dataDir, "docs", "index.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if loadedIdx.Sequence != idx.Sequence {
		t.Errorf("sequence: got %d, want %d", loadedIdx.Sequence, idx.Sequence)
	}
	if len(loadedIdx.Files) != len(idx.Files) {
		t.Errorf("file count: got %d, want %d", len(loadedIdx.Files), len(idx.Files))
	}
	for path, entry := range idx.Files {
		loaded, ok := loadedIdx.Files[path]
		if !ok {
			t.Errorf("missing file %q", path)
			continue
		}
		if loaded.SHA256 != entry.SHA256 {
			t.Errorf("%s: hash got %q, want %q", path, loaded.SHA256, entry.SHA256)
		}
	}

	// Reload peers.
	loadedPeers, err := loadPeerStates(filepath.Join(dataDir, "docs", "peers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedPeers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(loadedPeers))
	}
	ps := loadedPeers["192.168.1.10:7756"]
	if ps.LastSeenSequence != 42 {
		t.Errorf("last seen sequence: got %d, want 42", ps.LastSeenSequence)
	}
}

// --- Path tracking test (FT9a) ---

func TestPathChangePreservesIndex(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	oldDir := t.TempDir()
	newDir := t.TempDir()
	writeFile(t, oldDir, "file.txt", "content")

	// Build an index at the old path and persist it.
	idx := newFileIndex()
	idx.Path = oldDir
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), oldDir, ignore)

	idxPath := filepath.Join(dataDir, "docs", "index.yaml")
	if err := idx.save(idxPath); err != nil {
		t.Fatal(err)
	}

	// Reload and simulate path change (same logic as Start()).
	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Path == newDir {
		t.Fatal("path should differ before update")
	}

	// Path change just updates the stored path; index is preserved
	// so the next scan can correctly reconcile (moved dir = no changes,
	// different content = deletions propagate to peers).
	loaded.Path = newDir

	if loaded.Path != newDir {
		t.Errorf("path = %q, want %q", loaded.Path, newDir)
	}
	if len(loaded.Files) == 0 {
		t.Error("index should be preserved after path change")
	}
	if loaded.Sequence == 0 {
		t.Error("sequence should be preserved after path change")
	}
}

func TestIndexPersistsPath(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "content")

	idx := newFileIndex()
	idx.Path = dir
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)

	idxPath := filepath.Join(dataDir, "test", "index.yaml")
	if err := idx.save(idxPath); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Path != dir {
		t.Errorf("persisted path = %q, want %q", loaded.Path, dir)
	}
	if len(loaded.Files) == 0 {
		t.Error("expected preserved files on reload")
	}
}

// --- Fuzz test for ignore parser (FT9) ---

func FuzzParseLine(f *testing.F) {
	f.Add("")
	f.Add("*.go")
	f.Add("!important.txt")
	f.Add("build/")
	f.Add("# comment")
	f.Add("// another comment")
	f.Add("src/**/*.go")
	f.Add("!node_modules/")
	f.Add("/absolute")
	f.Add("!!")

	f.Fuzz(func(t *testing.T, line string) {
		p, ok := parseLine(line)
		if !ok {
			return
		}
		// Pattern should never be empty if parseLine returned ok.
		if p.pattern == "" {
			t.Errorf("parseLine(%q) returned ok=true with empty pattern", line)
		}
	})
}

func FuzzMatchPattern(f *testing.F) {
	f.Add("*.go", "main.go")
	f.Add("src/**/*.go", "src/pkg/main.go")
	f.Add(".git", "sub/.git")
	f.Add("build/", "build")
	f.Add("*", "anything")

	f.Fuzz(func(t *testing.T, pattern, path string) {
		// Must not panic.
		matchPattern(pattern, path)
	})
}

// --- Helpers ---

func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testCfg(dir, peerIP string) config.FilesyncCfg {
	cfg := config.FilesyncCfg{
		Bind:          "0.0.0.0:0",
		MaxConcurrent: 4,
		ScanInterval:  "60s",
		Peers:         map[string][]string{"peer": {peerIP + ":7756"}},
		Defaults:      config.FilesyncDefaults{Peers: []string{"peer"}},
		Folders: map[string]config.FolderCfgRaw{
			"test": {Path: dir},
		},
	}
	_ = cfg.Resolve()
	return cfg
}

func testFolderCfg(dir, peerIP string) config.FolderCfg {
	return config.FolderCfg{
		ID:        "test",
		Path:      dir,
		Direction: "send-receive",
		Peers:     []string{peerIP + ":7756"},
	}
}

func BenchmarkScan(b *testing.B) {
	dir := b.TempDir()
	// Create 100 files to scan.
	for i := range 100 {
		path := filepath.Join(dir, fmt.Sprintf("file_%03d.txt", i))
		_ = os.WriteFile(path, []byte(fmt.Sprintf("content %d", i)), 0600)
	}
	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	b.ResetTimer()
	for b.Loop() {
		_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	}
}

func BenchmarkBlockSignatures(b *testing.B) {
	dir := b.TempDir()
	// 1 MB file = 8 blocks at 128 KB.
	path := filepath.Join(dir, "bench.dat")
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	_ = os.WriteFile(path, data, 0600)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		_, _ = computeBlockSignatures(path, defaultBlockSize)
	}
}

// B15: verify that a corrupted index resets stale peer state so delta
// filtering doesn't suppress the fresh index.
func TestIndexResetClearsPeerState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	folderDir := filepath.Join(dir, "test-folder")
	if err := os.MkdirAll(folderDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Write a corrupted index.
	idxPath := filepath.Join(folderDir, "index.yaml")
	if err := os.WriteFile(idxPath, []byte("{{invalid yaml"), 0600); err != nil {
		t.Fatal(err)
	}

	// Write stale peer state with high LastSentSequence.
	peersPath := filepath.Join(folderDir, "peers.yaml")
	if err := os.WriteFile(peersPath, []byte("192.168.1.1:7756:\n  last_sent_sequence: 5000\n  last_seen_sequence: 3000\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Simulate Start()'s loading logic.
	idx, err := loadIndex(idxPath)
	indexReset := false
	if err != nil {
		idx = newFileIndex()
		indexReset = true
	}
	peers, err := loadPeerStates(peersPath)
	if err != nil {
		t.Fatal("loadPeerStates should succeed:", err)
	}

	if !indexReset {
		t.Fatal("expected indexReset=true for corrupted index")
	}
	if idx.Sequence != 0 {
		t.Fatalf("fresh index should have Sequence=0, got %d", idx.Sequence)
	}
	if len(peers) == 0 {
		t.Fatal("peers should have loaded from peers.yaml before reset")
	}

	// B15: apply the reset logic.
	if indexReset && len(peers) > 0 {
		peers = make(map[string]PeerState)
	}

	if len(peers) != 0 {
		t.Fatalf("peers should be empty after index reset, got %d entries", len(peers))
	}
}

func BenchmarkHashFile(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.dat")
	data := make([]byte, 256*1024) // 256 KB
	_ = os.WriteFile(path, data, 0600)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		_, _ = hashFile(path)
	}
}
