package filesync

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenFolderDB_CreatesSchemaAndPragmas pins that the first open of a
// new folder cache dir yields a SQLite database with the v1 tables, the
// v1 PRAGMA values, and the seeded folder_meta rows.
func TestOpenFolderDB_CreatesSchemaAndPragmas(t *testing.T) {
	dir := t.TempDir()
	const devID = "ABCDE12345"

	db, err := openFolderDB(dir, devID)
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// File lands at the expected path.
	if _, err := filepath.Abs(filepath.Join(dir, folderDBFilename)); err != nil {
		t.Fatalf("abs db path: %v", err)
	}

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Fatalf("journal_mode=%q want wal", mode)
	}

	var sync int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&sync); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	// SQLite reports synchronous=NORMAL as integer 1.
	if sync != 1 {
		t.Fatalf("synchronous=%d want 1 (NORMAL)", sync)
	}

	wantTables := []string{"folder_meta", "files", "blocks", "peer_state"}
	for _, tbl := range wantTables {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}

	gotDev, err := folderMeta(db, "device_id")
	if err != nil {
		t.Fatalf("folderMeta(device_id): %v", err)
	}
	if gotDev != devID {
		t.Fatalf("device_id=%q want %q", gotDev, devID)
	}
	gotVer, err := folderMeta(db, "schema_version")
	if err != nil {
		t.Fatalf("folderMeta(schema_version): %v", err)
	}
	if gotVer != "1" {
		t.Fatalf("schema_version=%q want \"1\"", gotVer)
	}
	gotEpoch, err := folderMeta(db, "epoch")
	if err != nil {
		t.Fatalf("folderMeta(epoch): %v", err)
	}
	if len(gotEpoch) != 16 {
		t.Fatalf("epoch=%q (len %d) want 16 hex chars", gotEpoch, len(gotEpoch))
	}
}

// TestOpenFolderDB_IdempotentReopen pins that reopening an existing
// database preserves the original device_id, epoch, and schema version —
// no row churn, no silent re-seed.
func TestOpenFolderDB_IdempotentReopen(t *testing.T) {
	dir := t.TempDir()

	first, err := openFolderDB(dir, "ORIGINAL01")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	originalEpoch, err := folderMeta(first, "epoch")
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	_ = first.Close()

	// Reopen with a different candidate device ID; the persisted row must
	// win because it is non-empty.
	second, err := openFolderDB(dir, "DIFFERENT2")
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	gotDev, err := folderMeta(second, "device_id")
	if err != nil {
		t.Fatalf("device_id: %v", err)
	}
	if gotDev != "ORIGINAL01" {
		t.Fatalf("device_id changed on reopen: got %q want ORIGINAL01", gotDev)
	}
	gotEpoch, err := folderMeta(second, "epoch")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if gotEpoch != originalEpoch {
		t.Fatalf("epoch churned on reopen: got %q want %q", gotEpoch, originalEpoch)
	}
}

// TestOpenFolderDB_RejectsMismatchedSchemaVersion pins the guard that
// refuses to open a database whose schema_version does not match the
// binary. A future migration must bump schemaVersion and supply an
// explicit upgrade path.
func TestOpenFolderDB_RejectsMismatchedSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE folder_meta SET value=? WHERE key='schema_version'`, 999,
	); err != nil {
		t.Fatalf("overwrite schema_version: %v", err)
	}
	_ = db.Close()

	_, err = openFolderDB(dir, "ABCDE12345")
	if err == nil {
		t.Fatalf("reopen with bumped schema_version: want error, got nil")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error does not mention schema_version: %v", err)
	}
}

// TestOpenFolderDB_EmptyDeviceIDRejected pins the argument check so that
// a misconfigured caller cannot produce a database with an empty
// device_id row.
func TestOpenFolderDB_EmptyDeviceIDRejected(t *testing.T) {
	if _, err := openFolderDB(t.TempDir(), ""); err == nil {
		t.Fatalf("empty device id: want error, got nil")
	}
}
