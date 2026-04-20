package filesync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// defaultMaxIndexFiles is the default cap on tracked files per folder.
// Configurable per-folder via FolderCfg.MaxFiles.
const defaultMaxIndexFiles = 500_000

// errIndexCapExceeded is returned by scanWithStats when the file count
// exceeds the configured cap. Callers must not swap a partial index.
var errIndexCapExceeded = errors.New("folder exceeds max tracked files")

// Hash256 is a SHA-256 digest stored as a fixed-size array.
// Zero value represents an empty/unknown hash.
type Hash256 [32]byte

// IsZero reports whether h is the zero hash (all bytes zero).
func (h Hash256) IsZero() bool { return h == Hash256{} }

// String returns the hex-encoded digest.
func (h Hash256) String() string { return hex.EncodeToString(h[:]) }

// MarshalYAML encodes the hash as a hex string for YAML serialization.
func (h Hash256) MarshalYAML() (any, error) { return h.String(), nil }

// UnmarshalYAML decodes a hex string into the hash.
func (h *Hash256) UnmarshalYAML(node *yaml.Node) error {
	b, err := hex.DecodeString(node.Value)
	if err != nil {
		return fmt.Errorf("invalid sha256 hex %q: %w", node.Value, err)
	}
	if len(b) != 32 {
		return fmt.Errorf("sha256 hex %q: need 32 bytes, got %d", node.Value, len(b))
	}
	copy(h[:], b)
	return nil
}

// hash256FromBytes copies a byte slice into a Hash256.
func hash256FromBytes(b []byte) Hash256 {
	var h Hash256
	copy(h[:], b)
	return h
}

// FileEntry holds metadata for a single tracked file.
type FileEntry struct {
	Size     int64   `yaml:"size"`
	MtimeNS  int64   `yaml:"mtime_ns"`
	SHA256   Hash256 `yaml:"sha256"`
	Deleted  bool    `yaml:"deleted,omitempty"`
	Sequence int64   `yaml:"sequence"`
	Mode     uint32  `yaml:"mode,omitempty"`  // L1: Unix permission bits (e.g., 0644)
	Inode    uint64  `yaml:"inode,omitempty"` // R1 Phase 2: filesystem inode for rename detection; 0 on Windows or when unavailable
	// PrevPath is a single-use hint set by the scan when it detects that a
	// new entry at this path is the same inode as a tombstoned entry at the
	// previous path. Sync consumes this hint to let peers apply a local
	// rename (plus optional /delta against the old content) instead of a
	// full re-download. Cleared on the next scan that re-sees the entry.
	// R1 Phase 2.
	PrevPath string `yaml:"prev_path,omitempty"`

	// C6: per-file vector clock. Keys are device IDs, values are
	// monotonic local write counters. A nil clock is a valid empty
	// clock (dominated by every non-empty clock). See vclock.go and
	// docs/filesync/DESIGN-v1.md §1.
	Version VectorClock `yaml:"version,omitempty"`

	// PH: incremental hashing state for append-only optimization.
	// HashState is the serialized sha256 internal state after hashing
	// HashedBytes bytes. PrefixCheck holds the last prefixCheckSize bytes
	// of the hashed region for boundary verification on the next scan.
	// Only stored for files >= minIncrementalHashSize.
	HashState   []byte `yaml:"hash_state,omitempty"`
	HashedBytes int64  `yaml:"hashed_bytes,omitempty"`
	PrefixCheck []byte `yaml:"prefix_check,omitempty"`
}

// FileIndex is the in-memory index for a single folder.
type FileIndex struct {
	Path     string               `yaml:"path"`
	Sequence int64                `yaml:"sequence"`
	Epoch    string               `yaml:"epoch,omitempty"`     // H2b: random ID, regenerated on index creation
	DeviceID uint64               `yaml:"device_id,omitempty"` // G3: filesystem device ID at first scan
	Files    map[string]FileEntry `yaml:"files"`

	// P18b: cached active (non-deleted) count and total size, maintained
	// incrementally by trackAdd/trackRemove. Avoids O(n) iteration on
	// every sync cycle and admin API call.
	cachedCount int   `yaml:"-"`
	cachedSize  int64 `yaml:"-"`

	// PG: secondary index sorted by Sequence for O(log N + delta) delta
	// exchange. May contain stale entries (path updated with a newer
	// sequence); consumers must verify against Files map. Rebuilt after
	// scan swap and index load; appended to by setEntry.
	seqIndex []seqEntry `yaml:"-"`

	// C6: this node's device ID (10-char Crockford base32). Populated by
	// folderState init from Node.deviceID. Used by scan to bump the
	// per-file VectorClock on local writes. Empty in tests that build a
	// FileIndex directly; bump is skipped in that case.
	selfID string `yaml:"-"`
}

// seqEntry maps a sequence number to a path for the secondary index.
type seqEntry struct {
	seq  int64
	path string
}

// PeerState tracks per-peer sync progress.
type PeerState struct {
	LastSeenSequence int64     `yaml:"last_seen_sequence"`
	LastSentSequence int64     `yaml:"last_sent_sequence"` // our index sequence last sent to this peer
	LastSync         time.Time `yaml:"last_sync"`
	LastEpoch        string    `yaml:"last_epoch,omitempty"`    // H2b: last known epoch of this peer
	PendingEpoch     string    `yaml:"pending_epoch,omitempty"` // H2b: new epoch detected, awaiting diff filter
	Removed          bool      `yaml:"removed,omitempty"`       // M3: peer no longer in config
	RemovedAt        time.Time `yaml:"removed_at,omitempty"`    // M3: when peer was marked removed

	// C2: per-path ancestor hash — the content hash both sides last
	// agreed on. Used by diff() to distinguish "only we modified" from
	// "only they modified" from "both modified". Populated after each
	// successful sync from paths where local and remote hashes match.
	// Absence (nil map or missing key) → fall back to the C1 mtime
	// heuristic.
	BaseHashes map[string]Hash256 `yaml:"base_hashes,omitempty"`
}

// newFileIndex creates an empty index.
func newFileIndex() *FileIndex {
	return &FileIndex{
		Epoch: generateEpoch(),
		Files: make(map[string]FileEntry),
	}
}

// generateEpoch returns 8 random bytes as hex (16 chars).
func generateEpoch() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// clone returns a deep copy of the index. Used by the scan path so WalkDir
// mutates a private copy and readers (admin UI, dashboard) never block on
// the folder's write lock.
func (idx *FileIndex) clone() *FileIndex {
	return idx.cloneInto(nil)
}

// cloneInto returns a deep copy reusing `dst` as the Files backing map.
// If dst is nil or has insufficient capacity, a fresh map is allocated.
// Callers use this with a recycled map from the previous scan to eliminate
// the ~30 MB per-scan allocation on large folders (P18c).
func (idx *FileIndex) cloneInto(dst map[string]FileEntry) *FileIndex {
	if dst == nil {
		dst = make(map[string]FileEntry, len(idx.Files))
	} else {
		clear(dst)
	}
	for k, v := range idx.Files {
		// C6: deep-copy the vector clock so mutations on the cloned
		// index (scan bumps) do not alias the source's Version maps.
		v.Version = v.Version.clone()
		dst[k] = v
	}
	return &FileIndex{
		Path: idx.Path, Sequence: idx.Sequence, Epoch: idx.Epoch, DeviceID: idx.DeviceID,
		Files: dst, cachedCount: idx.cachedCount, cachedSize: idx.cachedSize,
		selfID: idx.selfID,
	}
}

// prevPath returns the backup path for double-write persistence.
func prevPath(path string) string { return path + ".prev" }

// loadIndex reads a persisted index from disk.
// P17b: tries gob files first (fast binary), falls back to YAML (migration).
// H2a: tries both primary and backup, returning whichever has the higher
// sequence. This survives single-file corruption (disk sector error, partial write).
func loadIndex(path string) (*FileIndex, error) {
	gobPath := yamlToGobPath(path)

	// Try gob files first (preferred format).
	primary := tryLoadGobIndex(gobPath)
	backup := tryLoadGobIndex(prevPath(gobPath))

	// Fall back to YAML for migration from older installations.
	if primary == nil {
		primary = tryLoadYAMLIndex(path)
	}
	if backup == nil {
		backup = tryLoadYAMLIndex(prevPath(path))
	}

	var idx *FileIndex
	switch {
	case primary != nil && backup != nil:
		if backup.Sequence > primary.Sequence {
			idx = backup
		} else {
			idx = primary
		}
	case primary != nil:
		idx = primary
	case backup != nil:
		slog.Warn("index loaded from backup (primary corrupted or missing)", "path", path)
		idx = backup
	default:
		// Both missing (first run) → not an error.
		if isNotExist(gobPath) && isNotExist(prevPath(gobPath)) &&
			isNotExist(path) && isNotExist(prevPath(path)) {
			return newFileIndex(), nil
		}
		return nil, fmt.Errorf("all index files unreadable: %s", path)
	}
	// H2b migration: assign an epoch to indexes persisted before epoch support.
	if idx.Epoch == "" {
		idx.Epoch = generateEpoch()
	}
	return idx, nil
}

// tryLoadGobIndex attempts to read and decode a gob index file.
func tryLoadGobIndex(path string) *FileIndex {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from user cache dir
	if err != nil {
		return nil
	}
	idx, err := gobUnmarshalIndex(data)
	if err != nil {
		slog.Warn("corrupt gob index file, skipping", "path", path, "error", err)
		return nil
	}
	return idx
}

// tryLoadYAMLIndex attempts to read and parse a YAML index file (legacy format).
func tryLoadYAMLIndex(path string) *FileIndex {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from user cache dir
	if err != nil {
		return nil
	}
	idx := newFileIndex()
	if err := yaml.Unmarshal(data, idx); err != nil {
		slog.Warn("corrupt yaml index file, skipping", "path", path, "error", err)
		return nil
	}
	return idx
}

// save writes the index to disk with fsync and double-write.
// P17b: uses gob (binary) encoding instead of YAML for ~3-5x faster
// marshal/unmarshal and ~40% smaller output. The path argument still
// ends in .yaml (callers unchanged); we derive .gob paths from it.
// H2a: writes to .prev first, then primary — same crash-safety guarantee.
func (idx *FileIndex) save(path string) error {
	gobPath := yamlToGobPath(path)
	data, err := gobMarshalIndex(idx)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	dir := filepath.Dir(gobPath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create index dir: %w", err)
	}
	if err := writeFileSync(prevPath(gobPath), data); err != nil {
		return fmt.Errorf("write index backup: %w", err)
	}
	if err := writeFileSync(gobPath, data); err != nil {
		return fmt.Errorf("write index primary: %w", err)
	}
	return nil
}

// yamlToGobPath replaces .yaml extension with .gob.
func yamlToGobPath(path string) string {
	return strings.TrimSuffix(path, ".yaml") + ".gob"
}

// gobMarshalIndex encodes a FileIndex to gob bytes.
func gobMarshalIndex(idx *FileIndex) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(idx); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gobUnmarshalIndex decodes a FileIndex from gob bytes.
func gobUnmarshalIndex(data []byte) (*FileIndex, error) {
	idx := newFileIndex()
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(idx); err != nil {
		return nil, err
	}
	return idx, nil
}

// loadPeerStates reads per-peer sync state from disk. H2a: tries both
// primary and backup, preferring the one with a later LastSync timestamp.
func loadPeerStates(path string) (map[string]PeerState, error) {
	primary := tryLoadPeerStates(path)
	backup := tryLoadPeerStates(prevPath(path))

	switch {
	case primary != nil && backup != nil:
		if latestSync(backup).After(latestSync(primary)) {
			return backup, nil
		}
		return primary, nil
	case primary != nil:
		return primary, nil
	case backup != nil:
		slog.Warn("peer state loaded from backup (primary corrupted or missing)", "path", path)
		return backup, nil
	default:
		if isNotExist(path) && isNotExist(prevPath(path)) {
			return make(map[string]PeerState), nil
		}
		return nil, fmt.Errorf("both peer state files unreadable: %s", path)
	}
}

// tryLoadPeerStates attempts to read and parse a single peer state file.
func tryLoadPeerStates(path string) map[string]PeerState {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from user cache dir
	if err != nil {
		return nil
	}
	peers := make(map[string]PeerState)
	if err := yaml.Unmarshal(data, &peers); err != nil {
		slog.Warn("corrupt peer state file, skipping", "path", path, "error", err)
		return nil
	}
	return peers
}

// latestSync returns the most recent LastSync across all peers.
func latestSync(peers map[string]PeerState) time.Time {
	var latest time.Time
	for _, ps := range peers {
		if ps.LastSync.After(latest) {
			latest = ps.LastSync
		}
	}
	return latest
}

// savePeerStates writes peer state to disk with fsync and double-write.
// Both copies are written every time (peer state is small) so they stay
// in sync and either can serve as a recovery source.
func savePeerStates(path string, peers map[string]PeerState) error {
	data, err := yaml.Marshal(peers)
	if err != nil {
		return fmt.Errorf("marshal peers: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create peers dir: %w", err)
	}
	if err := writeFileSync(path, data); err != nil {
		return fmt.Errorf("write peers primary: %w", err)
	}
	if err := writeFileSync(prevPath(path), data); err != nil {
		return fmt.Errorf("write peers backup: %w", err)
	}
	return nil
}

// writeFileSync writes data to path via temp+fsync+rename. The fsync
// ensures data hits stable storage before the rename makes it visible.
func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600) //nolint:gosec // G304
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// isNotExist returns true if the path does not exist.
func isNotExist(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// activeCountAndSize returns the number of non-deleted files and their total size.
// activeCountAndSize returns the cached active file count and total size.
// P18b: O(1) instead of O(n) — maintained incrementally by setEntry.
func (idx *FileIndex) activeCountAndSize() (int, int64) {
	return idx.cachedCount, idx.cachedSize
}

// recomputeCache recalculates cachedCount/cachedSize from scratch.
// Called after bulk operations (load, clone, scan swap) where incremental
// tracking would be error-prone.
func (idx *FileIndex) recomputeCache() {
	var count int
	var size int64
	for _, e := range idx.Files {
		if !e.Deleted {
			count++
			size += e.Size
		}
	}
	idx.cachedCount = count
	idx.cachedSize = size
}

// setEntry updates a file entry and maintains the cached counters
// and the secondary sequence index.
// Must be used instead of direct idx.Files[key] = entry assignment
// in all mutation paths outside of scanWithStats (which bulk-recomputes).
func (idx *FileIndex) setEntry(key string, entry FileEntry) {
	old, exists := idx.Files[key]
	if exists && !old.Deleted {
		idx.cachedCount--
		idx.cachedSize -= old.Size
	}
	if !entry.Deleted {
		idx.cachedCount++
		idx.cachedSize += entry.Size
	}
	idx.Files[key] = entry
	// PG: append to secondary index. Stale entries (same path, older seq)
	// are tolerated and filtered at query time.
	idx.seqIndex = append(idx.seqIndex, seqEntry{seq: entry.Sequence, path: key})
}

// rebuildSeqIndex reconstructs the secondary sequence index from the
// Files map. Called after scan swap and index load.
func (idx *FileIndex) rebuildSeqIndex() {
	idx.seqIndex = make([]seqEntry, 0, len(idx.Files))
	for path, entry := range idx.Files {
		idx.seqIndex = append(idx.seqIndex, seqEntry{seq: entry.Sequence, path: path})
	}
	sort.Slice(idx.seqIndex, func(i, j int) bool {
		return idx.seqIndex[i].seq < idx.seqIndex[j].seq
	})
}

// ScanStats captures measurable work performed by a single scan pass so
// callers can attribute wall time to concrete phases instead of guessing.
// All counters are populated even on error — partial stats are still useful
// for triage. Zero-valued fields mean "phase did not run" (e.g. deletions
// are skipped on WalkDir error).
type ScanStats struct {
	WalkDuration   time.Duration // total time in directory walk (parallel ReadDir + entry processing)
	HashDuration   time.Duration // cumulative time spent in hashFile
	StatDuration   time.Duration // cumulative time spent in d.Info()
	IgnoreDuration time.Duration // cumulative time spent in ignore.shouldIgnore
	DeletionScan   time.Duration // post-walk tombstone pass

	EntriesVisited  int // total WalkDir callbacks
	DirsWalked      int // directories descended (excluding root)
	DirsIgnored     int // directories skipped by ignore rules
	FilesIgnored    int // files skipped by ignore rules
	SymlinksSkipped int
	TempCleaned     int // stale .mesh-tmp-* / .mesh-delta-tmp removed
	FastPathHits    int // stat matched — skipped rehash
	FilesHashed     int
	BytesHashed     int64 // sum of sizes of hashed files
	StatErrors      int
	HashErrors      int
	TocTouSkips     int // files skipped because stat changed during hashing
	Deletions       int // tombstones created in this pass
	RenamesDetected int // R1 Phase 2: tombstone/new-entry pairs paired by inode
}

// hashJob is a file path to hash, sent from the walk to the worker pool.
type hashJob struct {
	absPath     string
	idx         int    // index into the shared results slice
	savedState  []byte // PH: serialized hasher state for incremental hashing
	hashedBytes int64  // PH: bytes covered by savedState
	newSize     int64  // PH: current file size
	prefixCheck []byte // PH: boundary bytes for truncate+regrow detection
}

// hashResult carries the SHA-256 digest or an error from a worker.
type hashResult struct {
	hash        Hash256
	err         error
	hashState   []byte // PH: serialized hasher state after hashing
	prefixCheck []byte // PH: boundary bytes captured after hashing
	inode       uint64 // R1 Phase 2 Step 5: inode observed from the open handle (Windows); 0 on Unix
}

// pendingHash records metadata captured during the walk for a file that
// needs hashing. The hash result is read from the shared results slice
// by index after all workers finish.
type pendingHash struct {
	rel     string
	absPath string
	size    int64
	mtimeNS int64
	mode    uint32
	inode   uint64    // R1 Phase 2: captured during walk; 0 when unavailable
	exists  bool      // true if the file was already in the index
	old     FileEntry // previous index entry (valid only when exists)
}

// scanHashWorkers is the number of parallel hash workers. Capped at 8 to
// avoid saturating disk I/O on spinning disks while still benefiting from
// SSD parallelism and multi-core SHA-256.
var scanHashWorkers = min(runtime.GOMAXPROCS(0), 8)

// scanWalkWorkers is the number of parallel directory readers (P20c).
// Concurrent ReadDir calls help on NFS/FUSE where per-call latency is
// high, and on SSDs where multiple outstanding I/O requests are cheap.
var scanWalkWorkers = min(runtime.GOMAXPROCS(0), 8)

// walkFile is a non-directory entry discovered by the parallel walker.
type walkFile struct {
	rel     string
	absPath string
	name    string
	info    fs.FileInfo // nil when infoErr is set
	infoErr error       // non-nil when d.Info() failed
}

// walkDirResult is the output of reading a single directory in the parallel
// walker. It contains the classified contents: files, subdirectories to
// recurse into, and lightweight per-directory stats.
type walkDirResult struct {
	// dirRel is the relative path of the directory itself (empty for root).
	dirRel string
	// readErr is non-nil when os.ReadDir failed for this directory.
	readErr error
	// subdirs are non-ignored child directories to recurse into.
	subdirs []dirJob
	// files are non-ignored, non-symlink file entries with stat results.
	files []walkFile
	// conflicts are relative paths of Syncthing-style conflict files.
	conflicts []string
	// Per-directory stats accumulated by the worker.
	entriesVisited  int
	dirsIgnored     int
	filesIgnored    int
	dirsWalked      int
	symlinksSkipped int
	tempCleaned     int
	ignoreDuration  time.Duration
	statDuration    time.Duration
}

// dirJob is a directory waiting to be read by a parallel walk worker.
type dirJob struct {
	absPath string
	relDir  string
}

// readDirEntries reads a single directory and classifies its entries.
// It is called from worker goroutines and does not touch shared state.
func readDirEntries(ctx context.Context, job dirJob, ignore *ignoreMatcher, tempCutoff time.Time) walkDirResult {
	res := walkDirResult{dirRel: job.relDir}

	entries, readErr := os.ReadDir(job.absPath)
	if readErr != nil {
		res.readErr = readErr
		return res
	}

	for _, d := range entries {
		if ctx.Err() != nil {
			// Context cancelled mid-directory: discard partial results so the
			// consumer doesn't recurse into a non-deterministic subset of
			// subdirs. Propagate as readErr so the consumer protects children.
			res.subdirs = nil
			res.files = nil
			res.conflicts = nil
			res.readErr = ctx.Err()
			return res
		}
		res.entriesVisited++

		// B17: normalize name to NFC so macOS NFD paths match Windows.
		name := norm.NFC.String(d.Name())
		absPath := filepath.Join(job.absPath, d.Name()) // OS name for I/O
		var rel string
		if job.relDir == "" {
			rel = name
		} else {
			rel = job.relDir + "/" + name
		}

		isDir := d.IsDir()

		// P8: Clean stale temp files inline.
		if !isDir && (strings.HasPrefix(name, ".mesh-tmp-") || strings.Contains(name, ".mesh-delta-tmp-")) {
			if info, infoErr := d.Info(); infoErr == nil && info.ModTime().Before(tempCutoff) {
				if os.Remove(absPath) == nil {
					res.tempCleaned++
				}
			}
			continue
		}

		// Ignore check (read-only, safe for concurrent use).
		ignStart := time.Now()
		skip := ignore.shouldIgnore(rel, isDir)
		res.ignoreDuration += time.Since(ignStart)
		if skip {
			if isDir {
				res.dirsIgnored++
			} else {
				res.filesIgnored++
			}
			continue
		}

		if isDir {
			res.dirsWalked++
			res.subdirs = append(res.subdirs, dirJob{absPath: absPath, relDir: rel})
			continue
		}

		// Skip symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			res.symlinksSkipped++
			continue
		}

		// Stat the file (I/O happens in the worker, not the consumer).
		statStart := time.Now()
		info, infoErr := d.Info()
		res.statDuration += time.Since(statStart)

		wf := walkFile{rel: rel, absPath: absPath, name: name, info: info, infoErr: infoErr}
		res.files = append(res.files, wf)

		if isConflictFile(name) {
			res.conflicts = append(res.conflicts, rel)
		}
	}

	return res
}

// scan walks the folder, updates the index, cleans stale temp files, and
// returns whether any files changed, the active (non-deleted) file count,
// and the number of directories walked (excluding the root and ignored subtrees).
func (idx *FileIndex) scan(ctx context.Context, folderRoot string, ignore *ignoreMatcher) (changed bool, activeCount, dirCount int, err error) {
	changed, activeCount, dirCount, _, _, err = idx.scanWithStats(ctx, folderRoot, ignore, defaultMaxIndexFiles)
	// PL: production callers refresh cachedCount after scan (see filesync.go).
	// The test-only wrapper mirrors that so PL's short-circuit has an
	// accurate activeBefore on the next call.
	idx.recomputeCache()
	return
}

// scanWithStats is scan with detailed per-phase instrumentation. Callers that
// want evidence (runScan) use this; tests keep the simpler signature.
//
//nolint:gocyclo // scan orchestrates parallel walk + hash pipeline; splitting would hurt locality.
func (idx *FileIndex) scanWithStats(ctx context.Context, folderRoot string, ignore *ignoreMatcher, maxFiles int) (changed bool, activeCount, dirCount int, stats ScanStats, conflicts []string, err error) {
	changed = false

	// B10: verify the folder root is accessible before scanning. If the
	// root is temporarily unmounted or missing, WalkDir returns immediately
	// and the empty `seen` map would cause every tracked file to be
	// tombstoned, propagating mass deletion to all peers.
	if _, statErr := os.Stat(folderRoot); statErr != nil {
		return false, 0, 0, stats, nil, fmt.Errorf("folder root inaccessible: %w", statErr)
	}

	// G3: verify the filesystem device ID hasn't changed. A different
	// device ID means the folder was unmounted and remounted on a
	// different filesystem — syncing into it would delete all files
	// (empty folder = mass tombstone) or mix data from two sources.
	if devID, devErr := folderDeviceID(folderRoot); devErr == nil {
		if idx.DeviceID == 0 {
			idx.DeviceID = devID // first scan — record for future checks
		} else if idx.DeviceID != devID {
			return false, 0, 0, stats, nil, fmt.Errorf(
				"folder device ID changed (was %d, now %d) — possible unmount/remount; refusing to scan %s",
				idx.DeviceID, devID, folderRoot)
		}
	}

	seen := make(map[string]struct{}, len(idx.Files)) // P18a: pre-size to avoid rehash cascades
	errorPaths := make(map[string]struct{})           // paths with walk/stat/hash errors — exempt from tombstoning
	tempCutoff := time.Now().Add(-maxTempFileAge)

	// PL: short-circuit deletion detection. If every previously-active file
	// was re-seen on disk, the deletion-detection O(N) loop can be skipped.
	// activeBefore is the pre-scan count (cachedCount is not recomputed until
	// after scan); seenPrevActive is incremented when a path that existed
	// and was not deleted before the scan is added to seen.
	activeBefore := idx.cachedCount
	seenPrevActive := 0

	// PM: lazy-built sorted slice of index keys for prefix search. The
	// error-dir branch used to iterate all of idx.Files to find descendants
	// of a failed directory — O(N × M) across M errored directories. Sort
	// once on the first error (O(N log N)) and binary-search each prefix
	// (O(log N + matches)). The zero-error common case pays nothing.
	var sortedPaths []string
	descendantsOf := func(dir string) []string {
		if sortedPaths == nil {
			sortedPaths = make([]string, 0, len(idx.Files))
			for rel := range idx.Files {
				sortedPaths = append(sortedPaths, rel)
			}
			sort.Strings(sortedPaths)
		}
		prefix := dir + "/"
		start := sort.SearchStrings(sortedPaths, prefix)
		end := start
		for end < len(sortedPaths) && strings.HasPrefix(sortedPaths[end], prefix) {
			end++
		}
		return sortedPaths[start:end]
	}

	// P20a: parallel hash worker pool. Files that miss the fast path
	// (stat changed) are sent to workers; results are drained after the walk.
	hashCh := make(chan hashJob, 64)
	var hashResults []hashResult // pre-allocated after walk, indexed by hashJob.idx
	var hashWg sync.WaitGroup
	for range scanHashWorkers {
		hashWg.Add(1)
		go func() {
			defer hashWg.Done()
			for job := range hashCh {
				h, st, pc, ino, hErr := hashFileIncremental(job.absPath, job.savedState, job.hashedBytes, job.newSize, job.prefixCheck)
				hashResults[job.idx] = hashResult{hash: h, err: hErr, hashState: st, prefixCheck: pc, inode: ino}
			}
		}()
	}
	var pending []pendingHash

	// P20c: parallel directory walk. Worker goroutines read directories
	// concurrently; a single consumer goroutine (below) processes the
	// results and updates all shared state (seen, errorPaths, idx.Files).
	walkStart := time.Now()
	resultCh := make(chan walkDirResult, scanWalkWorkers*2)
	var outstanding sync.WaitGroup

	// walkWorker reads directories from the sem-bounded pool and sends results.
	sem := make(chan struct{}, scanWalkWorkers)
	walkOne := func(job dirJob) {
		sem <- struct{}{} // acquire
		dr := readDirEntries(ctx, job, ignore, tempCutoff)
		<-sem // release
		resultCh <- dr
	}

	// Seed with root directory.
	outstanding.Add(1)
	go walkOne(dirJob{absPath: folderRoot, relDir: ""})

	// Closer: when all directories have been processed, close the channel.
	go func() {
		outstanding.Wait()
		close(resultCh)
	}()

	// Consumer: process results and submit subdirectory jobs. All shared
	// state mutations (seen, errorPaths, idx.Files, pending) happen here.
	capExceeded := false
	for dr := range resultCh {
		// Submit subdirectories BEFORE calling Done, so outstanding never
		// hits zero prematurely.
		for _, sub := range dr.subdirs {
			outstanding.Add(1)
			go walkOne(sub)
		}
		outstanding.Done()

		// Merge per-directory stats.
		stats.EntriesVisited += dr.entriesVisited
		stats.DirsIgnored += dr.dirsIgnored
		stats.FilesIgnored += dr.filesIgnored
		stats.DirsWalked += dr.dirsWalked
		stats.SymlinksSkipped += dr.symlinksSkipped
		stats.TempCleaned += dr.tempCleaned
		stats.IgnoreDuration += dr.ignoreDuration
		stats.StatDuration += dr.statDuration
		dirCount += dr.dirsWalked

		// Handle directory read errors.
		if dr.readErr != nil {
			// Context cancellation → propagate as scan error, don't treat
			// as a directory I/O failure.
			if ctx.Err() != nil {
				err = ctx.Err()
				continue
			}
			// H1: the directory was unreadable. Protect it and all known
			// descendants from tombstoning. PM: use descendantsOf for an
			// O(log N + matches) lookup instead of O(N) per error.
			if dr.dirRel != "" {
				if _, already := seen[dr.dirRel]; !already {
					if e, ok := idx.Files[dr.dirRel]; ok && !e.Deleted {
						seenPrevActive++
					}
				}
				seen[dr.dirRel] = struct{}{}
				errorPaths[dr.dirRel] = struct{}{}
				for _, child := range descendantsOf(dr.dirRel) {
					if _, already := seen[child]; !already {
						if e := idx.Files[child]; !e.Deleted {
							seenPrevActive++
						}
					}
					seen[child] = struct{}{}
					errorPaths[child] = struct{}{}
				}
			}
			stats.StatErrors++
			continue
		}

		// Process file entries.
		conflicts = append(conflicts, dr.conflicts...)

		for _, wf := range dr.files {
			if capExceeded {
				break
			}
			if len(seen) >= maxFiles {
				capExceeded = true
				err = errIndexCapExceeded
				break
			}
			existing, exists := idx.Files[wf.rel]
			if _, already := seen[wf.rel]; !already && exists && !existing.Deleted {
				seenPrevActive++ // PL
			}
			seen[wf.rel] = struct{}{}

			if wf.infoErr != nil {
				stats.StatErrors++
				errorPaths[wf.rel] = struct{}{}
				continue
			}
			mtimeNS := wf.info.ModTime().UnixNano()
			size := wf.info.Size()
			mode := uint32(wf.info.Mode().Perm())
			inode := inodeOf(wf.info)

			// R1 Phase 2 Step 5: force hash phase when the entry has no
			// recorded inode and the walk phase did not produce one
			// (Windows migration from pre-Step-5 indexes). The hash phase
			// opens the file and can extract the NT file index via
			// GetFileInformationByHandle — fast path alone cannot
			// backfill on Windows.
			needInodeBackfill := exists && !existing.Deleted && existing.Inode == 0 && inode == 0

			// Fast path: skip hashing if size and mtime are unchanged.
			// Direct idx.Files assignment OK here — scanWithStats bulk-rebuilds
			// seqIndex and cachedCount/cachedSize at the end.
			if !needInodeBackfill && exists && !existing.Deleted && existing.Size == size && existing.MtimeNS == mtimeNS {
				dirty := false
				if existing.Mode != mode {
					existing.Mode = mode
					dirty = true
				}
				// R1 Phase 2: backfill inode when a previous scan had no
				// value (migration from pre-inode indexes) or when the
				// inode changed silently (e.g. restore from backup).
				// Skip when the current scan could not observe an inode
				// (inode == 0) so we do not clobber a known-good value.
				if inode != 0 && existing.Inode != inode {
					existing.Inode = inode
					dirty = true
				}
				// R1 Phase 2: the rename hint is single-use. If this entry
				// is being re-seen at the same path in a later scan, the
				// rename has already been sent; clearing now prevents a
				// stale hint from leaking to peers joining later.
				if existing.PrevPath != "" {
					existing.PrevPath = ""
					dirty = true
				}
				if dirty {
					idx.Files[wf.rel] = existing
				}
				stats.FastPathHits++
				continue
			}

			// Collect for hash pool — submitted after the walk loop to
			// avoid deadlock: sending to hashCh here would block the
			// consumer that drains resultCh, stalling walk workers.
			pending = append(pending, pendingHash{
				rel: wf.rel, absPath: wf.absPath,
				size: size, mtimeNS: mtimeNS, mode: mode, inode: inode,
				exists: exists, old: existing,
			})
		}
	}
	stats.WalkDuration = time.Since(walkStart)

	// F4: pre-allocate results slice so workers write by index instead
	// of allocating a channel per file. Safe because:
	// 1. The slice variable is assigned here before any sends to hashCh;
	//    the channel send/receive provides happens-before so workers see
	//    the initialized slice.
	// 2. Each worker writes to a distinct index (no element contention).
	// 3. The consumer reads only after hashWg.Wait().
	hashResults = make([]hashResult, len(pending))

	// P20a: submit all hash jobs now that the walk is complete and
	// resultCh is drained. This avoids the deadlock where sending to
	// hashCh blocks the only consumer of resultCh.
	for i := range pending {
		p := pending[i]
		job := hashJob{absPath: p.absPath, idx: i, newSize: p.size}
		// PH: pass saved hash state for incremental hashing.
		if p.exists && !p.old.Deleted && len(p.old.HashState) > 0 && p.size > p.old.HashedBytes {
			job.savedState = p.old.HashState
			job.hashedBytes = p.old.HashedBytes
			job.prefixCheck = p.old.PrefixCheck
		}
		hashCh <- job
	}

	// P20a: close the hash channel and wait for all workers to finish.
	close(hashCh)
	hashWg.Wait()

	// P20a: drain hash results and update index sequentially.
	hashDrainStart := time.Now()
	for i, p := range pending {
		r := hashResults[i]
		if r.err != nil {
			stats.HashErrors++
			errorPaths[p.rel] = struct{}{}
			continue
		}
		stats.FilesHashed++
		stats.BytesHashed += p.size

		// B11: TOCTOU guard — if the file was modified during hashing,
		// the hash corresponds to a partially-modified file. Discard it;
		// the next scan will re-hash the stable version.
		postInfo, postErr := os.Stat(p.absPath)
		if postErr != nil || postInfo.ModTime().UnixNano() != p.mtimeNS || postInfo.Size() != p.size {
			stats.TocTouSkips++
			continue
		}

		// R1 Phase 2 Step 5: prefer the walk-phase inode (Unix stat), but
		// fall back to the hash-phase inode (Windows GetFileInformationByHandle)
		// when the walk could not extract one. Zero stays zero when neither
		// path produced a value.
		effectiveInode := p.inode
		if effectiveInode == 0 {
			effectiveInode = r.inode
		}

		if p.exists && !p.old.Deleted && p.old.SHA256 == r.hash {
			// Content identical despite stat change (e.g., touch, chmod). Update stat only.
			entry := p.old
			entry.MtimeNS = p.mtimeNS
			entry.Size = p.size
			entry.Mode = p.mode
			entry.HashState = r.hashState
			entry.HashedBytes = p.size
			entry.PrefixCheck = r.prefixCheck
			if effectiveInode != 0 {
				entry.Inode = effectiveInode
			}
			// R1 Phase 2: single-use rename hint cleared on re-seen entry.
			entry.PrevPath = ""
			idx.Files[p.rel] = entry
			continue
		}

		idx.Sequence++
		// C6: this is a local write — bump our counter in the prior
		// vector (nil when the path is new). Skipped when selfID is
		// empty (tests that construct FileIndex directly).
		var version VectorClock
		if idx.selfID != "" {
			version = p.old.Version.bump(idx.selfID)
		}
		idx.Files[p.rel] = FileEntry{
			Size:        p.size,
			MtimeNS:     p.mtimeNS,
			SHA256:      r.hash,
			Sequence:    idx.Sequence,
			Mode:        p.mode,
			Inode:       effectiveInode,
			Version:     version,
			HashState:   r.hashState,
			HashedBytes: p.size,
			PrefixCheck: r.prefixCheck,
		}
		changed = true
	}
	stats.HashDuration = time.Since(hashDrainStart)

	if err != nil {
		return changed, len(seen), dirCount, stats, conflicts, fmt.Errorf("scan %s: %w", folderRoot, err)
	}

	// M1: TOCTOU guard — if the walk found zero files but the index has
	// entries, the folder root may have been unmounted between the pre-walk
	// stat and the WalkDir. Re-stat to distinguish a genuinely empty folder
	// from a vanished mount point. Without this, all tracked files would be
	// tombstoned and the deletions propagated to every peer.
	if len(seen) == 0 && len(idx.Files) > 0 {
		if _, statErr := os.Stat(folderRoot); statErr != nil {
			return false, 0, 0, stats, nil, fmt.Errorf("folder root vanished during scan: %w", statErr)
		}
		// Root still exists and is accessible — the folder is legitimately
		// empty. Proceed with tombstoning.
	}

	// R1 Phase 2: pair tombstone candidates with newly-hashed entries
	// sharing the same inode, so the tombstone pass can set PrevPath on
	// the new entry and peers can apply a local rename instead of a
	// re-download.
	//
	// A rename candidate is a file in idx.Files that is NOT in `seen`
	// (so the tombstone pass would normally mark it deleted) with a
	// matching inode in the freshly-hashed `pending` set.
	//
	// Only entries hashed this scan can be rename targets — a path that
	// already existed with the same inode is not a rename, it is the
	// untouched file. This is cheap because hashed entries are in
	// `pending`; we don't need to scan idx.Files.
	//
	// Inode reuse after deletion can cause a false positive; the receiver
	// verifies the prev-path content before applying the rename locally
	// and falls back to a full download on mismatch.
	inodeToNewPath := make(map[uint64]string)
	for i := range pending {
		p := pending[i]
		// Skip entries that already existed before this scan — only
		// genuinely new paths are rename targets. (Existing-path hash
		// changes are edits, not renames.)
		if p.exists && !p.old.Deleted {
			continue
		}
		// Skip entries that failed to hash.
		if _, already := errorPaths[p.rel]; already {
			continue
		}
		// Record only if the hashed path is actually in idx.Files now
		// (drain succeeded and TOCTOU didn't reject it).
		entry, ok := idx.Files[p.rel]
		if !ok {
			continue
		}
		// R1 Phase 2 Step 5: the drainer above already reconciled the
		// walk-phase inode (Unix) with the hash-phase inode (Windows)
		// into entry.Inode. Read from there rather than p.inode so
		// Windows rename detection picks up files hashed via the
		// open-handle path.
		if entry.Inode == 0 {
			continue
		}
		inodeToNewPath[entry.Inode] = p.rel
	}

	delStart := time.Now()
	// B10/M2: per-file error suppression with bulk safety net.
	//
	// Individual files that errored during walk/stat/hash are in errorPaths
	// and have been added to seen, so they won't be tombstoned. For the
	// remaining files, tombstone detection runs normally.
	//
	// Bulk safety net: if errors exceed 10% of tracked files or 100 absolute,
	// suppress all tombstones — the scan is likely seeing a systemic failure
	// (NFS flap, permission reset) rather than individual file issues.
	totalErrors := len(errorPaths)
	// Use live file count from the walk (len(seen)) as denominator so old
	// tombstones don't inflate the threshold. This is O(1) — the seen map
	// was populated during the walk.
	liveFiles := len(seen)
	bulkFailure := totalErrors > 100 || (liveFiles > 0 && totalErrors*10 > liveFiles)
	if bulkFailure {
		slog.Warn("scan had bulk errors, suppressing all deletion detection",
			"folder", folderRoot,
			"error_paths", totalErrors,
			"hash_errors", stats.HashErrors,
			"stat_errors", stats.StatErrors)
	} else {
		if totalErrors > 0 {
			slog.Warn("scan had errors on individual files, suppressing their tombstones only",
				"folder", folderRoot,
				"error_paths", totalErrors,
				"hash_errors", stats.HashErrors,
				"stat_errors", stats.StatErrors)
		}
		// PL: short-circuit when every previously-active file was re-seen.
		// seenPrevActive counts paths added to seen that were active before
		// the scan. When cachedCount is accurate, seenPrevActive == activeBefore
		// proves no deletions. Any other relation (under/over) means either
		// a real deletion or a stale cache — fall through to the full loop.
		if seenPrevActive != activeBefore {
			// Mark deletions: entries in index not seen on disk.
			for rel, entry := range idx.Files {
				if entry.Deleted {
					continue
				}
				if _, ok := seen[rel]; !ok {
					// R1 Phase 2: before tombstoning, check whether this
					// file was renamed — its inode is present on a new
					// path hashed in this scan. Tag the new entry with
					// PrevPath so the sync cycle can propagate the hint
					// to peers. Still emit the tombstone so capability-
					// less peers drop the old path correctly.
					if entry.Inode != 0 {
						if newRel, found := inodeToNewPath[entry.Inode]; found && newRel != rel {
							if newEntry, ok := idx.Files[newRel]; ok {
								newEntry.PrevPath = rel
								idx.Files[newRel] = newEntry
								stats.RenamesDetected++
							}
						}
					}
					idx.Sequence++
					entry.Deleted = true
					entry.MtimeNS = time.Now().UnixNano() // deletion time, not last-modification time
					entry.Sequence = idx.Sequence
					// C6: local deletion is a new write — bump self.
					if idx.selfID != "" {
						entry.Version = entry.Version.bump(idx.selfID)
					}
					idx.Files[rel] = entry
					changed = true
					stats.Deletions++
				}
			}
		}
	}
	stats.DeletionScan = time.Since(delStart)

	sort.Strings(conflicts)
	// P7: len(seen) is the active file count — computed during walk, not a separate loop.
	return changed, len(seen), dirCount, stats, conflicts, nil
}

// F12: pool sha256 hashers to avoid 310k+ allocations on cold scans.
var sha256Pool = sync.Pool{
	New: func() any { return sha256.New() },
}

// minIncrementalHashSize is the minimum file size for storing hash state.
// Files below this threshold are fully re-hashed every scan (cheap enough).
const minIncrementalHashSize = 1 << 20 // 1 MB

// hashFile computes the SHA-256 digest of a file.
func hashFile(path string) (Hash256, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from scan walk within a user-configured folder
	if err != nil {
		return Hash256{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	defer sha256Pool.Put(h)
	if _, err := io.Copy(h, f); err != nil {
		return Hash256{}, err
	}
	return hash256FromBytes(h.Sum(nil)), nil
}

// prefixCheckSize is the number of bytes verified at the boundary before
// using incremental hashing. This catches truncate+regrow scenarios where
// the file prefix was silently replaced.
const prefixCheckSize = 4096

// hashFileIncremental computes the SHA-256 digest, optionally resuming from
// a previously saved hasher state. Returns the hex digest and the serialized
// hasher state for the next incremental run.
//
// Incremental path is taken when: (1) savedState is non-empty, (2) the file
// grew (newSize > hashedBytes), (3) newSize >= minIncrementalHashSize, and
// (4) the boundary region (last prefixCheckSize bytes of the previously-hashed
// content) matches the saved prefixCheck. Condition (4) catches truncate+regrow
// where the file was rewritten with different content.
func hashFileIncremental(path string, savedState []byte, hashedBytes, newSize int64, prefixCheck []byte) (digest Hash256, state []byte, newPrefixCheck []byte, inode uint64, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from scan walk within a user-configured folder
	if err != nil {
		return Hash256{}, nil, nil, 0, err
	}
	defer func() { _ = f.Close() }()

	// R1 Phase 2 Step 5: Windows cannot get the NT file index during the
	// walk phase, so extract it now from the open handle. inodeFromFile
	// is a no-op on Unix (the walk already captured it via stat).
	inode = inodeFromFile(f)

	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	defer sha256Pool.Put(h)

	incremental := len(savedState) > 0 && newSize > hashedBytes && newSize >= minIncrementalHashSize && len(prefixCheck) > 0
	if incremental {
		// Verify the boundary region before trusting saved state.
		checkOffset := hashedBytes - int64(len(prefixCheck))
		if checkOffset < 0 {
			checkOffset = 0
		}
		checkLen := int(hashedBytes - checkOffset)
		buf := make([]byte, checkLen)
		n, readErr := f.ReadAt(buf, checkOffset)
		if readErr != nil || n != checkLen || !bytes.Equal(buf[:n], prefixCheck) {
			incremental = false // prefix changed — fall back to full hash
		}
	}

	if incremental {
		if um, ok := h.(encoding.BinaryUnmarshaler); ok {
			if restoreErr := um.UnmarshalBinary(savedState); restoreErr == nil {
				if _, seekErr := f.Seek(hashedBytes, io.SeekStart); seekErr == nil {
					if _, cpErr := io.Copy(h, f); cpErr != nil {
						return Hash256{}, nil, nil, 0, cpErr
					}
					digest = hash256FromBytes(h.Sum(nil))
					if m, mok := h.(encoding.BinaryMarshaler); mok {
						state, _ = m.MarshalBinary()
					}
					newPrefixCheck = capturePrefixCheck(f, newSize)
					return digest, state, newPrefixCheck, inode, nil
				}
			}
		}
		// Fall through to full hash on any restore/seek failure.
		h.Reset()
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return Hash256{}, nil, nil, 0, seekErr
		}
	}

	// Full hash.
	if _, err := io.Copy(h, f); err != nil {
		return Hash256{}, nil, nil, 0, err
	}
	digest = hash256FromBytes(h.Sum(nil))

	// Save state for files above the threshold.
	if newSize >= minIncrementalHashSize {
		if m, ok := h.(encoding.BinaryMarshaler); ok {
			state, _ = m.MarshalBinary()
		}
		newPrefixCheck = capturePrefixCheck(f, newSize)
	}
	return digest, state, newPrefixCheck, inode, nil
}

// capturePrefixCheck reads the last prefixCheckSize bytes of the file (or
// all of it if shorter) for use as a boundary verification on the next
// incremental hash.
func capturePrefixCheck(f *os.File, size int64) []byte {
	checkLen := int64(prefixCheckSize)
	if size < checkLen {
		checkLen = size
	}
	buf := make([]byte, checkLen)
	n, err := f.ReadAt(buf, size-checkLen)
	if err != nil || int64(n) != checkLen {
		return nil
	}
	return buf
}

// DiffAction represents what to do with a file during sync.
type DiffAction int

const (
	ActionDownload DiffAction = iota // Pull file from peer
	ActionConflict                   // Both sides modified
	ActionDelete                     // Delete local file
)

// DiffEntry describes a single file that needs action during sync.
type DiffEntry struct {
	Path           string
	Action         DiffAction
	RemoteHash     Hash256
	RemoteSize     int64
	RemoteMtime    int64
	RemoteMode     uint32 // L1: file permission bits from remote
	RemoteSequence int64  // B9: track for safe LastSeenSequence advancement
	// RemotePrevPath is the sender's rename hint when Action==ActionDownload:
	// the peer observed the same inode at this prior path. Empty when no
	// hint is present. R1 Phase 2 receiver pairs this with a matching
	// ActionDelete to perform a local rename plus /delta instead of a full
	// re-download. Never set on non-download actions.
	RemotePrevPath string
	// C6: the peer's vector clock for this path. Receive-side handlers
	// adopt this into FileEntry.Version after the local write is durable
	// so subsequent compareClocks calls reflect what we observed. Nil
	// when the peer has no clock (pre-C6 legacy or empty).
	RemoteVersion VectorClock
}

// diff compares the local index with a remote index and produces a list of
// actions needed to bring the local side up to date.
//
// C6 semantics drive classification when both sides carry non-empty vector
// clocks:
//   - local dominates remote → skip (our write is newer).
//   - remote dominates local → Download (or Delete for remote tombstone).
//   - concurrent → Conflict; the C1 mtime tiebreak later picks a winner
//     and preserves the loser as .sync-conflict-*.
//
// When either side has an empty clock — legacy indexes loaded from pre-C6
// persistence, or an entry that has not been re-scanned yet — diff() falls
// back to the C1/C2 mtime-and-ancestor heuristic. The fallback is bug-
// compatible with pre-C6 behavior so rolling upgrades can't deadlock.
//
// lastSeenSeq is the highest remote sequence we've previously processed
// from this peer; it filters out already-seen remote entries.
// lastSyncNS is PeerState.LastSync in Unix nanoseconds. It drives the C1
// mtime-vs-last-sync fallback when no ancestor hash is known for a path.
// baseHashes is PeerState.BaseHashes, the per-path ancestor hash we and
// the peer last agreed on.
func (idx *FileIndex) diff(remote *FileIndex, lastSeenSeq int64, lastSyncNS int64, baseHashes map[string]Hash256, direction string) []DiffEntry {
	canReceive := direction == "send-receive" || direction == "receive-only" || direction == "dry-run"
	if !canReceive {
		return nil
	}

	// locallyModified reports whether our copy of path has changed since
	// we last agreed on content with this peer. The ancestor hash is the
	// definitive signal; mtime-vs-lastSync is the fallback.
	locallyModified := func(path string, lEntry FileEntry) bool {
		if ancestor, ok := baseHashes[path]; ok {
			return lEntry.SHA256 != ancestor
		}
		return lEntry.MtimeNS > lastSyncNS
	}

	var actions []DiffEntry

	downloadEntry := func(path string, rEntry FileEntry) DiffEntry {
		return DiffEntry{
			Path:           path,
			Action:         ActionDownload,
			RemoteHash:     rEntry.SHA256,
			RemoteSize:     rEntry.Size,
			RemoteMtime:    rEntry.MtimeNS,
			RemoteMode:     rEntry.Mode,
			RemoteSequence: rEntry.Sequence,
			RemotePrevPath: rEntry.PrevPath,
			RemoteVersion:  rEntry.Version,
		}
	}
	conflictEntry := func(path string, rEntry FileEntry) DiffEntry {
		return DiffEntry{
			Path:           path,
			Action:         ActionConflict,
			RemoteHash:     rEntry.SHA256,
			RemoteSize:     rEntry.Size,
			RemoteMtime:    rEntry.MtimeNS,
			RemoteMode:     rEntry.Mode,
			RemoteSequence: rEntry.Sequence,
			RemoteVersion:  rEntry.Version,
		}
	}
	deleteEntry := func(path string, rEntry FileEntry) DiffEntry {
		return DiffEntry{
			Path:           path,
			Action:         ActionDelete,
			RemoteSequence: rEntry.Sequence,
			RemoteVersion:  rEntry.Version,
		}
	}

	for path, rEntry := range remote.Files {
		if rEntry.Sequence <= lastSeenSeq {
			continue // Already processed
		}

		lEntry, localExists := idx.Files[path]

		if rEntry.Deleted {
			// Remote tombstoned. Decide whether to apply the delete.
			if !localExists || lEntry.Deleted {
				continue
			}
			// H8: never honor a remote tombstone on the first sync with
			// this peer — the tombstone refers to a deletion they saw
			// from a third party and our local copy was never shared.
			if lastSeenSeq == 0 {
				continue
			}
			// C6: if both sides have clocks, the vector decides.
			if len(lEntry.Version) > 0 && len(rEntry.Version) > 0 {
				switch compareClocks(lEntry.Version, rEntry.Version) {
				case ClockAfter, ClockEqual:
					// Local write dominates the tombstone (or matches it,
					// which can't happen for alive-vs-deleted but is
					// harmless). Keep local.
					continue
				case ClockConcurrent:
					// Write/delete race — write wins. Syncthing uses the
					// same rule and it matches user expectations.
					continue
				case ClockBefore:
					actions = append(actions, deleteEntry(path, rEntry))
					continue
				}
			}
			// Legacy fallback: ancestor/mtime heuristic.
			if locallyModified(path, lEntry) {
				continue
			}
			actions = append(actions, deleteEntry(path, rEntry))
			continue
		}

		if !localExists || lEntry.Deleted {
			// Remote has a file we don't have.
			// C6: if our tombstone dominates the remote write, the remote
			// is resurrecting a path we have already deleted. Fall back to
			// Download — the peer is more recent. But if our tombstone
			// clock dominates, skip (our delete wins).
			if localExists && lEntry.Deleted &&
				len(lEntry.Version) > 0 && len(rEntry.Version) > 0 {
				if compareClocks(lEntry.Version, rEntry.Version) == ClockAfter {
					continue
				}
			}
			actions = append(actions, downloadEntry(path, rEntry))
			continue
		}

		if lEntry.SHA256 == rEntry.SHA256 {
			continue // Same content — no action regardless of clocks.
		}

		// Both sides have the file with different content.
		// C6: vector-clock classification takes precedence when both
		// sides have non-empty clocks.
		if len(lEntry.Version) > 0 && len(rEntry.Version) > 0 {
			switch compareClocks(lEntry.Version, rEntry.Version) {
			case ClockEqual:
				// Clocks equal but hashes differ — shouldn't happen in a
				// correctly-operating system, but be safe: treat as
				// conflict so data is never silently lost.
				actions = append(actions, conflictEntry(path, rEntry))
			case ClockAfter:
				// Local dominates — skip (our side propagates outbound).
			case ClockBefore:
				actions = append(actions, downloadEntry(path, rEntry))
			case ClockConcurrent:
				actions = append(actions, conflictEntry(path, rEntry))
			}
			continue
		}

		// Legacy fallback — preserves pre-C6 behavior for entries loaded
		// from an index that predates vector-clock persistence.
		if ancestor, ok := baseHashes[path]; ok {
			remoteMod := rEntry.SHA256 != ancestor
			localMod := lEntry.SHA256 != ancestor
			switch {
			case remoteMod && localMod:
				actions = append(actions, conflictEntry(path, rEntry))
			case remoteMod:
				actions = append(actions, downloadEntry(path, rEntry))
				// case localMod only: local will propagate on our next
				// outbound sync — nothing to do from the receive side.
			}
			continue
		}

		// No ancestor known. C1 mtime fallback.
		if lEntry.MtimeNS <= lastSyncNS {
			actions = append(actions, downloadEntry(path, rEntry))
		} else {
			actions = append(actions, conflictEntry(path, rEntry))
		}
	}

	// Sort for deterministic ordering.
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Path < actions[j].Path
	})

	return actions
}

// purgeTombstones removes deleted entries older than maxAge, but only when
// ALL tracked peers (including removed ones) have acknowledged them
// (LastSeenSequence >= tombstone Sequence). This prevents file resurrection
// when a peer reconnects after extended offline (B14).
//
// M3: removed peers are also checked — their LastSeenSequence must have
// caught up before we purge. Removed peer entries are garbage-collected
// once they are older than maxAge themselves.
func (idx *FileIndex) purgeTombstones(maxAge time.Duration, peers map[string]PeerState) int {
	cutoff := time.Now().Add(-maxAge).UnixNano()
	purged := 0
	for path, entry := range idx.Files {
		if !entry.Deleted || entry.MtimeNS >= cutoff {
			continue
		}
		// B14: only purge if every tracked peer has seen this tombstone.
		allAcked := true
		for _, ps := range peers {
			if ps.LastSeenSequence < entry.Sequence {
				allAcked = false
				break
			}
		}
		if allAcked {
			delete(idx.Files, path)
			purged++
		}
	}
	return purged
}

// updateBaseHashes folds a completed index exchange into the ancestor map
// (C2). For every remote entry in this exchange:
//
//   - Tombstone: the path is no longer synced — drop any stale ancestor.
//   - Hash matches our local non-deleted entry: both sides now agree on
//     this content; record the shared hash as the ancestor.
//   - Hash differs: leave any prior ancestor untouched. Successful
//     downloads and conflict auto-resolves in syncFolder have already
//     written the remote hash into our index, so a download that
//     completed in this cycle shows up in the "matches" branch above.
//     A failed download stays diverged — we keep the older ancestor so
//     diff() can still classify it correctly next cycle.
//
// The remote index passed here is whatever the peer sent in this
// exchange (full or delta). Unchanged paths not in the exchange keep
// their prior ancestor in prior.
func updateBaseHashes(prior map[string]Hash256, local *FileIndex, remote *FileIndex) map[string]Hash256 {
	if remote == nil || len(remote.Files) == 0 {
		return prior
	}
	out := prior
	if out == nil {
		out = make(map[string]Hash256, len(remote.Files))
	}
	for path, rEntry := range remote.Files {
		if rEntry.Deleted {
			delete(out, path)
			continue
		}
		lEntry, ok := local.Files[path]
		if !ok || lEntry.Deleted {
			continue
		}
		if lEntry.SHA256 == rEntry.SHA256 {
			out[path] = lEntry.SHA256
		}
	}
	return out
}

// RenamePlan describes a download/delete pair that can be satisfied by a
// local filesystem rename (R1). The sender deleted the file at OldPath
// and created an identical one at NewPath; the receiver already holds
// the content and only needs to move it.
type RenamePlan struct {
	OldPath     string
	NewPath     string
	RemoteHash  Hash256
	RemoteSize  int64
	RemoteMtime int64
	RemoteMode  uint32
	NewSequence int64 // peer's sequence for the new-path entry
	DelSequence int64 // peer's sequence for the tombstone
	// C6: peer's vector clock on the new-path entry. Adopted into the
	// local index after the rename lands so subsequent diffs see the
	// remote's write history. Nil when the peer has no clock.
	RemoteVersion VectorClock
	// C6 / R4: peer's vector clock on the OldPath tombstone. Merged
	// into the local OldPath tombstone clock after the rename lands,
	// so the tombstone reflects the peer's delete and subsequent
	// diffs do not keep re-emitting ActionDelete for the old path.
	RemoteDelVersion VectorClock
}

// planRenames finds download/delete action pairs that can be resolved
// by a local filesystem rename instead of a re-download (R1). A pair
// matches when:
//
//   - There is an ActionDownload at path NewPath with RemoteHash H.
//   - There is an ActionDelete at path OldPath, and our local index
//     has OldPath with hash H and not already tombstoned.
//   - NewPath does not already exist locally (or is tombstoned).
//
// Each delete matches at most one download; matching is stable across
// hash collisions (first-indexed delete wins). The returned set lists
// paths (both sides of every planned rename) that the caller must skip
// in its normal action loop.
func planRenames(actions []DiffEntry, local *FileIndex) ([]RenamePlan, map[string]bool) {
	if len(actions) == 0 || local == nil {
		return nil, nil
	}

	type delCandidate struct {
		path    string
		seq     int64
		version VectorClock
	}
	delsByHash := make(map[Hash256][]delCandidate)
	for _, a := range actions {
		if a.Action != ActionDelete {
			continue
		}
		lEntry, ok := local.Files[a.Path]
		if !ok || lEntry.Deleted || lEntry.SHA256.IsZero() {
			continue
		}
		delsByHash[lEntry.SHA256] = append(delsByHash[lEntry.SHA256], delCandidate{
			path: a.Path, seq: a.RemoteSequence, version: a.RemoteVersion,
		})
	}
	if len(delsByHash) == 0 {
		return nil, nil
	}

	var plans []RenamePlan
	skip := make(map[string]bool)
	for _, a := range actions {
		if a.Action != ActionDownload || a.RemoteHash.IsZero() {
			continue
		}
		if lEntry, ok := local.Files[a.Path]; ok && !lEntry.Deleted {
			continue // target exists locally — do not clobber
		}
		queue := delsByHash[a.RemoteHash]
		if len(queue) == 0 {
			continue
		}
		pick := queue[0]
		delsByHash[a.RemoteHash] = queue[1:]
		plans = append(plans, RenamePlan{
			OldPath:          pick.path,
			NewPath:          a.Path,
			RemoteHash:       a.RemoteHash,
			RemoteSize:       a.RemoteSize,
			RemoteMtime:      a.RemoteMtime,
			RemoteMode:       a.RemoteMode,
			NewSequence:      a.RemoteSequence,
			DelSequence:      pick.seq,
			RemoteVersion:    a.RemoteVersion,
			RemoteDelVersion: pick.version,
		})
		skip[a.Path] = true
		skip[pick.path] = true
	}
	return plans, skip
}

// gcRemovedPeers deletes removed peer entries that are older than maxAge.
// Called after purgeTombstones so stale removed peers don't block purge
// indefinitely.
func gcRemovedPeers(peers map[string]PeerState, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for addr, ps := range peers {
		if ps.Removed && !ps.RemovedAt.IsZero() && ps.RemovedAt.Before(cutoff) {
			delete(peers, addr)
			removed++
		}
	}
	return removed
}

// markRemovedPeers marks peers that are no longer in the configured address
// list. Peers already marked as removed are left unchanged. Returns true if
// any peer entry was modified.
func markRemovedPeers(peers map[string]PeerState, configuredAddrs []string) bool {
	active := make(map[string]struct{}, len(configuredAddrs))
	for _, addr := range configuredAddrs {
		active[addr] = struct{}{}
	}
	changed := false
	now := time.Now()
	for addr, ps := range peers {
		if ps.Removed {
			continue
		}
		if _, ok := active[addr]; !ok {
			ps.Removed = true
			ps.RemovedAt = now
			peers[addr] = ps
			changed = true
		}
	}
	// Un-remove peers that came back into the config.
	for addr, ps := range peers {
		if !ps.Removed {
			continue
		}
		if _, ok := active[addr]; ok {
			ps.Removed = false
			ps.RemovedAt = time.Time{}
			peers[addr] = ps
			changed = true
		}
	}
	return changed
}

// cleanTempFiles removes stale .mesh-tmp-* files from the entire folder tree.
func cleanTempFiles(folderRoot string, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.WalkDir(folderRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, ".mesh-tmp-") && !strings.Contains(name, ".mesh-delta-tmp-") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
		return nil
	})
}
