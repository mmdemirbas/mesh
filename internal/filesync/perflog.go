package filesync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	perfMaxSize    = 10 * 1024 * 1024 // 10 MB — rotate when exceeded
	perfMaxBackups = 3                // keep <node>-perf.1.jsonl .. <node>-perf.3.jsonl
)

// perfLogger writes structured JSONL performance events to ~/.mesh/perf.jsonl.
// Thread-safe, append-only, auto-rotating by size.
type perfLogger struct {
	mu   sync.Mutex
	f    *os.File
	path string
	size int64
}

// logger is stored via atomic.Pointer so parallel tests can swap it
// under -race without tripping the detector. In production it is
// written once from initPerfLog and never reassigned.
var globalPerfLog struct {
	once   sync.Once
	logger atomic.Pointer[perfLogger]
}

// initPerfLog initializes the global perf logger. Called once from Start().
// Path: ~/.mesh/log/<nodeName>-perf.jsonl
func initPerfLog(meshDir, nodeName string) {
	globalPerfLog.once.Do(func() {
		logDir := filepath.Join(meshDir, "log")
		path := filepath.Join(logDir, nodeName+"-perf.jsonl")
		pl := &perfLogger{path: path}
		if err := pl.open(); err != nil {
			// Non-fatal: perf logging is best-effort.
			return
		}
		globalPerfLog.logger.Store(pl)
	})
}

// closePerfLog closes the global perf logger.
func closePerfLog() {
	if pl := globalPerfLog.logger.Load(); pl != nil {
		pl.mu.Lock()
		defer pl.mu.Unlock()
		if pl.f != nil {
			_ = pl.f.Close()
			pl.f = nil
		}
	}
}

func (pl *perfLogger) open() error {
	dir := filepath.Dir(pl.path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	f, err := os.OpenFile(pl.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, _ := f.Stat()
	pl.f = f
	if info != nil {
		pl.size = info.Size()
	}
	return nil
}

func (pl *perfLogger) emit(event map[string]any) {
	if pl == nil {
		return
	}
	event["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')

	pl.mu.Lock()
	defer pl.mu.Unlock()

	if pl.f == nil {
		return
	}
	n, _ := pl.f.Write(data)
	pl.size += int64(n)

	if pl.size >= perfMaxSize {
		pl.rotate()
	}
}

func (pl *perfLogger) rotate() {
	_ = pl.f.Close()
	pl.f = nil

	// Shift backups: perf.3.jsonl → delete, perf.2 → perf.3, etc.
	// F11: unconditional Remove before Rename so Windows doesn't fail
	// when the target already exists.
	for i := perfMaxBackups; i >= 1; i-- {
		from := pl.backupPath(i - 1)
		to := pl.backupPath(i)
		_ = os.Remove(to)
		_ = os.Rename(from, to)
	}

	// Re-open fresh.
	_ = pl.open()
}

func (pl *perfLogger) backupPath(n int) string {
	if n == 0 {
		return pl.path
	}
	ext := filepath.Ext(pl.path)
	base := pl.path[:len(pl.path)-len(ext)]
	return fmt.Sprintf("%s.%d%s", base, n, ext)
}

// --- Emit helpers ---

func perfEmit(event map[string]any) {
	if pl := globalPerfLog.logger.Load(); pl != nil {
		pl.emit(event)
	}
}

// perfScan emits a scan event. memDelta captures heap and alloc growth
// over the scan so profile regressions attribute to a specific scan
// rather than surfacing only in the periodic snapshot.
func perfScan(folder string, stats ScanStats, activeFiles, dirs int, changed bool, snapMs, purgeMs, swapMs float64, memDelta MemDelta) {
	perfEmit(map[string]any{
		"event":            "scan",
		"folder":           folder,
		"walk_ms":          ms(stats.WalkDuration),
		"hash_ms":          ms(stats.HashDuration),
		"stat_ms":          ms(stats.StatDuration),
		"ignore_ms":        ms(stats.IgnoreDuration),
		"deletion_ms":      ms(stats.DeletionScan),
		"snapshot_ms":      snapMs,
		"purge_ms":         purgeMs,
		"swap_ms":          swapMs,
		"entries_visited":  stats.EntriesVisited,
		"dirs":             dirs,
		"dirs_walked":      stats.DirsWalked,
		"dirs_ignored":     stats.DirsIgnored,
		"files_ignored":    stats.FilesIgnored,
		"symlinks_skipped": stats.SymlinksSkipped,
		"temp_cleaned":     stats.TempCleaned,
		"renames_detected": stats.RenamesDetected,
		"active_files":     activeFiles,
		"files_hashed":     stats.FilesHashed,
		"bytes_hashed":     stats.BytesHashed,
		"fast_path_hits":   stats.FastPathHits,
		"deletions":        stats.Deletions,
		"stat_errors":      stats.StatErrors,
		"hash_errors":      stats.HashErrors,
		"toctou_skips":     stats.TocTouSkips,
		"heap_delta_bytes": memDelta.HeapDeltaBytes,
		"alloc_delta":      memDelta.AllocsDelta,
		"changed":          changed,
	})
}

// MemDelta captures runtime memory growth over a measured interval.
// Heap delta may be negative when GC runs during the window.
type MemDelta struct {
	HeapDeltaBytes int64
	AllocsDelta    uint64 // cumulative allocation count delta
}

// captureMemDelta reads MemStats at scope entry. Pair with .Close(after).
// Use the returned `after` call to compute the delta at scope exit.
func captureMemStats() (runtime.MemStats, func() MemDelta) {
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	return before, func() MemDelta {
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		return MemDelta{
			HeapDeltaBytes: int64(after.HeapAlloc) - int64(before.HeapAlloc),
			AllocsDelta:    after.Mallocs - before.Mallocs,
		}
	}
}

// perfPersist emits a persist event.
func perfPersist(folder string, indexBytes int, indexMs, peersMs float64, skippedIndex bool) {
	perfEmit(map[string]any{
		"event":         "persist",
		"folder":        folder,
		"index_bytes":   indexBytes,
		"index_ms":      indexMs,
		"peers_ms":      peersMs,
		"skipped_index": skippedIndex,
	})
}

// SyncPerfSummary captures byte-level telemetry for one sync cycle so
// the JSONL can reason about throughput, rename savings, and delta
// efficiency without re-deriving counts from individual events.
type SyncPerfSummary struct {
	RemoteEntries   int
	Downloads       int
	Conflicts       int
	Deletes         int
	Failed          int
	Renames         int   // R1 resolutions (no bytes on wire)
	BytesPlanned    int64 // sum of RemoteSize across planned actions
	BytesDownloaded int64 // actual bytes read from peers this cycle
	BytesSavedRname int64 // bytes avoided via local rename
	DurationMs      float64
	IndexFetchMs    float64 // time spent fetching/decoding peer index
	FirstFailReason string
}

// perfSyncEvent builds the event map for a sync event. Split out so
// tests can assert on the field shape without touching the global logger.
func perfSyncEvent(folder, peer string, s SyncPerfSummary) map[string]any {
	m := map[string]any{
		"event":             "sync",
		"folder":            folder,
		"peer":              peer,
		"remote_entries":    s.RemoteEntries,
		"downloads":         s.Downloads,
		"conflicts":         s.Conflicts,
		"deletes":           s.Deletes,
		"failed":            s.Failed,
		"renames":           s.Renames,
		"bytes_planned":     s.BytesPlanned,
		"bytes_downloaded":  s.BytesDownloaded,
		"bytes_saved_rname": s.BytesSavedRname,
		"index_fetch_ms":    s.IndexFetchMs,
		"duration_ms":       s.DurationMs,
	}
	if s.FirstFailReason != "" {
		m["failure_reason"] = s.FirstFailReason
	}
	return m
}

// perfSync emits a sync event.
func perfSync(folder, peer string, s SyncPerfSummary) {
	perfEmit(perfSyncEvent(folder, peer, s))
}

// DownloadPerfSummary captures per-file transfer telemetry. Mode is
// "whole", "delta", or "resume". FirstByteMs is the TTFB for the primary
// HTTP response; TotalMs covers write+fsync+rename.
type DownloadPerfSummary struct {
	Folder       string
	Peer         string
	SizeBytes    int64
	BytesOnWire  int64 // bytes read from the response body (post-gzip)
	BytesReused  int64 // delta mode: bytes taken from local copy
	ChunksTotal  int
	ChunksReused int // delta mode: chunks resolved from local hashes
	Mode         string
	Resumed      bool
	Retries      int
	FirstByteMs  float64
	TotalMs      float64
	Error        string // empty on success
}

// perfDownloadEvent builds the download event map.
func perfDownloadEvent(s DownloadPerfSummary) map[string]any {
	m := map[string]any{
		"event":         "download",
		"folder":        s.Folder,
		"peer":          s.Peer,
		"size_bytes":    s.SizeBytes,
		"bytes_on_wire": s.BytesOnWire,
		"bytes_reused":  s.BytesReused,
		"chunks_total":  s.ChunksTotal,
		"chunks_reused": s.ChunksReused,
		"mode":          s.Mode,
		"resumed":       s.Resumed,
		"retries":       s.Retries,
		"first_byte_ms": s.FirstByteMs,
		"total_ms":      s.TotalMs,
	}
	if s.Error != "" {
		m["error"] = s.Error
	}
	return m
}

// perfDownload emits a per-file download event.
func perfDownload(s DownloadPerfSummary) {
	perfEmit(perfDownloadEvent(s))
}

// perfSnapshot emits a periodic process-level snapshot.
// perfSnapshotEvent builds the snapshot event map. Split out so tests can
// assert on field shape without touching the global logger.
func perfSnapshotEvent(folders map[string]*folderState) map[string]any {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	folderStats := make([]map[string]any, 0, len(folders))
	for id, fs := range folders {
		fs.indexMu.RLock()
		count, size := fs.index.activeCountAndSize()
		seq := fs.index.Sequence
		mapLen := len(fs.index.Files)
		peerCount := len(fs.peers)
		pendingCount := len(fs.pending)
		fs.indexMu.RUnlock()
		// Reading cumulative atomics requires no lock; they are
		// monotonic so a concurrent sync only raises them.
		folderStats = append(folderStats, map[string]any{
			"id":                id,
			"active":            count,
			"total_bytes":       size,
			"sequence":          seq,
			"map_entries":       mapLen,
			"peer_count":        peerCount,
			"pending_syncs":     pendingCount,
			"bytes_downloaded":  fs.metrics.BytesDownloaded.Load(),
			"bytes_uploaded":    fs.metrics.BytesUploaded.Load(),
			"bytes_saved_rname": fs.metrics.BytesSavedByRename.Load(),
			"files_downloaded":  fs.metrics.FilesDownloaded.Load(),
			"files_renamed":     fs.metrics.FilesRenamed.Load(),
			"files_conflicted":  fs.metrics.FilesConflicted.Load(),
			"files_deleted":     fs.metrics.FilesDeleted.Load(),
			"peer_syncs":        fs.metrics.PeerSyncs.Load(),
			"sync_errors":       fs.metrics.SyncErrors.Load(),
			"index_exchanges":   fs.metrics.IndexExchanges.Load(),
			"scan_count":        fs.metrics.ScanCount.Load(),
			"last_scan_ms":      float64(fs.metrics.ScanDurationNS.Load()) / 1e6,
			"last_sync_ms":      float64(fs.metrics.PeerSyncNS.Load()) / 1e6,
		})
	}

	return map[string]any{
		"event":       "snapshot",
		"goroutines":  runtime.NumGoroutine(),
		"heap_mb":     memStats.HeapAlloc / (1024 * 1024),
		"sys_mb":      memStats.Sys / (1024 * 1024),
		"gc_pause_us": memStats.PauseNs[(memStats.NumGC+255)%256] / 1000,
		"open_fds":    countOpenFDs(),
		"folders":     folderStats,
	}
}

func perfSnapshot(folders map[string]*folderState) {
	perfEmit(perfSnapshotEvent(folders))
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// countOpenFDs returns the open FD count or -1 on unsupported platforms.
func countOpenFDs() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		// /dev/fd not available (Windows).
		if f, err2 := os.Open("/proc/self/fd"); err2 == nil {
			defer f.Close()
			names, _ := f.Readdirnames(-1)
			return len(names)
		}
		return -1
	}
	return len(entries)
}
