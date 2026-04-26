# Deep lifecycle audit — 2026-04-26

Scope: filesync lifecycle, lock ordering, goroutine ownership,
resource cleanup, shutdown drain. Sequel to the crash-resilience
pass; looks for slow-burn data races and lock-induced wedges.

## Critical

### C1 — Data race on `folderState.writerCtx`

**Location:** `internal/filesync/filesync.go` ·
`persistAllCtx` (writer) vs `syncFolder` download / delete commit
callbacks (readers)

`persistAllCtx` swaps `fs.writerCtx` under `indexMu.Lock()`. The
download / delete commit callbacks (inside the per-action
goroutines spawned by `syncFolder`) read `fs.writerCtx` bare —
no lock held — and pass it to `saveIndex`. This is an
unsynchronised concurrent read+write on the same address. Go's
race detector panics on first detection; on arm64 the memory
model gives no coherence guarantees without synchronisation.
Trigger: any shutdown of a node that has at least one download
or delete in flight when `<-ctx.Done()` fires — every restart
of an active daemon.

Functional consequence beyond the race: goroutines that read the
swapped `shutdownCtx` (a 10 s deadline that started before the
goroutines finish) see `saveIndex` fail with
`context.DeadlineExceeded`. The file is on disk (rename
completed) but its SQLite row is never committed; on next
startup the file is re-downloaded.

**Fix.** Stop swapping `fs.writerCtx`. Add a `ctx context.Context`
parameter to `persistFolder` and pass `shutdownCtx` explicitly
from `persistAllCtx`. Normal calls pass `fs.writerCtx`.

**Confidence: 5/5.**

### C2 — `closeOneFolder` nullifies handles while in-flight goroutines use them

**Location:** `internal/filesync/filesync.go` ·
`closeOneFolder` vs `syncFolder` ActionDownload / ActionDelete
commit callbacks

`closeOneFolder` does direct `fs.db = nil`, `fs.dbReader = nil`,
`fs.root = nil` without waiting for in-flight per-action
goroutines. A goroutine past the concurrency semaphore is
actively using `fs.db` (in `saveIndex`) and `fs.root` (in
`installDownloadedFile`). Using a closed `*sql.DB` is undefined
behaviour in `database/sql`; using a closed `*os.Root` returns
`os.ErrClosed` at best, panics at worst. Trigger: any `/reopen`
or `/restore` admin call while a sync cycle is past the
semaphore.

**Fix.** Add a per-folder `sync.WaitGroup` (`actionWG`).
ActionDownload / ActionConflict / ActionDelete goroutines
`actionWG.Add(1)` on entry (after the semaphore) and
`actionWG.Done()` on exit. `closeOneFolder` calls
`writerCancel()` then `actionWG.Wait()` before closing handles.

**Confidence: 5/5.**

### C3 — `ReopenFolder` reads `n.folders` map without `foldersMu`

**Location:** `internal/filesync/filesync.go` · `ReopenFolder`

`ReopenFolder` does `if _, ok := n.folders[folderID]; ok` inside
an `activeNodes.ForEach` block. `ForEach` holds the registry's
RLock, not `foldersMu`. `closeOneFolder` deletes from the same
map under `foldersMu.Lock`. Concurrent map read+write panic.
The first `/reopen` or `/restore` hit while a scan or sync cycle
is iterating `n.folders` via `folderEntries()` — or while a
concurrent reopen on a different folder is running
`closeOneFolder` — fires the runtime panic.

**Fix.** Replace the bare lookup with `n.findFolder(folderID)`.
One-line change.

**Confidence: 5/5.**

## High

### H1 — `runBackupSweep` holds `reopenLockMu` for the full SQLite backup

**Location:** `internal/filesync/filesync.go` · `runBackupSweep`

Acquires `reopenLockMu.Lock()` and iterates folders calling
`writeBackup`. `writeBackup` is a full SQLite copy
(`VACUUM INTO`) — 30–120 s on a 500 MB index. `reopenFolder` and
`restoreFromBackup` also take `reopenLockMu`. An operator
hitting `/reopen` after a `FolderDisabled` alert during a backup
window blocks silently with no progress indication. On a
multi-week daemon the backup tick and an alert will eventually
coincide.

**Fix.** Snapshot the per-folder `db` handle under
`foldersMu.RLock`, release `reopenLockMu`, then run `writeBackup`
without any global lock. If a concurrent reopen closes the
original handle, `writeBackup`'s query fails with
`sql: database is closed`; log and continue — next cycle
succeeds.

**Confidence: 4/5.**

### H2 — `persistAllCtx` runs before `wg.Wait()`; concurrent with shutdown drain

**Location:** `internal/filesync/filesync.go` · `Start` shutdown
path

The shutdown sequence calls `persistAllCtx(shutdownCtx)` and
then `wg.Wait()`. The node-level `wg` tracks top-level
goroutines (`scanLoop`, `syncLoop`, etc.), not per-action
goroutines spawned inside `syncAllPeers`. The window between
`persistAllCtx` running and `wg.Wait()` returning has live
download / delete goroutines that may commit new rows after
`persistAllCtx` finishes — those rows are then never persisted.
Subordinate to C1; fixing C1 mostly closes this.

**Fix.** Move `persistAllCtx` to after `wg.Wait()`. Goroutines
in `wg` observe ctx cancellation and drain; the post-drain
persist is uncontested.

**Confidence: 4/5.**

## Medium

### M1 — `buildIndexExchange` uses `context.Background()` for peer-facing SQLite reads

**Location:** `internal/filesync/filesync.go` · `buildIndexExchange`

Calls `queryFilesSinceSeq(context.Background(), ...)`. A 500 k
row exchange takes several seconds. During shutdown,
`httpSrv.Shutdown` waits 5 s. An exchange in the last few
seconds before shutdown holds the connection open beyond the
budget; peer sees a torn response.

**Fix.** Thread the HTTP request context (`r.Context()`)
through `buildIndexExchange` to `queryFilesSinceSeq`. Bounds the
query to the remaining shutdown window.

**Confidence: 4/5.**

### M2 — Subset of C2: goroutines past semaphore use closed handles

**Location:** `internal/filesync/filesync.go` · `closeOneFolder`,
download / delete goroutines

The goroutine-lifetime view of C2. After `writerCancel()`,
goroutines past the semaphore continue execution. `saveIndex`
fails with `context.Canceled`; by then `fs.db` may be closed.
Use of a closed handle is UB.

**Fix.** Addressed by C2's `actionWG` drain.

**Confidence: 4/5.**

## Summary

| Severity | Count |
| -------- | ----- |
| Critical | 3     |
| High     | 2     |
| Medium   | 2     |

**Top hazard for a 24 h+ daemon: C3.** First `/reopen` or
`/restore` hit while a scan or sync iterates `n.folders`
panics the runtime. One-line fix.
