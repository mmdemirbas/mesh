# filesync Deep Review — 2026-04-26

Scope: `internal/filesync/` (focused on `filesync.go`, `index.go`,
`index_sqlite.go`, plus tests).
Reviewed for: correctness / data integrity, concurrency, security,
resource handling, error handling.
Pre-pinned changes (D4 SQLite cutover, H8 local-wins conflict-bytes
preservation) excluded.

## Critical

### C1 — `persistFolder` clears more dirty entries than it persisted

**Location:** `internal/filesync/filesync.go` · `persistFolder`

`persistFolder` (1) sets `fs.indexDirty = false` under `Lock`,
(2) snapshots `idx.DirtyPaths()` under `RLock`, (3) commits to
SQLite without any lock, (4) calls `fs.index.ClearDirty()` under
`Lock` on success.

`ClearDirty()` zeroes the entire dirty map. Between step 2 and
step 4, the scan swap path (under `indexMu.Lock()`) calls
`fs.index.Set()` and adds new paths to `fs.index.dirty`. Those
paths were not part of the snapshot, so they were not written to
SQLite. `ClearDirty()` silently removes their dirty markers anyway.

Failure path:

1. `persistFolder` snapshots dirty `{A, B}`, releases the RLock.
2. The scan swap (under `indexMu.Lock()`) calls
   `fs.index.Set("C", ...)`, marking C dirty, sets
   `fs.indexDirty = true`.
3. `persistFolder` commits `{A, B}`, calls `ClearDirty()` —
   wipes `{A, B, C}`.
4. C is missing from SQLite. Next `persistFolder` run sees
   `fs.indexDirty = true` but `fs.index.dirty` is empty — nothing
   is written.
5. SQLite permanently diverges from the in-memory index for C.
   Survives process restart. Peers never see C through
   `buildIndexExchange` (it reads SQLite). Silent data hole.

**Fix.** Drop the blanket `ClearDirty()`. After the commit
succeeds, delete only the snapshotted paths from the dirty and
deleted sets:

```go
fs.indexMu.Lock()
for p := range dirty {
    delete(fs.index.dirty, p)
}
for p := range deleted {
    delete(fs.index.deleted, p)
}
fs.indexMu.Unlock()
```

Add `ClearSpecific(dirty, deleted map[string]struct{})` to
`FileIndex` and remove `ClearDirty()` so the bug cannot reappear.

**Confidence: 95**

## High

### H1 — Unlocked read of `fs.peers` map in `syncFolder`

**Location:** `internal/filesync/filesync.go` · `syncFolder` peer
reset detection block

After `indexMu` is released earlier in `syncFolder`, the code
reads `fs.peers[peerAddr]` without any lock held in the peer
reset detection block. `fs.peers` is `map[string]PeerState`.
Concurrent writers exist:

- Other `syncFolder` goroutines (one per configured peer, started
  by `syncAllPeers`) each write to `fs.peers` under
  `indexMu.Lock()` at the end of their own sync cycle.
- The scan swap calls `markRemovedPeers` / `gcRemovedPeers`
  inside the swap block under `indexMu.Lock()`.

Any concurrent map write makes the unlocked read a runtime panic
("concurrent map read and map write").

**Fix.** Capture `currentLastEpoch` inside a short `RLock` block
immediately before `classifyPeerResetTrigger`, or fold it into the
existing `fs.indexMu.RLock()` block higher up where other peer-
state fields are already read.

**Confidence: 90**

## Medium

### M1 — `n.folders` map race between admin reads and reopen / restore

**Location:** `internal/filesync/filesync.go` · `GetFolderStatuses`,
`GetConflicts`, `closeOneFolder`

`GetFolderStatuses` and `GetConflicts` iterate `n.folders` inside
`activeNodes.ForEach`. `ForEach` holds `activeNodes.mu.RLock()`,
which protects only the `activeNodes.nodes` slice — not
`n.folders` inside each node. `closeOneFolder` (invoked by
`reopenFolder` and `restoreFromBackup`) calls
`delete(n.folders, folderID)` while holding the package-level
`reopenLockMu`. The status-read path never acquires `reopenLockMu`.
A concurrent reopen + status request races on the same map with
no shared synchronization.

**Fix.** Add `foldersMu sync.RWMutex` to `Node`. Acquire RLock in
`GetFolderStatuses`, `GetConflicts`, `GetFolderMetrics` when
iterating `n.folders`. Acquire Lock in `closeOneFolder`. Hot sync
and scan paths only read `n.folders` after startup so the lock
adds no steady-state overhead.

**Confidence: 85**

### M2 — CVE-2026-32282: `Root.Chmod` TOCTOU on Linux (Go 1.26.x)

**Location:** `internal/filesync/filesync.go` · download commit
callback, conflict remote-wins path, rename-plan path

Go 1.26.x carries CVE-2026-32282: on Linux, `os.Root.Chmod` is
vulnerable to a TOCTOU race. The Linux kernel's `fchmodat` syscall
ignores `AT_SYMLINK_NOFOLLOW`, which `os.Root.Chmod` depends on
for symlink confinement. If an attacker replaces the just-renamed
target with a symlink to a path outside the folder root between
the `Rename` and `Chmod` calls, `Chmod` changes permissions on an
arbitrary file reachable by the process. Three sync-path call
sites all follow `fs.root.Rename` with `fs.root.Chmod`, creating
the race window.

Impact: local permission escalation. Not content read or write.

**Fix.** Open the file by fd after the rename and call
`syscall.Fchmod(int(f.Fd()), mode)`, bypassing the path-based
race. Requires `_unix.go` / `_windows.go` split. Stopgap: skip
`Chmod` when `RemoteMode == 0` or `RemoteMode == 0o644` (the
default), covering most synced files. Track upstream
[golang/go#78293](https://go.dev/issue/78293) for the stdlib fix.

**Confidence: 80**

## Summary

| Severity | Count |
| -------- | ----- |
| Critical | 1     |
| High     | 1     |
| Medium   | 2     |

**Most load-bearing:** C1. `persistFolder`'s `ClearDirty()`
silently drops dirty entries added between the snapshot and the
commit, creating a permanent SQLite divergence that survives
restart and corrupts delta exchanges with no error signal. Fix
must replace `ClearDirty()` with a path-set-scoped `ClearSpecific()`
in every `persistFolder` commit branch.
