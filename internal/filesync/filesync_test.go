package filesync

import (
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
	_, err := downloadFile(client, "127.0.0.1:9999", "test", "../../../etc/passwd", "abcdef0123456789abcdef0123456789", t.TempDir())
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestDownloadFile_ShortHash(t *testing.T) {
	client := &http.Client{}
	_, err := downloadFile(client, "127.0.0.1:9999", "test", "file.txt", "abc", t.TempDir())
	if err == nil {
		t.Fatal("expected error for short hash")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("unexpected error: %v", err)
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
		cfg:           testCfg(dir, "127.0.0.1"),
		folders:       make(map[string]*folderState),
		remoteIndices: make(map[string]map[string]*pb.IndexExchange),
		deviceID:      "test-device",
	}
	idx := newFileIndex()
	idx.Sequence = 5
	idx.Files["local.txt"] = FileEntry{Size: 100, SHA256: "abc123", Sequence: 5}
	n.folders["test"] = &folderState{
		cfg:   testFolderCfg(dir, "127.0.0.1"),
		index: idx,
		peers: make(map[string]PeerState),
	}
	n.remoteIndices["test"] = make(map[string]*pb.IndexExchange)

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

	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", byteReader(data))
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

func TestHandleIndex_RejectsUnknownPeer(t *testing.T) {
	dir := t.TempDir()

	n := &Node{
		cfg:      testCfg(dir, "10.99.99.99"), // peer that won't match localhost
		folders:  make(map[string]*folderState),
		deviceID: "test-device",
	}
	n.folders["test"] = &folderState{
		cfg: testFolderCfg(dir, "10.99.99.99"),
	}

	srv := &server{node: n}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req := &pb.IndexExchange{FolderId: "test"}
	data, _ := proto.Marshal(req)

	resp, err := http.Post(ts.URL+"/index", "application/x-protobuf", byteReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
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

func byteReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

type bytesReader struct {
	data []byte
	pos  int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	if r.pos >= len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	buf := make([]byte, 1024*1024)
	var total int
	for {
		n, err := resp.Body.Read(buf[total:])
		total += n
		if err != nil {
			break
		}
	}
	return buf[:total]
}

func testCfg(dir, peerIP string) config.FilesyncCfg {
	return config.FilesyncCfg{
		Bind:          "0.0.0.0:0",
		MaxConcurrent: 4,
		ScanInterval:  "60s",
		Folders: []config.FolderCfg{
			testFolderCfg(dir, peerIP),
		},
	}
}

func testFolderCfg(dir, peerIP string) config.FolderCfg {
	return config.FolderCfg{
		ID:        "test",
		Path:      dir,
		Direction: "send-receive",
		Peers:     []string{peerIP + ":7756"},
	}
}
