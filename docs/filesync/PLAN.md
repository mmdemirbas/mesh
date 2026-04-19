# Filesync Plan

Single source of truth for all filesync features, bugs, performance work, and
design ideas. Supersedes the previous `PLAN_PERF.local.md`, `FILESYNC-GAPS.md`,
and `FILESYNC-CONFLICT-PREVENTION.md` working notes.

Companion documents:

- `RESEARCH.md` — Syncthing internals reference, used as the comparison
  baseline in the per-item analysis below.
- `FILESYNC-ROLLOUT.local.md` — folder-by-folder rollout gate for the local
  MBP ⇄ Windows deployment. Not checked in.

Last updated: 2026-04-19.

---

## Resume Context

For a fresh session picking up this plan:

1. Read `RESEARCH.md §3` (version vectors) and `§4` (block model). Those two
   sections are the conceptual baseline the analysis below rests on.
2. Read the `Summary Table` and the `Verification of Done Items` below, then
   the `Execution Order`. That is the full state — nothing else is in flight.
3. Start work at **C1**. It is a one-line change in `index.go:diff()` with a
   focused test matrix (four edit states). Ship it before touching anything
   else in the conflict path.
4. Follow the execution order. Each item lists its prerequisites inline under
   "Recommendation".
5. Keep the Summary Table and the Verification table honest — update them as
   items flip from ⏳ to ✅. Do not promote to ✅ without citing the file and
   symbol where the behavior is locked in.

The local rollout gate (`FILESYNC-ROLLOUT.local.md`) is personal and
gitignored. It tracks which folders are enabled for `send-receive` on the
MBP ⇄ Windows deployment. It is downstream of this plan — phases 3 / 4 / 5
there depend on items tracked here.

---

## Legend

**Priority**

| Icon | Tier | Meaning                                                  |
|------|------|----------------------------------------------------------|
| 🔴   | P0   | Correctness / data integrity. Can silently lose data.    |
| 🟠   | P1   | Performance / scale. Blocks rollout to large folders.    |
| 🟡   | P2   | Robustness. Rare but observable edge cases.              |
| 🟢   | P3   | Differentiation. Optional; competitive parity or better. |
| ⚪   | —    | Deferred with explicit rationale.                        |

**Status**

| Icon | Meaning                                        |
|------|------------------------------------------------|
| ✅   | Done; verified against source.                 |
| 🔧   | In progress.                                   |
| ⏳   | Pending.                                       |
| ⏸    | Deferred (see per-item notes).                 |

**Effort**

| Icon | Scale | Rough size                                          |
|------|-------|-----------------------------------------------------|
| 🟩   | XS    | One-line fix or a single helper.                    |
| 🟨   | S     | One or two files, hours.                            |
| 🟧   | M     | Multi-file, a day or two, needs a test matrix.      |
| 🟥   | L     | Multi-module or protocol-touching, multi-day.       |

**Blast radius**

| Icon | Meaning                                                           |
|------|-------------------------------------------------------------------|
| 📄   | Local — one file or a leaf helper.                                |
| 📦   | Module — several files inside `internal/filesync`.                |
| 🔌   | Wire / on-disk format — affects peer compatibility or migration.  |

**Risk**

| Icon | Scale                                                               |
|------|---------------------------------------------------------------------|
| 🟢   | Low — bugs are obvious and local.                                   |
| 🟡   | Medium — needs a full test matrix.                                  |
| 🔴   | High — silent data corruption or protocol break if done wrong.      |

---

## Summary Table

| ID    | Item                                                 | Pri   | Area          | Status | Effort | Risk | Blast |
|-------|------------------------------------------------------|-------|---------------|--------|--------|------|-------|
| C1    | mtime vs last-sync in `diff()` (Idea A)              | 🔴 P0 | conflict      | ✅     | 🟩 XS  | 🟡   | 📄    |
| C2    | Per-peer last-exchanged hash (Idea B / ancestor)     | 🔴 P0 | conflict      | ✅     | 🟧 M   | 🟡   | 📦    |
| C3    | Per-block verify during write                        | 🔴 P0 | correctness   | ⏳     | 🟧 M   | 🟡   | 📦    |
| C4    | Immediate multi-peer fallback on hash mismatch       | 🔴 P0 | correctness   | ⏳     | 🟨 S   | 🟢   | 📦    |
| P17a  | Dirty flag — skip persist when unchanged             | 🟠 P1 | perf          | ✅     | 🟩 XS  | 🟢   | 📄    |
| P17b  | Gob persistence + YAML fallback                      | 🟠 P1 | perf          | ✅     | 🟨 S   | 🟡   | 🔌    |
| P18a  | Pre-size `seen` map                                  | 🟠 P1 | perf          | ✅     | 🟩 XS  | 🟢   | 📄    |
| P18b  | Incremental `activeCount` / `activeSize`             | 🟠 P1 | perf          | ✅     | 🟨 S   | 🟢   | 📄    |
| P18c  | Eliminate index clone (scan into `pending`)          | 🟠 P1 | perf          | ⏳     | 🟧 M   | 🟡   | 📦    |
| P18d  | Cap `buildIndexExchange` pre-allocation              | 🟠 P1 | perf          | ✅     | 🟩 XS  | 🟢   | 📄    |
| P3sc  | Adaptive watch / scan                                | 🟠 P1 | perf          | ⏳     | 🟧 M   | 🟡   | 📦    |
| PF    | Trie-based ignore with cursor propagation            | 🟠 P1 | perf          | ⏳     | 🟥 L   | 🟡   | 📦    |
| PK    | Clone elimination (COW / change-set on persist)      | 🟠 P1 | perf          | ⏳     | 🟧 M   | 🔴   | 📦    |
| PL    | Incremental deletion detection                       | 🟠 P1 | perf          | ⏳     | 🟨 S   | 🟢   | 📄    |
| PM    | Directory-keyed child index                          | 🟠 P1 | perf          | ⏳     | 🟨 S   | 🟢   | 📄    |
| PN    | Incremental `recomputeCache`                         | 🟠 P1 | perf          | ⏳     | 🟨 S   | 🟢   | 📄    |
| R1    | Inode-based rename / move detection                  | 🟡 P2 | robustness    | 🔧     | 🟧 M   | 🟡   | 🔌    |
| R2    | Formal folder-level state machine                    | 🟡 P2 | robustness    | ⏳     | 🟧 M   | 🟢   | 📦    |
| R3    | Peer-level failure blacklist                         | 🟡 P2 | robustness    | ⏳     | 🟨 S   | 🟢   | 📦    |
| D1    | FastCDC content-defined chunking                     | 🟢 P3 | differentiate | ⏳     | 🟥 L   | 🔴   | 🔌    |
| D2    | BLAKE3 instead of SHA-256                            | 🟢 P3 | differentiate | ⏳     | 🟧 M   | 🔴   | 🔌    |
| D3    | Linux `fanotify` backend                             | 🟢 P3 | differentiate | ⏳     | 🟧 M   | 🟡   | 📦    |
| D4    | SQLite-backed index                                  | 🟢 P3 | differentiate | ⏳     | 🟥 L   | 🔴   | 🔌    |
| D5    | Sparse file detection                                | 🟢 P3 | differentiate | ⏳     | 🟧 M   | 🟡   | 📦    |
| D6    | Per-transfer zstd compression                        | 🟢 P3 | differentiate | ⏳     | 🟧 M   | 🟡   | 🔌    |
| C5    | 3-way text merge (Idea C)                            | ⚪    | conflict      | ⏸      | 🟥 L   | 🔴   | 📦    |
| C6    | Full vector clocks per file (Idea D)                 | ⚪    | conflict      | ⏸      | 🟥 L   | 🔴   | 🔌    |

Counts: **4** P0 (1 ✅ / 3 ⏳) · **12** P1 (5 ✅ / 7 ⏳) · **3** P2 · **6** P3 · **2** deferred.

---

## Verification of Done Items

All `done` entries re-verified against the tree on 2026-04-19.

| ID    | Verification                                                                                                |
|-------|-------------------------------------------------------------------------------------------------------------|
| P17a  | `indexDirty` / `peersDirty` fields on `folderState` (`filesync.go` ~L427); `persistFolder` gates (~L2127).  |
| P17b  | `encoding/gob` Encode / Decode in `index.go` (~L265 / L274). YAML fallback path present for migration.      |
| P18a  | `seen := make(map[string]struct{}, len(idx.Files))` in `index.go` (~L673).                                  |
| P18b  | `cachedCount` / `cachedSize` on `FileIndex`; `activeCountAndSize()` is O(1) field read (~L389).             |
| P18d  | Delta path uses `len(tail)` via `seqIndex` binary search (`filesync.go` ~L2031). Full path only on bootstrap. |
| C1    | `diff()` takes `lastSyncNS` and compares `lEntry.MtimeNS` against it for both the B8 tombstone guard and the conflict classifier (`index.go`, `FileIndex.diff`). Caller in `syncFolder` passes `ps.LastSync.UnixNano()`. Covered by `TestDiffC1MtimeVsLastSync` and `TestDiffC1TombstoneMtimeVsLastSync`. |
| C2    | `PeerState.BaseHashes` holds the last agreed hash per path; `diff()` uses it as the primary signal (ancestor match ⇒ download-or-skip, both diverged ⇒ conflict) and falls back to C1 mtime when absent. `updateBaseHashes` folds each completed exchange into the ancestor map (hash match records, tombstone drops, mismatch preserves prior). Caller in `syncFolder` snapshots `ps.BaseHashes` before diff and re-merges on both the no-action and sync-end paths. Covered by `TestDiffC2AncestorClassifier`, `TestDiffC2TombstoneAncestor`, and `TestUpdateBaseHashes`. |
| R1 (partial) | Receiver-side content-hash rename landed: `planRenames` (in `index.go`) pairs each ActionDelete whose local file has hash H with one ActionDownload whose RemoteHash is H, and `syncFolder` performs an atomic local rename (with Chtimes/Chmod, tombstone + new-path index entry) for each plan. Both sides of the rename are skipped in the bundle loop and the main dispatch loop. Metrics `FilesRenamed` and `BytesSavedByRename` exported via `/api/metrics` (`mesh_filesync_files_renamed_total`, `mesh_filesync_bytes_saved_by_rename_total`). Covered by `TestPlanRenames*` (happy path, hash mismatch, one-to-one pairing, target exists, tombstoned source, missing source, nil inputs) and `TestR1RenameFilesystemIntegration`. Remaining: inode-based sender-side rename detection with wire protocol capability handshake for the case where the renamed file was also edited. See R1 Status note. |

`P18c` is still pending: `fs.index.clone()` remains at `filesync.go:1030` (runScan) and `filesync.go:2151` (persistFolder).

---

## Goals and Targets

Make filesync viable as a full Syncthing replacement for folders up to
~200 k files without heavier resource use than Syncthing.

| Metric                                   | Current (est.) | Target     |
|------------------------------------------|----------------|------------|
| Scan cycle time (168 k files, stable)    | ~30 s          | < 10 s     |
| Memory during scan                       | ~160 MB spikes | < 200 MB   |
| Persistence write (168 k files)          | several s      | < 1 s      |
| Silent conflict files on 2-device edits  | occasional     | 0          |
| Bandwidth on rename                      | full file      | metadata   |

---

## Per-Item Deep Dive

Each entry follows the same structure:

- **Problem** — concrete symptom / code site.
- **Why it matters** — user-visible impact if left alone.
- **Fix options** — alternatives considered, with the trade-off axes named.
- **Risks** — what can go wrong if the fix is rushed.
- **Impact** — performance / security / UX axes.
- **Blast radius** — how many files / whether wire or on-disk format changes.
- **Syncthing / competitor handling** — prior art for sanity checking.
- **Recommendation** — the path this plan picks.

---

## 🔴 P0 — Correctness

### C1 · mtime vs last-sync timestamp in `diff()` (Idea A)

- **Problem.** `index.go:diff()` decides "was our copy locally modified since
  we last talked to this peer?" by comparing `lEntry.Sequence` (our folder
  counter) with `PeerState.LastSeenSequence` (their folder counter at our last
  exchange). Those two counters live on different scales. When one side has
  done many more operations than the other, the heuristic misfires and a
  file that was only touched remotely is flagged as a two-sided conflict.
- **Why it matters.** False positive conflicts create `.sync-conflict-*`
  siblings for files the user never modified. The user has to prune them by
  hand and loses trust in the sync.
- **Fix options.**
  1. Replace with `lEntry.MtimeNS <= lastSyncNS` where `lastSyncNS` comes
     from `PeerState.LastSync` (already persisted). One-line change.
  2. Use an incrementing local "last-edited-by-us" counter per file that is
     reset on every download. Equivalent semantics but adds a field.
  3. Skip ahead to C2 (ancestor hash) — fully correct, not a heuristic.
- **Risks.**
  - Tools that preserve mtime on rewrite (`git checkout`, some editors,
    `rsync -t`) can set mtime backwards. Such a file would pass the
    "not locally modified" check and be overwritten by the remote — which
    in the git-checkout case is correct, but surprising.
  - VM clock jumps remain a risk but strictly smaller than the sequence
    heuristic.
- **Impact.**
  - *Perf:* zero — same comparison cost.
  - *Security:* none.
  - *UX:* removes a known source of spurious conflict files.
- **Blast radius.** 📄 one conditional in `diff()`, plus a test matrix of the
  four edit states (neither / local-only / remote-only / both).
- **Syncthing handling.** Syncthing does not rely on mtime comparisons in its
  `diff` path at all — it uses version vectors (see C6). Our option (1) is
  strictly weaker but still a material improvement over the cross-scale
  sequence compare. `RESEARCH.md §3.1` describes Syncthing's version-vector
  model.
- **Recommendation.** Ship option (1) first. It is a one-line correctness
  improvement with no protocol change. C2 subsumes it and can land later
  without reverting this.

### C2 · Per-peer last-exchanged hash (Idea B / ancestor)

- **Problem.** `diff()` has no memory of the version both sides last agreed
  on. It only sees "their hash" and "our hash". If the two differ, it must
  guess which side caused the divergence.
- **Why it matters.** This is the canonical cause of false conflicts in
  two-device setups. Without an ancestor, there is no way to distinguish
  "we modified the stale copy we got from them" from "we both modified the
  same starting point independently".
- **Fix options.**
  1. Add a parallel map `peerBaseHash[peerAddr]map[path]Hash256`, updated on
     every successful download and successful ack'd upload. Persisted
     alongside `peers.yaml`. Used in `diff()`:
     `localModified := lEntry.SHA256 != ancestor; remoteModified :=
     rEntry.SHA256 != ancestor; conflict := localModified && remoteModified`.
  2. Embed `ReceivedSHA256` on every `FileEntry`. Simpler lookup, but
     inflates every record whether the folder has one peer or four. Costs
     32 B × files × peers-per-file.
  3. Skip to vector clocks (C6). Strictly more correct at N devices, but
     requires protocol change.
- **Risks.**
  - Bootstrap period: before any ancestor is recorded, we must fall back
    to C1. A cold peer therefore takes one cycle before ancestor-aware
    resolution kicks in.
  - Map can drift if updates are not atomic with the index swap. Persist
    under the same write as the peer state.
- **Impact.**
  - *Perf:* O(1) extra map lookup per file in `diff()`. Negligible.
  - *Memory:* 32 B per file per peer. 168 k × 2 = ~10 MB. Acceptable.
  - *Security:* none. Hashes only, no content.
  - *UX:* eliminates ~all false conflicts in two-device mode.
- **Blast radius.** 📦 new persistence record, small touch in `syncFolder`
  download and upload success paths, new branch in `diff()`. No wire-format
  change.
- **Syncthing handling.** Syncthing does not carry a per-peer ancestor
  explicitly — its version vector effectively encodes the same information
  in causal form. Ancestor-hash is the pragmatic two-device equivalent.
- **Recommendation.** Ship option (1). It is the minimum solution that is
  definitively correct for two devices and unlocks 3-way merge (C5) later.

### C3 · Per-block verify during write

- **Problem.** `transfer.go:downloadToVerifiedTemp` writes the entire file
  to a temp path and then hashes. A single corrupted byte anywhere in a
  10 GB file forces the whole file to be re-requested.
- **Why it matters.** No data-integrity gap — corruption is always caught
  before rename, never propagated. But recovery cost is unbounded in file
  size, and on a flaky link a single large file may never complete.
- **Fix options.**
  1. Stream the hash per 128 KB block during write; on mismatch, discard
     the block and re-request only that offset range. Reuses existing
     block boundaries from `blockhash.go`.
  2. Add a Merkle tree over blocks and retry mid-file without restart.
     Overkill for this scale; adds on-disk state.
  3. Leave as-is and document the trade-off.
- **Risks.**
  - Per-block hash means the block hashes must be trusted. They must come
    from the sender's authoritative index, not be computed on the fly
    from the bytes we're validating.
  - Retry loops must have a ceiling to avoid infinite block churn on a
    peer that serves garbage (relates to C4 and R3).
- **Impact.**
  - *Perf:* marginal CPU increase (hash is streaming anyway on modern
    hardware). Big win on flaky networks with large files.
  - *Security:* reduces window where corrupted intermediate data sits on
    disk.
  - *UX:* large-file transfers become robust on lossy links.
- **Blast radius.** 📦 `transfer.go` and the block-sig request path in
  `protocol.go`. No wire-format change if we already request by block.
- **Syncthing handling.** Syncthing hashes each block on receipt and only
  keeps good blocks, exactly option (1). See `RESEARCH.md §4` on the
  block-by-block transfer model.
- **Recommendation.** Ship option (1) after C1 and C2. Land together with
  C4 so the retry policy is consistent.

### C4 · Immediate multi-peer fallback on hash mismatch

- **Problem.** On hash mismatch, `retryTracker.record` bumps a
  `(path, remoteHash)` failure count and waits for the next sync cycle
  (30 s) before trying anyone else. Files that hit `maxRetries=3` are
  quarantined until the remote hash changes.
- **Why it matters.** If peer A is serving a bad version of a file, peers
  B and C are not tried until A's failure count trips. On a two-peer
  folder, one bad peer can delay any successful sync by minutes.
- **Fix options.**
  1. Within the same sync cycle, iterate remaining peers offering the same
     target hash before giving up on that file.
  2. Scope `retryTracker` by `(path, hash, peer)` so a bad peer doesn't
     poison the retry budget for other peers.
  3. Both, together. Option (2) is the data model change, option (1) uses
     the updated model.
- **Risks.**
  - Thundering herd: if we retry across peers in the same cycle, a bad
    file hits every peer immediately. Cap the in-cycle fallback to a
    small constant (e.g., 3 peers) before deferring.
  - Peer-scoped failure counts interact with R3 (peer blacklist) — keep
    the models aligned.
- **Impact.**
  - *Perf:* faster convergence on transient corruption.
  - *Security:* limits how long a malicious peer can stall a folder.
  - *UX:* fewer quarantined files in dashboards.
- **Blast radius.** 📦 `retryTracker` internals and the `syncFolder`
  action loop.
- **Syncthing handling.** Syncthing has multi-peer block request routing
  — any block can come from any peer advertising it. Mesh currently
  requests whole files and so must iterate at the file level.
- **Recommendation.** Ship (3). Not urgent but satisfying to pair with C3
  so the retry story is complete.

---

## 🟠 P1 — Performance

### P17a · Dirty flag — skip persist when unchanged · ✅

- **Problem.** `persistFolder` used to run after every `syncFolder` (~30 s
  per peer) whether or not anything changed. ~30 MB of YAML × 2 writes +
  fsync per idle cycle.
- **Fix shipped.** `indexDirty` / `peersDirty` bits on `folderState`, flipped
  on every mutation path, checked before serialization. Commit `a4623a6`.
- **Verification.** See Verification table above.
- **Notes.** Eliminated ~90 % of persists in idle state.

### P17b · Gob persistence + YAML fallback · ✅

- **Problem.** YAML marshal for 168 k entries produced ~30 MB and took
  several seconds, hurting scan-cycle latency.
- **Fix shipped.** Switched to `encoding/gob` for both index and peer state.
  Legacy YAML is read as fallback on load; next save rewrites as gob.
- **Why gob, not protobuf.** Gob is in the stdlib, avoids a new wire-format
  generator, and is ~3–5× faster than YAML. Protobuf would also work and
  is future-compatible with the wire-format index, but was out of scope
  for P17.
- **Verification.** See Verification table.
- **Note for future.** If D4 (SQLite index) happens, this format is
  superseded. The migration path (gob → SQLite) must read the existing
  on-disk gob once.

### P18a · Pre-size `seen` map · ✅

- **Problem.** `make(map[string]struct{})` with no capacity hint causes
  8+ rehashes as it grows to 168 k entries during scan.
- **Fix shipped.** Pre-size to `len(idx.Files)`. One-line change.
- **Verification.** See Verification table.

### P18b · Incremental `activeCount` / `activeSize` · ✅

- **Problem.** `activeCountAndSize()` iterated the whole index to report
  file count and total size, called after every sync and on every
  `/api/filesync/folders` request. O(N) per call.
- **Fix shipped.** `cachedCount` / `cachedSize` fields on `FileIndex`,
  maintained incrementally by `setEntry`. `recomputeCache()` exists for
  bulk reloads (load, clone, scan swap).
- **Verification.** See Verification table.
- **Open follow-up.** PN — make `recomputeCache` itself incremental on
  scan swap.

### P18c · Eliminate index clone (scan into `pending`) · ⏳

- **Problem.** `runScan` calls `fs.index.clone()` before walking the tree,
  so the walker can mutate a private copy while readers see the old
  snapshot. For 168 k entries that is ~50 MB copied on every tick
  (60 s ticker + fsnotify triggers).
- **Why it matters.** Highest single memory allocation in steady-state
  scans. The cost is paid even when the scan ends up touching zero
  entries.
- **Fix options.**
  1. Scan builds a `changes map[string]FileEntry` (new / modified /
     deleted). `runScan` applies the change-set under write lock:
     `for k,v := range changes { idx.Files[k] = v }`. No clone.
     Additional cost: deletion detection must still iterate the `seen`
     set (covered by PL).
  2. Keep the clone but make it COW (PK). Adds complexity at every map
     access — higher risk than (1).
  3. Do nothing and rely on periodic scan being rare enough.
- **Risks.**
  - Readers (admin UI, `activeCountAndSize`) must never observe a
    partially-applied change-set. The write lock already protects this;
    the swap is atomic.
  - The change-set must correctly encode "deletion" vs "unchanged" —
    absence from the map must mean the latter.
- **Impact.**
  - *Perf:* 50 MB × scan-frequency allocation disappears.
  - *Memory:* peak scan memory drops from 2× index to 1× index + delta.
- **Blast radius.** 📦 `runScan`, `scanWithStats`, readers of the post-scan
  index. No persistence or wire change.
- **Syncthing handling.** Syncthing holds its index in SQLite and never
  clones. The parallel here is D4.
- **Recommendation.** Ship (1) once PL (deletion detection) lands, since
  PL provides the "seen" set needed to identify deletions under the new
  model.

### P18d · Cap `buildIndexExchange` pre-allocation · ✅

- **Problem.** `make([]*pb.FileInfo, 0, len(fs.index.Files))` pre-allocated
  168 k pointer slots even when delta-since was 0–10 entries.
- **Fix shipped.** The delta path now walks `seqIndex` (secondary index
  sorted by sequence), allocating `len(tail)`. Full exchange only runs on
  first contact (sinceSequence == 0).
- **Verification.** See Verification table.

### P3sc · Adaptive watch / scan · ⏳

- **Problem.** `defaultMaxWatches = 4096` is a hard cap on fsnotify
  watches. Folders with more directories (spark-kit 16 k dirs, m2-repo
  27 k dirs) fall back to scan-only mode with the full periodic scan
  cost. Static "watch everything" or "watch nothing" are both wrong.
- **Why it matters.** Large-folder rollout (phases 4 and 5 in the local
  rollout) is gated on this. Scan-only mode at 30 s ticks with 168 k
  files is too slow to pick up active work.
- **Fix options.**
  1. Adaptive, self-tuning: track per-directory change frequency over a
     5-minute window. Promote hot directories to fsnotify up to a soft
     cap (3000 watches); demote after 2 quiet windows (~10 min) with a
     cooldown before re-promoting.
  2. User-configurable watch list per folder — simpler but pushes the
     heuristic onto the user.
  3. Watch top-N directories by file count, not change frequency. Wrong
     signal — biggest directories are often the least active (asset
     trees).
- **Risks.**
  - Promotion must not starve the demotion logic. Cooldown prevents
    thrash.
  - A burst in a cold directory is missed until the next scan notices
    the change, then promotes. Acceptable latency.
- **Impact.**
  - *Perf:* scan-only folders pick up active work in seconds, not per
    scan-cycle.
  - *UX:* no new config — works automatically.
- **Blast radius.** 📦 `watcher.go`, new frequency map, integration with
  scan loop.
- **Syncthing handling.** Syncthing watches everything inside the folder
  via the OS-native watcher (inotify on Linux). On Linux 5.1+ the right
  answer is `fanotify` (see D3) which lifts the per-FD limit entirely.
- **Recommendation.** Ship (1). D3 (fanotify) supersedes it on modern
  Linux but is opt-in; (1) is the universal fallback.

### PF · Trie-based ignore with cursor propagation · ⏳

- **Problem.** `ignore.shouldIgnore` evaluates patterns linearly —
  O(P) per path segment where P = pattern count. On the 310 k-file
  client it is the single largest scan-time hotspot (~3.6 s per full
  scan).
- **Why it matters.** Every file in every scan pays the cost. Large
  projects (monorepos, node_modules, build trees) have many patterns.
- **Fix options.**
  1. Segment trie built once at config load. Each directory descent
     advances a cursor O(1) amortized; wildcards fan out from a single
     node. Pattern evaluation becomes O(depth × branch-factor).
  2. Precompile patterns into regex alternation. Cheaper to write but
     same big-O as current and loses gitignore semantics.
  3. Cache the decision per directory. Helps only if the same directory
     is re-asked — our scan already visits each directory exactly once.
- **Risks.**
  - Gitignore semantics are subtle: later rules override, negation
    un-ignores, directory-only patterns (trailing `/`), `**` matches
    zero or more segments, `!pattern` requires rule-priority tracking
    at trie terminals.
  - Regression risk: a wrongly-included file is a data leak. Requires
    an exhaustive conformance test suite against `git check-ignore`.
- **Impact.**
  - *Perf:* 10–100× on deep trees; break-even on shallow trees.
  - *UX:* no behaviour change if semantics match.
- **Blast radius.** 📦 `ignore.go` full rewrite.
- **Syncthing handling.** Syncthing uses a compiled glob set with a
  similar-in-spirit fast path. `RESEARCH.md §2` on the ignore model.
- **Recommendation.** Ship (1) as Phase 2 of scan-time perf work.
  Mandatory conformance test suite: generate 10 k random patterns ×
  10 k random paths; compare against a reference linear evaluator.

### PK · Clone elimination (COW / change-set on persist) · ⏳

- **Problem.** Even after P18c removes the scan-time clone,
  `persistFolder` still clones to get a stable snapshot for the writer.
  Same 50 MB alloc cost, just moved.
- **Fix options.**
  1. Copy-on-write: `clone()` returns a handle sharing the backing map;
     writers go through an overlay; flatten on persist completion.
  2. Persist under a read lock without cloning. Blocks writers for the
     duration of the write (~ seconds for 168 k entries). Bad.
  3. Double-buffered index: two full maps, swap on scan completion.
     Halves peak memory vs today but still double-sized.
- **Risks.**
  - COW correctness bugs manifest as silent data corruption.
  - The overlay model must compose with the scan-time change-set from
    P18c — two overlays stacked needs an ordering discipline.
- **Impact.**
  - *Perf:* O(1) snapshot instead of O(N).
  - *Memory:* eliminates ~750 MB/hr of transient allocation across 15
    folders.
- **Blast radius.** 📦 index-access hot path. Risk is widespread if map
  access is open-coded elsewhere.
- **Syncthing handling.** Not applicable — SQLite provides MVCC for free
  (D4).
- **Recommendation.** Only ship once P18c has landed and measurement
  shows persist-time allocation still dominates. Heavy test matrix is a
  prerequisite.

### PL · Incremental deletion detection · ⏳

- **Problem.** After scan, `index.go:834-848` iterates the whole index to
  find entries the scan did not visit. O(N) per scan.
- **Fix options.**
  1. During scan, track seen paths. After scan, iterate `idx.Files - seen`.
     O(deleted) in the common case.
  2. Mark deletions eagerly from fsnotify Remove events. Periodic scan
     remains the safety net for missed events.
  3. Both — (2) for real-time, (1) for the safety-net pass.
- **Risks.**
  - `seen` set is transient memory. For 168 k entries, ~5 MB — fine.
  - fsnotify can miss events under load. Must still reconcile via (1).
- **Impact.** O(deleted) instead of O(N). For a folder with 10 deletions
  per scan, this is 17000× less work.
- **Blast radius.** 📄 local to the scan loop.
- **Syncthing handling.** Syncthing uses the DB diff directly — the
  per-scan delta is a query, not a scan.
- **Recommendation.** Ship (3). Required prerequisite for P18c.

### PM · Directory-keyed child index · ⏳

- **Problem.** Error-path protection (`index.go:665-670`) scans the full
  index to find children of a failed directory. O(N × M) where M = number
  of failed directories in a scan.
- **Fix options.**
  1. Secondary index `dirChildren map[string][]string`, appended during
     scan. O(children) lookup.
  2. Sort Files by path once and binary-search the prefix. Works but
     requires a sort or a separate sorted slice.
- **Risks.**
  - Secondary index must stay consistent with `Files`. Simplest: rebuild
    after swap, appended during scan.
- **Impact.** Rarely exercised, but when a directory fails (perm denied,
  unmount), the scan slowdown compounds with the number of failures.
- **Blast radius.** 📄 `index.go` internals.
- **Syncthing handling.** Directory-keyed queries come for free with the
  SQLite schema.
- **Recommendation.** Ship (1) alongside PL — same kind of secondary
  index work.

### PN · Incremental `recomputeCache` · ⏳

- **Problem.** `recomputeCache` rebuilds the active-files cache
  (advisory, used by `claimPath` dedup) from scratch after every scan
  swap. O(N).
- **Fix options.**
  1. Apply cache delta from the scan change-set (requires P18c).
  2. Leave as-is — cache is advisory and stale entries only cause
     redundant downloads, not data loss.
- **Risks.** Low. Advisory cache.
- **Impact.** O(delta) per scan instead of O(N).
- **Blast radius.** 📄 single helper.
- **Syncthing handling.** Not applicable.
- **Recommendation.** Ship (1) after P18c. Pure incremental win; cheap.

---

## 🟡 P2 — Robustness

### R1 · Inode-based rename / move detection

- **Problem.** `FileEntry` has no inode field. Renames within the synced
  tree look like delete + create, forcing a full file re-upload.
- **Why it matters.** `git mv` of a 1 GB asset re-uploads 1 GB. IDE
  refactors and file-manager moves pay the same cost. Disproportionately
  bad for monorepos.
- **Fix options.**
  1. Add `Inode uint64` to `FileEntry`. Maintain an inode → path reverse
     index during scan. Rename = same inode, different path → emit
     tombstone + metadata-only update message. Peers apply the rename
     locally when the inode + content hash both match.
  2. Accept the waste. Small folders don't notice.
- **Risks.**
  - Windows has no stable inode; use the file ID from
    `GetFileInformationByHandle` (already used in platform-specific
    device detection). Falls back to delete-create on filesystems that
    don't expose a stable ID.
  - Inode reuse after deletion is a real thing on some filesystems.
    Verify by also checking content hash before treating as rename.
- **Impact.**
  - *Perf:* eliminates the biggest bandwidth waste case.
  - *UX:* invisible — peers see the rename happen, not a
    delete-then-appear flicker.
- **Blast radius.** 🔌 new field on `FileEntry`, new message type in
  `proto/filesync.proto`, scan-loop inode bookkeeping. Wire-format
  backward compatible if the new message has a feature flag; peers
  without rename support fall back to full transfer.
- **Syncthing handling.** Syncthing tracks inodes per folder and does
  exactly this. See `RESEARCH.md` §4.6 and the stat-pre-filter section.
- **Recommendation.** Ship (1). Gate the on-wire message behind a
  capability handshake; on unknown peer, fall back to current behavior.
- **Status.** Partial — receiver-side content-hash rename landed without
  any wire-format or proto change (see `planRenames` in `index.go` and
  the R1 branch in `syncFolder`). It captures the primary bandwidth
  win whenever the renamed file's content is unchanged: the receiver
  notices a download/delete pair where its local file at the delete
  path already hashes to the download's target, and performs an atomic
  local rename instead of redownloading. **Open question:** is this
  sufficient, or should we also ship the inode-tracking + wire
  capability handshake variant for the case where the sender sees
  inode-same / hash-different (content edited during rename)? Today
  that falls back to full re-transfer, which mirrors the recommended
  behavior before rename-support peers handshake. Decision deferred.

### R2 · Formal folder-level state machine

- **Problem.** `firstScanDone` gates the first sync but steady-state has
  no explicit `scanning` / `syncing` state. A slow scan plus a sync cycle
  starting mid-scan operates on a stale index snapshot. Today the mutex
  usage keeps this safe, but the invariant is implicit — easy to break
  in a future refactor.
- **Fix options.**
  1. Explicit states per folder (`idle`, `scanning`, `syncing`,
     `degraded`). Reject overlapping transitions; queue otherwise.
  2. Keep implicit; add assertions (race-only) that fire on invariant
     break.
- **Risks.** Low — state machine is additive; worst case is a missed
  scan tick.
- **Impact.**
  - *Perf:* marginal — a slow scan doesn't overlap with sync, which can
    either speed things up (no lock fight) or slow them down (sync
    waits) depending on the workload.
  - *Maintainability:* the big win. Invariants are enforceable.
- **Blast radius.** 📦 adds a field to `folderState` and gates scan/sync
  loop entry.
- **Syncthing handling.** Syncthing has explicit folder states
  (`idle`, `scanning`, `syncing`, `error`, etc.) surfaced in its UI.
- **Recommendation.** Ship (1) after R1 — it gives the state machine
  something real to coordinate (rename handling needs explicit
  quiescence).

### R3 · Peer-level failure blacklist

- **Problem.** Today a peer that serves consistently wrong data is
  indistinguishable from a network error. Each bad file requires its own
  quarantine.
- **Fix options.**
  1. `peerFailures map[string]int`, exponential backoff on the peer
     (not the file) after N consecutive errors across any file.
     Unblacklisted after a successful exchange or a timeout.
  2. Score peers continuously and prefer the best one; don't blacklist.
     Harder to implement; softer signal.
- **Risks.**
  - False blacklist from a bad WiFi link would stop sync entirely.
    Threshold and reset conditions must be forgiving.
- **Impact.**
  - *Perf:* zero when everyone is healthy.
  - *Security:* narrows the window for a compromised peer.
  - *UX:* the dashboard can show "peer X backing off, N errors" rather
    than N scattered quarantines.
- **Blast radius.** 📦 `retryTracker`, admin API surface for visibility.
- **Syncthing handling.** Syncthing has an explicit device-paused state
  triggered by repeated errors or manual action.
- **Recommendation.** Ship (1). Pairs well with C4 for a consistent
  retry story.

---

## 🟢 P3 — Differentiation

### D1 · FastCDC content-defined chunking

- **Problem.** Fixed 128 KB blocks mean a 1-byte insertion at offset 0
  shifts every downstream block boundary; full retransfer.
- **Fix options.**
  1. FastCDC (`github.com/jotfs/fastcdc-go`): rolling-hash cut points
     with average 128 KB, min 32 KB, max 512 KB. Boundaries are stable
     under insertions.
  2. Gear CDC / rabin fingerprints. Older, slower than FastCDC.
  3. Leave as-is; rely on rsync-style rolling hash matching at transfer
     time.
- **Risks.**
  - 🔌 wire / on-disk format change. Existing block-sig arrays are tied
    to fixed size; variable-size requires carrying the offset per block.
  - Different peers chunking the same file must arrive at the same
    boundaries, which FastCDC guarantees given the same parameters.
- **Impact.**
  - *Perf:* 2–3× better delta efficiency on text and non-aligned binary
    modifications. Log files, append-only databases, source trees are
    the big winners.
- **Blast radius.** 🔌 `blockhash.go` boundary generation, all wire
  messages carrying block metadata, on-disk index if we cache block
  boundaries (we don't today — recomputed on request).
- **Syncthing handling.** Syncthing uses fixed 128 KB blocks like us.
  See `RESEARCH.md §16.1`. FastCDC would actually differentiate us.
- **Recommendation.** Phase-gated: after C3, C4, R1 are stable. Protocol
  version bump required.

### D2 · BLAKE3 instead of SHA-256

- **Problem.** Full-scan CPU is dominated by SHA-256 hashing.
- **Fix options.**
  1. Replace `sha256.New()` pool and `sha256.Sum256` calls with
     `github.com/zeebo/blake3`. Wire format change: rename
     `FileInfo.Sha256` or add an `algo` discriminator.
  2. Parallelize SHA-256 across cores. Helps but does not close the
     gap to BLAKE3.
  3. Use BLAKE2b. Faster than SHA-256 but slower than BLAKE3 and not a
     meaningful win vs the migration cost.
- **Risks.**
  - 🔌 wire / on-disk change. Mixed-algo folders need negotiation.
  - Hash output stored in every `FileEntry`; adding `algo` per entry
    is correct but bloats the index. Probably move `algo` to folder
    level with per-file override for migration.
- **Impact.**
  - *Perf:* ~75 % CPU reduction on full-scan hashing.
- **Blast radius.** 🔌 every hashing call site, wire, on-disk.
- **Syncthing handling.** Syncthing uses SHA-256. `RESEARCH.md §16.2`
  notes this as an upgrade opportunity.
- **Recommendation.** Design-gated behind C6 (vector clocks) wire bump
  — land both together if the protocol version is going to move anyway.

### D3 · Linux `fanotify` backend

- **Problem.** inotify needs one FD per directory; `defaultMaxWatches`
  caps at 4096.
- **Fix options.**
  1. fanotify with `FAN_MARK_FILESYSTEM` (Linux 5.1+): one FD per mount,
     no queue overflow, covers NFS / CIFS.
  2. eBPF-based file-event tap. Much harder to deploy.
- **Risks.**
  - Requires `CAP_SYS_ADMIN` or file capabilities. Opt-in backend with
    fsnotify as universal fallback.
  - Event shape differs slightly from inotify — need a small adapter
    layer.
- **Impact.**
  - *Perf:* lifts the watch ceiling; removes the need for scan-only
    fallback on huge trees.
  - *Security:* running with `CAP_SYS_ADMIN` is a privilege escalation
    surface. Document clearly.
- **Blast radius.** 📦 `watcher.go`, a new backend file behind build
  tags.
- **Syncthing handling.** Syncthing is evaluating fanotify but has not
  shipped it. `RESEARCH.md §4.6`.
- **Recommendation.** Ship (1) as opt-in. Pairs well with P3sc which
  remains the universal fallback.

### D4 · SQLite-backed index

- **Problem.** The gob store is correct and crash-safe but requires
  loading the entire index into memory to answer any query. The in-process
  secondary index (`seqIndex` — PG) is a workaround.
- **Fix options.**
  1. SQLite in WAL mode. `CREATE INDEX idx_sequence ON files(folder,
     sequence)`, `CREATE INDEX idx_inode ON files(folder, inode)`,
     etc. O(log N) lookups; no full-index load.
  2. BoltDB / bbolt. Embedded KV, simpler than SQL but without secondary
     indexes (have to maintain them manually, re-introducing the
     in-process problem).
  3. RocksDB. Overkill; CGo dependency conflicts with `CGO_ENABLED=0`
     release target.
- **Risks.**
  - 🔌 on-disk format change. Migration from gob → SQLite once on
    upgrade.
  - SQLite has its own crash-safety semantics; must audit fsync behavior
    vs today's explicit double-write + fsync.
  - Adds a dependency. Needs explicit approval per repo rules.
- **Impact.**
  - *Perf:* huge for large folders — no more O(N) anything.
  - *Memory:* drops to working set only.
- **Blast radius.** 🔌 `index.go` re-implementation, migration path.
- **Syncthing handling.** Syncthing uses LevelDB today and is migrating
  to SQLite. `RESEARCH.md §16.5`.
- **Recommendation.** Right long-term answer, but large enough to need
  a design doc and dependency approval first.

### D5 · Sparse file detection

- **Problem.** A 10 GB sparse file with 1 MB of data is hashed and
  transferred as 10 GB.
- **Fix options.**
  1. `SEEK_DATA` / `SEEK_HOLE` to enumerate data extents during scan;
     hash only populated extents.
     `fallocate(FALLOC_FL_PUNCH_HOLE)` to recreate holes on write.
  2. Ignore sparseness; accept the waste.
- **Risks.**
  - Platform differences. Linux and macOS support the seek flags;
    Windows has its own sparse API.
  - Receiver must pre-allocate file as sparse or punch holes after
    write — interacts with the temp-file transfer strategy.
- **Impact.**
  - *Perf:* order-of-magnitude for VM images, DB files, container
    layers.
- **Blast radius.** 📦 scan (size + extents) and transfer (write
  behavior). No wire-format change if extents are negotiated as a
  block-level optimization.
- **Syncthing handling.** Syncthing does not preserve sparseness.
- **Recommendation.** Defer until a concrete user workload needs it.

### D6 · Per-transfer zstd compression

- **Problem.** Index exchanges are gzip-compressed; file payloads are
  raw.
- **Fix options.**
  1. Replace gzip with zstd for index exchanges (already a header-flag
     change). Add optional per-transfer zstd with magic-byte detection
     to skip already-compressed formats.
  2. Leave as-is — modern links are fast enough.
- **Risks.**
  - 🔌 wire change. Needs a content-encoding negotiation.
  - False negatives on magic-byte detection waste CPU recompressing
    compressed bytes.
- **Impact.**
  - *Perf:* 30–50 % throughput on source code, configs, logs.
- **Blast radius.** 🔌 `protocol.go` transfer endpoints.
- **Syncthing handling.** Syncthing has per-folder compression settings
  covering both indexes and transfers.
- **Recommendation.** Land the zstd index swap first (small, self-
  contained). File-payload compression second, behind a folder
  config flag.

---

## ⚪ Deferred

### C5 · 3-way text merge (Idea C)

- **Why deferred.**
  - Needs an ancestor content cache on disk (LRU, size-bounded, text-only
    allow-list). Adds bookkeeping, crash recovery, and storage policy.
  - diff3 correctness at edge cases (adjacent edits, blank lines, mixed
    line endings) needs a well-tested library. `conflict.go:diffLines`
    is not production-grade.
  - Correctness delivered by C2 alone is the large win; C5 is a UX nice
    to have.
- **Revisit trigger.** Conflict files still a visible pain point after
  C2 is stable in two-device use for four weeks.
- **Syncthing handling.** No 3-way merge. Unison does it; rsync doesn't.
  Git's own merge drivers are the gold standard here.

### C6 · Full vector clocks per file (Idea D)

- **Why deferred.**
  - 🔌 wire-format change (`repeated Counter version = N` on
    `FileInfo`) plus peer-version negotiation and a migration path.
  - Current deployment is two devices. C1 + C2 cover that case fully.
    Vector clocks become strictly required only at 3+ devices.
  - If D2 (BLAKE3) or D1 (FastCDC) land, their protocol bump is a good
    moment to fold in vector clocks and avoid two separate breaking
    changes.
- **Revisit trigger.** A third device joins any folder, or D1 / D2
  lands and the protocol version is moving anyway.
- **Syncthing handling.** Vector clocks are core. `RESEARCH.md §3.1` is
  the implementation reference.

---

## Execution Order

Ordered from low-risk / high-leverage to high-risk / specialized. Each
group is safe to ship independently.

1. **C1** — one-line heuristic upgrade; stops sequence-mismatch false
   positives. No persistence change.
2. **C2** — ancestor-hash persistence; definitively correct for two
   devices. Unblocks C5 later.
3. **R1** — inode rename; biggest bandwidth win, wire-compatible via
   capability flag.
4. **P3sc** — adaptive watch/scan; unblocks big-folder rollout.
5. **PL** + **PM** — both low-risk incremental indexes; ship together.
6. **PN** — incremental `recomputeCache`; trivial once PL is in.
7. **PF** — trie ignore; gated on a conformance test suite against
   `git check-ignore`.
8. **P18c** — eliminate scan-time clone; depends on PL for deletion
   detection.
9. **PK** — COW on persist; only if measurement shows persist-time
   allocation still dominates after P18c.
10. **C3** + **C4** — per-block verify and multi-peer fallback; pair
    with R3 for consistent retry semantics.
11. **R2** — folder state machine; after R1 gives it something real to
    coordinate.
12. **R3** — peer blacklist; fits alongside C4.
13. **D3** — fanotify backend; opt-in, universal fallback stays.
14. **D6** — zstd index swap first, then per-transfer (flag-gated).
15. **D1 + D2 + C6** — if the protocol version has to move, fold them
    together. Otherwise defer individually.
16. **D4** — SQLite index; needs design doc + dependency approval
    first.
17. **D5** — sparse files; defer until a user workload needs it.

---

## Process

- Every item ships with tests that pin the behavior before the fix /
  feature change lands.
- After each batch, a separate reviewer pass across all modified files
  before considering the batch done.
- Mark items `✅` only after verifying against source (not commit
  message). Update the Verification table with the checked file /
  symbol.
- Remove `✅` items from this file only once no other document
  references them — the Verification table is the audit trail.
