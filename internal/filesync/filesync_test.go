package filesync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

// --- Ignore pattern tests ---

func TestParseLine(t *testing.T) {
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
			got := m.shouldIgnore(tt.path, tt.isDir)
			if got != tt.ignore {
				t.Errorf("shouldIgnore(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.ignore)
			}
		})
	}
}

func TestMatchPattern(t *testing.T) {
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
			if got := isConflictFile(tt.name); got != tt.want {
				t.Errorf("isConflictFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// --- Index tests ---

func TestScanAndPersist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "sub/b.txt", "world")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	changed, err := idx.scan(dir, ignore)
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
	changed, err = idx.scan(dir, ignore)
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
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _ = idx.scan(dir, ignore)

	// Delete b.txt
	_ = os.Remove(filepath.Join(dir, "b.txt"))

	changed, _ := idx.scan(dir, ignore)
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
	dir := t.TempDir()
	writeFile(t, dir, "old.txt", "data")

	// Backdate the file to 60 days ago.
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "old.txt"), oldTime, oldTime)

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _ = idx.scan(dir, ignore)

	// Verify the indexed mtime reflects the backdated time.
	entry := idx.Files["old.txt"]
	if entry.MtimeNS > time.Now().Add(-59*24*time.Hour).UnixNano() {
		t.Fatal("pre-condition: file mtime should be ~60 days ago")
	}

	// Delete the file and re-scan.
	_ = os.Remove(filepath.Join(dir, "old.txt"))
	_, _ = idx.scan(dir, ignore)

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
	idx.purgeTombstones(30 * 24 * time.Hour)
	if _, ok := idx.Files["old.txt"]; !ok {
		t.Error("fresh tombstone should survive purge")
	}
}

func TestScanRespectsIgnore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "keep")
	writeFile(t, dir, "skip.log", "skip")

	idx := newFileIndex()
	ignore := &ignoreMatcher{
		patterns: []ignorePattern{{pattern: "*.log"}},
	}

	_, _ = idx.scan(dir, ignore)

	if _, ok := idx.Files["keep.txt"]; !ok {
		t.Error("keep.txt should be indexed")
	}
	if _, ok := idx.Files["skip.log"]; ok {
		t.Error("skip.log should be ignored")
	}
}

func TestDiff(t *testing.T) {
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
	local := newFileIndex()
	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	actions := local.diff(remote, 0, "receive-only")
	if len(actions) != 1 || actions[0].Action != ActionDownload {
		t.Error("receive-only should allow downloads")
	}
}

func TestDiffSendOnly(t *testing.T) {
	local := newFileIndex()
	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	actions := local.diff(remote, 0, "send-only")
	if len(actions) != 0 {
		t.Error("send-only should produce no actions (no receiving)")
	}
}

func TestDiffDeleteTombstone(t *testing.T) {
	local := newFileIndex()
	local.Files["a.txt"] = FileEntry{SHA256: "aaa", Sequence: 1}

	remote := newFileIndex()
	remote.Files["a.txt"] = FileEntry{Deleted: true, Sequence: 5}

	actions := local.diff(remote, 0, "send-receive")
	if len(actions) != 1 || actions[0].Action != ActionDelete {
		t.Errorf("expected delete action, got %v", actions)
	}
}

func TestPurgeTombstones(t *testing.T) {
	idx := newFileIndex()
	// Old tombstone (mtime = 0 means epoch, well past 30 days ago).
	idx.Files["old.txt"] = FileEntry{Deleted: true, MtimeNS: 0}
	// Recent tombstone.
	idx.Files["recent.txt"] = FileEntry{Deleted: true, MtimeNS: time.Now().UnixNano()}

	idx.purgeTombstones(30 * 24 * time.Hour)

	if _, ok := idx.Files["old.txt"]; ok {
		t.Error("old tombstone should have been purged")
	}
	if _, ok := idx.Files["recent.txt"]; !ok {
		t.Error("recent tombstone should be kept")
	}
}

func TestCleanTempFiles(t *testing.T) {
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
	result := conflictFileName("docs/report.docx", "abc123def")
	if !isConflictFile(result) {
		t.Errorf("expected conflict pattern, got %q", result)
	}
	if filepath.Dir(result) != "docs" {
		t.Errorf("expected dir 'docs', got %q", filepath.Dir(result))
	}
}

func TestResolveConflict_RemoteWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "local content")

	localMtime := time.Now().Add(-1 * time.Hour).UnixNano()
	remoteMtime := time.Now().UnixNano()

	winner, err := resolveConflict(dir, "file.txt", localMtime, remoteMtime, "remote123")
	if err != nil {
		t.Fatal(err)
	}
	if winner != "remote" {
		t.Errorf("expected remote to win, got %q", winner)
	}

	// Original file should be renamed to conflict.
	if _, err := os.Stat(filepath.Join(dir, "file.txt")); !os.IsNotExist(err) {
		t.Error("original file should have been renamed")
	}

	// A conflict file should exist.
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if isConflictFile(e.Name()) {
			found = true
		}
	}
	if !found {
		t.Error("no conflict file found")
	}
}

func TestResolveConflict_LocalWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "local content")

	localMtime := time.Now().UnixNano()
	remoteMtime := time.Now().Add(-1 * time.Hour).UnixNano()

	winner, err := resolveConflict(dir, "file.txt", localMtime, remoteMtime, "remote123")
	if err != nil {
		t.Fatal(err)
	}
	if winner != "local" {
		t.Errorf("expected local to win, got %q", winner)
	}

	// Original should still exist.
	if _, err := os.Stat(filepath.Join(dir, "file.txt")); err != nil {
		t.Error("original file should still exist")
	}
}

// --- Transfer tests ---

func TestDownloadFile_PathTraversal(t *testing.T) {
	client := &http.Client{}
	_, err := downloadFile(t.Context(), client, "127.0.0.1:9999", "test", "../../../etc/passwd", "abcdef0123456789abcdef0123456789", t.TempDir(), nil)
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestDownloadFile_ShortHash(t *testing.T) {
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
	dir := t.TempDir()
	// Server has a file with a changed middle block.
	writeFile(t, dir, "data.bin", "AAAAXXXXcc")

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
	writeFile(t, clientDir, "data.bin", "AAAABBBBcc")
	localHashes, _ := computeBlockSignatures(filepath.Join(clientDir, "data.bin"), 4)

	req := &pb.BlockSignatures{
		FolderId:    "test",
		Path:        "data.bin",
		BlockSize:   4,
		FileSize:    10,
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
	if string(deltaResp.GetBlocks()[0].GetData()) != "XXXX" {
		t.Errorf("delta data = %q, want 'XXXX'", deltaResp.GetBlocks()[0].GetData())
	}
}

func TestSafePath(t *testing.T) {
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
		{"absolute path", "/etc/passwd", true},
		{"null byte", "file\x00.txt", true},
		{"empty path", "", false}, // resolves to root itself, which is allowed
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := safePath(root, tt.relPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("safePath(%q) error=%v, wantErr=%v", tt.relPath, err, tt.wantErr)
			}
		})
	}
}

func TestDeleteFile(t *testing.T) {
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
	err := deleteFile(t.TempDir(), "../escape.txt")
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

// --- Protocol tests ---

func TestHandleFile_ServesContent(t *testing.T) {
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
	// Non-loopback peers that don't match any configured peer are rejected.
	n := &Node{
		folders: map[string]*folderState{
			"docs": {cfg: config.FolderCfg{Peers: []string{"10.1.1.1:7756"}}},
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
	dir := t.TempDir()
	ignore := &ignoreMatcher{}

	fw, err := newFolderWatcher([]string{dir}, map[string]*ignoreMatcher{dir: ignore})
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

// --- End-to-end sync test (FT1) ---

func TestTwoNodeSync(t *testing.T) {
	// Set up two folders with a file on each side.
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeFile(t, dirA, "from-a.txt", "content from A")

	// Node A: scan to build index.
	idxA := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _ = idxA.scan(dirA, ignore)

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
	dir := t.TempDir()
	writeFile(t, dir, "local.txt", "local content")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _ = idx.scan(dir, ignore)

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

	n.runScan()

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

// --- listConflicts test (FT6) ---

func TestListConflicts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "normal.txt", "ok")
	writeFile(t, dir, "report.sync-conflict-20260406-143022-abc123.docx", "conflict1")
	writeFile(t, dir, "sub/data.sync-conflict-20260101-000000-def456.csv", "conflict2")

	conflicts, err := listConflicts(dir)
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
}

// --- persistFolder roundtrip test (FT8) ---

func TestPersistFolder_Roundtrip(t *testing.T) {
	dataDir := t.TempDir()
	folderDir := t.TempDir()
	writeFile(t, folderDir, "a.txt", "hello")

	idx := newFileIndex()
	ignore := &ignoreMatcher{}
	_, _ = idx.scan(folderDir, ignore)

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
	dataDir := t.TempDir()
	oldDir := t.TempDir()
	newDir := t.TempDir()
	writeFile(t, oldDir, "file.txt", "content")

	// Build an index at the old path and persist it.
	idx := newFileIndex()
	idx.Path = oldDir
	ignore := &ignoreMatcher{}
	_, _ = idx.scan(oldDir, ignore)

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
	dataDir := t.TempDir()
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "content")

	idx := newFileIndex()
	idx.Path = dir
	ignore := &ignoreMatcher{}
	_, _ = idx.scan(dir, ignore)

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
