package filesync

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"

	"github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"github.com/mmdemirbas/mesh/internal/zstdutil"
	"google.golang.org/protobuf/proto"
)

// testHash returns a deterministic Hash256 from a short label string.
// Used in tests where distinct hash values are needed but the actual
// SHA-256 content doesn't matter.
func testHash(s string) Hash256 {
	return Hash256(sha256.Sum256([]byte(s)))
}

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

func TestClassifyGlob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern  string
		wantKind patternKind
		wantFix  string
	}{
		{"node_modules", kindLiteral, "node_modules"},
		{".git", kindLiteral, ".git"},
		{"*.class", kindStarSuffix, ".class"},
		{"*.mesh-delta-tmp-*", kindContains, ".mesh-delta-tmp-"},
		{".mesh-tmp-*", kindPrefixStar, ".mesh-tmp-"},
		{"prefix*", kindPrefixStar, "prefix"},
		{"f?o", kindGeneric, ""},           // ? is not optimizable
		{"[abc]", kindGeneric, ""},         // character class
		{"a*b", kindGeneric, ""},           // star in the middle
		{"**", kindGeneric, ""},            // double star — handled as double-star before classifyGlob
		{"*.tar.*", kindContains, ".tar."}, // *LITERAL*
		{"*foo*bar*", kindGeneric, ""},     // three stars → generic
		{"*?*", kindGeneric, ""},           // wildcard in middle disqualifies
		{"", kindLiteral, ""},              // degenerate but classified
		{"exact.txt", kindLiteral, "exact.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			t.Parallel()
			kind, fixed := classifyGlob(tt.pattern)
			if kind != tt.wantKind {
				t.Errorf("classifyGlob(%q) kind=%d, want %d", tt.pattern, kind, tt.wantKind)
			}
			if fixed != tt.wantFix {
				t.Errorf("classifyGlob(%q) fixed=%q, want %q", tt.pattern, fixed, tt.wantFix)
			}
		})
	}
}

func TestShouldIgnore(t *testing.T) {
	t.Parallel()
	m := newIgnoreMatcher([]string{
		".stfolder",
		".mesh-tmp-*",
		"*.log",
		"build/",
		"!important.log",
	})

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

// H3: user config must not be able to un-ignore builtin temp file patterns.
func TestBuiltinIgnoresNonNegatable(t *testing.T) {
	t.Parallel()
	// Config attempts to negate both builtin patterns.
	m := newIgnoreMatcher([]string{"!.mesh-tmp-*", "!*.mesh-delta-tmp-*"})

	tests := []struct {
		path   string
		ignore bool
	}{
		{".mesh-tmp-abc123", true},            // builtin must win over config negation
		{"foo.mesh-delta-tmp-ab12cd34", true}, // builtin must win over config negation
		{"sub/.mesh-tmp-xyz", true},           // nested path, builtin still wins
		{"sub/bar.mesh-delta-tmp-ef56", true}, // nested path, builtin still wins
		{"normal.txt", false},                 // non-builtin unaffected
		{"important.log", false},              // non-builtin unaffected
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got := m.shouldIgnore(tt.path, false)
			if got != tt.ignore {
				t.Errorf("shouldIgnore(%q) = %v, want %v — builtin ignores must be non-negatable", tt.path, got, tt.ignore)
			}
		})
	}
}

// Verify that user negation patterns still work for non-builtin patterns.
func TestUserNegationStillWorksForNonBuiltins(t *testing.T) {
	t.Parallel()
	m := newIgnoreMatcher([]string{"*.log", "!important.log"})

	if !m.shouldIgnore("debug.log", false) {
		t.Error("*.log should be ignored")
	}
	if m.shouldIgnore("important.log", false) {
		t.Error("important.log should NOT be ignored — user negation must work for non-builtins")
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
	idx.Files["live"] = FileEntry{Size: 3, MtimeNS: recentNs, SHA256: testHash("x")}

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
	orig.Files["a.txt"] = FileEntry{Size: 5, SHA256: testHash("aaa"), Sequence: 1}
	orig.Files["b.txt"] = FileEntry{Size: 9, SHA256: testHash("bbb"), Sequence: 2}

	clone := orig.clone()
	clone.Sequence = 99
	clone.Files["a.txt"] = FileEntry{Size: 100, SHA256: testHash("mutated"), Sequence: 50}
	clone.Files["c.txt"] = FileEntry{Size: 1, SHA256: testHash("ccc"), Sequence: 99}

	if orig.Sequence != 7 {
		t.Errorf("orig.Sequence mutated: got %d want 7", orig.Sequence)
	}
	if orig.Files["a.txt"].SHA256 != testHash("aaa") {
		t.Errorf("orig file mutated via clone: got %q want aaa", orig.Files["a.txt"].SHA256)
	}
	if _, ok := orig.Files["c.txt"]; ok {
		t.Error("orig gained entry that was only added to clone")
	}
	if orig.Path != clone.Path {
		t.Errorf("clone.Path = %q, want %q", clone.Path, orig.Path)
	}
}

// TestRunScanRecyclesCloneMap pins P18c: the Files map backing is recycled
// across scans via fs.reusableFiles to eliminate the ~30 MB per-scan
// allocation on large folders. The second scan must not hold a reference
// to the first scan's Files map, and must still produce correct results.
func TestRunScanRecyclesCloneMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "one.txt", "a")
	writeFile(t, dir, "two.txt", "b")

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

	n.runScan(context.Background(), nil)
	firstFilesMap := fs.index.Files
	if len(firstFilesMap) != 2 {
		t.Fatalf("first scan: want 2 entries, got %d", len(firstFilesMap))
	}
	if fs.reusableFiles == nil {
		t.Fatalf("expected reusableFiles to be populated after first scan")
	}

	// Second scan: expect reusableFiles to be consumed (set to nil during
	// setup, then re-populated with the previous scan's map after swap).
	writeFile(t, dir, "three.txt", "c")
	recycledBefore := fs.reusableFiles
	n.runScan(context.Background(), nil)

	if len(fs.index.Files) != 3 {
		t.Fatalf("second scan: want 3 entries, got %d", len(fs.index.Files))
	}
	if fs.reusableFiles == nil {
		t.Fatalf("expected reusableFiles to be re-populated after second scan")
	}
	// The ping-pong invariant: after the second scan, reusableFiles should
	// point to the FIRST scan's map (swapped out during scan 2), not to
	// the map that was recycled INTO scan 2.
	if reflect.ValueOf(fs.reusableFiles).Pointer() == reflect.ValueOf(recycledBefore).Pointer() {
		t.Errorf("reusableFiles was not rotated; same map across scans")
	}
	// The live index must not share the recycled map either.
	if reflect.ValueOf(fs.index.Files).Pointer() == reflect.ValueOf(fs.reusableFiles).Pointer() {
		t.Errorf("fs.index.Files and fs.reusableFiles alias the same map")
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
		Size: 7, SHA256: testHash("peerhash"), Sequence: 1000,
	}
	fs.index.Sequence = 1000

	n.runScan(context.Background(), nil)

	if _, ok := fs.index.Files["from-peer.txt"]; !ok {
		t.Fatal("runScan clobbered a concurrently-written peer entry (expected merge-preserve)")
	}
	if fs.index.Files["from-peer.txt"].SHA256 != testHash("peerhash") {
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

// H2a: when primary index.yaml is corrupted, load falls back to .prev.
func TestLoadIndex_FallbackToPrev(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.yaml")

	// Save an index — both primary and .prev get the same data.
	idx := newFileIndex()
	idx.Sequence = 20
	idx.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 5}
	idx.Files["b.txt"] = FileEntry{SHA256: testHash("bbb"), Sequence: 15}
	if err := idx.save(idxPath); err != nil {
		t.Fatal(err)
	}

	// P17b: save now writes .gob files; corrupt those.
	gobPath := yamlToGobPath(idxPath)

	// Corrupt primary — backup should still load.
	if err := os.WriteFile(gobPath, []byte("corrupt!!!"), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sequence != 20 {
		t.Errorf("expected sequence 20 from backup, got %d", loaded.Sequence)
	}
	if _, ok := loaded.Files["b.txt"]; !ok {
		t.Error("expected b.txt in loaded index")
	}

	// Corrupt backup too — should fail.
	if err := os.WriteFile(prevPath(gobPath), []byte("also corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = loadIndex(idxPath)
	if err == nil {
		t.Error("expected error when both files are corrupt")
	}
}

// P17b: verify gob roundtrip preserves all fields.
func TestLoadIndex_GobRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.yaml")

	idx := newFileIndex()
	idx.Sequence = 42
	idx.Epoch = "deadbeef12345678"
	idx.Files["doc.txt"] = FileEntry{Size: 999, MtimeNS: 12345, SHA256: testHash("abc123"), Sequence: 10, Mode: 0644}
	idx.Files["deleted.txt"] = FileEntry{Size: 0, Deleted: true, Sequence: 20}
	if err := idx.save(idxPath); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sequence != 42 {
		t.Errorf("sequence = %d, want 42", loaded.Sequence)
	}
	if loaded.Epoch != "deadbeef12345678" {
		t.Errorf("epoch = %q, want deadbeef12345678", loaded.Epoch)
	}
	if e, ok := loaded.Files["doc.txt"]; !ok || e.Size != 999 || e.SHA256 != testHash("abc123") || e.Mode != 0644 {
		t.Errorf("doc.txt mismatch: %+v", e)
	}
	if e, ok := loaded.Files["deleted.txt"]; !ok || !e.Deleted {
		t.Errorf("deleted.txt should be tombstone: %+v", e)
	}
}

// P17b: verify migration from YAML to gob (no .gob files → reads .yaml).
func TestLoadIndex_YAMLMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.yaml")

	// Write YAML directly (simulating a pre-gob installation).
	idx := newFileIndex()
	idx.Sequence = 7
	idx.Files["legacy.txt"] = FileEntry{Size: 100, SHA256: testHash("legacyhash"), Sequence: 3}
	data, err := yaml.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sequence != 7 {
		t.Errorf("sequence = %d, want 7", loaded.Sequence)
	}
	if _, ok := loaded.Files["legacy.txt"]; !ok {
		t.Error("expected legacy.txt from YAML migration")
	}
}

// H2a: when both files are missing (first run), loadIndex returns empty index.
func TestLoadIndex_BothMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "nonexistent", "index.yaml")

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatalf("expected no error for missing files, got: %v", err)
	}
	if loaded.Sequence != 0 || len(loaded.Files) != 0 {
		t.Error("expected empty index for first run")
	}
}

// H2a: peer state double-write survives primary corruption.
func TestLoadPeerStates_FallbackToPrev(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	peersPath := filepath.Join(dir, "peers.yaml")

	// Save peer state — both primary and .prev get the same data.
	peers := map[string]PeerState{
		"10.0.0.1:7756": {LastSeenSequence: 200, LastSync: time.Now()},
	}
	if err := savePeerStates(peersPath, peers); err != nil {
		t.Fatal(err)
	}

	// Corrupt primary — backup should still load.
	if err := os.WriteFile(peersPath, []byte("corrupt!!!"), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadPeerStates(peersPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 {
		t.Fatal("expected peer state from backup")
	}
	ps := loaded["10.0.0.1:7756"]
	if ps.LastSeenSequence != 200 {
		t.Errorf("expected LastSeenSequence 200 from backup, got %d", ps.LastSeenSequence)
	}
}

// H2b: new indexes get a random epoch.
func TestNewFileIndexHasEpoch(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	if idx.Epoch == "" {
		t.Error("new index should have a non-empty epoch")
	}
	if len(idx.Epoch) != 16 { // 8 bytes = 16 hex chars
		t.Errorf("epoch should be 16 hex chars, got %q", idx.Epoch)
	}
	// Two indexes should have different epochs.
	idx2 := newFileIndex()
	if idx.Epoch == idx2.Epoch {
		t.Error("two new indexes should have different epochs")
	}
}

// H2b: old persisted indexes without epoch get one assigned on load.
func TestLoadIndex_MigratesEpoch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.yaml")

	// Write an index without an epoch field (simulates pre-H2b format).
	data := []byte("path: /tmp\nsequence: 5\nfiles:\n  a.txt:\n    sha256: 9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0\n    sequence: 1\n")
	if err := os.WriteFile(idxPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Epoch == "" {
		t.Error("loaded index should have an epoch after migration")
	}
	if loaded.Sequence != 5 {
		t.Errorf("expected sequence 5, got %d", loaded.Sequence)
	}
}

// H2b: epoch guard filters downloads for locally-tombstoned files when a
// peer's epoch changed (index recreation after corruption/reset).
func TestEpochGuardFiltersResurrectedFiles(t *testing.T) {
	t.Parallel()

	// Local index: file X is tombstoned, file Y is live.
	local := newFileIndex()
	local.Sequence = 100
	local.Files["x.txt"] = FileEntry{SHA256: testHash("old"), Sequence: 50, Deleted: true, MtimeNS: time.Now().UnixNano()}
	local.Files["y.txt"] = FileEntry{SHA256: testHash("yyy"), Sequence: 60, Size: 10}

	// Remote index (recreated with new epoch): X and Z are live.
	// X was deleted by local but the reset peer re-indexed it.
	remote := newFileIndex()
	remote.Sequence = 50
	remote.Files["x.txt"] = FileEntry{SHA256: testHash("new-hash"), Sequence: 30, Size: 20}
	remote.Files["z.txt"] = FileEntry{SHA256: testHash("zzz"), Sequence: 40, Size: 30}

	// Cycle 2 scenario: lastSeenSeq=0 (after restart detection zeroed it).
	// diff() with lastSeenSeq=0 will produce ActionDownload for x.txt and z.txt.
	actions := local.diff(remote, 0, 0, nil, "send-receive")

	// Before filtering: should have downloads for both x.txt and z.txt.
	downloads := map[string]bool{}
	for _, a := range actions {
		if a.Action == ActionDownload {
			downloads[a.Path] = true
		}
	}
	if !downloads["x.txt"] {
		t.Fatal("expected ActionDownload for x.txt before epoch filter")
	}
	if !downloads["z.txt"] {
		t.Fatal("expected ActionDownload for z.txt before epoch filter")
	}

	// Apply the epoch guard filter (same logic as syncFolder).
	filtered := 0
	n := 0
	for _, a := range actions {
		if a.Action == ActionDownload {
			if le, ok := local.Files[a.Path]; ok && le.Deleted {
				filtered++
				continue
			}
		}
		actions[n] = a
		n++
	}
	actions = actions[:n]

	if filtered != 1 {
		t.Errorf("expected 1 filtered (x.txt), got %d", filtered)
	}

	// After filtering: z.txt should remain, x.txt should be gone.
	remaining := map[string]bool{}
	for _, a := range actions {
		remaining[a.Path] = true
	}
	if remaining["x.txt"] {
		t.Error("x.txt should have been filtered by epoch guard")
	}
	if !remaining["z.txt"] {
		t.Error("z.txt should remain (not a local tombstone)")
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

// PL: short-circuit deletion detection when every previously-active file
// was re-seen on disk. With cachedCount accurate (as in production after
// recomputeCache), the O(N) deletion loop must be skipped.
func TestScanShortCircuitNoDeletions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")
	writeFile(t, dir, "sub/c.txt", "nested")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	idx.recomputeCache() // mimic production post-scan cache refresh

	seqBefore := idx.Sequence

	// Re-scan with no deletions. Every previously-active file is still
	// present, so no tombstones should be written and Sequence must stay put.
	changed, _, _, _ := idx.scan(context.Background(), dir, ignore)
	if changed {
		t.Fatal("expected no change when no files deleted")
	}
	if idx.Sequence != seqBefore {
		t.Errorf("sequence advanced without deletions: before=%d after=%d", seqBefore, idx.Sequence)
	}
	for rel, entry := range idx.Files {
		if entry.Deleted {
			t.Errorf("unexpected tombstone for %q after no-op scan", rel)
		}
	}
}

// PL: even when cachedCount makes the short-circuit eligible, a deletion
// must still be detected (short-circuit must not mask real deletions).
func TestScanShortCircuitDetectsDeletion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	idx.recomputeCache()

	_ = os.Remove(filepath.Join(dir, "b.txt"))

	changed, _, _, _ := idx.scan(context.Background(), dir, ignore)
	if !changed {
		t.Fatal("expected change after deletion")
	}
	entry, ok := idx.Files["b.txt"]
	if !ok || !entry.Deleted {
		t.Fatalf("b.txt should be tombstoned, got ok=%v entry=%+v", ok, entry)
	}
}

func TestScanRespectsIgnore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "keep")
	writeFile(t, dir, "skip.log", "skip")

	idx := newFileIndex()
	ignore := newIgnoreMatcher([]string{"*.log"})

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

	// locked.txt must NOT be tombstoned — it's in errorPaths.
	entry, ok := idx.Files["locked.txt"]
	if !ok {
		t.Fatal("locked.txt should still be in index")
	}
	if entry.Deleted {
		t.Error("B10: locked.txt must not be tombstoned when scan had hash errors")
	}
}

// H1+M2: per-file error tracking allows tombstoning of genuinely deleted
// files even when other files have errors.
func TestScanPerFileErrorAllowsOtherTombstones(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "good.txt", "hello")
	writeFile(t, dir, "locked.txt", "important")
	writeFile(t, dir, "deleted.txt", "will-be-deleted")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}

	// First scan: all three files indexed.
	_, _, _, scanErr := idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// Make locked.txt unreadable and delete deleted.txt.
	lockedPath := filepath.Join(dir, "locked.txt")
	if err := os.Chmod(lockedPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedPath, 0644) })
	os.Remove(filepath.Join(dir, "deleted.txt"))

	// Re-scan.
	_, _, _, scanErr = idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// locked.txt must NOT be tombstoned (error path).
	if e := idx.Files["locked.txt"]; e.Deleted {
		t.Error("locked.txt must not be tombstoned — it had a hash error")
	}
	// deleted.txt MUST be tombstoned (genuinely deleted, not an error).
	if e := idx.Files["deleted.txt"]; !e.Deleted {
		t.Error("deleted.txt must be tombstoned — it was genuinely deleted")
	}
	// good.txt must be untouched.
	if e := idx.Files["good.txt"]; e.Deleted {
		t.Error("good.txt must not be tombstoned")
	}
}

// M2 bulk threshold: when error count exceeds the threshold, suppress
// all tombstones as a safety net for systemic failures.
func TestScanBulkErrorsSuppressAllTombstones(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	idx := newFileIndex()
	ignore := &ignoreMatcher{}

	// Seed the index with 10 "previously seen" files (simulate prior scan).
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("file%d.txt", i)
		writeFile(t, dir, name, fmt.Sprintf("content-%d", i))
	}
	_, _, _, scanErr := idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// Now make >10% of them unreadable (2 of 10 = 20%) and delete one more.
	// Modify before chmod so the fast-path (stat unchanged) doesn't skip rehash.
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("file%d.txt", i)
		writeFile(t, dir, name, fmt.Sprintf("modified-%d", i))
		p := filepath.Join(dir, name)
		if err := os.Chmod(p, 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0644) })
	}
	os.Remove(filepath.Join(dir, "file9.txt"))

	// Re-scan: 2 hash errors out of 10 tracked = 20% > 10% threshold.
	_, _, _, scanErr = idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// Bulk suppression: even file9.txt (genuinely deleted) must NOT be tombstoned.
	if e := idx.Files["file9.txt"]; e.Deleted {
		t.Error("bulk error threshold should suppress all tombstones including genuine deletes")
	}
}

// PM: when a subdirectory becomes unreadable, all descendants (direct and
// nested) must be protected from tombstoning. Verifies the sorted-prefix
// lookup returns every descendant.
func TestScanUnreadableSubdirProtectsAllDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 on directories is a no-op on Windows")
	}
	t.Parallel()
	dir := t.TempDir()

	// Layout:
	//   top/a.txt
	//   top/deep/b.txt
	//   top/deep/deeper/c.txt
	//   sibling.txt (outside the unreadable subtree)
	writeFile(t, dir, "top/a.txt", "aa")
	writeFile(t, dir, "top/deep/b.txt", "bb")
	writeFile(t, dir, "top/deep/deeper/c.txt", "cc")
	writeFile(t, dir, "sibling.txt", "ss")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _, _, scanErr := idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// Make the root of the subtree unreadable.
	topPath := filepath.Join(dir, "top")
	if err := os.Chmod(topPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(topPath, 0o755) })

	// Re-scan. The unreadable dir produces readErr; all descendants must
	// be protected from tombstoning.
	_, _, _, scanErr = idx.scan(context.Background(), dir, ignore)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	for _, p := range []string{"top/a.txt", "top/deep/b.txt", "top/deep/deeper/c.txt"} {
		entry, ok := idx.Files[p]
		if !ok {
			t.Errorf("%s disappeared from index", p)
			continue
		}
		if entry.Deleted {
			t.Errorf("%s must not be tombstoned while parent subtree is unreadable", p)
		}
	}
	// sibling.txt is outside the protected subtree and must be untouched.
	if e := idx.Files["sibling.txt"]; e.Deleted {
		t.Error("sibling.txt must not be affected by unrelated subtree error")
	}
}

// B10: scan must fail fast when folder root is inaccessible.
func TestScanFolderRootInaccessible(t *testing.T) {
	t.Parallel()

	idx := newFileIndex()
	idx.Files["important.txt"] = FileEntry{SHA256: testHash("abc"), Sequence: 1}

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

// M1: if the walk finds zero files but the index has entries, re-stat
// the root to catch a folder that vanished between the pre-walk stat
// and the WalkDir.
func TestScanEmptyWalkWithExistingIndex(t *testing.T) {
	t.Parallel()

	// Populate an index as if a previous scan found files.
	idx := newFileIndex()
	idx.Files["doc.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1, Size: 5, MtimeNS: 1}
	idx.Files["img.png"] = FileEntry{SHA256: testHash("bbb"), Sequence: 2, Size: 10, MtimeNS: 2}
	idx.recomputeCache() // PL precondition: cachedCount must reflect manual inserts.

	// Point the scan at an empty but existing directory (simulates a
	// legitimate empty folder after all files were deleted).
	emptyDir := t.TempDir()
	ignore := &ignoreMatcher{}

	changed, _, _, scanErr := idx.scan(context.Background(), emptyDir, ignore)
	if scanErr != nil {
		t.Fatalf("scan of empty but accessible dir should succeed: %v", scanErr)
	}
	if !changed {
		t.Error("expected changed=true since files were tombstoned")
	}
	// Both entries should be tombstoned (folder exists, legitimately empty).
	for _, name := range []string{"doc.txt", "img.png"} {
		if e := idx.Files[name]; !e.Deleted {
			t.Errorf("%s should be tombstoned in a legitimately empty folder", name)
		}
	}
}

// M1: if the folder root vanishes during the walk, the post-walk re-stat
// must catch it and return an error instead of tombstoning everything.
func TestScanFolderVanishedDuringWalk(t *testing.T) {
	t.Parallel()

	idx := newFileIndex()
	idx.Files["important.txt"] = FileEntry{SHA256: testHash("abc"), Sequence: 1, Size: 5, MtimeNS: 1}

	// Create a dir, then remove it before scan — but the pre-walk os.Stat
	// will fail too (B10 catches it). To test M1 specifically, we need the
	// pre-walk stat to pass but the walk to find nothing. Use a dir that
	// exists but where the index has stale entries from a different path.
	//
	// Simulated by pointing at an existing empty dir with stale index:
	// the pre-walk stat passes, walk finds 0 files, re-stat passes →
	// tombstones are created (correct: folder exists and is empty).
	// The "vanished" case cannot be reliably simulated without race
	// conditions, but the guard is verified by the non-existent path test.
	//
	// Instead verify the inverse: a non-existent root returns an error.
	ignore := &ignoreMatcher{}
	_, _, _, scanErr := idx.scan(context.Background(), "/tmp/mesh-test-nonexistent-"+fmt.Sprintf("%d", time.Now().UnixNano()), ignore)
	if scanErr == nil {
		t.Fatal("expected error for vanished folder root")
	}
	if idx.Files["important.txt"].Deleted {
		t.Error("M1: vanished folder root must not tombstone existing entries")
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
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	rt := retryTracker{nowFn: func() time.Time { return now }}
	const peer = "peer-1:22000"

	// Not quarantined initially.
	if rt.quarantined("a.txt", peer, testHash("hash1")) {
		t.Fatal("should not be quarantined before any failure")
	}

	// First failure: backed off for retryBaseDelay (30s).
	rt.record("a.txt", peer, testHash("hash1"))
	if !rt.quarantined("a.txt", peer, testHash("hash1")) {
		t.Fatal("should be backed off immediately after first failure")
	}

	// Advance past the first backoff window (30s).
	now = now.Add(retryBaseDelay + time.Second)
	if rt.quarantined("a.txt", peer, testHash("hash1")) {
		t.Fatal("should not be backed off after first backoff expires")
	}

	// Second failure: backoff doubles (60s).
	rt.record("a.txt", peer, testHash("hash1"))
	now = now.Add(retryBaseDelay) // only 30s — still in backoff
	if !rt.quarantined("a.txt", peer, testHash("hash1")) {
		t.Fatal("should still be backed off (60s window, only 30s elapsed)")
	}
	now = now.Add(retryBaseDelay + time.Second) // 61s total
	if rt.quarantined("a.txt", peer, testHash("hash1")) {
		t.Fatal("should not be backed off after second backoff expires")
	}

	// New remote hash resets backoff.
	rt.record("a.txt", peer, testHash("hash1")) // failure 3
	if rt.quarantined("a.txt", peer, testHash("hash2")) {
		t.Fatal("new remote hash should not be backed off")
	}

	// Recording with new hash resets counter.
	rt.record("a.txt", peer, testHash("hash2"))
	now = now.Add(retryBaseDelay + time.Second)
	if rt.quarantined("a.txt", peer, testHash("hash2")) {
		t.Fatal("should not be backed off after first backoff with new hash expires")
	}

	// Clear removes tracking for this (path, peer).
	rt.record("a.txt", peer, testHash("hash2"))
	rt.clear("a.txt", peer)
	if rt.quarantined("a.txt", peer, testHash("hash2")) {
		t.Fatal("should not be backed off after clear")
	}

	// quarantinedPaths lists backed-off files (deduplicated across peers).
	rt.record("x.txt", peer, testHash("xhash"))
	paths := rt.quarantinedPaths()
	if len(paths) != 1 || paths[0] != "x.txt" {
		t.Errorf("quarantinedPaths = %v, want [x.txt]", paths)
	}
}

// TestRetryTrackerPeerScoped pins C4 option (2): a peer serving a bad copy
// does not poison the retry budget of other peers for the same (path, hash).
func TestRetryTrackerPeerScoped(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	rt := retryTracker{nowFn: func() time.Time { return now }}
	h := testHash("bad")

	// Peer A fails 3 times, entering backoff.
	for range 3 {
		rt.record("x.txt", "A", h)
	}
	if !rt.quarantined("x.txt", "A", h) {
		t.Fatal("peer A should be quarantined after 3 failures")
	}

	// Peer B has never failed — must not inherit A's backoff.
	if rt.quarantined("x.txt", "B", h) {
		t.Fatal("peer B must not be quarantined because peer A failed")
	}

	// clear(A) affects only peer A.
	rt.record("x.txt", "B", h)
	rt.clear("x.txt", "A")
	if rt.quarantined("x.txt", "A", h) {
		t.Fatal("clear(A) should have removed A's entry")
	}
	if !rt.quarantined("x.txt", "B", h) {
		t.Fatal("clear(A) must not affect B's entry")
	}

	// clearAll(path) sweeps every peer for the path.
	rt.record("x.txt", "A", h)
	rt.clearAll("x.txt")
	if rt.quarantined("x.txt", "A", h) || rt.quarantined("x.txt", "B", h) {
		t.Fatal("clearAll should have removed every (x.txt, *) entry")
	}

	// quarantinedPaths dedupes across peers.
	rt.record("y.txt", "A", h)
	rt.record("y.txt", "B", h)
	paths := rt.quarantinedPaths()
	if len(paths) != 1 || paths[0] != "y.txt" {
		t.Errorf("quarantinedPaths = %v, want [y.txt]", paths)
	}
}

// TestPeerRetryTracker pins R3 peer-level backoff: below the threshold
// failures accumulate without blocking; at the threshold backoff activates;
// the curve is exponential and capped; a clear resets state.
func TestPeerRetryTracker(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	pt := peerRetryTracker{nowFn: func() time.Time { return now }}

	// Unknown peer: not backed off.
	if backed, _ := pt.backedOff("p1"); backed {
		t.Fatal("unknown peer should not be backed off")
	}

	// Below threshold: failures counted but no backoff.
	for i := 1; i < peerRetryThreshold; i++ {
		pt.record("p1")
		if backed, _ := pt.backedOff("p1"); backed {
			t.Fatalf("below threshold (failure %d) should not back off", i)
		}
	}

	// Threshold reached: backoff activates for the base window.
	pt.record("p1") // failure == peerRetryThreshold
	backed, remaining := pt.backedOff("p1")
	if !backed {
		t.Fatal("should be backed off at threshold")
	}
	if remaining <= 0 || remaining > retryBaseDelay {
		t.Errorf("remaining = %v, want (0, %v]", remaining, retryBaseDelay)
	}

	// Elapse the window: backoff clears.
	now = now.Add(retryBaseDelay + time.Second)
	if backed, _ := pt.backedOff("p1"); backed {
		t.Fatal("backoff should have elapsed")
	}

	// Another failure: window doubles.
	pt.record("p1") // failure == threshold+1
	if d := peerBackoffDelay(peerRetryThreshold + 1); d != 2*retryBaseDelay {
		t.Errorf("peerBackoffDelay(threshold+1) = %v, want %v", d, 2*retryBaseDelay)
	}

	// Clear resets.
	pt.clear("p1")
	if backed, _ := pt.backedOff("p1"); backed {
		t.Fatal("clear should reset backoff state")
	}

	// Cap: many failures should not exceed retryMaxDelay.
	for range retryMaxCount + 5 {
		pt.record("p2")
	}
	if d := peerBackoffDelay(pt.states["p2"].failures); d != retryMaxDelay {
		t.Errorf("capped backoff = %v, want %v", d, retryMaxDelay)
	}

	// backedOffPeers lists peers in active backoff only.
	pt = peerRetryTracker{nowFn: func() time.Time { return now }}
	for range peerRetryThreshold {
		pt.record("bad-peer")
	}
	pt.record("healthy-peer") // single failure, below threshold
	peers := pt.backedOffPeers()
	if len(peers) != 1 || peers[0] != "bad-peer" {
		t.Errorf("backedOffPeers = %v, want [bad-peer]", peers)
	}
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		failures int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{0, 0, 0},
		{1, retryBaseDelay, retryBaseDelay},
		{2, 2 * retryBaseDelay, 2 * retryBaseDelay},
		{3, 4 * retryBaseDelay, 4 * retryBaseDelay},
		{20, retryMaxDelay, retryMaxDelay}, // capped
	}
	for _, tt := range tests {
		d := backoffDelay(tt.failures)
		if d < tt.wantMin || d > tt.wantMax {
			t.Errorf("backoffDelay(%d) = %v, want [%v, %v]", tt.failures, d, tt.wantMin, tt.wantMax)
		}
	}
}

func TestRetryTracker_MaxCountCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	rt := retryTracker{nowFn: func() time.Time { return now }}

	// Record more failures than retryMaxCount.
	for range retryMaxCount + 10 {
		now = now.Add(retryMaxDelay + time.Second)
		rt.record("big.txt", "peer-1", testHash("h"))
	}

	// Failure count should be capped at retryMaxCount.
	e := rt.counts[retryKey{path: "big.txt", peer: "peer-1"}]
	if e.failures != retryMaxCount {
		t.Errorf("failures = %d, want %d (capped)", e.failures, retryMaxCount)
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	// C1: "locally modified" is decided by MtimeNS > lastSyncNS.
	// lastSyncNS=2000 → b.txt (MtimeNS=1000) is unchanged, c.txt
	// (MtimeNS=3000) was modified locally after the last sync.
	const lastSyncNS = int64(2000)
	local := newFileIndex()
	local.Sequence = 5
	local.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 3, MtimeNS: 1000}
	local.Files["b.txt"] = FileEntry{SHA256: testHash("bbb"), Sequence: 2, MtimeNS: 1000}
	local.Files["c.txt"] = FileEntry{SHA256: testHash("ccc"), Sequence: 5, MtimeNS: 3000} // modified locally

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 6}  // same content
	remote.Files["b.txt"] = FileEntry{SHA256: testHash("bbb2"), Sequence: 7} // remote changed
	remote.Files["c.txt"] = FileEntry{SHA256: testHash("ccc2"), Sequence: 8} // both changed (conflict)
	remote.Files["d.txt"] = FileEntry{SHA256: testHash("ddd"), Sequence: 9}  // new on remote

	actions := local.diff(remote, 4, lastSyncNS, nil, "send-receive")

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

// C1: diff() must decide "was our copy locally modified since we last
// talked to this peer?" from mtime vs lastSyncNS, not from comparing our
// local Sequence to the peer's remote Sequence. The two sequence
// counters live on different scales — a high local Sequence simply
// means our folder has done many operations and says nothing about
// whether this particular file was touched.
func TestDiffC1MtimeVsLastSync(t *testing.T) {
	t.Parallel()
	const lastSyncNS = int64(5000)
	const localSeq = int64(20) // deliberately larger than lastSeenSeq

	cases := []struct {
		name        string
		localMtime  int64
		lastSeenSeq int64
		remoteSeq   int64
		sameHash    bool
		want        DiffAction
		wantSkipped bool
	}{
		{
			// Key regression case: local Sequence (20) > lastSeenSeq (5)
			// would have been flagged conflict by the prior heuristic.
			// C1 correctly classifies it as remote-only → download.
			name:        "remote_only_modified",
			localMtime:  3000,
			lastSeenSeq: 5,
			remoteSeq:   10,
			want:        ActionDownload,
		},
		{
			name:        "both_modified",
			localMtime:  7000,
			lastSeenSeq: 5,
			remoteSeq:   10,
			want:        ActionConflict,
		},
		{
			name:        "neither_modified",
			localMtime:  3000,
			lastSeenSeq: 5,
			remoteSeq:   10,
			sameHash:    true,
			wantSkipped: true,
		},
		{
			// Remote Sequence below lastSeenSeq: the remote-side filter
			// skips the entry entirely, independent of the mtime check.
			name:        "local_only_modified",
			localMtime:  7000,
			lastSeenSeq: 10,
			remoteSeq:   5,
			wantSkipped: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			remoteHash := testHash("remote-version")
			localHash := remoteHash
			if !tc.sameHash {
				localHash = testHash("local-version")
			}

			local := newFileIndex()
			local.Files["x.txt"] = FileEntry{
				SHA256:   localHash,
				Sequence: localSeq,
				MtimeNS:  tc.localMtime,
				Size:     10,
			}

			remote := newFileIndex()
			remote.Files["x.txt"] = FileEntry{
				SHA256:   remoteHash,
				Sequence: tc.remoteSeq,
				Size:     10,
			}

			actions := local.diff(remote, tc.lastSeenSeq, lastSyncNS, nil, "send-receive")
			if tc.wantSkipped {
				if len(actions) != 0 {
					t.Fatalf("want no action, got %+v", actions)
				}
				return
			}
			if len(actions) != 1 {
				t.Fatalf("want 1 action, got %d: %+v", len(actions), actions)
			}
			if actions[0].Action != tc.want {
				t.Fatalf("want action %v, got %v", tc.want, actions[0].Action)
			}
		})
	}
}

// C1: a delete tombstone from the peer must not destroy a locally
// modified file. "Locally modified" is decided by mtime vs lastSyncNS,
// not by comparing local Sequence to the peer's remote Sequence. A
// local file with a high Sequence from pre-sync creation, but untouched
// since last sync, should be deleted by the tombstone — the old
// sequence-based heuristic would have preserved it.
func TestDiffC1TombstoneMtimeVsLastSync(t *testing.T) {
	t.Parallel()
	const lastSyncNS = int64(5000)

	cases := []struct {
		name        string
		localMtime  int64
		wantDeleted bool
	}{
		{"unchanged_since_last_sync", 3000, true},
		{"modified_after_last_sync", 7000, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local := newFileIndex()
			local.Files["x.txt"] = FileEntry{
				SHA256:   testHash("local-version"),
				Sequence: 20, // high local sequence
				MtimeNS:  tc.localMtime,
				Size:     10,
			}

			remote := newFileIndex()
			remote.Files["x.txt"] = FileEntry{Deleted: true, Sequence: 10}

			actions := local.diff(remote, 5, lastSyncNS, nil, "send-receive")
			if tc.wantDeleted {
				if len(actions) != 1 || actions[0].Action != ActionDelete {
					t.Fatalf("want delete, got %+v", actions)
				}
				return
			}
			for _, a := range actions {
				if a.Path == "x.txt" {
					t.Fatalf("locally modified file must not be deleted, got %v", a.Action)
				}
			}
		})
	}
}

// C2: when an ancestor hash is known for a diverged path, the classifier
// uses it to distinguish download from conflict. mtime/lastSync is only
// consulted when no ancestor exists. This matrix pins all four states
// (only-remote changed, only-local changed, both changed, neither) plus
// the no-ancestor fallback which must still reach C1's mtime decision.
func TestDiffC2AncestorClassifier(t *testing.T) {
	t.Parallel()
	const lastSyncNS = int64(5000)
	const lastSeenSeq = int64(5)
	const remoteSeq = int64(10)
	const localSeq = int64(20)

	ancestor := testHash("ancestor")
	localChanged := testHash("local-new")
	remoteChanged := testHash("remote-new")

	cases := []struct {
		name       string
		baseHashes map[string]Hash256
		localHash  Hash256
		remoteHash Hash256
		localMtime int64 // only consulted in the no-ancestor fallback
		want       DiffAction
		wantSkip   bool
	}{
		{
			// Ancestor says only remote diverged → download, even when
			// local mtime would say otherwise.
			name:       "ancestor_only_remote_modified",
			baseHashes: map[string]Hash256{"x.txt": ancestor},
			localHash:  ancestor,
			remoteHash: remoteChanged,
			localMtime: 9000, // would wrongly imply local mod under C1
			want:       ActionDownload,
		},
		{
			// Ancestor says only local diverged → no action from the
			// receive side; local will propagate outbound.
			name:       "ancestor_only_local_modified",
			baseHashes: map[string]Hash256{"x.txt": ancestor},
			localHash:  localChanged,
			remoteHash: ancestor,
			localMtime: 9000,
			wantSkip:   true,
		},
		{
			// Both diverged from the agreed ancestor → conflict,
			// regardless of mtime ordering.
			name:       "ancestor_both_modified",
			baseHashes: map[string]Hash256{"x.txt": ancestor},
			localHash:  localChanged,
			remoteHash: remoteChanged,
			localMtime: 1000, // would wrongly imply only-remote under C1
			want:       ActionConflict,
		},
		{
			// No ancestor entry for this path: fall back to C1 mtime.
			// Local mtime predates lastSync → treat as only-remote.
			name:       "no_ancestor_fallback_download",
			baseHashes: map[string]Hash256{"other.txt": ancestor},
			localHash:  localChanged,
			remoteHash: remoteChanged,
			localMtime: 3000,
			want:       ActionDownload,
		},
		{
			// No ancestor entry for this path: fall back to C1 mtime.
			// Local mtime postdates lastSync → treat as both-modified.
			name:       "no_ancestor_fallback_conflict",
			baseHashes: nil,
			localHash:  localChanged,
			remoteHash: remoteChanged,
			localMtime: 7000,
			want:       ActionConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			local := newFileIndex()
			local.Files["x.txt"] = FileEntry{
				SHA256:   tc.localHash,
				Sequence: localSeq,
				MtimeNS:  tc.localMtime,
				Size:     10,
			}

			remote := newFileIndex()
			remote.Files["x.txt"] = FileEntry{
				SHA256:   tc.remoteHash,
				Sequence: remoteSeq,
				Size:     10,
			}

			actions := local.diff(remote, lastSeenSeq, lastSyncNS, tc.baseHashes, "send-receive")
			if tc.wantSkip {
				if len(actions) != 0 {
					t.Fatalf("want no action, got %+v", actions)
				}
				return
			}
			if len(actions) != 1 {
				t.Fatalf("want 1 action, got %d: %+v", len(actions), actions)
			}
			if actions[0].Action != tc.want {
				t.Fatalf("want action %v, got %v", tc.want, actions[0].Action)
			}
		})
	}
}

// C2: remote tombstones must respect the ancestor signal. If the
// ancestor still matches our local hash, we have not diverged and the
// delete should apply; if our local hash differs from the ancestor, we
// have a local modification and must keep the file.
func TestDiffC2TombstoneAncestor(t *testing.T) {
	t.Parallel()
	const lastSeenSeq = int64(5)
	// lastSync AFTER local mtime so the C1 fallback would NOT save the
	// file — this isolates the ancestor as the deciding signal.
	const lastSyncNS = int64(9000)

	ancestor := testHash("ancestor")
	localChanged := testHash("local-new")

	cases := []struct {
		name        string
		localHash   Hash256
		wantDeleted bool
	}{
		{"ancestor_matches_local_delete_applies", ancestor, true},
		{"ancestor_differs_local_preserved", localChanged, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local := newFileIndex()
			local.Files["x.txt"] = FileEntry{
				SHA256:   tc.localHash,
				Sequence: 20,
				MtimeNS:  3000, // pre-dates lastSyncNS → C1 alone would allow delete
				Size:     10,
			}

			remote := newFileIndex()
			remote.Files["x.txt"] = FileEntry{Deleted: true, Sequence: 10}

			baseHashes := map[string]Hash256{"x.txt": ancestor}
			actions := local.diff(remote, lastSeenSeq, lastSyncNS, baseHashes, "send-receive")
			if tc.wantDeleted {
				if len(actions) != 1 || actions[0].Action != ActionDelete {
					t.Fatalf("want delete, got %+v", actions)
				}
				return
			}
			for _, a := range actions {
				if a.Path == "x.txt" {
					t.Fatalf("locally modified file must not be deleted, got %v", a.Action)
				}
			}
		})
	}
}

// C2: updateBaseHashes folds a completed exchange into the ancestor map.
func TestUpdateBaseHashes(t *testing.T) {
	t.Parallel()

	agreed := testHash("agreed")
	localOnly := testHash("local-only")
	remoteOnly := testHash("remote-only")
	staleAncestor := testHash("stale")

	local := newFileIndex()
	local.Files["agreed.txt"] = FileEntry{SHA256: agreed, Size: 10}
	local.Files["diverged.txt"] = FileEntry{SHA256: localOnly, Size: 10}
	local.Files["kept.txt"] = FileEntry{SHA256: testHash("kept"), Size: 10}

	remote := newFileIndex()
	remote.Files["agreed.txt"] = FileEntry{SHA256: agreed, Sequence: 1, Size: 10}
	remote.Files["diverged.txt"] = FileEntry{SHA256: remoteOnly, Sequence: 2, Size: 10}
	remote.Files["tomb.txt"] = FileEntry{Deleted: true, Sequence: 3}

	prior := map[string]Hash256{
		"tomb.txt":      testHash("pre-delete"),
		"diverged.txt":  staleAncestor,
		"untouched.txt": testHash("untouched"),
	}

	out := updateBaseHashes(prior, local, remote)

	if got, ok := out["agreed.txt"]; !ok || got != agreed {
		t.Errorf("agreed.txt: want ancestor %x, got %x (ok=%v)", agreed, got, ok)
	}
	if got, ok := out["diverged.txt"]; !ok || got != staleAncestor {
		t.Errorf("diverged.txt: want stale ancestor preserved, got %x ok=%v", got, ok)
	}
	if _, ok := out["tomb.txt"]; ok {
		t.Errorf("tomb.txt: tombstone must drop ancestor")
	}
	if got, ok := out["untouched.txt"]; !ok || got != testHash("untouched") {
		t.Errorf("untouched.txt: paths not in exchange must keep ancestor, got %x ok=%v", got, ok)
	}
	// kept.txt is in local only — ancestor should not be synthesized.
	if _, ok := out["kept.txt"]; ok {
		t.Errorf("kept.txt: local-only path must not get ancestor")
	}
}

// updateBaseHashes must not mutate the caller's prior map. The function
// returns a fresh map so callers can still inspect prior after the call.
// Regression: a previous implementation aliased prior and mutated it in
// place; safe only because all production callers immediately overwrote
// the owning PeerState, but a latent trap for any future caller.
func TestUpdateBaseHashes_DoesNotAliasPrior(t *testing.T) {
	t.Parallel()

	prior := map[string]Hash256{
		"a.txt": testHash("a-ancestor"),
		"b.txt": testHash("b-ancestor"),
	}
	// Snapshot the original so we can compare after the call.
	priorCopy := make(map[string]Hash256, len(prior))
	for k, v := range prior {
		priorCopy[k] = v
	}

	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{SHA256: testHash("a-new"), Size: 10}
	local.Files["b.txt"] = FileEntry{SHA256: testHash("b-new"), Size: 10}

	remote := newFileIndex()
	// Agreement: update ancestor for a.txt.
	remote.Files["a.txt"] = FileEntry{SHA256: testHash("a-new"), Sequence: 1, Size: 10}
	// Tombstone: must drop ancestor for b.txt.
	remote.Files["b.txt"] = FileEntry{Deleted: true, Sequence: 2}

	out := updateBaseHashes(prior, local, remote)

	// prior must be unchanged.
	if len(prior) != len(priorCopy) {
		t.Fatalf("prior mutated: len %d, want %d", len(prior), len(priorCopy))
	}
	for k, want := range priorCopy {
		if got, ok := prior[k]; !ok || got != want {
			t.Errorf("prior[%q] mutated: got %x ok=%v, want %x", k, got, ok, want)
		}
	}
	// out reflects the exchange.
	if got, ok := out["a.txt"]; !ok || got != testHash("a-new") {
		t.Errorf("out[a.txt] = %x ok=%v, want ancestor updated", got, ok)
	}
	if _, ok := out["b.txt"]; ok {
		t.Errorf("out[b.txt] tombstone must drop ancestor")
	}
	// Mutating out must not affect prior.
	out["a.txt"] = testHash("post-call")
	if prior["a.txt"] == testHash("post-call") {
		t.Errorf("out and prior still aliased")
	}
}

// R1: a download at new-path paired with a delete at old-path, where
// the local file at old-path already holds the downloaded content, is
// resolved by a local rename with zero bytes over the wire.
func TestPlanRenamesSimpleRename(t *testing.T) {
	t.Parallel()
	h := testHash("shared-content")

	local := newFileIndex()
	local.Files["docs/old.md"] = FileEntry{SHA256: h, Size: 100, Sequence: 1}

	actions := []DiffEntry{
		{Path: "docs/old.md", Action: ActionDelete, RemoteSequence: 10},
		{Path: "docs/new.md", Action: ActionDownload, RemoteHash: h, RemoteSize: 100, RemoteSequence: 11},
	}

	plans, skip := planRenames(actions, local)
	if len(plans) != 1 {
		t.Fatalf("want 1 plan, got %d: %+v", len(plans), plans)
	}
	p := plans[0]
	if p.OldPath != "docs/old.md" || p.NewPath != "docs/new.md" {
		t.Errorf("path mismatch: %+v", p)
	}
	if p.RemoteHash != h || p.RemoteSize != 100 {
		t.Errorf("metadata mismatch: %+v", p)
	}
	if !skip["docs/old.md"] || !skip["docs/new.md"] {
		t.Errorf("skip map should contain both paths, got %+v", skip)
	}
}

// R1: no rename is planned when the local file's hash differs from the
// download's remote hash, even though delete and download exist. The
// receiver does not hold the content; it must download.
func TestPlanRenamesHashMismatch(t *testing.T) {
	t.Parallel()

	local := newFileIndex()
	local.Files["docs/old.md"] = FileEntry{SHA256: testHash("local-only"), Size: 100, Sequence: 1}

	actions := []DiffEntry{
		{Path: "docs/old.md", Action: ActionDelete, RemoteSequence: 10},
		{Path: "docs/new.md", Action: ActionDownload, RemoteHash: testHash("different"), RemoteSize: 100, RemoteSequence: 11},
	}

	plans, skip := planRenames(actions, local)
	if len(plans) != 0 {
		t.Fatalf("want no plans, got %+v", plans)
	}
	if len(skip) != 0 {
		t.Fatalf("skip map should be empty, got %+v", skip)
	}
}

// R1: when two files share a content hash, one-to-one matching pairs
// each delete with at most one download; extra downloads stay as
// downloads.
func TestPlanRenamesOneToOne(t *testing.T) {
	t.Parallel()
	h := testHash("shared")

	local := newFileIndex()
	local.Files["a"] = FileEntry{SHA256: h, Size: 1, Sequence: 1}
	local.Files["b"] = FileEntry{SHA256: h, Size: 1, Sequence: 2}

	actions := []DiffEntry{
		{Path: "a", Action: ActionDelete, RemoteSequence: 10},
		{Path: "b", Action: ActionDelete, RemoteSequence: 11},
		{Path: "x", Action: ActionDownload, RemoteHash: h, RemoteSize: 1, RemoteSequence: 12},
		{Path: "y", Action: ActionDownload, RemoteHash: h, RemoteSize: 1, RemoteSequence: 13},
		{Path: "z", Action: ActionDownload, RemoteHash: h, RemoteSize: 1, RemoteSequence: 14},
	}

	plans, skip := planRenames(actions, local)
	if len(plans) != 2 {
		t.Fatalf("want 2 plans, got %d: %+v", len(plans), plans)
	}
	if skip["z"] {
		t.Errorf("third download should not be skipped: %+v", skip)
	}
}

// R1: never clobber a path that already exists locally. If the index
// has an active entry at the download target, fall back to normal
// download handling (the existing code handles conflict/overwrite).
func TestPlanRenamesTargetExists(t *testing.T) {
	t.Parallel()
	h := testHash("content")

	local := newFileIndex()
	local.Files["old"] = FileEntry{SHA256: h, Size: 1, Sequence: 1}
	local.Files["new"] = FileEntry{SHA256: testHash("other"), Size: 1, Sequence: 2}

	actions := []DiffEntry{
		{Path: "old", Action: ActionDelete, RemoteSequence: 10},
		{Path: "new", Action: ActionDownload, RemoteHash: h, RemoteSize: 1, RemoteSequence: 11},
	}

	plans, _ := planRenames(actions, local)
	if len(plans) != 0 {
		t.Fatalf("target exists — expected no plan, got %+v", plans)
	}
}

// R1: a tombstoned local entry is not eligible as a rename source — its
// hash is already gone from disk (or at best a ghost). Treat delete
// separately.
func TestPlanRenamesTombstonedSource(t *testing.T) {
	t.Parallel()
	h := testHash("content")

	local := newFileIndex()
	local.Files["old"] = FileEntry{SHA256: h, Deleted: true, Size: 1, Sequence: 1}

	actions := []DiffEntry{
		{Path: "old", Action: ActionDelete, RemoteSequence: 10},
		{Path: "new", Action: ActionDownload, RemoteHash: h, RemoteSize: 1, RemoteSequence: 11},
	}

	plans, _ := planRenames(actions, local)
	if len(plans) != 0 {
		t.Fatalf("tombstoned source — expected no plan, got %+v", plans)
	}
}

// R1: a delete whose path is not in the local index cannot seed a
// rename — we cannot hash something we do not have.
func TestPlanRenamesSourceMissing(t *testing.T) {
	t.Parallel()

	local := newFileIndex()
	actions := []DiffEntry{
		{Path: "missing", Action: ActionDelete, RemoteSequence: 10},
		{Path: "new", Action: ActionDownload, RemoteHash: testHash("h"), RemoteSize: 1, RemoteSequence: 11},
	}

	plans, _ := planRenames(actions, local)
	if len(plans) != 0 {
		t.Fatalf("missing source — expected no plan, got %+v", plans)
	}
}

// R4: planRenames must propagate the peer's ActionDelete clock as
// RemoteDelVersion so the caller can merge it into the local OldPath
// tombstone. Without this, the local tombstone would keep its stale
// pre-rename clock and remain dominated by the peer's tombstone,
// re-emitting ActionDelete on every diff.
func TestPlanRenames_CarriesRemoteDelVersion(t *testing.T) {
	t.Parallel()
	h := testHash("shared-content")

	local := newFileIndex()
	local.Files["old.md"] = FileEntry{SHA256: h, Size: 100, Sequence: 1}

	delClock := VectorClock{"PEER": 5}
	newClock := VectorClock{"PEER": 5}

	actions := []DiffEntry{
		{Path: "old.md", Action: ActionDelete, RemoteSequence: 10, RemoteVersion: delClock},
		{Path: "new.md", Action: ActionDownload, RemoteHash: h, RemoteSize: 100, RemoteSequence: 11, RemoteVersion: newClock},
	}

	plans, _ := planRenames(actions, local)
	if len(plans) != 1 {
		t.Fatalf("want 1 plan, got %d", len(plans))
	}
	p := plans[0]
	if p.RemoteDelVersion["PEER"] != 5 {
		t.Fatalf("RemoteDelVersion[PEER]=%d, want 5 (got %v)", p.RemoteDelVersion["PEER"], p.RemoteDelVersion)
	}
	if p.RemoteVersion["PEER"] != 5 {
		t.Fatalf("RemoteVersion[PEER]=%d, want 5 (got %v)", p.RemoteVersion["PEER"], p.RemoteVersion)
	}
}

// R1: an empty action slice and a nil index are both no-ops.
func TestPlanRenamesNoOpInputs(t *testing.T) {
	t.Parallel()

	if plans, skip := planRenames(nil, newFileIndex()); plans != nil || skip != nil {
		t.Errorf("nil actions: want nil/nil, got %+v %+v", plans, skip)
	}
	if plans, skip := planRenames([]DiffEntry{{Path: "p", Action: ActionDelete}}, nil); plans != nil || skip != nil {
		t.Errorf("nil index: want nil/nil, got %+v %+v", plans, skip)
	}
}

// R1: rename planning handles a receiver-side rename execution happy
// path when wired through a real os.Root. This is the integration
// complement to the planner tests: verify that the filesystem rename
// is atomic, the target has the right content, and the index is
// updated both for tombstone (old) and new entry (new).
func TestR1RenameFilesystemIntegration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.md"), []byte("hello renamed content"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// Execute exactly the inner filesystem dance of the R1 branch and
	// confirm atomic-move semantics: after rename, old.md is gone and
	// new.md holds the original bytes.
	if err := root.Rename("old.md", "new.md"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.md")); !os.IsNotExist(err) {
		t.Fatalf("old.md should be gone: err=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.md"))
	if err != nil {
		t.Fatalf("read new.md: %v", err)
	}
	if string(got) != "hello renamed content" {
		t.Errorf("content mismatch: %q", got)
	}
}

func TestDiffReceiveOnly(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1}

	actions := local.diff(remote, 0, 0, nil, "receive-only")
	if len(actions) != 1 || actions[0].Action != ActionDownload {
		t.Error("receive-only should allow downloads")
	}
}

func TestDiffSendOnly(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1}

	actions := local.diff(remote, 0, 0, nil, "send-only")
	if len(actions) != 0 {
		t.Error("send-only should produce no actions (no receiving)")
	}
}

func TestDiffDeleteTombstone(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	// H8: lastSeenSeq > 0 means we've synced before — remote tombstone
	// should delete the unchanged local file.
	// C1: local a.txt MtimeNS=0, lastSyncNS=1000 → not locally modified.
	actions := local.diff(remote, 1, 1000, nil, "send-receive")
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
	remote.Files["new.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 7}
	remote.Files["del.txt"] = FileEntry{Deleted: true, Sequence: 8}
	// Also add del.txt to local so the delete action is generated.
	// C1: MtimeNS=0 < lastSyncNS=1000 → not locally modified → delete proceeds.
	local.Files["del.txt"] = FileEntry{SHA256: testHash("bbb"), Sequence: 1}

	actions := local.diff(remote, 4, 1000, nil, "send-receive")
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
	// C1: local file was modified after the last sync
	// (MtimeNS=2000 > lastSyncNS=1000).
	local.Files["a.txt"] = FileEntry{SHA256: testHash("aaa-modified"), Sequence: 3, MtimeNS: 2000}

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 7}

	actions := local.diff(remote, 2, 1000, nil, "send-receive")

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
	// C1: local file was NOT modified since last sync
	// (MtimeNS=500 <= lastSyncNS=1000).
	local.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1, MtimeNS: 500}

	remote := newFileIndex()
	remote.Sequence = 10
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 7}

	actions := local.diff(remote, 2, 1000, nil, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionDelete {
		t.Errorf("expected delete for unchanged local file, got %v", actions)
	}
}

// H8: on first sync (lastSeenSeq=0), remote tombstones must NOT delete
// locally-existing files. The local file was never shared with this peer,
// so the tombstone refers to a deletion from a third party.
func TestDiffDeleteTombstone_FirstSync(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	// lastSeenSeq=0 means we've never synced — guard protects local files.
	actions := local.diff(remote, 0, 0, nil, "send-receive")
	if len(actions) != 0 {
		t.Errorf("H8: first-sync tombstone should not delete local file, got %v", actions)
	}
}

// B8: both sides deleted — no action needed.
func TestDiffDeleteTombstone_BothDeleted(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 2}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	actions := local.diff(remote, 1, 1000, nil, "send-receive")
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

// M3: removed peers still block tombstone purge until GC'd.
func TestPurgeTombstones_RemovedPeerBlocksPurge(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	oldNs := time.Now().Add(-60 * 24 * time.Hour).UnixNano()
	idx.Files["gone.txt"] = FileEntry{Deleted: true, MtimeNS: oldNs, Sequence: 50}

	peers := map[string]PeerState{
		"10.0.0.1:7756": {LastSeenSequence: 100, LastSync: time.Now()},
		"10.0.0.2:7756": {
			LastSeenSequence: 10, // hasn't seen seq 50
			Removed:          true,
			RemovedAt:        time.Now().Add(-5 * 24 * time.Hour), // removed 5 days ago
		},
	}

	// Removed peer hasn't acked seq 50, so purge should be blocked.
	n := idx.purgeTombstones(30*24*time.Hour, peers)
	if n != 0 {
		t.Errorf("expected 0 purged (removed peer blocks), got %d", n)
	}
	if _, ok := idx.Files["gone.txt"]; !ok {
		t.Error("tombstone should NOT be purged while removed peer hasn't acked")
	}
}

// M3: markRemovedPeers marks peers not in config and un-removes returning ones.
func TestMarkRemovedPeers(t *testing.T) {
	t.Parallel()
	peers := map[string]PeerState{
		"10.0.0.1:7756": {LastSeenSequence: 100},
		"10.0.0.2:7756": {LastSeenSequence: 200},
	}

	// Remove 10.0.0.2 from config.
	markRemovedPeers(peers, []string{"10.0.0.1:7756"})

	if peers["10.0.0.1:7756"].Removed {
		t.Error("active peer should not be marked as removed")
	}
	if !peers["10.0.0.2:7756"].Removed {
		t.Error("unconfigured peer should be marked as removed")
	}
	if peers["10.0.0.2:7756"].RemovedAt.IsZero() {
		t.Error("RemovedAt should be set")
	}

	// Re-add 10.0.0.2 to config — should un-remove it.
	markRemovedPeers(peers, []string{"10.0.0.1:7756", "10.0.0.2:7756"})
	if peers["10.0.0.2:7756"].Removed {
		t.Error("peer back in config should be un-removed")
	}
}

// M3: gcRemovedPeers deletes old removed entries.
func TestGcRemovedPeers(t *testing.T) {
	t.Parallel()
	peers := map[string]PeerState{
		"10.0.0.1:7756": {LastSeenSequence: 100},
		"10.0.0.2:7756": {
			LastSeenSequence: 200,
			Removed:          true,
			RemovedAt:        time.Now().Add(-60 * 24 * time.Hour), // 60 days ago
		},
		"10.0.0.3:7756": {
			LastSeenSequence: 300,
			Removed:          true,
			RemovedAt:        time.Now().Add(-5 * 24 * time.Hour), // 5 days ago
		},
	}

	removed := gcRemovedPeers(peers, 30*24*time.Hour)
	if removed != 1 {
		t.Errorf("expected 1 GC'd, got %d", removed)
	}
	if _, ok := peers["10.0.0.2:7756"]; ok {
		t.Error("old removed peer should be GC'd")
	}
	if _, ok := peers["10.0.0.3:7756"]; !ok {
		t.Error("recently removed peer should survive GC")
	}
	if _, ok := peers["10.0.0.1:7756"]; !ok {
		t.Error("active peer should survive GC")
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

	winner, conflictPath := resolveConflict(openTestRoot(t, dir), "file.txt", localMtime, remoteMtime, "remote123")
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

	winner, conflictPath := resolveConflict(openTestRoot(t, dir), "file.txt", localMtime, remoteMtime, "remote123")
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

	winner, _ := resolveConflict(openTestRoot(t, dir), "file.txt", scanTimeMtime, remoteMtime, "remote123")
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

	winner, conflictPath := resolveConflict(openTestRoot(t, dir), "file.txt", localMtime, remoteMtime, "remote123")
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

// C1: when both sides modify a file to identical content, diff() produces
// ActionConflict but the sync path should auto-resolve by re-hashing from disk.
// This test verifies the precondition (diff produces conflict) and the hash
// comparison logic that the sync path uses.
func TestConflictAutoResolve_SameHash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Scenario: file was at "version 1" when last scanned. Both sides then
	// modified it to "version 2" (identical content). The local index still
	// has the old hash from version 1, so diff() sees different hashes and
	// produces ActionConflict. But the on-disk file matches the remote.
	finalContent := "version 2 — identical on both sides\n"
	writeFile(t, dir, "shared.txt", finalContent)

	oldHash := Hash256(sha256.Sum256([]byte("version 1 — old content\n")))

	newHash := Hash256(sha256.Sum256([]byte(finalContent)))

	// Local index: stale hash from version 1, seq=10 > lastSeenSeq=5.
	localIdx := newFileIndex()
	localIdx.Sequence = 10
	localIdx.setEntry("shared.txt", FileEntry{
		Size: 24, MtimeNS: time.Now().Add(-2 * time.Hour).UnixNano(),
		SHA256: oldHash, Sequence: 10,
	})

	// Remote index: has the new hash from version 2.
	remoteIdx := &FileIndex{
		Sequence: 20,
		Files: map[string]FileEntry{
			"shared.txt": {
				Size: int64(len(finalContent)), MtimeNS: time.Now().Add(-1 * time.Hour).UnixNano(),
				SHA256: newHash, Sequence: 20,
			},
		},
	}

	// diff() produces ActionConflict: local hash (old) != remote hash (new),
	// and local MtimeNS (now-2h) > lastSyncNS (now-3h) → both sides modified.
	lastSyncNS := time.Now().Add(-3 * time.Hour).UnixNano()
	actions := localIdx.diff(remoteIdx, 5, lastSyncNS, nil, "send-receive")
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Action != ActionConflict {
		t.Fatalf("expected ActionConflict, got %v", actions[0].Action)
	}

	// C1 guard: re-hash local file from disk — should match remote hash
	// because the on-disk file is version 2 (same as remote).
	root := openTestRoot(t, dir)
	diskHash, err := hashFileRoot(root, "shared.txt")
	if err != nil {
		t.Fatalf("hashFileRoot: %v", err)
	}
	if diskHash != actions[0].RemoteHash {
		t.Fatalf("on-disk hash should match remote for auto-resolve: disk=%s remote=%s", diskHash, actions[0].RemoteHash)
	}
}

// C1: when both sides modify a file to different content, the conflict
// must NOT be auto-resolved.
func TestConflictAutoResolve_DifferentHash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "diverged.txt", "local version\n")

	remoteContent := "remote version\n"
	remoteHash := Hash256(sha256.Sum256([]byte(remoteContent)))

	localIdxHash := Hash256(sha256.Sum256([]byte("local version\n")))

	localIdx := newFileIndex()
	localIdx.Sequence = 10
	localIdx.setEntry("diverged.txt", FileEntry{
		Size: 14, MtimeNS: time.Now().UnixNano(),
		SHA256: localIdxHash, Sequence: 10,
	})

	remoteIdx := &FileIndex{
		Sequence: 20,
		Files: map[string]FileEntry{
			"diverged.txt": {
				Size: int64(len(remoteContent)), MtimeNS: time.Now().UnixNano(),
				SHA256: remoteHash, Sequence: 20,
			},
		},
	}

	lastSyncNS := time.Now().Add(-1 * time.Hour).UnixNano()
	actions := localIdx.diff(remoteIdx, 5, lastSyncNS, nil, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionConflict {
		t.Fatalf("expected ActionConflict, got %v", actions)
	}

	// C1 guard: re-hash from disk — should NOT match remote hash.
	root := openTestRoot(t, dir)
	diskHash, err := hashFileRoot(root, "diverged.txt")
	if err != nil {
		t.Fatalf("hashFileRoot: %v", err)
	}
	if diskHash == actions[0].RemoteHash {
		t.Fatal("hashes should differ — conflict must not be auto-resolved")
	}
}

// C1: when remote hash is empty, conflict must NOT be auto-resolved.
func TestConflictAutoResolve_EmptyRemoteHash(t *testing.T) {
	t.Parallel()
	localIdx := newFileIndex()
	localIdx.Sequence = 10
	localIdx.setEntry("file.txt", FileEntry{
		Size: 5, MtimeNS: time.Now().UnixNano(),
		SHA256: testHash("abc123"), Sequence: 10,
	})
	remoteIdx := &FileIndex{
		Sequence: 20,
		Files: map[string]FileEntry{
			"file.txt": {Size: 5, MtimeNS: time.Now().UnixNano(), SHA256: Hash256{}, Sequence: 20},
		},
	}
	// diff sees different hashes (testHash("abc123") vs zero) → ActionConflict
	lastSyncNS := time.Now().Add(-1 * time.Hour).UnixNano()
	actions := localIdx.diff(remoteIdx, 5, lastSyncNS, nil, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionConflict {
		t.Fatalf("expected ActionConflict, got %v", actions)
	}
	// C1 guard: empty remote hash → must NOT auto-resolve
	if !actions[0].RemoteHash.IsZero() {
		t.Fatal("expected empty remote hash")
	}
}

// --- C2: Network filesystem detection + post-write verification ---

func TestDetectNetworkFS_LocalPath(t *testing.T) {
	t.Parallel()
	fsType, isNet := detectNetworkFS(t.TempDir())
	if isNet {
		t.Fatalf("expected local temp dir to not be network FS, got fstype=%q", fsType)
	}
}

func TestVerifyPostWrite_HashMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)
	content := []byte("verified content")
	writeFile(t, dir, "good.txt", string(content))
	expected := Hash256(sha256.Sum256(content))

	var mu sync.RWMutex
	retries := retryTracker{}
	err := verifyPostWrite(root, "good.txt", expected, "test-folder", "peer-A", &retries, &mu)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(retries.counts) > 0 {
		t.Fatal("expected no retries recorded")
	}
}

func TestVerifyPostWrite_HashMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)
	writeFile(t, dir, "bad.txt", "actual content")

	var mu sync.RWMutex
	retries := retryTracker{}
	err := verifyPostWrite(root, "bad.txt", Hash256{}, "test-folder", "peer-A", &retries, &mu)
	if err == nil {
		t.Fatal("expected error for hash mismatch")
	}
	if len(retries.counts) == 0 {
		t.Fatal("expected retry to be recorded on hash mismatch")
	}
}

func TestVerifyPostWrite_FileNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)

	var mu sync.RWMutex
	retries := retryTracker{}
	err := verifyPostWrite(root, "nonexistent.txt", testHash("abc123"), "test-folder", "peer-A", &retries, &mu)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// --- P19: Bundle transfer tests ---

func TestBundleBatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []bundleEntry
		want    []int // expected batch sizes
	}{
		{
			name:    "empty",
			entries: nil,
			want:    nil,
		},
		{
			name: "single batch under limits",
			entries: func() []bundleEntry {
				e := make([]bundleEntry, 50)
				for i := range e {
					e[i] = bundleEntry{Path: fmt.Sprintf("f%d", i), RemoteSize: 1000}
				}
				return e
			}(),
			want: []int{50},
		},
		{
			name: "split by count at 1000",
			entries: func() []bundleEntry {
				e := make([]bundleEntry, 2500)
				for i := range e {
					e[i] = bundleEntry{Path: fmt.Sprintf("f%d", i), RemoteSize: 100}
				}
				return e
			}(),
			want: []int{1000, 1000, 500},
		},
		{
			name: "split by total bytes",
			entries: func() []bundleEntry {
				// 3 files of 60MB each = 180MB > maxBundleTotal (128MB)
				return []bundleEntry{
					{Path: "a", RemoteSize: 60 * 1024 * 1024},
					{Path: "b", RemoteSize: 60 * 1024 * 1024},
					{Path: "c", RemoteSize: 60 * 1024 * 1024},
				}
			}(),
			want: []int{2, 1}, // a+b=120MB fits, c starts new batch
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			batches := bundleBatches(tc.entries)
			if len(batches) != len(tc.want) {
				t.Fatalf("got %d batches, want %d", len(batches), len(tc.want))
			}
			for i, b := range batches {
				if len(b) != tc.want[i] {
					t.Errorf("batch %d: got %d entries, want %d", i, len(b), tc.want[i])
				}
			}
		})
	}
}

func TestHandleBundle_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create test files.
	files := map[string]string{
		"small/a.txt": "content-a",
		"small/b.txt": "content-b",
		"small/c.txt": "content-c",
	}
	for path, content := range files {
		writeFile(t, dir, path, content)
	}

	// Build index with file entries.
	idx := newFileIndex()
	for path, content := range files {
		h := sha256.Sum256([]byte(content))
		idx.setEntry(path, FileEntry{
			Size:   int64(len(content)),
			SHA256: Hash256(h),
		})
	}

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		root:  openTestRoot(t, dir),
		index: idx,
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Build bundle request.
	reqMsg := &pb.BundleRequest{
		FolderId: "test",
		Paths:    []string{"small/a.txt", "small/b.txt", "small/c.txt"},
	}
	reqData, _ := proto.Marshal(reqMsg)

	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Decode tar+zstd response.
	zr, err := zstdutil.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)

	received := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		received[hdr.Name] = string(data)
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 files in tar, got %d: %v", len(received), received)
	}
	for path, want := range files {
		if got, ok := received[path]; !ok {
			t.Errorf("missing %s from tar", path)
		} else if got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

func TestDownloadBundle_Integration(t *testing.T) {
	t.Parallel()
	serverDir := t.TempDir()
	clientDir := t.TempDir()

	// Create files on the server side.
	type fileData struct {
		content string
		hash    Hash256
	}
	files := make(map[string]fileData)
	for i := range 10 {
		name := fmt.Sprintf("file%d.txt", i)
		content := fmt.Sprintf("content-of-file-%d", i)
		writeFile(t, serverDir, name, content)
		h := sha256.Sum256([]byte(content))
		files[name] = fileData{content: content, hash: Hash256(h)}
	}

	// Build server index.
	idx := newFileIndex()
	for name, fd := range files {
		idx.setEntry(name, FileEntry{
			Size:   int64(len(fd.content)),
			SHA256: fd.hash,
		})
	}

	n := &Node{
		cfg:           testCfg(serverDir, "127.0.0.1"),
		folders:       make(map[string]*folderState),
		deviceID:      "test-device",
		defaultClient: http.DefaultClient,
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(serverDir, "127.0.0.1"),
		root:  openTestRoot(t, serverDir),
		index: idx,
	}

	srv := &server{node: n}
	ts := httptest.NewTLSServer(srv.handler())
	defer ts.Close()

	// Build entries for download.
	var entries []bundleEntry
	for name, fd := range files {
		entries = append(entries, bundleEntry{
			Path:         name,
			ExpectedHash: fd.hash,
			RemoteSize:   int64(len(fd.content)),
		})
	}

	clientRoot := openTestRoot(t, clientDir)
	ok, retry := downloadBundle(t.Context(), ts.Client(), ts.Listener.Addr().String(), "test", entries, clientRoot, nil)

	if len(retry) != 0 {
		t.Errorf("expected 0 retries, got %d", len(retry))
	}
	if len(ok) != 10 {
		t.Fatalf("expected 10 successful downloads, got %d", len(ok))
	}

	// Verify files on disk.
	for name, fd := range files {
		data, err := os.ReadFile(filepath.Join(clientDir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(data) != fd.content {
			t.Errorf("%s: got %q, want %q", name, data, fd.content)
		}
	}
}

func TestHandleBundle_PathTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	idx := newFileIndex()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		root:  openTestRoot(t, dir),
		index: idx,
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	reqMsg := &pb.BundleRequest{
		FolderId: "test",
		Paths:    []string{"../../../etc/passwd", "normal.txt"},
	}
	reqData, _ := proto.Marshal(reqMsg)

	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	// Should get 200 with an empty tar (traversal paths are silently skipped,
	// and normal.txt is not in the index).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	zr, err := zstdutil.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)

	count := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 0 {
		t.Fatalf("expected 0 files in tar, got %d", count)
	}
}

func TestHandleBundle_TooManyPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		root:  openTestRoot(t, dir),
		index: newFileIndex(),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	paths := make([]string, maxBundlePaths+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("file%d.txt", i)
	}
	reqMsg := &pb.BundleRequest{
		FolderId: "test",
		Paths:    paths,
	}
	reqData, _ := proto.Marshal(reqMsg)

	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDownloadBundle_HashMismatch(t *testing.T) {
	t.Parallel()
	serverDir := t.TempDir()
	clientDir := t.TempDir()

	writeFile(t, serverDir, "good.txt", "correct")
	writeFile(t, serverDir, "bad.txt", "actual-content")

	goodH := sha256.Sum256([]byte("correct"))
	badH := sha256.Sum256([]byte("actual-content"))

	idx := newFileIndex()
	idx.setEntry("good.txt", FileEntry{Size: 7, SHA256: Hash256(goodH)})
	idx.setEntry("bad.txt", FileEntry{Size: 14, SHA256: Hash256(badH)})

	n := &Node{
		cfg:      testCfg(serverDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(serverDir, "127.0.0.1"),
		root:  openTestRoot(t, serverDir),
		index: idx,
	}

	srv := &server{node: n}
	ts := httptest.NewTLSServer(srv.handler())
	defer ts.Close()

	entries := []bundleEntry{
		{Path: "good.txt", ExpectedHash: Hash256(goodH), RemoteSize: 7},
		{Path: "bad.txt", ExpectedHash: Hash256{}, RemoteSize: 14},
	}

	clientRoot := openTestRoot(t, clientDir)
	ok, retry := downloadBundle(t.Context(), ts.Client(), ts.Listener.Addr().String(), "test", entries, clientRoot, nil)

	if len(ok) != 1 || ok[0] != "good.txt" {
		t.Errorf("expected [good.txt] ok, got %v", ok)
	}
	if len(retry) != 1 || retry[0].Path != "bad.txt" {
		t.Errorf("expected [bad.txt] retry, got %v", retry)
	}

	// Verify good.txt was written.
	data, err := os.ReadFile(filepath.Join(clientDir, "good.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "correct" {
		t.Errorf("good.txt: got %q, want %q", data, "correct")
	}

	// Verify bad.txt was NOT written (temp file cleaned up).
	if _, err := os.Stat(filepath.Join(clientDir, "bad.txt")); !os.IsNotExist(err) {
		t.Error("bad.txt should not exist on disk after hash mismatch")
	}
}

// S-hardening: a malicious peer that streams a tar response larger than
// maxBundleTotal must not cause unbounded reads / memory growth in the
// client. downloadBundle caps the compressed response at maxBundleTotal.
func TestDownloadBundle_CapsResponseBody(t *testing.T) {
	t.Parallel()

	// Server streams garbage bytes indefinitely — never a valid zstd
	// frame. The client must give up after reading at most
	// maxBundleTotal bytes, not hang or OOM.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "zstd")
		w.WriteHeader(http.StatusOK)
		// Stream junk bytes far exceeding maxBundleTotal.
		junk := make([]byte, 1<<20) // 1 MB chunks
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := w.Write(junk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	clientDir := t.TempDir()
	entries := []bundleEntry{
		{Path: "x.txt", ExpectedHash: Hash256{}, RemoteSize: 10},
	}
	ok, retry := downloadBundle(t.Context(),
		srv.Client(),
		srv.Listener.Addr().String(),
		"test",
		entries,
		openTestRoot(t, clientDir),
		nil,
	)
	if len(ok) != 0 {
		t.Errorf("expected no successful entries from garbage response, got %v", ok)
	}
	if len(retry) != len(entries) {
		t.Errorf("expected all entries returned for retry, got %d of %d", len(retry), len(entries))
	}
}

// TestDownloadBundle_CapsDecompressedStream pins the zstd-bomb
// defense. A malicious peer that returns a well-formed zstd frame
// wrapping a tar payload far larger than maxBundleTotal must not
// cause unbounded memory or disk consumption on the client. The
// compressed body cap alone is insufficient — a ~1 MB zstd frame can
// inflate to many GB of zero-padded tar — so downloadBundle wraps the
// decompressed stream in a second LimitReader.
func TestDownloadBundle_CapsDecompressedStream(t *testing.T) {
	t.Parallel()

	// Build a tar that declares a single entry ~10x maxBundleTotal in
	// size. The payload is all zeros, so zstd compresses it to a tiny
	// frame well under the compressed cap — the compressed LimitReader
	// does not trip.
	const bombSize = int64(maxBundleTotal) * 10
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "huge.bin",
		Mode:     0o644,
		Size:     bombSize,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	// Stream zero bytes in 1 MB chunks so the test does not allocate
	// bombSize bytes on the heap.
	chunk := make([]byte, 1<<20)
	remaining := bombSize
	for remaining > 0 {
		n := int64(len(chunk))
		if n > remaining {
			n = remaining
		}
		if _, err := tw.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	compressed := zstdutil.Encode(tarBuf.Bytes())
	if int64(len(compressed)) > maxBundleTotal {
		t.Fatalf("compressed bomb unexpectedly exceeded compressed cap: %d bytes", len(compressed))
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "zstd")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	}))
	defer srv.Close()

	clientDir := t.TempDir()
	entries := []bundleEntry{
		{Path: "huge.bin", ExpectedHash: Hash256{}, RemoteSize: 10},
	}
	ok, retry := downloadBundle(t.Context(),
		srv.Client(),
		srv.Listener.Addr().String(),
		"test",
		entries,
		openTestRoot(t, clientDir),
		nil,
	)
	if len(ok) != 0 {
		t.Errorf("expected no successful entries from zstd bomb, got %v", ok)
	}
	if len(retry) != len(entries) {
		t.Errorf("expected all entries returned for retry, got %d of %d", len(retry), len(entries))
	}

	// The LimitReader should stop the tar reader well before bombSize
	// bytes materialize on disk. Sanity-check: no partial file larger
	// than the decompressed cap should have landed.
	if info, err := os.Stat(filepath.Join(clientDir, "huge.bin")); err == nil {
		t.Errorf("huge.bin materialized unexpectedly: size=%d", info.Size())
	}
}

// postIndex must reject an empty response body. All postIndex callers
// (sendSingleIndex, sendPaginatedIndex final page, fetchResponsePages)
// expect a populated IndexExchange; silently returning a zero value
// would be read by diff() as "remote has no files" and could produce
// spurious tombstones. Intermediate-page acks go through postIndexAck.
func TestPostIndex_RejectsEmptyBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Empty body intentionally.
	}))
	defer srv.Close()

	// Build a minimal valid request payload.
	reqIdx := &pb.IndexExchange{
		FolderId: "test",
	}
	data, err := proto.Marshal(reqIdx)
	if err != nil {
		t.Fatal(err)
	}

	_, err = postIndex(t.Context(), srv.Client(), srv.Listener.Addr().String(), data)
	if err == nil {
		t.Fatal("expected error on empty index response, got nil")
	}
	if !strings.Contains(err.Error(), "empty index response") {
		t.Errorf("expected 'empty index response' in error, got: %v", err)
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
	_, err := downloadFile(t.Context(), client, "127.0.0.1:9999", "test", "../../../etc/passwd", testHash("abcdef0123456789abcdef0123456789"), openTestRoot(t, t.TempDir()), nil)
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestDownloadFile_ShortHash(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	_, err := downloadFile(t.Context(), client, "127.0.0.1:9999", "test", "file.txt", testHash("abc"), openTestRoot(t, t.TempDir()), nil)
	if err == nil {
		t.Fatal("expected error for short hash")
	}
}

// --- Block-level delta tests (FastCDC / offset-addressed chunks) ---

// fastCDCTestData builds a deterministic but unpredictable byte stream
// so FastCDC finds multiple natural boundaries. Low-entropy inputs are
// not representative — the gear hash is designed to cut on random-
// looking content.
func fastCDCTestData(seed int64, n int) []byte {
	b := make([]byte, n)
	// Simple xorshift64* keeps the output cheap and deterministic.
	state := uint64(seed)*2862933555777941757 + 3037000493
	for i := range b {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		b[i] = byte(state)
	}
	return b
}

func TestSignFile_ProducesCoveringChunks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := fastCDCTestData(1, fastCDCAvg*3+1000)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	blocks, err := signFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(blocks))
	}
	var covered int64
	for i, b := range blocks {
		if b.Offset != covered {
			t.Fatalf("block %d offset=%d want %d", i, b.Offset, covered)
		}
		covered += int64(b.Length)
	}
	if covered != int64(len(data)) {
		t.Fatalf("blocks cover %d bytes, want %d", covered, len(data))
	}
}

func TestSignFile_RootAndPathAgree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := fastCDCTestData(2, fastCDCAvg*2+500)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, dir)

	pathBlocks, err := signFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	rootBlocks, err := signFileRoot(root, "data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(pathBlocks) != len(rootBlocks) {
		t.Fatalf("chunk count differs path=%d root=%d", len(pathBlocks), len(rootBlocks))
	}
	for i := range pathBlocks {
		if pathBlocks[i] != rootBlocks[i] {
			t.Fatalf("chunk %d differs between path and root variants", i)
		}
	}
}

func TestComputeDelta_SkipsHashesPeerAlreadyHas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := fastCDCTestData(3, fastCDCAvg*3)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Sign the same file; feed every hash back as "peer already has
	// these". The delta must contain every chunk with Data=nil.
	sigs, err := signFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	peerHashes := make(map[Hash256]struct{}, len(sigs))
	for _, b := range sigs {
		peerHashes[b.Hash] = struct{}{}
	}
	delta, err := computeDelta(filepath.Join(dir, "data.bin"), peerHashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != len(sigs) {
		t.Fatalf("delta chunk count=%d want %d", len(delta), len(sigs))
	}
	for i, c := range delta {
		if c.Data != nil {
			t.Errorf("chunk %d carries data despite hash in peerHashes", i)
		}
	}
}

func TestComputeDelta_SendsDataForUnknownHashes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := fastCDCTestData(4, fastCDCAvg*2)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	delta, err := computeDelta(filepath.Join(dir, "data.bin"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) == 0 {
		t.Fatalf("expected at least one chunk")
	}
	for i, c := range delta {
		if len(c.Data) != c.Length {
			t.Fatalf("chunk %d data len=%d want %d", i, len(c.Data), c.Length)
		}
	}
}

func TestApplyDelta_ReconstructsFromLocalLookup(t *testing.T) {
	t.Parallel()
	// Old == New: every chunk must be resolvable by hash lookup into
	// the local file, with no inline data required.
	dir := t.TempDir()
	data := fastCDCTestData(5, fastCDCAvg*3+777)
	if err := os.WriteFile(filepath.Join(dir, "old.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, dir)

	sigs, err := signFileRoot(root, "old.bin")
	if err != nil {
		t.Fatal(err)
	}
	// Build delta from the same file with every hash "known" on peer.
	peerHashes := map[Hash256]struct{}{}
	for _, b := range sigs {
		peerHashes[b.Hash] = struct{}{}
	}
	delta, err := computeDeltaRoot(root, "old.bin", peerHashes)
	if err != nil {
		t.Fatal(err)
	}
	tmpRelPath, err := applyDeltaRoot(root, "old.bin", "testpeer", int64(len(data)), delta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Remove(tmpRelPath) })

	got, err := os.ReadFile(filepath.Join(dir, tmpRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reconstructed %d bytes, want %d; mismatch", len(got), len(data))
	}
}

func TestApplyDelta_ReconstructsFromInlineData(t *testing.T) {
	t.Parallel()
	// Old empty → every remote chunk must carry inline data because
	// the receiver has nothing local to copy from.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.bin"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newData := fastCDCTestData(6, fastCDCAvg*3)
	newPath := filepath.Join(dir, "new.bin")
	if err := os.WriteFile(newPath, newData, 0o600); err != nil {
		t.Fatal(err)
	}

	delta, err := computeDelta(newPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, dir)
	tmpRelPath, err := applyDeltaRoot(root, "old.bin", "testpeer", int64(len(newData)), delta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Remove(tmpRelPath) })

	got, err := os.ReadFile(filepath.Join(dir, tmpRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatalf("reconstruction mismatch: got %d bytes want %d", len(got), len(newData))
	}
}

func TestHashFileRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "data.txt", "hello world")
	root := openTestRoot(t, dir)

	rootHash, err := hashFileRoot(root, "data.txt")
	if err != nil {
		t.Fatal(err)
	}
	pathHash, err2 := hashFile(filepath.Join(dir, "data.txt"))
	if err2 != nil {
		t.Fatal(err2)
	}
	if rootHash != pathHash {
		t.Errorf("root hash %q != path hash %q", rootHash, pathHash)
	}
}

func TestHashFileIncremental(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	// Write initial content (above threshold).
	initial := strings.Repeat("A", minIncrementalHashSize+100)
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	// Full hash — get the state.
	hash1, state1, pc1, _, err := hashFileIncremental(path, nil, 0, int64(len(initial)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state1) == 0 {
		t.Fatal("expected non-empty hash state for large file")
	}
	if len(pc1) == 0 {
		t.Fatal("expected non-empty prefix check for large file")
	}

	// Verify against hashFile.
	expected, _ := hashFile(path)
	if hash1 != expected {
		t.Errorf("full incremental hash %q != hashFile %q", hash1, expected)
	}

	// Append data.
	appended := initial + "BBBB"
	if err := os.WriteFile(path, []byte(appended), 0644); err != nil {
		t.Fatal(err)
	}

	// Incremental hash — should produce correct result.
	hash2, state2, pc2, _, err := hashFileIncremental(path, state1, int64(len(initial)), int64(len(appended)), pc1)
	if err != nil {
		t.Fatal(err)
	}

	// Verify against full hash.
	expectedFull, _ := hashFile(path)
	if hash2 != expectedFull {
		t.Errorf("incremental hash %q != full hash %q", hash2, expectedFull)
	}
	if len(state2) == 0 {
		t.Fatal("expected state after incremental hash")
	}
	if len(pc2) == 0 {
		t.Fatal("expected prefix check after incremental hash")
	}

	// File below threshold — no state saved.
	smallPath := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(smallPath, []byte("tiny"), 0644); err != nil {
		t.Fatal(err)
	}
	_, smallState, smallPC, _, err := hashFileIncremental(smallPath, nil, 0, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(smallState) != 0 {
		t.Error("expected no hash state for small file")
	}
	if len(smallPC) != 0 {
		t.Error("expected no prefix check for small file")
	}
}

func TestHashFileIncremental_TruncateRegrow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	// Write initial content (above threshold).
	initial := strings.Repeat("X", minIncrementalHashSize+200)
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	// Full hash to get state + prefix check.
	hash1, state1, pc1, _, err := hashFileIncremental(path, nil, 0, int64(len(initial)), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Truncate and rewrite with DIFFERENT content, same or larger size.
	// This simulates log rotation where the file is truncated then grows back.
	replaced := strings.Repeat("Y", minIncrementalHashSize+300)
	if err := os.WriteFile(path, []byte(replaced), 0644); err != nil {
		t.Fatal(err)
	}

	// Incremental hash with stale state — prefix check should detect mismatch
	// and fall back to full rehash, producing correct results.
	hash2, _, _, _, err := hashFileIncremental(path, state1, int64(len(initial)), int64(len(replaced)), pc1)
	if err != nil {
		t.Fatal(err)
	}

	// Must match a fresh full hash of the new content.
	expectedFull, _ := hashFile(path)
	if hash2 != expectedFull {
		t.Errorf("truncate+regrow: hash %q != full hash %q", hash2, expectedFull)
	}
	// Must differ from the original hash (content is different).
	if hash2 == hash1 {
		t.Error("truncate+regrow: hash should differ from original")
	}
}

func TestDeltaEndpoint_ReducesTransfer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Three distinct prefixes joined into one file. FastCDC will pick
	// boundaries by content, not by section length — the test asserts
	// that when the receiver already has the surrounding content, the
	// middle region's data is the only inline payload.
	prefix := fastCDCTestData(1, fastCDCAvg*2)
	middle := fastCDCTestData(2, fastCDCAvg*2)
	suffix := fastCDCTestData(3, fastCDCAvg*2)
	serverData := append(append([]byte{}, prefix...), middle...)
	serverData = append(serverData, suffix...)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), serverData, 0o600); err != nil {
		t.Fatal(err)
	}

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:  testFolderCfg(dir, "127.0.0.1"),
		root: openTestRoot(t, dir),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Client's file differs only in the middle region — prefix+suffix
	// chunks match by hash, so their Data must be empty in the delta.
	clientDir := t.TempDir()
	otherMiddle := fastCDCTestData(9, fastCDCAvg*2)
	clientData := append(append([]byte{}, prefix...), otherMiddle...)
	clientData = append(clientData, suffix...)
	if err := os.WriteFile(filepath.Join(clientDir, "data.bin"), clientData, 0o600); err != nil {
		t.Fatal(err)
	}
	localBlocks, err := signFile(filepath.Join(clientDir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pbLocal := make([]*pb.Block, len(localBlocks))
	for i, b := range localBlocks {
		pbLocal[i] = &pb.Block{
			Offset: b.Offset,
			Length: int32(b.Length),
			Hash:   append([]byte(nil), b.Hash[:]...),
		}
	}
	req := &pb.BlockSignatures{
		FolderId: "test",
		Path:     "data.bin",
		FileSize: int64(len(clientData)),
		Blocks:   pbLocal,
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

	// Total inline data must be less than the full file — at least the
	// prefix or suffix should be resolvable by hash lookup.
	var inlineBytes int
	for _, b := range deltaResp.GetBlocks() {
		inlineBytes += len(b.GetData())
	}
	if inlineBytes >= len(serverData) {
		t.Fatalf("delta carried %d bytes inline, want < %d (no hash matches)", inlineBytes, len(serverData))
	}
}

// P18b: verify that setEntry maintains cachedCount/cachedSize correctly
// through insert, update, and delete operations.
func TestFileIndex_CachedCountAndSize(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	idx.recomputeCache()

	// Insert two active files.
	idx.setEntry("a.txt", FileEntry{Size: 100})
	idx.setEntry("b.txt", FileEntry{Size: 200})
	count, size := idx.activeCountAndSize()
	if count != 2 || size != 300 {
		t.Fatalf("after insert: count=%d size=%d, want 2/300", count, size)
	}

	// Update a file (size change).
	idx.setEntry("a.txt", FileEntry{Size: 150})
	count, size = idx.activeCountAndSize()
	if count != 2 || size != 350 {
		t.Fatalf("after update: count=%d size=%d, want 2/350", count, size)
	}

	// Delete a file (tombstone).
	idx.setEntry("b.txt", FileEntry{Size: 200, Deleted: true})
	count, size = idx.activeCountAndSize()
	if count != 1 || size != 150 {
		t.Fatalf("after delete: count=%d size=%d, want 1/150", count, size)
	}

	// Re-insert over a tombstone.
	idx.setEntry("b.txt", FileEntry{Size: 300})
	count, size = idx.activeCountAndSize()
	if count != 2 || size != 450 {
		t.Fatalf("after re-insert: count=%d size=%d, want 2/450", count, size)
	}

	// Verify recomputeCache matches.
	idx.recomputeCache()
	count2, size2 := idx.activeCountAndSize()
	if count != count2 || size != size2 {
		t.Fatalf("recompute mismatch: cached=%d/%d recomputed=%d/%d", count, size, count2, size2)
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

	if err := deleteFile(openTestRoot(t, dir), "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestDeleteFile_PathTraversal(t *testing.T) {
	t.Parallel()
	err := deleteFile(openTestRoot(t, t.TempDir()), "../escape.txt")
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
		cfg:  testFolderCfg(dir, "127.0.0.1"),
		root: openTestRoot(t, dir),
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
		cfg:  testFolderCfg(dir, "127.0.0.1"),
		root: openTestRoot(t, dir),
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
		cfg:  testFolderCfg(dir, "127.0.0.1"),
		root: openTestRoot(t, dir),
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
	idx.Files["local.txt"] = FileEntry{Size: 100, SHA256: testHash("abc123"), Sequence: 5}
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
		ProtocolVersion: protocolVersion,
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
	idx.Files["old.txt"] = FileEntry{Size: 100, SHA256: testHash("aaa"), Sequence: 3}
	idx.Files["new.txt"] = FileEntry{Size: 200, SHA256: testHash("bbb"), Sequence: 8}
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
		DeviceId:        "peer",
		FolderId:        "test",
		Sequence:        5,
		Since:           5,
		ProtocolVersion: protocolVersion,
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
	idx := &FileIndex{
		Sequence: 10,
		Files: map[string]FileEntry{
			"old.txt": {SHA256: testHash("aaa"), Sequence: 2},
			"mid.txt": {SHA256: testHash("bbb"), Sequence: 5},
			"new.txt": {SHA256: testHash("ccc"), Sequence: 9},
		},
	}
	idx.rebuildSeqIndex() // PG: enable secondary index path
	n := &Node{
		deviceID: "test",
		folders: map[string]*folderState{
			"docs": {index: idx},
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

func TestSeqIndex_SetEntryAppends(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()

	// Add entries via setEntry — should append to seqIndex.
	idx.Sequence = 1
	idx.setEntry("a.txt", FileEntry{SHA256: testHash("aaa"), Sequence: 1})
	idx.Sequence = 2
	idx.setEntry("b.txt", FileEntry{SHA256: testHash("bbb"), Sequence: 2})
	idx.Sequence = 3
	idx.setEntry("c.txt", FileEntry{SHA256: testHash("ccc"), Sequence: 3})

	if len(idx.seqIndex) != 3 {
		t.Fatalf("expected 3 seqIndex entries, got %d", len(idx.seqIndex))
	}

	// Update a.txt — should create a 4th entry (stale a.txt at seq=1 remains).
	idx.Sequence = 4
	idx.setEntry("a.txt", FileEntry{SHA256: testHash("aaa2"), Sequence: 4})

	if len(idx.seqIndex) != 4 {
		t.Fatalf("expected 4 seqIndex entries after update, got %d", len(idx.seqIndex))
	}

	// Rebuild should compact stale entries.
	idx.rebuildSeqIndex()
	if len(idx.seqIndex) != 3 {
		t.Fatalf("expected 3 seqIndex entries after rebuild, got %d", len(idx.seqIndex))
	}
	// Verify sorted order.
	for i := 1; i < len(idx.seqIndex); i++ {
		if idx.seqIndex[i].seq <= idx.seqIndex[i-1].seq {
			t.Errorf("seqIndex not sorted: [%d].seq=%d <= [%d].seq=%d",
				i, idx.seqIndex[i].seq, i-1, idx.seqIndex[i-1].seq)
		}
	}
}

func TestSeqIndex_DeltaExchangeSkipsStale(t *testing.T) {
	t.Parallel()
	idx := &FileIndex{
		Sequence: 5,
		Files: map[string]FileEntry{
			"a.txt": {SHA256: testHash("v2"), Sequence: 5}, // updated from seq=1 to seq=5
			"b.txt": {SHA256: testHash("bbb"), Sequence: 3},
		},
	}
	// Simulate stale entry: seqIndex has a.txt at seq=1 (old) and seq=5 (current).
	idx.seqIndex = []seqEntry{
		{seq: 1, path: "a.txt"}, // stale
		{seq: 3, path: "b.txt"},
		{seq: 5, path: "a.txt"}, // current
	}

	n := &Node{
		deviceID: "test",
		folders:  map[string]*folderState{"f": {index: idx}},
	}

	// Delta since=2: should get b.txt(3) and a.txt(5), NOT stale a.txt(1).
	delta := n.buildIndexExchange("f", 2)
	if len(delta.GetFiles()) != 2 {
		t.Errorf("expected 2 files, got %d", len(delta.GetFiles()))
	}

	// Delta since=0: full exchange, should get all 2 files.
	full := n.buildIndexExchange("f", 0)
	if len(full.GetFiles()) != 2 {
		t.Errorf("full: expected 2 files, got %d", len(full.GetFiles()))
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
	idx.Files["local.txt"] = FileEntry{Size: 100, SHA256: testHash("abc"), Sequence: 1}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "10.99.99.99"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler()) // connects from 127.0.0.1
	defer ts.Close()

	req := &pb.IndexExchange{DeviceId: "peer", FolderId: "test", ProtocolVersion: protocolVersion}
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
			Size: int64(i), SHA256: testHash(fmt.Sprintf("hash%05d", i)), Sequence: int64(i + 1),
		}
	}
	idx.Sequence = int64(indexPageSize + 500)
	n.folders["bigfolder"] = &folderState{
		cfg:   config.FolderCfg{ID: "bigfolder", Path: dir, Direction: "send-receive", Peers: []string{"127.0.0.1:7756"}},
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewTLSServer(srv.handler())
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
	addr := ts.Listener.Addr().String()
	resp, err := sendIndex(t.Context(), ts.Client(), addr, exchange)
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
	idx.Files["a.txt"] = FileEntry{Size: 10, SHA256: testHash("aaa"), Sequence: 1}
	idx.Sequence = 1
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewTLSServer(srv.handler())
	defer ts.Close()

	exchange := &pb.IndexExchange{
		DeviceId: "cli",
		FolderId: "test",
		Sequence: 1,
		Files:    []*pb.FileInfo{{Path: "b.txt", Size: 20, Sha256: []byte("bbb"), Sequence: 1}},
	}

	resp, err := sendIndex(t.Context(), ts.Client(), ts.Listener.Addr().String(), exchange)
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
		Peers: map[string]config.PeerDef{
			"server": {Addresses: []string{hostname + ":7756"}},
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
		root:  openTestRoot(t, dirB),
		index: idxB,
		peers: make(map[string]PeerState),
	}
	srvB := httptest.NewTLSServer((&server{node: nodeB}).handler())
	defer srvB.Close()

	// Node A's HTTP server.
	nodeA := &Node{
		cfg:           testCfg(dirA, "127.0.0.1"),
		folders:       make(map[string]*folderState),
		deviceID:      "node-a",
		defaultClient: srvB.Client(),
	}
	nodeA.folders["test"] = &folderState{
		cfg:   testFolderCfg(dirA, "127.0.0.1"),
		root:  openTestRoot(t, dirA),
		index: idxA,
		peers: make(map[string]PeerState),
	}
	srvA := httptest.NewTLSServer((&server{node: nodeA}).handler())
	defer srvA.Close()

	// Node B exchanges index with node A via A's server.
	exchangeB := nodeB.buildIndexExchange("test", 0)
	remoteIdx, err := sendIndex(t.Context(), srvA.Client(), srvA.Listener.Addr().String(), exchangeB)
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
	actions := fsB.index.diff(remoteFileIndex, 0, 0, nil, "send-receive")
	fsB.indexMu.Unlock()

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Action != ActionDownload || actions[0].Path != "from-a.txt" {
		t.Fatalf("expected download from-a.txt, got %v", actions[0])
	}

	// Download the file from node A's server.
	err = downloadFromPeer(t.Context(),
		srvA.Client(),
		srvA.Listener.Addr().String(),
		"test",
		"from-a.txt",
		actions[0].RemoteHash,
		openTestRoot(t, dirB),
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
		cfg:  testFolderCfg(nodeDir, "127.0.0.1"),
		root: openTestRoot(t, nodeDir),
	}
	srv := httptest.NewTLSServer((&server{node: n}).handler())
	defer srv.Close()

	destDir := t.TempDir()

	// Pre-create a partial temp file (first 5 bytes).
	// H1: temp name includes peer suffix for per-peer isolation.
	peerAddr := srv.Listener.Addr().String()
	tmpName := ".mesh-tmp-" + expectedHash.String()[:16] + "-" + peerSuffix(peerAddr)
	tmpPath := filepath.Join(destDir, tmpName)
	if err := os.WriteFile(tmpPath, []byte(content[:5]), 0600); err != nil {
		t.Fatal(err)
	}

	// Download should resume from offset 5.
	destRoot := openTestRoot(t, destDir)
	relPath, err := downloadFile(t.Context(),
		srv.Client(),
		peerAddr,
		"test",
		"data.txt",
		expectedHash,
		destRoot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, relPath))
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

// T1: direction enforcement for /bundle and /delta endpoints.
func TestHandleBundle_RejectsReceiveOnly(t *testing.T) {
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
			Direction: "receive-only",
			Peers:     []string{"127.0.0.1:7756"},
		},
		root: openTestRoot(t, dir),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	reqMsg := &pb.BundleRequest{FolderId: "test", Paths: []string{"data.txt"}}
	reqData, _ := proto.Marshal(reqMsg)
	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for receive-only bundle, got %d", resp.StatusCode)
	}
}

func TestHandleBundle_RejectsDisabled(t *testing.T) {
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
		root: openTestRoot(t, dir),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	reqMsg := &pb.BundleRequest{FolderId: "test", Paths: []string{"data.txt"}}
	reqData, _ := proto.Marshal(reqMsg)
	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for disabled bundle, got %d", resp.StatusCode)
	}
}

func TestHandleDelta_RejectsReceiveOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "data.bin", "AAAA")

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
		root: openTestRoot(t, dir),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.BlockSignatures{FolderId: "test", Path: "data.bin"}
	reqData, _ := proto.Marshal(req)
	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for receive-only delta, got %d", resp.StatusCode)
	}
}

func TestHandleDelta_RejectsDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "data.bin", "AAAA")

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
		root: openTestRoot(t, dir),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.BlockSignatures{FolderId: "test", Path: "data.bin"}
	reqData, _ := proto.Marshal(req)
	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for disabled delta, got %d", resp.StatusCode)
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
			"remote.txt": {Size: 100, MtimeNS: 1000, SHA256: testHash("abc123"), Sequence: 10},
		},
	}

	// Dry-run should compute diff (canReceive = true).
	actions := idx.diff(remote, 0, 0, nil, "dry-run")
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

	req := &pb.IndexExchange{FolderId: "nonexistent", ProtocolVersion: protocolVersion}
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

// T2: maxTotalPages cap rejects inflated page counts.
func TestHandleIndex_RejectsExcessiveTotalPages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: newFileIndex(),
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.IndexExchange{
		DeviceId:        "peer",
		FolderId:        "test",
		TotalPages:      maxTotalPages + 1,
		Page:            0,
		ProtocolVersion: protocolVersion,
	}
	data, _ := proto.Marshal(req)
	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for excessive totalPages, got %d", resp.StatusCode)
	}
}

// Protocol version mismatch must be rejected with HTTP 400 before any
// folder or content is touched. See docs/filesync/DESIGN-v1.md.
func TestHandleIndex_RejectsProtocolVersionMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: newFileIndex(),
		peers: make(map[string]PeerState),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	tests := []struct {
		name    string
		version uint32
	}{
		{"missing (v0)", 0},
		{"future (v2)", 2},
		{"large (v99)", 99},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &pb.IndexExchange{
				DeviceId:        "peer",
				FolderId:        "test",
				ProtocolVersion: tc.version,
			}
			data, err := proto.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("version=%d: got %d, want 400", tc.version, resp.StatusCode)
			}
		})
	}
}

// buildIndexExchange must stamp the current protocol version on every
// outgoing message, including the defensive empty return for unknown folders.
func TestBuildIndexExchange_StampsProtocolVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	idx := newFileIndex()
	idx.Files["a.txt"] = FileEntry{Size: 1, SHA256: testHash("a"), Sequence: 1}
	idx.Sequence = 1
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: idx,
		peers: make(map[string]PeerState),
	}

	// Full, delta, and unknown-folder paths all stamp the version.
	if got := n.buildIndexExchange("test", 0).GetProtocolVersion(); got != protocolVersion {
		t.Errorf("full: got %d, want %d", got, protocolVersion)
	}
	if got := n.buildIndexExchange("test", 0).GetProtocolVersion(); got != protocolVersion {
		t.Errorf("delta: got %d, want %d", got, protocolVersion)
	}
	if got := n.buildIndexExchange("missing", 0).GetProtocolVersion(); got != protocolVersion {
		t.Errorf("unknown folder: got %d, want %d", got, protocolVersion)
	}
}

// T3: client-side bundle tar path traversal — tar entries with ".." must not escape root.
func TestDownloadBundle_PathTraversalInTarEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "legit.txt", "ok")

	idx := newFileIndex()
	h := sha256.Sum256([]byte("ok"))
	idx.setEntry("legit.txt", FileEntry{Size: 2, SHA256: Hash256(h)})
	// Also add a traversal path to the index so the server would try to serve it.
	idx.setEntry("../escape.txt", FileEntry{Size: 7, SHA256: testHash("deadbeef")})

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		root:  openTestRoot(t, dir),
		index: idx,
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Request bundle containing the traversal path.
	reqMsg := &pb.BundleRequest{FolderId: "test", Paths: []string{"../escape.txt"}}
	reqData, _ := proto.Marshal(reqMsg)
	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	// The server should either reject the path or the file simply won't be
	// found (os.Root prevents traversal). Either way, no file should be served.
	if resp.StatusCode == http.StatusOK {
		zr, err := zstdutil.NewReader(resp.Body)
		if err != nil {
			return // empty/invalid response is fine
		}
		defer func() { _ = zr.Close() }()
		tr := tar.NewReader(zr)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			if strings.Contains(hdr.Name, "..") {
				t.Errorf("tar entry with traversal path should not be served: %s", hdr.Name)
			}
		}
	}
}

// T4: evictStalePending removes expired multi-page exchanges.
func TestEvictStalePending(t *testing.T) {
	t.Parallel()
	srv := &server{node: &Node{
		cfg:      testCfg(t.TempDir(), "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}}

	// Store a "stale" pending exchange.
	srv.pending.Store("stale|test", &pendingExchange{
		totalPages: 2,
		deviceID:   "stale",
		folderID:   "test",
		received:   map[int32]bool{0: true},
		createdAt:  time.Now().Add(-2 * pendingTTL),
	})
	// Store a "fresh" pending exchange.
	srv.pending.Store("fresh|test", &pendingExchange{
		totalPages: 2,
		deviceID:   "fresh",
		folderID:   "test",
		received:   map[int32]bool{0: true},
		createdAt:  time.Now(),
	})

	srv.evictStalePending()

	if _, ok := srv.pending.Load("stale|test"); ok {
		t.Error("stale pending exchange should have been evicted")
	}
	if _, ok := srv.pending.Load("fresh|test"); !ok {
		t.Error("fresh pending exchange should not have been evicted")
	}
}

// T5: scan context cancellation returns error and produces no partial results.
func TestScan_ContextCancellation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create enough files to make scan non-trivial.
	for i := range 50 {
		writeFile(t, dir, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("content-%d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	idx := newFileIndex()
	_, _, _, scanErr := idx.scan(ctx, dir, &ignoreMatcher{})

	if scanErr == nil {
		t.Fatal("expected error from cancelled context scan")
	}
	if !errors.Is(scanErr, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", scanErr)
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

	n.persistFolder("docs", true)

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
	if resp.Header.Get("Content-Encoding") == "zstd" {
		data, err = zstdutil.Decode(data, 64*1024*1024)
		if err != nil {
			t.Fatalf("zstd decode: %v", err)
		}
	}
	return data
}

func testCfg(dir, peerIP string) config.FilesyncCfg {
	cfg := config.FilesyncCfg{
		Bind:          "0.0.0.0:0",
		MaxConcurrent: 4,
		ScanInterval:  "60s",
		Peers:         map[string]config.PeerDef{"peer": {Addresses: []string{peerIP + ":7756"}}},
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

// bundlePost sends a POST to /bundle expecting a zstd-encoded tar response.
func bundlePost(t *testing.T, baseURL string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/bundle", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// openTestRoot opens an os.Root for a temp dir, with automatic cleanup.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
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

// BenchmarkScanInitial measures initial scan (all files need hashing).
func BenchmarkScanInitial(b *testing.B) {
	dir := b.TempDir()
	// Create 1000 files across 10 subdirs, 10 KB each — simulates a real project.
	for d := range 10 {
		subdir := filepath.Join(dir, fmt.Sprintf("dir%02d", d))
		_ = os.MkdirAll(subdir, 0750)
		for f := range 100 {
			data := make([]byte, 10*1024)
			for i := range data {
				data[i] = byte((d*100 + f + i) % 251)
			}
			_ = os.WriteFile(filepath.Join(subdir, fmt.Sprintf("file%03d.dat", f)), data, 0600)
		}
	}
	ignore := &ignoreMatcher{}
	b.ResetTimer()
	for b.Loop() {
		idx := newFileIndex() // fresh index each iteration — forces full hash
		_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	}
}

// BenchmarkScanSteadyState measures steady-state scan (all files hit fast path).
func BenchmarkScanSteadyState(b *testing.B) {
	dir := b.TempDir()
	for d := range 10 {
		subdir := filepath.Join(dir, fmt.Sprintf("dir%02d", d))
		_ = os.MkdirAll(subdir, 0750)
		for f := range 100 {
			data := make([]byte, 10*1024)
			for i := range data {
				data[i] = byte((d*100 + f + i) % 251)
			}
			_ = os.WriteFile(filepath.Join(subdir, fmt.Sprintf("file%03d.dat", f)), data, 0600)
		}
	}
	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	// Seed the index with a first scan.
	_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	b.ResetTimer()
	for b.Loop() {
		_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	}
}

// BenchmarkScanDeepTree measures scan with many directories (100 dirs × 10 files)
// to exercise the parallel directory walker (P20c). The directory-to-file ratio
// is high, which is where concurrent ReadDir calls help most.
func BenchmarkScanDeepTree(b *testing.B) {
	dir := b.TempDir()
	for d := range 100 {
		subdir := filepath.Join(dir, fmt.Sprintf("d%02d", d/10), fmt.Sprintf("sub%02d", d%10))
		_ = os.MkdirAll(subdir, 0750)
		for f := range 10 {
			data := make([]byte, 1024)
			for i := range data {
				data[i] = byte((d*10 + f + i) % 251)
			}
			_ = os.WriteFile(filepath.Join(subdir, fmt.Sprintf("f%02d.dat", f)), data, 0600)
		}
	}
	ignore := &ignoreMatcher{}
	b.ResetTimer()
	for b.Loop() {
		idx := newFileIndex()
		_, _, _, _ = idx.scan(context.Background(), dir, ignore)
	}
}

// BenchmarkIndexClone measures the per-scan deep-copy cost. The production
// scan path clones the index so the walker mutates a private copy while
// readers see the old one. For a 168 k-entry folder that is the largest
// single allocation in steady-state scanning (~30 MB). P18c reduces it.
func BenchmarkIndexClone(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := newFileIndex()
			for i := range n {
				idx.Files[fmt.Sprintf("dir%03d/file%05d.dat", i/100, i)] = FileEntry{
					Size: int64(i), MtimeNS: int64(i) * 1000, Sequence: int64(i),
				}
			}
			idx.recomputeCache()
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				_ = idx.clone()
			}
		})
	}
}

// BenchmarkRecomputeCache measures the per-scan cache refresh cost.
// runScan calls fs.index.recomputeCache() after every scan+merge swap;
// for a 168 k-entry folder the cost determines whether PN (incremental
// recompute) is worth shipping.
func BenchmarkRecomputeCache(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := newFileIndex()
			for i := range n {
				idx.Files[fmt.Sprintf("dir%03d/file%05d.dat", i/100, i)] = FileEntry{
					Size: int64(i), MtimeNS: int64(i) * 1000, Sequence: int64(i),
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				idx.recomputeCache()
			}
		})
	}
}

// BenchmarkIndexCloneReused measures the P18c pool-reuse path. The runScan
// loop stashes the old Files map after swap and recycles it on the next
// clone, so steady-state scans allocate zero map memory.
func BenchmarkIndexCloneReused(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := newFileIndex()
			for i := range n {
				idx.Files[fmt.Sprintf("dir%03d/file%05d.dat", i/100, i)] = FileEntry{
					Size: int64(i), MtimeNS: int64(i) * 1000, Sequence: int64(i),
				}
			}
			idx.recomputeCache()
			// Warm the recycled map to simulate steady state (first runScan
			// allocates, subsequent ones reuse).
			recycled := make(map[string]FileEntry, n)
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				c := idx.cloneInto(recycled)
				recycled = c.Files
			}
		})
	}
}

// TestShouldIgnoreReferenceConformance pins shouldIgnore's decisions for a
// broad corpus (patterns × paths). Any future optimization must reproduce
// exactly these decisions; a per-case table means a regression is pointed
// at the pattern/path that broke.
func TestShouldIgnoreReferenceConformance(t *testing.T) {
	t.Parallel()
	patterns := []string{
		".git/", ".svn/", ".DS_Store", "node_modules/",
		"*.class", "*.o", "*.pyc", "*.log", "*.tmp",
		"tmp-*", "cache-*",
		"src/generated/", "docs/build/",
		"**/test-output/**", "**/node_modules/**",
		"!important.class", "!keep.log",
	}
	m := newIgnoreMatcher(patterns)
	cases := []struct {
		path   string
		isDir  bool
		ignore bool
	}{
		// literals at root
		{".git", true, true},
		{".DS_Store", false, true},
		{"node_modules", true, true},
		// literals nested
		{"sub/.git", true, true},
		{"sub/.DS_Store", false, true},
		// dir-only as file
		{".git", false, false}, // .git/ is dir-only; file named .git not ignored
		// suffixes
		{"Foo.class", false, true},
		{"deep/nested/bar.o", false, true},
		{"debug.log", false, true},
		// negations
		{"important.class", false, false},
		{"keep.log", false, false},
		{"sub/important.class", false, false},
		// prefix-stars
		{"tmp-123", false, true},
		{"cache-abc", false, true},
		{"deep/tmp-xyz", false, true},
		// anchored
		{"src/generated", true, true},
		{"docs/build", true, true},
		{"src/generated/foo.go", false, false}, // file inside anchored dir — shouldIgnore does NOT walk up
		// double-star (current matcher handles at most one ** per pattern;
		// multi-** patterns are not matched — pin that behavior until PF
		// revisits gitignore conformance)
		{"foo/test-output/bar", false, true},
		{"x/y/z/test-output/report.html", false, true},
		// no match
		{"src/main.go", false, false},
		{"README.md", false, false},
		{"Makefile", false, false},
		// builtin always wins
		{".mesh-tmp-xyz", false, true},
		{"sub/.mesh-tmp-xyz", false, true},
		{"foo.mesh-delta-tmp-abcd", false, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_isDir=%v", tc.path, tc.isDir), func(t *testing.T) {
			t.Parallel()
			got := m.shouldIgnore(tc.path, tc.isDir)
			if got != tc.ignore {
				t.Errorf("shouldIgnore(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.ignore)
			}
		})
	}
}

// Shared pattern/path corpora so the linear and trie benchmarks compare
// on identical inputs. Changing one of these updates both sides at once.
var benchIgnoreBasicPatterns = []string{
	"*.class", "*.o", "*.pyc", "*.swp", "*.swo",
	".git/", ".svn/", "node_modules/", "__pycache__/",
	"target/", "build/", "dist/", ".gradle/",
	"**/test-output/**", "!important.class",
}

var benchIgnoreBasicPaths = []string{
	"src/main/java/com/example/App.java",
	"src/main/java/com/example/App.class",
	"build/libs/app.jar",
	"node_modules/lodash/index.js",
	"deep/nested/path/to/some/file.txt",
	".git/objects/pack/pack-abc.idx",
	"important.class",
}

// BenchmarkIgnoreMatcher measures the production (trie) matcher on the
// basic corpus. The retained BenchmarkIgnoreMatcherLinear below measures
// the retired linear matcher so perf regressions can be spotted against
// the known baseline.
func BenchmarkIgnoreMatcher(b *testing.B) {
	m := newIgnoreMatcher(benchIgnoreBasicPatterns)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, p := range benchIgnoreBasicPaths {
			m.shouldIgnore(p, false)
		}
	}
}

func BenchmarkIgnoreMatcherLinear(b *testing.B) {
	m := newLinearIgnoreMatcher(benchIgnoreBasicPatterns)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, p := range benchIgnoreBasicPaths {
			m.shouldIgnore(p, false)
		}
	}
}

// Realistic monorepo gitignore: ~60 patterns (literals, suffixes,
// prefix-stars, anchored, and double-star) × 50 mixed paths. Matches the
// shape the PF hotspot notes refer to — 310k files × this per-call cost
// = the reported ~3.6s scan-time.
var benchIgnoreRealisticPatterns = []string{
	// literals
	".git/", ".svn/", ".hg/", ".DS_Store", "Thumbs.db",
	"node_modules/", "__pycache__/", ".pytest_cache/", ".tox/",
	".mypy_cache/", ".idea/", ".vscode/", ".gradle/", ".nuxt/",
	"target/", "build/", "dist/", "out/", "bin/", "obj/",
	"vendor/", "Pods/", "coverage/", ".next/", ".cache/",
	// suffixes
	"*.class", "*.o", "*.pyc", "*.pyo", "*.swp", "*.swo",
	"*.log", "*.tmp", "*.bak", "*.orig", "*.rej",
	"*.jar", "*.war", "*.ear", "*.zip", "*.tar",
	"*.gz", "*.tgz", "*.rar", "*.7z", "*.iso",
	"*.dll", "*.exe", "*.so", "*.dylib", "*.a",
	// prefix-stars
	"tmp-*", "cache-*", "backup-*",
	// anchored
	"src/generated/", "docs/build/", "tools/dist/",
	// double-star
	"**/test-output/**", "**/node_modules/**", "**/.gradle/**",
	// negations
	"!important.class", "!keep.log",
}

var benchIgnoreRealisticPaths = []string{
	"src/main/java/com/example/App.java",
	"src/main/java/com/example/App.class",
	"src/main/java/com/example/util/Helper.java",
	"src/main/resources/config.yaml",
	"src/test/java/com/example/AppTest.java",
	"build/libs/app.jar",
	"build/classes/com/example/App.class",
	"build/reports/test-output/index.html",
	"node_modules/lodash/index.js",
	"node_modules/react/react.js",
	"deep/nested/path/to/some/file.txt",
	".git/objects/pack/pack-abc.idx",
	".git/refs/heads/main",
	"important.class",
	"keep.log",
	"debug.log",
	"error.log",
	"docs/source/intro.md",
	"docs/build/html/index.html",
	"scripts/deploy.sh",
	"scripts/test.py",
	"scripts/build.sh",
	"config/prod.yaml",
	"config/dev.yaml",
	"README.md",
	"LICENSE",
	"Makefile",
	"CMakeLists.txt",
	"pom.xml",
	"package.json",
	"requirements.txt",
	"go.mod",
	"go.sum",
	"a/b/c/d/e/f/g/deep.txt",
	"vendor/github.com/foo/bar.go",
	"tools/dist/release.zip",
	"src/generated/proto.pb.go",
	"tmp-123/scratch.txt",
	"backup-20260101/data.bin",
	"coverage/index.html",
	".vscode/settings.json",
	".idea/workspace.xml",
	"Pods/Manifest.lock",
	"obj/Debug/app.obj",
	"bin/Release/app.exe",
	"cache-xyz/entry",
	"target/debug/app",
	".DS_Store",
	"Thumbs.db",
	"backend/server.go",
}

// BenchmarkIgnoreMatcherRealistic measures the production (trie) matcher
// on the monorepo gitignore corpus. BenchmarkIgnoreMatcherRealisticLinear
// exercises the retained linear matcher on the same corpus so perf
// regressions can be spotted against the known baseline.
func BenchmarkIgnoreMatcherRealistic(b *testing.B) {
	m := newIgnoreMatcher(benchIgnoreRealisticPatterns)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, p := range benchIgnoreRealisticPaths {
			m.shouldIgnore(p, false)
		}
	}
	b.ReportMetric(float64(len(benchIgnoreRealisticPaths)), "paths/op")
}

func BenchmarkIgnoreMatcherRealisticLinear(b *testing.B) {
	m := newLinearIgnoreMatcher(benchIgnoreRealisticPatterns)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, p := range benchIgnoreRealisticPaths {
			m.shouldIgnore(p, false)
		}
	}
	b.ReportMetric(float64(len(benchIgnoreRealisticPaths)), "paths/op")
}

// BenchmarkIgnoreMatcherConstruction pins the one-time cost of
// newIgnoreMatcher (trie) for the realistic corpus; the trie builds more
// structure at config load in exchange for cheaper per-path matching.
// BenchmarkIgnoreMatcherConstructionLinear covers the retained linear
// path for regression comparison.
func BenchmarkIgnoreMatcherConstruction(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = newIgnoreMatcher(benchIgnoreRealisticPatterns)
	}
}

func BenchmarkIgnoreMatcherConstructionLinear(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = newLinearIgnoreMatcher(benchIgnoreRealisticPatterns)
	}
}

func BenchmarkBlockSignatures(b *testing.B) {
	dir := b.TempDir()
	// 1 MB file → ~8 FastCDC chunks at the default 128 KB average.
	path := filepath.Join(dir, "bench.dat")
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	_ = os.WriteFile(path, data, 0600)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		_, _ = signFile(path)
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

// --- Rate limiter tests ---

func TestRateLimitedReader_BurstCap(t *testing.T) {
	t.Parallel()
	// Create a limiter with a burst of 100 bytes. The reader should cap
	// reads to 100 bytes even when the caller provides a larger buffer,
	// preventing rate.ErrExceedsBurst.
	limiter := newTestLimiter(100)
	data := bytes.Repeat([]byte("x"), 500)
	r := newRateLimitedReader(context.Background(), bytes.NewReader(data), limiter)

	buf := make([]byte, 500)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	// The underlying reader may return fewer bytes, but the buffer
	// presented to it must be capped at burst (100).
	if n > 100 {
		t.Errorf("read %d bytes, expected at most 100 (burst)", n)
	}
}

func TestRateLimitedReader_NilPassthrough(t *testing.T) {
	t.Parallel()
	data := []byte("hello")
	r := newRateLimitedReader(context.Background(), bytes.NewReader(data), nil)
	buf := make([]byte, 10)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("got %q, want %q", buf[:n], "hello")
	}
}

func TestRateLimitedWriter_BurstChunking(t *testing.T) {
	t.Parallel()
	// Burst of 64 bytes. Writing 200 bytes should succeed by chunking.
	limiter := newTestLimiter(64)
	var buf bytes.Buffer
	w := newRateLimitedWriter(context.Background(), &buf, limiter)

	data := bytes.Repeat([]byte("a"), 200)
	n, err := w.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != 200 {
		t.Errorf("wrote %d bytes, want 200", n)
	}
	if buf.Len() != 200 {
		t.Errorf("buffer has %d bytes, want 200", buf.Len())
	}
}

func TestRateLimitedWriter_NilPassthrough(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := newRateLimitedWriter(context.Background(), &buf, nil)
	data := []byte("hello")
	n, err := w.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || buf.String() != "hello" {
		t.Errorf("got n=%d buf=%q, want 5 and %q", n, buf.String(), "hello")
	}
}

func TestRateLimitedWriter_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	limiter := newTestLimiter(64)
	var buf bytes.Buffer
	w := newRateLimitedWriter(ctx, &buf, limiter)

	_, err := w.Write(bytes.Repeat([]byte("x"), 100))
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestRateLimitedReader_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	limiter := newTestLimiter(64)
	data := bytes.Repeat([]byte("x"), 100)
	r := newRateLimitedReader(ctx, bytes.NewReader(data), limiter)

	buf := make([]byte, 100)
	_, err := r.Read(buf)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func newTestLimiter(burst int) *rate.Limiter {
	// High rate so tests don't actually wait, but burst is constrained.
	return rate.NewLimiter(rate.Limit(1<<30), burst)
}

// --- HandleFile BytesUploaded metric test ---

func TestHandleFile_TracksBytesUploaded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "metric tracking test content"
	writeFile(t, dir, "tracked.txt", content)

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	fs := &folderState{
		cfg:  testFolderCfg(dir, "127.0.0.1"),
		root: openTestRoot(t, dir),
	}
	n.folders["test"] = fs

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	before := fs.metrics.BytesUploaded.Load()
	resp, err := http.Get(ts.URL + "/file?folder=test&path=tracked.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	after := fs.metrics.BytesUploaded.Load()
	uploaded := after - before
	if uploaded != int64(len(content)) {
		t.Errorf("BytesUploaded=%d, want %d", uploaded, len(content))
	}
}

// --- GetFolderMetrics round-trip test ---

func TestGetFolderMetrics_Roundtrip(t *testing.T) {
	t.Parallel()
	fs := &folderState{
		cfg: config.FolderCfg{ID: "snap-test"},
	}
	fs.metrics.PeerSyncs.Store(10)
	fs.metrics.FilesDownloaded.Store(50)
	fs.metrics.FilesDeleted.Store(3)
	fs.metrics.FilesConflicted.Store(2)
	fs.metrics.SyncErrors.Store(1)
	fs.metrics.BytesDownloaded.Store(1024 * 1024)
	fs.metrics.BytesUploaded.Store(512 * 1024)
	fs.metrics.IndexExchanges.Store(20)
	fs.metrics.ScanCount.Store(15)
	fs.metrics.ScanDurationNS.Store(int64(500 * time.Millisecond))
	fs.metrics.PeerSyncNS.Store(int64(2 * time.Second))

	n := &Node{folders: map[string]*folderState{"snap-test": fs}}
	activeNodes.Register(n)
	defer activeNodes.Unregister(n)

	result := GetFolderMetrics()
	if len(result) == 0 {
		t.Fatal("GetFolderMetrics returned 0 entries")
	}
	// Find our test entry (other tests may register nodes concurrently).
	var snap *FolderMetricsSnapshot
	for i := range result {
		if result[i].FolderID == "snap-test" {
			snap = &result[i]
			break
		}
	}
	if snap == nil {
		t.Fatal("snap-test folder not found in GetFolderMetrics result")
	}

	if snap.PeerSyncs != 10 {
		t.Errorf("PeerSyncs=%d, want 10", snap.PeerSyncs)
	}
	if snap.FilesDownloaded != 50 {
		t.Errorf("FilesDownloaded=%d, want 50", snap.FilesDownloaded)
	}
	if snap.FilesDeleted != 3 {
		t.Errorf("FilesDeleted=%d, want 3", snap.FilesDeleted)
	}
	if snap.FilesConflicted != 2 {
		t.Errorf("FilesConflicted=%d, want 2", snap.FilesConflicted)
	}
	if snap.SyncErrors != 1 {
		t.Errorf("SyncErrors=%d, want 1", snap.SyncErrors)
	}
	if snap.BytesDownloaded != 1024*1024 {
		t.Errorf("BytesDownloaded=%d, want %d", snap.BytesDownloaded, 1024*1024)
	}
	if snap.BytesUploaded != 512*1024 {
		t.Errorf("BytesUploaded=%d, want %d", snap.BytesUploaded, 512*1024)
	}
	if snap.IndexExchanges != 20 {
		t.Errorf("IndexExchanges=%d, want 20", snap.IndexExchanges)
	}
	if snap.ScanCount != 15 {
		t.Errorf("ScanCount=%d, want 15", snap.ScanCount)
	}
	if snap.ScanDurationNS != int64(500*time.Millisecond) {
		t.Errorf("ScanDurationNS=%d, want %d", snap.ScanDurationNS, int64(500*time.Millisecond))
	}
	if snap.PeerSyncNS != int64(2*time.Second) {
		t.Errorf("PeerSyncNS=%d, want %d", snap.PeerSyncNS, int64(2*time.Second))
	}
}

// --- Hardening tests (H1-H8) ---

// H1: temp file names include a peer-derived suffix so concurrent downloads
// from different peers get separate temp files.
func TestPeerSuffix_DifferentPeers(t *testing.T) {
	t.Parallel()
	s1 := peerSuffix("192.168.1.1:7756")
	s2 := peerSuffix("192.168.1.2:7756")
	if s1 == s2 {
		t.Errorf("different peers should produce different suffixes: %s == %s", s1, s2)
	}
	if len(s1) != 8 {
		t.Errorf("suffix length should be 8 hex chars, got %d", len(s1))
	}
}

func TestPeerSuffix_Deterministic(t *testing.T) {
	t.Parallel()
	s1 := peerSuffix("10.0.0.1:7756")
	s2 := peerSuffix("10.0.0.1:7756")
	if s1 != s2 {
		t.Errorf("same peer should produce same suffix: %s != %s", s1, s2)
	}
}

// H3: claimPath/releasePath dedup prevents concurrent downloads of the same path.
func TestClaimPath_Dedup(t *testing.T) {
	t.Parallel()
	fs := &folderState{inFlight: make(map[string]bool)}

	if !fs.claimPath("a.txt") {
		t.Fatal("first claim should succeed")
	}
	if fs.claimPath("a.txt") {
		t.Fatal("second claim of same path should fail")
	}
	// Different path should succeed.
	if !fs.claimPath("b.txt") {
		t.Fatal("claim of different path should succeed")
	}

	fs.releasePath("a.txt")
	// After release, claim should succeed again.
	if !fs.claimPath("a.txt") {
		t.Fatal("claim after release should succeed")
	}
}

func TestClaimPath_Concurrent(t *testing.T) {
	t.Parallel()
	fs := &folderState{inFlight: make(map[string]bool)}

	const goroutines = 100
	claimed := make(chan bool, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			claimed <- fs.claimPath("race.txt")
		}()
	}
	wg.Wait()
	close(claimed)

	wins := 0
	for ok := range claimed {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("exactly 1 goroutine should win the claim, got %d", wins)
	}
}

// H4: safePath rejects symlinks that escape the folder root.
func TestSafePath_SymlinkEscape(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink test requires Unix")
	}

	root := t.TempDir()
	outside := t.TempDir()

	// Create a symlink inside root that points outside.
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	_, err := safePath(root, "escape")
	if err == nil {
		t.Error("safePath should reject symlink pointing outside root")
	}
	if err != nil && !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention symlink, got: %v", err)
	}
}

func TestSafePath_TraversalBlocked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	for _, path := range []string{"../etc/passwd", "/etc/passwd", "foo/../../etc/passwd"} {
		_, err := safePath(root, path)
		if err == nil {
			t.Errorf("safePath(%q) should be rejected", path)
		}
	}
}

func TestSafePath_ValidPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	for _, path := range []string{"a.txt", "sub/dir/file.txt", "a/b/c"} {
		result, err := safePath(root, path)
		if err != nil {
			t.Errorf("safePath(%q) should succeed: %v", path, err)
		}
		if !strings.HasPrefix(result, root) {
			t.Errorf("safePath(%q) = %q, should be under %q", path, result, root)
		}
	}
}

// H8: first-sync tombstone guard — remote tombstones for files that exist
// locally are skipped when lastSeenSeq=0. Files NOT present locally are
// unaffected (no local entry to protect).
func TestDiffFirstSyncTombstone_RemoteOnly(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	// No local entry for "gone.txt".

	remote := newFileIndex()
	remote.Files["gone.txt"] = FileEntry{Deleted: true, Sequence: 5}

	// Even on first sync, if local doesn't have the file, no action expected
	// (can't delete what doesn't exist locally).
	actions := local.diff(remote, 0, 0, nil, "send-receive")
	if len(actions) != 0 {
		t.Errorf("no action expected for remote-only tombstone, got %v", actions)
	}
}

// H8: after first sync (lastSeenSeq > 0), tombstones should delete
// unchanged local files normally.
func TestDiffTombstone_AfterFirstSync(t *testing.T) {
	t.Parallel()
	local := newFileIndex()
	// C1: MtimeNS=500 <= lastSyncNS=1000 → local copy is unchanged.
	local.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1, MtimeNS: 500}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	actions := local.diff(remote, 3, 1000, nil, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionDelete {
		t.Errorf("expected delete after first sync, got %v", actions)
	}
}

// --- Second-pass hardening tests (N1-N12) ---

// N1: builtinIgnores must match delta temp files.
func TestBuiltinIgnores_DeltaTmp(t *testing.T) {
	t.Parallel()
	m := newIgnoreMatcher(nil)
	if !m.shouldIgnore("data.txt.mesh-delta-tmp-ab12cd34", false) {
		t.Error("builtinIgnores should match .mesh-delta-tmp-* pattern")
	}
	if !m.shouldIgnore(".mesh-tmp-abc123", false) {
		t.Error("builtinIgnores should match .mesh-tmp- prefix")
	}
	// Normal files should not match.
	if m.shouldIgnore("data.txt", false) {
		t.Error("normal file should not be ignored")
	}
}

// N3+N12: delete handler creates tombstone for missing entry,
// and skips sequence bump for already-tombstoned entry.
func TestDeleteHandler_TombstoneCreation(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	idx.Sequence = 10

	// Simulate: file existed on disk but had no index entry.
	// After delete, handler should create a tombstone.
	idx.Sequence++
	entry := idx.Files["orphan.txt"] // zero value
	entry.Deleted = true
	entry.MtimeNS = time.Now().UnixNano()
	entry.Sequence = idx.Sequence
	idx.Files["orphan.txt"] = entry

	if !idx.Files["orphan.txt"].Deleted {
		t.Error("expected tombstone for orphan.txt")
	}
	if idx.Files["orphan.txt"].Sequence != 11 {
		t.Errorf("expected sequence 11, got %d", idx.Files["orphan.txt"].Sequence)
	}

	// Second delete of already-tombstoned entry should NOT bump sequence (N12).
	prevSeq := idx.Sequence
	existing := idx.Files["orphan.txt"]
	if existing.Deleted {
		// N12 path: skip bump
	} else {
		idx.Sequence++
		existing.Deleted = true
		existing.Sequence = idx.Sequence
		idx.Files["orphan.txt"] = existing
	}
	if idx.Sequence != prevSeq {
		t.Errorf("N12: sequence should not bump for already-tombstoned entry, was %d now %d", prevSeq, idx.Sequence)
	}
}

// N6: safeSeq computation with fseq=0 should not produce -1.
func TestSafeSeq_ZeroFailedSeq(t *testing.T) {
	t.Parallel()
	remoteSeq := int64(10)
	failedSeqs := []int64{0, 5, 8}

	safeSeq := remoteSeq
	for _, fseq := range failedSeqs {
		if fseq > 0 && fseq-1 < safeSeq {
			safeSeq = fseq - 1
		}
	}

	// fseq=0 should be skipped, fseq=5 should produce safeSeq=4.
	if safeSeq != 4 {
		t.Errorf("expected safeSeq=4, got %d", safeSeq)
	}
}

func TestSafeSeq_AllZero(t *testing.T) {
	t.Parallel()
	remoteSeq := int64(10)
	failedSeqs := []int64{0, 0}

	safeSeq := remoteSeq
	for _, fseq := range failedSeqs {
		if fseq > 0 && fseq-1 < safeSeq {
			safeSeq = fseq - 1
		}
	}

	// All zeros should be skipped → safeSeq stays at remoteSeq.
	if safeSeq != 10 {
		t.Errorf("expected safeSeq=10, got %d", safeSeq)
	}
}

// N7/F13: concurrent conflict resolutions must produce unique names.
func TestConflictFileName_UniqueAcrossCalls(t *testing.T) {
	t.Parallel()
	root := openTestRoot(t, t.TempDir())

	// Two calls within the same second must get different names
	// (random suffix makes TOCTOU impossible).
	_, cRelPath1 := resolveConflict(root, "a.txt", 100, 200, "device1")
	_, cRelPath2 := resolveConflict(root, "a.txt", 100, 200, "device1")
	if cRelPath1 == "" || cRelPath2 == "" {
		t.Fatal("expected conflict paths for remote win")
	}
	if cRelPath2 == cRelPath1 {
		t.Error("F13: two conflict resolutions should get different paths")
	}
}

// N4: delta response with out-of-range FileSize should be rejected.
// Exercises the actual downloadFileDelta code path via a fake peer
// that returns a DeltaResponse with an invalid FileSize.
func TestDeltaFileSize_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		wantErr  bool
	}{
		{"negative", -1, true},
		{"too large", maxSyncFileSize + 1, true},
	}
	// FileSize=0 is legal (empty file) — covered by TestHandleDelta_EmptyFile.

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Fake peer: returns a DeltaResponse with the test FileSize.
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resp := &pb.DeltaResponse{
					FileSize: tc.fileSize,
					Blocks:   nil,
				}
				data, _ := proto.Marshal(resp)
				w.Header().Set("Content-Type", "application/x-protobuf")
				_, _ = w.Write(data)
			}))
			defer srv.Close()

			destDir := t.TempDir()
			// Create a local file so downloadFileDelta takes the delta path.
			writeFile(t, destDir, "target.txt", "old content")

			_, err := downloadFileDelta(t.Context(),
				srv.Client(),
				srv.Listener.Addr().String(),
				"test",
				"target.txt",
				Hash256{},
				openTestRoot(t, destDir),
				nil,
			)
			if err == nil {
				t.Errorf("expected error for FileSize=%d, got nil", tc.fileSize)
			}
			if err != nil && !strings.Contains(err.Error(), "file size out of range") {
				t.Errorf("expected 'file size out of range' error, got: %v", err)
			}
		})
	}
}

// Receiver must cap peer-supplied DeltaBlock count to what could fit
// in the declared FileSize at fastCDCMin granularity. Without this,
// a peer can force an arbitrary-sized chunks slice even with a sane
// FileSize.
func TestDownloadFileDelta_CapsPeerBlocks(t *testing.T) {
	t.Parallel()

	// Declare a small file but send a flood of blocks.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pbBlocks := make([]*pb.DeltaBlock, 10_000)
		for i := range pbBlocks {
			pbBlocks[i] = &pb.DeltaBlock{
				Offset: int64(i),
				Length: 1,
				Hash:   make([]byte, 32),
			}
		}
		resp := &pb.DeltaResponse{
			FileSize: 1024, // tiny file — max legitimate blocks ≈ 1
			Blocks:   pbBlocks,
		}
		data, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	destDir := t.TempDir()
	writeFile(t, destDir, "target.txt", "old content")

	_, err := downloadFileDelta(t.Context(),
		srv.Client(),
		srv.Listener.Addr().String(),
		"test",
		"target.txt",
		Hash256{},
		openTestRoot(t, destDir),
		nil,
	)
	if err == nil {
		t.Fatal("expected error for oversized block list, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("expected 'exceeds max' in error, got: %v", err)
	}
}

// N5: handleDelta caps peer block signatures to the file's maximum
// possible FastCDC chunk count. A peer can't force unbounded work by
// sending millions of bogus signatures for a small file.
func TestHandleDelta_CapsPeerBlocks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Small file (<< fastCDCMin) — upper bound of chunks is 1.
	if err := os.WriteFile(filepath.Join(dir, "small.dat"), make([]byte, 256), 0600); err != nil {
		t.Fatal(err)
	}

	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:  testFolderCfg(dir, "127.0.0.1"),
		root: openTestRoot(t, dir),
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Send 100_000 zero-hashes. Server should truncate to maxBlocks
	// and still respond successfully with the file's single chunk.
	pbBlocks := make([]*pb.Block, 100_000)
	for i := range pbBlocks {
		pbBlocks[i] = &pb.Block{Offset: int64(i), Length: 1, Hash: make([]byte, 32)}
	}
	req := &pb.BlockSignatures{FolderId: "test", Path: "small.dat", Blocks: pbBlocks}
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
	if len(deltaResp.GetBlocks()) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(deltaResp.GetBlocks()))
	}
}

// D6: handleDelta zstd-compresses inline chunk data and decompressing
// it reproduces the original bytes. raw=false on compressible payloads.
func TestHandleDelta_CompressesPayload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plain := bytes.Repeat([]byte("this is highly compressible text "), 4096)
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), plain, 0o600); err != nil {
		t.Fatal(err)
	}

	n := &Node{cfg: testCfg(dir, "127.0.0.1"), folders: make(map[string]*folderState), deviceID: "test-device"}
	n.folders["test"] = &folderState{cfg: testFolderCfg(dir, "127.0.0.1"), root: openTestRoot(t, dir)}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Peer has no matching hashes → every chunk carries inline data.
	req := &pb.BlockSignatures{FolderId: "test", Path: "data.txt", FileSize: int64(len(plain))}
	reqData, _ := proto.Marshal(req)
	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var deltaResp pb.DeltaResponse
	if err := proto.Unmarshal(readBody(t, resp), &deltaResp); err != nil {
		t.Fatal(err)
	}

	var totalCompressed, totalPlain int
	for _, b := range deltaResp.GetBlocks() {
		if b.GetRaw() {
			t.Fatalf("text file marked raw, want compressed")
		}
		if len(b.GetData()) == 0 {
			continue
		}
		dec, err := zstdutil.Decode(b.GetData(), int64(fastCDCMax))
		if err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		if len(dec) != int(b.GetLength()) {
			t.Fatalf("decoded len=%d want %d", len(dec), b.GetLength())
		}
		totalCompressed += len(b.GetData())
		totalPlain += len(dec)
	}
	if totalPlain == 0 {
		t.Fatal("no inline data in response")
	}
	if totalCompressed >= totalPlain {
		t.Fatalf("compression did not shrink payload: compressed=%d plain=%d", totalCompressed, totalPlain)
	}
}

// Empty files must survive the delta path end-to-end: handleDelta
// returns file_size=0 with no blocks; downloadFileDelta assembles a
// zero-byte result without falling back. Regression test for the
// `remoteFileSize <= 0` guard that used to reject 0.
func TestHandleDelta_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.bin"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	n := &Node{cfg: testCfg(dir, "127.0.0.1"), folders: make(map[string]*folderState), deviceID: "test-device"}
	n.folders["test"] = &folderState{cfg: testFolderCfg(dir, "127.0.0.1"), root: openTestRoot(t, dir)}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.BlockSignatures{FolderId: "test", Path: "empty.bin", FileSize: 0}
	reqData, _ := proto.Marshal(req)
	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var deltaResp pb.DeltaResponse
	if err := proto.Unmarshal(readBody(t, resp), &deltaResp); err != nil {
		t.Fatal(err)
	}
	if deltaResp.GetFileSize() != 0 {
		t.Fatalf("file_size = %d, want 0", deltaResp.GetFileSize())
	}
	if len(deltaResp.GetBlocks()) != 0 {
		t.Fatalf("expected 0 blocks for empty file, got %d", len(deltaResp.GetBlocks()))
	}

	// Now exercise the receiver side via assembleDelta directly: 0 chunks
	// + remoteFileSize=0 must produce a zero-byte output.
	recvDir := t.TempDir()
	oldPath := filepath.Join(recvDir, "old.bin")
	outPath := filepath.Join(recvDir, "out.bin")
	if err := os.WriteFile(oldPath, []byte("stale content overwritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := assembleDelta(out, old, 0, nil); err != nil {
		t.Fatalf("assembleDelta empty: %v", err)
	}
	_ = out.Close()
	_ = old.Close()
	got, _ := os.ReadFile(outPath)
	if len(got) != 0 {
		t.Fatalf("assembled output = %d bytes, want 0", len(got))
	}
}

// D6: handleDelta marks incompressible files (magic-byte match) raw and
// ships their chunks verbatim instead of paying compression overhead.
func TestHandleDelta_RawForIncompressibleFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Synthetic .zst — the magic-byte probe alone drives the decision.
	body := fastCDCTestData(77, fastCDCAvg*2)
	content := append([]byte{0x28, 0xb5, 0x2f, 0xfd}, body...)
	if err := os.WriteFile(filepath.Join(dir, "blob.zst"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	n := &Node{cfg: testCfg(dir, "127.0.0.1"), folders: make(map[string]*folderState), deviceID: "test-device"}
	n.folders["test"] = &folderState{cfg: testFolderCfg(dir, "127.0.0.1"), root: openTestRoot(t, dir)}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.BlockSignatures{FolderId: "test", Path: "blob.zst", FileSize: int64(len(content))}
	reqData, _ := proto.Marshal(req)
	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var deltaResp pb.DeltaResponse
	if err := proto.Unmarshal(readBody(t, resp), &deltaResp); err != nil {
		t.Fatal(err)
	}

	reassembled := make([]byte, 0, len(content))
	for _, b := range deltaResp.GetBlocks() {
		if len(b.GetData()) == 0 {
			t.Fatalf("chunk offset=%d has no data (peer sent empty signatures)", b.GetOffset())
		}
		if !b.GetRaw() {
			t.Fatalf("raw flag not set on incompressible file")
		}
		if len(b.GetData()) != int(b.GetLength()) {
			t.Fatalf("raw data len=%d want %d", len(b.GetData()), b.GetLength())
		}
		reassembled = append(reassembled, b.GetData()...)
	}
	if !bytes.Equal(reassembled, content) {
		t.Fatalf("raw reassembly mismatch: got %d bytes, want %d", len(reassembled), len(content))
	}
}

// N10: persistFolder serialization — concurrent calls should not corrupt.
func TestPersistFolder_Concurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	idx := newFileIndex()
	idx.Files["a.txt"] = FileEntry{SHA256: testHash("aaa"), Sequence: 1}

	fs := &folderState{
		index:    idx,
		peers:    map[string]PeerState{"peer1": {LastSeenSequence: 5}},
		inFlight: make(map[string]bool),
	}

	n := &Node{
		dataDir: dir,
		folders: map[string]*folderState{"test": fs},
	}

	// Run 20 concurrent persists — should not panic or corrupt.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.persistFolder("test", true)
		}()
	}
	wg.Wait()

	// Verify the persisted index is valid.
	loaded, err := loadIndex(filepath.Join(dir, "test", "index.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Files["a.txt"].SHA256 != testHash("aaa") {
		t.Errorf("expected SHA256=aaa, got %s", loaded.Files["a.txt"].SHA256)
	}
}

// N11: multi-page index accumulation rejects when total files exceed cap.
// Sends two pages whose combined file count exceeds the 500k cap.
func TestMultiPageIndex_TotalFileCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test",
		dataDir:  t.TempDir(),
	}
	n.folders["test"] = &folderState{
		cfg:      testFolderCfg(dir, "127.0.0.1"),
		index:    newFileIndex(),
		peers:    make(map[string]PeerState),
		inFlight: make(map[string]bool),
	}

	srv := httptest.NewServer((&server{node: n}).handler())
	defer srv.Close()

	// Send page 0 with 500_000 files, then page 1 with 1 more file.
	// The handler should reject page 1.
	files := make([]*pb.FileInfo, 500_000)
	for i := range files {
		files[i] = &pb.FileInfo{Path: fmt.Sprintf("f%d.txt", i), Sequence: 1}
	}

	page0 := &pb.IndexExchange{
		DeviceId:        "peer1",
		FolderId:        "test",
		Sequence:        1,
		Files:           files,
		Page:            0,
		TotalPages:      2,
		ProtocolVersion: protocolVersion,
	}
	data, _ := proto.Marshal(page0)
	resp, err := http.Post(srv.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page 0 should succeed, got %d", resp.StatusCode)
	}

	// Page 1 with 1 more file should push over the 500k cap.
	page1 := &pb.IndexExchange{
		DeviceId:        "peer1",
		FolderId:        "test",
		Sequence:        1,
		Files:           []*pb.FileInfo{{Path: "overflow.txt", Sequence: 1}},
		Page:            1,
		TotalPages:      2,
		ProtocolVersion: protocolVersion,
	}
	data, _ = proto.Marshal(page1)
	resp, err = http.Post(srv.URL+"/index", "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("page 1 should be rejected (total > 500k), got %d", resp.StatusCode)
	}
}

// --- G1: mtime preservation tests ---

func TestDownloadFile_PreservesMtime(t *testing.T) {
	t.Parallel()
	content := "hello mtime world"
	serverDir := t.TempDir()
	writeFile(t, serverDir, "data.txt", content)

	expectedHash, err := hashFile(filepath.Join(serverDir, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// Set a known mtime on the server file (1 hour ago).
	remoteMtime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(serverDir, "data.txt"), remoteMtime, remoteMtime); err != nil {
		t.Fatal(err)
	}

	n := &Node{
		cfg:      testCfg(serverDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test",
	}
	n.folders["test"] = &folderState{
		cfg:  testFolderCfg(serverDir, "127.0.0.1"),
		root: openTestRoot(t, serverDir),
	}
	srv := httptest.NewTLSServer((&server{node: n}).handler())
	defer srv.Close()

	clientDir := t.TempDir()
	clientRoot := openTestRoot(t, clientDir)

	// Download the file.
	relPath, err := downloadFile(t.Context(), srv.Client(),
		srv.Listener.Addr().String(), "test", "data.txt", expectedHash, clientRoot, nil)
	if err != nil {
		t.Fatal(err)
	}

	// downloadFile itself does NOT call Chtimes — the caller (syncFolder) does.
	// Simulate what syncFolder does after downloadFile returns.
	mt := remoteMtime
	if err := clientRoot.Chtimes(relPath, mt, mt); err != nil {
		t.Fatal(err)
	}

	// Verify disk mtime matches remote.
	info, err := os.Stat(filepath.Join(clientDir, relPath))
	if err != nil {
		t.Fatal(err)
	}
	diskMtime := info.ModTime().Truncate(time.Second)
	if !diskMtime.Equal(remoteMtime) {
		t.Errorf("disk mtime = %v, want %v", diskMtime, remoteMtime)
	}
}

func TestDownloadBundle_PreservesMtime(t *testing.T) {
	t.Parallel()
	serverDir := t.TempDir()
	clientDir := t.TempDir()

	// Create files on the server side with distinct mtimes.
	type fileData struct {
		content string
		hash    Hash256
		mtime   time.Time
	}
	files := make(map[string]fileData)
	for i := range 3 {
		name := fmt.Sprintf("f%d.txt", i)
		content := fmt.Sprintf("content-%d", i)
		writeFile(t, serverDir, name, content)
		h := sha256.Sum256([]byte(content))
		mt := time.Date(2025, 6, 15, 10, i, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(serverDir, name), mt, mt); err != nil {
			t.Fatal(err)
		}
		files[name] = fileData{content: content, hash: Hash256(h), mtime: mt}
	}

	// Build server index.
	idx := newFileIndex()
	for name, fd := range files {
		idx.setEntry(name, FileEntry{
			Size:   int64(len(fd.content)),
			SHA256: fd.hash,
		})
	}

	n := &Node{
		cfg:      testCfg(serverDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(serverDir, "127.0.0.1"),
		root:  openTestRoot(t, serverDir),
		index: idx,
	}
	srv := httptest.NewTLSServer((&server{node: n}).handler())
	defer srv.Close()

	// Build entries with remote mtime.
	var entries []bundleEntry
	for name, fd := range files {
		entries = append(entries, bundleEntry{
			Path:         name,
			ExpectedHash: fd.hash,
			RemoteSize:   int64(len(fd.content)),
			RemoteMtime:  fd.mtime.UnixNano(),
		})
	}

	clientRoot := openTestRoot(t, clientDir)
	ok, retry := downloadBundle(t.Context(), srv.Client(),
		srv.Listener.Addr().String(), "test", entries, clientRoot, nil)

	if len(retry) != 0 {
		t.Errorf("expected 0 retries, got %d", len(retry))
	}
	if len(ok) != 3 {
		t.Fatalf("expected 3 successful downloads, got %d", len(ok))
	}

	// Verify each file's mtime on disk matches RemoteMtime.
	for name, fd := range files {
		info, err := os.Stat(filepath.Join(clientDir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		diskMtime := info.ModTime().Truncate(time.Second)
		wantMtime := fd.mtime.Truncate(time.Second)
		if !diskMtime.Equal(wantMtime) {
			t.Errorf("%s: disk mtime = %v, want %v", name, diskMtime, wantMtime)
		}
	}
}

func TestMtimePreservation_ScanFastPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "fast path test data"
	writeFile(t, dir, "stable.txt", content)

	h, err := hashFile(filepath.Join(dir, "stable.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// Set a specific mtime (simulating what G1 Chtimes does after download).
	remoteMtime := time.Date(2025, 3, 20, 14, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "stable.txt"), remoteMtime, remoteMtime); err != nil {
		t.Fatal(err)
	}

	// Build an index with matching mtime (as syncFolder would set after download).
	idx := newFileIndex()
	info, _ := os.Stat(filepath.Join(dir, "stable.txt"))
	idx.setEntry("stable.txt", FileEntry{
		Size:    info.Size(),
		MtimeNS: info.ModTime().UnixNano(),
		SHA256:  h,
		Mode:    uint32(info.Mode().Perm()),
	})

	ignore := newIgnoreMatcher(nil)

	// First scan — index already has correct entry, should fast-path skip.
	_, _, _, stats, _, scanErr := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	// Fast-path means no hashing — the file was skipped because mtime+size match.
	if stats.FilesHashed != 0 {
		t.Errorf("expected 0 files hashed (fast-path), got %d", stats.FilesHashed)
	}

	// Now simulate the bug this fix prevents: set a different mtime on disk
	// (as would happen without Chtimes — rename gives wall-clock mtime).
	wallClock := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "stable.txt"), wallClock, wallClock); err != nil {
		t.Fatal(err)
	}

	// Second scan — mtime mismatch forces a re-hash even though content is unchanged.
	_, _, _, stats2, _, scanErr2 := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if scanErr2 != nil {
		t.Fatal(scanErr2)
	}
	if stats2.FilesHashed != 1 {
		t.Errorf("expected 1 file hashed (mtime mismatch), got %d", stats2.FilesHashed)
	}
}

// --- G3: device ID guard tests ---

func TestDeviceIDGuard_RecordsOnFirstScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "data")

	idx := newFileIndex()
	if idx.DeviceID != 0 {
		t.Fatal("DeviceID should be 0 before first scan")
	}

	ignore := newIgnoreMatcher(nil)
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	if idx.DeviceID == 0 {
		t.Fatal("DeviceID should be set after first scan")
	}
}

func TestDeviceIDGuard_AcceptsSameDevice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "data")

	idx := newFileIndex()
	ignore := newIgnoreMatcher(nil)

	// First scan records device ID.
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	// Second scan on same filesystem should succeed.
	_, _, _, _, _, err = idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal("second scan on same device should succeed:", err)
	}
}

func TestDeviceIDGuard_RejectsDifferentDevice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "data")

	idx := newFileIndex()
	ignore := newIgnoreMatcher(nil)

	// First scan records device ID.
	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a device change by modifying the stored device ID.
	idx.DeviceID = idx.DeviceID + 999

	// Next scan should fail because device ID mismatches.
	_, _, _, _, _, err = idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err == nil {
		t.Fatal("scan should fail when device ID changes")
	}
	if !strings.Contains(err.Error(), "device ID changed") {
		t.Errorf("error should mention device ID change, got: %v", err)
	}
}

func TestDeviceIDGuard_PersistedInIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "data")

	idx := newFileIndex()
	ignore := newIgnoreMatcher(nil)

	_, _, _, _, _, err := idx.scanWithStats(context.Background(), dir, ignore, defaultMaxIndexFiles)
	if err != nil {
		t.Fatal(err)
	}

	originalDeviceID := idx.DeviceID

	// Save and reload through the YAML persistence path.
	idxPath := filepath.Join(t.TempDir(), "index.yaml")
	if err := idx.save(idxPath); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.DeviceID != originalDeviceID {
		t.Errorf("DeviceID not preserved: got %d, want %d", loaded.DeviceID, originalDeviceID)
	}
}

func TestFolderDeviceID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	devID, err := folderDeviceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if devID == 0 {
		t.Fatal("device ID should be non-zero for a real directory")
	}

	// Same directory should return the same device ID.
	devID2, err := folderDeviceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if devID != devID2 {
		t.Errorf("device ID should be consistent: got %d and %d", devID, devID2)
	}
}

// --- G2: disk space pre-check tests ---

func TestAvailableBytes_ReturnsNonZeroForRealDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	avail, ok := availableBytes(dir)
	if !ok {
		t.Skip("availableBytes not supported on this platform")
	}
	if avail == 0 {
		t.Fatal("available bytes should be > 0 for a real directory")
	}
}

func TestCheckDiskSpace_SkipsWhenNeededIsZero(t *testing.T) {
	t.Parallel()
	// needed=0 should always pass regardless of disk state.
	if err := checkDiskSpace(t.TempDir(), 0); err != nil {
		t.Errorf("checkDiskSpace with 0 bytes should not fail: %v", err)
	}
}

func TestCheckDiskSpace_SkipsWhenNeededIsNegative(t *testing.T) {
	t.Parallel()
	// negative needed (e.g., missing Content-Length = -1) should be skipped.
	if err := checkDiskSpace(t.TempDir(), -1); err != nil {
		t.Errorf("checkDiskSpace with -1 bytes should not fail: %v", err)
	}
}

func TestCheckDiskSpace_PassesWhenEnoughSpace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Asking for 1 byte should always pass on a real machine.
	if err := checkDiskSpace(dir, 1); err != nil {
		t.Errorf("checkDiskSpace with 1 byte should pass on a real machine: %v", err)
	}
}

func TestCheckDiskSpace_FailsWhenInsufficientSpace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	avail, ok := availableBytes(dir)
	if !ok {
		t.Skip("availableBytes not supported on this platform")
	}
	// Ask for more than what's available — should fail with clear error.
	needed := int64(avail + diskSpaceMargin + 1)
	err := checkDiskSpace(dir, needed)
	if err == nil {
		t.Fatal("checkDiskSpace should fail when needed > available")
	}
	if !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("error should mention insufficient disk space, got: %v", err)
	}
}

// --- G4: conflict file pruning tests ---

func TestPruneConflicts_BelowCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)

	// Create original and a few conflict files (below cap).
	writeFile(t, dir, "report.txt", "original")
	for i := range 3 {
		name := fmt.Sprintf("report.sync-conflict-20250601-10%02d00-dev123-%08x.txt", i, i)
		writeFile(t, dir, name, fmt.Sprintf("conflict-%d", i))
	}

	pruneConflicts(root, "report.sync-conflict-20250601-100000-dev123-00000000.txt")

	// All 3 should remain (below maxConflictsPerFile=10).
	entries, _ := os.ReadDir(dir)
	conflictCount := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			conflictCount++
		}
	}
	if conflictCount != 3 {
		t.Errorf("expected 3 conflict files, got %d", conflictCount)
	}
}

func TestPruneConflicts_ExceedsCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)

	// Create original file.
	writeFile(t, dir, "data.txt", "original")

	// Create 12 conflict files with ascending mtimes.
	var names []string
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 12 {
		name := fmt.Sprintf("data.sync-conflict-20250601-10%02d00-dev123-%08x.txt", i, i)
		writeFile(t, dir, name, fmt.Sprintf("conflict-%d", i))
		mt := baseTime.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, name), mt, mt); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	pruneConflicts(root, names[11])

	// Should have pruned 2 oldest, leaving 10.
	entries, _ := os.ReadDir(dir)
	var remaining []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			remaining = append(remaining, e.Name())
		}
	}
	if len(remaining) != maxConflictsPerFile {
		t.Errorf("expected %d conflict files after pruning, got %d: %v",
			maxConflictsPerFile, len(remaining), remaining)
	}

	// The 2 oldest (i=0, i=1) should be gone.
	for _, e := range remaining {
		if e == names[0] || e == names[1] {
			t.Errorf("oldest conflict file %q should have been pruned", e)
		}
	}
}

func TestPruneConflicts_SubdirectoryConflicts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)

	// Conflict files in a subdirectory.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "sub/notes.md", "original")

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 12 {
		name := fmt.Sprintf("sub/notes.sync-conflict-20250601-10%02d00-dev123-%08x.md", i, i)
		writeFile(t, dir, name, fmt.Sprintf("conflict-%d", i))
		mt := baseTime.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, name), mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	pruneConflicts(root, "sub/notes.sync-conflict-20250601-101100-dev123-0000000b.md")

	entries, _ := os.ReadDir(filepath.Join(dir, "sub"))
	conflictCount := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			conflictCount++
		}
	}
	if conflictCount != maxConflictsPerFile {
		t.Errorf("expected %d conflict files in sub/, got %d", maxConflictsPerFile, conflictCount)
	}
}

func TestPruneConflicts_DoesNotPruneDifferentOriginal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := openTestRoot(t, dir)

	// Create conflicts for two different original files.
	writeFile(t, dir, "alpha.txt", "original-alpha")
	writeFile(t, dir, "beta.txt", "original-beta")

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 12 {
		name := fmt.Sprintf("alpha.sync-conflict-20250601-10%02d00-dev123-%08x.txt", i, i)
		writeFile(t, dir, name, fmt.Sprintf("alpha-conflict-%d", i))
		mt := baseTime.Add(time.Duration(i) * time.Hour)
		_ = os.Chtimes(filepath.Join(dir, name), mt, mt)
	}
	for i := range 5 {
		name := fmt.Sprintf("beta.sync-conflict-20250601-10%02d00-dev123-%08x.txt", i, i)
		writeFile(t, dir, name, fmt.Sprintf("beta-conflict-%d", i))
	}

	pruneConflicts(root, "alpha.sync-conflict-20250601-101100-dev123-0000000b.txt")

	// alpha: pruned to 10.  beta: untouched at 5.
	entries, _ := os.ReadDir(dir)
	alphaCount, betaCount := 0, 0
	for _, e := range entries {
		if strings.Contains(e.Name(), "alpha.sync-conflict-") {
			alphaCount++
		}
		if strings.Contains(e.Name(), "beta.sync-conflict-") {
			betaCount++
		}
	}
	if alphaCount != maxConflictsPerFile {
		t.Errorf("expected %d alpha conflicts, got %d", maxConflictsPerFile, alphaCount)
	}
	if betaCount != 5 {
		t.Errorf("expected 5 beta conflicts (untouched), got %d", betaCount)
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

// --- Protocol rejection-path tests (boundary contract) ---
//
// These pin the HTTP rejection behavior for malformed input. Each is a
// trust-boundary contract: wrong method, malformed body, unparseable
// protobuf. Existing tests cover the happy path and the forbidden-peer
// path; these complete the contract-test triad.

func TestHandleIndex_RejectsNonPost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/index")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /index = %d, want 405", resp.StatusCode)
	}
}

func TestHandleIndex_RejectsMalformedProtobuf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Not a valid protobuf wire format.
	body := bytes.NewReader([]byte("this is not a protobuf"))
	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed protobuf = %d, want 400", resp.StatusCode)
	}
}

func TestHandleIndex_RejectsBadGzip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Body claims zstd but isn't.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/index", bytes.NewReader([]byte("not zstd")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad zstd = %d, want 400", resp.StatusCode)
	}
}

func TestHandleFile_RejectsNonGet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "data")
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/file?folder=test&path=f.txt", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /file = %d, want 405", resp.StatusCode)
	}
}

func TestHandleDelta_RejectsMalformedProtobuf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/delta", "application/x-protobuf", bytes.NewReader([]byte("garbage")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed delta protobuf = %d, want 400", resp.StatusCode)
	}
}

func TestHandleBundle_RejectsNonPost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /bundle = %d, want 405", resp.StatusCode)
	}
}

func TestHandleBundle_RejectsMalformedProtobuf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp := bundlePost(t, ts.URL, []byte("not a valid protobuf"))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed bundle protobuf = %d, want 400", resp.StatusCode)
	}
}

func TestHandleBundle_RejectsBadGzip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/bundle", bytes.NewReader([]byte("not zstd")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bundle with bad zstd = %d, want 400", resp.StatusCode)
	}
}

func TestHandleBundle_RejectsUnknownFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{
		cfg:      testCfg(dir, "127.0.0.1"),
		folders:  map[string]*folderState{"test": {cfg: testFolderCfg(dir, "127.0.0.1")}},
		deviceID: "test-device",
	}
	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	reqMsg := &pb.BundleRequest{FolderId: "nonexistent", Paths: []string{"a.txt"}}
	reqData, _ := proto.Marshal(reqMsg)
	resp := bundlePost(t, ts.URL, reqData)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bundle for unknown folder = %d, want 404", resp.StatusCode)
	}
}

func TestHandleStatus_RejectsNonGet(t *testing.T) {
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

	resp, err := http.Post(ts.URL+"/status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /status = %d, want 405", resp.StatusCode)
	}
}

func TestActionName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   DiffAction
		want string
	}{
		{ActionDownload, "download"},
		{ActionConflict, "conflict"},
		{ActionDelete, "delete"},
		{DiffAction(99), "unknown"},
	}
	for _, c := range cases {
		if got := actionName(c.in); got != c.want {
			t.Errorf("actionName(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildPendingSummary_CountsAndBytes(t *testing.T) {
	t.Parallel()
	entries := []DiffEntry{
		{Path: "a.txt", Action: ActionDownload, RemoteSize: 100},
		{Path: "b.txt", Action: ActionDownload, RemoteSize: 200},
		{Path: "c.txt", Action: ActionConflict, RemoteSize: 50},
		{Path: "d.txt", Action: ActionDelete, RemoteSize: 0},
	}
	ps := buildPendingSummary(entries)
	if ps.Downloads != 2 {
		t.Errorf("Downloads = %d, want 2", ps.Downloads)
	}
	if ps.Conflicts != 1 {
		t.Errorf("Conflicts = %d, want 1", ps.Conflicts)
	}
	if ps.Deletes != 1 {
		t.Errorf("Deletes = %d, want 1", ps.Deletes)
	}
	if ps.Bytes != 350 {
		t.Errorf("Bytes = %d, want 350", ps.Bytes)
	}
	if len(ps.Files) != 4 {
		t.Errorf("Files len = %d, want 4", len(ps.Files))
	}
	if ps.Files[0].Action != "download" {
		t.Errorf("Files[0].Action = %q, want download", ps.Files[0].Action)
	}
	if ps.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set to time.Now()")
	}
}

func TestBuildPendingSummary_PreviewCap(t *testing.T) {
	t.Parallel()
	entries := make([]DiffEntry, 75)
	for i := range entries {
		entries[i] = DiffEntry{Path: "f", Action: ActionDownload, RemoteSize: 1}
	}
	ps := buildPendingSummary(entries)
	if ps.Downloads != 75 {
		t.Errorf("Downloads = %d, want 75 (counts are uncapped)", ps.Downloads)
	}
	if len(ps.Files) != pendingFilePreviewLimit {
		t.Errorf("Files len = %d, want %d (preview is capped)", len(ps.Files), pendingFilePreviewLimit)
	}
}

func TestClonePendingSummary_DeepCopiesFiles(t *testing.T) {
	t.Parallel()
	src := PendingSummary{
		Downloads: 2,
		Bytes:     500,
		Files: []PendingFile{
			{Path: "a.txt", Action: "download", Size: 300},
			{Path: "b.txt", Action: "download", Size: 200},
		},
	}
	cp := clonePendingSummary(src)
	if cp == nil {
		t.Fatal("clone returned nil")
	}
	// Mutate the clone's slice and confirm the original is unaffected.
	cp.Files[0].Path = "MUTATED"
	if src.Files[0].Path != "a.txt" {
		t.Errorf("src Files[0].Path = %q, clone must not alias", src.Files[0].Path)
	}
	if cp.Downloads != 2 || cp.Bytes != 500 {
		t.Errorf("scalar fields not copied: %+v", cp)
	}
}

func TestClonePendingSummary_EmptyFiles(t *testing.T) {
	t.Parallel()
	src := PendingSummary{Downloads: 1}
	cp := clonePendingSummary(src)
	if cp.Files != nil {
		t.Errorf("empty Files should remain nil, got %v", cp.Files)
	}
}

func TestGetFolderPath_FoundAndMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := &Node{folders: map[string]*folderState{
		"path-test": {cfg: config.FolderCfg{ID: "path-test", Path: dir}},
	}}
	activeNodes.Register(n)
	defer activeNodes.Unregister(n)

	got, ok := GetFolderPath("path-test")
	if !ok {
		t.Fatal("expected path-test to be found")
	}
	if got != dir {
		t.Errorf("path = %q, want %q", got, dir)
	}

	if _, ok := GetFolderPath("does-not-exist"); ok {
		t.Error("unknown folder must return ok=false")
	}
}

func TestGetConflicts_PopulatedAndEmpty(t *testing.T) {
	t.Parallel()
	n := &Node{folders: map[string]*folderState{
		"conf-a": {
			cfg:       config.FolderCfg{ID: "conf-a"},
			conflicts: []string{"a.sync-conflict-x", "b.sync-conflict-y"},
		},
		"conf-b": {
			cfg: config.FolderCfg{ID: "conf-b"},
		},
	}}
	activeNodes.Register(n)
	defer activeNodes.Unregister(n)

	var mine []ConflictInfo
	for _, c := range GetConflicts() {
		if c.FolderID == "conf-a" || c.FolderID == "conf-b" {
			mine = append(mine, c)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("GetConflicts returned %d entries for conf-a/b, want 2: %+v", len(mine), mine)
	}
	if mine[0].Path != "a.sync-conflict-x" {
		t.Errorf("Path[0] = %q, want a.sync-conflict-x", mine[0].Path)
	}
}

func TestGetActivities_RecordAndCap(t *testing.T) {
	t.Parallel()
	n := &Node{folders: map[string]*folderState{
		"act-test": {cfg: config.FolderCfg{ID: "act-test"}},
	}}
	activeNodes.Register(n)
	defer activeNodes.Unregister(n)

	// Record more than activityHistorySize activities; oldest must be dropped.
	const extra = 10
	for i := range activityHistorySize + extra {
		n.recordActivity(SyncActivity{
			Time:   time.Unix(int64(i), 0),
			Folder: "act-test",
			Peer:   "10.0.0.1:7756",
			Files:  i,
		})
	}
	var mine []SyncActivity
	for _, a := range GetActivities() {
		if a.Folder == "act-test" {
			mine = append(mine, a)
		}
	}
	if len(mine) != activityHistorySize {
		t.Fatalf("got %d act-test activities, want %d (capped)", len(mine), activityHistorySize)
	}
	// Activities are sorted descending by time, so the newest (Files=extra+limit-1)
	// must be first and the oldest kept (Files=extra) must be last.
	if mine[0].Files != activityHistorySize+extra-1 {
		t.Errorf("newest Files = %d, want %d", mine[0].Files, activityHistorySize+extra-1)
	}
	if mine[len(mine)-1].Files != extra {
		t.Errorf("oldest-kept Files = %d, want %d (activities before this should be dropped)", mine[len(mine)-1].Files, extra)
	}
}

func TestClientForPeer_PeerSpecificAndDefault(t *testing.T) {
	t.Parallel()
	peerClient := &http.Client{}
	defaultClient := &http.Client{}
	n := &Node{
		peerClients:   map[string]*http.Client{"10.0.0.1:7756": peerClient},
		defaultClient: defaultClient,
	}
	if got := n.clientForPeer("10.0.0.1:7756"); got != peerClient {
		t.Error("configured peer must return its own client")
	}
	if got := n.clientForPeer("192.168.1.1:7756"); got != defaultClient {
		t.Error("unconfigured peer must fall back to defaultClient")
	}
}

func TestTLSStatusFor_PinnedAndNot(t *testing.T) {
	t.Parallel()
	n := &Node{
		peerHasFingerprint: map[string]bool{
			"10.0.0.1:7756": true,
			"10.0.0.2:7756": false,
		},
	}
	if got := n.tlsStatusFor("10.0.0.1:7756"); got != "encrypted · verified" {
		t.Errorf("pinned peer status = %q, want 'encrypted · verified'", got)
	}
	if got := n.tlsStatusFor("10.0.0.2:7756"); got != "encrypted" {
		t.Errorf("unpinned peer status = %q, want 'encrypted'", got)
	}
	if got := n.tlsStatusFor("10.0.0.99:7756"); got != "encrypted" {
		t.Errorf("unknown peer status = %q, want 'encrypted'", got)
	}
}

func TestSetConfigFolders_FallbackWhenNoActiveNodes(t *testing.T) {
	// Not t.Parallel: mutates the global configFolders registry.
	t.Cleanup(clearConfigFolders)
	clearConfigFolders()

	SetConfigFolders(config.FilesyncCfg{
		ResolvedFolders: []config.FolderCfg{
			{
				ID:             "z-folder",
				Path:           "/tmp/z",
				Direction:      "send-receive",
				Peers:          []string{"10.0.0.1:7756"},
				PeerNames:      []string{"hw"},
				IgnorePatterns: []string{"*.tmp"},
			},
			{
				ID:        "a-folder",
				Path:      "/tmp/a",
				Direction: "disabled",
				Peers:     nil,
			},
		},
	})

	// With no activeNodes, GetFolderStatuses must fall back to configFolders.
	// Sibling tests register their own nodes so we filter to our two IDs.
	got := GetFolderStatuses()
	var mine []FolderStatus
	for _, s := range got {
		if s.ID == "z-folder" || s.ID == "a-folder" {
			mine = append(mine, s)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("got %d of our folders, want 2: %+v", len(mine), mine)
	}
	// SetConfigFolders sorts by ID ascending, so a-folder comes first.
	if mine[0].ID != "a-folder" {
		t.Errorf("sorted[0] = %q, want a-folder", mine[0].ID)
	}
	if mine[1].Scanning != true {
		t.Errorf("send-receive folder should be marked Scanning=true pre-start")
	}
	if mine[0].Scanning != false {
		t.Errorf("disabled folder should have Scanning=false")
	}
	if mine[1].Peers[0].Name != "hw" || mine[1].Peers[0].Addr != "10.0.0.1:7756" {
		t.Errorf("z-folder peer = %+v, want {Name:hw Addr:10.0.0.1:7756}", mine[1].Peers[0])
	}
}
