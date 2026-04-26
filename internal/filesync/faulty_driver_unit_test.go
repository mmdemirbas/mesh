package filesync

import (
	"context"
	"errors"
	"testing"
)

// withFaultyDriver swaps folderDBDriver to "sqlite_faulty" for
// the duration of a test. Cleanup restores the production
// default. The package-global swap is fine because tests that
// use this helper do NOT use t.Parallel — concurrent driver
// switching would race.
func withFaultyDriver(t *testing.T) {
	t.Helper()
	orig := folderDBDriver
	folderDBDriver = faultyDriverName
	t.Cleanup(func() { folderDBDriver = orig })
}

// TestFaultyDriver_CommitInjection pins audit §6 commit 10 /
// decision §5 #13: with sqlite_faulty registered and a
// faultPointCommit rule installed, saveIndex's COMMIT returns
// the injected error rather than landing the rows. Production
// code's error-handling path treats this as a normal commit
// failure (no row written, in-memory state preserved).
//
// Mental mutation: a Commit() that bypasses checkFault would
// silently land the rows and the assertion below would catch
// it (the file row would be present after the failed call).
func TestFaultyDriver_CommitInjection(t *testing.T) {
	withFaultyDriver(t)
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Install a one-shot fault that fires on the next COMMIT.
	wantErr := errors.New("simulated SQLITE_FULL")
	cleanup := installFault(faultPointCommit, "", wantErr, 1)
	defer cleanup()

	idx := newFileIndex()
	idx.Sequence = 1
	idx.Set("doomed.txt", FileEntry{
		Size: 1, MtimeNS: 1, SHA256: testHash("a"), Sequence: 1,
	})

	err = saveIndex(context.Background(), db, "f", idx)
	if !errors.Is(err, wantErr) {
		t.Fatalf("saveIndex error: %v, want %v", err, wantErr)
	}

	// Verify the row did NOT land — the failed COMMIT rolled back.
	got, err := loadIndexDB(db, "f")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}
	if _, ok := got.Get("doomed.txt"); ok {
		t.Error("doomed.txt present after failed COMMIT — fault injection did not roll back")
	}
}

// TestFaultyDriver_BeginInjection pins the begin hook: a
// fault on faultPointBegin fires before BEGIN IMMEDIATE acquires
// the writer lock, so no tx is even opened.
func TestFaultyDriver_BeginInjection(t *testing.T) {
	withFaultyDriver(t)
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	wantErr := errors.New("simulated SQLITE_BUSY at BEGIN")
	cleanup := installFault(faultPointBegin, "", wantErr, 1)
	defer cleanup()

	idx := newFileIndex()
	idx.Sequence = 1
	idx.Set("a.txt", FileEntry{Size: 1, MtimeNS: 1, SHA256: testHash("a"), Sequence: 1})

	err = saveIndex(context.Background(), db, "f", idx)
	if !errors.Is(err, wantErr) {
		t.Fatalf("saveIndex error: %v, want %v", err, wantErr)
	}
}

// TestFaultyDriver_BoundedFire pins the count semantics: a fault
// installed with count=2 fires twice and then becomes a no-op.
// Without the bound, a single rule would fire forever and tests
// using it would have to clean up between every assertion.
func TestFaultyDriver_BoundedFire(t *testing.T) {
	withFaultyDriver(t)
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	wantErr := errors.New("limited fault")
	cleanup := installFault(faultPointCommit, "", wantErr, 2)
	defer cleanup()

	mkIdx := func(seq int64) *FileIndex {
		idx := newFileIndex()
		idx.Sequence = seq
		idx.Set("a.txt", FileEntry{Size: 1, MtimeNS: 1, SHA256: testHash("a"), Sequence: seq})
		return idx
	}

	// First two commits fail.
	if err := saveIndex(context.Background(), db, "f", mkIdx(1)); !errors.Is(err, wantErr) {
		t.Errorf("first call: err=%v, want fault", err)
	}
	if err := saveIndex(context.Background(), db, "f", mkIdx(2)); !errors.Is(err, wantErr) {
		t.Errorf("second call: err=%v, want fault", err)
	}
	// Third commit succeeds (rule budget exhausted).
	if err := saveIndex(context.Background(), db, "f", mkIdx(3)); err != nil {
		t.Errorf("third call: err=%v, want nil (budget exhausted)", err)
	}
}
