package filesync

import (
	"bytes"
	"context"
	"database/sql"
	"hash/crc32"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

// TestOpen_SynchronousIsFULL pins decision §5 #5 (W5):
// synchronous=FULL is required so the last committed tx survives a
// power loss; NORMAL is rejected. It also asserts journal_mode=wal in
// the same test (D5 / iter-3 review §D) so a future refactor cannot
// regress one PRAGMA without the other — the two are inseparable for
// the audit's durability contract.
func TestOpen_SynchronousIsFULL(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var sync int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&sync); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	// SQLite reports synchronous=FULL as integer 2.
	if sync != 2 {
		t.Fatalf("synchronous=%d want 2 (FULL)", sync)
	}

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Fatalf("journal_mode=%q want wal", mode)
	}
}

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

	// Per audit decision §5 #7 / iter-3 A6 / commit 3, the `blocks`
	// table is NOT in the v1 schema — block hashes compute on demand
	// (see comment in applyFolderDBSchema).
	wantTables := []string{"folder_meta", "files", "peer_state"}
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
	idx.Set("docs/readme.md", FileEntry{
		Size:     1234,
		MtimeNS:  1_700_000_000_000_000_000,
		SHA256:   hash256FromBytes(bytes32('a')),
		Deleted:  false,
		Sequence: 10,
		Mode:     0o644,
		Inode:    987654,
		Version:  VectorClock{"ABCDE12345": 7, "PEER00002X": 3},
	})
	idx.Set("archive/old.log", FileEntry{
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
	})
	idx.recomputeCache()

	if err := saveIndex(context.Background(), db, "shared", idx); err != nil {
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
	if got.Len() != idx.Len() {
		t.Fatalf("Files len=%d want %d", got.Len(), idx.Len())
	}
	for path, want := range idx.Range {
		have, ok := got.Get(path)
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

// TestSaveIndex_DeletePathRemovesRow pins the per-path persist
// model (PERSISTENCE-AUDIT.md §2.5 P2): a row is removed from SQLite
// only when the FileIndex's Delete API is called for that path; a
// second saveIndex without Delete leaves untouched rows in place.
// This is a deliberate behavior change from commit 1's
// DELETE+INSERT-everything pattern (which was the bench-disqualified
// 655 ms cost path) — the dirty/deleted set tells saveIndex what to
// touch.
func TestSaveIndex_DeletePathRemovesRow(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first := newFileIndex()
	first.Set("a.txt", FileEntry{Size: 1, Sequence: 1, SHA256: hash256FromBytes(bytes32('a'))})
	first.Set("b.txt", FileEntry{Size: 2, Sequence: 2, SHA256: hash256FromBytes(bytes32('b'))})
	first.Sequence = 2
	if err := saveIndex(context.Background(), db, "shared", first); err != nil {
		t.Fatalf("first saveIndex: %v", err)
	}
	first.ClearDirty()

	// Update a.txt and explicitly Delete b.txt — the second save
	// must UPSERT a.txt and DELETE b.txt; rows not touched stay
	// untouched (there are none here, but the contract is clear).
	// Phase 7E: the UPSERT carries WHERE excluded.sequence >
	// files.sequence, so the rewrite must bump the sequence.
	first.Set("a.txt", FileEntry{Size: 11, Sequence: 3, SHA256: hash256FromBytes(bytes32('a'))})
	first.Sequence = 3
	first.Delete("b.txt")
	if err := saveIndex(context.Background(), db, "shared", first); err != nil {
		t.Fatalf("second saveIndex: %v", err)
	}

	reloaded, err := loadIndexDB(db, "shared")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}
	if _, present := reloaded.Get("b.txt"); present {
		t.Fatalf("b.txt not removed after Delete + saveIndex: %+v", reloaded.Files())
	}
	if got := reloaded.Files()["a.txt"].Size; got != 11 {
		t.Fatalf("a.txt Size=%d want 11", got)
	}
}

// TestMigrate_NoOpForV1 pins audit §6 commit 12 / V1: the
// schema-evolution migration hook fires unconditionally at every
// folder open. v1 → v1 is a no-op (the only valid transition
// today) but the invocation site is structurally anchored so
// future bumps (V2) compose without reintroducing the hook.
//
// Cannot use t.Parallel — the test reads a process-global
// counter and concurrent tests calling openFolderDB would
// advance it too. Sequential execution is fine; the test is
// fast.
//
// Mental mutation: removing the migrateSchema call from
// openFolderDB makes the per-open delta zero and the assertion
// below catches it.
func TestMigrate_NoOpForV1(t *testing.T) {
	dir := t.TempDir()

	before := migrateSchemaInvocations.Load()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	after := migrateSchemaInvocations.Load()
	t.Cleanup(func() { _ = db.Close() })

	if after-before < 1 {
		t.Errorf("migrateSchemaInvocations did not advance across one openFolderDB; want at least +1, got delta=%d",
			after-before)
	}

	// Direct call: v1 → v1 returns nil (no-op).
	if err := migrateSchema(db, schemaVersion, schemaVersion); err != nil {
		t.Errorf("v1→v1: err=%v, want nil (no-op)", err)
	}
	// Downgrade rejected.
	if err := migrateSchema(db, schemaVersion+1, schemaVersion); err == nil {
		t.Error("downgrade accepted; expected error")
	}
	// Forward past binary's schema rejected.
	if err := migrateSchema(db, schemaVersion, schemaVersion+1); err == nil {
		t.Error("forward past binary schema accepted; expected error")
	}
}

// TestSequenceConditionedUpsert_OldSequenceLoses pins audit §6
// commit 7 phase E / INV-3: the UPSERT in applyIndexToTx carries
// WHERE excluded.sequence > files.sequence. A second UPSERT with a
// SMALLER sequence than the row's current sequence MUST NOT
// overwrite the row — even if every other column is different.
// This is the load-bearing protection against a torn write or a
// future race that opens a parallel writer connection.
//
// Mental mutation: removing the WHERE clause would make the second
// UPSERT clobber the row and the assertion below would catch it.
func TestSequenceConditionedUpsert_OldSequenceLoses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Step 1: write a row at sequence=10 with content "high".
	idx := newFileIndex()
	idx.Sequence = 10
	idx.Set("doc.txt", FileEntry{
		Size:     100,
		MtimeNS:  1,
		SHA256:   hash256FromBytes(bytes32('h')),
		Sequence: 10,
	})
	if err := saveIndex(context.Background(), db, "f", idx); err != nil {
		t.Fatalf("first saveIndex: %v", err)
	}

	// Step 2: try to overwrite with sequence=5 (smaller). The
	// WHERE clause must reject this UPSERT silently — the row
	// should remain at sequence=10 with the original content.
	idx2 := newFileIndex()
	idx2.Sequence = 5
	idx2.Set("doc.txt", FileEntry{
		Size:     999,
		MtimeNS:  9,
		SHA256:   hash256FromBytes(bytes32('l')),
		Sequence: 5,
	})
	if err := saveIndex(context.Background(), db, "f", idx2); err != nil {
		t.Fatalf("second saveIndex: %v", err)
	}

	// Reload — the original sequence=10 entry must survive.
	got, err := loadIndexDB(db, "f")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}
	entry, ok := got.Get("doc.txt")
	if !ok {
		t.Fatal("doc.txt missing after both upserts")
	}
	if entry.Sequence != 10 {
		t.Errorf("sequence=%d, want 10 (older UPSERT must lose)", entry.Sequence)
	}
	if entry.Size != 100 {
		t.Errorf("size=%d, want 100 (older UPSERT must not clobber row body)", entry.Size)
	}
	if entry.SHA256 != hash256FromBytes(bytes32('h')) {
		t.Errorf("sha256 changed; older UPSERT clobbered the row")
	}

	// Sanity: a NEWER sequence DOES overwrite, proving the
	// check is asymmetric and not a no-op for every UPSERT.
	idx3 := newFileIndex()
	idx3.Sequence = 20
	idx3.Set("doc.txt", FileEntry{
		Size:     200,
		MtimeNS:  20,
		SHA256:   hash256FromBytes(bytes32('n')),
		Sequence: 20,
	})
	if err := saveIndex(context.Background(), db, "f", idx3); err != nil {
		t.Fatalf("third saveIndex: %v", err)
	}
	got2, _ := loadIndexDB(db, "f")
	entry2, _ := got2.Get("doc.txt")
	if entry2.Sequence != 20 {
		t.Errorf("after newer UPSERT sequence=%d, want 20", entry2.Sequence)
	}
	if entry2.Size != 200 {
		t.Errorf("after newer UPSERT size=%d, want 200", entry2.Size)
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
// decodes as nil rather than panicking or returning garbage. Two
// length classes are now legitimate (legacy = body, current = body+CRC);
// any other length is malformed.
func TestDecodeVectorClock_RejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":         nil,
		"short":         {0},
		"header only":   {0, 1}, // count=1 but no entry — neither legacy nor current
		"wrong length":  append(append([]byte{0, 1}, []byte("ABCDE12345")...), 0, 0, 0, 0),
		"between forms": make([]byte, 2+1*(deviceIDChars+8)+2), // body + 2 bytes (not 0, not 4)
	}
	for name, blob := range cases {
		if got := decodeVectorClock(blob); got != nil {
			t.Errorf("%s: got %v, want nil", name, got)
		}
	}
}

// TestEncodeVectorClock_TrailerRoundTrip pins audit D7 / commit 6
// phase A: every emitted blob carries a CRC-32/IEEE trailer over all
// preceding bytes, and decode round-trips. The empty case carries
// the trailer too (uniform format — no special-cased lengths).
func TestEncodeVectorClock_TrailerRoundTrip(t *testing.T) {
	cases := []VectorClock{
		nil,
		{},
		{"ABCDE12345": 1},
		{"ABCDE12345": 7, "PEER00002X": 3, "QRSTU67890": 1},
	}
	for _, vc := range cases {
		blob := encodeVectorClock(vc)
		// Body length matches the entry-count header.
		body := 2 + int(uint16BE(blob[0:2]))*(deviceIDChars+8)
		if len(blob) != body+vclockCRCLen {
			t.Errorf("blob len=%d, want body(%d)+CRC(%d)", len(blob), body, vclockCRCLen)
			continue
		}
		// CRC trailer matches IEEE checksum of body.
		want := crc32.ChecksumIEEE(blob[:body])
		got := uint32BE(blob[body : body+vclockCRCLen])
		if want != got {
			t.Errorf("CRC mismatch: blob trailer=%08x, recomputed=%08x", got, want)
		}
		// Round-trips through decode.
		decoded := decodeVectorClock(blob)
		if !clocksEqual(decoded, normalizeVClock(vc)) {
			t.Errorf("round-trip lost data:\n  in =%v\n  out=%v", vc, decoded)
		}
	}
}

// normalizeVClock mirrors encodeVectorClock's filtering: zero-valued
// entries are dropped, empty becomes nil. Used by round-trip asserts
// so a tiny input with all-zero counters compares as nil rather than
// {} (and matches what decode returns).
func normalizeVClock(v VectorClock) VectorClock {
	out := VectorClock{}
	for k, val := range v {
		if val == 0 || len(k) != deviceIDChars {
			continue
		}
		out[k] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TestDecodeVectorClock_AcceptsLegacyBlob pins the dev-loop migration
// path: a blob written by an earlier D4 commit (no CRC trailer)
// decodes successfully so a cold dev DB is not refused on first
// open after Phase A. Upgrade to CRC happens at the next saveIndex
// for that row.
func TestDecodeVectorClock_AcceptsLegacyBlob(t *testing.T) {
	// Hand-construct the legacy format directly (no CRC).
	// One entry: ABCDE12345 → 7.
	blob := make([]byte, 2+1*(deviceIDChars+8))
	putUint16BE(blob[0:2], 1)
	copy(blob[2:2+deviceIDChars], "ABCDE12345")
	putUint64BE(blob[2+deviceIDChars:2+deviceIDChars+8], 7)

	resetLegacyVClockWarned()
	got := decodeVectorClock(blob)
	want := VectorClock{"ABCDE12345": 7}
	if !clocksEqual(got, want) {
		t.Errorf("legacy decode: got %v, want %v", got, want)
	}
}

// TestDecodeVectorClock_RejectsBadCRC pins that a current-format
// blob with a corrupted trailer decodes as nil. The classifier
// (Phase D) treats a missing VectorClock as "unknown ancestor →
// conflict" rather than overwriting good local state with the
// corrupted ancestor.
func TestDecodeVectorClock_RejectsBadCRC(t *testing.T) {
	good := encodeVectorClock(VectorClock{"ABCDE12345": 7, "PEER00002X": 3})
	if decodeVectorClock(good) == nil {
		t.Fatal("baseline: good blob should decode")
	}

	// Flip one bit in the CRC trailer.
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0x01
	if got := decodeVectorClock(bad); got != nil {
		t.Errorf("bad CRC: decoded as %v, want nil", got)
	}

	// Flip a bit in the body — CRC over body changes, so the trailer
	// no longer matches and the blob is rejected.
	bad2 := append([]byte(nil), good...)
	bad2[5] ^= 0x01 // somewhere in device_id bytes
	if got := decodeVectorClock(bad2); got != nil {
		t.Errorf("bit-flip in body: decoded as %v, want nil", got)
	}
}

// TestDecodeVectorClock_LegacyWarnOnce pins the once-per-process
// contract on the legacy-format WARN: a dev DB carrying many pre-CRC
// blobs from D4 commits 2-5 must produce a single WARN on the first
// decode, not one per row. The test installs a counting slog handler,
// decodes a hundred legacy blobs, and asserts the WARN counter
// landed at exactly one.
func TestDecodeVectorClock_LegacyWarnOnce(t *testing.T) {
	// Capture WARN-level "filesync: decoding legacy VectorClock blob"
	// emissions through a custom slog handler. Restore the previous
	// default on cleanup so neighboring tests are not affected.
	var warnCount atomic.Int64
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&legacyWarnCounter{count: &warnCount}))

	resetLegacyVClockWarned()

	legacy := make([]byte, 2+1*(deviceIDChars+8))
	putUint16BE(legacy[0:2], 1)
	copy(legacy[2:2+deviceIDChars], "ABCDE12345")
	putUint64BE(legacy[2+deviceIDChars:2+deviceIDChars+8], 7)

	for range 100 {
		if got := decodeVectorClock(legacy); got == nil {
			t.Fatal("legacy decode returned nil")
		}
	}
	if got := warnCount.Load(); got != 1 {
		t.Errorf("legacy WARN fired %d times across 100 decodes, want 1", got)
	}

	// A second batch with a fresh atomic counter must not re-emit
	// — the once-per-process latch survives until the test resets it.
	warnCount.Store(0)
	for range 50 {
		_ = decodeVectorClock(legacy)
	}
	if got := warnCount.Load(); got != 0 {
		t.Errorf("legacy WARN fired %d times after first round, want 0 (once-per-process)", got)
	}
}

// legacyWarnCounter is a minimal slog.Handler that increments count
// for every record whose message contains "legacy VectorClock blob".
// Other records are accepted but ignored — we are testing the
// counting contract, not log routing.
type legacyWarnCounter struct {
	count *atomic.Int64
	mu    sync.Mutex
	attrs []slog.Attr
}

func (h *legacyWarnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *legacyWarnCounter) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "legacy VectorClock blob") {
		h.count.Add(1)
	}
	return nil
}
func (h *legacyWarnCounter) WithAttrs(a []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &legacyWarnCounter{count: h.count, attrs: append(h.attrs, a...)}
}
func (h *legacyWarnCounter) WithGroup(string) slog.Handler { return h }

// TestProtoRoundTrip_NoCRCOnWire pins the wire-vs-disk split in
// COMMIT-6-SCOPE.md §2: the protobuf-encoded wire form (Counter
// messages in IndexExchange) carries NO CRC bytes. Disk-side CRC
// changes at Phase A do not change the wire format. Mixed-version
// composition (commit-6 peer talking to a commit-5 peer) is
// transparent.
func TestProtoRoundTrip_NoCRCOnWire(t *testing.T) {
	vc := VectorClock{"ABCDE12345": 7, "PEER00002X": 3}

	// Wire form: protobuf-encoded Counter messages.
	counters := vc.toProto()
	wire, err := proto.Marshal(&pb.FileInfo{Version: counters})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Disk form: encodeVectorClock with CRC trailer.
	disk := encodeVectorClock(vc)

	// The CRC trailer is the last 4 bytes of `disk`. Those exact 4
	// bytes must NOT appear at the end of the wire-encoded bytes —
	// the wire never carries the trailer.
	crcTrailer := disk[len(disk)-vclockCRCLen:]
	if bytes.Contains(wire[len(wire)-vclockCRCLen:], crcTrailer) {
		t.Errorf("wire form ends with the disk CRC trailer (%x); the wire must not carry CRC bytes", crcTrailer)
	}

	// Round-trip through wire — no CRC enforcement on the receive side.
	var got pb.FileInfo
	if err := proto.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	decoded := vectorClockFromProto(got.GetVersion())
	if !clocksEqual(decoded, vc) {
		t.Errorf("wire round-trip: got %v, want %v", decoded, vc)
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

	if err := savePeerStatesDB(context.Background(), db, "shared", peers); err != nil {
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
	if err := savePeerStatesDB(context.Background(), db, "shared", first); err != nil {
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
	if err := savePeerStatesDB(context.Background(), db, "shared", second); err != nil {
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

// TestSavePeerSyncOutcome_RoundTrip pins audit §6 commit 6 phase C:
// the combined writer commits BOTH the file-index dirty set AND the
// per-peer map atomically, and a subsequent reload sees the post-tx
// state on every column. Closes the structural side of Gap 2 / Gap 2'
// — the classifier-side test (TestCrashBeforeBaseHashCommit) lands
// in Phase D.
func TestSavePeerSyncOutcome_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	idx := newFileIndex()
	idx.Sequence = 7
	idx.Epoch = "deadbeefcafef00d"
	idx.DeviceID = 0xABCDE12345
	idx.Set("docs/a.txt", FileEntry{
		Size: 11, MtimeNS: 1_700_000_000_000_000_000, SHA256: testHash("a"),
		Sequence: 5, Mode: 0o644,
	})
	idx.Set("docs/b.txt", FileEntry{
		Size: 22, MtimeNS: 1_700_000_000_000_000_001, SHA256: testHash("b"),
		Sequence: 7, Mode: 0o644,
	})

	peers := map[string]PeerState{
		"peer1": {
			LastSeenSequence: 7,
			LastSentSequence: 7,
			LastSync:         time.Unix(0, 1_700_000_001_000_000_000).UTC(),
			LastEpoch:        "deadbeefcafef00d",
			BaseHashes: map[string]Hash256{
				"docs/a.txt": testHash("a"),
				"docs/b.txt": testHash("b"),
			},
		},
		"peer2": {
			LastSeenSequence: 5,
			LastSentSequence: 5,
			LastSync:         time.Unix(0, 1_700_000_002_000_000_000).UTC(),
			BaseHashes: map[string]Hash256{
				"docs/a.txt": testHash("a"),
			},
		},
	}

	if err := savePeerSyncOutcome(context.Background(), db, "f", idx, peers); err != nil {
		t.Fatalf("savePeerSyncOutcome: %v", err)
	}

	gotIdx, err := loadIndexDB(db, "f")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}
	if gotIdx.Sequence != 7 || gotIdx.Epoch != "deadbeefcafef00d" {
		t.Errorf("folder_meta wrong after combined write: seq=%d epoch=%q",
			gotIdx.Sequence, gotIdx.Epoch)
	}
	for _, p := range []string{"docs/a.txt", "docs/b.txt"} {
		if _, ok := gotIdx.Get(p); !ok {
			t.Errorf("file row %s missing after combined write", p)
		}
	}

	gotPeers, err := loadPeerStatesDB(db, "f")
	if err != nil {
		t.Fatalf("loadPeerStatesDB: %v", err)
	}
	if len(gotPeers) != 2 {
		t.Errorf("peers count=%d, want 2", len(gotPeers))
	}
	if h, ok := gotPeers["peer1"].BaseHashes["docs/a.txt"]; !ok || h != testHash("a") {
		t.Errorf("peer1.BaseHashes[docs/a.txt] missing or wrong after combined write")
	}
}

// TestApplyToTx_RollbackLeavesNothing pins the structural co-tx
// invariant: applyIndexToTx and applyPeerStatesToTx write through
// the SAME *sql.Tx, so a rollback discards both halves together. If
// either helper opened its own connection or its own implicit tx,
// the rollback would leave the half it wrote behind. Mental
// mutation: replacing `tx` with `db` on either inner Exec call
// would make this test fail.
func TestApplyToTx_RollbackLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed a known baseline so we can detect any mutation that
	// survived the rollback.
	seedIdx := newFileIndex()
	seedIdx.Sequence = 1
	seedIdx.Set("seed.txt", FileEntry{
		Size: 1, MtimeNS: 1, SHA256: testHash("seed"), Sequence: 1, Mode: 0o644,
	})
	seedPeers := map[string]PeerState{
		"seedpeer": {LastSeenSequence: 1, LastSync: time.Unix(0, 1).UTC()},
	}
	if err := savePeerSyncOutcome(context.Background(), db, "f", seedIdx, seedPeers); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Build a new state both halves would change.
	newIdx := newFileIndex()
	newIdx.Sequence = 2
	newIdx.Set("seed.txt", FileEntry{
		Size: 1, MtimeNS: 1, SHA256: testHash("seed"), Sequence: 1, Mode: 0o644,
	})
	newIdx.Set("postroll.txt", FileEntry{
		Size: 99, MtimeNS: 99, SHA256: testHash("postroll"), Sequence: 2, Mode: 0o644,
	})
	newPeers := map[string]PeerState{
		"postpeer": {LastSeenSequence: 2, LastSync: time.Unix(0, 2).UTC()},
	}

	// Manually drive both apply* helpers through one tx, then rollback.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := applyIndexToTx(tx, "f", newIdx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("applyIndexToTx: %v", err)
	}
	if err := applyPeerStatesToTx(tx, "f", newPeers); err != nil {
		_ = tx.Rollback()
		t.Fatalf("applyPeerStatesToTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Verify the seed survived intact (rollback restored it).
	gotIdx, err := loadIndexDB(db, "f")
	if err != nil {
		t.Fatalf("loadIndexDB: %v", err)
	}
	if gotIdx.Sequence != 1 {
		t.Errorf("Sequence=%d after rollback, want 1 (seed)", gotIdx.Sequence)
	}
	if _, ok := gotIdx.Get("postroll.txt"); ok {
		t.Error("postroll.txt visible after rollback — applyIndexToTx wrote outside the tx")
	}
	if _, ok := gotIdx.Get("seed.txt"); !ok {
		t.Error("seed.txt vanished after rollback — applyIndexToTx mangled the seed")
	}

	gotPeers, err := loadPeerStatesDB(db, "f")
	if err != nil {
		t.Fatalf("loadPeerStatesDB: %v", err)
	}
	if _, ok := gotPeers["postpeer"]; ok {
		t.Error("postpeer visible after rollback — applyPeerStatesToTx wrote outside the tx")
	}
	if _, ok := gotPeers["seedpeer"]; !ok {
		t.Error("seedpeer vanished after rollback — applyPeerStatesToTx mangled the seed")
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

// explainPlanRows runs `EXPLAIN QUERY PLAN <q>` and returns the
// concatenated detail column. Used by the query-plan pinning tests
// below; the format is `id parent notused detail` per SQLite docs.
func explainPlanRows(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		if b.Len() > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}

// TestQueryPlans_NoFullTableScan pins audit Q1/Q2/INV-1: every hot
// peer-facing read MUST use a SQLite index, not a full table scan.
// The retired in-memory `seqIndex` was the previous safety net; now
// the SQL-side `files_by_seq` and `files_by_inode` indexes carry the
// invariant. EXPLAIN QUERY PLAN reports `SCAN` for table scans and
// `SEARCH ... USING INDEX <name>` (or COVERING INDEX) for index seeks
// — we assert the latter and reject the former.
//
// Coverage:
//   - queryFilesSinceSeq → files_by_seq (Q1, INV-1).
//   - queryFilesByPaths → PRIMARY KEY on (folder_id, path) (INV-1).
//   - inode-by-folder lookup → files_by_inode (Q2, lands fully wired
//     in commit 5; this test pins the index is queryable today so the
//     subsequent commit cannot accidentally drop it).
func TestQueryPlans_NoFullTableScan(t *testing.T) {
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed enough rows that the planner will not fall back to a scan
	// because the table is "tiny" — modernc.org/sqlite, like upstream
	// SQLite, sometimes prefers a scan for very small tables.
	idx := newFileIndex()
	idx.Sequence = 1024
	idx.Epoch = "deadbeefcafef00d"
	idx.DeviceID = 0xABCDE12345
	for i := 0; i < 1024; i++ {
		idx.Set(syntheticPath(i), FileEntry{
			Size:     int64(i),
			MtimeNS:  int64(1_700_000_000_000_000_000) + int64(i)*1_000_000,
			SHA256:   hash256FromBytes(syntheticHash(i)),
			Sequence: int64(i + 1),
			Mode:     0o644,
			Inode:    uint64(1_000_000 + i),
		})
	}
	idx.recomputeCache()
	if err := saveIndex(context.Background(), db, "shared", idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	// ANALYZE so the planner has stats; without it SQLite may skip an
	// index even when one exists. The schema applies it on every open
	// in production via PRAGMA optimize at close, but the bench test
	// is short-lived so we run it here.
	if _, err := db.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	cases := []struct {
		name     string
		query    string
		args     []any
		wantIdx  string // substring required in the plan detail
		rejectFS bool   // assert no full SCAN of files
	}{
		{
			name: "queryFilesSinceSeq uses files_by_seq",
			query: `SELECT path, size, mtime_ns, hash, deleted, sequence,
				mode, version, inode, prev_path
				FROM files
				WHERE folder_id=? AND sequence > ?
				ORDER BY sequence`,
			args:     []any{"shared", int64(0)},
			wantIdx:  "files_by_seq",
			rejectFS: true,
		},
		{
			name:  "queryFilesByPaths uses primary key index",
			query: `SELECT path FROM files WHERE folder_id=? AND deleted=0 AND path IN (?,?,?)`,
			args:  []any{"shared", syntheticPath(7), syntheticPath(101), syntheticPath(900)},
			// SQLite materializes the PRIMARY KEY (folder_id, path) as
			// the auto-index `sqlite_autoindex_files_1`; on this driver
			// EXPLAIN QUERY PLAN reports the index by name.
			wantIdx:  "sqlite_autoindex_files_1",
			rejectFS: true,
		},
		{
			name:     "inode lookup uses files_by_inode",
			query:    `SELECT path FROM files WHERE folder_id=? AND inode=?`,
			args:     []any{"shared", int64(1_000_500)},
			wantIdx:  "files_by_inode",
			rejectFS: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainPlanRows(t, db, tc.query, tc.args...)
			if !strings.Contains(plan, tc.wantIdx) {
				t.Errorf("plan does not mention %q\nplan: %s", tc.wantIdx, plan)
			}
			if tc.rejectFS && strings.Contains(plan, "SCAN files") {
				t.Errorf("plan contains full table scan of files\nplan: %s", plan)
			}
		})
	}
}
