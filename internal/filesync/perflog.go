package filesync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	perfLogFile    = "perf.jsonl"
	perfMaxSize    = 10 * 1024 * 1024 // 10 MB — rotate when exceeded
	perfMaxBackups = 3                // keep perf.1.jsonl .. perf.3.jsonl
)

// perfLogger writes structured JSONL performance events to ~/.mesh/perf.jsonl.
// Thread-safe, append-only, auto-rotating by size.
type perfLogger struct {
	mu   sync.Mutex
	f    *os.File
	path string
	size int64
}

var globalPerfLog struct {
	once   sync.Once
	logger *perfLogger
}

// initPerfLog initializes the global perf logger. Called once from Start().
func initPerfLog(meshDir string) {
	globalPerfLog.once.Do(func() {
		path := filepath.Join(meshDir, perfLogFile)
		pl := &perfLogger{path: path}
		if err := pl.open(); err != nil {
			// Non-fatal: perf logging is best-effort.
			return
		}
		globalPerfLog.logger = pl
	})
}

// closePerfLog closes the global perf logger.
func closePerfLog() {
	if pl := globalPerfLog.logger; pl != nil {
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
	if pl == nil || pl.f == nil {
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
	for i := perfMaxBackups; i >= 1; i-- {
		from := pl.backupPath(i - 1)
		to := pl.backupPath(i)
		if i == perfMaxBackups {
			_ = os.Remove(to)
		}
		if from == pl.path {
			_ = os.Rename(from, to)
		} else {
			_ = os.Rename(from, to)
		}
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
	if pl := globalPerfLog.logger; pl != nil {
		pl.emit(event)
	}
}

// perfScan emits a scan event.
func perfScan(folder string, stats ScanStats, activeFiles, dirs int, changed bool, snapMs, purgeMs, swapMs float64) {
	perfEmit(map[string]any{
		"event":           "scan",
		"folder":          folder,
		"walk_ms":         ms(stats.WalkDuration),
		"hash_ms":         ms(stats.HashDuration),
		"stat_ms":         ms(stats.StatDuration),
		"ignore_ms":       ms(stats.IgnoreDuration),
		"deletion_ms":     ms(stats.DeletionScan),
		"snapshot_ms":     snapMs,
		"purge_ms":        purgeMs,
		"swap_ms":         swapMs,
		"entries_visited": stats.EntriesVisited,
		"dirs":            dirs,
		"active_files":    activeFiles,
		"files_hashed":    stats.FilesHashed,
		"bytes_hashed":    stats.BytesHashed,
		"fast_path_hits":  stats.FastPathHits,
		"deletions":       stats.Deletions,
		"stat_errors":     stats.StatErrors,
		"hash_errors":     stats.HashErrors,
		"toctou_skips":    stats.TocTouSkips,
		"changed":         changed,
	})
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

// perfSync emits a sync event.
func perfSync(folder, peer string, remoteEntries, downloads, conflicts, deletes, failed int, durationMs float64) {
	perfEmit(map[string]any{
		"event":          "sync",
		"folder":         folder,
		"peer":           peer,
		"remote_entries": remoteEntries,
		"downloads":      downloads,
		"conflicts":      conflicts,
		"deletes":        deletes,
		"failed":         failed,
		"duration_ms":    durationMs,
	})
}

// perfSnapshot emits a periodic process-level snapshot.
func perfSnapshot(folders map[string]*folderState) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	folderStats := make([]map[string]any, 0, len(folders))
	for id, fs := range folders {
		fs.indexMu.RLock()
		count, size := fs.index.activeCountAndSize()
		seq := fs.index.Sequence
		mapLen := len(fs.index.Files)
		fs.indexMu.RUnlock()
		folderStats = append(folderStats, map[string]any{
			"id":          id,
			"active":      count,
			"total_bytes": size,
			"sequence":    seq,
			"map_entries": mapLen,
		})
	}

	perfEmit(map[string]any{
		"event":       "snapshot",
		"goroutines":  runtime.NumGoroutine(),
		"heap_mb":     memStats.HeapAlloc / (1024 * 1024),
		"sys_mb":      memStats.Sys / (1024 * 1024),
		"gc_pause_us": memStats.PauseNs[(memStats.NumGC+255)%256] / 1000,
		"open_fds":    countOpenFDs(),
		"folders":     folderStats,
	})
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
