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

// applyFolderDBPragmas sets the durability and concurrency knobs that the
// v1 design pins:
//   - journal_mode=WAL for readers that do not block writers.
//   - synchronous=NORMAL — crash-safe under WAL; FULL is overhead without
//     a concrete benefit for our workload.
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
		"PRAGMA synchronous=NORMAL;",
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
  folder_id TEXT    NOT NULL,
  path      TEXT    NOT NULL,
  size      INTEGER NOT NULL,
  mtime_ns  INTEGER NOT NULL,
  hash      BLOB    NOT NULL,
  deleted   INTEGER NOT NULL,
  sequence  INTEGER NOT NULL,
  mode      INTEGER NOT NULL,
  version   BLOB    NOT NULL,
  inode     INTEGER,
  prev_path TEXT,
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
  PRIMARY KEY (folder_id, peer_id)
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
