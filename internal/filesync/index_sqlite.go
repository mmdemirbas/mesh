package filesync

import (
	"context"
	"database/sql"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// detectSIGKILLLeftover reports whether the folder's SQLite
// `-wal` file exists with non-zero size at the moment of open,
// indicating a previous run did not checkpoint cleanly. Audit
// §6 commit 10 / iter-4 Z8: a SIGKILL'd process leaves WAL
// frames un-checkpointed; on the next open we run a synchronous
// `integrity_check` before going live to close the silent
// live-but-corrupt window between `quick_check` and the async
// `integrity_check`.
//
// Called BEFORE openFolderDB so the writer's setup writes
// (WAL pragma, schema check) don't make the result
// indistinguishable from a normal startup. Returns false on any
// stat error (no WAL file ⇒ clean shutdown; permission denied ⇒
// caller's open will fail loudly anyway).
func detectSIGKILLLeftover(folderCacheDir string) bool {
	walPath := filepath.Join(folderCacheDir, folderDBFilename+"-wal")
	info, err := os.Stat(walPath)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// folderDBDriver is the database/sql driver name used by
// openFolderDB. Defaults to "sqlite" (modernc.org/sqlite, the
// pure-Go driver pinned at v1.49.1). Tests override this to
// "sqlite_faulty" to exercise SQL error paths via the wrapper
// in faulty_driver_test.go. Audit §6 commit 10 / decision §5
// #13: plumbing the driver name lets the H-series tests inject
// SQLITE_FULL / SQLITE_IOERR_FSYNC etc. without changing the
// production code path.
var folderDBDriver = "sqlite"

// schemaVersion is the on-disk schema generation for the v1 SQLite index.
// Any future schema change bumps this integer and writes a migration.
const schemaVersion = 1

// errLegacyIndexRefused is the typed error returned by refuseLegacyIndex
// when any legacy gob / YAML sidecar is present in a folder's cache
// directory. Per DESIGN-v1.md §0 cold-swap posture and
// PERSISTENCE-AUDIT.md §6 commit 2, the v1 binary refuses to start a
// folder whose cache directory carries left-over dev files; recovery is
// to delete them by hand.
var errLegacyIndexRefused = fmt.Errorf("filesync_legacy_index_refused")

// refuseLegacyIndex returns errLegacyIndexRefused (wrapped) when any
// known legacy filename is present in dir. The check is cheap: at most
// a handful of os.Stat calls per folder open. It runs before
// openFolderDB so the SQLite file is not created in a directory the
// operator clearly forgot to clean up.
//
// The legacy filenames cover the gob path's outputs: index.yaml (P17b
// legacy), index.gob, index.gob.prev, peers.yaml, peers.yaml.prev,
// plus .tmp variants left behind by a mid-write crash.
func refuseLegacyIndex(dir string) error {
	legacy := []string{
		"index.yaml",
		"index.yaml.prev",
		"index.yaml.tmp",
		"index.gob",
		"index.gob.prev",
		"index.gob.tmp",
		"peers.yaml",
		"peers.yaml.prev",
		"peers.yaml.tmp",
	}
	for _, name := range legacy {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%w: %s exists; delete legacy state before opening folder",
				errLegacyIndexRefused, p)
		}
	}
	return nil
}

// folderDBFilename is the SQLite file inside each folder's cache dir.
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
	// PRAGMA statements below so the values are visible in logs and
	// tests. _txlock=immediate (D3 / iter-3 review §D) makes db.Begin
	// emit BEGIN IMMEDIATE so writers hold the writer lock before any
	// other connection can claim it; this replaces the prior pattern of
	// running tx.Exec("BEGIN IMMEDIATE") after sql.Begin (which is a
	// no-op or error in many drivers).
	//
	// folderDBDriver is "sqlite" in production; tests can override
	// to "sqlite_faulty" via TestMain or per-test setup to exercise
	// the SQL error paths (audit §6 commit 10 / decision §5 #13).
	db, err := sql.Open(folderDBDriver, path+"?_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite serializes writes anyway; a single writer connection avoids
	// the "database is locked" surprise when two goroutines happen to
	// start a transaction at the same time under load. Readers still run
	// concurrently via WAL snapshot isolation.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// D6 / iter-3 review: cap connection lifetime at 24h instead of
	// SetConnMaxLifetime(0) (= never expire). Connection rotation is
	// cheap; leak containment is free on a weeks-long daemon.
	db.SetConnMaxLifetime(24 * time.Hour)

	if err := applyFolderDBPragmas(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := applyFolderDBSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Audit §6 commit 12 / V1: schema-evolution migration stub.
	// Invoked unconditionally at every folder open so future
	// schema bumps land at a known interception point. v1 → v1
	// is a no-op; the function exists today to prove the
	// invocation site and to give future schema bumps a stable
	// home (no need to weave a new call site through openFolderDB
	// when V2 lands).
	if err := migrateSchema(db, schemaVersion, schemaVersion); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := seedFolderMeta(db, deviceID); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openFolderDBReader opens the per-folder SQLite file as a read-only
// handle for peer-facing reads (decision §5 #10 / iter-3 review §B2 /
// audit §6 commit 4). The reader pool is sized for the expected
// concurrency: maxConn callers can issue overlapping reads through
// WAL snapshot isolation without serializing behind the writer's
// MaxOpenConns=1.
//
// Must be called AFTER openFolderDB so the schema, pragmas, and
// folder_meta rows already exist. Reader-side pragmas are minimal —
// query_only=true forbids accidental writes; mode=ro is a SQLite-DSN
// safety belt; _txlock=deferred avoids the IMMEDIATE writer-lock
// acquisition reads do not need.
//
// maxConn = max(2, n_peers + 3) per the audit. Caller passes the
// peer count; the floor of 2 is a safety belt for folders that have
// not yet resolved peers.
func openFolderDBReader(dir string, maxConn int) (*sql.DB, error) {
	if maxConn < 2 {
		maxConn = 2
	}
	path := filepath.Join(dir, folderDBFilename)
	dsn := path + "?_pragma=query_only(true)&mode=ro&_txlock=deferred"
	db, err := sql.Open(folderDBDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite reader %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxConn)
	db.SetMaxIdleConns(maxConn)
	db.SetConnMaxLifetime(24 * time.Hour) // mirror the writer (D6)
	// Smoke test: if the file is unreadable or the schema is wrong,
	// fail at open rather than at the first peer read.
	if _, err := db.Exec("PRAGMA query_only=true"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma query_only on reader: %w", err)
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
		// Decision §5 #12 (iter-3): mmap_size = 64 MiB. Apple Silicon
		// and Linux desktops have ample VA; 64-bit Windows is fine.
		// Resident memory is bounded by actual page touches, not by
		// the mapping size.
		"PRAGMA mmap_size=67108864;", // 64 * 1024 * 1024
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// migrateSchemaInvocations is a test-only counter incremented by
// migrateSchema each time it is called. The audit (commit 12 / V1)
// asserts the migration hook fires unconditionally at folder open;
// the test reads this counter to prove invocation without coupling
// to internal call sites.
var migrateSchemaInvocations atomic.Int64

// migrateSchema runs the schema-evolution migration ladder from
// `from` to `to`. Audit §6 commit 12 / V1: the function exists at
// v1 as a no-op for v1 → v1 (the only valid transition today)
// and as a structural anchor — when V2 lands, the migration
// arrows go HERE rather than threading new call sites through
// openFolderDB. Idempotent within a version; calling twice with
// the same (from, to) is harmless.
//
// Returns an error if to < from (downgrade is not supported) or
// if to is past the current binary's known schema. The caller
// transitions the folder to FolderDisabled with reason
// `schema_version_mismatch` on either case.
func migrateSchema(db *sql.DB, from, to int) error {
	migrateSchemaInvocations.Add(1)
	if db == nil {
		return fmt.Errorf("migrateSchema: nil db")
	}
	if to < from {
		return fmt.Errorf("migrateSchema: downgrade unsupported (from=%d to=%d)", from, to)
	}
	if to > schemaVersion {
		return fmt.Errorf("migrateSchema: target %d past binary's schema %d", to, schemaVersion)
	}
	// v1 → v1: no-op. Future arrows (v1 → v2, etc.) land here as
	// switch cases:
	//
	//	for v := from; v < to; v++ {
	//	    switch v {
	//	    case 1: ... // v1 → v2
	//	    }
	//	}
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

-- The blocks table from DESIGN-v1 section 4 is intentionally absent.
-- Per audit decision section 5 #7 (iter-3 A6), block hashes are
-- computed on demand by handleBlockSigs from the open file;
-- persisting them would bloat the DB on large media files (a 10 GB
-- MP4 at 128 KB avg = ~80k rows per file) for no measured win.
-- Reopen criterion: cold-cache /blocksigs latency under load, not
-- speculation.

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

// queryFilesSinceSeq powers buildIndexExchange's delta path.
// Returns rows ordered by sequence ascending, restricted to entries
// whose sequence > sinceSeq. Uses the files_by_seq covering index
// (audit Q1 closes when seqIndex retires; the SQL index supplants
// the in-memory secondary index).
//
// The yield callback receives one row per entry; returning false
// stops iteration. Caller must handle the SHA256 byte slice (it is
// re-used between rows; the callback should copy it if it needs to
// retain it).
//
// Use the read-only handle (fs.dbReader) for peer-facing calls so
// concurrent exchanges do not serialize behind the writer.
func queryFilesSinceSeq(ctx context.Context, db *sql.DB, folderID string, sinceSeq int64,
	yield func(path string, e FileEntry) bool,
) error {
	if db == nil {
		return fmt.Errorf("queryFilesSinceSeq: nil db")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	const q = `SELECT path, size, mtime_ns, hash, deleted, sequence,
		mode, version, inode, prev_path
		FROM files
		WHERE folder_id=? AND sequence > ?
		ORDER BY sequence`
	rows, err := db.QueryContext(ctx, q, folderID, sinceSeq)
	if err != nil {
		return fmt.Errorf("query files by sequence: %w", err)
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
		)
		if err := rows.Scan(&path, &e.Size, &e.MtimeNS, &hashBytes, &deletedInt,
			&e.Sequence, &modeInt, &versionBlob, &inode, &prevPath); err != nil {
			return fmt.Errorf("scan files row: %w", err)
		}
		if len(hashBytes) != len(e.SHA256) {
			return fmt.Errorf("hash row for %s is %d bytes, want %d",
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
		if !yield(path, e) {
			return nil
		}
	}
	return rows.Err()
}

// queryFilesByPaths powers the bundle handler's "is this path in the
// index" check. Returns the subset of paths that are present and
// not tombstoned (Deleted=false). Empty input returns empty output.
//
// The query uses an explicit ORD-by-path so result ordering is
// deterministic for tests. Path count is capped at 1000 because
// SQLite's IN-clause has implementation limits and the bundle
// handler validates against maxPaths upstream.
func queryFilesByPaths(ctx context.Context, db *sql.DB, folderID string, paths []string,
) (map[string]struct{}, error) {
	if db == nil {
		return nil, fmt.Errorf("queryFilesByPaths: nil db")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(paths) == 0 {
		return map[string]struct{}{}, nil
	}
	// Build a parameter list of N placeholders. SQLite's default
	// max parameter count is 32766; the bundle handler caps at
	// maxBundlePaths well below that.
	args := make([]any, 0, len(paths)+1)
	args = append(args, folderID)
	placeholders := make([]byte, 0, len(paths)*2)
	for i, p := range paths {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, p)
	}
	q := `SELECT path FROM files WHERE folder_id=? AND deleted=0 AND path IN (` +
		string(placeholders) + `)`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query files by path list: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{}, len(paths))
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan files row: %w", err)
		}
		out[path] = struct{}{}
	}
	return out, rows.Err()
}

// errSchemaVersionMismatch is returned when folder_meta.schema_version
// is non-zero but does not match the binary's expected schemaVersion.
// Maps to FolderDisabled reason `schema_version_mismatch`.
var errSchemaVersionMismatch = fmt.Errorf("schema_version_mismatch")

// errQuickCheckFailed is returned by runQuickCheck when PRAGMA
// quick_check finds gross corruption. Maps to FolderDisabled reason
// `quick_check_failed`. R2 / iter-3 audit §2.2.
var errQuickCheckFailed = fmt.Errorf("quick_check_failed")

// errIntegrityCheckFailed is returned by runIntegrityCheck when
// PRAGMA integrity_check finds subtle corruption that quick_check
// missed. Maps to FolderDisabled reason `integrity_check_failed`.
// Iter-3 audit §2.2 / Gap 5 (iter-2): the two-phase split keeps
// folder open fast (~ms) while still surfacing deep corruption
// asynchronously (~10 MB/s scan, tens of seconds on large DBs).
var errIntegrityCheckFailed = fmt.Errorf("integrity_check_failed")

// runQuickCheck runs PRAGMA quick_check and returns errQuickCheckFailed
// when it reports anything other than "ok". The check runs in
// milliseconds even on large databases — it scans page headers and
// btree structure but does not verify every row. Folder open blocks
// on this; deeper integrity verification runs asynchronously via
// runIntegrityCheck.
func runQuickCheck(db *sql.DB) error {
	rows, err := db.Query("PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("%w: query: %w", errQuickCheckFailed, err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%w: scan: %w", errQuickCheckFailed, err)
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: iterate: %w", errQuickCheckFailed, err)
	}
	if len(results) == 1 && results[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %v", errQuickCheckFailed, results)
}

// runIntegrityCheck runs the full PRAGMA integrity_check and returns
// errIntegrityCheckFailed when it reports anything other than "ok".
// The check is much slower than quick_check (~10 MB/s, tens of
// seconds on a 200 MB DB) and is intended to run on a goroutine
// after folder open. The ctx parameter lets the caller cancel the
// check at shutdown.
func runIntegrityCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("%w: query: %w", errIntegrityCheckFailed, err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%w: scan: %w", errIntegrityCheckFailed, err)
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: iterate: %w", errIntegrityCheckFailed, err)
	}
	if len(results) == 1 && results[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %v", errIntegrityCheckFailed, results)
}

// errDeviceIDMismatch is returned by checkDeviceID when the folder's
// stored device_id differs from the node-level identity. Maps to
// FolderDisabled reason `device_id_mismatch`. Iter-3 review I7,
// audit decision §5 #20.
var errDeviceIDMismatch = fmt.Errorf("device_id_mismatch")

// seedFolderMeta writes the identity rows on first open. schema_version
// is written once and verified on subsequent opens; device_id and epoch
// are inserted only when the row is missing (no silent backfill of an
// empty value — I7 makes that an explicit mismatch on the next open).
//
// The function deliberately does NOT compare device_id; that check
// lives in checkDeviceID and runs from the caller (Node.Start) so the
// caller can route a mismatch to the FolderDisabled state machinery
// instead of failing folder open outright.
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
			return fmt.Errorf("%w: db=%d binary=%d",
				errSchemaVersionMismatch, current.Int64, schemaVersion)
		}
	}
	return nil
}

// checkDeviceID compares folder_meta.device_id against the node-level
// identity. Returns errDeviceIDMismatch (wrapped with both values) if
// they differ. Empty stored value is treated as first-run and accepted
// — seedFolderMeta inserts the node identity on first open, so an
// empty stored value can only occur if a previous open crashed
// between schema creation and the seed insert (extremely rare; we
// tolerate it rather than fail).
//
// I7 / audit decision §5 #20: route the mismatch through the
// FolderDisabled state machinery instead of silently overwriting; the
// pre-I7 backfill silently accepted a rotated identity and corrupted
// every subsequent VectorClock bump.
func checkDeviceID(db *sql.DB, expected string) error {
	stored, err := folderMeta(db, "device_id")
	if err != nil {
		return fmt.Errorf("read device_id: %w", err)
	}
	if stored == "" || stored == expected {
		return nil
	}
	return fmt.Errorf("%w: file=%q db=%q", errDeviceIDMismatch, expected, stored)
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
//
// Per PERSISTENCE-AUDIT.md §2.5 P2 (per-path dirty-set), saveIndex
// writes only the paths the FileIndex reports as dirty since the last
// successful persist, plus the deleted-paths set. The previous
// DELETE+INSERT-everything pattern is gone — the bench gate (§6
// commit 2) showed full reload at 655 ms median, which makes
// per-path UPSERT mandatory for any folder past a few thousand
// entries.
//
// Special case: when MarkAllDirty has been called (initial load,
// scan-rebuild), every path is dirty and the persist writes the
// whole index. The path that previously DELETEd-then-INSERTed every
// row is reachable that way, but only when explicitly requested.
//
// On commit success, callers MUST call idx.ClearDirty() to empty the
// dirty/deleted sets. On commit failure, the sets stay populated so
// the next cycle retries the same rows. saveIndex itself does NOT
// clear them — the caller decides based on commit success.
//
// The ctx parameter is the per-folder writer context. When the
// folder transitions to FolderDisabled (iter-4 Z6 / decision §5 #25),
// the ctx is cancelled and any in-flight tx rolls back. Pass
// context.Background when no folder-level cancellation applies
// (e.g., test setup that opens a DB directly).
func saveIndex(ctx context.Context, db *sql.DB, folderID string, idx *FileIndex) (err error) {
	if db == nil {
		return fmt.Errorf("saveIndex: nil db")
	}
	if idx == nil {
		return fmt.Errorf("saveIndex: nil index")
	}
	if ctx == nil {
		// Tolerate a nil ctx — tests that build folderState directly
		// without wiring writerCtx would otherwise panic inside
		// db.BeginTx. Production callers always pass fs.writerCtx.
		ctx = context.Background()
	}
	// Begin a writer tx. With ?_txlock=immediate (D3) this emits
	// BEGIN IMMEDIATE so we hold the writer lock before any other
	// connection can claim it. The ctx propagates a folder-level
	// cancellation (Z6) so disable() can roll back an in-flight tx.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = applyIndexToTx(tx, folderID, idx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit save tx: %w", err)
	}
	return nil
}

// applyIndexToTx writes the dirty/deleted set from idx into the open
// tx without committing. Extracted from saveIndex (audit §6 commit 6
// phase C) so savePeerSyncOutcome can ride a single tx with both the
// file rows and the peer_state / peer_base_hashes rows. Closes the
// split-write window between saveIndex's commit and the subsequent
// savePeerStatesDB commit (Gap 2 / Gap 2'): a crash after the index
// commit but before the peer-state commit could leave fs.peers with
// stale BaseHashes against a fresh file row, which the Phase D
// classifier would then read as "unknown ancestor → conflict."
func applyIndexToTx(tx *sql.Tx, folderID string, idx *FileIndex) error {
	dirty, deleted := idx.DirtyPaths()
	if err := setFolderMeta(tx, "sequence", formatInt64(idx.Sequence)); err != nil {
		return err
	}
	if idx.Epoch != "" {
		if err := setFolderMeta(tx, "epoch", idx.Epoch); err != nil {
			return err
		}
	}
	if err := setFolderMeta(tx, "fs_device_id", formatUint64(idx.DeviceID)); err != nil {
		return err
	}
	if idx.Path != "" {
		if err := setFolderMeta(tx, "path", idx.Path); err != nil {
			return err
		}
	}
	// UPSERT every dirty path. Audit §6 commit 7 phase E / INV-3:
	// the WHERE excluded.sequence > files.sequence clause ensures
	// no UPSERT silently overwrites a row whose sequence is
	// already at or past the incoming value. With the writer's
	// MaxOpenConns=1 the within-process race is impossible by
	// construction; the WHERE is a defense-in-depth assertion that
	// makes a future code path that opens a parallel connection
	// (or a torn write recovered with a higher row sequence) loud-
	// fail rather than silently regress.
	//
	// "Pending marker" sequence assignment (the second half of
	// the audit's Phase E spec) — replacing pre-assigned sequence
	// values with commit-time stamping — is deferred to a follow-up
	// commit. Today's flow pre-assigns sequence under indexMu just
	// before saveIndex (filesync.go ActionDownload commit callback,
	// scan loop, conflict resolver) and writes folder_meta.sequence
	// inside the same tx. The architectural pending-marker shift is
	// substantial (changes when the in-memory entry is "real") and
	// scoped out of this commit; the WHERE-clause half is the
	// load-bearing protection.
	if len(dirty) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO files(
			folder_id, path, size, mtime_ns, hash, deleted, sequence, mode,
			version, inode, prev_path, hash_state, hashed_bytes, prefix_check
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(folder_id, path) DO UPDATE SET
			size=excluded.size, mtime_ns=excluded.mtime_ns,
			hash=excluded.hash, deleted=excluded.deleted,
			sequence=excluded.sequence, mode=excluded.mode,
			version=excluded.version, inode=excluded.inode,
			prev_path=excluded.prev_path, hash_state=excluded.hash_state,
			hashed_bytes=excluded.hashed_bytes,
			prefix_check=excluded.prefix_check
		WHERE excluded.sequence > files.sequence`)
		if err != nil {
			return fmt.Errorf("prepare file upsert: %w", err)
		}
		defer stmt.Close()
		for path := range dirty {
			e, ok := idx.Get(path)
			if !ok {
				// Path was Set then Deleted before commit — the deleted
				// set carries it; skip the upsert.
				continue
			}
			var inode any
			if e.Inode != 0 {
				inode = int64(e.Inode) //nolint:gosec // G115: inode bits preserved by int64 round-trip
			}
			var prevPath any
			if e.PrevPath != "" {
				prevPath = e.PrevPath
			}
			if _, err := stmt.Exec(
				folderID, path, e.Size, e.MtimeNS, e.SHA256[:], boolToInt(e.Deleted),
				e.Sequence, int64(e.Mode), encodeVectorClock(e.Version),
				inode, prevPath, nullIfEmpty(e.HashState), nullIfZero(e.HashedBytes),
				nullIfEmpty(e.PrefixCheck),
			); err != nil {
				return fmt.Errorf("upsert files[%s]: %w", path, err)
			}
		}
	}
	// DELETE rows for hard-removed paths.
	if len(deleted) > 0 {
		dstmt, err := tx.Prepare(`DELETE FROM files WHERE folder_id=? AND path=?`)
		if err != nil {
			return fmt.Errorf("prepare file delete: %w", err)
		}
		defer dstmt.Close()
		for path := range deleted {
			if _, err := dstmt.Exec(folderID, path); err != nil {
				return fmt.Errorf("delete files[%s]: %w", path, err)
			}
		}
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
		v, perr := parseInt64(seq)
		if perr != nil {
			return nil, fmt.Errorf("folder_meta.sequence: %w", perr)
		}
		idx.Sequence = v
	}
	if epoch, err := folderMeta(db, "epoch"); err != nil {
		return nil, err
	} else if epoch != "" {
		idx.Epoch = epoch
	}
	if fsdev, err := folderMeta(db, "fs_device_id"); err != nil {
		return nil, err
	} else if fsdev != "" {
		v, perr := parseUint64(fsdev)
		if perr != nil {
			return nil, fmt.Errorf("folder_meta.fs_device_id: %w", perr)
		}
		idx.DeviceID = v
	}
	if path, err := folderMeta(db, "path"); err != nil {
		return nil, err
	} else if path != "" {
		idx.Path = path
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
		// Reload from SQLite — bounded by the row count we just
		// queried, so the dirty-set cap is structurally unreachable.
		// Set populates the dirty set even on reload (every Set marks
		// the path dirty), but ClearDirty fires immediately below.
		_ = idx.Set(path, e)
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
func savePeerStatesDB(ctx context.Context, db *sql.DB, folderID string, peers map[string]PeerState) (err error) {
	if db == nil {
		return fmt.Errorf("savePeerStatesDB: nil db")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Z6 / decision §5 #25: writer ctx propagates folder-level
	// cancellation so disable() can roll back an in-flight tx.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin peer save tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = applyPeerStatesToTx(tx, folderID, peers); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit peer save tx: %w", err)
	}
	return nil
}

// applyPeerStatesToTx writes the full per-peer map into the open tx
// without committing. peer_state is rewritten in full alongside
// peer_base_hashes so a crash within the tx leaves either pre-update
// or post-update rows, never a hybrid (audit INV-4 peer-update
// bullet, Gap 2 / Gap 2'). Extracted from savePeerStatesDB so
// savePeerSyncOutcome can wrap both this and applyIndexToTx in a
// single tx.
func applyPeerStatesToTx(tx *sql.Tx, folderID string, peers map[string]PeerState) error {
	if _, err := tx.Exec(`DELETE FROM peer_state       WHERE folder_id=?`, folderID); err != nil {
		return fmt.Errorf("clear peer_state: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM peer_base_hashes WHERE folder_id=?`, folderID); err != nil {
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
		if _, err := stmt.Exec(
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
			if _, err := hashStmt.Exec(folderID, peer, path, h[:]); err != nil {
				return fmt.Errorf("insert peer_base_hashes[%s/%s]: %w", peer, path, err)
			}
		}
	}
	return nil
}

// savePeerSyncOutcome writes the file-index dirty set AND the
// per-peer map in ONE BEGIN IMMEDIATE...COMMIT (audit §6 commit 6
// phase C). Closes Gap 2 / Gap 2': previously persistFolder ran
// saveIndex and savePeerStatesDB as two separate transactions, so
// a crash between them could commit a fresh file row while leaving
// peer_state with stale BaseHashes. The Phase D classifier reads an
// absent BaseHash on a non-first-sync peer as "unknown ancestor →
// conflict," so the split-tx window would translate directly into
// spurious .sync-conflict-* files on the next exchange. Bundling
// both writes into one tx makes the failure mode atomic: either
// both halves land or neither does.
func savePeerSyncOutcome(ctx context.Context, db *sql.DB, folderID string, idx *FileIndex, peers map[string]PeerState) (err error) {
	if db == nil {
		return fmt.Errorf("savePeerSyncOutcome: nil db")
	}
	if idx == nil {
		return fmt.Errorf("savePeerSyncOutcome: nil index")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin peer-sync outcome tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = applyIndexToTx(tx, folderID, idx); err != nil {
		return err
	}
	if err = applyPeerStatesToTx(tx, folderID, peers); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit peer-sync outcome tx: %w", err)
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

// vclockCRCLen is the trailer width on packed VectorClock blobs
// (audit D7, §6 commit 6 phase A). CRC-32/IEEE — same polynomial as
// gzip and SQLite's own page checksum, so future operator tools can
// reuse the standard implementation. Disk-only: the wire form
// (protobuf Counter messages in pb.IndexExchange) carries no CRC.
// See COMMIT-6-SCOPE.md §2 for the wire-vs-disk rationale.
const vclockCRCLen = 4

// encodeVectorClock packs a VectorClock into a deterministic byte
// slice: uint16 BE count, then per entry: 10-byte ASCII device_id,
// uint64 BE counter, then a CRC-32 (IEEE polynomial) trailer over
// all preceding bytes. Device IDs in v1 are exactly 10 Crockford
// base32 characters; any other width is a bug.
//
// The trailer catches torn writes and silent on-disk corruption that
// PRAGMA quick_check at open does not surface (single-row bit flips
// inside a btree page). It MUST be appended before any production
// write path emits the packed form; pre-CRC blobs from earlier
// commits in the D4 cutover series decode with a one-shot warning
// (decodeVectorClock) and upgrade on next write.
func encodeVectorClock(v VectorClock) []byte {
	keys := make([]string, 0, len(v))
	for k, val := range v {
		if val == 0 || len(k) != deviceIDChars {
			continue
		}
		keys = append(keys, k)
	}
	sortStrings(keys)
	n := len(keys)
	out := make([]byte, 2+n*(deviceIDChars+8)+vclockCRCLen)
	putUint16BE(out[0:2], uint16(n)) //nolint:gosec // len bounded by VectorClock size
	off := 2
	for _, k := range keys {
		copy(out[off:off+deviceIDChars], k)
		off += deviceIDChars
		putUint64BE(out[off:off+8], v[k])
		off += 8
	}
	putUint32BE(out[off:off+vclockCRCLen], crc32.ChecksumIEEE(out[:off]))
	return out
}

// legacyVClockWarned latches true the first time decodeVectorClock
// accepts a pre-CRC (legacy) blob. Once-per-process: a dev DB
// carrying many pre-CRC blobs from commits 2-5 produces a single
// WARN, not one per row, so the operator notices the upgrade is in
// flight without log spam. atomic.Bool over sync.Once because tests
// need to reset it (resetLegacyVClockWarned, test-only).
var legacyVClockWarned atomic.Bool

// resetLegacyVClockWarned is for tests only — the production
// contract is "warn once for the lifetime of the process."
func resetLegacyVClockWarned() { legacyVClockWarned.Store(false) }

// decodeVectorClock reverses encodeVectorClock. Returns nil for
// malformed or short blobs (rather than an error) so a single bad
// row cannot block loading an entire index. CRC-mismatched blobs
// also decode as nil — the row's VectorClock becomes "missing,"
// which the classifier handles as "unknown ancestor → conflict"
// (Phase D) rather than silently overwriting good local state with
// corrupted ancestor data.
//
// Pre-CRC (legacy) blobs from earlier D4 cutover commits are
// accepted with a one-shot WARN. The blob upgrades to CRC the next
// time saveIndex writes the row.
func decodeVectorClock(data []byte) VectorClock {
	if len(data) < 2 {
		return nil
	}
	n := int(uint16BE(data[0:2]))
	body := 2 + n*(deviceIDChars+8)
	switch len(data) {
	case body + vclockCRCLen:
		// Current format — verify CRC trailer.
		want := uint32BE(data[body : body+vclockCRCLen])
		got := crc32.ChecksumIEEE(data[:body])
		if want != got {
			return nil
		}
	case body:
		// Legacy format (pre-Phase-A). Accept; emit one-shot WARN
		// so the operator sees the upgrade is in flight.
		if legacyVClockWarned.CompareAndSwap(false, true) {
			slog.Warn("filesync: decoding legacy VectorClock blob without CRC trailer; upgrades to CRC on next write (audit D7)")
		}
	default:
		return nil
	}
	if n == 0 {
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

func putUint32BE(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func uint16BE(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func uint32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

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

func formatInt64(n int64) string   { return strconv.FormatInt(n, 10) }
func formatUint64(n uint64) string { return strconv.FormatUint(n, 10) }

// parseInt64 / parseUint64 are the strict counterparts: garbage in
// folder_meta scalar columns surfaces as an error rather than being
// silently coerced to zero (D1 / iter-3 review §D). A zero
// folder_meta.sequence after a previously-non-zero one would re-run
// the entire index from sequence 1 and confuse every peer with a
// LastSeenSequence mismatch — fail loud instead, let commit 3's
// FolderDisabled scaffold transition the folder to
// `metadata_parse_failed`.
func parseInt64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse int64 %q: %w", s, err)
	}
	return n, nil
}

func parseUint64(s string) (uint64, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse uint64 %q: %w", s, err)
	}
	return n, nil
}
