package filesync

import (
	"crypto/sha256"
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
	LastSync         time.Time `yaml:"last_sync"`
}

// newFileIndex creates an empty index.
func newFileIndex() *FileIndex {
	return &FileIndex{Files: make(map[string]FileEntry)}
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
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename index: %w", err)
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
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename peers: %w", err)
	}
	return nil
}

// scan walks the folder tree and updates the index with any changes.
// Returns true if any files changed.
func (idx *FileIndex) scan(folderRoot string, ignore *ignoreMatcher) (bool, error) {
	changed := false
	seen := make(map[string]struct{})

	err := filepath.WalkDir(folderRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible entries
		}

		rel, err := filepath.Rel(folderRoot, path)
		if err != nil {
			return nil
		}
		// Normalize to forward slashes for cross-platform consistency.
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		isDir := d.IsDir()

		if ignore.shouldIgnore(rel, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		if isDir {
			return nil
		}

		// Skip symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if len(seen) >= maxIndexFiles {
			return fmt.Errorf("folder exceeds max tracked files (%d)", maxIndexFiles)
		}
		seen[rel] = struct{}{}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		existing, exists := idx.Files[rel]
		mtimeNS := info.ModTime().UnixNano()
		size := info.Size()

		// Fast path: skip hashing if stat is unchanged.
		if exists && !existing.Deleted && existing.Size == size && existing.MtimeNS == mtimeNS {
			return nil
		}

		hash, err := hashFile(path)
		if err != nil {
			return nil // skip unreadable files
		}

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
	if err != nil {
		return changed, fmt.Errorf("scan %s: %w", folderRoot, err)
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
		}
	}

	return changed, nil
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
	return fmt.Sprintf("%x", h.Sum(nil)), nil
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

// purgeTombstones removes deleted entries older than the given duration.
func (idx *FileIndex) purgeTombstones(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge).UnixNano()
	for path, entry := range idx.Files {
		if entry.Deleted && entry.MtimeNS < cutoff {
			delete(idx.Files, path)
		}
	}
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
