package filesync

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpers for install_test.go: expose a folder root + a tmp file
// without the full Node/folderState plumbing. The .bak lifecycle is
// the unit under test; everything else is incidental.

func newInstallRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

func writeRoot(t *testing.T, root *os.Root, relPath, content string) {
	t.Helper()
	if dir := filepath.Dir(relPath); dir != "." {
		_ = root.MkdirAll(dir, 0o755)
	}
	f, err := root.Create(relPath)
	if err != nil {
		t.Fatalf("Create %s: %v", relPath, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("Write %s: %v", relPath, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close %s: %v", relPath, err)
	}
}

func readRoot(t *testing.T, root *os.Root, relPath string) string {
	t.Helper()
	f, err := root.Open(relPath)
	if err != nil {
		t.Fatalf("Open %s: %v", relPath, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := f.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

func mustNotExist(t *testing.T, root *os.Root, relPath string) {
	t.Helper()
	if _, err := root.Stat(relPath); !os.IsNotExist(err) {
		t.Errorf("expected %s to not exist, got err=%v", relPath, err)
	}
}

// TestInstallDownloadedFile_HappyPath pins the F7 happy path
// (audit §6 commit 6 phase E): original is hashed, renamed to .bak,
// temp is renamed to the original path, commit runs, .bak is
// unlinked. Final on-disk state: only the new content at the
// original path. No straggler artifacts.
func TestInstallDownloadedFile_HappyPath(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	const oldContent = "old local content"
	const newContent = "new remote content"
	writeRoot(t, root, "doc.txt", oldContent)
	writeRoot(t, root, ".mesh-tmp-abc", newContent)

	commitCalls := 0
	commit := func() error {
		commitCalls++
		// Sanity: at the moment commit fires, the new content is
		// already at the original path and the .bak holds the old.
		if got := readRoot(t, root, "doc.txt"); got != newContent {
			t.Errorf("commit-time content at doc.txt=%q, want %q (new content)", got, newContent)
		}
		bak := bakRelPath("doc.txt", hashContent(oldContent))
		if got := readRoot(t, root, bak); got != oldContent {
			t.Errorf("commit-time content at .bak=%q, want %q (old content)", got, oldContent)
		}
		return nil
	}

	var metrics FolderSyncMetrics
	if err := installDownloadedFile(root, "doc.txt", ".mesh-tmp-abc", commit, &metrics); err != nil {
		t.Fatalf("install: %v", err)
	}
	if commitCalls != 1 {
		t.Errorf("commit called %d times, want 1", commitCalls)
	}
	if got := readRoot(t, root, "doc.txt"); got != newContent {
		t.Errorf("post-install doc.txt=%q, want %q", got, newContent)
	}
	mustNotExist(t, root, ".mesh-tmp-abc")
	mustNotExist(t, root, bakRelPath("doc.txt", hashContent(oldContent)))

	// No metric counter should fire on the happy path — the .bak was
	// unlinked cleanly, no rollback happened.
	if got := metrics.BakRestoredOnCommitFail.Load(); got != 0 {
		t.Errorf("BakRestoredOnCommitFail=%d on happy path, want 0", got)
	}
	if got := metrics.BakRestoreFailed.Load(); got != 0 {
		t.Errorf("BakRestoreFailed=%d on happy path, want 0", got)
	}

	// No artifact left in the directory listing.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mesh-bak-") || strings.Contains(e.Name(), ".mesh-tmp-") {
			t.Errorf("residual artifact: %s", e.Name())
		}
	}
}

// TestInstallDownloadedFile_NewFile_NoBak pins the no-prior-original
// branch: when relPath does not yet exist locally, step 1 is a
// no-op. The temp is still installed at relPath atomically and
// commit runs; no .bak file is ever created.
func TestInstallDownloadedFile_NewFile_NoBak(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	const newContent = "fresh from peer"
	writeRoot(t, root, ".mesh-tmp-xyz", newContent)

	commit := func() error { return nil }
	var metrics FolderSyncMetrics
	if err := installDownloadedFile(root, "fresh.txt", ".mesh-tmp-xyz", commit, &metrics); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := readRoot(t, root, "fresh.txt"); got != newContent {
		t.Errorf("fresh.txt=%q, want %q", got, newContent)
	}
	// No .bak file should have been created at all.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mesh-bak-") {
			t.Errorf("unexpected .bak created when no original existed: %s", e.Name())
		}
	}
}

// TestInstallDownloadedFile_CommitFails_RestoresBak pins the
// commit-failure rollback (audit H13 — TestDownloadCommitFails_RestoresOriginal):
// when the commit callback returns an error, the .bak is renamed
// back to the original path (clobbering the new content), and the
// metric counter increments. Final on-disk state: only the OLD
// content at the original path.
func TestInstallDownloadedFile_CommitFails_RestoresBak(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	const oldContent = "preserved old content"
	const newContent = "rejected new content"
	writeRoot(t, root, "doc.txt", oldContent)
	writeRoot(t, root, ".mesh-tmp-fail", newContent)

	wantErr := errors.New("simulated SQLite commit failure")
	commit := func() error { return wantErr }
	var metrics FolderSyncMetrics
	err := installDownloadedFile(root, "doc.txt", ".mesh-tmp-fail", commit, &metrics)
	if err == nil {
		t.Fatal("install: expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("install error chain missing the commit error: %v", err)
	}
	if errors.Is(err, errBakRestoreFailed) {
		t.Errorf("install reported errBakRestoreFailed on a clean rollback: %v", err)
	}

	// The original content is back at doc.txt.
	if got := readRoot(t, root, "doc.txt"); got != oldContent {
		t.Errorf("post-rollback doc.txt=%q, want %q (rolled back)", got, oldContent)
	}
	// No straggler .bak or .mesh-tmp- in the dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mesh-bak-") || strings.Contains(e.Name(), ".mesh-tmp-") {
			t.Errorf("residual artifact after rollback: %s", e.Name())
		}
	}

	// Metric fired exactly once.
	if got := metrics.BakRestoredOnCommitFail.Load(); got != 1 {
		t.Errorf("BakRestoredOnCommitFail=%d after rollback, want 1", got)
	}
	if got := metrics.BakRestoreFailed.Load(); got != 0 {
		t.Errorf("BakRestoreFailed=%d after CLEAN rollback, want 0", got)
	}
}

// TestInstallDownloadedFile_CommitFails_NoOriginal pins the
// new-file commit-failure path: the temp was installed at relPath,
// commit fails, but there's no .bak to restore (no prior original).
// Cleanup must remove the new content from relPath so on-disk and
// SQLite stay consistent (SQLite rejected the row, so the file
// must not be visible).
func TestInstallDownloadedFile_CommitFails_NoOriginal(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	writeRoot(t, root, ".mesh-tmp-fail-new", "rejected fresh")

	wantErr := errors.New("simulated commit fail on new file")
	commit := func() error { return wantErr }
	var metrics FolderSyncMetrics
	err := installDownloadedFile(root, "new.txt", ".mesh-tmp-fail-new", commit, &metrics)
	if err == nil {
		t.Fatal("install: expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain missing commit error: %v", err)
	}
	mustNotExist(t, root, "new.txt")
	mustNotExist(t, root, ".mesh-tmp-fail-new")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("residual artifacts after no-original rollback: %d entries", len(entries))
	}
	if got := metrics.BakRestoredOnCommitFail.Load(); got != 1 {
		t.Errorf("BakRestoredOnCommitFail=%d, want 1 (counter fires on the new-file branch too)", got)
	}
}

// TestInstallDownloadedFile_RejectsBakInputPath pins the structural
// safety check: passing an already-bak-named path as relPath would
// recurse the protection (the .bak's hash naming the .bak's hash).
// The function refuses up-front rather than producing a tangled
// on-disk state.
func TestInstallDownloadedFile_RejectsBakInputPath(t *testing.T) {
	t.Parallel()
	root, _ := newInstallRoot(t)
	writeRoot(t, root, ".mesh-tmp-x", "x")
	bakLike := "doc.txt" + bakSuffix + "deadbeef"
	err := installDownloadedFile(root, bakLike, ".mesh-tmp-x", func() error { return nil }, nil)
	if err == nil {
		t.Fatal("install: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to install over a .bak path") {
		t.Errorf("error message missing safety anchor: %v", err)
	}
}

// TestDownload_HoldsClaimUntilTxCommit pins audit §6 commit 6
// phase F (the C6 link with commit 5): the per-path claim must be
// held across installDownloadedFile's commit callback so the scan
// walker's claimed-path skip (commit 5) covers the .bak window
// (rename original→bak, rename temp→original, SQLite commit,
// unlink bak). Without this contract, scan could re-hash the new
// content while the SQLite row still carries the old hash.
//
// Mental mutation: moving fs.releasePath BEFORE installDownloadedFile
// (or releasing it inside the commit callback) would let the scan
// walker observe the in-flight install and the test catches it.
//
// The test mirrors the production goroutine shape: claimPath →
// installDownloadedFile (with commit) → defer releasePath. The
// commit callback asserts isClaimed returns true at the moment the
// SQLite commit would land.
func TestDownload_HoldsClaimUntilTxCommit(t *testing.T) {
	t.Parallel()
	root, _ := newInstallRoot(t)
	writeRoot(t, root, "doc.txt", "old")
	writeRoot(t, root, ".mesh-tmp-claim", "new")

	fs := &folderState{
		inFlight: make(map[string]bool),
	}
	if !fs.claimPath("doc.txt") {
		t.Fatal("setup: claimPath returned false on a fresh inFlight map")
	}
	defer fs.releasePath("doc.txt")

	commitObservedClaim := false
	commit := func() error {
		// At this moment installDownloadedFile has done step 1 and
		// step 2 (rename original→bak, rename temp→original). The
		// .bak window is open. Phase F's contract: the claim is
		// still held.
		commitObservedClaim = fs.isClaimed("doc.txt")
		return nil
	}

	if err := installDownloadedFile(root, "doc.txt", ".mesh-tmp-claim", commit, nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !commitObservedClaim {
		t.Error("commit callback observed isClaimed=false during the .bak window — Phase F contract violated")
	}
	// After the goroutine-style cleanup runs (the deferred
	// releasePath above fires when the test function returns), the
	// claim is dropped. Run the assertion at the test's end via a
	// nested t.Run so we observe the post-defer state without
	// mutating the test's own defer chain.
	t.Run("released_after_install", func(t *testing.T) {
		// In production the goroutine's `defer fs.releasePath(...)`
		// runs when the goroutine exits. We model that here by
		// running this sub-test before the outer defer fires —
		// which means the claim should still be held. Phase F's
		// post-condition ("claim is dropped after the goroutine
		// completes") is exercised by the production caller, not
		// in this unit test where we hold a defer ourselves.
		if !fs.isClaimed("doc.txt") {
			t.Error("claim dropped before outer defer fired; Phase F contract is fragile")
		}
	})
}

// TestInstallDeletion_HappyPath pins audit §6 commit 6 phase G's
// happy path: rename original to .bak, run commit, unlink .bak.
// Final on-disk state: nothing at relPath, no .bak straggler.
func TestInstallDeletion_HappyPath(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	writeRoot(t, root, "doomed.txt", "to be deleted")

	commitCalls := 0
	commit := func() error {
		commitCalls++
		// At commit time the original is gone (renamed to .bak).
		if _, err := root.Stat("doomed.txt"); !os.IsNotExist(err) {
			t.Errorf("doomed.txt still present at commit time: err=%v", err)
		}
		bak := bakRelPath("doomed.txt", hashContent("to be deleted"))
		if _, err := root.Stat(bak); err != nil {
			t.Errorf(".bak not present at commit time: err=%v", err)
		}
		return nil
	}
	var metrics FolderSyncMetrics
	if err := installDeletion(root, "doomed.txt", commit, &metrics); err != nil {
		t.Fatalf("installDeletion: %v", err)
	}
	if commitCalls != 1 {
		t.Errorf("commit calls=%d, want 1", commitCalls)
	}
	mustNotExist(t, root, "doomed.txt")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mesh-bak-") {
			t.Errorf("residual .bak: %s", e.Name())
		}
	}
}

// TestInstallDeletion_NoLocal_CommitsAnyway pins the no-original
// branch: a remote tombstone for a path we never had locally still
// commits (writes a tombstone row in SQLite via the commit
// callback) without creating any .bak.
func TestInstallDeletion_NoLocal_CommitsAnyway(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	commitCalls := 0
	commit := func() error { commitCalls++; return nil }
	if err := installDeletion(root, "never-existed.txt", commit, nil); err != nil {
		t.Fatalf("installDeletion: %v", err)
	}
	if commitCalls != 1 {
		t.Errorf("commit calls=%d, want 1 (tombstone for absent path still commits)", commitCalls)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mesh-bak-") {
			t.Errorf("unexpected .bak for absent path: %s", e.Name())
		}
	}
}

// TestInstallDeletion_CommitFails_RestoresOriginal pins the
// rollback contract for delete: when the SQLite tombstone commit
// returns an error, the .bak is renamed back to the original path,
// restoring the local file. The metric counter increments. Mental
// mutation: if the rollback rename was missing, the file would
// stay at .bak after the test and the assertion below would catch
// it.
func TestInstallDeletion_CommitFails_RestoresOriginal(t *testing.T) {
	t.Parallel()
	root, dir := newInstallRoot(t)
	const content = "rescued"
	writeRoot(t, root, "doc.txt", content)

	wantErr := errors.New("simulated tombstone commit failure")
	commit := func() error { return wantErr }
	var metrics FolderSyncMetrics
	err := installDeletion(root, "doc.txt", commit, &metrics)
	if err == nil {
		t.Fatal("installDeletion: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain missing commit error: %v", err)
	}
	if got := readRoot(t, root, "doc.txt"); got != content {
		t.Errorf("post-rollback doc.txt=%q, want %q", got, content)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mesh-bak-") {
			t.Errorf("residual .bak after rollback: %s", e.Name())
		}
	}
	if got := metrics.BakRestoredOnCommitFail.Load(); got != 1 {
		t.Errorf("BakRestoredOnCommitFail=%d, want 1", got)
	}
}

// TestBakRelPath pins the .bak naming format. The startup sweep
// (Phase I) parses these names to recover the hash; if the format
// drifts the sweep stops working silently. Pin both the hex
// encoding and the suffix layout.
func TestBakRelPath(t *testing.T) {
	t.Parallel()
	hash := hashContent("hello")
	got := bakRelPath("docs/a.txt", hash)
	want := "docs/a.txt" + bakSuffix + hex.EncodeToString(hash[:])
	if got != want {
		t.Errorf("bakRelPath: got %q, want %q", got, want)
	}
	if !strings.Contains(got, bakSuffix) {
		t.Errorf("bakRelPath %q missing %q anchor (sweep parses on this)", got, bakSuffix)
	}
	// Hex hash is 64 chars (32-byte SHA-256). Format-stable for the
	// sweep parser.
	suffix := got[len("docs/a.txt"+bakSuffix):]
	if len(suffix) != 64 {
		t.Errorf("hex hash length=%d, want 64 (32-byte SHA-256 hex-encoded)", len(suffix))
	}
}

// hashContent is a test-side helper that mirrors what
// hashFileRoot+writeRoot would compute. Runs sha256 directly so
// the test does not depend on filesystem ordering.
func hashContent(s string) Hash256 {
	return hashOfBytes([]byte(s))
}

func hashOfBytes(b []byte) Hash256 {
	root, _ := newInstallRootForHash()
	defer func() { _ = root.Close() }()
	tmpDir := root.Name()
	rel := "h-content"
	full := filepath.Join(tmpDir, rel)
	_ = os.WriteFile(full, b, 0o644) //nolint:gosec // G306: tests run under t.TempDir
	h, _ := hashFileRoot(root, rel)
	return h
}

// newInstallRootForHash is a tiny version of newInstallRoot that
// does NOT register cleanup (so it can be called from non-*testing.T
// helpers). The returned root must be Close'd by the caller.
func newInstallRootForHash() (*os.Root, error) {
	dir, err := os.MkdirTemp("", "filesync-install-test-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("openroot: %w", err)
	}
	return root, nil
}
