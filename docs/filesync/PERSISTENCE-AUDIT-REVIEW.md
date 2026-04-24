# Persistence-Audit — Iteration-3 Critical Review

> Independent review of `PERSISTENCE-AUDIT.md` (iteration 2) against
> `DESIGN-v1.md` §4 and the live code in `internal/filesync/`.
> Planning phase — no code touched. Target bar: world-grade, zero data
> corruption, minimal resource use, ship once and forget.
>
> Status: draft, 2026-04-24.

---

## Legend

| Icon | Meaning                                                  |
|------|----------------------------------------------------------|
| 🚨   | Blocker — can corrupt data or break the cutover. Must resolve. |
| ⚠️   | Architectural concern — likely needs a design change or benchmark. |
| 🤔   | Decision needed — open question with non-obvious trade-off. |
| 🔧   | Obvious fix — no decision needed, document and go. |
| 💬   | Nit / clarity — small improvement. |

---

## Executive summary

| #    | Area                              | Status | Severity |
|------|-----------------------------------|--------|----------|
| A1   | Sequence counter under β          | 🚨     | Blocker  |
| A2   | Commit sequence has stale-read gap | 🚨     | Blocker  |
| A3   | Scan vs in-flight download race   | 🚨     | Blocker  |
| A4   | `.bak` file lifecycle             | 🚨     | Blocker  |
| A5   | BaseHash + `last_sync_ns` co-atomicity | 🚨 | Blocker  |
| A6   | `blocks` table — populate or drop? | 🤔    | Design   |
| A7   | `device_id` rotation mismatch     | 🚨     | Blocker  |
| A8   | `prev_path` consumption semantics | 🚨     | Blocker  |
| B1   | Per-scan `SELECT` may regress scan time | ⚠️ | Measure  |
| B2   | Reader handle deferred            | ⚠️     | Measure  |
| B3   | 24 backups × many folders = huge  | 🤔     | Policy   |
| B4   | `cache_size=-4000` per folder     | 💬     | Document |
| C1   | `PRAGMA mmap_size`                | 🤔     | Design   |
| C2   | `PRAGMA optimize` on close        | 🔧     | Add      |
| C3   | `ctx` cancellation in modernc     | 🔧     | Verify   |
| C4   | Fault-injection driver wiring     | 🤔     | Design   |
| C5   | `SQLITE_FULL` on checkpoint       | 🔧     | Document |
| C6   | Dirty-set unbounded on disk-full  | 🤔     | Policy   |
| C7   | Tombstone GC policy in SQLite     | 🤔     | Policy   |
| D1   | `fmt.Sscanf` silently eats errors | 🔧     | Fix      |
| D2   | `saveIndex` naming collision      | 🔧     | Rename   |
| D3   | `BEGIN IMMEDIATE` inside open tx  | 🔧     | Fix      |
| D4   | Metric label cardinality          | 🔧     | Enum-ize |
| D5   | `TestOpen_SynchronousIsFULL` + journal_mode | 🔧 | Extend |
| D6   | `SetConnMaxLifetime(0)` on daemon | 🔧     | Cap      |
| D7   | Late CRC on VectorClock           | 🔧     | Reorder  |
| D8   | Missing `SIGKILL` recovery test   | 🔧     | Add      |

**Counts:** 🚨 7 · ⚠️ 2 · 🤔 7 · 🔧 10 · 💬 1.

---

## A. Critical blockers — must resolve before code

### 🚨 A1 · Sequence counter is not safely mutable under β

**Current model.** `fs.index.Sequence++` under `fs.indexMu.Lock()`. Every
mutation — scan, download, rename, delete — takes the single lock,
reads, increments, writes.

**β model.** `FileIndex` does not exist between scans. The counter lives
in `folder_meta.sequence`. INV-3 says "every write is sequence-
conditioned" but never says how each write *picks* its sequence.

**Failure mode.**

```
t0: scan loads folder_meta.sequence = 100
t1: download D1 reads    folder_meta.sequence = 100 (tx not yet begun)
t2: scan walks; marks 10 paths dirty, assigns 101..110 in memory
t3: D1 begins tx, reads 100, writes row P_d with seq=101, commits 101
t4: scan begins tx, writes rows with seq=101..110, commits

Result: two different paths carry sequence=101.
```

**Why it matters.**

| Consequence                                   | Impact                         |
|-----------------------------------------------|--------------------------------|
| Peer delta `WHERE seq > ?` returns both rows  | Wastes bandwidth, not wrong.   |
| `seqIndex` binary search returns duplicates   | Breaks P18d invariant.         |
| Ack-based peer pruning assumes `seq` uniquely identifies an event | Could mis-ack. |

**Sequence-guard** (`excluded.sequence > files.sequence`) **does NOT
catch this** — it guards *same-path* overwrites, not global sequence
collisions.

**Required decision.** Sequences must be assigned inside the commit
transaction, not during scan:

```
BEGIN IMMEDIATE
  S := SELECT sequence FROM folder_meta
  assign S+1..S+N to dirty rows (deterministic order)
  UPSERT rows
  UPDATE folder_meta SET sequence = S+N
COMMIT
```

Same discipline for non-scan writes. Equivalent to SQLite's
`AUTOINCREMENT`, but we need per-folder scoping; per-folder DB gives
that, so AUTOINCREMENT is also viable.

**Action:** name this explicitly in the audit. Without it, INV-3 is
unreachable.

---

### 🚨 A2 · Commit 4 reads from an empty SQLite

**Audit's sequence.**

| Commit | What changes                       |
|--------|------------------------------------|
| 4      | Peer-facing *reads* → SQLite        |
| 5      | Non-scan *writes* → SQLite          |

**Verified against repo.** Only `*_test.go` files call
`saveIndex`/`savePeerStatesDB`/`loadIndexDB`. Production still uses
gob everywhere.

So between commit 4 and commit 5:
- Admin UI, peer exchange, dashboard → SQLite → **empty**.
- Persist path → still gob → not visible to readers.
- Every folder looks empty from the outside.

The audit claims "still green at every step." It will not be.

**Fix.** Insert a dual-write step. Proposed reshape:

```
C2  : dirty-set plumbing (gob only)
C3  : FolderDisabled + two-phase integrity check
C3.5: ★ dual-write in persistFolder (gob + SQLite).
      Gob remains authoritative; SQLite is a mirror.
      Test: assert loadIndex() and loadIndexDB() agree after each persist.
      Run in production for N days as low-risk verification window.
C4  : switch reads to SQLite
C5  : non-scan writes → SQLite (sync-persist)
C6  : β finish
C7  : retire gob
...
```

Given the zero-data-corruption bar, the dual-write verification window
is the single cheapest insurance available.

---

### 🚨 A3 · Download rename vs concurrent scan sees new bytes

**INV-4 download pattern.**

```
  1. rename original → .bak
  2. rename temp → original       ◀── window opens
  3. commit SQLite row
  4. unlink .bak                  ◀── window closes
```

**Between steps 2 and 3**, path on disk has *new* content; SQLite still
carries the *old* row. If `runScan` walks during that window:

```
scan: fast-path sees mtime changed → re-hashes → computes new hash
scan: writes row with scan's Version vector (local-bumped)
download: commits row with adopt-remote Version vector

sequence-guard picks the higher-seq write
→ semantically wrong VectorClock on the winning row
→ next C6 classification may misattribute
```

Not a data-loss bug. But for a zero-corruption bar, the VectorClock
semantics matter — `local-bumped` and `adopt-remote` mean different
things to the next diff.

**Fix.** Extend `claimPath`/`releasePath` so the scan's fast-path
skips paths currently claimed by an in-flight download. Pin with a
test that runs a slow download and a scan side-by-side.

---

### 🚨 A4 · `.bak` files have no lifecycle owner

Three-step pattern creates `<path>.bak`. The audit says `cleanTempFiles`
(R7) is kept — but `cleanTempFiles` matches `.mesh-tmp-*` and
`*.mesh-delta-tmp-*`, not `.bak`.

**Crash windows.**

| Crash after step | On-disk state         | Correct recovery                    |
|------------------|-----------------------|-------------------------------------|
| 1                | `.bak` only           | Restore `.bak → original`.          |
| 2                | `.bak` + new original | If SQLite row matches new → unlink `.bak`. Else restore `.bak`. |
| 3                | `.bak` + new original | Unlink `.bak` (commit succeeded).   |

**Fix** (no decision needed):

1. Rename the pattern to `.mesh-bak-<hash>` so the builtin ignore +
   existing temp sweep picks it up.
2. Add an explicit startup sweep that reconciles `.bak` against the
   SQLite row for that path.
3. Document in the file-format stanza of DESIGN-v1.

---

### 🚨 A5 · BaseHash and `last_sync_ns` must land in the same tx

**New rule** (INV-4): "Absence of BaseHash means conflict, never C1
fallback. C1 only used when we have positive knowledge of no prior
sync" — presumably `last_sync_ns == 0`.

**Failure mode if the two writes are split:**

```
t0: tx1 writes last_sync_ns = NOW, commits
t1: process crash before BaseHashes commit
---
restart
---
t2: diff() sees last_sync_ns > 0   → NOT first-sync
t3: diff() sees BaseHash missing   → classify as conflict
→ spurious .sync-conflict-* on every path
```

The audit implies single-tx but never spells it out. H12 names the
test but does not pin the co-atomicity in the audit's prose.

**Fix.** State explicitly in INV-4: BaseHashes row(s) and the
`last_sync_ns` update ride in **one** `BEGIN IMMEDIATE` per sync
outcome. Add the assertion to the H12 test.

---

### 🚨 A7 · `device_id` rotation goes undetected

**Current `seedFolderMeta` logic** (from `index_sqlite.go:192`):

```go
UPDATE folder_meta
SET    value = ?
WHERE  key = 'device_id' AND value = ''
```

**Backfills only when empty.** If the node-level `device-id` file is
rotated (manually, corruption-regenerate), `folder_meta.device_id`
silently keeps the *old* value. From now on:

- every local-bump uses the new device_id (VectorClock entry with new key)
- every peer exchange claims we are the new device_id
- but `folder_meta.device_id` (read elsewhere) disagrees

Subtle; hard to diagnose.

**Fix.** On open:

```
if file_id != folder_meta.device_id:
    enter FolderDisabled("device_id mismatch: file=X, db=Y")
```

Do not silently overwrite.

---

### 🚨 A8 · `prev_path` becomes permanently sticky in SQLite

**Today.** `PrevPath` on `FileEntry` is a single-use transient hint,
cleared on the next rescan.

**In SQLite.** It is a column. If `buildIndexExchange` serializes it
unconditionally, every delta re-sends a stale rename hint forever.
Receivers either misapply or no-op — neither is what we want.

**Fix options:**

| Option | Description | Trade-off |
|--------|-------------|-----------|
| (a) Clear in same tx | On the commit that sends the hint, also set `prev_path=NULL`. | Requires "have we sent this yet" tracking. |
| (b) One-shot counter | Add `hint_seq INTEGER`; receiver records consumed hint_seq. | More state, more correct across retries. |
| (c) Clear on next rescan | Mirror today's behavior: `prev_path=NULL` on any row update. | Simplest; may re-fire if the hint is seen twice before rescan. |

**Recommendation (mine).** Option (c). The semantic matches today's
shipped behavior and the audit's goal ("SQLite is the store, not the
semantics change"). Pin with a test: two consecutive delta exchanges
to the same peer — second one has `prev_path` unset for the same
path.

---

## B. Architectural push-back

### ⚠️ B1 · Per-scan `SELECT` may regress scan wall-time

**Today.** `cloneInto` on 100k entries: **7 ms / 0 allocs** (Apple M1
Max, measured).

**Proposed β.** `SELECT ... FROM files WHERE folder_id=?` at each scan
start. Audit hand-waves "~100 ms for 168k rows."

**Reality check.**

| Factor                               | Impact |
|--------------------------------------|--------|
| `modernc.org/sqlite` is pure Go      | 2–4× slower than CGo SQLite. |
| Row has 14 columns incl. 2 BLOBs     | Higher decode cost per row. |
| `database/sql` pool size = 1         | No parallel reads. |
| Realistic throughput                 | 30–80 ms for 168k rows. |

Conclusion: **4–10× slower per scan than today**, amplified by tight
watch-triggered cadence.

**The audit's justification** — "dwarfed by the filesystem walk and
hash phase" — only holds for cold full scans. Hot watch-triggered
scans on subtrees may walk in tens of ms; the SELECT then dominates.

**Deeper point.** β is sold as "eliminating split-brain by
construction." But `cloneInto` already provides an atomic snapshot
discipline. Gap 1 (concurrent scan+download race) is solvable with
**tighter sequence discipline on the existing in-memory model** (via
A1 and A3). β pays a real per-scan cost to solve a problem that is
also solvable without β.

**Hybrid to consider.**

| Goal                         | Model                                  |
|------------------------------|-----------------------------------------|
| Peer-visible truth (INV-1)   | SQLite only.                            |
| Scan's internal working copy | In-memory (nanosecond reads).           |
| Commit                       | Sequence-assigned in tx (A1).           |
| Admin/dashboard              | SQLite + summary cache.                 |

You keep the 7 ms floor; you keep the correctness gain; you only
relax "discard after scan" for a perf-sensitive private snapshot.

**Action.** Before committing to β, run
`BenchmarkLoadIndex_168kFiles` on modernc. If ≥150 ms, reconsider
INV-2. The §4.4 target of ≤500 ms is far too lenient given what
`cloneInto` already delivers.

---

### ⚠️ B2 · Reader handle decision should not be deferred

**Current wiring.** `SetMaxOpenConns(1)` + `SetMaxIdleConns(1)`.

**Post-cutover traffic pattern.**

```
15 folders × 2–3 peers × (index exchange + delta + blocksigs)
+ admin UI polling /api/filesync/*
+ dashboard refreshes
+ fs scan commits
= many concurrent reads serialized behind a single writer conn
```

"Defer until measured regression" leaves the production path with a
latent bottleneck on cutover day.

**Proposal.** Ship with two handles:

| Handle | DSN flags                                          | MaxOpen |
|--------|----------------------------------------------------|---------|
| writer | default                                            | 1       |
| reader | `?_pragma=query_only(true)&mode=ro&_txlock=deferred` | `n_peers + 3` |

Cost: ~150 LOC + one benchmark. Benefit: no "peers got slower after
the cutover" post-mortem.

**Recommendation (mine).** Add this at commit 4, before peer reads
flip. If the benchmark says one handle suffices, trim the reader out
— but build it first.

---

### 🤔 B3 · 24 backups × many folders violates the "least resource" bar

**Math.**

| Variable                | Value       |
|-------------------------|-------------|
| DB size (168k files)    | 100–200 MB  |
| Backups retained        | 24          |
| Per-folder storage      | 2.4–4.8 GB  |
| 15 folders              | 36–72 GB    |

**Options.**

| Option | Retention policy                                   | Storage (est.) |
|--------|----------------------------------------------------|----------------|
| (a) Audit's default — 24 rolling                    | 36–72 GB       |
| (b) GFS: 5 recent + 4 weekly + 1 monthly           | 10 × size      |
| (c) Time-bounded: 24 h × N / 7 d × M / 30 d × K    | Tunable        |
| (d) Size-capped: retain until total backup size > X GB | Predictable |

**Recommendation (mine).** Option (b) as the default, option (d) as
an operator override (`max_backup_bytes` in config). Rationale:

- GFS covers the common recovery horizons (last-mile, yesterday,
  last week, last month).
- Size cap protects against a pathological folder eating the disk.
- Both land on the backup scheduler already in commit 9.

---

### 💬 B4 · `cache_size=-4000`

4 MB × 15 folders = 60 MB. Fine on desktop, borderline on a low-end
device. Document the trade-off in DESIGN-v1 §4; do not add a knob.

---

## C. Missing decisions

### 🤔 A6 · `blocks` table — populate or drop?

`index_sqlite.go` already creates `CREATE TABLE IF NOT EXISTS
blocks(folder_id, path, offset, length, hash, PRIMARY KEY(...))`.
No production code references it. The audit never mentions it.

**Options.**

| Option | Description | Pros | Cons |
|--------|-------------|------|------|
| Populate | Scan writes FastCDC boundaries + hashes. `handleBlockSigs` reads them. | Saves O(filesize) disk re-reads per block-sig request. Matches Syncthing's model. | ~15 % more row writes per scan. Another table in the migration story. |
| Drop    | Delete the table. Block sigs computed on demand (today). | Simpler. Fewer rows to write/compaction. | Per-request cost unchanged. |

**Recommendation (mine).** Drop for v1. Reopen only if block-sig
request latency pressures the system. Rationale:

- Today's on-demand compute is bounded by file read speed, which is
  usually already in page cache on the sender.
- Populating adds O(N blocks) rows per file; on a 10 GB file with
  128 KB avg, that's ~80k rows *per file*, quickly bloating the DB.
- One less dead table in the v1 bundle reduces the cutover surface.

Decide and name in the audit.

---

### 🤔 C1 · `PRAGMA mmap_size`

Not discussed. mmap-backed reads substantially speed up hot query
paths on WAL-mode SQLite.

**Options.**

| Value    | Effect |
|----------|--------|
| unset (0) | Default — no mmap. Most portable, slowest reads on large DBs. |
| 64 MB    | Good default; caps virtual memory use. |
| 256 MB   | Aggressive; large VA footprint per folder. |

**Recommendation (mine).** 64 MB. Apple Silicon / Linux desktops
have ample VA; Windows is fine. For 15 folders, VA reservation is
960 MB but resident memory is bounded by actual page touches
(typically << the DB size).

---

### 🔧 C2 · `PRAGMA optimize` on close

Recommended by SQLite project for query-plan freshness. Add to
`fs.db.Close()` shutdown path. One line.

---

### 🔧 C3 · Verify `ctx` cancellation in modernc

`modernc.org/sqlite` has had historical quirks with `ctx` propagation.
Before L1 (shutdown deadline) ships, run `TestBeginTx_CtxCancelReleasesLock`
on the pinned driver version. If the lock leaks, upgrade or surface.

---

### 🤔 C4 · Fault-injection driver wiring

Audit says "wrapping `driver.Driver` lives in `_test.go`." Two
wiring choices:

| Option | Description | Notes |
|--------|-------------|-------|
| (a) Register as separate name `sqlite_faulty` at init in `_test.go`. Tests open with that DSN. | Clean separation; zero production surface. | Plumbing — `openFolderDB` needs to accept a driver name param. |
| (b) Unexported global hook (`var testDriverWrap func(driver.Driver) driver.Driver`). Tests set it at `TestMain`. | No plumbing. | Global mutable state in tests. |

**Recommendation (mine).** (a). Plumb `driverName` through
`openFolderDB` as an argument with production default `"sqlite"`.
The test-only constant `testDriverName = "sqlite_faulty"` registers
in an `_test.go` file.

---

### 🔧 C5 · `SQLITE_FULL` on checkpoint

Not a correctness hazard (next write sees the error first), but
document: the writer will see `SQLITE_FULL` on its next commit, which
transitions the folder to FolderDisabled. Checkpoint itself doesn't
propagate the error.

---

### 🤔 C6 · Dirty-set unbounded on sustained disk-full

Every scan adds to the dirty-set; commit keeps failing; set grows
without bound.

**Options.**

| Option | Behavior | Trade-off |
|--------|----------|-----------|
| (a) Cap at N paths (e.g. 10k); on exceed → FolderDisabled | Bounded memory; manual recovery. | Operator sees folder stop, must fix disk then restart. |
| (b) Drop oldest on exceed | Memory bounded; scan retries rebuild. | Silent data delay; hard to reason about. |
| (c) Keep growing | Simple. | OOM risk on a long-running disk-full daemon. |

**Recommendation (mine).** (a) with N=50_000. Matches the spirit of
"fail loud, keep other folders running." Metric
`mesh_filesync_folder_disabled{reason="dirty_set_overflow"}` makes
it visible.

---

### 🤔 C7 · Tombstone GC policy in SQLite

Today: `purgeTombstones` prunes by age after each scan. In SQLite,
tombstones are rows with `deleted=1`. Audit is silent on pruning.

**Options.**

| Option | Behavior |
|--------|----------|
| (a) Mirror today: `DELETE WHERE deleted=1 AND mtime_ns < now - tombstoneMaxAge` after every Nth scan. | Simplest; row count stays bounded. |
| (b) Never delete; rely on `WHERE deleted=0` in hot queries. | Indexes bloat forever. |
| (c) Shift tombstones to a separate `tombstones` table. | Smaller hot table; more schema complexity. |

**Recommendation (mine).** (a) with N=10 (every 10th scan). Matches
shipped semantics; keeps the hot table lean.

---

## D. Small obvious fixes

### 🔧 D1 · `fmt.Sscanf` eats parse errors silently

```go
// index_sqlite.go:710
func parseInt64(s string) int64 {
    var n int64
    _, _ = fmt.Sscanf(s, "%d", &n) // ← eats errors
    return n
}
```

If `folder_meta.sequence` is garbage, we silently read `0` and start
writing sequences from `1`. Sequence-guard rejects them because
existing rows have higher seqs → folder stops making progress.

**Fix.** Use `strconv.ParseInt`, propagate the error, treat failure
as DB corruption → FolderDisabled.

---

### 🔧 D2 · `saveIndex` name collision

- Function: `index_sqlite.go:275` — `func saveIndex(db, folderID, idx)`
- Local var: `filesync.go:2677` — `saveIndex := force || fs.indexDirty`

After the cutover, this will cause confusing shadowing. Rename the
local variable (e.g. `shouldSaveIndex`) when you touch persistFolder
for the dual-write.

---

### 🔧 D3 · `BEGIN IMMEDIATE` inside an open tx is broken

```go
tx, err := db.Begin()                  // DEFERRED
...
if _, err = tx.Exec(`BEGIN IMMEDIATE`); err != nil {
    // ...no-op comment...
}
```

`BEGIN` inside an existing tx errors in standard SQLite. The comment
hand-waves this.

**Fix (any of):**

- Open a dedicated conn: `conn, _ := db.Conn(ctx); conn.ExecContext(ctx, "BEGIN IMMEDIATE"); ...`
- Use modernc DSN option: `?_txlock=immediate` so `db.Begin()` emits
  `BEGIN IMMEDIATE`.

Delete the dead `tx.Exec("BEGIN IMMEDIATE")` block.

---

### 🔧 D4 · `mesh_filesync_folder_disabled{reason=...}` cardinality

If `reason` is free-form (`"disk full on /dev/sda1 at 12:34"`),
Prometheus series count explodes.

**Fix.** Map to a closed enum:

```
integrity_check_failed
quick_check_failed
device_id_mismatch
schema_version_mismatch
read_only_fs
disk_full
dirty_set_overflow
unknown
```

Log the full reason, label with the enum.

---

### 🔧 D5 · Extend `TestOpen_SynchronousIsFULL` to also pin journal_mode

Pinning `synchronous=2` without also asserting `journal_mode=wal` lets
a future refactor regress one without the other.

---

### 🔧 D6 · `SetConnMaxLifetime(0)` on a weeks-long daemon

`0` = connections never expire. Combined with pure-Go modernc, any
small connection-scoped leak compounds over weeks.

**Fix.** Cap at 24h (arbitrary but safe). Connection rotation is
cheap; leak containment is free.

---

### 🔧 D7 · VectorClock CRC lands at commit 10, format goes live at commit 6

Blob format without CRC ships at commit 6 (or earlier, given it is
already in `encodeVectorClock`). Waiting until commit 10 to add the
CRC means 4 commits of write paths that omit the trailer — any of
those paths can silently miss the later retrofit.

**Fix.** Add the CRC32 trailer to `encodeVectorClock` **before** any
production write path lights up. I.e. fold into commit 2 (FileIndex
encapsulation) or commit 6 (β finish) — but not commit 10.

---

### 🔧 D8 · Missing `SIGKILL` recovery test

§4.5 covers in-process restart; the fault-injection driver covers
SQLite-level commit failure. Not covered: OS-level `kill -9` with
dirty WAL.

**Fix.** One scripted e2e test:

```
1. launch node, run a scan that begins a tx
2. kill -9 the process
3. restart
4. assert: DB opens cleanly, quick_check passes, no data lost
   since the last committed tx
```

This is the one failure mode a daemon owner actually hits in the
wild (battery dies, OOM-killer, kernel panic). Worth the 50 LOC.

---

## E. Proposed cutover sequence (replacement for §6)

```
 1. this doc (no code)
 2. FileIndex encapsulation + dirty-set                     │ gob only
 3. FolderDisabled + two-phase integrity check              │ gob only
 4. ★ DUAL-WRITE in persistFolder (gob + SQLite mirror)     │ A2
    ─ test: loadIndex() == loadIndexDB() after every persist
    ─ burn-in period in production
 5. Peer-facing reads → SQLite (with summary cache; 2 DB handles per B2)
 6. Non-scan writes → SQLite (three-step + A5 co-atomic)
    ─ CRC32 on VectorClock blob LANDS HERE (not commit 10)
 7. β finish — sequence assignment inside commit (A1), FileIndex scan-local
 8. Retire gob + legacy refusal
 9. Shutdown deadline propagates to transactions
10. VACUUM INTO backups with B3 retention policy
11. Fault-injection driver + SIGKILL recovery test
12. Benchmarks with pinned baselines (+ the LoadIndex bench from B1)
13. Schema-evolution migration stub
14. DESIGN-v1 banner flip
```

Key differences from the audit's §6:

- 🆕 step 4 — dual-write verification window.
- ➡️  CRC moved from commit 10 to commit 6 (D7).
- ➡️  Reader handle from B2 lands at commit 5, not deferred.
- ➡️  B1 benchmark gates commit 7, not informational.

---

## F. What I need from you before code

| # | Question | Default (mine) |
|---|----------|----------------|
| A6 | Populate `blocks` table, or drop it for v1? | Drop. |
| A8 | `prev_path` consumption strategy: (a) clear-in-tx, (b) hint_seq, (c) clear-on-rescan. | (c) mirror today. |
| B1 | Run LoadIndex bench now, reconsider β if ≥150 ms? | Yes, mandatory. |
| B2 | Ship the reader handle at commit 5 (yes/no)? | Yes. |
| B3 | Backup retention policy: GFS? size-capped? | GFS + size cap. |
| C1 | `mmap_size`: 0 / 64 MB / 256 MB? | 64 MB. |
| C4 | Fault-injection wiring: plumbed driverName or unexported hook? | Plumbed. |
| C6 | Dirty-set overflow → FolderDisabled at what cap? | 50_000. |
| C7 | Tombstone GC cadence? | Every 10th scan, age-based. |

All 🚨 items (A1–A8) and 🔧 items need no input — they're fixes to
apply as part of the revised cutover.

---

## G. Meta-observation

The audit is strong at **enumerating invariants and mapping them to
tests**. It is weaker where it **asserts an outcome without tracing
the transition**:

- "SQLite is the sole source of truth" — but the sequence counter
  transition (A1) isn't traced.
- "Reads go to SQLite at commit 4" — but the write-side mirror
  (A2) isn't named.
- "Downloads are atomic via three-step" — but the `.bak` cleanup
  story (A4) isn't closed.
- "Absence of BaseHash means conflict" — but the co-atomic write
  (A5) isn't pinned.

The gaps cluster on exactly these transitions. Once A1–A8 are
resolved the iteration-3 audit should be ready for code.
