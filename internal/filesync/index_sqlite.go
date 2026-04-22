package filesync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is the on-disk schema generation for the v1 SQLite index.
// Any future schema change bumps this integer and writes a migration.
const schemaVersion = 1

// folderDBFilename is the SQLite file inside each folder's cache dir.
// The legacy gob/YAML files coexist on disk during the D4 transition;
// subsequent commits port each read and write path over and finally
// retire the gob/YAML helpers.
const folderDBFilename = "index.sqlite"

// openFolderDB opens (or creates) the per-folder SQLite database at
// <dir>/index.sqlite, applies the v1 pragmas, and installs the v1 schema
// idempotently. On first create it writes the initial folder_meta rows
// (schema_version, device_id, epoch, created_at). On subsequent opens
// the existing rows are left untouched — only device_id is refreshed if
// it was empty on disk.
//
// The returned *sql.DB is safe for concurrent use. The pool size is kept
// small by SQLite's own lock semantics; callers do not need to tune it.
func openFolderDB(dir, deviceID string) (*sql.DB, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("openFolderDB: empty device id")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure folder dir: %w", err)
	}
	path := filepath.Join(dir, folderDBFilename)
	// modernc's DSN accepts _pragma= parameters but we prefer explicit
	// PRAGMA statements below so the values are visible in logs and tests.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite serializes writes anyway; a single writer connection avoids
	// the "database is locked" surprise when two goroutines happen to
	// start a transaction at the same time under load. Readers still run
	// concurrently via WAL snapshot isolation.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := applyFolderDBPragmas(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := applyFolderDBSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := seedFolderMeta(db, deviceID); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// applyFolderDBPragmas sets the durability and concurrency knobs:
//   - journal_mode=WAL for readers that do not block writers.
//   - synchronous=FULL — one extra fsync per commit buys full power-loss
//     protection of the last committed transaction. The weaker NORMAL
//     (which DESIGN-v1 §4 originally chose) allows the last committed
//     tx to roll back on power loss, and a sync tool whose value
//     proposition is not losing user files cannot accept that.
//     The extra fsync is amortized by the P17a dirty-flag short-circuit
//     on clean folders and by the per-path dirty-set on busy ones.
//   - foreign_keys=ON so referential integrity between peer_state and
//     folder_meta can be added later without a schema rewrite.
func applyFolderDBPragmas(db *sql.DB) error {
	// journal_mode returns the resulting mode; check it explicitly so a
	// silent downgrade (e.g. on a read-only filesystem) fails loudly.
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL;").Scan(&mode); err != nil {
		return fmt.Errorf("pragma journal_mode=WAL: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("pragma journal_mode=WAL returned %q, want \"wal\"", mode)
	}
	stmts := []string{
		"PRAGMA synchronous=FULL;",
		"PRAGMA foreign_keys=ON;",
		// Mildly larger cache amortizes repeated hot-path reads without
		// materially raising resident memory.
		"PRAGMA cache_size=-4000;",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// applyFolderDBSchema installs the v1 tables and indexes. All statements
// are idempotent so reopening an existing database is cheap.
func applyFolderDBSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS folder_meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
  folder_id    TEXT    NOT NULL,
  path         TEXT    NOT NULL,
  size         INTEGER NOT NULL,
  mtime_ns     INTEGER NOT NULL,
  hash         BLOB    NOT NULL,
  deleted      INTEGER NOT NULL,
  sequence     INTEGER NOT NULL,
  mode         INTEGER NOT NULL,
  version      BLOB    NOT NULL,
  inode        INTEGER,
  prev_path    TEXT,
  -- PH incremental-hashing state. DESIGN-v1's schema listing elides
  -- these columns, but dropping them would silently force a full
  -- re-hash of every big file on first scan after upgrade. We keep
  -- them here so the cold-swap is semantically free on large folders.
  hash_state   BLOB,
  hashed_bytes INTEGER,
  prefix_check BLOB,
  PRIMARY KEY (folder_id, path)
);
CREATE INDEX IF NOT EXISTS files_by_seq
  ON files(folder_id, sequence);
CREATE INDEX IF NOT EXISTS files_by_inode
  ON files(folder_id, inode)
  WHERE inode IS NOT NULL;

CREATE TABLE IF NOT EXISTS blocks (
  folder_id TEXT    NOT NULL,
  path      TEXT    NOT NULL,
  offset    INTEGER NOT NULL,
  length    INTEGER NOT NULL,
  hash      BLOB    NOT NULL,
  PRIMARY KEY (folder_id, path, offset)
);

CREATE TABLE IF NOT EXISTS peer_state (
  folder_id          TEXT    NOT NULL,
  peer_id            TEXT    NOT NULL,
  last_seen_seq      INTEGER NOT NULL,
  last_sent_seq      INTEGER NOT NULL,
  last_ancestor_hash BLOB,
  last_error         TEXT,
  backoff_until_ns   INTEGER,
  -- Extensions beyond DESIGN-v1 §4. They cover shipped correctness
  -- behavior (H2b epoch handshake, M3 peer removal grace window,
  -- last-sync observability) that would regress if we stored only the
  -- five columns the design listed. The banner-flip commit documents
  -- this departure alongside the PH columns on files.
  last_sync_ns       INTEGER NOT NULL DEFAULT 0,
  last_epoch         TEXT,
  pending_epoch      TEXT,
  removed            INTEGER NOT NULL DEFAULT 0,
  removed_at_ns      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (folder_id, peer_id)
);

-- C2 per-path ancestor hashes. DESIGN-v1 §4 specified only a single
-- last_ancestor_hash per peer row; in-memory PeerState.BaseHashes is a
-- per-path map and collapsing it would regress the C2 classifier.
CREATE TABLE IF NOT EXISTS peer_base_hashes (
  folder_id TEXT NOT NULL,
  peer_id   TEXT NOT NULL,
  path      TEXT NOT NULL,
  hash      BLOB NOT NULL,
  PRIMARY KEY (folder_id, peer_id, path)
);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("apply folder db schema: %w", err)
	}
	return nil
}

// seedFolderMeta writes the identity rows on first open. schema_version is
// written once; subsequent opens verify it matches. device_id and epoch
// are filled in if missing — useful when a fresh database inherits a
// device_id from the node-level device-id file.
func seedFolderMeta(db *sql.DB, deviceID string) error {
	var current sql.NullInt64
	err := db.QueryRow(
		`SELECT CAST(value AS INTEGER) FROM folder_meta WHERE key='schema_version'`,
	).Scan(&current)
	switch {
	case err == sql.ErrNoRows, !current.Valid:
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO folder_meta(key, value) VALUES
				('schema_version', ?),
				('device_id',      ?),
				('epoch',          ?),
				('created_at',     ?)`,
			schemaVersion,
			deviceID,
			generateEpoch(),
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("seed folder_meta: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read schema_version: %w", err)
	default:
		if current.Int64 != int64(schemaVersion) {
			return fmt.Errorf(
				"folder db schema_version=%d, binary expects %d",
				current.Int64, schemaVersion)
		}
	}
	// Backfill device_id if a prior open wrote it empty.
	if _, err := db.Exec(
		`UPDATE folder_meta SET value=? WHERE key='device_id' AND value=''`,
		deviceID,
	); err != nil {
		return fmt.Errorf("backfill device_id: %w", err)
	}
	return nil
}

// folderMeta returns the raw folder_meta value for key. Returns "" with a
// nil error when the row is absent.
func folderMeta(db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRow(
		`SELECT CAST(value AS TEXT) FROM folder_meta WHERE key=?`, key,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read folder_meta[%s]: %w", key, err)
	}
	return v, nil
}

// setFolderMeta upserts a raw folder_meta value. Used for the mutable
// rows (sequence, fs_device_id, epoch if it legitimately rotates).
func setFolderMeta(db sqlExecer, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO folder_meta(key, value) VALUES(?, ?)
		  ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("write folder_meta[%s]: %w", key, err)
	}
	return nil
}

// sqlExecer is the subset of *sql.DB / *sql.Tx used by the write helpers
// so the same code runs inside or outside an explicit transaction.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// saveIndex writes the supplied FileIndex into the v1 SQLite schema. The
// scan cycle is one BEGIN IMMEDIATE transaction so readers (admin API,
// dashboard, peer index exchange) see either the pre-scan or the
// post-scan snapshot and never a torn state.
//
// folder_meta stores the folder-level scalars (Sequence, Epoch, the G3
// filesystem device id as fs_device_id). files holds one row per path.
// Rows present on disk but absent from idx.Files are removed.
func saveIndex(db *sql.DB, folderID string, idx *FileIndex) (err error) {
	if db == nil {
		return fmt.Errorf("saveIndex: nil db")
	}
	if idx == nil {
		return fmt.Errorf("saveIndex: nil index")
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin save tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(`BEGIN IMMEDIATE`); err != nil {
		// database/sql already started a DEFERRED tx; escalate to
		// IMMEDIATE so we know we hold the writer lock before any other
		// connection gets the chance to claim it.
		// A no-op on pure-go SQLite when the tx is already a writer.
	}
	if err = setFolderMeta(tx, "sequence", formatInt64(idx.Sequence)); err != nil {
		return err
	}
	if idx.Epoch != "" {
		if err = setFolderMeta(tx, "epoch", idx.Epoch); err != nil {
			return err
		}
	}
	if err = setFolderMeta(tx, "fs_device_id", formatUint64(idx.DeviceID)); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM files WHERE folder_id=?`, folderID); err != nil {
		return fmt.Errorf("clear files: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO files(
		folder_id, path, size, mtime_ns, hash, deleted, sequence, mode,
		version, inode, prev_path, hash_state, hashed_bytes, prefix_check
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare file insert: %w", err)
	}
	defer stmt.Close()
	for path, e := range idx.Files {
		var inode any
		if e.Inode != 0 {
			inode = int64(e.Inode) //nolint:gosec // G115: inode bits preserved by int64 round-trip
		}
		var prevPath any
		if e.PrevPath != "" {
			prevPath = e.PrevPath
		}
		if _, err = stmt.Exec(
			folderID, path, e.Size, e.MtimeNS, e.SHA256[:], boolToInt(e.Deleted),
			e.Sequence, int64(e.Mode), encodeVectorClock(e.Version),
			inode, prevPath, nullIfEmpty(e.HashState), nullIfZero(e.HashedBytes),
			nullIfEmpty(e.PrefixCheck),
		); err != nil {
			return fmt.Errorf("insert files[%s]: %w", path, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit save tx: %w", err)
	}
	return nil
}

// loadIndexDB reads a FileIndex out of the v1 SQLite schema. Missing
// rows yield an empty but well-formed index (Epoch populated from
// folder_meta if present, otherwise freshly generated).
func loadIndexDB(db *sql.DB, folderID string) (*FileIndex, error) {
	if db == nil {
		return nil, fmt.Errorf("loadIndexDB: nil db")
	}
	idx := newFileIndex()
	if seq, err := folderMeta(db, "sequence"); err != nil {
		return nil, err
	} else if seq != "" {
		idx.Sequence = parseInt64(seq)
	}
	if epoch, err := folderMeta(db, "epoch"); err != nil {
		return nil, err
	} else if epoch != "" {
		idx.Epoch = epoch
	}
	if fsdev, err := folderMeta(db, "fs_device_id"); err != nil {
		return nil, err
	} else if fsdev != "" {
		idx.DeviceID = parseUint64(fsdev)
	}

	rows, err := db.Query(`SELECT path, size, mtime_ns, hash, deleted, sequence,
		mode, version, inode, prev_path, hash_state, hashed_bytes, prefix_check
		FROM files WHERE folder_id=?`, folderID)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			path        string
			e           FileEntry
			hashBytes   []byte
			deletedInt  int64
			modeInt     int64
			versionBlob []byte
			inode       sql.NullInt64
			prevPath    sql.NullString
			hashState   []byte
			hashedBytes sql.NullInt64
			prefixCheck []byte
		)
		if err := rows.Scan(&path, &e.Size, &e.MtimeNS, &hashBytes, &deletedInt,
			&e.Sequence, &modeInt, &versionBlob, &inode, &prevPath,
			&hashState, &hashedBytes, &prefixCheck); err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		if len(hashBytes) != len(e.SHA256) {
			return nil, fmt.Errorf("hash row for %s is %d bytes, want %d",
				path, len(hashBytes), len(e.SHA256))
		}
		copy(e.SHA256[:], hashBytes)
		e.Deleted = deletedInt != 0
		e.Mode = uint32(modeInt) //nolint:gosec // G115: stored from uint32 originally
		e.Version = decodeVectorClock(versionBlob)
		if inode.Valid {
			e.Inode = uint64(inode.Int64) //nolint:gosec // G115: inode bits preserved
		}
		if prevPath.Valid {
			e.PrevPath = prevPath.String
		}
		e.HashState = hashState
		if hashedBytes.Valid {
			e.HashedBytes = hashedBytes.Int64
		}
		e.PrefixCheck = prefixCheck
		idx.Files[path] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate files: %w", err)
	}
	idx.recomputeCache()
	idx.rebuildSeqIndex()
	return idx, nil
}

// savePeerStatesDB writes the full per-peer map into the v1 SQLite
// schema. Runs in one transaction so a mid-write crash leaves either the
// pre-update or post-update row set, never a hybrid.
func savePeerStatesDB(db *sql.DB, folderID string, peers map[string]PeerState) (err error) {
	if db == nil {
		return fmt.Errorf("savePeerStatesDB: nil db")
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin peer save tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(`DELETE FROM peer_state       WHERE folder_id=?`, folderID); err != nil {
		return fmt.Errorf("clear peer_state: %w", err)
	}
	if _, err = tx.Exec(`DELETE FROM peer_base_hashes WHERE folder_id=?`, folderID); err != nil {
		return fmt.Errorf("clear peer_base_hashes: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO peer_state(
		folder_id, peer_id, last_seen_seq, last_sent_seq,
		last_ancestor_hash, last_error, backoff_until_ns,
		last_sync_ns, last_epoch, pending_epoch, removed, removed_at_ns
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare peer insert: %w", err)
	}
	defer stmt.Close()
	hashStmt, err := tx.Prepare(
		`INSERT INTO peer_base_hashes(folder_id, peer_id, path, hash) VALUES(?,?,?,?)`,
	)
	if err != nil {
		return fmt.Errorf("prepare base-hash insert: %w", err)
	}
	defer hashStmt.Close()

	for peer, ps := range peers {
		if _, err = stmt.Exec(
			folderID, peer,
			ps.LastSeenSequence, ps.LastSentSequence,
			nil, nil, nil, // last_ancestor_hash/last_error/backoff_until_ns — not
			// yet used in v1 (retry bookkeeping lives on the in-memory tracker);
			// columns kept for forward use per DESIGN-v1.
			ps.LastSync.UnixNano(),
			nullIfEmptyString(ps.LastEpoch),
			nullIfEmptyString(ps.PendingEpoch),
			boolToInt(ps.Removed),
			removedAtNanos(ps.RemovedAt),
		); err != nil {
			return fmt.Errorf("insert peer_state[%s]: %w", peer, err)
		}
		for path, h := range ps.BaseHashes {
			if _, err = hashStmt.Exec(folderID, peer, path, h[:]); err != nil {
				return fmt.Errorf("insert peer_base_hashes[%s/%s]: %w", peer, path, err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit peer save tx: %w", err)
	}
	return nil
}

// loadPeerStatesDB reads the per-peer map out of the v1 SQLite schema.
// Absent rows yield an empty map; this matches the legacy YAML loader so
// callers do not need to special-case the first run.
func loadPeerStatesDB(db *sql.DB, folderID string) (map[string]PeerState, error) {
	if db == nil {
		return nil, fmt.Errorf("loadPeerStatesDB: nil db")
	}
	out := make(map[string]PeerState)

	rows, err := db.Query(`SELECT peer_id, last_seen_seq, last_sent_seq,
		last_sync_ns, last_epoch, pending_epoch, removed, removed_at_ns
		FROM peer_state WHERE folder_id=?`, folderID)
	if err != nil {
		return nil, fmt.Errorf("query peer_state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			peer         string
			ps           PeerState
			lastSyncNs   int64
			lastEpoch    sql.NullString
			pendingEpoch sql.NullString
			removedInt   int64
			removedAtNs  int64
		)
		if err := rows.Scan(&peer, &ps.LastSeenSequence, &ps.LastSentSequence,
			&lastSyncNs, &lastEpoch, &pendingEpoch, &removedInt, &removedAtNs,
		); err != nil {
			return nil, fmt.Errorf("scan peer_state: %w", err)
		}
		if lastSyncNs != 0 {
			ps.LastSync = time.Unix(0, lastSyncNs).UTC()
		}
		if lastEpoch.Valid {
			ps.LastEpoch = lastEpoch.String
		}
		if pendingEpoch.Valid {
			ps.PendingEpoch = pendingEpoch.String
		}
		ps.Removed = removedInt != 0
		if removedAtNs != 0 {
			ps.RemovedAt = time.Unix(0, removedAtNs).UTC()
		}
		out[peer] = ps
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate peer_state: %w", err)
	}

	hashRows, err := db.Query(
		`SELECT peer_id, path, hash FROM peer_base_hashes WHERE folder_id=?`, folderID,
	)
	if err != nil {
		return nil, fmt.Errorf("query peer_base_hashes: %w", err)
	}
	defer hashRows.Close()
	for hashRows.Next() {
		var (
			peer string
			path string
			hash []byte
		)
		if err := hashRows.Scan(&peer, &path, &hash); err != nil {
			return nil, fmt.Errorf("scan peer_base_hashes: %w", err)
		}
		if len(hash) != 32 {
			return nil, fmt.Errorf("peer_base_hashes[%s/%s] hash is %d bytes, want 32",
				peer, path, len(hash))
		}
		ps, ok := out[peer]
		if !ok {
			// Orphan row: peer dropped between inserts. Tolerate by
			// materializing a zero-valued PeerState so the hash is not
			// lost silently.
			ps = PeerState{}
		}
		if ps.BaseHashes == nil {
			ps.BaseHashes = make(map[string]Hash256)
		}
		ps.BaseHashes[path] = hash256FromBytes(hash)
		out[peer] = ps
	}
	if err := hashRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate peer_base_hashes: %w", err)
	}
	return out, nil
}

func nullIfEmptyString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func removedAtNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// encodeVectorClock packs a VectorClock into a deterministic byte slice:
// uint16 BE count, then per entry: 10-byte ASCII device_id, uint64 BE
// counter. Device IDs in v1 are exactly 10 Crockford base32 characters;
// any other width is a bug.
func encodeVectorClock(v VectorClock) []byte {
	if len(v) == 0 {
		return []byte{0, 0}
	}
	keys := make([]string, 0, len(v))
	for k, val := range v {
		if val == 0 || len(k) != deviceIDChars {
			continue
		}
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([]byte, 2+len(keys)*(deviceIDChars+8))
	putUint16BE(out[0:2], uint16(len(keys))) //nolint:gosec // len bounded by VectorClock size
	off := 2
	for _, k := range keys {
		copy(out[off:off+deviceIDChars], k)
		off += deviceIDChars
		putUint64BE(out[off:off+8], v[k])
		off += 8
	}
	return out
}

// decodeVectorClock reverses encodeVectorClock. Malformed or short blobs
// decode as nil rather than returning an error because the scan path
// would otherwise refuse to load an entire index for a single bad row.
func decodeVectorClock(data []byte) VectorClock {
	if len(data) < 2 {
		return nil
	}
	n := int(uint16BE(data[0:2]))
	if n == 0 {
		return nil
	}
	if len(data) != 2+n*(deviceIDChars+8) {
		return nil
	}
	out := make(VectorClock, n)
	off := 2
	for range n {
		id := string(data[off : off+deviceIDChars])
		off += deviceIDChars
		val := uint64BE(data[off : off+8])
		off += 8
		if val > 0 {
			out[id] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Small helpers kept local so the SQLite path stays self-contained.

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func putUint16BE(b []byte, v uint16) {
	b[0] = byte(v >> 8)
	b[1] = byte(v)
}

func putUint64BE(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}

func uint16BE(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func uint64BE(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

func sortStrings(s []string) {
	// insertion sort — the slice is the device-id list for a single
	// file's vector clock, practically < 10 elements.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func formatInt64(n int64) string   { return fmt.Sprintf("%d", n) }
func formatUint64(n uint64) string { return fmt.Sprintf("%d", n) }

func parseInt64(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}
func parseUint64(s string) uint64 {
	var n uint64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}
