package filesync

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newTestPerfLogger returns a perfLogger rooted in t.TempDir() with the global
// singleton bypassed. Tests mutate its size threshold indirectly by writing
// large events; they do not touch globalPerfLog.
func newTestPerfLogger(t *testing.T) *perfLogger {
	t.Helper()
	dir := t.TempDir()
	pl := &perfLogger{path: filepath.Join(dir, "log", "n1-perf.jsonl")}
	if err := pl.open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		pl.mu.Lock()
		defer pl.mu.Unlock()
		if pl.f != nil {
			_ = pl.f.Close()
		}
	})
	return pl
}

// swapPerfLoggerForTest atomically installs pl as the global logger for
// the duration of the test, restoring the prior pointer on cleanup. pl
// may be nil (nil-global assertion tests). Using the atomic pointer API
// prevents -race flags when this test overlaps with another that calls
// perfEmit via the production code path (e.g. TestPersistFolder_Concurrent).
func swapPerfLoggerForTest(t *testing.T, pl *perfLogger) {
	t.Helper()
	saved := globalPerfLog.logger.Load()
	globalPerfLog.logger.Store(pl)
	t.Cleanup(func() { globalPerfLog.logger.Store(saved) })
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test fixture
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024*1024), 1024*1024)
	for s.Scan() {
		out = append(out, s.Text())
	}
	return out
}

func TestPerfLogger_OpenCreatesDirAndAppends(t *testing.T) {
	t.Parallel()
	pl := newTestPerfLogger(t)
	if _, err := os.Stat(filepath.Dir(pl.path)); err != nil {
		t.Fatalf("expected log dir: %v", err)
	}
	pl.emit(map[string]any{"event": "scan", "folder": "f1"})
	lines := readLines(t, pl.path)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event"] != "scan" || got["folder"] != "f1" {
		t.Errorf("payload mismatch: %+v", got)
	}
	if _, ok := got["ts"].(string); !ok {
		t.Errorf("ts missing/wrong type: %+v", got)
	}
}

func TestPerfLogger_BackupPath(t *testing.T) {
	t.Parallel()
	pl := &perfLogger{path: "/var/log/node-perf.jsonl"}
	cases := []struct {
		n    int
		want string
	}{
		{0, "/var/log/node-perf.jsonl"},
		{1, "/var/log/node-perf.1.jsonl"},
		{2, "/var/log/node-perf.2.jsonl"},
		{3, "/var/log/node-perf.3.jsonl"},
	}
	for _, c := range cases {
		if got := pl.backupPath(c.n); got != c.want {
			t.Errorf("backupPath(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestPerfLogger_BackupPath_NoExt(t *testing.T) {
	t.Parallel()
	pl := &perfLogger{path: "/var/log/noext"}
	if got := pl.backupPath(2); got != "/var/log/noext.2" {
		t.Errorf("backupPath no-ext = %q", got)
	}
}

func TestPerfLogger_Rotate_ShiftsBackups(t *testing.T) {
	t.Parallel()
	pl := newTestPerfLogger(t)
	dir := filepath.Dir(pl.path)
	base := strings.TrimSuffix(filepath.Base(pl.path), ".jsonl")

	// Seed the live file and two existing backups so we can watch them shift.
	pl.emit(map[string]any{"event": "live"})
	// Pre-create backup 1 and 2 by writing content directly.
	if err := os.WriteFile(filepath.Join(dir, base+".1.jsonl"), []byte(`{"event":"b1"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".2.jsonl"), []byte(`{"event":"b2"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Also a stale backup 3 to verify it gets removed (not renamed further).
	if err := os.WriteFile(filepath.Join(dir, base+".3.jsonl"), []byte(`{"event":"stale"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	pl.mu.Lock()
	pl.rotate()
	pl.mu.Unlock()

	// After rotate: live → .1, old .1 → .2, old .2 → .3, old .3 removed.
	b1 := readLines(t, filepath.Join(dir, base+".1.jsonl"))
	if len(b1) != 1 || !strings.Contains(b1[0], `"event":"live"`) {
		t.Errorf(".1 should hold old live, got %v", b1)
	}
	b2 := readLines(t, filepath.Join(dir, base+".2.jsonl"))
	if len(b2) != 1 || !strings.Contains(b2[0], `"event":"b1"`) {
		t.Errorf(".2 should hold old .1, got %v", b2)
	}
	b3 := readLines(t, filepath.Join(dir, base+".3.jsonl"))
	if len(b3) != 1 || !strings.Contains(b3[0], `"event":"b2"`) {
		t.Errorf(".3 should hold old .2 (stale .3 overwritten), got %v", b3)
	}
	// Live file was re-opened empty.
	if lines := readLines(t, pl.path); len(lines) != 0 {
		t.Errorf("live after rotate should be empty, got %v", lines)
	}
}

func TestPerfLogger_EmitSizeThresholdTriggersRotate(t *testing.T) {
	// Not parallel: we reuse the default 10 MB threshold by building a
	// fresh logger with a small file pre-populated to near threshold.
	pl := newTestPerfLogger(t)

	// Force size to just under threshold so the next emit crosses it.
	pl.mu.Lock()
	pl.size = perfMaxSize - 10
	pl.mu.Unlock()

	pl.emit(map[string]any{"event": "trigger"})

	dir := filepath.Dir(pl.path)
	base := strings.TrimSuffix(filepath.Base(pl.path), ".jsonl")
	if _, err := os.Stat(filepath.Join(dir, base+".1.jsonl")); err != nil {
		t.Fatalf("rotate did not create .1 backup: %v", err)
	}
	// New live file starts fresh (size reset, file empty or with only the
	// triggering event written pre-rotate). After rotate, the triggering
	// event sits in the .1 backup.
	if pl.size != 0 {
		t.Errorf("size after rotate = %d, want 0", pl.size)
	}
}

func TestPerfLogger_EmitNilSafe(t *testing.T) {
	t.Parallel()
	var pl *perfLogger
	pl.emit(map[string]any{"x": 1}) // should not panic
}

func TestPerfLogger_EmitAfterCloseIsNoop(t *testing.T) {
	t.Parallel()
	pl := newTestPerfLogger(t)
	pl.mu.Lock()
	_ = pl.f.Close()
	pl.f = nil
	pl.mu.Unlock()
	pl.emit(map[string]any{"event": "dropped"})
	// File may still exist from open() but must contain no lines.
	if lines := readLines(t, pl.path); len(lines) != 0 {
		t.Errorf("emit after close wrote %d lines", len(lines))
	}
}

func TestPerfLogger_ConcurrentEmit(t *testing.T) {
	t.Parallel()
	pl := newTestPerfLogger(t)
	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			for j := range perWriter {
				pl.emit(map[string]any{"event": "p", "writer": i, "seq": j})
			}
		}()
	}
	wg.Wait()

	lines := readLines(t, pl.path)
	if len(lines) != writers*perWriter {
		t.Fatalf("lines = %d, want %d", len(lines), writers*perWriter)
	}
	// Each line must be valid JSON — no torn writes.
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d not JSON: %v\n%s", i, err, line)
		}
	}
}

func TestPerfEmit_NilGlobalIsNoop(t *testing.T) {
	t.Parallel()
	// Sanity: when the global is nil, perfEmit is a silent no-op.
	swapPerfLoggerForTest(t, nil)
	perfEmit(map[string]any{"event": "nope"})
}

func TestCountOpenFDs(t *testing.T) {
	t.Parallel()
	// -1 means unsupported platform (Windows). On Unix it must be ≥ 1
	// because at minimum stdin/out/err are open.
	n := countOpenFDs()
	if n != -1 && n < 1 {
		t.Errorf("countOpenFDs = %d, want -1 or ≥1", n)
	}
}

func TestClosePerfLog_NilSafe(t *testing.T) {
	t.Parallel()
	// With logger == nil the close path is a silent no-op (mirrors the
	// production invariant that perf logging is best-effort).
	swapPerfLoggerForTest(t, nil)
	closePerfLog()
}

func TestPerfSync_EmitsExpectedFields(t *testing.T) {
	t.Parallel()
	// Assert on perfSyncEvent directly — the wrapper perfSync just calls
	// perfEmit on this map, and asserting via the emit path is racy with
	// other tests that touch the global logger (e.g. TestPersistFolder_Concurrent).
	got := perfSyncEvent("f1", "peer-a", 10, 3, 1, 0, 2, 42.5, "boom")
	for k, want := range map[string]any{
		"event":          "sync",
		"folder":         "f1",
		"peer":           "peer-a",
		"remote_entries": 10,
		"downloads":      3,
		"conflicts":      1,
		"deletes":        0,
		"failed":         2,
		"duration_ms":    42.5,
		"failure_reason": "boom",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestPerfSync_OmitsEmptyFailureReason(t *testing.T) {
	t.Parallel()
	got := perfSyncEvent("f1", "p", 0, 0, 0, 0, 0, 0, "")
	if _, has := got["failure_reason"]; has {
		t.Errorf("failure_reason should be omitted when empty: %+v", got)
	}
}

func TestPerfSnapshot_EmitsFolderStats(t *testing.T) {
	t.Parallel()
	idx := newFileIndex()
	idx.Sequence = 7
	idx.setEntry("a", FileEntry{Size: 100})
	idx.setEntry("b", FileEntry{Size: 250})
	folders := map[string]*folderState{
		"fid": {index: idx},
	}
	got := perfSnapshotEvent(folders)
	if got["event"] != "snapshot" {
		t.Errorf("event = %v", got["event"])
	}
	fs, ok := got["folders"].([]map[string]any)
	if !ok || len(fs) != 1 {
		t.Fatalf("folders payload = %+v", got["folders"])
	}
	f0 := fs[0]
	if f0["id"] != "fid" {
		t.Errorf("folder id = %v", f0["id"])
	}
	if f0["sequence"] != int64(7) {
		t.Errorf("folder sequence = %v (%T)", f0["sequence"], f0["sequence"])
	}
}

func TestMsHelper(t *testing.T) {
	t.Parallel()
	// 1500 microseconds → 1.5 ms
	if got := ms(1500 * 1000); got != 1.5 {
		t.Errorf("ms(1.5ms) = %v, want 1.5", got)
	}
	if got := ms(0); got != 0 {
		t.Errorf("ms(0) = %v, want 0", got)
	}
}
