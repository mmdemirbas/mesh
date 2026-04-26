# Deep filesync logic audit — 2026-04-26

Focus: silent data-correctness bugs, sync stalls, divergent state.
Scope: `internal/filesync/`. Driven by the user's "go deeper" request
after the crash-resilience pass.

## Critical

### C1 — Remote-wins conflict path missing crash-safe SQLite commit

**Location:** `internal/filesync/filesync.go` ·
`syncFolder` ActionConflict goroutine, remote-wins branch

`ActionDownload` uses `installDownloadedFile` + a `commit()` callback
that calls `saveIndex` inside the goroutine, before `fs.index.Set`.
The F7 `.bak` lifecycle ensures a SIGKILL anywhere in the sequence
leaves either the old or the new state in SQLite and on disk, with
the `.bak` sweep at next startup resolving any gap.

The remote-wins branch of `ActionConflict` does NOT use that pattern.
After `renameReplaceRoot(fs.root, tmpRelPath, action.Path)` succeeds,
the goroutine calls `fs.index.Set` (marks the path dirty) but issues
no `saveIndex`. Persistence is deferred to the `persistFolder` call
at the end of `syncFolder`, after `wg.Wait()`.

A SIGKILL between the final rename and that `persistFolder` leaves:

- **Disk:** remote content (rename completed)
- **SQLite:** pre-conflict row (old hash, old clock, old sequence)

On next startup `loadIndexDB` restores the old row. The scan re-
hashes the on-disk file, detects content change, bumps `selfID`,
assigns a new sequence, and sends it outbound as a local write. The
remote peer receives an entry with remote content but a local-origin
clock — the conflict bookkeeping is permanently wrong for the path.

**Fix.** Use `installDownloadedFile` for remote-wins conflicts the
same way `ActionDownload` does: move `Sequence++`, `fs.index.Set`,
and `saveIndex` into a `commit()` callback; pass it to
`installDownloadedFile`. The manual rename / Chtimes / Chmod / verify
should move inside the callback or integrate with the
`installDownloadedFile` metadata-fixup contract.

**Confidence: 5/5.**

### C2 — Local-wins R11 clock update not atomically persisted; conflict replay on crash

**Location:** `internal/filesync/filesync.go` ·
`syncFolder` ActionConflict goroutine, local-wins branch (R11)

R11 merges the remote clock into the local entry and bumps `selfID`
so the local clock dominates the remote's concurrent clock — the
mechanism that prevents the same conflict from re-firing every
cycle. The bump is written only to in-memory state via
`fs.index.Set`, with no `saveIndex` call. Persistence is deferred
to `persistFolder`.

A SIGKILL after R11's `Set` reverts the clock on restart. On the
next sync the peer still presents its concurrent clock,
`compareClocks` returns `ClockConcurrent` again, another
`.sync-conflict-*` file is created. The loop terminates only when a
later scan catches the on-disk content and bumps the clock through a
different code path.

**Fix.** Call `saveIndex` on a single-path snapshot before releasing
`indexMu`, mirroring the `ActionDelete commit()` pattern. SQLite must
reflect the updated clock before the goroutine exits.

**Confidence: 4/5.**

## Important

### I1 — `diff()` first-sync download leg is dead; all first-sync differing files become conflicts

**Location:** `internal/filesync/index.go` · `diff`, C1 mtime fallback

The branch for "no ancestor hash, no vector clocks, `lastSyncNS==0`"
gates the download path on `lEntry.MtimeNS <= lastSyncNS`, where
`lastSyncNS` is 0. On any real filesystem `MtimeNS` is a Unix
nanosecond timestamp — always > 0. The condition is never true.
Every differing file on a true first sync takes `conflictEntry`,
never `downloadEntry`.

A new peer joining a cluster with thousands of pre-existing files
generates thousands of `.sync-conflict-*` files instead of cleanly
adopting remote content. The code comment acknowledges the
behaviour ("overwhelmingly produces conflictEntry"), but the UX
impact on onboarding and restore is severe.

**Fix.** Compare the two sides' mtimes:

```go
if lastSyncNS == 0 {
    if lEntry.MtimeNS <= rEntry.MtimeNS {
        actions = append(actions, downloadEntry(path, rEntry))
    } else {
        actions = append(actions, conflictEntry(path, rEntry))
    }
}
```

**Confidence: 4/5.**

### I2 — `buildIndexExchange` sends a stale `Sequence` field relative to the SQLite rows in the same exchange

**Location:** `internal/filesync/filesync.go` · `buildIndexExchange`

Reads `currentSeq = fs.index.Sequence` under `indexMu.RLock`,
releases the lock, then calls `queryFilesSinceSeq` on SQLite without
holding `indexMu`. A concurrent `ActionDownload` between those steps
can bump `fs.index.Sequence` and commit a new row with the higher
sequence. The exchange is sent with the stale `currentSeq` but
includes the newer row.

The receiver records `LastSeenSequence = exchange.Sequence` (stale).
Next cycle, `sinceSeq = currentSeq` re-delivers the same row. In the
common case `lEntry.SHA256 == rEntry.SHA256` short-circuits with no
action. Under concurrent local mutation it can produce an incorrect
Download or Conflict.

**Fix.** Read `currentSeq` after the SQLite query, taking
`max(in-memory Sequence, highest row sequence returned)`.

**Confidence: 3/5.**

### I3 — `purgeTombstones` uses pre-scan peers snapshot; acknowledged tombstones survive an extra GC cycle

**Location:** `internal/filesync/filesync.go` · `runScan` /
`purgeTombstones`

`peersCopy` is snapshotted at scan-clone time. During the scan,
`syncFolder` goroutines advance `LastSeenSequence` for acknowledged
entries — invisible to `purgeTombstones`. A tombstone that all peers
acknowledged during the scan waits for the next GC cycle (≥ 10 scans
× scan interval).

GC latency only. No data loss.

**Fix.** Re-read `fs.peers` under `indexMu.RLock` immediately before
calling `purgeTombstones`.

**Confidence: 3/5.**

## Audited and clean

- Vector clock ops (`compareClocks`, `bump`, `merge`) — no off-by-
  one, commutative merge, nil-handling correct.
- `ClearPersisted` invariant — mutated path stays dirty because
  `Sequence` differs.
- First-sync tombstone guard (H8) — fires correctly on
  `lastSeenSeq == 0`.
- `PendingEpoch` lifecycle — cleared after first post-reset cycle.
- ActionDownload / Delete goroutine lifecycle — Add/Done balanced;
  claim/release paired.
- `selfID` empty path — set at openFolderInit, all `bump` calls
  guarded.
- Shutdown / mid-sync race — `persistAllCtx` runs before `wg.Wait`;
  re-persist or no-clear both safe.
- `scanWithStats` partial-scan failure — old index stays live on
  ctx error; no mass tombstone.

## Summary

| Severity | Count |
| -------- | ----- |
| Critical | 2     |
| Important | 2    |

**Top correctness risk for 2-peer steady-state:** C1. A SIGKILL
during conflict resolution permanently brands the remote bytes as a
local write through the next scan's clock bump. Compounds across
restarts in a high-churn folder.
