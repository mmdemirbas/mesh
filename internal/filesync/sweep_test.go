package filesync

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sweepSetup opens a folder DB and an os.Root for sweep tests.
// Caller registers cleanup on t. The returned db is the writer
// handle from openFolderDB; the root points at a fresh tempdir
// the test populates with .mesh-bak-* files.
func sweepSetup(t *testing.T) (*sql.DB, *os.Root) {
	t.Helper()
	cacheDir := t.TempDir()
	db, err := openFolderDB(cacheDir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	folderDir := t.TempDir()
	root, err := os.OpenRoot(folderDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return db, root
}

// seedRow writes a single live (or tombstoned) row to SQLite for
// the given folder. Called before runStartupBakSweep so SQLite's
// view is established.
func seedRow(t *testing.T, db *sql.DB, folderID, relPath string, hash Hash256, deleted bool) {
	t.Helper()
	idx := newFileIndex()
	idx.Sequence = 1
	idx.Set(relPath, FileEntry{
		Size: int64(len("c")), MtimeNS: 1, SHA256: hash,
		Sequence: 1, Mode: 0o644, Deleted: deleted,
	})
	if err := saveIndex(context.Background(), db, folderID, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
}

// putBakFile creates a .mesh-bak-<hash> sidecar at the given
// original-path's directory with the supplied content. The hash in
// the filename is taken from filenameHash (which may or may not
// match the content hash — used to exercise both branches).
func putBakFile(t *testing.T, root *os.Root, originalRel string, filenameHash Hash256, content string) string {
	t.Helper()
	if dir := filepath.Dir(originalRel); dir != "." {
		_ = root.MkdirAll(dir, 0o755)
	}
	bak := bakRelPath(originalRel, filenameHash)
	f, err := root.Create(bak)
	if err != nil {
		t.Fatalf("Create %s: %v", bak, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("Write %s: %v", bak, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close %s: %v", bak, err)
	}
	return bak
}

func putFile(t *testing.T, root *os.Root, relPath, content string) {
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

// TestSweep_BakMatchesOriginalHash_Restores pins audit F7 sweep
// branch (b): when SQLite still carries the original (pre-download)
// hash and it equals the .bak filename hash, the .bak is renamed
// back to the original path. This is the "commit-failed-then-
// crashed-before-rollback" recovery — without the sweep, the
// folder would silently keep the SQLite-rejected new content (or
// a missing-file state) forever.
func TestSweep_BakMatchesOriginalHash_Restores(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	oldHash := hashContent("old content")
	// Seed SQLite with the old hash — represents the pre-download
	// state. The download crashed before commit, leaving a .bak
	// holding the old content and the original holding the new.
	seedRow(t, db, folderID, "doc.txt", oldHash, false)
	putBakFile(t, root, "doc.txt", oldHash, "old content")
	putFile(t, root, "doc.txt", "new content (rejected by crashed commit)")

	res, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Restored != 1 {
		t.Errorf("Restored=%d, want 1", res.Restored)
	}
	if res.Unlinked != 0 {
		t.Errorf("Unlinked=%d, want 0", res.Unlinked)
	}
	// On disk: doc.txt now holds the OLD content (restored from
	// .bak), and no .bak file remains.
	gotContent, _ := readRootContent(t, root, "doc.txt")
	if gotContent != "old content" {
		t.Errorf("doc.txt=%q, want %q (restored)", gotContent, "old content")
	}
	bak := bakRelPath("doc.txt", oldHash)
	if _, err := root.Stat(bak); !os.IsNotExist(err) {
		t.Errorf(".bak still present after restore: err=%v", err)
	}
}

// TestSweep_BakMatchesNewHash_Unlinks pins audit F7 sweep branch
// (a): SQLite carries the new content's hash AND the on-disk file
// matches; the .bak is stale (commit succeeded, only the unlink
// crashed). Sweep removes the .bak and leaves the original alone.
func TestSweep_BakMatchesNewHash_Unlinks(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	oldHash := hashContent("old content")
	newHash := hashContent("new content")
	seedRow(t, db, folderID, "doc.txt", newHash, false)
	putBakFile(t, root, "doc.txt", oldHash, "old content")
	putFile(t, root, "doc.txt", "new content")

	res, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Unlinked != 1 {
		t.Errorf("Unlinked=%d, want 1", res.Unlinked)
	}
	if res.Restored != 0 {
		t.Errorf("Restored=%d, want 0", res.Restored)
	}
	gotContent, _ := readRootContent(t, root, "doc.txt")
	if gotContent != "new content" {
		t.Errorf("doc.txt=%q, want %q (untouched, post-commit state)", gotContent, "new content")
	}
	bak := bakRelPath("doc.txt", oldHash)
	if _, err := root.Stat(bak); !os.IsNotExist(err) {
		t.Errorf(".bak still present after unlink: err=%v", err)
	}
}

// TestSweep_TombstoneRow_UnlinksBak pins the Phase G interaction:
// installDeletion left a .bak holding the pre-tombstone content;
// the SQLite tombstone landed before the unlink crashed. Sweep
// recognizes the deleted=1 row and treats it as branch (a)
// regardless of whether the bak's filename hash matches — a
// tombstoned file MUST NOT be resurrected by the sweep.
func TestSweep_TombstoneRow_UnlinksBak(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	oldHash := hashContent("doomed")
	// Seed a tombstone row with the old hash.
	seedRow(t, db, folderID, "doomed.txt", oldHash, true)
	putBakFile(t, root, "doomed.txt", oldHash, "doomed")

	res, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Unlinked != 1 {
		t.Errorf("Unlinked=%d, want 1", res.Unlinked)
	}
	if res.Restored != 0 {
		t.Errorf("Restored=%d, want 0 (tombstoned row must not resurrect)", res.Restored)
	}
	if _, err := root.Stat("doomed.txt"); !os.IsNotExist(err) {
		t.Errorf("doomed.txt resurrected by sweep: err=%v", err)
	}
}

// TestSweep_OrphanBak_Recorded pins the orphan branch: a .bak
// whose original path has no SQLite row at all. The sweep records
// it but does not touch it — cleanTempFiles handles aged stragglers
// on the slow path.
func TestSweep_OrphanBak_Recorded(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	someHash := hashContent("orphan content")
	putBakFile(t, root, "ghost.txt", someHash, "orphan content")

	res, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Orphans) != 1 {
		t.Errorf("Orphans len=%d, want 1: %v", len(res.Orphans), res.Orphans)
	}
	bak := bakRelPath("ghost.txt", someHash)
	if _, err := root.Stat(bak); err != nil {
		t.Errorf("orphan .bak removed by sweep: err=%v (sweep must NOT touch orphans)", err)
	}
}

// TestSweep_NeitherMatches_DisablesWithUnknown pins audit F7
// iter-4 Z13 (commit 6.2 phase J): when SQLite's hash matches
// neither the on-disk file's hash NOR the .bak filename hash, the
// sweep returns errSweepNeitherMatches and records the path so the
// caller transitions to FolderDisabled(unknown). Without this
// branch, Phase D's classifier would later read the divergent
// state as a long stream of false-positive conflicts.
func TestSweep_NeitherMatches_DisablesWithUnknown(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	sqliteHash := hashContent("what SQLite says")
	bakHash := hashContent("what bak filename says (different)")
	seedRow(t, db, folderID, "doc.txt", sqliteHash, false)
	putBakFile(t, root, "doc.txt", bakHash, "bak content")
	putFile(t, root, "doc.txt", "yet another wrong content")

	res, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if !errors.Is(err, errSweepNeitherMatches) {
		t.Fatalf("err=%v, want errSweepNeitherMatches", err)
	}
	if len(res.NeitherMatches) != 1 || res.NeitherMatches[0] != "doc.txt" {
		t.Errorf("NeitherMatches=%v, want [doc.txt]", res.NeitherMatches)
	}
	// .bak intact — the operator must triage before sweep moves
	// anything around.
	bak := bakRelPath("doc.txt", bakHash)
	if _, err := root.Stat(bak); err != nil {
		t.Errorf(".bak removed despite Z13: %v", err)
	}
}

// TestSweep_DBUnreadable_PreservesBak pins audit F7 iter-4 Z1
// (commit 6.2 phase J): if a per-row SQLite query fails after the
// folder open's quick_check passed, the sweep returns
// errSweepDBUnreadable WITHOUT touching the .bak files. The
// restore-from-backup procedure (commit 9) re-runs the sweep
// against the restored DB after reopen.
//
// Simulates the DB-unreadable case by closing the writer handle
// before running the sweep — every Query returns "sql: database
// is closed" which the sweep wraps as errSweepDBUnreadable.
func TestSweep_DBUnreadable_PreservesBak(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	bakH := hashContent("survives")
	putBakFile(t, root, "doc.txt", bakH, "survives")
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if !errors.Is(err, errSweepDBUnreadable) {
		t.Fatalf("err=%v, want errSweepDBUnreadable", err)
	}
	bak := bakRelPath("doc.txt", bakH)
	if _, err := root.Stat(bak); err != nil {
		t.Errorf(".bak removed when DB was unreadable: %v (Z1 contract: preserve)", err)
	}
}

// TestSweep_IgnoresMalformedBakNames pins the parser's safety
// against external files that happen to contain ".mesh-bak-" but
// don't have a 64-char hex hash suffix. The walker must silently
// skip them — they are not the F7 sweep's concern.
func TestSweep_IgnoresMalformedBakNames(t *testing.T) {
	t.Parallel()
	db, root := sweepSetup(t)
	const folderID = "f"

	putFile(t, root, "doc.mesh-bak-not-hex", "external file")
	putFile(t, root, "doc.mesh-bak-tooshort", "external file")
	putFile(t, root, "doc.mesh-bak-"+strings.Repeat("g", 64), "external file (g is not hex)")

	res, err := runStartupBakSweep(context.Background(), db, root, folderID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned=%d, want 0 (malformed names should be skipped)", res.Scanned)
	}
}

// TestSweep_HashFormatRoundTrip pins parse symmetry with bakRelPath:
// every name produced by bakRelPath must decode to the same hash
// the sweep parses out. Mental mutation: changing the hex.Decode
// width would silently break the sweep on every restart.
func TestSweep_HashFormatRoundTrip(t *testing.T) {
	t.Parallel()
	hash := hashContent("round trip me")
	relBak := bakRelPath("docs/x.txt", hash)
	idx := strings.LastIndex(relBak, bakSuffix)
	if idx < 0 {
		t.Fatalf("bakRelPath produced unparseable name: %s", relBak)
	}
	hashHex := relBak[idx+len(bakSuffix):]
	if len(hashHex) != 64 {
		t.Errorf("hash hex length=%d, want 64", len(hashHex))
	}
	var decoded Hash256
	if _, err := hex.Decode(decoded[:], []byte(hashHex)); err != nil {
		t.Errorf("hex.Decode: %v", err)
	}
	if decoded != hash {
		t.Errorf("decoded hash != original")
	}
}

// readRootContent is a small reader for the sweep tests. Returns
// the file's content as a string and any read error.
func readRootContent(t *testing.T, root *os.Root, relPath string) (string, error) {
	t.Helper()
	f, err := root.Open(relPath)
	if err != nil {
		return "", err
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
	return string(buf), nil
}
