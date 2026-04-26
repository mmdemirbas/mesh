package filesync

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	// backupCadence is the interval between automatic backup
	// runs. Audit §6 commit 9 / decision §5 #11: 24 hours is the
	// natural anchor for a daily-tier retention policy. Each tick
	// the scheduler walks every active folder, calls writeBackup,
	// and runs gfsPrune. The first backup fires `backupCadence`
	// after Run starts (no startup-time backup); operators who
	// want an immediate snapshot can run a manual VACUUM INTO via
	// the SQLite shell, but the daily cadence is the
	// audit-prescribed contract.
	backupCadence = 24 * time.Hour

	// shutdownTxTimeout bounds how long the shutdown-time persist
	// pass waits on a single writer transaction. Audit §6 commit 8
	// / Gap 6: without a deadline, a stuck COMMIT (filesystem
	// stall on the WAL fsync, NFS server unreachable, disk
	// pressure stalling the kernel) would hang the process
	// indefinitely. 10s is comfortably longer than a healthy
	// fsync (sub-ms) and short enough that a hung shutdown
	// surfaces as an operator-visible delay rather than "the
	// process never exits." The deadline propagates via
	// db.BeginTx(ctx, ...) so the SQLite driver observes
	// ctx.Done and rolls back the partial tx cleanly.
	shutdownTxTimeout = 10 * time.Second

	// tombstoneGCEvery is the scan cadence for the tombstone GC
	// pass. Audit §6 commit 7 phase D / P6 / decision §5 #15: every
	// 10th scan, purgeTombstones runs as part of the scan-tail
	// logic; the other 9 scans skip the O(N) tombstone-walk and
	// the per-row Range traversal it costs. The cadence is a perf
	// optimization — correctness is preserved by purgeTombstones'
	// B14/M3 invariants (only purge when ALL peers including
	// removed ones have acknowledged the tombstone).
	tombstoneGCEvery = 10
	// C3: stale download temp files older than this are removed on
	// startup. Anything younger is kept so downloadWithBlockVerify can
	// resume from the last verified block boundary across a process
	// restart. Seven days is longer than any plausible retry window
	// and far shorter than a sync graveyard.
	tempFileMaxAge = 7 * 24 * time.Hour
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

	// PERSISTENCE-AUDIT.md §2.2 R8 + iter-4 O11 / O8: when the folder
	// has transitioned to FolderDisabled, Status="disabled", Reason
	// names the closed-enum value, Action carries the operator's
	// next sentence (matches OPERATOR-RUNBOOK.md §4 verbatim).
	// ErrorText / StackTrace / RecentLog populate only for
	// reason=unknown so the operator can triage inline. TxRolledBack
	// records iter-4 Z6: any in-flight writer tx canceled by the
	// disable() call.
	Status       string         `json:"status,omitempty"` // "disabled" when disabled, empty otherwise
	Reason       DisabledReason `json:"reason,omitempty"`
	Action       string         `json:"action,omitempty"`
	ErrorText    string         `json:"error_text,omitempty"`
	StackTrace   string         `json:"stack_trace,omitempty"`
	RecentLog    []string       `json:"recent_log,omitempty"`
	TxRolledBack bool           `json:"tx_in_flight_rolled_back,omitempty"`
	DisabledAtMs int64          `json:"disabled_at_ms,omitempty"` // Unix milliseconds; 0 when not disabled
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

			// PERSISTENCE-AUDIT.md §6 commit 3: a disabled folder may
			// have nil fs.index / fs.peers if it failed before the
			// open path completed. Surface the disabled-state JSON
			// without dereferencing the in-memory index.
			if ds := fs.disabled.Load(); ds != nil {
				fst := FolderStatus{
					ID:           id,
					Path:         fs.cfg.Path,
					Direction:    fs.cfg.Direction,
					Status:       "disabled",
					Reason:       ds.Reason,
					Action:       ds.Action,
					ErrorText:    ds.ErrorText,
					StackTrace:   ds.StackTrace,
					RecentLog:    ds.RecentLog,
					TxRolledBack: ds.TxRolledBack,
					DisabledAtMs: ds.DisabledAt.UnixMilli(),
				}
				result = append(result, fst)
				continue
			}

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

// folderCacheDirFor returns the folder's SQLite cache directory
// (where the index.sqlite, the F7 sweep, and the backups subtree
// all live). Used by the admin endpoints that operate on a
// folder's cache state without needing the full folderState.
func folderCacheDirFor(folderID string) (string, bool) {
	var dir string
	var found bool
	activeNodes.ForEach(func(n *Node) {
		if found {
			return
		}
		if _, ok := n.folders[folderID]; ok {
			dir = filepath.Join(n.dataDir, folderID)
			found = true
		}
	})
	return dir, found
}

// ListFolderBackups returns the persisted backup files for a
// folder, sorted by sequence descending. Audit §6 commit 9a /
// iter-4 O9. Backed by listBackups against the folder's cache
// directory; the admin endpoint at GET
// /api/filesync/folders/<id>/backups consumes this.
//
// Returns (nil, false) when the folder is unknown to any active
// node — the admin handler turns this into HTTP 404.
func ListFolderBackups(folderID string) ([]BackupInfo, bool) {
	dir, ok := folderCacheDirFor(folderID)
	if !ok {
		return nil, false
	}
	backups, err := listBackups(dir)
	if err != nil {
		slog.Warn("ListFolderBackups: listBackups failed",
			"folder", folderID, "error", err)
		return []BackupInfo{}, true
	}
	if backups == nil {
		return []BackupInfo{}, true
	}
	return backups, true
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

	// Disabled is 1 when the folder is in the FolderDisabled state, 0
	// otherwise. Reason carries the closed-enum reason when disabled
	// (empty string when enabled). cmd/mesh/admin.go emits these as
	// the mesh_filesync_folder_disabled{folder=..., reason=...} gauge
	// (decision §5 #18 + iter-4 O11 / Z10).
	Disabled int
	Reason   DisabledReason
}

// peerSessionDropped is the process-level counter behind the
// mesh_filesync_peer_session_dropped{reason=...} Prometheus gauge.
// Audit §6 commit 7 phase A / iter-4 Z10: every reason for which a
// handleIndex request is rejected at the wire layer (today:
// "filesync_index_model_mismatch") increments the corresponding
// reason bucket so the dashboard surfaces a warning row when a
// rolling deploy lands a drifted const. Map keyed by reason string;
// guarded by peerSessionDroppedMu so concurrent goroutines (each
// peer is its own goroutine) cannot race the map write.
var (
	peerSessionDroppedMu sync.Mutex
	peerSessionDropped   = make(map[string]int64)
)

func incPeerSessionDropped(reason string) {
	peerSessionDroppedMu.Lock()
	peerSessionDropped[reason]++
	peerSessionDroppedMu.Unlock()
}

// SnapshotPeerSessionDropped returns a copy of the per-reason
// counter map so the metrics handler can serialize it without
// holding the lock across the response write. Stable iteration
// order is the caller's responsibility (admin.go sorts by reason).
func SnapshotPeerSessionDropped() map[string]int64 {
	peerSessionDroppedMu.Lock()
	defer peerSessionDroppedMu.Unlock()
	out := make(map[string]int64, len(peerSessionDropped))
	for k, v := range peerSessionDropped {
		out[k] = v
	}
	return out
}

// GetFolderMetrics returns a snapshot of sync metrics for all active folders.
func GetFolderMetrics() []FolderMetricsSnapshot {
	var result []FolderMetricsSnapshot
	activeNodes.ForEach(func(n *Node) {
		for id, fs := range n.folders {
			snap := FolderMetricsSnapshot{
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
			}
			if ds := fs.disabled.Load(); ds != nil {
				snap.Disabled = 1
				snap.Reason = ds.Reason
			}
			result = append(result, snap)
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
	// F7 / commit 6.2 phase E: backup-protected installs.
	BakRestoredOnCommitFail atomic.Int64 // .bak rolled back to original after SQLite commit failed
	BakRestoreFailed        atomic.Int64 // .bak restore itself failed; manual recovery required
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
	inFlightMu sync.Mutex
	inFlight   map[string]bool
	persistMu  sync.Mutex // N10: serializes persistFolder calls
	indexDirty bool       // P17a: true when file index changed since last persist
	peersDirty bool       // P17a: true when peer state changed since last persist
	// db is the per-folder SQLite writer handle, opened by Node.Run
	// via openFolderDB (MaxOpenConns=1, _txlock=immediate). All write
	// transactions go through this handle; SQLite serializes them
	// internally. PERSISTENCE-AUDIT.md §6 commit 2: this is the only
	// on-disk store for the folder index and peer state. Closed at
	// shutdown after wg.Wait so no goroutine still holds rows. Nil
	// when the folder is in the FolderDisabled state because openFolderDB
	// failed at startup; sync/scan loops check IsDisabled() and skip.
	db *sql.DB

	// dbReader is the dedicated read-only handle for peer-facing
	// queries (audit §6 commit 4 + decision §5 #10). Opened with
	// query_only=true and mode=ro; MaxOpenConns sized for the peer
	// count so concurrent buildIndexExchange / bundle / blocksigs
	// calls do not serialize behind the writer's MaxOpenConns=1.
	// Reads use WAL snapshot isolation — they never block the writer
	// and never see a torn intermediate state. Nil if the writer
	// open failed.
	dbReader *sql.DB

	// folderDisabledFields carries the FolderDisabled-state machinery
	// (decision §5 #25, iter-4 Z6 + Z10 + Z12). See disabled.go for
	// the contract.
	folderDisabledFields

	isNetworkFS bool              // C2: true when folder root is on a network filesystem
	metrics     FolderSyncMetrics // lock-free counters for Prometheus
	// scansSinceTombstoneGC counts completed scans since the last
	// tombstone-GC pass. The scan-tail logic runs purgeTombstones
	// every tombstoneGCEvery scans (audit §6 commit 7 phase D / P6
	// / decision §5 #15). Scans that hit the cap (errIndexCapExceeded)
	// or that errored out do not count — only successful completions
	// advance the counter. Read/written under indexMu.
	scansSinceTombstoneGC int
	// P18c: recycled scan-clone backing map. Stashed after swap, reused on
	// the next runScan to avoid a ~30 MB/scan allocation on large folders.
	// Accessed only under indexMu.
	reusableFiles map[string]FileEntry
}

// openFolderInit runs the per-folder open path: opens os.Root,
// creates folderState, registers in n.folders, opens SQLite
// writer + reader, runs every open-time check (legacy sidecar
// refusal, device-id, quick_check, F7 sweep, .tmp cleanup, load
// index + peer states, network-FS detect, scanning state).
// Audit §6 commit 9.2: extracted from the original Run loop body
// so the /reopen and /restore admin endpoints can re-run the
// same sequence without the closure capture.
//
// Returns a non-nil *integrityCheckTarget when the open succeeded
// and the deferred PRAGMA integrity_check goroutine should fire.
// Returns nil when the folder failed to open (in which case it
// transitioned to FolderDisabled with the appropriate reason and
// is registered in n.folders for the dashboard).
//
// Concurrency: caller must serialize concurrent invocations
// against the same folderID — the Run init loop runs
// sequentially; the admin endpoints (commit 9.2) take a
// per-folder lock before calling.
func (n *Node) openFolderInit(ctx context.Context, fcfg config.FolderCfg) *integrityCheckTarget {
	// L5: open an os.Root handle for TOCTOU-safe file operations.
	// A missing path (e.g. host-specific mount point not present on this
	// machine) must not abort the whole node — record the folder as
	// failed and return without an integrity check.
	folderRoot, rootErr := os.OpenRoot(fcfg.Path)
	if rootErr != nil {
		slog.Warn("filesync folder path missing, skipping", "folder", fcfg.ID, "path", fcfg.Path, "error", rootErr)
		state.Global.Update("filesync-folder", fcfg.ID, state.Failed, "path missing: "+rootErr.Error())
		for _, peer := range fcfg.Peers {
			state.Global.Update("filesync-peer", fcfg.ID+"|"+peer, state.Failed, "folder path missing")
		}
		return nil
	}

	// C3: remove abandoned download temp files from previous crashed
	// runs. Recent temps (within tempFileMaxAge) are preserved so
	// downloadWithBlockVerify can resume across a restart; older ones
	// are stale and would otherwise accumulate forever.
	cleanTempFiles(fcfg.Path, tempFileMaxAge)

	// PERSISTENCE-AUDIT.md §6 commit 3: open path is wrapped in
	// the FolderDisabled state machinery. Folder is registered in
	// n.folders BEFORE openFolderDB so any failure routes through
	// fs.disable() and surfaces on the dashboard with the audit's
	// closed-enum reason + action string.
	folderCacheDir := filepath.Join(n.dataDir, fcfg.ID)
	// Audit §6 commit 10 / iter-4 Z8: detect un-checkpointed WAL
	// BEFORE openFolderDB writes anything. A non-zero -wal at
	// this point means the previous run did not shut down
	// cleanly; we run a synchronous integrity_check below
	// (after quick_check) instead of the async path.
	sigKillLeftover := detectSIGKILLLeftover(folderCacheDir)
	if sigKillLeftover {
		state.Global.Update("filesync-folder", fcfg.ID, state.Recovering,
			"un-checkpointed WAL detected; running integrity_check synchronously")
		slog.Info("Z8: SIGKILL recovery detected; integrity_check will run synchronously",
			"folder", fcfg.ID, "cache_dir", folderCacheDir)
	}
	writerCtx, writerCancel := context.WithCancel(ctx)
	fs := &folderState{
		cfg:           fcfg,
		root:          folderRoot,
		pending:       make(map[string]PendingSummary),
		peerLastError: make(map[string]string),
		inFlight:      make(map[string]bool),
	}
	fs.writerCtx = writerCtx
	fs.writerCancel = writerCancel
	n.folders[fcfg.ID] = fs

	if err := refuseLegacyIndex(folderCacheDir); err != nil {
		fs.disable(DisabledLegacyIndex, err.Error(), "")
		return nil
	}

	db, err := openFolderDB(folderCacheDir, n.deviceID)
	if err != nil {
		fs.disable(classifyOpenError(err), err.Error(), "")
		return nil
	}
	fs.db = db

	// Audit §6 commit 4 / decision §5 #10: dedicated read-only
	// handle for peer-facing reads. Sized for n_peers + 3 so
	// concurrent index exchanges across all peers do not
	// serialize behind the writer's MaxOpenConns=1.
	dbReader, err := openFolderDBReader(folderCacheDir, len(fcfg.Peers)+3)
	if err != nil {
		_ = db.Close()
		fs.db = nil
		fs.disable(classifyOpenError(err), err.Error(), "")
		return nil
	}
	fs.dbReader = dbReader

	// I7 (audit decision §5 #20): compare folder_meta.device_id
	// against the node-level identity.
	if err := checkDeviceID(db, n.deviceID); err != nil {
		fs.disable(DisabledDeviceIDMismatch, err.Error(), "")
		return nil
	}

	// R2 quick_check: synchronous at folder open.
	if err := runQuickCheck(db); err != nil {
		fs.disable(DisabledQuickCheck, err.Error(), "")
		return nil
	}

	// Audit §6 commit 10 / iter-4 Z8: when SIGKILL recovery is
	// in flight, run integrity_check SYNCHRONOUSLY before going
	// live. The ~10 MB/s scan takes tens of seconds on a large
	// DB but closes the silent live-but-corrupt window between
	// quick_check and the async integrity_check that would
	// otherwise let peers read pre-corruption rows during the
	// recovery window. The state-Global Recovering pin set
	// above remains until the check completes.
	if sigKillLeftover {
		if err := runIntegrityCheck(ctx, db); err != nil {
			fs.disable(DisabledIntegrityCheck, err.Error(), "")
			return nil
		}
	}

	// Audit §6 commit 6 phase I + J: F7 .bak sweep.
	sweepRes, sweepErr := runStartupBakSweep(ctx, db, folderRoot, fcfg.ID)
	switch {
	case sweepErr == nil:
		if sweepRes.Scanned > 0 {
			slog.Info("F7 sweep complete",
				"folder", fcfg.ID,
				"scanned", sweepRes.Scanned,
				"unlinked", sweepRes.Unlinked,
				"restored", sweepRes.Restored,
				"orphans", len(sweepRes.Orphans))
		}
		// Audit §6 commit 9a: clean any leftover *.sqlite.tmp
		// strays under <folderCacheDir>/backups/.
		cleanBackupTmp(folderCacheDir, fcfg.ID)
	case errors.Is(sweepErr, errSweepDBUnreadable):
		fs.disable(DisabledIntegrityCheck, sweepErr.Error(), "")
		return nil
	case errors.Is(sweepErr, errSweepNeitherMatches):
		diag := fmt.Sprintf(
			"sweep: neither disk file matches SQLite for path(s) %v",
			sweepRes.NeitherMatches)
		fs.disable(DisabledUnknown, diag, "")
		return nil
	default:
		fs.disable(DisabledUnknown, "sweep: "+sweepErr.Error(), "")
		return nil
	}

	idx, err := loadIndexDB(db, fcfg.ID)
	if err != nil {
		fs.disable(classifyMetaError(err), err.Error(), "")
		return nil
	}

	peers, err := loadPeerStatesDB(db, fcfg.ID)
	if err != nil {
		fs.disable(classifyMetaError(err), err.Error(), "")
		return nil
	}

	// Detect path change and warn.
	if idx.Path != "" && idx.Path != fcfg.Path {
		slog.Warn("folder path changed, next scan will reconcile",
			"folder", fcfg.ID, "old_path", idx.Path, "new_path", fcfg.Path)
	}
	idx.Path = fcfg.Path
	idx.selfID = n.deviceID

	ignore := newIgnoreMatcher(fcfg.IgnorePatterns)

	var initConflicts []string
	for path, entry := range idx.Range {
		if !entry.Deleted && isConflictFile(filepath.Base(path)) {
			initConflicts = append(initConflicts, path)
		}
	}
	sort.Strings(initConflicts)

	fs.index = idx
	fs.ignore = ignore
	fs.peers = peers
	fs.conflicts = initConflicts

	// C2: detect network filesystem at startup.
	if fsType, isNet := detectNetworkFS(fcfg.Path); isNet {
		fs.isNetworkFS = true
		slog.Warn("folder root is on a network filesystem — sync durability depends on mount options",
			"folder", fcfg.ID, "path", fcfg.Path, "fstype", fsType)
	}

	if fcfg.Direction == "disabled" {
		state.Global.Update("filesync-folder", fcfg.ID, state.Connected, "disabled")
	} else {
		state.Global.Update("filesync-folder", fcfg.ID, state.Scanning, "initial scan "+fcfg.Path)
		for _, peer := range fcfg.Peers {
			state.Global.Update("filesync-peer", fcfg.ID+"|"+peer, state.Connecting, "")
		}
	}

	// Z8: when SIGKILL recovery already ran integrity_check
	// synchronously above, we do NOT need to queue the deferred
	// async run. Returning nil here means the Run loop's
	// integrityChecks slice doesn't get a target for this folder
	// (no double-check); the folder is already in Scanning
	// state ready for the initial scan.
	if sigKillLeftover {
		return nil
	}

	return &integrityCheckTarget{fs: fs, db: db}
}

// closeOneFolder shuts a folder down: cancels the writer ctx
// (rolling back any in-flight tx), closes the SQLite handles,
// closes the os.Root, removes the folder from n.folders. After
// this returns, /reopen or /restore can call openFolderInit
// against the same fcfg to bring the folder back. Audit §6
// commit 9.2.
//
// Concurrency: caller must hold a serialization point against
// other reopen / restore invocations on the same folderID.
// In-flight scan / sync goroutines on this folder's writerCtx
// observe ctx.Done from the writerCancel and exit.
func (n *Node) closeOneFolder(folderID string) {
	fs, ok := n.folders[folderID]
	if !ok {
		return
	}
	if fs.writerCancel != nil {
		fs.writerCancel()
	}
	if fs.dbReader != nil {
		_ = fs.dbReader.Close()
		fs.dbReader = nil
	}
	if fs.db != nil {
		// PRAGMA optimize on close (audit §6 commit 8 phase B —
		// also fired on full shutdown; running it on per-folder
		// reopen keeps fresh-stats parity).
		if _, err := fs.db.Exec("PRAGMA optimize"); err != nil {
			slog.Debug("PRAGMA optimize on reopen close failed (non-fatal)",
				"folder", folderID, "error", err)
		}
		_ = fs.db.Close()
		fs.db = nil
	}
	if fs.root != nil {
		_ = fs.root.Close()
		fs.root = nil
	}
	delete(n.folders, folderID)
}

// runBackupSweep walks every folder on the node, calls
// writeBackup, then gfsPrune. Audit §6 commit 9 backup scheduler
// tie-in. Skips folders that are disabled (no live writer) or
// whose db handle is nil. Errors per folder are logged at WARN
// but never abort the sweep — one folder's backup failure must
// not delay backups for others.
//
// Concurrency: holds reopenLockMu so a concurrent /reopen or
// /restore cannot race the backup against a closing handle. The
// ticker cadence (24h) makes contention rare in practice; the
// lock is a defense-in-depth.
func (n *Node) runBackupSweep(ctx context.Context) {
	reopenLockMu.Lock()
	defer reopenLockMu.Unlock()

	for id, fs := range n.folders {
		if fs.IsDisabled() || fs.db == nil {
			continue
		}
		folderCacheDir := filepath.Join(n.dataDir, id)
		info, err := writeBackup(ctx, fs.db, folderCacheDir)
		if err != nil {
			slog.Warn("backup write failed",
				"folder", id, "error", err)
			continue
		}
		slog.Info("backup written",
			"folder", id, "path", info.Path,
			"sequence", info.Sequence)
		pruned, err := gfsPrune(folderCacheDir, id, defaultGFS, time.Now)
		if err != nil {
			slog.Warn("backup prune failed",
				"folder", id, "error", err)
			continue
		}
		if pruned > 0 {
			slog.Info("backup retention pruned",
				"folder", id, "pruned", pruned)
		}
	}
}

// ReopenFolder is the package-public entry point for the
// /api/filesync/folders/<id>/reopen admin endpoint. Audit §6
// commit 9.2 / iter-4 O12. Looks up the folder on the active
// node, runs the reopen flow, returns the disabled-state error
// (or nil on success). Returns errUnknownFolder when no active
// node knows the folder.
//
// The endpoint dispatcher in cmd/mesh/admin.go uses this
// instead of directly calling Node methods to keep the public
// API surface narrow.
func ReopenFolder(ctx context.Context, folderID string) error {
	var foundNode *Node
	activeNodes.ForEach(func(n *Node) {
		if foundNode != nil {
			return
		}
		if _, ok := n.folders[folderID]; ok {
			foundNode = n
			return
		}
		// Folder may exist in cfg but not yet in n.folders if a
		// previous open failed and the disabled state was
		// cleared. Check cfg too.
		if _, ok := n.folderConfig(folderID); ok {
			foundNode = n
		}
	})
	if foundNode == nil {
		return errUnknownFolder
	}
	return foundNode.reopenFolder(ctx, folderID)
}

// RestoreFolderFromBackup is the package-public entry point for
// the /api/filesync/folders/<id>/restore admin endpoint. Audit §6
// commit 9.2 / iter-4 O10 / Z11. Validates the operator-supplied
// backup path is under the folder's backups/ directory (defense
// against path traversal in the JSON payload), then runs the
// L5 restore lifecycle.
func RestoreFolderFromBackup(ctx context.Context, folderID, backupPath string) error {
	var foundNode *Node
	activeNodes.ForEach(func(n *Node) {
		if foundNode != nil {
			return
		}
		if _, ok := n.folderConfig(folderID); ok {
			foundNode = n
		}
	})
	if foundNode == nil {
		return errUnknownFolder
	}

	// Path-traversal guard: the operator supplies an absolute
	// path via the JSON body; we require it to be inside the
	// folder's backups/ directory. Otherwise an attacker with
	// the admin endpoint reachable could swap in arbitrary file
	// content as the new index.sqlite.
	expected := backupDirFor(filepath.Join(foundNode.dataDir, folderID))
	clean := filepath.Clean(backupPath)
	if !strings.HasPrefix(clean, expected+string(filepath.Separator)) {
		return fmt.Errorf("restore: backup path %q must be under %q",
			clean, expected)
	}

	return foundNode.restoreFromBackup(ctx, folderID, clean)
}

// reopenLockMu serializes reopen / restore operations across
// all folders. Audit §6 commit 9.2: while a folder is being
// closed-and-re-opened, no other lifecycle operation against
// any folder can race. The lock is package-global because the
// admin endpoints are dispatched per-process; Node-level
// granularity would not buy enough parallelism to justify the
// extra locking surface for v1's three-folder topology.
var reopenLockMu sync.Mutex

// ErrUnknownFolder is returned when a /reopen or /restore
// request names a folder ID that is not in cfg.ResolvedFolders.
// The admin handler maps this to HTTP 404. The folder may have
// been removed from config or never registered.
var ErrUnknownFolder = errors.New("filesync: unknown folder")

// errUnknownFolder is the package-internal alias for the
// exported sentinel; the rename let the admin handler use
// errors.Is via the public API while keeping the package's own
// call sites short.
var errUnknownFolder = ErrUnknownFolder

// folderConfig returns the resolved FolderCfg for a folder ID
// from the running Node's config. Used by the lifecycle endpoints
// to fetch the same FolderCfg the init loop saw, so reopen
// preserves the original peer list, ignore patterns, etc.
func (n *Node) folderConfig(folderID string) (config.FolderCfg, bool) {
	for _, fcfg := range n.cfg.ResolvedFolders {
		if fcfg.ID == folderID {
			return fcfg, true
		}
	}
	return config.FolderCfg{}, false
}

// reopenFolder closes the named folder and re-opens it via
// openFolderInit. Audit §6 commit 9.2 / iter-4 O12. Used by:
//   - POST /api/filesync/folders/<id>/reopen (operator-driven
//     recovery from disabled state).
//   - restoreFromBackup's final step.
//
// Holds reopenLockMu for the duration so concurrent reopen /
// restore calls serialize. The integrity-check goroutine for
// the freshly-opened DB fires inline (caller's lifetime owns
// it; no shared wg). Returns the final state of the folder —
// nil when the open succeeded, errUnknownFolder when the folder
// is unknown to config, or the disable error when the open
// transitioned to FolderDisabled.
func (n *Node) reopenFolder(ctx context.Context, folderID string) error {
	reopenLockMu.Lock()
	defer reopenLockMu.Unlock()

	fcfg, ok := n.folderConfig(folderID)
	if !ok {
		return errUnknownFolder
	}
	n.closeOneFolder(folderID)

	target := n.openFolderInit(ctx, fcfg)
	if target == nil {
		// openFolderInit registered the folder in n.folders with
		// a disabled state. Surface the disabled reason as a
		// returned error so the admin endpoint can include it in
		// the response.
		if fs, ok := n.folders[folderID]; ok {
			if ds := fs.disabled.Load(); ds != nil {
				return fmt.Errorf("folder reopened in disabled state: reason=%s message=%s",
					ds.Reason, ds.ErrorText)
			}
		}
		return fmt.Errorf("folder %s reopen failed", folderID)
	}

	// Fire the deferred integrity check inline; caller's lifetime
	// is the admin endpoint's request, which already has its own
	// goroutine in net/http's handler pool.
	go func() {
		if err := runIntegrityCheck(ctx, target.db); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			target.fs.disable(DisabledIntegrityCheck, err.Error(), "")
		}
	}()
	return nil
}

// restoreFromBackup runs the L5 restore lifecycle: validate the
// chosen backup with quick_check, stop the folder, swap the
// SQLite file (and -wal / -shm sidecars), bump folder_meta.epoch
// to a fresh value, and reopen via openFolderInit. Audit §6
// commit 9.2 / iter-4 O10 / Z11.
//
// The epoch bump is the load-bearing step: peers running against
// the pre-restore epoch see a fresh re-baseline on next exchange
// (per the Phase 7B reset trigger). The pre-swap quick_check
// closes the iter-4 Z11 between-list-and-swap corruption window.
func (n *Node) restoreFromBackup(ctx context.Context, folderID, backupPath string) error {
	reopenLockMu.Lock()
	defer reopenLockMu.Unlock()

	fcfg, ok := n.folderConfig(folderID)
	if !ok {
		return errUnknownFolder
	}

	// Step 0: Z11 — quick_check the chosen backup BEFORE we touch
	// anything. If the backup is corrupt, abort with a typed
	// error and leave the folder in its current state.
	checkDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		return fmt.Errorf("restore: open backup %s: %w", backupPath, err)
	}
	checkErr := runQuickCheck(checkDB)
	_ = checkDB.Close()
	if checkErr != nil {
		return fmt.Errorf("restore: backup quick_check failed: %w", checkErr)
	}

	// Step 1: stop the folder. closeOneFolder cancels the writer
	// ctx (rolling back any in-flight tx) and closes the
	// SQLite handles. After this, no goroutine holds rows
	// against the folder's index.sqlite.
	n.closeOneFolder(folderID)

	folderCacheDir := filepath.Join(n.dataDir, fcfg.ID)
	livePath := filepath.Join(folderCacheDir, folderDBFilename)

	// Step 2: swap. Copy backup → livePath (replacing the
	// existing index.sqlite). Sidecars (-wal, -shm) are removed
	// because they belong to the OLD index and are stale against
	// the restored content.
	if err := copyFile(backupPath, livePath, 0o600); err != nil {
		return fmt.Errorf("restore: copy backup → live: %w", err)
	}
	for _, sidecar := range []string{livePath + "-wal", livePath + "-shm"} {
		_ = os.Remove(sidecar)
	}

	// Step 3: bump folder_meta.epoch to a fresh value so peers
	// see a re-baselined source on next exchange. Open the
	// restored DB just long enough to update the epoch row, then
	// close so step 4's openFolderInit gets a clean handle.
	bumpDB, err := sql.Open("sqlite", livePath)
	if err != nil {
		return fmt.Errorf("restore: reopen for epoch bump: %w", err)
	}
	newEpoch := newRandomEpoch()
	if _, err := bumpDB.ExecContext(ctx,
		`INSERT INTO folder_meta(key, value) VALUES('epoch', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		newEpoch); err != nil {
		_ = bumpDB.Close()
		return fmt.Errorf("restore: epoch bump: %w", err)
	}
	_ = bumpDB.Close()

	// Step 4: restart via reopenFolder semantics. Drop the lock
	// across the call to openFolderInit because reopenFolder also
	// takes it; we hold it for the steps above so a concurrent
	// /reopen against the same folder cannot race.
	target := n.openFolderInit(ctx, fcfg)
	if target == nil {
		if fs, ok := n.folders[folderID]; ok {
			if ds := fs.disabled.Load(); ds != nil {
				return fmt.Errorf("restore: post-restore folder is disabled: reason=%s message=%s",
					ds.Reason, ds.ErrorText)
			}
		}
		return fmt.Errorf("restore: openFolderInit failed for %s", folderID)
	}
	go func() {
		if err := runIntegrityCheck(ctx, target.db); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			target.fs.disable(DisabledIntegrityCheck, err.Error(), "")
		}
	}()
	slog.Info("restore: folder restored from backup",
		"folder", folderID, "backup", backupPath, "new_epoch", newEpoch)
	return nil
}

// copyFile copies src to dst with the given mode. Used by the
// restore path; isolated here so the unit test can call it
// without spinning up SQLite. Streams via io.Copy so the size
// is bounded by file size (no memory ceiling for huge backups).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is operator-supplied via the restore endpoint
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dst: %w", err)
	}
	return nil
}

// newRandomEpoch returns a fresh 16-character hex epoch. Audit
// decision §5 #17 / iter-4 O10: the restore lifecycle bumps the
// folder_meta.epoch so peers re-baseline rather than treat the
// rewind as silent data loss. Hex-encoded 64-bit random value;
// collision probability across the lifetime of the daemon is
// negligible.
func newRandomEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// integrityCheckTarget pairs a folderState with its writer DB
// handle so the deferred PRAGMA integrity_check goroutine can
// route a failure through fs.disable() (audit §6 commit 3 / R2 /
// Gap 5). Promoted from a Run-local type so openOneFolder can
// return it for both the initial open path and the restore /
// reopen lifecycle (audit §6 commit 9b).
type integrityCheckTarget struct {
	fs *folderState
	db *sql.DB
}

// classifyPeerResetTrigger names the reset-trigger condition that
// applies to an incoming index exchange — empty string if no reset
// is needed, otherwise one of the closed enum values used in log
// lines and (potentially) future metrics. Audit §6 commit 7 phase
// B / iter-4 Z2.
//
// Two conditions trigger a reset (drop BaseHashes, zero
// LastSeenSequence, full exchange next cycle):
//
//  1. **Sequence drop.** remoteSeq < peerLastSeq means the peer's
//     sequence counter went backward — the peer was reset (deleted
//     index, fresh install, restore from a backup whose sequence
//     was lower than what we last saw). Today's behavior; preserved.
//
//  2. **Epoch flip.** The remote presents a different epoch from
//     the one we last recorded against this peer. This closes the
//     offline-peer-during-restore gap (iter-4 Z2): when an operator
//     runs the folder restore lifecycle (commit 9), the restored DB
//     carries a fresh epoch but the SAME or HIGHER sequence number
//     than before, so the sequence-drop trigger alone would not
//     fire and peers would silently keep stale BaseHashes against
//     a divergent DB.
//
// Both conditions can fire at the same time; the trigger string
// identifies which (compound returns "sequence_drop_and_epoch_flip"
// for log readability).
//
// Edge cases:
//   - currentLastEpoch == "" means we have never recorded an epoch
//     against this peer (legitimate first sync). Skip the epoch
//     check; rely on sequence-drop alone.
//   - remoteEpoch == "" means the peer is on a pre-epoch-field
//     build (rolling-upgrade compat). Skip the epoch check.
func classifyPeerResetTrigger(remoteSeq, peerLastSeq int64, remoteEpoch, currentLastEpoch string) string {
	seqDropped := remoteSeq < peerLastSeq
	epochFlipped := currentLastEpoch != "" && remoteEpoch != "" && remoteEpoch != currentLastEpoch
	switch {
	case seqDropped && epochFlipped:
		return "sequence_drop_and_epoch_flip"
	case seqDropped:
		return "sequence_drop"
	case epochFlipped:
		return "epoch_flip"
	default:
		return ""
	}
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

// isClaimed reports whether path is currently held by an in-flight
// download. Read-only inspection of the inFlight map; the scanner
// passes this as the `claimed` callback to scanWithStats so claimed
// paths are skipped this cycle (audit §6 commit 5, closes Gap 5' /
// C6). Returns false when fs.inFlight is nil so test fixtures that
// construct folderState without inFlight do not need to initialize
// it (the production constructor at Node.Run always does).
func (fs *folderState) isClaimed(path string) bool {
	fs.inFlightMu.Lock()
	defer fs.inFlightMu.Unlock()
	if fs.inFlight == nil {
		return false
	}
	return fs.inFlight[path]
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
		oldEntry, exists := fs.index.Get(a.RemotePrevPath)
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
		oldEntry, _ := fs.index.Get(oldPath)
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
			// Hint-rename mutation: tombstone the old path with a
			// vector-clock bump. Set returns errDirtySetOverflow only
			// when the dirty set is at the 1.5M cap; bounded by
			// in-flight sync action count, never reaches the cap.
			_ = fs.index.Set(oldPath, oldEntry)
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

// errScanClaimSkipMissing is returned by preflightScanClaimSkip when
// the scan-walker invariant from audit §6 commit 5 has been reverted.
// Names the contract by content (not by commit-sequence number) so
// the message stays readable even if the audit's §6 numbering shifts
// later.
var errScanClaimSkipMissing = errors.New(
	"filesync: scan walker missing in-flight claim skip; refusing to open any folder. " +
		"scanWithStats MUST consult fs.isClaimed() per the C6 / Gap 5' invariant " +
		"(docs/filesync/PERSISTENCE-AUDIT.md §6 commit 5). Without it, a download " +
		"in flight while a scan re-hashes the same path produces scan-derived " +
		"(local-bumped) VectorClock semantics that race the download tx and " +
		"corrupt peer-visible state. Restore the claim-skip in scanWithStats " +
		"and the corresponding scanClaimSkipWired = true assignment, then retry")

// preflightScanClaimSkip enforces the structural-ordering invariant
// declared at audit §6 commit 5/6 prose: scanWithStats must consult
// the in-flight claim map. The flag flips to true at the install
// site in scanWithStats; if it has been reverted (commit 5 rolled
// back independently of commit 6), Start refuses to open any folder
// rather than silently re-opening Gap 5'. Pinned by
// TestStartup_RefusesWithoutClaimSkip.
func preflightScanClaimSkip() error {
	if !scanClaimSkipWired {
		return errScanClaimSkipMissing
	}
	return nil
}

// Start initializes and runs the filesync node. Blocks until ctx is cancelled.
func Start(ctx context.Context, cfg config.FilesyncCfg) error {
	if err := preflightScanClaimSkip(); err != nil {
		return err
	}
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

	// integrityChecks queues per-folder PRAGMA integrity_check
	// goroutines spawned below the folder loop where wg is in scope.
	var integrityChecks []integrityCheckTarget

	// Initialize folders. The per-folder open path is extracted to
	// (*Node).openFolderInit so the /reopen and /restore admin
	// endpoints can re-run the same logic post-init (audit §6
	// commit 9.2 / iter-4 O12).
	for _, fcfg := range cfg.ResolvedFolders {
		if t := n.openFolderInit(ctx, fcfg); t != nil {
			integrityChecks = append(integrityChecks, *t)
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

	// R2 / Gap 5: spawn the asynchronous integrity_check goroutine
	// for each folder that opened cleanly. A failure transitions the
	// folder to FolderDisabled (Z6: any in-flight writer tx is
	// canceled) but does not block the rest of Start. The goroutine
	// is owned by the Run context; cancel propagates from shutdown.
	for _, t := range integrityChecks {
		wg.Add(1)
		go func(fs *folderState, db *sql.DB) {
			defer wg.Done()
			if err := runIntegrityCheck(ctx, db); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				fs.disable(DisabledIntegrityCheck, err.Error(), "")
			}
		}(t.fs, t.db)
	}

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

	// Audit §6 commit 9 backup scheduler. Runs on a ticker
	// (backupCadence) and walks every active folder, calling
	// writeBackup on each, then gfsPrune to apply retention.
	// Errors are logged but never abort the loop — backup is
	// best-effort cleanup, not a correctness boundary. The
	// goroutine exits on ctx.Done.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(backupCadence)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.runBackupSweep(ctx)
			}
		}
	}()

	// Run initial scan (all folders).
	n.runScan(ctx, nil)
	close(n.firstScanDone)

	<-ctx.Done()

	// Audit §6 commit 8 / Gap 6: shutdown gets a bounded deadline.
	// Without one, a stuck SQLite tx (e.g., a filesystem stall on
	// the WAL fsync) would hang the process indefinitely. The
	// folder-level writerCtx derives from the parent ctx and is
	// already cancelled at this point; we now also re-derive a
	// shutdown-wide deadline for the per-folder persists below so
	// over-long transactions observe ctx.Err and roll back rather
	// than deadlock on COMMIT. 10 seconds is comfortably longer
	// than a healthy fsync (~ms) and short enough that a hung
	// shutdown surfaces as an operator-visible delay rather than
	// "the process never exits."
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTxTimeout)
	defer cancelShutdown()
	n.persistAllCtx(shutdownCtx)

	wg.Wait()

	// Close idle HTTP connections after all goroutines have stopped.
	for _, c := range n.peerClients {
		c.CloseIdleConnections()
	}
	n.defaultClient.CloseIdleConnections()

	closePerfLog()

	// L5: close os.Root handles. PERSISTENCE-AUDIT.md §2.7 L2:
	// also close the per-folder SQLite handles, AFTER wg.Wait so
	// no goroutine still holds rows. Close the reader before the
	// writer so any straggling read tx releases its WAL snapshot
	// before the writer's checkpoint flush.
	//
	// Audit §6 commit 8 phase B: PRAGMA optimize on the writer
	// before close. SQLite recommends running PRAGMA optimize at
	// shutdown so stale ANALYZE stats from updated tables are
	// refreshed lazily; the cost is bounded (read-only scan of
	// dirty tables) and the next folder open starts with fresh
	// query plans. Errors are logged but never block close — a
	// stale stat is a perf concern, not a correctness one.
	for _, fs := range n.folders {
		if fs.root != nil {
			_ = fs.root.Close()
		}
		if fs.dbReader != nil {
			_ = fs.dbReader.Close()
		}
		if fs.db != nil {
			if _, err := fs.db.Exec("PRAGMA optimize"); err != nil {
				slog.Debug("PRAGMA optimize at close failed (non-fatal)",
					"folder", fs.cfg.ID, "error", err)
			}
			_ = fs.db.Close()
		}
	}

	// Clean up state. Only the "filesync" bind accumulates metrics
	// (via GetMetrics); folders and peers are status-only entries.
	for _, fcfg := range cfg.ResolvedFolders {
		state.Global.Delete("filesync-folder", fcfg.ID)
		for _, peer := range fcfg.Peers {
			state.Global.Delete("filesync-peer", fcfg.ID+"|"+peer)
		}
	}
	state.Global.Delete("filesync", cfg.Bind)
	state.Global.DeleteMetrics("filesync", cfg.Bind)

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
		// PERSISTENCE-AUDIT.md §2.2 R8: skip folders that have
		// transitioned to FolderDisabled (open failed, integrity
		// check failed, etc.). Other folders and other mesh
		// components keep running.
		if fs.IsDisabled() {
			continue
		}
		// When dirtyRoots is set, skip folders not affected by fsnotify events.
		if dirtyRoots != nil && !dirtyRoots[fs.cfg.Path] {
			continue
		}

		folderStart := time.Now()
		_, memAfter := captureMemStats()

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
		existingFiles := fs.index.Len()
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
		changed, count, dirs, stats, conflicts, err := idxCopy.scanWithStats(ctx, fs.cfg.Path, ignore, maxFiles, fs.isClaimed)
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

		// Audit §6 commit 7 phase D / P6: tombstone GC runs every
		// tombstoneGCEvery scans (see counter on folderState). The
		// other 9 scans skip the O(N) Range walk that
		// purgeTombstones costs. Read+bump the counter under
		// indexMu so concurrent reads of fs see a consistent value.
		fs.indexMu.Lock()
		fs.scansSinceTombstoneGC++
		runTombstoneGC := fs.scansSinceTombstoneGC >= tombstoneGCEvery
		if runTombstoneGC {
			fs.scansSinceTombstoneGC = 0
		}
		fs.indexMu.Unlock()

		purgeStart := time.Now()
		var purged int
		if runTombstoneGC {
			purged = idxCopy.purgeTombstones(tombstoneMaxAge, peersCopy)
		}
		purgeDuration := time.Since(purgeStart)

		// Swap under a short write lock. Merge-preserve any entries that were
		// written after the scan started (concurrent sync downloads bumped
		// Sequence past scanStartSeq) so we don't clobber their work.
		swapStart := time.Now()
		fs.indexMu.Lock()
		mergedBack := 0
		for path, live := range fs.index.Range {
			if live.Sequence > scanStartSeq {
				// Merge-back of concurrent sync downloads. Bounded by
				// in-flight goroutines, never approaches the 1.5M cap.
				_ = idxCopy.Set(path, live)
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
		fs.reusableFiles = fs.index.files
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
		perfScan(id, stats, countAfterSwap, dirs, changed, ms(snapDuration), ms(purgeDuration), ms(swapDuration), memAfter())

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
		// Skip folders that have transitioned to FolderDisabled —
		// see §2.2 R8.
		if fs.IsDisabled() {
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
	// Snapshot cumulative byte counters so the perf event can report the
	// per-cycle delta without threading a counter through every download
	// path. Safe: these atomics only increase during the sync we own.
	bytesDownloadedBefore := fs.metrics.BytesDownloaded.Load()
	bytesSavedByRenameBefore := fs.metrics.BytesSavedByRename.Load()
	filesRenamedBefore := fs.metrics.FilesRenamed.Load()
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
	indexFetchStart := time.Now()
	remoteIdx, err := sendIndex(indexCtx, n.clientForPeer(peerAddr), peerAddr, exchange)
	indexFetchMs := ms(time.Since(indexFetchStart))
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

	// Detect peer restart or epoch flip: either condition triggers a
	// reset that drops BaseHashes and resets LastSeenSequence so the
	// next cycle does a full exchange. Audit §6 commit 7 phase B /
	// iter-4 Z2: see classifyPeerResetTrigger for the full contract.
	currentLastEpoch := ""
	if old, ok := fs.peers[peerAddr]; ok {
		currentLastEpoch = old.LastEpoch
	}
	remoteEpoch := remoteIdx.GetEpoch()
	if trigger := classifyPeerResetTrigger(remoteIdx.GetSequence(), peerLastSeq, remoteEpoch, currentLastEpoch); trigger != "" {
		slog.Info("peer reset detected, will do full exchange next cycle",
			"folder", folderID, "peer", peerAddr, "trigger", trigger,
			"remote_seq", remoteIdx.GetSequence(), "last_seen", peerLastSeq,
			"remote_epoch", remoteEpoch, "last_epoch", currentLastEpoch)
		fs.indexMu.Lock()
		// Store remote's new epoch as PendingEpoch. On the next
		// cycle's diff, downloads for locally-tombstoned files will
		// be filtered.
		var pendingEpoch string
		if remoteEpoch != "" && remoteEpoch != currentLastEpoch {
			pendingEpoch = remoteEpoch
		}
		fs.peers[peerAddr] = PeerState{
			LastSeenSequence: 0,
			LastSentSequence: 0,
			LastSync:         time.Now(),
			LastEpoch:        currentLastEpoch,
			PendingEpoch:     pendingEpoch,
			// BaseHashes intentionally unset: the reset path drops
			// them per the audit's branch A semantics. The next
			// successful sync re-populates via updateBaseHashes.
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
				if le, ok := fs.index.Get(a.Path); ok && le.Deleted {
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
				oldEntry, _ := fs.index.Get(rp.OldPath)
				if !oldEntry.Deleted {
					fs.index.Sequence++
					oldEntry.Deleted = true
					oldEntry.MtimeNS = time.Now().UnixNano()
					oldEntry.Sequence = fs.index.Sequence
					// C6 / R4: merge the peer's tombstone clock into the
					// local tombstone so it reflects their delete. Without
					// this, the local tombstone keeps the live file's old
					// clock and remains dominated by the peer's tombstone
					// forever, re-emitting ActionDelete on every diff.
					// Fall back to a self-bump when the peer had no clock
					// so the tombstone at least carries a non-empty vector.
					if len(rp.RemoteDelVersion) > 0 {
						oldEntry.Version = oldEntry.Version.merge(rp.RemoteDelVersion)
					} else if fs.index.selfID != "" {
						oldEntry.Version = oldEntry.Version.bump(fs.index.selfID)
					}
					// Bounded by per-action mutation rate; the 1.5M
					// dirty-set cap is unreachable here.
					_ = fs.index.Set(rp.OldPath, oldEntry)
				}
				// Write the new-path entry with remote metadata.
				fs.index.Sequence++
				// C6: merge local and remote clocks. Merge (not clone)
				// preserves any local components the peer did not carry,
				// e.g., a pre-C6 peer sending an empty clock, or a local
				// tombstone at rp.NewPath from an earlier delete.
				newLocal, _ := fs.index.Get(rp.NewPath)
				_ = fs.index.Set(rp.NewPath, FileEntry{
					Size:     rp.RemoteSize,
					MtimeNS:  rp.RemoteMtime,
					SHA256:   rp.RemoteHash,
					Sequence: fs.index.Sequence,
					Mode:     rp.RemoteMode,
					Version:  newLocal.Version.merge(rp.RemoteVersion),
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
						// C6: merge clocks so any local component the peer
						// did not carry (rolling upgrade, concurrent local
						// write) survives the adopt.
						localPrev, _ := fs.index.Get(path)
						_ = fs.index.Set(path, FileEntry{
							Size:     a.RemoteSize,
							MtimeNS:  a.RemoteMtime,
							SHA256:   a.RemoteHash,
							Sequence: fs.index.Sequence,
							Mode:     a.RemoteMode,
							Version:  localPrev.Version.merge(a.RemoteVersion),
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
				// Audit §6 commit 6 phase F (closes the C6 link with
				// commit 5): claimPath was acquired above before this
				// goroutine started; releasePath is deferred FIRST so
				// it runs LAST, after installDownloadedFile's commit
				// callback (which holds the SQLite write). The claim
				// therefore spans the entire .bak window — rename
				// original→bak, rename temp→original, SQLite commit,
				// unlink bak — covering the new race the F7 lifecycle
				// would otherwise open between rename and commit.
				// Pinned by TestDownload_HoldsClaimUntilTxCommit.
				defer wg.Done()
				defer fs.releasePath(action.Path)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				dlStart := time.Now()
				// Observe whether a local copy exists *before* the download
				// so perfDownload can report the likely transfer mode and
				// installDownloadedFile knows whether it'll write a .bak.
				hadLocal := false
				if _, statErr := fs.root.Stat(action.Path); statErr == nil {
					hadLocal = true
				}

				// Audit §6 commit 6 phase E: download to a verified temp
				// without renaming, then install via the F7 lifecycle.
				// installDownloadedFile owns the rename-original-to-bak,
				// rename-temp-to-original, commit, and unlink/restore
				// sequence. The commit callback runs the SQLite write +
				// in-memory mutation; on failure the .bak is restored
				// atomically.
				tmpRelPath, err := downloadToVerifiedFinalTemp(ctx, n.clientForPeer(peerAddr), peerAddr, folderID, action.Path, action.RemoteHash, fs.root, n.rateLimiter)
				dlDur := time.Since(dlStart)
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
					mode := "full"
					if hadLocal {
						mode = "delta"
					}
					perfDownload(DownloadPerfSummary{
						Folder:    folderID,
						Peer:      peerAddr,
						SizeBytes: action.RemoteSize,
						Mode:      mode,
						TotalMs:   ms(dlDur),
						Error:     err.Error(),
					})
					return
				}
				slog.Debug("file downloaded", "folder", folderID, "path", action.Path,
					"peer", peerAddr, "size", action.RemoteSize, "duration", dlDur)
				mode := "full"
				if hadLocal {
					mode = "delta"
				}
				perfDownload(DownloadPerfSummary{
					Folder:    folderID,
					Peer:      peerAddr,
					SizeBytes: action.RemoteSize,
					Mode:      mode,
					TotalMs:   ms(dlDur),
				})

				commit := func() error {
					// Best-effort metadata fixups on the freshly-installed
					// content. These run between installDownloadedFile's
					// step 2 (rename temp→original) and step 3 (this
					// callback's SQLite write). The .bak is still in
					// place so a Chtimes/Chmod failure doesn't break
					// rollback semantics.
					if action.RemoteMtime > 0 {
						mt := time.Unix(0, action.RemoteMtime)
						_ = fs.root.Chtimes(action.Path, mt, mt)
					}
					fileMode := os.FileMode(action.RemoteMode)
					if fileMode == 0 {
						fileMode = 0644
					}
					_ = fs.root.Chmod(action.Path, fileMode)

					// C2: post-write verification on network filesystems.
					// Returning an error here triggers the .bak restore
					// path inside installDownloadedFile — exactly the
					// rollback we want when write-back caching corrupted
					// the new content.
					if fs.isNetworkFS {
						actualHash, hashErr := hashFileRoot(fs.root, action.Path)
						if hashErr != nil {
							return fmt.Errorf("post-write verify read: %w", hashErr)
						}
						if actualHash != action.RemoteHash {
							slog.Error("C2: post-write verification failed: data corruption detected",
								"folder", folderID, "path", action.Path, "peer", peerAddr,
								"expected", action.RemoteHash, "actual", actualHash)
							return fmt.Errorf("post-write verify hash mismatch: have %s, want %s",
								actualHash, action.RemoteHash)
						}
					}

					// Build the new entry and a single-path snapshot for
					// SQLite. The Sequence bump and the in-memory Set are
					// deferred until after saveIndex commits — a Sequence
					// gap on commit failure is acceptable, but a stale
					// in-memory entry paired with on-disk old content is
					// the Gap 2' shape Phase D explicitly fences out.
					fs.indexMu.RLock()
					localPrev, _ := fs.index.Get(action.Path)
					epoch := fs.index.Epoch
					deviceID := fs.index.DeviceID
					fs.indexMu.RUnlock()

					fs.indexMu.Lock()
					fs.index.Sequence++
					seq := fs.index.Sequence
					fs.indexMu.Unlock()

					newEntry := FileEntry{
						Size:     action.RemoteSize,
						MtimeNS:  action.RemoteMtime,
						SHA256:   action.RemoteHash,
						Sequence: seq,
						Mode:     action.RemoteMode,
						Version:  localPrev.Version.merge(action.RemoteVersion),
					}
					snapshot := newFileIndex()
					snapshot.Sequence = seq
					snapshot.Epoch = epoch
					snapshot.DeviceID = deviceID
					// Single-path snapshot — the dirty-set cap cannot fire on a fresh FileIndex with one Set.
					_ = snapshot.Set(action.Path, newEntry)

					if err := saveIndex(fs.writerCtx, fs.db, folderID, snapshot); err != nil {
						return fmt.Errorf("saveIndex: %w", err)
					}

					// SQLite committed — promote the new entry into the
					// live in-memory FileIndex. Other goroutines in this
					// sync cycle observe the post-download state.
					fs.indexMu.Lock()
					_ = fs.index.Set(action.Path, newEntry)
					fs.retries.clearAll(action.Path)
					fs.indexMu.Unlock()

					m := state.Global.GetMetrics("filesync", n.cfg.Bind)
					m.BytesRx.Add(action.RemoteSize)
					fs.metrics.FilesDownloaded.Add(1)
					fs.metrics.BytesDownloaded.Add(action.RemoteSize)
					return nil
				}

				if err := installDownloadedFile(fs.root, action.Path, tmpRelPath, commit, &fs.metrics); err != nil {
					slog.Warn("F7 install failed", "folder", folderID, "path", action.Path, "peer", peerAddr, "error", err)
					fs.indexMu.Lock()
					fs.retries.record(action.Path, peerAddr, action.RemoteHash)
					fs.indexMu.Unlock()
					failMu.Lock()
					setFailReason("install: " + err.Error())
					failedSeqs = append(failedSeqs, action.RemoteSequence)
					failMu.Unlock()
					fs.metrics.SyncErrors.Add(1)
					return
				}

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
						// C6: identical content — merge clocks so the
						// path converges without losing local components
						// the peer did not carry.
						localPrev, _ := fs.index.Get(action.Path)
						_ = fs.index.Set(action.Path, FileEntry{
							Size:     action.RemoteSize,
							MtimeNS:  action.RemoteMtime,
							SHA256:   action.RemoteHash,
							Sequence: fs.index.Sequence,
							Mode:     action.RemoteMode,
							Version:  localPrev.Version.merge(action.RemoteVersion),
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
					// C6: remote wins — merge clocks so local components
					// the peer did not carry survive the adopt.
					localPrev, _ := fs.index.Get(action.Path)
					_ = fs.index.Set(action.Path, FileEntry{
						Size:     action.RemoteSize,
						MtimeNS:  action.RemoteMtime,
						SHA256:   action.RemoteHash,
						Sequence: fs.index.Sequence,
						Mode:     action.RemoteMode,
						Version:  localPrev.Version.merge(action.RemoteVersion),
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
					//
					// R11: merge remote's clock components into local and
					// bump self so the resolved entry dominates what we saw
					// from the peer. Without this, the next time the peer
					// pushes the same (or any concurrent) state, our clock
					// will still be concurrent with theirs and we'll enter
					// conflict again. File contents are unchanged — only
					// the clock and sequence advance to encode the
					// "local-wins" decision.
					fs.indexMu.Lock()
					entry, exists := fs.index.Get(action.Path)
					if exists && !entry.Deleted {
						fs.index.Sequence++
						entry.Sequence = fs.index.Sequence
						entry.Version = entry.Version.merge(action.RemoteVersion)
						if fs.index.selfID != "" {
							entry.Version = entry.Version.bump(fs.index.selfID)
						}
						_ = fs.index.Set(action.Path, entry)
					}
					fs.indexMu.Unlock()
				}
				slog.Info("resolved conflict", "folder", folderID, "path", action.Path, "winner", winner)
			}()

		case ActionDelete:
			// F10: run deletes through the semaphore like downloads so
			// high-latency filesystems (NFS, FUSE) don't block dispatch.
			//
			// Audit §6 commit 6 phase G: the delete write path now
			// goes through installDeletion, which renames the local
			// file to a .mesh-bak-<hash> sidecar BEFORE running the
			// SQLite tombstone commit. A crash between the rename
			// and the commit leaves a sweep-recognized .bak that
			// Phase I reconciles against the SQLite row at folder
			// open. The claim spans the entire goroutine (Phase F),
			// so commit 5's scan walker skip covers the .bak window.
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

				commit := func() error {
					// Build the tombstone entry and a single-path
					// snapshot. The Sequence bump and in-memory Set
					// are deferred until SQLite commits.
					fs.indexMu.RLock()
					prior, _ := fs.index.Get(action.Path)
					epoch := fs.index.Epoch
					deviceID := fs.index.DeviceID
					selfID := fs.index.selfID
					fs.indexMu.RUnlock()

					if prior.Deleted {
						// N12: already a tombstone on our side; nothing to
						// commit. The .bak rename has already been done
						// (since the file existed on disk, otherwise
						// installDeletion's step 1 was a no-op and we
						// land here with no rollback to perform).
						return nil
					}

					fs.indexMu.Lock()
					fs.index.Sequence++
					seq := fs.index.Sequence
					fs.indexMu.Unlock()

					tomb := prior
					tomb.Deleted = true
					tomb.MtimeNS = time.Now().UnixNano()
					tomb.Sequence = seq
					// C6: merge the peer's tombstone clock with our
					// own so any local component the peer did not
					// carry survives. Fall back to a self-bump when
					// the peer has no clock.
					if len(action.RemoteVersion) > 0 {
						tomb.Version = prior.Version.merge(action.RemoteVersion)
					} else if selfID != "" {
						tomb.Version = prior.Version.bump(selfID)
					}

					snapshot := newFileIndex()
					snapshot.Sequence = seq
					snapshot.Epoch = epoch
					snapshot.DeviceID = deviceID
					_ = snapshot.Set(action.Path, tomb)

					if err := saveIndex(fs.writerCtx, fs.db, folderID, snapshot); err != nil {
						return fmt.Errorf("saveIndex (tombstone): %w", err)
					}

					fs.indexMu.Lock()
					_ = fs.index.Set(action.Path, tomb)
					fs.indexMu.Unlock()
					fs.metrics.FilesDeleted.Add(1)
					return nil
				}

				if err := installDeletion(fs.root, action.Path, commit, &fs.metrics); err != nil {
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
	perfSync(folderID, peerAddr, SyncPerfSummary{
		RemoteEntries:   remoteEntries,
		Downloads:       downloads,
		Conflicts:       conflicts,
		Deletes:         deletes,
		Failed:          len(failedSeqs),
		Renames:         int(fs.metrics.FilesRenamed.Load() - filesRenamedBefore),
		BytesPlanned:    totalBytes,
		BytesDownloaded: fs.metrics.BytesDownloaded.Load() - bytesDownloadedBefore,
		BytesSavedRname: fs.metrics.BytesSavedByRename.Load() - bytesSavedByRenameBefore,
		DurationMs:      ms(syncDuration),
		IndexFetchMs:    indexFetchMs,
		FirstFailReason: firstFailReason,
	})
}

// buildIndexExchange creates a protobuf IndexExchange from the local
// index by querying SQLite (audit §6 commit 4 / INV-1). Reads go
// through fs.dbReader so concurrent exchanges across peers do not
// serialize behind the writer's MaxOpenConns=1; WAL snapshot
// isolation guarantees a consistent view even if a write tx is in
// flight (audit C4 + N4).
//
// If sinceSequence > 0, only entries with Sequence > sinceSequence
// are included (delta mode); the files_by_seq index makes the query
// O(log N + delta). Sequence==0 returns the full index.
//
// folder_meta scalars (Sequence, Epoch) and the device ID still come
// from the in-memory state — they are folder-level singletons that
// the writer keeps current and the indexMu RLock makes consistent.
// The audit's INV-1 applies to per-row state; folder-level scalars
// are an implementation detail of how a peer reads "what version of
// this folder are you at right now."
func (n *Node) buildIndexExchange(folderID string, sinceSequence int64) *pb.IndexExchange {
	fs, ok := n.folders[folderID]
	if !ok {
		return &pb.IndexExchange{
			ProtocolVersion: protocolVersion,
			IndexModel:      FILESYNC_INDEX_MODEL,
		}
	}

	// Folder-level scalars under RLock. The dbReader query runs
	// without indexMu so a concurrent write tx never blocks it.
	fs.indexMu.RLock()
	currentSeq := fs.index.Sequence
	currentEpoch := fs.index.Epoch
	fs.indexMu.RUnlock()

	// Disabled folders surface a minimal protocol-version-only
	// response — the peer will see the empty Files list and try
	// again on the next cycle. The handshake guards above will
	// have already declined the request earlier, but defense in
	// depth is cheap.
	if fs.dbReader == nil {
		return &pb.IndexExchange{
			DeviceId:        n.deviceID,
			FolderId:        folderID,
			Sequence:        currentSeq,
			Epoch:           currentEpoch,
			ProtocolVersion: protocolVersion,
			IndexModel:      FILESYNC_INDEX_MODEL,
		}
	}

	files := make([]*pb.FileInfo, 0, 64)
	err := queryFilesSinceSeq(context.Background(), fs.dbReader, folderID, sinceSequence,
		func(path string, e FileEntry) bool {
			files = append(files, &pb.FileInfo{
				Path:     path,
				Size:     e.Size,
				MtimeNs:  e.MtimeNS,
				Sha256:   append([]byte(nil), e.SHA256[:]...),
				Deleted:  e.Deleted,
				Sequence: e.Sequence,
				Mode:     e.Mode,
				PrevPath: e.PrevPath,
				Version:  e.Version.toProto(),
			})
			return true
		},
	)
	if err != nil {
		// SQLite read errors should not crash the peer-facing
		// handler. Log and serve an empty delta; the peer retries
		// on the next cycle.
		slog.Warn("buildIndexExchange: SQLite read failed",
			"folder", folderID, "since", sinceSequence, "error", err)
	}

	return &pb.IndexExchange{
		DeviceId:        n.deviceID,
		FolderId:        folderID,
		Sequence:        currentSeq,
		Epoch:           currentEpoch,
		Files:           files,
		ProtocolVersion: protocolVersion,
		IndexModel:      FILESYNC_INDEX_MODEL,
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

// persistAll saves all folder indices and peer states to disk
// using each folder's writerCtx. Used by hot-path persists where
// the writer has its own folder-level ctx that disable() can
// cancel.
func (n *Node) persistAll() {
	for id := range n.folders {
		n.persistFolder(id, true)
	}
}

// persistAllCtx is the shutdown-path persist that overrides each
// folder's writerCtx with a single bounded shutdown deadline
// (audit §6 commit 8 / Gap 6). Each folder's saveIndex /
// savePeerSyncOutcome runs under shutdownCtx so a stuck COMMIT
// trips ctx.Done and rolls back rather than wedging the process.
// Per-folder failures are logged and the loop continues — every
// folder gets its persist attempt within the shared deadline.
func (n *Node) persistAllCtx(shutdownCtx context.Context) {
	for id, fs := range n.folders {
		// Swap the folder's writerCtx for the shutdown-wide ctx.
		// Saved state is restored on exit so post-shutdown
		// teardown of the folder still observes a sane writerCtx
		// (the folder is closing anyway, but defensive).
		fs.indexMu.Lock()
		origCtx := fs.writerCtx
		fs.writerCtx = shutdownCtx
		fs.indexMu.Unlock()

		n.persistFolder(id, true)

		fs.indexMu.Lock()
		fs.writerCtx = origCtx
		fs.indexMu.Unlock()
	}
}

// persistFolder saves a single folder's index and peer state to
// SQLite (PERSISTENCE-AUDIT.md §6 commit 2).
//
// N10: serialized via persistMu to prevent concurrent syncFolder
// goroutines from racing on the same write transaction. P17a:
// skips unchanged components — saveIndex is gated by indexDirty,
// savePeerStatesDB is gated by peersDirty. force=true bypasses both
// checks (used at shutdown).
//
// P2: saveIndex writes only the paths the FileIndex marks dirty
// since the last successful persist; ClearDirty fires after a
// commit succeeds. On commit failure the dirty/deleted sets stay
// populated so the next cycle retries the same rows.
func (n *Node) persistFolder(folderID string, force bool) {
	fs, ok := n.folders[folderID]
	if !ok {
		return
	}
	if fs.db == nil {
		// Folder failed to open SQLite at startup; skip silently —
		// commit 3 (FolderDisabled scaffold) wires the proper status
		// transition. For now, the in-memory fs.index is the only
		// state and it lives until process exit.
		return
	}

	fs.persistMu.Lock()
	defer fs.persistMu.Unlock()

	fs.indexMu.Lock()
	shouldSaveIndex := force || fs.indexDirty
	shouldSavePeers := force || fs.peersDirty
	if shouldSaveIndex {
		fs.indexDirty = false
	}
	if shouldSavePeers {
		fs.peersDirty = false
	}
	fs.indexMu.Unlock()

	if !shouldSaveIndex && !shouldSavePeers {
		perfPersist(folderID, 0, 0, 0, true)
		return
	}

	// Snapshot under the index lock so the SQLite write does not
	// hold the lock across the commit fsync. The clone is shallow —
	// FileEntry values are immutable in practice (mutation paths
	// allocate new VectorClock maps). The dirty/deleted sets ride
	// with the clone.
	var idxSnapshot *FileIndex
	var peersSnapshot map[string]PeerState
	fs.indexMu.RLock()
	if shouldSaveIndex {
		idxSnapshot = fs.index.clone()
		// clone() does NOT copy the dirty/deleted sets; carry them
		// explicitly so saveIndex sees what's pending.
		dirty, deleted := fs.index.DirtyPaths()
		idxSnapshot.dirty = dirty
		idxSnapshot.deleted = deleted
	}
	if shouldSavePeers {
		peersSnapshot = make(map[string]PeerState, len(fs.peers))
		for k, v := range fs.peers {
			peersSnapshot[k] = v
		}
	}
	fs.indexMu.RUnlock()

	var indexBytes int
	var indexMs, peersMs float64

	switch {
	case shouldSaveIndex && shouldSavePeers:
		// Audit §6 commit 6 phase C: file rows and peer-state rows ride
		// ONE BEGIN IMMEDIATE...COMMIT so a crash mid-persist cannot
		// leave a fresh file row paired with a stale BaseHashes map
		// (which the Phase D classifier would read as "unknown
		// ancestor → conflict"). Closes Gap 2 / Gap 2'.
		idxStart := time.Now()
		err := savePeerSyncOutcome(fs.writerCtx, fs.db, folderID, idxSnapshot, peersSnapshot)
		bothMs := ms(time.Since(idxStart))
		if err != nil {
			slog.Warn("failed to save peer-sync outcome to SQLite",
				"folder", folderID, "error", err)
			fs.indexMu.Lock()
			fs.indexDirty = true
			fs.peersDirty = true
			fs.indexMu.Unlock()
		} else {
			fs.indexMu.Lock()
			fs.index.ClearDirty()
			fs.indexMu.Unlock()
		}
		// Both halves rode one tx; the perf log keeps two columns for
		// continuity with single-half writes — split the timing
		// proportionally to row count so the columns still sum to the
		// observed total.
		idxRows := float64(idxSnapshot.Len())
		peerCount := float64(len(peersSnapshot))
		if total := idxRows + peerCount; total > 0 {
			indexMs = bothMs * idxRows / total
			peersMs = bothMs * peerCount / total
		} else {
			indexMs = bothMs
		}
		indexBytes = idxSnapshot.Len()

	case shouldSaveIndex:
		idxStart := time.Now()
		if err := saveIndex(fs.writerCtx, fs.db, folderID, idxSnapshot); err != nil {
			slog.Warn("failed to save index to SQLite",
				"folder", folderID, "error", err)
			fs.indexMu.Lock()
			fs.indexDirty = true // retry next cycle
			fs.indexMu.Unlock()
		} else {
			fs.indexMu.Lock()
			fs.index.ClearDirty()
			fs.indexMu.Unlock()
		}
		indexMs = ms(time.Since(idxStart))
		indexBytes = idxSnapshot.Len()

	case shouldSavePeers:
		peersStart := time.Now()
		if err := savePeerStatesDB(fs.writerCtx, fs.db, folderID, peersSnapshot); err != nil {
			slog.Warn("failed to save peer states to SQLite",
				"folder", folderID, "error", err)
			fs.indexMu.Lock()
			fs.peersDirty = true // retry next cycle
			fs.indexMu.Unlock()
		}
		peersMs = ms(time.Since(peersStart))
	}
	perfPersist(folderID, indexBytes, indexMs, peersMs, !shouldSaveIndex)
}

// protoToFileIndex converts a protobuf IndexExchange to our internal FileIndex.
func protoToFileIndex(idx *pb.IndexExchange) *FileIndex {
	fi := &FileIndex{
		Sequence: idx.GetSequence(),
		Epoch:    idx.GetEpoch(),
		files:    make(map[string]FileEntry, len(idx.GetFiles())),
	}
	for _, f := range idx.GetFiles() {
		// B17: normalize remote paths to NFC for cross-platform consistency.
		path := norm.NFC.String(f.GetPath())
		_ = fi.Set(path, FileEntry{
			Size:     f.GetSize(),
			MtimeNS:  f.GetMtimeNs(),
			SHA256:   hash256FromBytes(f.GetSha256()),
			Deleted:  f.GetDeleted(),
			Sequence: f.GetSequence(),
			Mode:     f.GetMode(),
			PrevPath: norm.NFC.String(f.GetPrevPath()),
			Version:  vectorClockFromProto(f.GetVersion()),
		})
	}
	fi.recomputeCache() // P18b
	return fi
}

// localMtime returns the local mtime for a file, or 0 if not found.
func localMtime(fs *folderState, relPath string) int64 {
	fs.indexMu.RLock()
	defer fs.indexMu.RUnlock()
	if entry, ok := fs.index.Get(relPath); ok {
		return entry.MtimeNS
	}
	return 0
}
