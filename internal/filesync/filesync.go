package filesync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"github.com/mmdemirbas/mesh/internal/nodeutil"
	"github.com/mmdemirbas/mesh/internal/state"
)

const (
	tombstoneMaxAge = 30 * 24 * time.Hour // 30 days
)

// activeNodes tracks running filesync nodes for admin API access.
var activeNodes nodeutil.Registry[Node]

// FolderStatus is an exported summary of a folder's sync state for the admin API.
type FolderStatus struct {
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	Direction string   `json:"direction"`
	FileCount int      `json:"file_count"`
	Peers     []string `json:"peers"`
}

// ConflictInfo describes a conflict file found in a synced folder.
type ConflictInfo struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
}

// GetFolderStatuses returns status summaries for all active filesync folders.
func GetFolderStatuses() []FolderStatus {
	var result []FolderStatus
	activeNodes.ForEach(func(n *Node) {
		for id, fs := range n.folders {
			fs.indexMu.RLock()
			count := fs.index.activeCount()
			fs.indexMu.RUnlock()

			result = append(result, FolderStatus{
				ID:        id,
				Path:      fs.cfg.Path,
				Direction: fs.cfg.Direction,
				FileCount: count,
				Peers:     fs.cfg.Peers,
			})
		}
	})
	return result
}

// GetConflicts returns all conflict files across all active filesync folders.
func GetConflicts() []ConflictInfo {
	var result []ConflictInfo
	activeNodes.ForEach(func(n *Node) {
		for id, fs := range n.folders {
			conflicts, _ := listConflicts(fs.cfg.Path)
			for _, c := range conflicts {
				result = append(result, ConflictInfo{FolderID: id, Path: c})
			}
		}
	})
	return result
}

// folderState holds runtime state for a single synced folder.
type folderState struct {
	cfg     config.FolderCfg
	index   *FileIndex
	ignore  *ignoreMatcher
	peers   map[string]PeerState // peerAddr -> state
	indexMu sync.RWMutex
}

// Node is the runtime instance for a filesync configuration.
type Node struct {
	cfg      config.FilesyncCfg
	deviceID string
	dataDir  string // ~/.mesh/filesync/

	folders map[string]*folderState // folderID -> state
	mu      sync.RWMutex

	httpClient *http.Client

	// rateLimiter throttles file transfer bandwidth. nil means unlimited.
	rateLimiter *rate.Limiter

	// scanTrigger signals the sync loop that a scan completed with changes.
	scanTrigger chan struct{}
}

// Start initializes and runs the filesync node. Blocks until ctx is cancelled.
func Start(ctx context.Context, cfg config.FilesyncCfg) error {
	deviceID := generateDeviceID()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home dir: %w", err)
	}
	dataDir := filepath.Join(home, ".mesh", "filesync")

	n := &Node{
		cfg:      cfg,
		deviceID: deviceID,
		dataDir:  dataDir,
		folders:  make(map[string]*folderState),
		httpClient: &http.Client{
			Timeout: 10 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		scanTrigger: make(chan struct{}, 1),
	}

	// Set up bandwidth limiter.
	if cfg.MaxBandwidth != "" {
		bps, err := config.ParseBandwidth(cfg.MaxBandwidth)
		if err == nil && bps > 0 {
			n.rateLimiter = rate.NewLimiter(rate.Limit(bps), int(min(bps, 1<<20))) // burst up to 1MB or bps
			slog.Info("filesync bandwidth throttle", "bytes_per_sec", bps)
		}
	}

	// Initialize folders.
	for _, fcfg := range cfg.ResolvedFolders {
		// Ensure folder root exists.
		if _, err := os.Stat(fcfg.Path); err != nil {
			return fmt.Errorf("folder %q path %q: %w", fcfg.ID, fcfg.Path, err)
		}

		// Load or create index.
		idxPath := filepath.Join(dataDir, fcfg.ID, "index.yaml")
		idx, err := loadIndex(idxPath)
		if err != nil {
			slog.Warn("failed to load index, starting fresh", "folder", fcfg.ID, "error", err)
			idx = newFileIndex()
		}

		// Load peer states.
		peersPath := filepath.Join(dataDir, fcfg.ID, "peers.yaml")
		peers, err := loadPeerStates(peersPath)
		if err != nil {
			slog.Warn("failed to load peer states, starting fresh", "folder", fcfg.ID, "error", err)
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

		ignore := newIgnoreMatcher(fcfg.IgnorePatterns)

		fs := &folderState{
			cfg:    fcfg,
			index:  idx,
			ignore: ignore,
			peers:  peers,
		}
		n.folders[fcfg.ID] = fs

		if fcfg.Direction == "disabled" {
			state.Global.Update("filesync-folder", fcfg.ID, state.Connected, "disabled")
		} else {
			state.Global.Update("filesync-folder", fcfg.ID, state.Starting, fcfg.Path)
			for _, peer := range fcfg.Peers {
				state.Global.Update("filesync-peer", fcfg.ID+"|"+peer, state.Connecting, "")
			}
		}
	}

	// Update global state.
	state.Global.Update("filesync", cfg.Bind, state.Listening, "")
	activeNodes.Register(n)
	defer activeNodes.Unregister(n)

	// Start HTTP server.
	srv := &server{node: n}
	httpSrv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           srv.handler(),
	}

	ln, err := net.Listen("tcp", cfg.Bind)
	if err != nil {
		state.Global.Update("filesync", cfg.Bind, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", cfg.Bind, err)
	}

	slog.Info("filesync listening", "bind", ln.Addr().String(), "device_id", deviceID, "folders", len(cfg.ResolvedFolders))

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

	watcher, watchErr := newFolderWatcher(roots, ignoreMap)
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

	// Run initial scan.
	n.runScan()

	<-ctx.Done()

	// Persist state before exit.
	n.persistAll()

	wg.Wait()

	// Close idle HTTP connections after all goroutines have stopped.
	n.httpClient.CloseIdleConnections()

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
			n.runScan()
		case <-dirtyCh:
			n.runScan()
		}
	}
}

// runScan scans all folders and triggers sync if anything changed.
func (n *Node) runScan() {
	anyChanged := false
	for id, fs := range n.folders {
		if fs.cfg.Direction == "disabled" {
			continue
		}
		fs.indexMu.Lock()
		state.Global.Update("filesync-folder", id, state.Connecting, "scanning")

		changed, count, err := fs.index.scan(fs.cfg.Path, fs.ignore)
		if err != nil {
			slog.Warn("scan error", "folder", id, "error", err)
		}

		// Purge old tombstones.
		fs.index.purgeTombstones(tombstoneMaxAge)
		state.Global.Update("filesync-folder", id, state.Connected, "idle")
		state.Global.UpdateFileCount("filesync-folder", id, count)
		fs.indexMu.Unlock()

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
	// Short initial delay to allow the first scan to complete.
	initDelay := time.NewTimer(2 * time.Second)
	select {
	case <-ctx.Done():
		initDelay.Stop()
		return
	case <-initDelay.C:
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
func (n *Node) syncAllPeers(ctx context.Context) {
	sem := make(chan struct{}, n.cfg.MaxConcurrent)

	for _, fs := range n.folders {
		if fs.cfg.Direction == "disabled" {
			continue
		}
		for _, peer := range fs.cfg.Peers {
			if ctx.Err() != nil {
				return
			}
			n.syncFolder(ctx, fs, peer, sem)
		}
	}
}

// syncFolder exchanges indices with a peer and downloads missing/newer files.
func (n *Node) syncFolder(ctx context.Context, fs *folderState, peerAddr string, sem chan struct{}) {
	folderID := fs.cfg.ID
	stateKey := folderID + "|" + peerAddr

	state.Global.Update("filesync-folder", folderID, state.Connecting, "syncing with "+peerAddr)

	// Build and send our index, requesting only entries newer than what we've seen.
	fs.indexMu.RLock()
	peerLastSeq := int64(0)
	ourLastSentSeq := int64(0)
	if ps, ok := fs.peers[peerAddr]; ok {
		peerLastSeq = ps.LastSeenSequence
		ourLastSentSeq = ps.LastSentSequence
	}
	ourCurrentSeq := fs.index.Sequence
	fs.indexMu.RUnlock()

	exchange := n.buildIndexExchange(folderID, ourLastSentSeq) // send only entries newer than last sent
	exchange.Since = peerLastSeq                               // ask peer to send only entries newer than this
	remoteIdx, err := sendIndex(ctx, n.httpClient, peerAddr, exchange)
	if err != nil {
		slog.Debug("sync failed", "folder", folderID, "peer", peerAddr, "error", err)
		state.Global.Update("filesync-peer", stateKey, state.Retrying, err.Error())
		return
	}

	state.Global.Update("filesync-peer", stateKey, state.Connected, "")

	// Detect peer restart: if remote sequence dropped below what we last saw,
	// reset tracking and request a full exchange on the next cycle.
	if remoteIdx.GetSequence() < peerLastSeq {
		slog.Info("peer sequence reset detected, will do full exchange next cycle",
			"folder", folderID, "peer", peerAddr,
			"remote_seq", remoteIdx.GetSequence(), "last_seen", peerLastSeq)
		fs.indexMu.Lock()
		fs.peers[peerAddr] = PeerState{LastSeenSequence: 0, LastSentSequence: 0, LastSync: time.Now()}
		fs.indexMu.Unlock()
		return
	}

	// Convert remote protobuf index to our internal format for diffing.
	remoteFileIndex := protoToFileIndex(remoteIdx)

	fs.indexMu.Lock()
	lastSeenSeq := peerLastSeq

	actions := fs.index.diff(remoteFileIndex, lastSeenSeq, fs.cfg.Direction)
	fs.indexMu.Unlock()

	if len(actions) == 0 {
		// Update peer state even when nothing to do.
		fs.indexMu.Lock()
		fs.peers[peerAddr] = PeerState{
			LastSeenSequence: remoteIdx.GetSequence(),
			LastSentSequence: ourCurrentSeq,
			LastSync:         time.Now(),
		}
		fs.indexMu.Unlock()

		fs.indexMu.RLock()
		count := fs.index.activeCount()
		fs.indexMu.RUnlock()
		now := time.Now()
		state.Global.Update("filesync-folder", folderID, state.Connected, "idle")
		state.Global.UpdateFileCount("filesync-folder", folderID, count)
		state.Global.UpdateLastSync("filesync-folder", folderID, now)
		return
	}

	downloads := countActions(actions, ActionDownload)
	conflicts := countActions(actions, ActionConflict)
	deletes := countActions(actions, ActionDelete)
	slog.Info("sync actions", "folder", folderID, "peer", peerAddr, "downloads", downloads, "conflicts", conflicts, "deletes", deletes)

	// Dry-run: log what would happen but don't modify files or update peer state.
	if fs.cfg.Direction == "dry-run" {
		state.Global.Update("filesync-folder", folderID, state.Connected,
			fmt.Sprintf("dry-run: %d downloads, %d deletes, %d conflicts", downloads, deletes, conflicts))
		state.Global.UpdateLastSync("filesync-folder", folderID, time.Now())
		return
	}

	state.Global.Update("filesync-folder", folderID, state.Connecting, fmt.Sprintf("syncing %d files", len(actions)))

	var wg sync.WaitGroup
	for _, action := range actions {
		if ctx.Err() != nil {
			break
		}

		switch action.Action {
		case ActionDownload:
			wg.Add(1)
			action := action
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				err := downloadFromPeer(ctx, n.httpClient, peerAddr, folderID, action.Path, action.RemoteHash, fs.cfg.Path, n.rateLimiter)
				if err != nil {
					slog.Warn("download failed", "folder", folderID, "path", action.Path, "peer", peerAddr, "error", err)
					return
				}

				// Update metrics.
				m := state.Global.GetMetrics("filesync", n.cfg.Bind)
				m.BytesRx.Add(action.RemoteSize)

				// Update local index.
				fs.indexMu.Lock()
				fs.index.Sequence++
				fs.index.Files[action.Path] = FileEntry{
					Size:     action.RemoteSize,
					MtimeNS:  action.RemoteMtime,
					SHA256:   action.RemoteHash,
					Sequence: fs.index.Sequence,
				}
				fs.indexMu.Unlock()

				slog.Info("synced file", "folder", folderID, "path", action.Path, "peer", peerAddr)
			}()

		case ActionConflict:
			remoteDeviceID := remoteIdx.GetDeviceId()
			winner, err := resolveConflict(fs.cfg.Path, action.Path, localMtime(fs, action.Path), action.RemoteMtime, remoteDeviceID)
			if err != nil {
				slog.Warn("conflict resolution failed", "folder", folderID, "path", action.Path, "error", err)
				continue
			}
			if winner == "remote" {
				// Download the remote version to replace local.
				err := downloadFromPeer(ctx, n.httpClient, peerAddr, folderID, action.Path, action.RemoteHash, fs.cfg.Path, n.rateLimiter)
				if err != nil {
					slog.Warn("conflict download failed", "folder", folderID, "path", action.Path, "error", err)
					continue
				}
				fs.indexMu.Lock()
				fs.index.Sequence++
				fs.index.Files[action.Path] = FileEntry{
					Size:     action.RemoteSize,
					MtimeNS:  action.RemoteMtime,
					SHA256:   action.RemoteHash,
					Sequence: fs.index.Sequence,
				}
				fs.indexMu.Unlock()
			}
			slog.Info("resolved conflict", "folder", folderID, "path", action.Path, "winner", winner)

		case ActionDelete:
			if err := deleteFile(fs.cfg.Path, action.Path); err != nil {
				slog.Warn("delete failed", "folder", folderID, "path", action.Path, "error", err)
				continue
			}
			fs.indexMu.Lock()
			fs.index.Sequence++
			if entry, ok := fs.index.Files[action.Path]; ok {
				entry.Deleted = true
				entry.MtimeNS = time.Now().UnixNano() // deletion time for tombstone age
				entry.Sequence = fs.index.Sequence
				fs.index.Files[action.Path] = entry
			}
			fs.indexMu.Unlock()
			slog.Info("deleted file", "folder", folderID, "path", action.Path, "peer", peerAddr)
		}
	}
	wg.Wait()

	// Update peer state.
	fs.indexMu.Lock()
	fs.peers[peerAddr] = PeerState{
		LastSeenSequence: remoteIdx.GetSequence(),
		LastSentSequence: ourCurrentSeq,
		LastSync:         time.Now(),
	}
	fs.indexMu.Unlock()

	// Persist index after sync.
	n.persistFolder(folderID)

	fs.indexMu.RLock()
	count := fs.index.activeCount()
	fs.indexMu.RUnlock()
	state.Global.Update("filesync-folder", folderID, state.Connected, "idle")
	state.Global.UpdateFileCount("filesync-folder", folderID, count)
	state.Global.UpdateLastSync("filesync-folder", folderID, time.Now())
}

// buildIndexExchange creates a protobuf IndexExchange from the local index.
// If sinceSequence > 0, only entries with Sequence > sinceSequence are included (delta mode).
func (n *Node) buildIndexExchange(folderID string, sinceSequence int64) *pb.IndexExchange {
	fs, ok := n.folders[folderID]
	if !ok {
		return &pb.IndexExchange{}
	}

	fs.indexMu.RLock()
	defer fs.indexMu.RUnlock()

	files := make([]*pb.FileInfo, 0, len(fs.index.Files))
	for path, entry := range fs.index.Files {
		if sinceSequence > 0 && entry.Sequence <= sinceSequence {
			continue
		}
		fi := &pb.FileInfo{
			Path:     path,
			Size:     entry.Size,
			MtimeNs:  entry.MtimeNS,
			Sha256:   hexToBytes(entry.SHA256),
			Deleted:  entry.Deleted,
			Sequence: entry.Sequence,
		}
		files = append(files, fi)
	}

	return &pb.IndexExchange{
		DeviceId: n.deviceID,
		FolderId: folderID,
		Sequence: fs.index.Sequence,
		Files:    files,
	}
}

// findFolder returns the folder state for the given ID, or nil.
func (n *Node) findFolder(folderID string) *folderState {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.folders[folderID]
}

// isPeerConfigured checks if the given IP is a configured peer for any folder.
func (n *Node) isPeerConfigured(requestIP string) bool {
	for _, fs := range n.folders {
		for _, peer := range fs.cfg.Peers {
			if peerMatchesAddr(peer, requestIP) {
				return true
			}
		}
	}
	return false
}

// persistAll saves all folder indices and peer states to disk.
func (n *Node) persistAll() {
	for id := range n.folders {
		n.persistFolder(id)
	}
}

// persistFolder saves a single folder's index and peer state.
func (n *Node) persistFolder(folderID string) {
	fs, ok := n.folders[folderID]
	if !ok {
		return
	}

	fs.indexMu.RLock()
	defer fs.indexMu.RUnlock()

	idxPath := filepath.Join(n.dataDir, folderID, "index.yaml")
	if err := fs.index.save(idxPath); err != nil {
		slog.Warn("failed to save index", "folder", folderID, "error", err)
	}

	peersPath := filepath.Join(n.dataDir, folderID, "peers.yaml")
	if err := savePeerStates(peersPath, fs.peers); err != nil {
		slog.Warn("failed to save peer states", "folder", folderID, "error", err)
	}
}

// protoToFileIndex converts a protobuf IndexExchange to our internal FileIndex.
func protoToFileIndex(idx *pb.IndexExchange) *FileIndex {
	fi := &FileIndex{
		Sequence: idx.GetSequence(),
		Files:    make(map[string]FileEntry, len(idx.GetFiles())),
	}
	for _, f := range idx.GetFiles() {
		fi.Files[f.GetPath()] = FileEntry{
			Size:     f.GetSize(),
			MtimeNS:  f.GetMtimeNs(),
			SHA256:   bytesToHex(f.GetSha256()),
			Deleted:  f.GetDeleted(),
			Sequence: f.GetSequence(),
		}
	}
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

// countActions counts entries with the given action type.
func countActions(actions []DiffEntry, action DiffAction) int {
	count := 0
	for _, a := range actions {
		if a.Action == action {
			count++
		}
	}
	return count
}

// generateDeviceID creates a random 16-character hex device identifier.
func generateDeviceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hexToBytes(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func bytesToHex(b []byte) string {
	return hex.EncodeToString(b)
}
