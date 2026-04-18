package filesync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
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

// setEntry updates a file entry and maintains the cached counters.
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
}

// ScanStats captures measurable work performed by a single scan pass so
// callers can attribute wall time to concrete phases instead of guessing.
// All counters are populated even on error — partial stats are still useful
// for triage. Zero-valued fields mean "phase did not run" (e.g. deletions
// are skipped on WalkDir error).
type ScanStats struct {
	WalkDuration   time.Duration // total time inside filepath.WalkDir
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
	absPath string
	result  chan hashResult
}

// hashResult carries the SHA-256 hex string or an error from a worker.
type hashResult struct {
	hash string
	err  error
}

// pendingHash records metadata captured during the walk for a file that
// needs hashing. The hash result is delivered via the result channel.
type pendingHash struct {
	rel     string
	absPath string
	size    int64
	mtimeNS int64
	mode    uint32
	exists  bool      // true if the file was already in the index
	old     FileEntry // previous index entry (valid only when exists)
	result  chan hashResult
}

// scanHashWorkers is the number of parallel hash workers. Capped at 8 to
// avoid saturating disk I/O on spinning disks while still benefiting from
// SSD parallelism and multi-core SHA-256.
var scanHashWorkers = min(runtime.GOMAXPROCS(0), 8)

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
//nolint:gocyclo // scan is a single-pass WalkDir; splitting it would hurt locality more than it helps.
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
	var hashWg sync.WaitGroup
	for range scanHashWorkers {
		hashWg.Add(1)
		go func() {
			defer hashWg.Done()
			for job := range hashCh {
				h, hErr := hashFile(job.absPath)
				job.result <- hashResult{h, hErr}
			}
		}()
	}
	var pending []pendingHash

	walkStart := time.Now()
	err = filepath.WalkDir(folderRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err() // bail out on shutdown
		}
		stats.EntriesVisited++
		if walkErr != nil {
			// H1: the entry exists on disk but we can't read it. Mark it in
			// seen (so the tombstone phase doesn't treat it as deleted) and
			// record it as an error path for per-file suppression.
			if rel, relErr := filepath.Rel(folderRoot, path); relErr == nil {
				rel = filepath.ToSlash(rel)
				rel = norm.NFC.String(rel)
				if rel != "." {
					seen[rel] = struct{}{}
					errorPaths[rel] = struct{}{}
					// If this is a directory, its children won't be walked.
					// Protect all known children from tombstoning by adding
					// them to seen (they still exist on disk, we just can't
					// traverse into the parent).
					dirPrefix := rel + "/"
					for child := range idx.Files {
						if strings.HasPrefix(child, dirPrefix) {
							seen[child] = struct{}{}
							errorPaths[child] = struct{}{}
						}
					}
				}
			}
			stats.StatErrors++
			return nil
		}

		// P8: Clean stale temp files during the walk instead of a separate traversal.
		name := d.Name()
		if !d.IsDir() && (strings.HasPrefix(name, ".mesh-tmp-") || strings.HasSuffix(name, ".mesh-delta-tmp")) {
			if info, infoErr := d.Info(); infoErr == nil && info.ModTime().Before(tempCutoff) {
				if os.Remove(path) == nil {
					stats.TempCleaned++
				}
			}
			return nil
		}

		rel, relErr := filepath.Rel(folderRoot, path)
		if relErr != nil {
			return nil
		}
		// Normalize to forward slashes for cross-platform consistency.
		rel = filepath.ToSlash(rel)
		// B17: normalize to NFC so macOS NFD paths match Windows NFC paths.
		rel = norm.NFC.String(rel)
		if rel == "." {
			return nil
		}

		isDir := d.IsDir()

		ignStart := time.Now()
		skip := ignore.shouldIgnore(rel, isDir)
		stats.IgnoreDuration += time.Since(ignStart)
		if skip {
			if isDir {
				stats.DirsIgnored++
				return filepath.SkipDir
			}
			stats.FilesIgnored++
			return nil
		}

		if isDir {
			dirCount++
			stats.DirsWalked++
			return nil
		}

		// Skip symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			stats.SymlinksSkipped++
			return nil
		}

		// Collect Syncthing-style conflict files during the main walk so the
		// admin UI doesn't need a second full-tree traversal per scan.
		// Conflict files are still tracked as normal files (they remain on
		// disk and get synced like any other file).
		if isConflictFile(name) {
			conflicts = append(conflicts, rel)
		}

		if len(seen) >= maxFiles {
			return errIndexCapExceeded
		}
		seen[rel] = struct{}{}

		statStart := time.Now()
		info, err := d.Info()
		stats.StatDuration += time.Since(statStart)
		if err != nil {
			stats.StatErrors++
			errorPaths[rel] = struct{}{}
			return nil
		}

		existing, exists := idx.Files[rel]
		mtimeNS := info.ModTime().UnixNano()
		size := info.Size()
		mode := uint32(info.Mode().Perm())

		// Fast path: skip hashing if size and mtime are unchanged.
		// Mode is not checked here — a chmod alone doesn't change content,
		// so we update it without rehashing.
		if exists && !existing.Deleted && existing.Size == size && existing.MtimeNS == mtimeNS {
			if existing.Mode != mode {
				existing.Mode = mode
				idx.Files[rel] = existing
			}
			stats.FastPathHits++
			return nil
		}

		// P20a: submit to parallel hash pool instead of hashing inline.
		resultCh := make(chan hashResult, 1)
		pending = append(pending, pendingHash{
			rel: rel, absPath: path,
			size: size, mtimeNS: mtimeNS, mode: mode,
			exists: exists, old: existing,
			result: resultCh,
		})
		hashCh <- hashJob{absPath: path, result: resultCh}
		return nil
	})
	stats.WalkDuration = time.Since(walkStart)

	// P20a: close the hash channel and wait for all workers to finish.
	close(hashCh)
	hashWg.Wait()

	// P20a: drain hash results and update index sequentially.
	hashDrainStart := time.Now()
	for _, p := range pending {
		r := <-p.result
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
			idx.Files[p.rel] = entry
			continue
		}

		idx.Sequence++
		idx.Files[p.rel] = FileEntry{
			Size:     p.size,
			MtimeNS:  p.mtimeNS,
			SHA256:   r.hash,
			Sequence: idx.Sequence,
			Mode:     p.mode,
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
	// Use live (non-tombstone) file count as denominator so old tombstones
	// don't inflate the threshold and make bulk suppression harder to trigger.
	liveFiles := 0
	for _, e := range idx.Files {
		if !e.Deleted {
			liveFiles++
		}
	}
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

// hashFile computes the SHA-256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from filepath.WalkDir within a user-configured folder
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
		if !strings.HasPrefix(name, ".mesh-tmp-") && !strings.HasSuffix(name, ".mesh-delta-tmp") {
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
