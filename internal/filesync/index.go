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

// FileEntry holds metadata for a single tracked file.
type FileEntry struct {
	Size     int64  `yaml:"size"`
	MtimeNS  int64  `yaml:"mtime_ns"`
	SHA256   string `yaml:"sha256"`
	Deleted  bool   `yaml:"deleted,omitempty"`
	Sequence int64  `yaml:"sequence"`
	Mode     uint32 `yaml:"mode,omitempty"` // L1: Unix permission bits (e.g., 0644)

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
	Epoch    string               `yaml:"epoch,omitempty"` // H2b: random ID, regenerated on index creation
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
	files := make(map[string]FileEntry, len(idx.Files))
	for k, v := range idx.Files {
		files[k] = v
	}
	c := &FileIndex{Path: idx.Path, Sequence: idx.Sequence, Epoch: idx.Epoch, Files: files,
		cachedCount: idx.cachedCount, cachedSize: idx.cachedSize}
	return c
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

// hashResult carries the SHA-256 hex string or an error from a worker.
type hashResult struct {
	hash        string
	err         error
	hashState   []byte // PH: serialized hasher state after hashing
	prefixCheck []byte // PH: boundary bytes captured after hashing
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

	seen := make(map[string]struct{}, len(idx.Files)) // P18a: pre-size to avoid rehash cascades
	errorPaths := make(map[string]struct{})           // paths with walk/stat/hash errors — exempt from tombstoning
	tempCutoff := time.Now().Add(-maxTempFileAge)

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
				h, st, pc, hErr := hashFileIncremental(job.absPath, job.savedState, job.hashedBytes, job.newSize, job.prefixCheck)
				hashResults[job.idx] = hashResult{hash: h, err: hErr, hashState: st, prefixCheck: pc}
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
			// children from tombstoning.
			if dr.dirRel != "" {
				seen[dr.dirRel] = struct{}{}
				errorPaths[dr.dirRel] = struct{}{}
				dirPrefix := dr.dirRel + "/"
				for child := range idx.Files {
					if strings.HasPrefix(child, dirPrefix) {
						seen[child] = struct{}{}
						errorPaths[child] = struct{}{}
					}
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
			seen[wf.rel] = struct{}{}

			if wf.infoErr != nil {
				stats.StatErrors++
				errorPaths[wf.rel] = struct{}{}
				continue
			}

			existing, exists := idx.Files[wf.rel]
			mtimeNS := wf.info.ModTime().UnixNano()
			size := wf.info.Size()
			mode := uint32(wf.info.Mode().Perm())

			// Fast path: skip hashing if size and mtime are unchanged.
			if exists && !existing.Deleted && existing.Size == size && existing.MtimeNS == mtimeNS {
				if existing.Mode != mode {
					existing.Mode = mode
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
				size: size, mtimeNS: mtimeNS, mode: mode,
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

		if p.exists && !p.old.Deleted && p.old.SHA256 == r.hash {
			// Content identical despite stat change (e.g., touch, chmod). Update stat only.
			entry := p.old
			entry.MtimeNS = p.mtimeNS
			entry.Size = p.size
			entry.Mode = p.mode
			entry.HashState = r.hashState
			entry.HashedBytes = p.size
			entry.PrefixCheck = r.prefixCheck
			idx.Files[p.rel] = entry
			continue
		}

		idx.Sequence++
		idx.Files[p.rel] = FileEntry{
			Size:        p.size,
			MtimeNS:     p.mtimeNS,
			SHA256:      r.hash,
			Sequence:    idx.Sequence,
			Mode:        p.mode,
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
		// Mark deletions: entries in index not seen on disk.
		for rel, entry := range idx.Files {
			if entry.Deleted {
				continue
			}
			if _, ok := seen[rel]; !ok {
				idx.Sequence++
				entry.Deleted = true
				entry.MtimeNS = time.Now().UnixNano() // deletion time, not last-modification time
				entry.Sequence = idx.Sequence
				idx.Files[rel] = entry
				changed = true
				stats.Deletions++
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

// hashFile computes the SHA-256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from scan walk within a user-configured folder
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	defer sha256Pool.Put(h)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
func hashFileIncremental(path string, savedState []byte, hashedBytes, newSize int64, prefixCheck []byte) (hexHash string, state []byte, newPrefixCheck []byte, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from scan walk within a user-configured folder
	if err != nil {
		return "", nil, nil, err
	}
	defer func() { _ = f.Close() }()

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
						return "", nil, nil, cpErr
					}
					hexHash = hex.EncodeToString(h.Sum(nil))
					if m, mok := h.(encoding.BinaryMarshaler); mok {
						state, _ = m.MarshalBinary()
					}
					newPrefixCheck = capturePrefixCheck(f, newSize)
					return hexHash, state, newPrefixCheck, nil
				}
			}
		}
		// Fall through to full hash on any restore/seek failure.
		h.Reset()
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return "", nil, nil, seekErr
		}
	}

	// Full hash.
	if _, err := io.Copy(h, f); err != nil {
		return "", nil, nil, err
	}
	hexHash = hex.EncodeToString(h.Sum(nil))

	// Save state for files above the threshold.
	if newSize >= minIncrementalHashSize {
		if m, ok := h.(encoding.BinaryMarshaler); ok {
			state, _ = m.MarshalBinary()
		}
		newPrefixCheck = capturePrefixCheck(f, newSize)
	}
	return hexHash, state, newPrefixCheck, nil
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
	RemoteHash     string
	RemoteSize     int64
	RemoteMtime    int64
	RemoteMode     uint32 // L1: file permission bits from remote
	RemoteSequence int64  // B9: track for safe LastSeenSequence advancement
}

// diff compares the local index with a remote index and produces a list of
// actions needed to bring the local side up to date.
// lastSeenSeq is the highest sequence we've previously processed from this peer.
func (idx *FileIndex) diff(remote *FileIndex, lastSeenSeq int64, direction string) []DiffEntry {
	canReceive := direction == "send-receive" || direction == "receive-only" || direction == "dry-run"
	if !canReceive {
		return nil
	}

	var actions []DiffEntry

	for path, rEntry := range remote.Files {
		if rEntry.Sequence <= lastSeenSeq {
			continue // Already processed
		}

		lEntry, localExists := idx.Files[path]

		if rEntry.Deleted {
			// Remote deleted the file.
			if localExists && !lEntry.Deleted {
				// B8: if we have a prior sync baseline (lastSeenSeq > 0)
				// and the local file was modified after that baseline,
				// local wins over remote delete. The local version will
				// propagate back to the peer on the next outbound sync.
				if lastSeenSeq > 0 && lEntry.Sequence > lastSeenSeq {
					continue
				}
				// H8: on first sync (lastSeenSeq=0), never delete a
				// locally-existing file based on a remote tombstone. The
				// local file was never shared with this peer, so the
				// tombstone refers to a deletion the peer saw from a
				// third party. The local file will propagate back on
				// the next outbound cycle.
				if lastSeenSeq == 0 {
					continue
				}
				actions = append(actions, DiffEntry{
					Path:           path,
					Action:         ActionDelete,
					RemoteSequence: rEntry.Sequence,
				})
			}
			continue
		}

		if !localExists || lEntry.Deleted {
			// Remote has a file we don't have.
			actions = append(actions, DiffEntry{
				Path:           path,
				Action:         ActionDownload,
				RemoteHash:     rEntry.SHA256,
				RemoteSize:     rEntry.Size,
				RemoteMtime:    rEntry.MtimeNS,
				RemoteMode:     rEntry.Mode,
				RemoteSequence: rEntry.Sequence,
			})
			continue
		}

		if lEntry.SHA256 == rEntry.SHA256 {
			continue // Same content
		}

		// Both sides have the file with different content.
		// Check if only the remote changed (our entry was unchanged since last sync).
		if lEntry.Sequence <= lastSeenSeq {
			// Only remote changed.
			actions = append(actions, DiffEntry{
				Path:           path,
				Action:         ActionDownload,
				RemoteHash:     rEntry.SHA256,
				RemoteSize:     rEntry.Size,
				RemoteMtime:    rEntry.MtimeNS,
				RemoteMode:     rEntry.Mode,
				RemoteSequence: rEntry.Sequence,
			})
		} else {
			// Both sides changed → conflict.
			actions = append(actions, DiffEntry{
				Path:           path,
				Action:         ActionConflict,
				RemoteHash:     rEntry.SHA256,
				RemoteSize:     rEntry.Size,
				RemoteMtime:    rEntry.MtimeNS,
				RemoteMode:     rEntry.Mode,
				RemoteSequence: rEntry.Sequence,
			})
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
