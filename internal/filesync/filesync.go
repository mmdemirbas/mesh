package filesync

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/time/rate"

	"github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"github.com/mmdemirbas/mesh/internal/nodeutil"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tlsutil"
)

const (
	tombstoneMaxAge = 30 * 24 * time.Hour // 30 days
)

// activeNodes tracks running filesync nodes for admin API access.
var activeNodes nodeutil.Registry[Node]

// configFolders provides folder metadata from config before Start() has
// registered with activeNodes. This lets the admin API return folder info
// (ID, path, direction, peers, ignore patterns) immediately on startup
// without waiting for index loading and scanning.
var configFolders struct {
	mu      sync.Mutex
	folders []FolderStatus
}

// SetConfigFolders pre-populates folder metadata from config so the admin
// API can show folder info before Start() finishes loading indexes from disk.
// Called from main.go before the admin server starts. Accumulates across
// multiple calls (one per filesync config block).
func SetConfigFolders(cfg config.FilesyncCfg) {
	configFolders.mu.Lock()
	defer configFolders.mu.Unlock()
	for _, fcfg := range cfg.ResolvedFolders {
		peers := make([]FolderPeer, len(fcfg.Peers))
		for i, addr := range fcfg.Peers {
			name := ""
			if i < len(fcfg.PeerNames) {
				name = fcfg.PeerNames[i]
			}
			peers[i] = FolderPeer{Name: name, Addr: addr}
		}
		configFolders.folders = append(configFolders.folders, FolderStatus{
			ID:             fcfg.ID,
			Path:           fcfg.Path,
			Direction:      fcfg.Direction,
			IgnorePatterns: append([]string(nil), fcfg.IgnorePatterns...),
			Peers:          peers,
			Scanning:       fcfg.Direction != "disabled",
		})
	}
	sort.Slice(configFolders.folders, func(i, j int) bool {
		return configFolders.folders[i].ID < configFolders.folders[j].ID
	})
}

func clearConfigFolders() {
	configFolders.mu.Lock()
	configFolders.folders = nil
	configFolders.mu.Unlock()
}

// FolderStatus is an exported summary of a folder's sync state for the admin API.
type FolderStatus struct {
	ID             string       `json:"id"`
	Path           string       `json:"path"`
	Direction      string       `json:"direction"`
	FileCount      int          `json:"file_count"`
	DirCount       int          `json:"dir_count"`
	TotalBytes     int64        `json:"total_bytes"`
	Sequence       int64        `json:"sequence"`
	IgnorePatterns []string     `json:"ignore_patterns,omitempty"`
	Peers          []FolderPeer `json:"peers"`
	// LastSync is the most recent successful sync time across all peers.
	// Zero value means no peer has synced yet.
	LastSync        time.Time `json:"last_sync"`
	QuarantineCount int       `json:"quarantine_count,omitempty"`
	QuarantinePaths []string  `json:"quarantine_paths,omitempty"`
	// Scanning is true when the folder has not yet completed its initial scan.
	// File counts and sizes may be stale (from the previous session's persisted
	// index) or zero (first run). The UI should indicate incomplete data.
	Scanning bool `json:"scanning,omitempty"`
}

// FolderPeer is a resolved peer entry for a folder: the configured nickname
// plus the address it expanded to, per-peer sync progress, and the most
// recent pending sync plan computed from diffing against that peer.
type FolderPeer struct {
	Name             string          `json:"name"`
	Addr             string          `json:"addr"`
	LastSync         time.Time       `json:"last_sync"`
	LastSeenSequence int64           `json:"last_seen_sequence"`
	LastSentSequence int64           `json:"last_sent_sequence"`
	LastError        string          `json:"last_error,omitempty"`
	BackoffRemaining time.Duration   `json:"backoff_remaining,omitempty"` // R3: non-zero while peer is in consecutive-failure backoff
	Pending          *PendingSummary `json:"pending,omitempty"`
}

// PendingSummary is the most recent sync plan against a peer: counts, total
// bytes, and a capped preview of affected files.
type PendingSummary struct {
	UpdatedAt time.Time     `json:"updated_at"`
	Downloads int           `json:"downloads"`
	Conflicts int           `json:"conflicts"`
	Deletes   int           `json:"deletes"`
	Bytes     int64         `json:"bytes"`
	Files     []PendingFile `json:"files,omitempty"`
}

// PendingFile is one entry in a pending-plan preview.
type PendingFile struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Size   int64  `json:"size"`
}

const pendingFilePreviewLimit = 50

func actionName(a DiffAction) string {
	switch a {
	case ActionDownload:
		return "download"
	case ActionConflict:
		return "conflict"
	case ActionDelete:
		return "delete"
	}
	return "unknown"
}

func buildPendingSummary(actions []DiffEntry) PendingSummary {
	ps := PendingSummary{UpdatedAt: time.Now()}
	for _, a := range actions {
		switch a.Action {
		case ActionDownload:
			ps.Downloads++
		case ActionConflict:
			ps.Conflicts++
		case ActionDelete:
			ps.Deletes++
		}
		ps.Bytes += a.RemoteSize
		if len(ps.Files) < pendingFilePreviewLimit {
			ps.Files = append(ps.Files, PendingFile{
				Path:   a.Path,
				Action: actionName(a.Action),
				Size:   a.RemoteSize,
			})
		}
	}
	return ps
}

func clonePendingSummary(p PendingSummary) *PendingSummary {
	out := p
	if len(p.Files) > 0 {
		out.Files = append([]PendingFile(nil), p.Files...)
	}
	return &out
}

// ConflictInfo describes a conflict file found in a synced folder.
type ConflictInfo struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
}

// SyncActivity records one completed sync cycle for the activity history.
type SyncActivity struct {
	Time      time.Time `json:"time"`
	Folder    string    `json:"folder"`
	Peer      string    `json:"peer"`
	Direction string    `json:"direction"` // "download", "upload", "conflict", "delete", "idle"
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	Error     string    `json:"error,omitempty"`
}

const activityHistorySize = 50

// GetFolderStatuses returns status summaries for all active filesync folders.
func GetFolderStatuses() []FolderStatus {
	var result []FolderStatus
	activeNodes.ForEach(func(n *Node) {
		ids := make([]string, 0, len(n.folders))
		for id := range n.folders {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fs := n.folders[id]
			fs.indexMu.RLock()
			count, totalBytes := fs.index.activeCountAndSize() // P18b: O(1) cached
			dirs := fs.dirCount
			seq := fs.index.Sequence
			var lastSync time.Time
			peers := make([]FolderPeer, len(fs.cfg.Peers))
			for i, addr := range fs.cfg.Peers {
				name := ""
				if i < len(fs.cfg.PeerNames) {
					name = fs.cfg.PeerNames[i]
				}
				fp := FolderPeer{Name: name, Addr: addr}
				if ps, ok := fs.peers[addr]; ok {
					fp.LastSync = ps.LastSync
					fp.LastSeenSequence = ps.LastSeenSequence
					fp.LastSentSequence = ps.LastSentSequence
					if ps.LastSync.After(lastSync) {
						lastSync = ps.LastSync
					}
				}
				if msg, ok := fs.peerLastError[addr]; ok {
					fp.LastError = msg
				}
				// R3: surface active peer-level backoff so operators see
				// "peer is being skipped" rather than inferring it from
				// LastError + clock math.
				if backed, remaining := fs.peerRetries.backedOff(addr); backed {
					fp.BackoffRemaining = remaining
				}
				if p, ok := fs.pending[addr]; ok {
					fp.Pending = clonePendingSummary(p)
				}
				peers[i] = fp
			}
			ignores := append([]string(nil), fs.cfg.IgnorePatterns...)
			qpaths := fs.retries.quarantinedPaths()
			fs.indexMu.RUnlock()

			scanning := !fs.initialScanDone.Load() && fs.cfg.Direction != "disabled"
			result = append(result, FolderStatus{
				ID:              id,
				Path:            fs.cfg.Path,
				Direction:       fs.cfg.Direction,
				FileCount:       count,
				DirCount:        dirs,
				TotalBytes:      totalBytes,
				Sequence:        seq,
				IgnorePatterns:  ignores,
				Peers:           peers,
				LastSync:        lastSync,
				QuarantineCount: len(qpaths),
				QuarantinePaths: qpaths,
				Scanning:        scanning,
			})
		}
	})
	// Fall back to config-only data when no nodes have registered yet.
	// This happens during the startup gap between admin server start and
	// filesync.Start() completing index loading.
	if result == nil {
		configFolders.mu.Lock()
		result = append([]FolderStatus(nil), configFolders.folders...)
		configFolders.mu.Unlock()
	}
	return result
}

// GetConflicts returns all conflict files across all active filesync folders.
// Reads from the per-folder cache refreshed during scan — does not walk the
// filesystem on the API path.
func GetConflicts() []ConflictInfo {
	var result []ConflictInfo
	activeNodes.ForEach(func(n *Node) {
		ids := make([]string, 0, len(n.folders))
		for id := range n.folders {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fs := n.folders[id]
			fs.indexMu.RLock()
			for _, c := range fs.conflicts {
				result = append(result, ConflictInfo{FolderID: id, Path: c})
			}
			fs.indexMu.RUnlock()
		}
	})
	return result
}

// RegisterFolderForTest inserts a minimal Node whose folder map contains a
// single (folderID → path) entry and returns a cleanup function that removes
// it. Intended for tests in packages outside filesync (e.g. cmd/mesh admin
// handler tests) that need GetFolderPath to resolve a folder without
// starting a full sync node.
//
// Test-only: do not call from production code paths.
func RegisterFolderForTest(folderID, path string) func() {
	n := &Node{folders: map[string]*folderState{
		folderID: {cfg: config.FolderCfg{Path: path}},
	}}
	activeNodes.Register(n)
	return func() { activeNodes.Unregister(n) }
}

// GetFolderPath returns the disk path for the given folder ID, or ("", false)
// if the folder is not active.
func GetFolderPath(folderID string) (string, bool) {
	var path string
	var found bool
	activeNodes.ForEach(func(n *Node) {
		if found {
			return
		}
		if fs, ok := n.folders[folderID]; ok {
			path = fs.cfg.Path
			found = true
		}
	})
	return path, found
}

// GetActivities returns the most recent sync activities across all active nodes.
func GetActivities() []SyncActivity {
	var result []SyncActivity
	activeNodes.ForEach(func(n *Node) {
		n.activityMu.RLock()
		result = append(result, n.activities...)
		n.activityMu.RUnlock()
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Time.After(result[j].Time) })
	if len(result) > activityHistorySize {
		result = result[:activityHistorySize]
	}
	return result
}

func (n *Node) recordActivity(a SyncActivity) {
	n.activityMu.Lock()
	n.activities = append(n.activities, a)
	if len(n.activities) > activityHistorySize {
		n.activities = n.activities[len(n.activities)-activityHistorySize:]
	}
	n.activityMu.Unlock()
}

// FolderMetricsSnapshot is a point-in-time read of a folder's sync counters.
type FolderMetricsSnapshot struct {
	FolderID           string
	PeerSyncs          int64
	FilesDownloaded    int64
	FilesDeleted       int64
	FilesConflicted    int64
	FilesRenamed       int64
	SyncErrors         int64
	BytesDownloaded    int64
	BytesUploaded      int64
	BytesSavedByRename int64
	IndexExchanges     int64
	ScanCount          int64
	ScanDurationNS     int64
	PeerSyncNS         int64
}

// GetFolderMetrics returns a snapshot of sync metrics for all active folders.
func GetFolderMetrics() []FolderMetricsSnapshot {
	var result []FolderMetricsSnapshot
	activeNodes.ForEach(func(n *Node) {
		for id, fs := range n.folders {
			result = append(result, FolderMetricsSnapshot{
				FolderID:           id,
				PeerSyncs:          fs.metrics.PeerSyncs.Load(),
				FilesDownloaded:    fs.metrics.FilesDownloaded.Load(),
				FilesDeleted:       fs.metrics.FilesDeleted.Load(),
				FilesConflicted:    fs.metrics.FilesConflicted.Load(),
				FilesRenamed:       fs.metrics.FilesRenamed.Load(),
				SyncErrors:         fs.metrics.SyncErrors.Load(),
				BytesDownloaded:    fs.metrics.BytesDownloaded.Load(),
				BytesUploaded:      fs.metrics.BytesUploaded.Load(),
				BytesSavedByRename: fs.metrics.BytesSavedByRename.Load(),
				IndexExchanges:     fs.metrics.IndexExchanges.Load(),
				ScanCount:          fs.metrics.ScanCount.Load(),
				ScanDurationNS:     fs.metrics.ScanDurationNS.Load(),
				PeerSyncNS:         fs.metrics.PeerSyncNS.Load(),
			})
		}
	})
	sort.Slice(result, func(i, j int) bool { return result[i].FolderID < result[j].FolderID })
	return result
}

// FolderSyncMetrics holds per-folder sync counters for Prometheus export.
// All fields are atomic — safe for concurrent reads from the metrics handler.
type FolderSyncMetrics struct {
	PeerSyncs          atomic.Int64 // completed peer sync completions (one per folder×peer pair)
	FilesDownloaded    atomic.Int64 // files successfully downloaded
	FilesDeleted       atomic.Int64 // files deleted by remote tombstones
	FilesConflicted    atomic.Int64 // conflict resolutions performed
	FilesRenamed       atomic.Int64 // files resolved by local rename (R1)
	SyncErrors         atomic.Int64 // per-file sync failures (download, delete, conflict)
	BytesDownloaded    atomic.Int64 // bytes downloaded from peers
	BytesUploaded      atomic.Int64 // bytes served to peers (file + delta endpoints)
	BytesSavedByRename atomic.Int64 // bytes avoided by R1 local rename
	IndexExchanges     atomic.Int64 // index exchange round trips
	ScanCount          atomic.Int64 // scan cycles completed
	ScanDurationNS     atomic.Int64 // last scan duration in nanoseconds
	PeerSyncNS         atomic.Int64 // last peer sync duration in nanoseconds (last-writer-wins)
}

// folderState holds runtime state for a single synced folder.
//
// Scan/sync coordination contract (see R2 in docs/filesync/PLAN.md):
// scan holds indexMu.Lock across the index swap; sync holds
// indexMu.RLock for the diff; Node.firstScanDone (and per-folder
// initialScanDone) gate all peer sync until the first full scan
// completes. Do not add a parallel coordination mechanism.
type folderState struct {
	cfg             config.FolderCfg
	root            *os.Root // L5: TOCTOU-safe folder root handle
	index           *FileIndex
	ignore          *ignoreMatcher
	peers           map[string]PeerState      // peerAddr -> state
	pending         map[string]PendingSummary // peerAddr -> most recent pending plan
	peerLastError   map[string]string         // peerAddr -> last transport error
	dirCount        int                       // directory count from last scan (dashboard only)
	conflicts       []string                  // conflict file paths from last scan (sorted)
	firstScanLogged atomic.Bool               // N8: true after first scan INFO line emitted
	initialScanDone atomic.Bool               // true after first full scan completes
	indexMu         sync.RWMutex
	retries         retryTracker
	peerRetries     peerRetryTracker // R3: per-peer backoff after consecutive sendIndex failures
	// H3: inFlight tracks paths currently being downloaded so concurrent
	// peer goroutines skip the same path. Protected by inFlightMu.
	inFlightMu  sync.Mutex
	inFlight    map[string]bool
	persistMu   sync.Mutex        // N10: serializes persistFolder calls
	indexDirty  bool              // P17a: true when file index changed since last persist
	peersDirty  bool              // P17a: true when peer state changed since last persist
	isNetworkFS bool              // C2: true when folder root is on a network filesystem
	metrics     FolderSyncMetrics // lock-free counters for Prometheus
	// P18c: recycled scan-clone backing map. Stashed after swap, reused on
	// the next runScan to avoid a ~30 MB/scan allocation on large folders.
	// Accessed only under indexMu.
	reusableFiles map[string]FileEntry
}

// claimPath attempts to mark a path as in-flight for download. Returns false
// if another goroutine is already downloading the same path.
func (fs *folderState) claimPath(path string) bool {
	fs.inFlightMu.Lock()
	defer fs.inFlightMu.Unlock()
	if fs.inFlight[path] {
		return false
	}
	fs.inFlight[path] = true
	return true
}

// releasePath removes a path from the in-flight set.
func (fs *folderState) releasePath(path string) {
	fs.inFlightMu.Lock()
	delete(fs.inFlight, path)
	fs.inFlightMu.Unlock()
}

// applyHintRenames walks the action set for sender-supplied rename hints
// (R1 Phase 2). When an ActionDownload carries RemotePrevPath and a
// matching ActionDelete is also present, the pair is a rename-with-edit
// the sender detected via inode. Rename the local file at OldPath to
// NewPath so the subsequent download finds an existing file and takes
// the /delta fast path, moving only the changed blocks over the wire.
//
// planRenames handles the content-unchanged case (local hash matches
// remote). This pass picks up the remaining content-changed cases.
//
// OldPath is added to renamedPaths so its tombstone action is skipped
// (we apply it ourselves). NewPath is intentionally not added — the
// download still runs and reaches H2 via /delta.
func (fs *folderState) applyHintRenames(ctx context.Context, folderID, peerAddr string, actions []DiffEntry, renamedPaths map[string]bool) {
	fs.indexMu.RLock()
	deletesInSet := make(map[string]bool, len(actions))
	for _, a := range actions {
		if a.Action == ActionDelete {
			deletesInSet[a.Path] = true
		}
	}
	var hintCandidates []DiffEntry
	for _, a := range actions {
		if a.Action != ActionDownload || a.RemotePrevPath == "" {
			continue
		}
		if renamedPaths[a.Path] || renamedPaths[a.RemotePrevPath] {
			continue // already handled by planRenames
		}
		if !deletesInSet[a.RemotePrevPath] {
			continue // stale hint — no matching delete
		}
		oldEntry, exists := fs.index.Files[a.RemotePrevPath]
		if !exists || oldEntry.Deleted {
			continue // local already has no file at OldPath
		}
		hintCandidates = append(hintCandidates, a)
	}
	fs.indexMu.RUnlock()

	for _, a := range hintCandidates {
		if ctx.Err() != nil {
			return
		}
		oldPath := a.RemotePrevPath
		if !fs.claimPath(oldPath) {
			continue
		}
		if !fs.claimPath(a.Path) {
			fs.releasePath(oldPath)
			continue
		}
		// Drift safety: do not clobber a pre-existing NewPath on disk
		// even if the index has no entry for it.
		if _, err := fs.root.Stat(a.Path); err == nil {
			fs.releasePath(oldPath)
			fs.releasePath(a.Path)
			continue
		}
		if err := fs.root.Rename(oldPath, a.Path); err != nil {
			slog.Debug("hint rename fallback to download", "folder", folderID,
				"old", oldPath, "new", a.Path, "error", err)
			fs.releasePath(oldPath)
			fs.releasePath(a.Path)
			continue
		}
		fs.indexMu.Lock()
		oldEntry := fs.index.Files[oldPath]
		if !oldEntry.Deleted {
			fs.index.Sequence++
			oldEntry.Deleted = true
			oldEntry.MtimeNS = time.Now().UnixNano()
			oldEntry.Sequence = fs.index.Sequence
			// C6: local tombstone for the rename source is a local
			// write — bump self.
			if fs.index.selfID != "" {
				oldEntry.Version = oldEntry.Version.bump(fs.index.selfID)
			}
			fs.index.setEntry(oldPath, oldEntry)
		}
		fs.retries.clearAll(oldPath)
		fs.indexMu.Unlock()
		fs.releasePath(oldPath)
		fs.releasePath(a.Path)

		renamedPaths[oldPath] = true
		fs.metrics.FilesRenamed.Add(1)
		slog.Info("renamed in place (hint)", "folder", folderID, "peer", peerAddr,
			"old", oldPath, "new", a.Path)
	}
}

const (
	// G5: exponential backoff parameters for failed file transfers.
	retryBaseDelay = 30 * time.Second // first retry after 30s
	retryMaxDelay  = 30 * time.Minute // cap backoff at 30 minutes
	retryMaxCount  = 20               // stop tracking after this many failures (prevents map growth)
)

// retryTracker tracks per-(file, peer) failure counts with exponential
// backoff. Protected by folderState.indexMu.
//
// C4: the key is (path, peer), not path alone. A peer serving a bad copy
// of a file does not poison the retry budget of other peers — each peer
// accrues its own backoff window for the same path.
type retryTracker struct {
	counts map[retryKey]retryEntry // (path, peer) -> entry
	nowFn  func() time.Time        // injectable clock for testing
}

// retryKey scopes retry state per (path, peer).
type retryKey struct {
	path string
	peer string
}

type retryEntry struct {
	failures   int
	lastHash   Hash256   // remote hash at last failure — reset when it changes
	lastFailed time.Time // wall-clock time of last failure
}

// now returns the current time, using the test clock if set.
func (rt *retryTracker) now() time.Time {
	if rt.nowFn != nil {
		return rt.nowFn()
	}
	return time.Now()
}

// backoffDelay computes the backoff duration for the given failure count.
// Formula: min(baseDelay * 2^(failures-1), maxDelay).
func backoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	delay := retryBaseDelay
	for i := 1; i < failures && delay < retryMaxDelay; i++ {
		delay *= 2
	}
	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}
	return delay
}

// record increments the failure count for (path, peer). If the remote
// hash changed since the last failure, the counter resets (the file was
// updated upstream).
func (rt *retryTracker) record(path, peer string, remoteHash Hash256) {
	if rt.counts == nil {
		rt.counts = make(map[retryKey]retryEntry)
	}
	k := retryKey{path: path, peer: peer}
	e := rt.counts[k]
	if e.lastHash != remoteHash {
		e = retryEntry{lastHash: remoteHash}
	}
	e.failures++
	if e.failures > retryMaxCount {
		e.failures = retryMaxCount // cap to prevent unbounded growth
	}
	e.lastFailed = rt.now()
	rt.counts[k] = e
}

// quarantined reports whether the (path, peer) pair should be skipped
// this cycle. Returns true when the backoff period has not yet elapsed
// since the last failure. A new remote hash always clears the backoff.
func (rt *retryTracker) quarantined(path, peer string, remoteHash Hash256) bool {
	if rt.counts == nil {
		return false
	}
	e, ok := rt.counts[retryKey{path: path, peer: peer}]
	if !ok {
		return false
	}
	// New remote version — backoff resets.
	if e.lastHash != remoteHash {
		return false
	}
	return rt.now().Sub(e.lastFailed) < backoffDelay(e.failures)
}

// clear removes (path, peer) from tracking after a successful sync with
// that peer.
func (rt *retryTracker) clear(path, peer string) {
	delete(rt.counts, retryKey{path: path, peer: peer})
}

// clearAll removes every (path, *) entry. Used when the file has been
// synced successfully from some peer — any stale quarantine entries on
// other peers for this version of the file are now moot.
func (rt *retryTracker) clearAll(path string) {
	for k := range rt.counts {
		if k.path == path {
			delete(rt.counts, k)
		}
	}
}

// quarantinedPaths returns the set of paths with at least one peer in
// active backoff. Deduplicated across peers — the dashboard shows paths
// that are currently stalled somewhere.
func (rt *retryTracker) quarantinedPaths() []string {
	now := rt.now()
	seen := make(map[string]struct{})
	for k, e := range rt.counts {
		if now.Sub(e.lastFailed) < backoffDelay(e.failures) {
			seen[k.path] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// R3: peer-level retry tracking. The file-level retryTracker quarantines a
// specific (path, remoteHash) tuple after a hash mismatch — fine-grained and
// keyed on content. It does nothing when the peer itself is unreachable: a
// peer whose sendIndex times out keeps getting retried every cycle, burning
// a goroutine and 2-minute timeout each time.
//
// peerRetryTracker fills that gap. After peerRetryThreshold consecutive
// sendIndex failures the peer enters exponential backoff; syncFolder skips
// it until the window elapses. Any successful sendIndex resets the state.
// Below the threshold, failures are counted but no backoff is applied — a
// transient glitch does not stall sync for the next 30 seconds.
const peerRetryThreshold = 3

type peerRetryState struct {
	failures   int       // consecutive sendIndex failures, capped at retryMaxCount
	lastFailed time.Time // wall-clock time of the last failure
}

// peerRetryTracker is a sibling of retryTracker keyed on peer address.
// Accessed under folderState.indexMu.
type peerRetryTracker struct {
	states map[string]peerRetryState // peerAddr -> state
	nowFn  func() time.Time          // injectable clock for testing
}

func (pt *peerRetryTracker) now() time.Time {
	if pt.nowFn != nil {
		return pt.nowFn()
	}
	return time.Now()
}

// peerBackoffDelay maps failures -> backoff duration. Failures below the
// threshold produce zero delay (no backoff yet). At the threshold, the
// curve matches file-level backoffDelay starting from base.
func peerBackoffDelay(failures int) time.Duration {
	if failures < peerRetryThreshold {
		return 0
	}
	return backoffDelay(failures - peerRetryThreshold + 1)
}

// backedOff reports whether the peer should be skipped this cycle and, if
// so, how long is left in the current backoff window.
func (pt *peerRetryTracker) backedOff(peer string) (bool, time.Duration) {
	if pt.states == nil {
		return false, 0
	}
	s, ok := pt.states[peer]
	if !ok || s.failures < peerRetryThreshold {
		return false, 0
	}
	elapsed := pt.now().Sub(s.lastFailed)
	delay := peerBackoffDelay(s.failures)
	if elapsed >= delay {
		return false, 0
	}
	return true, delay - elapsed
}

// record marks a sendIndex failure for the peer. After peerRetryThreshold
// consecutive failures the peer enters backoff.
func (pt *peerRetryTracker) record(peer string) {
	if pt.states == nil {
		pt.states = make(map[string]peerRetryState)
	}
	s := pt.states[peer]
	s.failures++
	if s.failures > retryMaxCount {
		s.failures = retryMaxCount
	}
	s.lastFailed = pt.now()
	pt.states[peer] = s
}

// clear removes tracking for a peer (e.g., after a successful exchange).
func (pt *peerRetryTracker) clear(peer string) {
	delete(pt.states, peer)
}

// backedOffPeers returns peers currently within their backoff window,
// sorted by address. Admin/dashboard surfaces.
func (pt *peerRetryTracker) backedOffPeers() []string {
	now := pt.now()
	var out []string
	for peer, s := range pt.states {
		if s.failures < peerRetryThreshold {
			continue
		}
		if now.Sub(s.lastFailed) < peerBackoffDelay(s.failures) {
			out = append(out, peer)
		}
	}
	sort.Strings(out)
	return out
}

// Node is the runtime instance for a filesync configuration.
type Node struct {
	cfg      config.FilesyncCfg
	deviceID string
	dataDir  string // ~/.mesh/filesync/

	// folders is populated once in Start() before any goroutine launches
	// and is never modified after that — no lock needed for map reads.
	folders map[string]*folderState

	// tlsFingerprint is the SHA-256 fingerprint of this node's server cert.
	tlsFingerprint string

	// peerClients maps peer address to an http.Client configured with the
	// appropriate TLS fingerprint for that peer. Read-only after Start().
	peerClients map[string]*http.Client

	// peerHasFingerprint records which peer addresses have a configured
	// TLS fingerprint (for TLS status label: "encrypted · verified" vs "encrypted").
	peerHasFingerprint map[string]bool

	// defaultClient is used for peer addresses not in peerClients.
	defaultClient *http.Client

	// rateLimiter throttles file transfer bandwidth. nil means unlimited.
	rateLimiter *rate.Limiter

	// scanTrigger signals the sync loop that a scan completed with changes.
	scanTrigger chan struct{}

	// firstScanDone is closed after the initial scan completes. L4: syncLoop
	// waits on this instead of a fixed timer.
	firstScanDone chan struct{}

	activityMu sync.RWMutex
	activities []SyncActivity // ring buffer, most recent last
}

// clientForPeer returns the http.Client for the given peer address, carrying
// the TLS config appropriate for that peer (fingerprint or encrypt-only).
func (n *Node) clientForPeer(addr string) *http.Client {
	if c, ok := n.peerClients[addr]; ok {
		return c
	}
	return n.defaultClient
}

// tlsStatusFor returns the label shown after a successful exchange with the
// peer at addr. "verified" means the HTTP client for this peer was built with
// a VerifyPeerCertificate callback that checks the configured fingerprint —
// because the caller only invokes this on the success path, the handshake (and
// thus the fingerprint check) already passed. Call sites must not use this on
// the error path; CERT MISMATCH is set there explicitly.
func (n *Node) tlsStatusFor(addr string) string {
	if n.peerHasFingerprint[addr] {
		return "encrypted · verified"
	}
	return "encrypted"
}

// Start initializes and runs the filesync node. Blocks until ctx is cancelled.
func Start(ctx context.Context, cfg config.FilesyncCfg) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home dir: %w", err)
	}
	meshDir := filepath.Join(home, ".mesh")
	dataDir := filepath.Join(meshDir, "filesync")
	initPerfLog(meshDir, cfg.NodeName)

	deviceID, err := loadOrCreateDeviceID(dataDir)
	if err != nil {
		return fmt.Errorf("device id: %w", err)
	}

	// Resolve TLS certificate for this node's HTTP server.
	tlsDir := filepath.Join(meshDir, "tls")
	var serverCert tls.Certificate
	var serverFP string
	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		serverCert, err = tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("load filesync TLS cert: %w", err)
		}
		serverFP = tlsutil.Fingerprint(serverCert)
	} else {
		serverCert, serverFP, err = tlsutil.AutoCert(
			filepath.Join(tlsDir, "filesync.crt"),
			filepath.Join(tlsDir, "filesync.key"),
			"mesh-filesync",
		)
		if err != nil {
			return fmt.Errorf("auto-cert filesync: %w", err)
		}
	}

	// Build per-peer HTTP clients keyed by peer address.
	// Each client carries the TLS config for the expected peer cert fingerprint.
	// No client-level Timeout: governed by ctx and per-call deadlines.
	newTransport := func(tlsCfg *tls.Config) *http.Transport {
		return &http.Transport{
			// Short dial: a blackholed peer should not hold a goroutine for
			// the kernel's default SYN timeout (~2m).
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:       tlsCfg,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	defaultClient := &http.Client{Transport: newTransport(tlsutil.ClientTLS(""))}
	peerClients := make(map[string]*http.Client)
	peerHasFingerprint := make(map[string]bool)
	fpToClient := make(map[string]*http.Client)
	for _, fcfg := range cfg.ResolvedFolders {
		for i, addr := range fcfg.Peers {
			if _, exists := peerClients[addr]; exists {
				continue
			}
			fp := ""
			if i < len(fcfg.PeerFingerprints) {
				fp = fcfg.PeerFingerprints[i]
			}
			if fp != "" {
				peerHasFingerprint[addr] = true
			}
			if c, exists := fpToClient[fp]; exists {
				peerClients[addr] = c
				continue
			}
			c := &http.Client{Transport: newTransport(tlsutil.ClientTLS(fp))}
			fpToClient[fp] = c
			peerClients[addr] = c
		}
	}

	n := &Node{
		cfg:                cfg,
		deviceID:           deviceID,
		dataDir:            dataDir,
		folders:            make(map[string]*folderState),
		tlsFingerprint:     serverFP,
		peerClients:        peerClients,
		peerHasFingerprint: peerHasFingerprint,
		defaultClient:      defaultClient,
		scanTrigger:        make(chan struct{}, 1),
		firstScanDone:      make(chan struct{}),
	}

	// Set up bandwidth limiter.
	if cfg.MaxBandwidth != "" {
		bps, err := config.ParseBandwidth(cfg.MaxBandwidth)
		if err == nil && bps > 0 {
			n.rateLimiter = rate.NewLimiter(rate.Limit(bps), int(min(bps, 1<<20))) // burst up to 1MB or bps
			slog.Info("filesync bandwidth throttle", "bytes_per_sec", bps)
		}
	}

	// Resolve peer hostnames to IPs for incoming request validation.
	// Done here (not at config load) so DNS lookups don't block boot.
	for i := range cfg.ResolvedFolders {
		cfg.ResolvedFolders[i].AllowedPeerHosts = config.ResolveAllowedPeerHosts(
			cfg.ResolvedFolders[i].ID, cfg.ResolvedFolders[i].Peers)
	}

	// Initialize folders.
	for _, fcfg := range cfg.ResolvedFolders {
		// L5: open an os.Root handle for TOCTOU-safe file operations.
		// A missing path (e.g. host-specific mount point not present on this
		// machine) must not abort the whole node — record the folder as
		// failed and continue so the listener and other folders come up.
		folderRoot, rootErr := os.OpenRoot(fcfg.Path)
		if rootErr != nil {
			slog.Warn("filesync folder path missing, skipping", "folder", fcfg.ID, "path", fcfg.Path, "error", rootErr)
			state.Global.Update("filesync-folder", fcfg.ID, state.Failed, "path missing: "+rootErr.Error())
			for _, peer := range fcfg.Peers {
				state.Global.Update("filesync-peer", fcfg.ID+"|"+peer, state.Failed, "folder path missing")
			}
			continue
		}

		// Load or create index.
		idxPath := filepath.Join(dataDir, fcfg.ID, "index.yaml")
		idx, err := loadIndex(idxPath)
		indexReset := false
		if err != nil {
			slog.Warn("failed to load index, starting fresh", "folder", fcfg.ID, "error", err)
			idx = newFileIndex()
			indexReset = true
		}
		idx.recomputeCache()  // P18b: initialize cached counters from loaded data
		idx.rebuildSeqIndex() // PG: build secondary sequence index

		// Load peer states.
		peersPath := filepath.Join(dataDir, fcfg.ID, "peers.yaml")
		peers, err := loadPeerStates(peersPath)
		if err != nil {
			slog.Warn("failed to load peer states, starting fresh", "folder", fcfg.ID, "error", err)
			peers = make(map[string]PeerState)
		}

		// B15: if index was recreated from scratch, reset peer state so stale
		// LastSentSequence doesn't suppress the full index on outbound sync.
		if indexReset && len(peers) > 0 {
			slog.Warn("resetting peer state after index recreation", "folder", fcfg.ID)
			peers = make(map[string]PeerState)
		}

		// Detect path change and warn. The scan will handle the rest correctly:
		// moved dir → same files, no changes; different content → deletions
		// propagate to peers, which is the correct behavior.
		if idx.Path != "" && idx.Path != fcfg.Path {
			slog.Warn("folder path changed, next scan will reconcile",
				"folder", fcfg.ID, "old_path", idx.Path, "new_path", fcfg.Path)
		}
		idx.Path = fcfg.Path
		// C6: pin the local device ID so scan can bump the per-file
		// vector clock on local writes.
		idx.selfID = n.deviceID

		ignore := newIgnoreMatcher(fcfg.IgnorePatterns)

		// Pre-populate conflicts from persisted index so the admin API
		// shows conflict files immediately on restart without waiting
		// for the first scan to walk the filesystem.
		var initConflicts []string
		for path, entry := range idx.Files {
			if !entry.Deleted && isConflictFile(filepath.Base(path)) {
				initConflicts = append(initConflicts, path)
			}
		}
		sort.Strings(initConflicts)

		fs := &folderState{
			cfg:           fcfg,
			root:          folderRoot,
			index:         idx,
			ignore:        ignore,
			peers:         peers,
			conflicts:     initConflicts,
			pending:       make(map[string]PendingSummary),
			peerLastError: make(map[string]string),
			inFlight:      make(map[string]bool),
		}
		n.folders[fcfg.ID] = fs

		// C2: detect network filesystem at startup.
		if fsType, isNet := detectNetworkFS(fcfg.Path); isNet {
			fs.isNetworkFS = true
			slog.Warn("folder root is on a network filesystem — sync durability depends on mount options",
				"folder", fcfg.ID, "path", fcfg.Path, "fstype", fsType)
		}

		if fcfg.Direction == "disabled" {
			state.Global.Update("filesync-folder", fcfg.ID, state.Connected, "disabled")
		} else {
			// Publish folder as Scanning immediately so the dashboard and
			// admin UI show a "loading" state from the first paint — before
			// the initial scan finishes walking the filesystem.
			state.Global.Update("filesync-folder", fcfg.ID, state.Scanning, "initial scan "+fcfg.Path)
			for _, peer := range fcfg.Peers {
				state.Global.Update("filesync-peer", fcfg.ID+"|"+peer, state.Connecting, "")
			}
		}
	}

	// Update global state.
	state.Global.Update("filesync", cfg.Bind, state.Listening, "")
	activeNodes.Register(n)
	clearConfigFolders() // real node data now available — drop config-only fallback
	defer activeNodes.Unregister(n)

	// Start HTTP server.
	srv := &server{node: n}
	httpSrv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout caps the time a slow client can take uploading an
		// index page (capped at maxIndexPayload, 10 MB) or block-signature
		// request — without it a slowloris peer holds a goroutine open
		// after the headers complete. WriteTimeout is deliberately omitted
		// so file downloads (capped at maxSyncFileSize, 4 GB) can stream
		// over slow links without being killed mid-transfer.
		ReadTimeout: 2 * time.Minute,
		IdleTimeout: 60 * time.Second,
		Handler:     srv.handler(),
	}

	tcpLn, err := net.Listen("tcp", cfg.Bind)
	if err != nil {
		state.Global.Update("filesync", cfg.Bind, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", cfg.Bind, err)
	}
	ln := tls.NewListener(tcpLn, tlsutil.ServerTLS(serverCert))

	slog.Info("filesync listening", "bind", ln.Addr().String(), "device_id", deviceID, "folders", len(cfg.ResolvedFolders), "tls_fingerprint", serverFP)
	state.Global.UpdateTLSFingerprint("filesync", cfg.Bind, serverFP)

	var wg sync.WaitGroup

	// HTTP server.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = httpSrv.Serve(ln)
	}()
	context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	})

	// Periodic eviction of stale pending index exchanges.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				srv.evictStalePending()
			}
		}
	}()

	// Filesystem watcher.
	roots := make([]string, 0, len(cfg.ResolvedFolders))
	ignoreMap := make(map[string]*ignoreMatcher)
	for _, fs := range n.folders {
		// Skip disabled and dry-run folders: disabled does nothing, dry-run only
		// compares without modifying files so real-time watching is unnecessary.
		if fs.cfg.Direction == "disabled" || fs.cfg.Direction == "dry-run" {
			continue
		}
		roots = append(roots, fs.cfg.Path)
		ignoreMap[fs.cfg.Path] = fs.ignore
	}

	watcher, watchErr := newFolderWatcher(roots, ignoreMap, cfg.MaxWatches)
	if watchErr != nil {
		slog.Warn("fsnotify unavailable, relying on periodic scan only", "error", watchErr)
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watcher.run(ctx)
		}()
		context.AfterFunc(ctx, func() { _ = watcher.close() })
	}

	// Scan loop.
	scanInterval := 60 * time.Second
	if cfg.ScanInterval != "" {
		if d, err := time.ParseDuration(cfg.ScanInterval); err == nil {
			scanInterval = d
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.scanLoop(ctx, scanInterval, watcher)
	}()

	// Sync loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		n.syncLoop(ctx)
	}()

	// Periodic performance snapshots (process-level metrics).
	wg.Add(1)
	go func() {
		defer wg.Done()
		snapTicker := time.NewTicker(1 * time.Minute)
		defer snapTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-snapTicker.C:
				perfSnapshot(n.folders)
			}
		}
	}()

	// Run initial scan (all folders).
	n.runScan(ctx, nil)
	close(n.firstScanDone)

	<-ctx.Done()

	// Persist state before exit.
	n.persistAll()

	wg.Wait()

	// Close idle HTTP connections after all goroutines have stopped.
	for _, c := range n.peerClients {
		c.CloseIdleConnections()
	}
	n.defaultClient.CloseIdleConnections()

	closePerfLog()

	// L5: close os.Root handles.
	for _, fs := range n.folders {
		if fs.root != nil {
			_ = fs.root.Close()
		}
	}

	// Clean up state.
	for _, fcfg := range cfg.ResolvedFolders {
		state.Global.Delete("filesync-folder", fcfg.ID)
		for _, peer := range fcfg.Peers {
			state.Global.Delete("filesync-peer", fcfg.ID+"|"+peer)
		}
	}
	state.Global.Delete("filesync", cfg.Bind)

	return nil
}

// scanLoop runs periodic scans and reacts to fsnotify triggers.
func (n *Node) scanLoop(ctx context.Context, interval time.Duration, watcher *folderWatcher) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var dirtyCh <-chan struct{}
	if watcher != nil {
		dirtyCh = watcher.dirtyCh
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Periodic: scan all folders as a safety net.
			n.runScan(ctx, nil)
		case <-dirtyCh:
			// Targeted: only scan the folders whose roots received events.
			dirtyRoots := watcher.drainDirtyRoots()
			n.runScan(ctx, dirtyRoots)
		}
	}
}

// runScan scans folders and triggers sync if anything changed.
// When dirtyRoots is non-nil, only folders whose path is in the set are
// scanned (targeted fsnotify trigger).  When nil, all folders are scanned
// (periodic safety-net).
// The FS walk runs against a private copy of the index so readers
// (admin UI, dashboard, syncLoop) are never blocked by a long scan.
// Bails out between folders if ctx is cancelled so shutdown doesn't wait for
// multi-minute WalkDirs over huge folders (m2 repo, build outputs).
func (n *Node) runScan(ctx context.Context, dirtyRoots map[string]bool) {
	anyChanged := false
	for id, fs := range n.folders {
		if ctx.Err() != nil {
			return
		}
		if fs.cfg.Direction == "disabled" {
			continue
		}
		// When dirtyRoots is set, skip folders not affected by fsnotify events.
		if dirtyRoots != nil && !dirtyRoots[fs.cfg.Path] {
			continue
		}

		folderStart := time.Now()

		// Snapshot the current index under a short read lock so the walk
		// operates on a private copy. Readers (GetFolderStatuses, syncLoop)
		// see the old index until we swap.
		snapStart := time.Now()
		// P18c: acquire the write lock briefly for setup. We take ownership
		// of reusableFiles (the map recycled from the previous scan), then
		// clone into it. Holding write lock for the clone matches the prior
		// RLock duration — readers still observe the old index; writers
		// (sync downloads) wait for the same clone window they did before.
		fs.indexMu.Lock()
		recycled := fs.reusableFiles
		fs.reusableFiles = nil
		idxCopy := fs.index.cloneInto(recycled)
		scanStartSeq := fs.index.Sequence
		ignore := fs.ignore
		existingFiles := len(fs.index.Files)
		peersCopy := make(map[string]PeerState, len(fs.peers))
		for k, v := range fs.peers {
			peersCopy[k] = v
		}
		fs.indexMu.Unlock()
		snapDuration := time.Since(snapStart)

		state.Global.Update("filesync-folder", id, state.Scanning, "scanning "+fs.cfg.Path)

		maxFiles := fs.cfg.MaxFiles
		if maxFiles <= 0 {
			maxFiles = defaultMaxIndexFiles
		}
		changed, count, dirs, stats, conflicts, err := idxCopy.scanWithStats(ctx, fs.cfg.Path, ignore, maxFiles)
		if errors.Is(err, errIndexCapExceeded) {
			slog.Error("scan aborted: folder exceeds max tracked files",
				"folder", id, "max_files", maxFiles, "path", fs.cfg.Path)
			state.Global.Update("filesync-folder", id, state.Retrying,
				fmt.Sprintf("exceeds %d file limit", maxFiles))
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("scan error", "folder", id, "error", err)
		}

		purgeStart := time.Now()
		purged := idxCopy.purgeTombstones(tombstoneMaxAge, peersCopy)
		purgeDuration := time.Since(purgeStart)

		// Swap under a short write lock. Merge-preserve any entries that were
		// written after the scan started (concurrent sync downloads bumped
		// Sequence past scanStartSeq) so we don't clobber their work.
		swapStart := time.Now()
		fs.indexMu.Lock()
		mergedBack := 0
		for path, live := range fs.index.Files {
			if live.Sequence > scanStartSeq {
				idxCopy.Files[path] = live
				if live.Sequence > idxCopy.Sequence {
					idxCopy.Sequence = live.Sequence
				}
				mergedBack++
			}
		}
		if fs.index.Sequence > idxCopy.Sequence {
			idxCopy.Sequence = fs.index.Sequence
		}
		// P18c: stash the old Files map for reuse by the next runScan. Safe
		// because no reader retains a reference across indexMu boundaries —
		// every lookup re-reads fs.index.Files under the lock. The old index
		// struct is discarded; only its map backing is recycled.
		fs.reusableFiles = fs.index.Files
		fs.index = idxCopy
		fs.index.recomputeCache()  // P18b: refresh after scan+merge
		fs.index.rebuildSeqIndex() // PG: rebuild secondary sequence index
		fs.dirCount = dirs
		if conflicts != nil {
			fs.conflicts = conflicts
		}
		// M3: maintain removed-peer markers on the live peers map.
		peersChanged := markRemovedPeers(fs.peers, fs.cfg.Peers)
		peersChanged = gcRemovedPeers(fs.peers, tombstoneMaxAge) > 0 || peersChanged
		if changed {
			fs.indexDirty = true // P17a
		}
		if peersChanged {
			fs.peersDirty = true // P17a
		}
		countAfterSwap, totalSize := fs.index.activeCountAndSize()
		fs.indexMu.Unlock()
		swapDuration := time.Since(swapStart)

		// Persist peer state when M3 maintenance modified it, so changes
		// survive restarts without waiting for the next syncFolder call.
		if peersChanged {
			n.persistFolder(id, false)
		}

		fs.initialScanDone.Store(true)
		state.Global.Update("filesync-folder", id, state.Connected, "idle")
		state.Global.UpdateFileCount("filesync-folder", id, countAfterSwap, totalSize)

		total := time.Since(folderStart)
		// Emit at DEBUG: volume-sensitive, but every field is evidence for
		// attributing slow scans to a concrete phase. Never enabled in
		// production steady-state; operators flip to debug to investigate.
		slog.Debug("filesync scan complete",
			"folder", id,
			"path", fs.cfg.Path,
			"total", total,
			"walk", stats.WalkDuration,
			"hash", stats.HashDuration,
			"stat", stats.StatDuration,
			"ignore_check", stats.IgnoreDuration,
			"deletion_scan", stats.DeletionScan,
			"snapshot", snapDuration,
			"tombstone_purge", purgeDuration,
			"conflicts_found", len(conflicts),
			"swap", swapDuration,
			"entries_visited", stats.EntriesVisited,
			"dirs_walked", stats.DirsWalked,
			"dirs_ignored", stats.DirsIgnored,
			"files_ignored", stats.FilesIgnored,
			"symlinks_skipped", stats.SymlinksSkipped,
			"temp_cleaned", stats.TempCleaned,
			"fast_path_hits", stats.FastPathHits,
			"files_hashed", stats.FilesHashed,
			"bytes_hashed", stats.BytesHashed,
			"stat_errors", stats.StatErrors,
			"hash_errors", stats.HashErrors,
			"toctou_skips", stats.TocTouSkips,
			"deletions", stats.Deletions,
			"tombstones_purged", purged,
			"merged_back", mergedBack,
			"existing_files", existingFiles,
			"active_files", count,
			"changed", changed,
		)
		perfScan(id, stats, countAfterSwap, dirs, changed, ms(snapDuration), ms(purgeDuration), ms(swapDuration))

		// Always log the first scan per folder at INFO so a single run gives
		// baseline evidence (walk/hash time, file counts, FD-pressure phase)
		// without requiring DEBUG. Subsequent scans only log INFO when
		// pathological: >2s, >100 MB hashed, or >1000 files rehashed.
		slow := total > 2*time.Second || stats.BytesHashed > 100<<20 || stats.FilesHashed > 1000
		if !fs.firstScanLogged.Load() || slow {
			reason := "first"
			if slow {
				reason = "slow"
			}
			slog.Info("filesync scan",
				"reason", reason,
				"folder", id,
				"total", total,
				"walk", stats.WalkDuration,
				"hash", stats.HashDuration,
				"stat", stats.StatDuration,
				"ignore_check", stats.IgnoreDuration,
				"deletion_scan", stats.DeletionScan,
				"entries_visited", stats.EntriesVisited,
				"dirs_walked", stats.DirsWalked,
				"dirs_ignored", stats.DirsIgnored,
				"files_ignored", stats.FilesIgnored,
				"fast_path_hits", stats.FastPathHits,
				"files_hashed", stats.FilesHashed,
				"bytes_hashed", stats.BytesHashed,
				"stat_errors", stats.StatErrors,
				"hash_errors", stats.HashErrors,
				"toctou_skips", stats.TocTouSkips,
				"active_files", count,
			)
			fs.firstScanLogged.Store(true)
		}

		// Update scan metrics.
		fs.metrics.ScanCount.Add(1)
		fs.metrics.ScanDurationNS.Store(total.Nanoseconds())

		if changed {
			anyChanged = true
		}
	}

	if anyChanged {
		select {
		case n.scanTrigger <- struct{}{}:
		default:
		}
	}
}

// syncLoop periodically reconciles with all configured peers.
func (n *Node) syncLoop(ctx context.Context) {
	// L4: wait for the initial scan to complete instead of a fixed timer.
	select {
	case <-ctx.Done():
		return
	case <-n.firstScanDone:
	}

	// Sync immediately, then on trigger or timer.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		n.syncAllPeers(ctx)

		select {
		case <-ctx.Done():
			return
		case <-n.scanTrigger:
		case <-ticker.C:
		}
	}
}

// syncAllPeers runs one sync cycle against all peers for all folders.
// F1: folder×peer pairs run concurrently so a slow/dead peer doesn't
// block other folders. The shared sem still limits total concurrent
// file transfers across all pairs.
func (n *Node) syncAllPeers(ctx context.Context) {
	sem := make(chan struct{}, n.cfg.MaxConcurrent)

	var wg sync.WaitGroup
	for _, fs := range n.folders {
		if fs.cfg.Direction == "disabled" {
			continue
		}
		for _, peer := range fs.cfg.Peers {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.syncFolder(ctx, fs, peer, sem)
			}()
		}
	}
	wg.Wait()
}

// syncFolder exchanges indices with a peer and downloads missing/newer files.
func (n *Node) syncFolder(ctx context.Context, fs *folderState, peerAddr string, sem chan struct{}) {
	folderID := fs.cfg.ID
	stateKey := folderID + "|" + peerAddr

	// R3: if the peer has hit consecutive-failure threshold, skip this cycle
	// until the backoff window expires. Each folder tracks its own view of
	// the peer, so a healthy folder→peer pair is not penalised by another
	// folder's troubles with the same peer.
	fs.indexMu.RLock()
	backedOff, remaining := fs.peerRetries.backedOff(peerAddr)
	fs.indexMu.RUnlock()
	if backedOff {
		slog.Debug("peer in backoff, skipping sync cycle",
			"folder", folderID, "peer", peerAddr, "remaining", remaining)
		state.Global.Update("filesync-peer", stateKey, state.Retrying,
			fmt.Sprintf("backoff %s", remaining.Round(time.Second)))
		return
	}

	// L6: verify folder root still exists before syncing. If the folder was
	// unmounted or deleted after startup, downloading files would recreate
	// the directory tree via MkdirAll — producing a zombie folder.
	if _, err := os.Stat(fs.cfg.Path); err != nil {
		slog.Warn("folder root gone, skipping sync", "folder", folderID, "path", fs.cfg.Path, "error", err)
		state.Global.Update("filesync-folder", folderID, state.Failed, "path missing")
		return
	}

	syncStart := time.Now()
	state.Global.Update("filesync-folder", folderID, state.Connecting, "syncing with "+peerAddr)

	// Build and send our index, requesting only entries newer than what we've seen.
	fs.indexMu.RLock()
	peerLastSeq := int64(0)
	ourLastSentSeq := int64(0)
	if ps, ok := fs.peers[peerAddr]; ok {
		peerLastSeq = ps.LastSeenSequence
		ourLastSentSeq = ps.LastSentSequence
	}
	fs.indexMu.RUnlock()

	exchange := n.buildIndexExchange(folderID, ourLastSentSeq) // send only entries newer than last sent
	// N2: capture sequence from the exchange (which reads the live index under
	// its own RLock) rather than from a stale pre-exchange snapshot. This
	// ensures LastSentSequence accurately reflects what was actually sent.
	ourCurrentSeq := exchange.GetSequence()
	exchange.Since = peerLastSeq // ask peer to send only entries newer than this
	slog.Debug("index exchange prepared",
		"folder", folderID, "peer", peerAddr,
		"our_seq", ourCurrentSeq, "peer_last_seen", peerLastSeq,
		"our_last_sent", ourLastSentSeq, "entries_out", len(exchange.GetFiles()))

	// Per-peer timeout prevents a hung peer from pinning this goroutine
	// for the node's entire lifetime. Index exchanges are small; 2 minutes
	// is generous even for paginated multi-page exchanges over slow links.
	indexCtx, indexCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer indexCancel()
	remoteIdx, err := sendIndex(indexCtx, n.clientForPeer(peerAddr), peerAddr, exchange)
	if err != nil {
		slog.Debug("sync failed", "folder", folderID, "peer", peerAddr, "error", err)
		state.Global.Update("filesync-peer", stateKey, state.Retrying, err.Error())
		if errors.Is(err, tlsutil.ErrFingerprintMismatch) {
			state.Global.UpdateTLSStatus("filesync-peer", stateKey, "CERT MISMATCH")
		}
		fs.indexMu.Lock()
		fs.peerLastError[peerAddr] = err.Error()
		fs.peerRetries.record(peerAddr) // R3
		failures := fs.peerRetries.states[peerAddr].failures
		fs.indexMu.Unlock()
		if failures == peerRetryThreshold {
			slog.Warn("peer entered backoff after consecutive failures",
				"folder", folderID, "peer", peerAddr, "failures", failures)
		}
		n.recordActivity(SyncActivity{
			Time:   time.Now(),
			Folder: folderID,
			Peer:   peerAddr,
			Error:  err.Error(),
		})
		return
	}

	fs.indexMu.Lock()
	delete(fs.peerLastError, peerAddr)
	fs.peerRetries.clear(peerAddr) // R3: healthy exchange resets backoff
	fs.indexMu.Unlock()
	remoteEntries := len(remoteIdx.GetFiles())
	fs.metrics.IndexExchanges.Add(1)
	slog.Debug("index exchange complete",
		"folder", folderID, "peer", peerAddr,
		"remote_seq", remoteIdx.GetSequence(), "remote_entries", remoteEntries,
		"duration", time.Since(syncStart))

	state.Global.Update("filesync-peer", stateKey, state.Connected, "")
	state.Global.UpdateTLSStatus("filesync-peer", stateKey, n.tlsStatusFor(peerAddr))

	// Detect peer restart: if remote sequence dropped below what we last saw,
	// reset tracking and request a full exchange on the next cycle.
	if remoteIdx.GetSequence() < peerLastSeq {
		slog.Info("peer sequence reset detected, will do full exchange next cycle",
			"folder", folderID, "peer", peerAddr,
			"remote_seq", remoteIdx.GetSequence(), "last_seen", peerLastSeq)
		fs.indexMu.Lock()
		// H2b: preserve LastEpoch from old state so the epoch change is
		// visible on the next cycle when diff actually runs.
		var lastEpoch string
		if old, ok := fs.peers[peerAddr]; ok {
			lastEpoch = old.LastEpoch
		}
		// Store remote's new epoch as PendingEpoch. On the next cycle's
		// diff, downloads for locally-tombstoned files will be filtered.
		var pendingEpoch string
		if remoteEpoch := remoteIdx.GetEpoch(); remoteEpoch != "" && remoteEpoch != lastEpoch {
			pendingEpoch = remoteEpoch
		}
		fs.peers[peerAddr] = PeerState{
			LastSeenSequence: 0,
			LastSentSequence: 0,
			LastSync:         time.Now(),
			LastEpoch:        lastEpoch,
			PendingEpoch:     pendingEpoch,
		}
		fs.peersDirty = true // P17a
		fs.indexMu.Unlock()
		n.persistFolder(folderID, false)
		return
	}

	// Convert remote protobuf index to our internal format for diffing.
	remoteFileIndex := protoToFileIndex(remoteIdx)

	// F5: diff() only reads the index — use RLock to avoid blocking scans
	// and admin API reads. Upgrade to Lock only for the pending update.
	// H2: re-read peerLastSeq here (not from the stale pre-exchange snapshot)
	// so concurrent downloads that bumped Sequence are visible to diff().
	fs.indexMu.RLock()
	lastSeenSeq := int64(0)
	var lastSyncNS int64
	var baseHashes map[string]Hash256
	var pendingEpoch string
	if ps, ok := fs.peers[peerAddr]; ok {
		lastSeenSeq = ps.LastSeenSequence
		if !ps.LastSync.IsZero() {
			lastSyncNS = ps.LastSync.UnixNano()
		}
		// C2: snapshot ancestor map for read-only use by diff(). The
		// post-sync update mutates fs.peers under indexMu.Lock(), so the
		// snapshot seen here remains valid for the duration of diff().
		baseHashes = ps.BaseHashes
		pendingEpoch = ps.PendingEpoch
	}
	actions := fs.index.diff(remoteFileIndex, lastSeenSeq, lastSyncNS, baseHashes, fs.cfg.Direction)

	// H2b: when an epoch change was detected on the previous cycle (restart
	// detection stored PendingEpoch), filter out downloads for files we have
	// locally as tombstones. The reset peer lost its index and re-advertised
	// everything — our tombstones are authoritative.
	if pendingEpoch != "" {
		filtered := 0
		n := 0
		for _, a := range actions {
			if a.Action == ActionDownload {
				if le, ok := fs.index.Files[a.Path]; ok && le.Deleted {
					filtered++
					continue
				}
			}
			actions[n] = a
			n++
		}
		actions = actions[:n]
		if filtered > 0 {
			slog.Info("epoch guard filtered resurrected files",
				"folder", folderID, "peer", peerAddr,
				"filtered", filtered, "remaining", len(actions))
		}
	}
	fs.indexMu.RUnlock()

	slog.Debug("diff computed",
		"folder", folderID, "peer", peerAddr, "actions", len(actions),
		"direction", fs.cfg.Direction)

	fs.indexMu.Lock()
	if len(actions) == 0 {
		delete(fs.pending, peerAddr)
	} else {
		fs.pending[peerAddr] = buildPendingSummary(actions)
	}
	fs.indexMu.Unlock()

	// H2b: resolve epoch for the updated peer state. If PendingEpoch was
	// set (epoch change in progress), promote it to LastEpoch. Otherwise
	// track the remote's current epoch.
	resolvedEpoch := remoteIdx.GetEpoch()
	if pendingEpoch != "" {
		resolvedEpoch = pendingEpoch
	}

	if len(actions) == 0 {
		// Update peer state even when nothing to do.
		fs.indexMu.Lock()
		// C2: preserve ancestor hashes across the PeerState rewrite, and
		// extend them with any paths that now agree (same SHA on both
		// sides) thanks to this exchange.
		prior := fs.peers[peerAddr].BaseHashes
		fs.peers[peerAddr] = PeerState{
			LastSeenSequence: remoteIdx.GetSequence(),
			LastSentSequence: ourCurrentSeq,
			LastSync:         time.Now(),
			LastEpoch:        resolvedEpoch,
			BaseHashes:       updateBaseHashes(prior, fs.index, remoteFileIndex),
		}
		fs.peersDirty = true // P17a
		fs.indexMu.Unlock()

		// P17a: persist peers only (index unchanged); skips the expensive
		// index serialize for idle folders.
		n.persistFolder(folderID, false)

		fs.indexMu.RLock()
		count, totalSize := fs.index.activeCountAndSize()
		fs.indexMu.RUnlock()
		now := time.Now()
		state.Global.Update("filesync-folder", folderID, state.Connected, "idle")
		state.Global.UpdateFileCount("filesync-folder", folderID, count, totalSize)
		state.Global.UpdateLastSync("filesync-folder", folderID, now)
		return
	}

	var downloads, conflicts, deletes int
	var totalBytes int64
	for _, a := range actions {
		switch a.Action {
		case ActionDownload:
			downloads++
		case ActionConflict:
			conflicts++
		case ActionDelete:
			deletes++
		}
		totalBytes += a.RemoteSize
	}
	slog.Info("sync actions", "folder", folderID, "peer", peerAddr, "downloads", downloads, "conflicts", conflicts, "deletes", deletes)

	// Dry-run: log what would happen but don't modify files or update peer state.
	if fs.cfg.Direction == "dry-run" {
		state.Global.Update("filesync-folder", folderID, state.Connected,
			fmt.Sprintf("dry-run: %d downloads, %d deletes, %d conflicts", downloads, deletes, conflicts))
		state.Global.UpdateLastSync("filesync-folder", folderID, time.Now())
		return
	}

	state.Global.Update("filesync-folder", folderID, state.Connecting, fmt.Sprintf("syncing %d files", len(actions)))

	// B9: track failed remote sequences so we don't advance
	// LastSeenSequence past entries that failed to process.
	// PJ: capture the first failure reason for the perf log.
	var failMu sync.Mutex
	var failedSeqs []int64
	var firstFailReason string
	setFailReason := func(reason string) {
		if firstFailReason == "" {
			firstFailReason = reason
		}
	}

	// R1: plan receiver-side renames. Any (delete old, download new) pair
	// where the local file at old already has the new hash is satisfied
	// by a local rename — no bytes cross the wire. Successful renames
	// mark both paths in renamedPaths so the bundle and per-action loops
	// skip them. Failures remove entries from renamedPaths so the normal
	// download/delete path still runs.
	renamedPaths := make(map[string]bool)
	{
		fs.indexMu.RLock()
		plans, skip := planRenames(actions, fs.index)
		fs.indexMu.RUnlock()
		if len(plans) > 0 {
			for p := range skip {
				renamedPaths[p] = true
			}
			for _, rp := range plans {
				if ctx.Err() != nil {
					break
				}
				if !fs.claimPath(rp.OldPath) {
					delete(renamedPaths, rp.OldPath)
					delete(renamedPaths, rp.NewPath)
					continue
				}
				if !fs.claimPath(rp.NewPath) {
					fs.releasePath(rp.OldPath)
					delete(renamedPaths, rp.OldPath)
					delete(renamedPaths, rp.NewPath)
					continue
				}
				// Refuse to clobber a non-empty target on the filesystem
				// even if the in-memory index had no entry (drift safety).
				if _, err := fs.root.Stat(rp.NewPath); err == nil {
					fs.releasePath(rp.OldPath)
					fs.releasePath(rp.NewPath)
					delete(renamedPaths, rp.OldPath)
					delete(renamedPaths, rp.NewPath)
					continue
				}
				if err := fs.root.Rename(rp.OldPath, rp.NewPath); err != nil {
					slog.Debug("rename fallback to download", "folder", folderID,
						"old", rp.OldPath, "new", rp.NewPath, "error", err)
					fs.releasePath(rp.OldPath)
					fs.releasePath(rp.NewPath)
					delete(renamedPaths, rp.OldPath)
					delete(renamedPaths, rp.NewPath)
					continue
				}

				if rp.RemoteMtime > 0 {
					mt := time.Unix(0, rp.RemoteMtime)
					_ = fs.root.Chtimes(rp.NewPath, mt, mt)
				}
				fileMode := os.FileMode(rp.RemoteMode)
				if fileMode == 0 {
					fileMode = 0644
				}
				_ = fs.root.Chmod(rp.NewPath, fileMode)

				fs.indexMu.Lock()
				// Tombstone the old path.
				oldEntry := fs.index.Files[rp.OldPath]
				if !oldEntry.Deleted {
					fs.index.Sequence++
					oldEntry.Deleted = true
					oldEntry.MtimeNS = time.Now().UnixNano()
					oldEntry.Sequence = fs.index.Sequence
					fs.index.setEntry(rp.OldPath, oldEntry)
				}
				// Write the new-path entry with remote metadata.
				fs.index.Sequence++
				fs.index.setEntry(rp.NewPath, FileEntry{
					Size:     rp.RemoteSize,
					MtimeNS:  rp.RemoteMtime,
					SHA256:   rp.RemoteHash,
					Sequence: fs.index.Sequence,
					Mode:     rp.RemoteMode,
					// C6: adopt the peer's vector clock — this write
					// reflects their observation, not ours.
					Version: rp.RemoteVersion.clone(),
				})
				fs.retries.clearAll(rp.OldPath)
				fs.retries.clearAll(rp.NewPath)
				fs.indexMu.Unlock()

				fs.releasePath(rp.OldPath)
				fs.releasePath(rp.NewPath)

				fs.metrics.FilesRenamed.Add(1)
				fs.metrics.BytesSavedByRename.Add(rp.RemoteSize)
				slog.Info("renamed in place", "folder", folderID, "peer", peerAddr,
					"old", rp.OldPath, "new", rp.NewPath, "bytes", rp.RemoteSize)
			}
		}
	}

	fs.applyHintRenames(ctx, folderID, peerAddr, actions, renamedPaths)

	// P19: bundle transfer for small download actions. Partition into
	// small (≤4 MB) and the rest. Small files are batched into tar+gzip
	// bundles to eliminate per-file HTTP round-trips.
	bundledPaths := make(map[string]bool)
	{
		var smallEntries []bundleEntry
		for _, a := range actions {
			if a.Action == ActionDownload && a.RemoteSize > 0 && a.RemoteSize <= maxBundleFileSize {
				// R1: skip paths already handled by local rename.
				if renamedPaths[a.Path] {
					continue
				}
				// Pre-check: skip quarantined and in-flight.
				fs.indexMu.RLock()
				q := fs.retries.quarantined(a.Path, peerAddr, a.RemoteHash)
				fs.indexMu.RUnlock()
				if q {
					continue
				}
				smallEntries = append(smallEntries, bundleEntry{
					Path:         a.Path,
					ExpectedHash: a.RemoteHash,
					RemoteSize:   a.RemoteSize,
					RemoteMode:   a.RemoteMode,
					RemoteMtime:  a.RemoteMtime,
				})
			}
		}

		if len(smallEntries) >= 2 { // only batch when there's something to batch
			// F7: build action lookup once, not per batch.
			actionMap := make(map[string]DiffEntry, len(actions))
			for _, a := range actions {
				if a.Action == ActionDownload {
					actionMap[a.Path] = a
				}
			}
			batches := bundleBatches(smallEntries)
			for _, batch := range batches {
				if ctx.Err() != nil {
					break
				}
				// Claim all paths in this batch.
				var claimed []bundleEntry
				for _, e := range batch {
					if fs.claimPath(e.Path) {
						claimed = append(claimed, e)
					}
				}
				if len(claimed) == 0 {
					continue
				}

				ok, retry := downloadBundle(ctx, n.clientForPeer(peerAddr), peerAddr, folderID, claimed, fs.root, n.rateLimiter)
				for _, path := range ok {
					bundledPaths[path] = true
					fs.releasePath(path)
				}
				for _, e := range retry {
					fs.releasePath(e.Path)
				}

				// F2: record failed bundle entries so safeSeq does not
				// advance LastSeenSequence past unprocessed files. Without
				// this, retry entries whose individual download also fails
				// (e.g., claimPath collision) are silently skipped on the
				// next sync cycle because diff() sees their sequence as
				// already processed.
				if len(retry) > 0 {
					// Build actionMap to look up RemoteSequence.
					retryMap := make(map[string]bool, len(retry))
					for _, e := range retry {
						retryMap[e.Path] = true
					}
					failMu.Lock()
					setFailReason("bundle retry")
					for _, a := range actions {
						if retryMap[a.Path] {
							failedSeqs = append(failedSeqs, a.RemoteSequence)
						}
					}
					failMu.Unlock()
				}

				// Update index for successful bundle downloads.
				if len(ok) > 0 {
					fs.indexMu.Lock()
					for _, path := range ok {
						a := actionMap[path]
						fs.index.Sequence++
						fs.index.setEntry(path, FileEntry{
							Size:     a.RemoteSize,
							MtimeNS:  a.RemoteMtime,
							SHA256:   a.RemoteHash,
							Sequence: fs.index.Sequence,
							Mode:     a.RemoteMode,
							// C6: adopt the peer's clock on successful download.
							Version: a.RemoteVersion.clone(),
						})
						fs.retries.clearAll(path)
					}
					fs.indexMu.Unlock()

					m := state.Global.GetMetrics("filesync", n.cfg.Bind)
					for _, path := range ok {
						a := actionMap[path]
						m.BytesRx.Add(a.RemoteSize)
						fs.metrics.FilesDownloaded.Add(1)
						fs.metrics.BytesDownloaded.Add(a.RemoteSize)
					}
				}
				slog.Info("bundle download", "folder", folderID, "peer", peerAddr,
					"requested", len(claimed), "ok", len(ok), "retry", len(retry))
			}
		}
	}

	var wg sync.WaitGroup
	var skippedQuarantine int
	for _, action := range actions {
		if ctx.Err() != nil {
			break
		}

		// R1: skip actions already satisfied by local rename.
		if renamedPaths[action.Path] {
			continue
		}

		// P19: skip actions already handled by bundle download.
		if bundledPaths[action.Path] {
			continue
		}

		// SR12: skip files quarantined after repeated failures.
		fs.indexMu.RLock()
		q := fs.retries.quarantined(action.Path, peerAddr, action.RemoteHash)
		fs.indexMu.RUnlock()
		if q {
			skippedQuarantine++
			failMu.Lock()
			setFailReason("quarantined")
			failedSeqs = append(failedSeqs, action.RemoteSequence)
			failMu.Unlock()
			continue
		}

		// H3: skip if another peer goroutine is already downloading
		// the same path in this sync round. The in-flight set prevents
		// concurrent writes to the same file and index entry.
		if !fs.claimPath(action.Path) {
			slog.Debug("skipping in-flight path", "folder", folderID, "path", action.Path, "peer", peerAddr)
			failMu.Lock()
			setFailReason("in-flight collision")
			failedSeqs = append(failedSeqs, action.RemoteSequence)
			failMu.Unlock()
			continue
		}

		switch action.Action {
		case ActionDownload:
			wg.Add(1)
			action := action
			go func() {
				defer wg.Done()
				defer fs.releasePath(action.Path)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				dlStart := time.Now()
				err := downloadFromPeer(ctx, n.clientForPeer(peerAddr), peerAddr, folderID, action.Path, action.RemoteHash, fs.root, n.rateLimiter)
				if err != nil {
					slog.Warn("download failed", "folder", folderID, "path", action.Path, "peer", peerAddr, "error", err)
					fs.indexMu.Lock()
					fs.retries.record(action.Path, peerAddr, action.RemoteHash)
					fs.indexMu.Unlock()
					failMu.Lock()
					setFailReason("download: " + err.Error())
					failedSeqs = append(failedSeqs, action.RemoteSequence)
					failMu.Unlock()
					fs.metrics.SyncErrors.Add(1)
					return
				}
				slog.Debug("file downloaded", "folder", folderID, "path", action.Path,
					"peer", peerAddr, "size", action.RemoteSize, "duration", time.Since(dlStart))

				// C2: post-write verification on network filesystems.
				if fs.isNetworkFS {
					if err := verifyPostWrite(fs.root, action.Path, action.RemoteHash, folderID, peerAddr, &fs.retries, &fs.indexMu); err != nil {
						failMu.Lock()
						setFailReason("post-write verify: " + err.Error())
						failedSeqs = append(failedSeqs, action.RemoteSequence)
						failMu.Unlock()
						fs.metrics.SyncErrors.Add(1)
						return
					}
				}

				// G1: preserve remote mtime so the next scan's fast-path skip works.
				if action.RemoteMtime > 0 {
					mt := time.Unix(0, action.RemoteMtime)
					_ = fs.root.Chtimes(action.Path, mt, mt)
				}

				// L1: apply file permissions from remote (default 0644).
				fileMode := os.FileMode(action.RemoteMode)
				if fileMode == 0 {
					fileMode = 0644
				}
				_ = fs.root.Chmod(action.Path, fileMode)

				// Update metrics.
				m := state.Global.GetMetrics("filesync", n.cfg.Bind)
				m.BytesRx.Add(action.RemoteSize)
				fs.metrics.FilesDownloaded.Add(1)
				fs.metrics.BytesDownloaded.Add(action.RemoteSize)

				// Update local index and clear retry tracking on success.
				fs.indexMu.Lock()
				fs.index.Sequence++
				fs.index.setEntry(action.Path, FileEntry{
					Size:     action.RemoteSize,
					MtimeNS:  action.RemoteMtime,
					SHA256:   action.RemoteHash,
					Sequence: fs.index.Sequence,
					Mode:     action.RemoteMode,
					// C6: adopt the peer's clock on successful download.
					Version: action.RemoteVersion.clone(),
				})
				fs.retries.clearAll(action.Path)
				fs.indexMu.Unlock()

				slog.Info("synced file", "folder", folderID, "path", action.Path, "peer", peerAddr)
			}()

		case ActionConflict:
			wg.Add(1)
			action := action
			lmtime := localMtime(fs, action.Path) // snapshot before goroutine
			go func() {
				defer wg.Done()
				defer fs.releasePath(action.Path)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				// C1: same-hash conflict resolution. Re-hash the local file
				// from disk (never trust cached index hash) and compare with
				// the remote hash. If identical, adopt remote metadata — no
				// conflict file needed, no download.
				if !action.RemoteHash.IsZero() {
					if localHash, hashErr := hashFileRoot(fs.root, action.Path); hashErr == nil && localHash == action.RemoteHash {
						// G1: set disk mtime to match index so next scan uses fast-path.
						if action.RemoteMtime > 0 {
							mt := time.Unix(0, action.RemoteMtime)
							_ = fs.root.Chtimes(action.Path, mt, mt)
						}
						fs.indexMu.Lock()
						fs.index.Sequence++
						fs.index.setEntry(action.Path, FileEntry{
							Size:     action.RemoteSize,
							MtimeNS:  action.RemoteMtime,
							SHA256:   action.RemoteHash,
							Sequence: fs.index.Sequence,
							Mode:     action.RemoteMode,
							// C6: identical content — adopt the peer's
							// clock so the path converges.
							Version: action.RemoteVersion.clone(),
						})
						fs.indexMu.Unlock()
						slog.Info("conflict auto-resolved: identical content",
							"folder", folderID, "path", action.Path, "peer", peerAddr)
						perfEmit(map[string]any{
							"event": "conflict_resolved", "folder": folderID,
							"path": action.Path, "peer": peerAddr, "hash": localHash[:16],
						})
						return
					}
				}

				remoteDeviceID := remoteIdx.GetDeviceId()
				winner, conflictRelPath := resolveConflict(fs.root, action.Path, lmtime, action.RemoteMtime, remoteDeviceID)
				slog.Debug("conflict resolved",
					"folder", folderID, "path", action.Path, "winner", winner,
					"local_mtime", lmtime, "remote_mtime", action.RemoteMtime,
					"remote_device", remoteDeviceID)
				if winner == "remote" {
					// B13: download remote to verified temp FIRST — local file
					// stays intact until the download succeeds. If the download
					// fails, the local file is untouched.
					tmpRelPath, err := downloadToVerifiedTemp(ctx, n.clientForPeer(peerAddr), peerAddr, folderID, action.Path, action.RemoteHash, fs.root, n.rateLimiter)
					if err != nil {
						slog.Warn("conflict download failed", "folder", folderID, "path", action.Path, "error", err)
						fs.indexMu.Lock()
						fs.retries.record(action.Path, peerAddr, action.RemoteHash)
						fs.indexMu.Unlock()
						failMu.Lock()
						setFailReason("conflict download: " + err.Error())
						failedSeqs = append(failedSeqs, action.RemoteSequence)
						failMu.Unlock()
						fs.metrics.SyncErrors.Add(1)
						return
					}

					// Download verified — now safe to rename local to conflict.
					if dir := filepath.Dir(conflictRelPath); dir != "." {
						if err := fs.root.MkdirAll(dir, 0750); err != nil {
							slog.Warn("create conflict dir failed", "folder", folderID, "path", action.Path, "error", err)
							_ = fs.root.Remove(tmpRelPath)
							fs.indexMu.Lock()
							fs.retries.record(action.Path, peerAddr, action.RemoteHash)
							fs.indexMu.Unlock()
							failMu.Lock()
							setFailReason("conflict mkdir: " + err.Error())
							failedSeqs = append(failedSeqs, action.RemoteSequence)
							failMu.Unlock()
							fs.metrics.SyncErrors.Add(1)
							return
						}
					}
					if err := fs.root.Rename(action.Path, conflictRelPath); err != nil {
						slog.Warn("rename local to conflict failed", "folder", folderID, "path", action.Path, "error", err)
						_ = fs.root.Remove(tmpRelPath)
						fs.indexMu.Lock()
						fs.retries.record(action.Path, peerAddr, action.RemoteHash)
						fs.indexMu.Unlock()
						failMu.Lock()
						setFailReason("conflict rename: " + err.Error())
						failedSeqs = append(failedSeqs, action.RemoteSequence)
						failMu.Unlock()
						fs.metrics.SyncErrors.Add(1)
						return
					}

					// Move verified temp to final destination.
					if err := renameReplaceRoot(fs.root, tmpRelPath, action.Path); err != nil {
						slog.Warn("rename temp to dest failed", "folder", folderID, "path", action.Path, "error", err)
						// H5: restore local file from conflict name. If restore
						// also fails, keep tmpRelPath so the downloaded data is
						// not lost — the user can recover manually.
						if restoreErr := fs.root.Rename(conflictRelPath, action.Path); restoreErr != nil {
							slog.Error("conflict recovery failed: local file is at conflict path, remote at temp path — manual recovery needed",
								"folder", folderID, "path", action.Path,
								"conflict_at", conflictRelPath, "temp_at", tmpRelPath,
								"error", restoreErr)
						} else {
							_ = fs.root.Remove(tmpRelPath)
						}
						fs.indexMu.Lock()
						fs.retries.record(action.Path, peerAddr, action.RemoteHash)
						fs.indexMu.Unlock()
						failMu.Lock()
						setFailReason("conflict finalize: " + err.Error())
						failedSeqs = append(failedSeqs, action.RemoteSequence)
						failMu.Unlock()
						fs.metrics.SyncErrors.Add(1)
						return
					}

					// C2: post-write verification on network filesystems.
					if fs.isNetworkFS {
						if err := verifyPostWrite(fs.root, action.Path, action.RemoteHash, folderID, peerAddr, &fs.retries, &fs.indexMu); err != nil {
							// Conflict rename already succeeded — clean up the
							// displaced local to avoid orphaned conflict files.
							_ = fs.root.Remove(conflictRelPath)
							failMu.Lock()
							setFailReason("conflict verify: " + err.Error())
							failedSeqs = append(failedSeqs, action.RemoteSequence)
							failMu.Unlock()
							fs.metrics.SyncErrors.Add(1)
							return
						}
					}

					// G1: preserve remote mtime so the next scan's fast-path skip works.
					if action.RemoteMtime > 0 {
						mt := time.Unix(0, action.RemoteMtime)
						_ = fs.root.Chtimes(action.Path, mt, mt)
					}

					// L1: apply file permissions from remote.
					cfMode := os.FileMode(action.RemoteMode)
					if cfMode == 0 {
						cfMode = 0644
					}
					_ = fs.root.Chmod(action.Path, cfMode)

					fs.indexMu.Lock()
					fs.index.Sequence++
					fs.index.setEntry(action.Path, FileEntry{
						Size:     action.RemoteSize,
						MtimeNS:  action.RemoteMtime,
						SHA256:   action.RemoteHash,
						Sequence: fs.index.Sequence,
						Mode:     action.RemoteMode,
						// C6: remote wins — adopt their clock.
						Version: action.RemoteVersion.clone(),
					})
					fs.retries.clearAll(action.Path)
					fs.indexMu.Unlock()
					fs.metrics.FilesConflicted.Add(1)
					fs.metrics.BytesDownloaded.Add(action.RemoteSize)

					// G4: prune old conflict files to prevent unbounded accumulation.
					pruneConflicts(fs.root, conflictRelPath)
				} else {
					// H7: local wins — let lastSeenSeq advance past this
					// entry. If the remote later modifies the file (new
					// sequence), it will naturally trigger re-evaluation.
					// NOT added to failedSeqs: doing so would hold
					// lastSeenSeq below RemoteSequence, re-triggering
					// the same conflict every cycle indefinitely.
				}
				slog.Info("resolved conflict", "folder", folderID, "path", action.Path, "winner", winner)
			}()

		case ActionDelete:
			// F10: run deletes through the semaphore like downloads so
			// high-latency filesystems (NFS, FUSE) don't block dispatch.
			wg.Add(1)
			action := action
			go func() {
				defer wg.Done()
				defer fs.releasePath(action.Path)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				if err := deleteFile(fs.root, action.Path); err != nil {
					slog.Warn("delete failed", "folder", folderID, "path", action.Path, "error", err)
					fs.indexMu.Lock()
					fs.retries.record(action.Path, peerAddr, action.RemoteHash)
					fs.indexMu.Unlock()
					failMu.Lock()
					setFailReason("delete: " + err.Error())
					failedSeqs = append(failedSeqs, action.RemoteSequence)
					failMu.Unlock()
					fs.metrics.SyncErrors.Add(1)
					return
				}
				fs.indexMu.Lock()
				// N3: always write a tombstone, even if the file wasn't in the
				// local index. Without a tombstone, the file can resurrect from
				// a third peer that still has it in their index.
				entry := fs.index.Files[action.Path] // zero value if absent
				if entry.Deleted {
					// N12: already a tombstone — skip sequence bump, mtime reset, and metric.
					fs.indexMu.Unlock()
				} else {
					fs.index.Sequence++
					entry.Deleted = true
					entry.MtimeNS = time.Now().UnixNano()
					entry.Sequence = fs.index.Sequence
					// C6: adopt the peer's tombstone clock when present
					// (tombstone write came from them). Fall back to a
					// self-bump when the peer has no clock so the local
					// entry still carries a non-empty vector.
					if len(action.RemoteVersion) > 0 {
						entry.Version = action.RemoteVersion.clone()
					} else if fs.index.selfID != "" {
						entry.Version = entry.Version.bump(fs.index.selfID)
					}
					fs.index.setEntry(action.Path, entry)
					fs.indexMu.Unlock()
					fs.metrics.FilesDeleted.Add(1)
				}
				slog.Info("deleted file", "folder", folderID, "path", action.Path, "peer", peerAddr)
			}()
		}
	}
	wg.Wait()

	if skippedQuarantine > 0 {
		slog.Warn("skipped quarantined files",
			"folder", folderID, "peer", peerAddr, "count", skippedQuarantine)
	}

	// Record sync activity.
	direction := "download"
	if fs.cfg.Direction == "send-only" {
		direction = "upload"
	}
	var errMsg string
	if len(failedSeqs) > 0 {
		errMsg = fmt.Sprintf("%d files failed", len(failedSeqs))
	}
	n.recordActivity(SyncActivity{
		Time:      time.Now(),
		Folder:    folderID,
		Peer:      peerAddr,
		Direction: direction,
		Files:     downloads + conflicts + deletes,
		Bytes:     totalBytes,
		Error:     errMsg,
	})

	// B9: compute safe LastSeenSequence — do not advance past entries
	// that failed to process, so they are re-evaluated next round.
	// N6: skip fseq=0 — subtracting 1 would produce -1, causing a full
	// re-diff every cycle.
	safeSeq := remoteIdx.GetSequence()
	for _, fseq := range failedSeqs {
		if fseq > 0 && fseq-1 < safeSeq {
			safeSeq = fseq - 1
		}
	}

	// Update peer state.
	fs.indexMu.Lock()
	// C2: recompute ancestor hashes from the post-sync agreement between
	// our index and the remote index we just exchanged. Successful
	// downloads and conflict auto-resolves have already written remote
	// hashes into fs.index, so paths now in agreement are picked up
	// here. Failed downloads leave the hashes diverged and get no
	// ancestor update — they are retried next cycle.
	prior := fs.peers[peerAddr].BaseHashes
	fs.peers[peerAddr] = PeerState{
		LastSeenSequence: safeSeq,
		LastSentSequence: ourCurrentSeq,
		LastSync:         time.Now(),
		LastEpoch:        resolvedEpoch,
		BaseHashes:       updateBaseHashes(prior, fs.index, remoteFileIndex),
	}
	fs.peersDirty = true // P17a: peer state always changes (LastSync)
	// P17a: mark index dirty only when file actions actually modified it.
	// Successful downloads/conflicts/deletes each wrote fs.index.Files entries
	// inside their goroutines. The expensive index serialize is skipped when
	// only peer bookkeeping (sequence numbers, timestamps) changed.
	appliedActions := (downloads + conflicts + deletes) - len(failedSeqs)
	if appliedActions > 0 {
		fs.indexDirty = true
	}
	fs.indexMu.Unlock()

	// Persist index after sync.
	n.persistFolder(folderID, false)

	fs.indexMu.RLock()
	count, totalSize := fs.index.activeCountAndSize()
	fs.indexMu.RUnlock()
	state.Global.Update("filesync-folder", folderID, state.Connected, "idle")
	state.Global.UpdateFileCount("filesync-folder", folderID, count, totalSize)
	state.Global.UpdateLastSync("filesync-folder", folderID, time.Now())

	syncDuration := time.Since(syncStart)
	fs.metrics.PeerSyncs.Add(1)
	fs.metrics.PeerSyncNS.Store(syncDuration.Nanoseconds())
	slog.Debug("sync cycle complete", "folder", folderID, "peer", peerAddr,
		"duration", syncDuration, "downloads", downloads, "conflicts", conflicts,
		"deletes", deletes, "errors", len(failedSeqs), "bytes", totalBytes)
	perfSync(folderID, peerAddr, remoteEntries, downloads, conflicts, deletes, len(failedSeqs), ms(syncDuration), firstFailReason)
}

// buildIndexExchange creates a protobuf IndexExchange from the local index.
// If sinceSequence > 0, only entries with Sequence > sinceSequence are included (delta mode).
func (n *Node) buildIndexExchange(folderID string, sinceSequence int64) *pb.IndexExchange {
	fs, ok := n.folders[folderID]
	if !ok {
		return &pb.IndexExchange{ProtocolVersion: protocolVersion}
	}

	fs.indexMu.RLock()
	defer fs.indexMu.RUnlock()

	// PG: for delta exchanges, use the secondary sequence index to avoid
	// iterating the full Files map. Binary search to find the start position,
	// then iterate only the tail. Stale entries (path updated with newer seq)
	// are filtered by checking against the Files map.
	if sinceSequence > 0 && len(fs.index.seqIndex) > 0 {
		start := sort.Search(len(fs.index.seqIndex), func(i int) bool {
			return fs.index.seqIndex[i].seq > sinceSequence
		})
		tail := fs.index.seqIndex[start:]
		files := make([]*pb.FileInfo, 0, len(tail))
		for _, se := range tail {
			entry, ok := fs.index.Files[se.path]
			if !ok || entry.Sequence != se.seq {
				continue // stale secondary index entry
			}
			files = append(files, &pb.FileInfo{
				Path:     se.path,
				Size:     entry.Size,
				MtimeNs:  entry.MtimeNS,
				Sha256:   entry.SHA256[:],
				Deleted:  entry.Deleted,
				Sequence: entry.Sequence,
				Mode:     entry.Mode,
				PrevPath: entry.PrevPath,
				Version:  entry.Version.toProto(),
			})
		}
		return &pb.IndexExchange{
			DeviceId:        n.deviceID,
			FolderId:        folderID,
			Sequence:        fs.index.Sequence,
			Epoch:           fs.index.Epoch,
			Files:           files,
			ProtocolVersion: protocolVersion,
		}
	}

	// Full exchange (sinceSequence == 0 or empty seqIndex): iterate all entries.
	files := make([]*pb.FileInfo, 0, len(fs.index.Files))
	for path, entry := range fs.index.Files {
		if sinceSequence > 0 && entry.Sequence <= sinceSequence {
			continue
		}
		files = append(files, &pb.FileInfo{
			Path:     path,
			Size:     entry.Size,
			MtimeNs:  entry.MtimeNS,
			Sha256:   entry.SHA256[:],
			Deleted:  entry.Deleted,
			Sequence: entry.Sequence,
			Mode:     entry.Mode,
			PrevPath: entry.PrevPath,
			Version:  entry.Version.toProto(),
		})
	}

	return &pb.IndexExchange{
		DeviceId:        n.deviceID,
		FolderId:        folderID,
		Sequence:        fs.index.Sequence,
		Epoch:           fs.index.Epoch,
		Files:           files,
		ProtocolVersion: protocolVersion,
	}
}

// findFolder returns the folder state for the given ID, or nil.
// n.folders is immutable after Start() — no lock needed.
func (n *Node) findFolder(folderID string) *folderState {
	return n.folders[folderID]
}

// isPeerConfigured checks if the given IP is a configured peer for any folder.
// AllowedPeerHosts is resolved at config load time so hostnames have already
// been expanded via DNS; the request IP is compared against each entry via
// peerMatchesAddr which handles IPv6 canonicalization and loopback variants.
func (n *Node) isPeerConfigured(requestIP string) bool {
	for _, fs := range n.folders {
		for _, host := range fs.cfg.AllowedPeerHosts {
			if peerMatchesAddr(host, requestIP) {
				return true
			}
		}
	}
	return false
}

// persistAll saves all folder indices and peer states to disk (shutdown path).
func (n *Node) persistAll() {
	for id := range n.folders {
		n.persistFolder(id, true)
	}
}

// persistFolder saves a single folder's index and peer state.
// N10: serialized via persistMu to prevent concurrent syncFolder
// goroutines from racing on the same .tmp file.
// P17a: skips unchanged components. The index file (~30 MB for large folders)
// is only written when indexDirty is set. Peer state (~1 KB) is always cheap
// to write but still gated by peersDirty. force=true bypasses both checks
// (used at shutdown).
func (n *Node) persistFolder(folderID string, force bool) {
	fs, ok := n.folders[folderID]
	if !ok {
		return
	}

	fs.persistMu.Lock()
	defer fs.persistMu.Unlock()

	fs.indexMu.Lock()
	saveIndex := force || fs.indexDirty
	savePeers := force || fs.peersDirty
	if saveIndex {
		fs.indexDirty = false
	}
	if savePeers {
		fs.peersDirty = false
	}
	fs.indexMu.Unlock()

	if !saveIndex && !savePeers {
		perfPersist(folderID, 0, 0, 0, true)
		return
	}

	// F1: snapshot under RLock, then release and serialize outside the
	// lock. This avoids holding indexMu.RLock across disk I/O (which
	// blocks scan swap and download index updates for the full fsync
	// duration) and eliminates the fragile RUnlock→Lock→Unlock→RLock
	// dance on save failure.
	var idxSnapshot *FileIndex
	var peersSnapshot map[string]PeerState
	fs.indexMu.RLock()
	if saveIndex {
		idxSnapshot = fs.index.clone()
	}
	if savePeers {
		peersSnapshot = make(map[string]PeerState, len(fs.peers))
		for k, v := range fs.peers {
			peersSnapshot[k] = v
		}
	}
	fs.indexMu.RUnlock()

	var indexBytes int
	var indexMs float64
	if saveIndex {
		idxStart := time.Now()
		idxPath := filepath.Join(n.dataDir, folderID, "index.yaml")
		if err := idxSnapshot.save(idxPath); err != nil {
			slog.Warn("failed to save index", "folder", folderID, "error", err)
			fs.indexMu.Lock()
			fs.indexDirty = true // retry next time
			fs.indexMu.Unlock()
		}
		indexMs = ms(time.Since(idxStart))
		indexBytes = len(idxSnapshot.Files)
	}

	var peersMs float64
	if savePeers {
		peersStart := time.Now()
		peersPath := filepath.Join(n.dataDir, folderID, "peers.yaml")
		if err := savePeerStates(peersPath, peersSnapshot); err != nil {
			slog.Warn("failed to save peer states", "folder", folderID, "error", err)
		}
		peersMs = ms(time.Since(peersStart))
	}
	perfPersist(folderID, indexBytes, indexMs, peersMs, !saveIndex)
}

// protoToFileIndex converts a protobuf IndexExchange to our internal FileIndex.
func protoToFileIndex(idx *pb.IndexExchange) *FileIndex {
	fi := &FileIndex{
		Sequence: idx.GetSequence(),
		Epoch:    idx.GetEpoch(),
		Files:    make(map[string]FileEntry, len(idx.GetFiles())),
	}
	for _, f := range idx.GetFiles() {
		// B17: normalize remote paths to NFC for cross-platform consistency.
		path := norm.NFC.String(f.GetPath())
		fi.Files[path] = FileEntry{
			Size:     f.GetSize(),
			MtimeNS:  f.GetMtimeNs(),
			SHA256:   hash256FromBytes(f.GetSha256()),
			Deleted:  f.GetDeleted(),
			Sequence: f.GetSequence(),
			Mode:     f.GetMode(),
			PrevPath: norm.NFC.String(f.GetPrevPath()),
			Version:  vectorClockFromProto(f.GetVersion()),
		}
	}
	fi.recomputeCache() // P18b
	return fi
}

// localMtime returns the local mtime for a file, or 0 if not found.
func localMtime(fs *folderState, relPath string) int64 {
	fs.indexMu.RLock()
	defer fs.indexMu.RUnlock()
	if entry, ok := fs.index.Files[relPath]; ok {
		return entry.MtimeNS
	}
	return 0
}
