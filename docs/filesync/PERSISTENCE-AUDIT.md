# Persistence-Layer Audit — gob/YAML → SQLite Cutover

> Spec document for the D4 cutover commits. Fills the gap between
> `DESIGN-v1.md` §4 (the schema sketch) and the actual code changes.
> No code lands until this document is reviewed.
>
> Status: **draft** · last updated 2026-04-22.
>
> Scope: everything that today reads or writes a file under
> `~/.mesh/filesync/<folder-id>/` and the behaviors built on top of
> those files. The goal is to enumerate **every** hard-won invariant
> in the current persistence layer so the SQLite rewrite is a
> deliberate transition per invariant, not a mechanical port.
>
> This project has no backward-compatibility constraint: no prior
> peers run v1, and the operator wipes `~/.mesh/filesync/` before the
> first v1 start. The audit therefore focuses on **correctness,
> crash resilience, race handling, and performance** rather than
> migration.

---

## 1. How to read this document

Three working sections:

- **§2 Inventory** — every current behavior, with the code site and
  the disposition in the SQLite world (`keep` / `redesign` /
  `supersede` / `drop`).
- **§3 New-risk taxonomy** — categories of bug that the SQLite
  switch can introduce even when every §2 row is addressed. Captured
  preemptively per the principle that invalidating a listed risk is
  cheaper than discovering it in production.
- **§4 Test strategy** — per-behavior and per-risk test hooks. Every
  §2 row and every §3 category lands with a named test.

A companion section at the end derives the commit sequence from the
audit.

---

## 2. Inventory of current persistence behaviors

Dispositions:

| Tag         | Meaning                                                              |
|-------------|----------------------------------------------------------------------|
| `keep`      | Behavior carried over unchanged; SQLite does not address it.         |
| `redesign`  | Behavior preserved but implemented differently.                      |
| `supersede` | SQLite naturally provides the guarantee; no replacement code needed. |
| `drop`      | Workaround for a limitation the rewrite removes; delete.             |

### 2.1 On-disk format and atomicity

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| F1 | Double-write (`primary` + `.prev`) for crash-safe persist | `writeFileSync`, `prevPath`, `FileIndex.save`, `savePeerStates` | `supersede` | WAL + rollback journal give atomic commit per transaction. A torn commit rolls back automatically on reopen. |
| F2 | `tmp` + `fsync` + `rename` per write | `writeFileSync` | `supersede` | SQLite transactions ship with the same fsync-before-commit guarantee. |
| F3 | `gob` binary encoding for speed | `gobMarshalIndex`, `gobUnmarshalIndex` | `supersede` | Native SQLite row writes replace it. |
| F4 | YAML fallback on read for legacy folders | `tryLoadYAMLIndex`, `tryLoadPeerStates` | `drop` | No v1 peer ever ran the gob/YAML layer in production; legacy files are refused at startup. |
| F5 | Atomic peer-state double-write | `savePeerStates` | `supersede` | Same transaction as the index update — one commit covers both tables. |
| F6 | `isNotExist` first-run detection | `loadIndex`, `loadPeerStates` | `redesign` | SQLite presence means "db file exists." First-run now means "no `folder_meta` row with `created_at`" — explicit, not filesystem-derived. |

### 2.2 Recovery and corruption handling

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| R1 | Pick higher-sequence of primary/backup on load (H2a) | `loadIndex` | `drop` | No backup file exists. The DB is atomic. A failed transaction is invisible to the next reader. |
| R2 | Warn-and-continue on corrupted gob primary | `tryLoadGobIndex` | `redesign` → **fail loud, per-folder** | Current path silently loses data on corruption. SQLite `PRAGMA integrity_check` at open. On failure: disable the affected folder, surface it in the dashboard with a red status, log at `ERROR`, and increment a `mesh_filesync_folder_integrity_failed_total` metric. **Do not exit the process.** `mesh` has other components (SSH, proxy, clipsync, gateway) that must keep running; filesync is a subcomponent. |
| R3 | Rebuild empty index if both files unreadable | `loadIndex` default branch | `redesign` → **per-folder disable** | In the SQLite world, an unreadable DB is an operator problem; do not auto-wipe. The affected folder enters a `FolderDisabled` state with the failing reason attached. Other folders on the same node keep syncing; unrelated components are untouched. |
| R4 | Epoch regeneration on load when empty (H2b) | `loadIndex` | `drop` | Epoch is written once at `folder_meta` seed. No empty-on-load case. |
| R5 | Peer-state reset when index was recreated (B15) | Folder startup in `Run` | `drop` | Motivated by "silent gob fallback gave us an empty index, so peer `LastSentSequence` is now wrong." Failure mode goes away when we fail loud on open (R2, R3). |
| R6 | `prevPath` helper | `prevPath` | `drop` | No `.prev` files. |
| R7 | Abandoned download temp-file sweep | `cleanTempFiles` | `keep` | Orthogonal to index storage; runs before folder init. |
| R8 | Per-folder `FolderDisabled` state (new) | new `folderState` field + dashboard / metric surface | `redesign` → **new** | Failure classes R2, R3, F3, F5 all need a way to park a folder without blowing up the process. Introduce a folder-level disabled flag carrying a human-readable reason; the dashboard renders a red row, `/api/filesync/folders` reports `status: "disabled"` with the reason, and `mesh_filesync_folder_disabled{reason=...}` goes to 1. Folder stays disabled until the operator fixes the underlying issue and restarts the node. Every other folder and every other mesh component (SSH, proxy, clipsync, gateway) is untouched. |

### 2.3 Concurrency and locking

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| C1 | `persistMu` serializes concurrent `persistFolder` calls (N10) | `folderState.persistMu`, `Node.persistFolder` | `drop` | SQLite's writer lock already serializes. The mutex is redundant. Verify no other caller relies on `persistMu` for ordering of non-DB side effects. |
| C2 | Snapshot-under-RLock then persist outside (F1) | `Node.persistFolder` | `keep` | We still must not hold `indexMu` across a SQLite transaction. The clone-release-transact pattern stands. |
| C3 | `indexDirty` / `peersDirty` flags skip persist when unchanged (P17a) | `folderState.indexDirty`, `.peersDirty` | `keep` | Skip the `BEGIN`/`COMMIT` round-trip when nothing changed. Cheap and useful even with SQLite. |
| C4 | Reader queries (`/api/filesync/folders`, index exchange) take `indexMu.RLock` | `filesync.go` admin handlers, `protocol.go` index-exchange handler | `redesign` | Readers can go to SQLite directly via WAL snapshot isolation and stop taking `indexMu`. Simplifies lock hierarchy. Pin the boundary with a test that runs a slow scan transaction while a reader hits `/api/filesync/folders`. |
| C5 | Scan and sync coexistence (ref `filesync.go` R2-cancelled invariant) | — | `keep` | Unchanged. |

### 2.4 Query shapes

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| Q1 | Secondary sequence index for delta exchange (PG) | `FileIndex.seqIndex`, `rebuildSeqIndex` | `redesign` | `CREATE INDEX files_by_seq ON files(folder_id, sequence)` makes `WHERE sequence > ?` cheap. Retire the hand-rolled slice; rebuild call goes away. |
| Q2 | Inode-keyed rename lookup (R1 Phase 2) | `scanWithStats` inode map | `redesign` | Schema already has `files_by_inode` partial index. Scan uses `SELECT path FROM files WHERE folder_id=? AND inode=?` instead of an in-memory map. |
| Q3 | Cached active count / size (P18b) | `FileIndex.cachedCount`, `activeCountAndSize` | `keep` | Still maintained in-memory; recompute on load. Avoids a `COUNT(*)` on every dashboard read. |
| Q4 | Admin dashboard per-path listing | `/api/filesync/folders` handler | `redesign` | Goes directly to SQLite. No intermediate `FileIndex.clone()`. |

### 2.5 Performance invariants

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| P1 | Dirty-flag short-circuit skips full persist (P17a) | `persistFolder` gate | `keep` | §2.3 C3 duplicate; listed here because it is also a perf item. |
| P2 | Gob binary write for `~30 MB` index in a few hundred ms | `FileIndex.save` | **`redesign` — headline risk** | My step-c helpers do `DELETE WHERE folder_id=?` + `INSERT` for every file row on every persist. On the 168k-file local folder, that is 168k INSERTs per cycle. Target: persist only changed rows. **Per-path dirty-set design below.** |
| P3 | Scan allocates ~0 B via `cloneInto` recycling (P18c) | `FileIndex.cloneInto` | `keep` | Unchanged by the storage swap. Scan still mutates an in-memory `FileIndex`. Persistence is downstream. |
| P4 | `buildIndexExchange` uses `seqIndex` for O(log N) delta-from-sequence | `filesync.go` index exchange | `redesign` | Replaced by Q1 / an indexed SQL query. |
| P5 | P18b O(1) counters avoid scan on every API call | `cachedCount`/`cachedSize` | `keep` | Unchanged. |

#### Per-path dirty-set — the P2 redesign

Problem: scan may change 3 files out of 168k; current gob path writes
the whole blob; my step-c SQLite helper does `DELETE+INSERT` of all
168k rows. Both waste work. The gob path is wasteful but fast because
serialization is linear and I/O is one sequential write. The naive
SQLite path is wasteful **and** slow because every row is a separate
index-maintenance operation.

Design:

- `folderState.dirtyPaths map[string]struct{}` — the set of paths
  touched since the last successful persist. Populated by
  `setEntry` and by any other mutation path that currently flips
  `indexDirty`.
- `folderState.deletedPaths map[string]struct{}` — paths removed
  outright (not just `Deleted=true` tombstones, which stay in
  `Files`; this is for the rare hard-remove case, e.g. after a
  tombstone garbage-collection pass).
- `persistFolder` writes only:
  - `INSERT OR REPLACE INTO files(...)` for each path in
    `dirtyPaths ∩ Files`.
  - `DELETE FROM files WHERE folder_id=? AND path=?` for each path
    in `deletedPaths`.
  - The folder-meta scalars (`sequence`, `epoch`, `fs_device_id`).
- On success, clear both sets inside the same critical section
  that flipped `indexDirty=false` / `peersDirty=false` today. If
  the transaction fails, the sets stay populated and the next
  cycle retries exactly the same work.

Peer-state table mirrors: `peersDirtyIds map[string]struct{}`. Same
shape.

Benchmark gate before the cutover ships: `BenchmarkPersist_168kFiles_3Dirty`
with a target of ≤ 50 ms per cycle. The comparable gob run is
currently in the ~500 ms range for full writes; incremental SQLite
should be materially faster because it touches three rows, not 168k.

### 2.6 Identity and metadata

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| I1 | Device ID loaded from `~/.mesh/filesync/device-id` | `loadOrCreateDeviceID` | `keep` | Orthogonal to folder DB. Continues to own the 10-char Crockford base32 identity file. |
| I2 | Folder-level `Epoch` regenerated on re-create | `newFileIndex` | `redesign` | Epoch is seeded once in `folder_meta` and never rewritten by the binary. Operator can delete the DB to force a new epoch. |
| I3 | Folder `Path` stored on index, warns on drift | `loadIndex` path-change check | `keep` | Store as `folder_meta.path`. Warning stays. |
| I4 | G3 filesystem device id on `FileIndex.DeviceID` | `FileIndex.DeviceID` | `keep` | Stored as `folder_meta.fs_device_id`; already wired in step c. |
| I5 | C6 per-file `VectorClock` | `FileEntry.Version`, `encodeVectorClock` | `keep` | Already round-trips through step c. |
| I6 | C2 per-peer `BaseHashes` per-path map | `PeerState.BaseHashes` | `keep` | Already round-trips through peer-state helpers. |

### 2.7 Lifecycle hooks

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| L1 | `persistAll(force=true)` at shutdown | `Node.persistAll` | `keep` | Still runs one final transaction per folder. |
| L2 | `fs.root.Close()` on shutdown | `Run` shutdown tail | `keep` | Add `fs.db.Close()` beside it. |
| L3 | `persistFolder(force=true)` after scan | `runScan` tail | `keep` | Becomes a scheduled `BEGIN`/`COMMIT`. |
| L4 | Admin backup currently copies gob bytes | n/a (planned) | `redesign` → `VACUUM INTO` | Per `DESIGN-v1.md` §4. Lands as its own commit after cutover. |

---

## 3. New-risk taxonomy — bugs the rewrite can introduce

The §2 audit guards against losing a behavior. This section guards
against the **new** bugs SQLite brings. Some rows will turn out to be
non-issues; that is fine — the purpose is to decide each one
explicitly rather than discover it later.

### 3.1 Driver risks (`modernc.org/sqlite`)

| # | Risk | Assessment plan |
|---|------|-----------------|
| D1 | Pure-Go port's behavior diverges from upstream SQLite in a corner case | Pin the driver version; upgrade only with a changelog read; property-test the round-trip against a recorded corpus. |
| D2 | Windows path handling (backslash, long paths, case-insensitive FS) | Test on Windows CI with a folder path containing spaces and non-ASCII characters. |
| D3 | Resource leak on `Close()` with outstanding rows | Every `Query` site paired with `defer rows.Close()`; enforce with a linter rule (add to `staticcheck` config). |
| D4 | Context cancellation mid-transaction leaves a dangling lock | Use `db.BeginTx(ctx, ...)`; assert lock release by opening a second writer right after. |
| D5 | Driver bug corrupts blob column (VectorClock / Hash256) | Checksum every blob on the way out in `loadIndexDB` tests for the first release. Catch early. |

### 3.2 Transaction-semantic risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| T1 | `sql.Tx` default is `DEFERRED`; two goroutines both enter a write tx and one receives `SQLITE_BUSY` on first write | Always use `BEGIN IMMEDIATE` for write transactions; tests assert no `SQLITE_BUSY` under concurrent writers. |
| T2 | `Tx.Commit` after error leaves DB in a half-state | Every write path uses the `defer func() { if err != nil { _ = tx.Rollback() } }()` idiom; covered by a crash-in-mid-transaction test. |
| T3 | Forgetting to wrap a multi-statement write in a tx (autocommit) | Code review checklist; one test that asserts `persistFolder` emits exactly one `BEGIN` per call (measure via `PRAGMA query_only` / driver hook). |
| T4 | `SQLITE_BUSY` retry loops create goroutine storms | Set `db.SetMaxOpenConns(1)` for writers; readers use separate `*sql.DB` if needed (not yet decided). |
| T5 | Writer tx held across a goroutine boundary leaks a connection | Never pass `*sql.Tx` between goroutines. Tests: none required — enforced by code review. |
| T6 | Reading from an `sql.Tx` that has been `Commit`ed | `defer tx.Rollback()` is a no-op after commit; idiom is safe. Documented. |

### 3.3 WAL-specific risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| W1 | `-wal` and `-shm` files exist alongside the main DB; operators copying the main file alone get inconsistent backups | `VACUUM INTO` is the sanctioned backup. Document in operator notes. |
| W2 | WAL grows unbounded when no checkpoint runs | `PRAGMA wal_checkpoint(TRUNCATE)` after each scan cycle; test that WAL size stays bounded over many cycles. |
| W3 | Checkpoint blocks readers briefly | Not a correctness risk; acceptable perf. Measure under load. |
| W4 | WAL on a filesystem that does not support it (older NFS, some Windows SMB mounts) silently degrades | `PRAGMA journal_mode=WAL` returns the actual mode; we already assert `"wal"` in `applyFolderDBPragmas`. Keep that assertion. |
| W5 | `synchronous=NORMAL` is weaker than `FULL` — power loss mid-commit can roll back the last committed tx | **Reject `NORMAL`. Use `FULL`.** Data-corruption risk is not acceptable for a sync tool whose entire value proposition is not losing user files. The perf cost of `FULL` is one extra fsync per commit, which the dirty-flag short-circuit (C3) already makes rare. DESIGN-v1 §4's `NORMAL` choice is overridden; the banner-flip commit documents the departure. |

### 3.4 Data-type and encoding risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| E1 | SQLite type affinity quietly coerces values (`INTEGER` column accepting `"42"` as text) | Use explicit binds; property test round-trips all fields. |
| E2 | `uint64 → INTEGER` overflow for values > 2^63 | `Inode` and `fs_device_id` are `uint64` in Go. Practically these fit in 63 bits; note the bound. Tests include `math.MaxInt64`. |
| E3 | `time.Time` precision truncation on `UnixNano` round-trip | We store `int64` nanos; truncation ceiling is year 2262. Acceptable. Test includes a timestamp with non-zero nanos. |
| E4 | `nil` vs empty byte slice ambiguity through `sql.NullX` | `nullIfEmpty` / `nullIfZero` helpers. Property test explicitly exercises both. |
| E5 | `VectorClock` encoding drift between Go versions (map iteration order) | `encodeVectorClock` sorts by device-id; `TestEncodeVectorClock_DeterministicOrder` pins it. Already done. |
| E6 | Empty string vs `NULL` confusion on `last_epoch` / `pending_epoch` | `nullIfEmptyString` at write; `sql.NullString` at read. Test round-trips both values. |

### 3.5 Schema and query risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| S1 | Missing index → full-table scan on hot query | `EXPLAIN QUERY PLAN` in tests for every production query. Fail if any expected query hits `SCAN TABLE`. |
| S2 | `INSERT OR REPLACE` changes ROWID (it's a DELETE + INSERT internally) | We do not rely on ROWID. Noted. |
| S3 | Partial-index predicate mismatch (`WHERE inode IS NOT NULL` on the index; query without predicate) | Test that `Q2` inode lookup plan uses `files_by_inode`. |
| S4 | Too many indexes slow writes | Three indexes today (`files_by_seq`, `files_by_inode`, implicit PK). Monitor the per-path UPSERT cost in the benchmark. |
| S5 | Collation default is BINARY; path comparison is case-sensitive even on case-insensitive filesystems | Current gob/YAML behavior is also case-sensitive. No change. |
| S6 | `CAST(... AS INTEGER)` vs `CAST(... AS TEXT)` ambiguity on `folder_meta.value` (stored as `BLOB`) | Explicit CAST in every read. Already done. |
| S7 | Schema extension later needs `ALTER TABLE ADD COLUMN` | `modernc.org/sqlite` supports ≥ 3.35 behavior. Future column adds are cheap; column **drops** require rebuild. Plan future migrations to only add. |

### 3.6 Concurrency risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| N1 | Two goroutines share one `*sql.Tx` and race | Never do this. Enforced by convention. |
| N2 | Closing the DB while another goroutine holds rows → crash | `fs.db.Close()` only after `wg.Wait()` on the shutdown path. |
| N3 | `database/sql` pool opens multiple connections; each sees its own view of in-flight tx | We set `SetMaxOpenConns(1)` today. Revisit if reader concurrency matters; may want a separate read-only `*sql.DB` handle. |
| N4 | Reader during writer commit sees an inconsistent intermediate view | WAL snapshot isolation prevents this. Test pins it. |
| N5 | Scan transaction runs longer than intended and blocks the next scan | Bound by scan duration; same as today but now visible as a tx. Benchmark. |

### 3.7 Filesystem and platform risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| F1 | DB file on a network filesystem (NFS, SMB) | Documented as unsupported. Same stance as Syncthing. |
| F2 | Symlinked data directory | `os.MkdirAll` + `sql.Open` both follow symlinks. No special handling needed. |
| F3 | Read-only filesystem | `sql.Open` succeeds; first write fails. Surface a folder-level error at open, enter the same `FolderDisabled` state used for R2 / R3; other folders and other mesh components keep running. |
| F4 | File permissions of the DB file | We `MkdirAll` at `0700`; modernc default is `0644`. Needs explicit `Chmod 0600` after open or a DSN flag. |
| F5 | Disk-full mid-commit | SQLite returns `SQLITE_FULL`; transaction rolls back. Surface as a folder-level error in the dashboard. |
| F6 | Case-insensitive filesystem (macOS default, Windows) | Same as S5. No change. |

### 3.8 Testing-infrastructure risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| X1 | `t.TempDir` cleanup misses `-wal` / `-shm` sidecars on failure | `t.TempDir` is recursive; sidecars go with it. Tested by inspecting the dir after a Cleanup. |
| X2 | Parallel tests (`t.Parallel`) sharing a temp dir | Every test uses its own `t.TempDir`. Enforced by code review. |
| X3 | Fault injection (simulate disk full, torn commit) | `modernc.org/sqlite` does not expose fault injection. Use a wrapping `driver.Driver` that injects errors on demand. Needed only for the crash-resilience tests; scoped to `_test.go`. |
| X4 | Flaky tests from leftover open handles | `defer db.Close()` + `defer rows.Close()` everywhere. |

### 3.9 Security risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| Y1 | SQL injection via a path or peer id | Parameterized queries everywhere; no string concat. Enforced by code review. Tests: one that passes a path containing `'; DROP TABLE files; --` and asserts it round-trips intact. |
| Y2 | DB contains per-path ancestor hashes; world-readable file leaks folder contents structure | Chmod `0600` on the file (F4). |
| Y3 | `VACUUM INTO` backup inherits mode `0644` by default | Chmod `0600` on the backup file; add to the backup handler. |
| Y4 | Log lines emit raw `BLOB` contents on error | Never log blobs verbatim; log lengths and hashes instead. |

### 3.10 Schema-evolution risks

| # | Risk | Assessment plan |
|---|------|-----------------|
| V1 | Next schema change lands with no migration path thought through | Commit a stub `migrate(db, fromVersion, toVersion)` function with the cutover. v1→v2 wiring exists as a no-op to prove the path. |
| V2 | `schema_version` read as INTEGER fails if a future writer stores it as TEXT | `CAST(value AS INTEGER)` on read. Already done. |
| V3 | Adding a `NOT NULL` column later requires rewriting every row | Future migrations use `ADD COLUMN ... DEFAULT ...` to stay cheap. Documented convention. |

---

## 4. Test strategy

### 4.1 Per-§2-row behavioral tests

Every row in §2 with disposition `keep` or `redesign` pairs with a
named test that asserts the behavior in the SQLite world. Tests live
at the `persistFolder` / `loadFolder` boundary wherever possible, so
they survive a future storage change.

| §2 row | Test |
|--------|------|
| F1 atomicity | `TestPersist_CrashMidCommitRollsBack` — fault injection wrapper aborts the tx; reopen; assert pre-commit state. |
| F4 legacy refusal | `TestOpen_RefusesLegacyGobFile` — touch `index.gob`; open returns the typed legacy error. |
| R2 fail-loud on corruption | `TestOpen_FailsIntegrityCheck` — corrupt the file after close; reopen returns error and the folder enters `FolderDisabled`. |
| R8 per-folder disable | `TestFolderDisabled_IsolatesFailure` — force R2 on folder A; assert folder B keeps syncing, SSH tunnels stay up, dashboard shows A as disabled with reason, metric is 1. |
| W5 synchronous=FULL | `TestOpen_SynchronousIsFULL` — read `PRAGMA synchronous` after open; assert the integer value is 2. |
| C3 dirty-flag short-circuit | `TestPersist_SkipsWhenClean` — run persist twice without mutation; assert the second call issues no `BEGIN` (count via driver hook). |
| C4 reader during writer | `TestReaders_SeeSnapshotDuringWriteTx` — goroutine A holds an IMMEDIATE tx; goroutine B runs the dashboard handler; B sees pre-tx state. |
| Q1 indexed delta | `TestDeltaExchange_UsesSeqIndex` — `EXPLAIN QUERY PLAN` asserts the plan names `files_by_seq`. |
| Q2 indexed inode | `TestInodeLookup_UsesInodeIndex` — same, for `files_by_inode`. |
| Q3 active count | `TestActiveCount_MaintainedIncrementally` — after N adds, 1 delete, counter equals N−1 without a reload. |
| P2 per-path persist | `TestPersist_WritesOnlyDirtyRows` — scan changes 3 of 1000; assert only 3 INSERT/REPLACE statements run. |
| R6 no `.prev` files | `TestPersist_LeavesNoSidecarFiles` — only `index.sqlite`, `-wal`, `-shm` exist after a persist cycle. |
| L2 clean shutdown | `TestShutdown_ClosesDB` — after `Run` returns, open the DB from a separate handle and confirm it opens cleanly (no residual locks). |

### 4.2 Per-§3-category risk tests

One test per bug category, each picked to be the highest-leverage
exemplar.

| Category | Test |
|----------|------|
| 3.1 D4 context cancellation | `TestBeginTx_CtxCancelReleasesLock` |
| 3.2 T1 IMMEDIATE required | `TestConcurrentWriters_NoBusyError` |
| 3.3 W2 WAL bounded | `TestPersist_WALSizeStaysBounded_ManyCycles` |
| 3.4 E4 nil/empty round-trip | `TestRoundTrip_NilVsEmptyByteSlices` |
| 3.5 S1 no SCAN in hot queries | `TestQueryPlans_NoFullTableScan` |
| 3.6 N4 snapshot isolation | (same as C4) |
| 3.7 F3 read-only FS | `TestOpen_ReadOnlyFilesystem_SurfacesError` |
| 3.8 X1 tempdir cleanup | `TestPersist_AllFilesUnderTempDir` |
| 3.9 Y1 injection-safe | `TestPersist_PathWithSQLMetacharacters` |
| 3.10 V1 migration stub | `TestMigrate_NoOpForV1` |

### 4.3 Property tests

- `TestFileIndex_RoundTripProperty` — `rapid`-driven generator of
  `FileIndex` values (random paths including unicode and SQL
  metacharacters, random `VectorClock`s including zero-entry
  dedup, random `Hash256`s, random `HashState` blobs). Assert
  `loadIndexDB(saveIndex(x)) == x` for all generated values.
- `TestPeerStates_RoundTripProperty` — same shape for
  `map[string]PeerState` including `BaseHashes` maps.

### 4.4 Benchmarks with pinned baselines

- `BenchmarkPersist_168kFiles_3Dirty` — target ≤ 50 ms.
- `BenchmarkPersist_168kFiles_FullWrite` — bootstrap case; target
  ≤ 2 s.
- `BenchmarkLoadIndex_168kFiles` — target ≤ 500 ms.
- `BenchmarkConcurrentReaderDuringScan` — reader latency median
  and p99 while a scan tx is open. No regression past 2x gob
  baseline.

### 4.5 End-to-end coverage

- `TestFilesyncTwoPeer` already restarts peer2; extend its
  post-restart assertion to include the peer map and a sampled
  `BaseHashes` entry. Zero new scenario cost.
- `TestFilesyncMeshC6` (just added) needs no changes; it already
  exercises the full persistence path via process restart.

---

## 5. Open questions

Things I would rather get your call on before coding, instead of
guessing:

1. **Separate reader handle?** `SetMaxOpenConns(1)` serializes
   readers behind the writer. For the admin dashboard that is
   usually fine. The hot path is the peer index-exchange handler
   that might serve many peers. Option: open a second `*sql.DB`
   in read-only mode for peer-facing reads. Proposal: **defer
   unless a benchmark shows contention.**
2. **One DB per folder vs one DB shared by the node?** DESIGN-v1
   says one per folder. Keeps blast radius small and allows
   per-folder `VACUUM` scheduling. Sticking with that.
3. **Retain `persistMu` as a belt-and-braces serializer?** It
   costs nothing. Arguments either way; I lean `drop` to keep the
   mutex hierarchy simple, but willing to `keep` if you prefer.
4. ~~`synchronous=NORMAL` vs `FULL`?~~ **Resolved: `FULL`.** Data
   corruption is not acceptable; see §3.3 W5. The DESIGN-v1 §4
   `NORMAL` choice is overridden and the banner-flip commit notes
   the departure alongside the PH columns and the `peer_state`
   extensions.
5. **Injection of a fault-injection driver for the test suite.**
   Adding a wrapping `driver.Driver` is straightforward but
   introduces a new test-only surface. Alternative: skip fault
   injection, accept weaker coverage for T2 / F5. Proposal:
   **add the wrapper; one-time cost.**

---

## 6. Commit sequence derived from the audit

Numbered against the tasks already in flight. Each commit closes a
named set of audit rows and names those rows in its message. No
commit lands without its tests.

1. **This doc.** (No code.)
2. **Persist hot path — per-path dirty-set.** Changes `folderState`
   to carry `dirtyPaths` / `deletedPaths` / `peersDirtyIds`.
   Instrument `setEntry` and the other mutation sites so the sets
   stay honest. No SQLite wiring yet — feeds both the current gob
   writer (which ignores them) and the future SQLite writer.
   Closes: P2 design prerequisite. Tests: behavioral assertions
   on dirty-set contents across common scan patterns.
3. **Cutover — writers.** `persistFolder` writes through SQLite
   using `INSERT OR REPLACE` on dirty rows, `DELETE` on deleted
   rows, one transaction per call. Gob writes remain active
   (mirror) so rollback is cheap until commit 5.
   Closes: F1, F2, F3, F5, F6, R1, R4, R5, R6, C1.
4. **Cutover — readers + query plans.** Dashboard and index
   exchange handlers read from SQLite. Q1, Q2 indexed queries
   land; `seqIndex` is retired. `EXPLAIN QUERY PLAN` tests pin
   the hot queries.
   Closes: Q1, Q2, Q4, C4, N4.
5. **Retire gob/YAML + legacy refusal + integrity check.** Delete
   `loadIndex`, `save`, `loadPeerStates`, `savePeerStates`,
   `tryLoad*`, `gobMarshalIndex`, etc. Add the typed
   `filesync_legacy_index_refused` error at open and the
   `PRAGMA integrity_check` assertion.
   Closes: R2, R3, R7 (verify it still runs).
6. **`VACUUM INTO` backup.** Admin endpoint emits the snapshot
   path; backup file gets `0600`. Closes: L4, Y3.
7. **Benchmarks landing.** The §4.4 benchmarks with pinned
   baselines. Separate commit so regressions are easy to bisect.
8. **Schema-evolution migration stub.** `migrate(db, from, to)`
   no-op for v1→v1, test asserts it is called on open. Closes: V1.
9. **DESIGN-v1 banner flip to "implemented".** Final commit.
   Verification table cites every code site and test name. Closes
   all outstanding checklist boxes.

Commits 3 and 4 together are the cutover pair. Commit 5 retires the
mirror write from commit 3 — that is the point where the gob path
stops running at all.

---

## 7. Non-goals for this audit

- Revisiting the DESIGN-v1 schema further. Two documented
  departures (PH columns on `files`; peer_state extensions +
  `peer_base_hashes` table) are the full set.
- Anything about the SSH / tunnel / proxy / clipsync layers.
- Performance work unrelated to persistence (scan walk, delta
  compression, watch/scan cadence — tracked separately in
  `PLAN.md`).

---

## 8. Review gate

Before any code from this audit lands:

- [ ] §2 inventory has every row's disposition explicitly chosen.
- [ ] §3 risk table has no "unassessed" cells.
- [ ] §4 test strategy names a test per kept / redesigned behavior
      and per risk category.
- [ ] §5 open questions answered or explicitly deferred with a
      trigger.
- [ ] §6 commit sequence reviewed; each commit's scope is bounded.
