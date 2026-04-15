package filesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// maxIndexFiles caps the number of files tracked in a single folder index
// to prevent OOM from scanning enormous directory trees.
const maxIndexFiles = 200_000

// FileEntry holds metadata for a single tracked file.
type FileEntry struct {
	Size     int64  `yaml:"size"`
	MtimeNS  int64  `yaml:"mtime_ns"`
	SHA256   string `yaml:"sha256"`
	Deleted  bool   `yaml:"deleted,omitempty"`
	Sequence int64  `yaml:"sequence"`
}

// FileIndex is the in-memory index for a single folder.
type FileIndex struct {
	Path     string               `yaml:"path"`
	Sequence int64                `yaml:"sequence"`
	Files    map[string]FileEntry `yaml:"files"`
}

// PeerState tracks per-peer sync progress.
type PeerState struct {
	LastSeenSequence int64     `yaml:"last_seen_sequence"`
	LastSentSequence int64     `yaml:"last_sent_sequence"` // our index sequence last sent to this peer
	LastSync         time.Time `yaml:"last_sync"`
}

// newFileIndex creates an empty index.
func newFileIndex() *FileIndex {
	return &FileIndex{Files: make(map[string]FileEntry)}
}

// clone returns a deep copy of the index. Used by the scan path so WalkDir
// mutates a private copy and readers (admin UI, dashboard) never block on
// the folder's write lock.
func (idx *FileIndex) clone() *FileIndex {
	files := make(map[string]FileEntry, len(idx.Files))
	for k, v := range idx.Files {
		files[k] = v
	}
	return &FileIndex{Path: idx.Path, Sequence: idx.Sequence, Files: files}
}

// loadIndex reads a persisted index from disk.
func loadIndex(path string) (*FileIndex, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from user cache dir
	if err != nil {
		if os.IsNotExist(err) {
			return newFileIndex(), nil
		}
		return nil, fmt.Errorf("read index: %w", err)
	}
	idx := newFileIndex()
	if err := yaml.Unmarshal(data, idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	return idx, nil
}

// save writes the index to disk atomically (temp + rename).
func (idx *FileIndex) save(path string) error {
	data, err := yaml.Marshal(idx)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create index dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write index temp: %w", err)
	}
	if err := renameReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename index: %w", err)
	}
	return nil
}

// renameReplace atomically renames src to dst.
// On Windows, os.Rename fails if dst exists. This helper removes the
// destination first when needed.
func renameReplace(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		if os.Remove(dst) == nil {
			return os.Rename(src, dst)
		}
		return err
	}
	return nil
}

// loadPeerStates reads per-peer sync state from disk.
func loadPeerStates(path string) (map[string]PeerState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from user cache dir
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]PeerState), nil
		}
		return nil, fmt.Errorf("read peers: %w", err)
	}
	peers := make(map[string]PeerState)
	if err := yaml.Unmarshal(data, &peers); err != nil {
		return nil, fmt.Errorf("parse peers: %w", err)
	}
	return peers, nil
}

// savePeerStates writes peer state to disk atomically.
func savePeerStates(path string, peers map[string]PeerState) error {
	data, err := yaml.Marshal(peers)
	if err != nil {
		return fmt.Errorf("marshal peers: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create peers dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write peers temp: %w", err)
	}
	if err := renameReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename peers: %w", err)
	}
	return nil
}

// activeCount returns the number of non-deleted files in the index.
func (idx *FileIndex) activeCount() int {
	count := 0
	for _, e := range idx.Files {
		if !e.Deleted {
			count++
		}
	}
	return count
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
	Deletions       int // tombstones created in this pass
}

// scan walks the folder, updates the index, cleans stale temp files, and
// returns whether any files changed, the active (non-deleted) file count,
// and the number of directories walked (excluding the root and ignored subtrees).
func (idx *FileIndex) scan(ctx context.Context, folderRoot string, ignore *ignoreMatcher) (changed bool, activeCount, dirCount int, err error) {
	changed, activeCount, dirCount, _, _, err = idx.scanWithStats(ctx, folderRoot, ignore)
	return
}

// scanWithStats is scan with detailed per-phase instrumentation. Callers that
// want evidence (runScan) use this; tests keep the simpler signature.
//
//nolint:gocyclo // scan is a single-pass WalkDir; splitting it would hurt locality more than it helps.
func (idx *FileIndex) scanWithStats(ctx context.Context, folderRoot string, ignore *ignoreMatcher) (changed bool, activeCount, dirCount int, stats ScanStats, conflicts []string, err error) {
	changed = false
	seen := make(map[string]struct{})
	tempCutoff := time.Now().Add(-maxTempFileAge)

	walkStart := time.Now()
	err = filepath.WalkDir(folderRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err() // bail out on shutdown
		}
		stats.EntriesVisited++
		if walkErr != nil {
			return nil // skip inaccessible entries
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

		if len(seen) >= maxIndexFiles {
			return fmt.Errorf("folder exceeds max tracked files (%d)", maxIndexFiles)
		}
		seen[rel] = struct{}{}

		statStart := time.Now()
		info, err := d.Info()
		stats.StatDuration += time.Since(statStart)
		if err != nil {
			stats.StatErrors++
			return nil
		}

		existing, exists := idx.Files[rel]
		mtimeNS := info.ModTime().UnixNano()
		size := info.Size()

		// Fast path: skip hashing if stat is unchanged.
		if exists && !existing.Deleted && existing.Size == size && existing.MtimeNS == mtimeNS {
			stats.FastPathHits++
			return nil
		}

		hashStart := time.Now()
		hash, hashErr := hashFile(path)
		stats.HashDuration += time.Since(hashStart)
		if hashErr != nil {
			stats.HashErrors++
			return nil // skip unreadable files
		}
		stats.FilesHashed++
		stats.BytesHashed += size

		if exists && !existing.Deleted && existing.SHA256 == hash {
			// Content identical despite stat change (e.g., touch). Update stat only.
			existing.MtimeNS = mtimeNS
			existing.Size = size
			idx.Files[rel] = existing
			return nil
		}

		idx.Sequence++
		idx.Files[rel] = FileEntry{
			Size:     size,
			MtimeNS:  mtimeNS,
			SHA256:   hash,
			Sequence: idx.Sequence,
		}
		changed = true
		return nil
	})
	stats.WalkDuration = time.Since(walkStart)
	if err != nil {
		return changed, len(seen), dirCount, stats, conflicts, fmt.Errorf("scan %s: %w", folderRoot, err)
	}

	delStart := time.Now()
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
	Path        string
	Action      DiffAction
	RemoteHash  string
	RemoteSize  int64
	RemoteMtime int64
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
				actions = append(actions, DiffEntry{
					Path:   path,
					Action: ActionDelete,
				})
			}
			continue
		}

		if !localExists || lEntry.Deleted {
			// Remote has a file we don't have.
			actions = append(actions, DiffEntry{
				Path:        path,
				Action:      ActionDownload,
				RemoteHash:  rEntry.SHA256,
				RemoteSize:  rEntry.Size,
				RemoteMtime: rEntry.MtimeNS,
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
				Path:        path,
				Action:      ActionDownload,
				RemoteHash:  rEntry.SHA256,
				RemoteSize:  rEntry.Size,
				RemoteMtime: rEntry.MtimeNS,
			})
		} else {
			// Both sides changed → conflict.
			actions = append(actions, DiffEntry{
				Path:        path,
				Action:      ActionConflict,
				RemoteHash:  rEntry.SHA256,
				RemoteSize:  rEntry.Size,
				RemoteMtime: rEntry.MtimeNS,
			})
		}
	}

	// Sort for deterministic ordering.
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Path < actions[j].Path
	})

	return actions
}

// purgeTombstones removes deleted entries older than maxAge and returns the
// number removed (useful for debug telemetry).
func (idx *FileIndex) purgeTombstones(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge).UnixNano()
	purged := 0
	for path, entry := range idx.Files {
		if entry.Deleted && entry.MtimeNS < cutoff {
			delete(idx.Files, path)
			purged++
		}
	}
	return purged
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
