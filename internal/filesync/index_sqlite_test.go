package filesync

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// SQLite reports synchronous=FULL as integer 2. W5 in
	// PERSISTENCE-AUDIT.md: NORMAL is rejected because it allows the
	// last committed tx to roll back on power loss.
	if sync != 2 {
		t.Fatalf("synchronous=%d want 2 (FULL)", sync)
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

// TestSaveLoadIndex_RoundTrip pins that every FileEntry field survives a
// save/load cycle through SQLite: basic metadata, SHA-256, vector clock,
// inode, prev_path, and PH incremental-hashing state.
func TestSaveLoadIndex_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	idx := newFileIndex()
	idx.Sequence = 42
	idx.Epoch = "deadbeefcafef00d"
	idx.DeviceID = 0x10203040
	idx.Files["docs/readme.md"] = FileEntry{
		Size:     1234,
		MtimeNS:  1_700_000_000_000_000_000,
		SHA256:   hash256FromBytes(bytes32('a')),
		Deleted:  false,
		Sequence: 10,
		Mode:     0o644,
		Inode:    987654,
		Version:  VectorClock{"ABCDE12345": 7, "PEER00002X": 3},
	}
	idx.Files["archive/old.log"] = FileEntry{
		Size:        0,
		MtimeNS:     1_700_000_001_000_000_000,
		SHA256:      hash256FromBytes(bytes32('b')),
		Deleted:     true,
		Sequence:    11,
		Mode:        0o600,
		PrevPath:    "archive/older.log",
		HashState:   []byte{0x01, 0x02, 0x03},
		HashedBytes: 4096,
		PrefixCheck: []byte{0xff, 0xee},
	}
	idx.recomputeCache()

	if err := saveIndex(db, "shared", idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}

	got, err := loadIndexDB(db, "shared")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}

	if got.Sequence != idx.Sequence {
		t.Errorf("Sequence=%d want %d", got.Sequence, idx.Sequence)
	}
	if got.Epoch != idx.Epoch {
		t.Errorf("Epoch=%q want %q", got.Epoch, idx.Epoch)
	}
	if got.DeviceID != idx.DeviceID {
		t.Errorf("DeviceID=%#x want %#x", got.DeviceID, idx.DeviceID)
	}
	if len(got.Files) != len(idx.Files) {
		t.Fatalf("Files len=%d want %d", len(got.Files), len(idx.Files))
	}
	for path, want := range idx.Files {
		have, ok := got.Files[path]
		if !ok {
			t.Errorf("%s missing after reload", path)
			continue
		}
		if have.Size != want.Size || have.MtimeNS != want.MtimeNS ||
			have.SHA256 != want.SHA256 || have.Deleted != want.Deleted ||
			have.Sequence != want.Sequence || have.Mode != want.Mode ||
			have.Inode != want.Inode || have.PrevPath != want.PrevPath ||
			have.HashedBytes != want.HashedBytes {
			t.Errorf("%s: scalar mismatch\nhave=%+v\nwant=%+v", path, have, want)
		}
		if !bytesEqual(have.HashState, want.HashState) {
			t.Errorf("%s HashState mismatch", path)
		}
		if !bytesEqual(have.PrefixCheck, want.PrefixCheck) {
			t.Errorf("%s PrefixCheck mismatch", path)
		}
		if !clocksEqual(have.Version, want.Version) {
			t.Errorf("%s Version=%v want %v", path, have.Version, want.Version)
		}
	}

	// Active count/size recomputed from the reloaded rows; archive/old.log
	// is deleted so only docs/readme.md contributes.
	wantCount, wantSize := 1, int64(1234)
	if got.cachedCount != wantCount || got.cachedSize != wantSize {
		t.Errorf("cache=(%d,%d) want (%d,%d)",
			got.cachedCount, got.cachedSize, wantCount, wantSize)
	}
}

// TestSaveIndex_ReplacesPriorRows pins that a second save drops file rows
// that disappeared between snapshots instead of leaving ghost entries.
func TestSaveIndex_ReplacesPriorRows(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first := newFileIndex()
	first.Files["a.txt"] = FileEntry{Size: 1, SHA256: hash256FromBytes(bytes32('a'))}
	first.Files["b.txt"] = FileEntry{Size: 2, SHA256: hash256FromBytes(bytes32('b'))}
	if err := saveIndex(db, "shared", first); err != nil {
		t.Fatalf("first saveIndex: %v", err)
	}

	second := newFileIndex()
	second.Files["a.txt"] = FileEntry{Size: 11, SHA256: hash256FromBytes(bytes32('a'))}
	if err := saveIndex(db, "shared", second); err != nil {
		t.Fatalf("second saveIndex: %v", err)
	}

	reloaded, err := loadIndexDB(db, "shared")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}
	if _, present := reloaded.Files["b.txt"]; present {
		t.Fatalf("b.txt not purged after second save: %+v", reloaded.Files)
	}
	if got := reloaded.Files["a.txt"].Size; got != 11 {
		t.Fatalf("a.txt Size=%d want 11", got)
	}
}

// TestEncodeVectorClock_DeterministicOrder pins that two semantically
// equal VectorClocks serialize to byte-identical blobs regardless of map
// iteration order — required so index saves don't drift on a no-op scan.
func TestEncodeVectorClock_DeterministicOrder(t *testing.T) {
	a := VectorClock{"ABCDE12345": 7, "PEER00002X": 3, "QRSTU67890": 1}
	b := VectorClock{"PEER00002X": 3, "QRSTU67890": 1, "ABCDE12345": 7}
	if !bytesEqual(encodeVectorClock(a), encodeVectorClock(b)) {
		t.Fatalf("encodeVectorClock not deterministic:\n a=%x\n b=%x",
			encodeVectorClock(a), encodeVectorClock(b))
	}
}

// TestDecodeVectorClock_RejectsMalformed pins that a truncated blob
// decodes as nil rather than panicking or returning garbage.
func TestDecodeVectorClock_RejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":        nil,
		"short":        {0},
		"header only":  {0, 1},
		"wrong length": append(append([]byte{0, 1}, []byte("ABCDE12345")...), 0, 0, 0, 0),
	}
	for name, blob := range cases {
		if got := decodeVectorClock(blob); got != nil {
			t.Errorf("%s: got %v, want nil", name, got)
		}
	}
}

func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func clocksEqual(a, b VectorClock) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestSaveLoadPeerStates_RoundTrip pins that every PeerState field
// survives a save/load cycle: sequences, LastSync, epochs, soft-delete
// state, and the per-path BaseHashes map that C2 relies on.
func TestSaveLoadPeerStates_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	lastSync := time.Date(2026, 4, 20, 12, 34, 56, 789, time.UTC)
	removedAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	peers := map[string]PeerState{
		"peer-alpha": {
			LastSeenSequence: 101,
			LastSentSequence: 99,
			LastSync:         lastSync,
			LastEpoch:        "epochaaa00000001",
			PendingEpoch:     "epochbbbb0000002",
			BaseHashes: map[string]Hash256{
				"docs/a.md":    hash256FromBytes(bytes32('x')),
				"notes/b.txt":  hash256FromBytes(bytes32('y')),
				"index/c.json": hash256FromBytes(bytes32('z')),
			},
		},
		"peer-bravo": {
			LastSeenSequence: 7,
			LastSentSequence: 7,
			Removed:          true,
			RemovedAt:        removedAt,
		},
	}

	if err := savePeerStatesDB(db, "shared", peers); err != nil {
		t.Fatalf("savePeerStatesDB: %v", err)
	}
	got, err := loadPeerStatesDB(db, "shared")
	if err != nil {
		t.Fatalf("loadPeerStatesDB: %v", err)
	}

	if len(got) != len(peers) {
		t.Fatalf("len=%d want %d", len(got), len(peers))
	}

	alpha := got["peer-alpha"]
	want := peers["peer-alpha"]
	if alpha.LastSeenSequence != want.LastSeenSequence ||
		alpha.LastSentSequence != want.LastSentSequence ||
		!alpha.LastSync.Equal(want.LastSync) ||
		alpha.LastEpoch != want.LastEpoch ||
		alpha.PendingEpoch != want.PendingEpoch {
		t.Errorf("alpha scalars mismatch:\nhave=%+v\nwant=%+v", alpha, want)
	}
	if len(alpha.BaseHashes) != len(want.BaseHashes) {
		t.Fatalf("alpha.BaseHashes len=%d want %d",
			len(alpha.BaseHashes), len(want.BaseHashes))
	}
	for path, hash := range want.BaseHashes {
		if alpha.BaseHashes[path] != hash {
			t.Errorf("alpha.BaseHashes[%s]=%x want %x",
				path, alpha.BaseHashes[path], hash)
		}
	}

	bravo := got["peer-bravo"]
	if !bravo.Removed {
		t.Errorf("bravo.Removed=false want true")
	}
	if !bravo.RemovedAt.Equal(removedAt) {
		t.Errorf("bravo.RemovedAt=%v want %v", bravo.RemovedAt, removedAt)
	}
	if bravo.BaseHashes != nil {
		t.Errorf("bravo.BaseHashes=%v want nil", bravo.BaseHashes)
	}
}

// TestSavePeerStates_ReplacesPriorRows pins that a second save drops
// peers and per-path hashes that disappeared between snapshots, so the
// table does not accumulate ghost state across config changes.
func TestSavePeerStates_ReplacesPriorRows(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first := map[string]PeerState{
		"peer-a": {
			LastSeenSequence: 1,
			BaseHashes: map[string]Hash256{
				"keep.txt": hash256FromBytes(bytes32('k')),
				"drop.txt": hash256FromBytes(bytes32('d')),
			},
		},
		"peer-b": {LastSeenSequence: 2},
	}
	if err := savePeerStatesDB(db, "shared", first); err != nil {
		t.Fatalf("first save: %v", err)
	}

	second := map[string]PeerState{
		"peer-a": {
			LastSeenSequence: 10,
			BaseHashes: map[string]Hash256{
				"keep.txt": hash256FromBytes(bytes32('k')),
			},
		},
	}
	if err := savePeerStatesDB(db, "shared", second); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := loadPeerStatesDB(db, "shared")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, present := got["peer-b"]; present {
		t.Fatalf("peer-b not purged after second save")
	}
	a, ok := got["peer-a"]
	if !ok {
		t.Fatalf("peer-a missing after second save")
	}
	if a.LastSeenSequence != 10 {
		t.Errorf("peer-a.LastSeenSequence=%d want 10", a.LastSeenSequence)
	}
	if _, present := a.BaseHashes["drop.txt"]; present {
		t.Fatalf("peer-a.BaseHashes[drop.txt] not purged: %v", a.BaseHashes)
	}
	if a.BaseHashes["keep.txt"] != hash256FromBytes(bytes32('k')) {
		t.Errorf("peer-a.BaseHashes[keep.txt] wrong: %x",
			a.BaseHashes["keep.txt"])
	}
}

// TestLoadPeerStates_Empty pins that a fresh database yields an empty
// map rather than an error — callers treat "no prior state" as normal
// for first-run behavior.
func TestLoadPeerStates_Empty(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	got, err := loadPeerStatesDB(db, "shared")
	if err != nil {
		t.Fatalf("loadPeerStatesDB: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh db returned %d peer states: %v", len(got), got)
	}
}
