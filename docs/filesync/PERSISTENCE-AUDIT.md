# Persistence-Layer Audit — gob/YAML → SQLite Cutover

> Spec document for the D4 cutover commits. Fills the gap between
> `DESIGN-v1.md` §4 (the schema sketch) and the actual code changes.
> No code lands until this document is reviewed.
>
> Status: **draft, iteration 3** · last updated 2026-04-25.
>
> Iteration 2 folded seven gaps found on adversarial self-review:
> (1) concurrent scan + download write race causing silent
> overwrite of the newer value; (2) un-durable `BaseHashes` letting
> the C2 classifier fall back to C1 mtime with data-loss risk;
> (3) download commit failure leaving the local file overwritten
> without a recoverable row; (4) the in-memory `FileIndex` between
> scans created split-brain risk — resolved by adopting **β
> architecture**: SQLite is the sole source of truth and
> `FileIndex` exists only during a scan; (5) `PRAGMA integrity_check`
> on startup too slow for large folders — split into quick_check
> sync / integrity_check async; (6) shutdown deadline not
> propagated into in-flight transactions; (7) fixed-schedule
> backups wasteful on quiet folders and thin on busy ones —
> replaced with commit-count + max-age schedule. Harness grew to
> cover each gap (H11–H15).
>
> Iteration 3 folded a further set of gaps surfaced by the
> companion review (`PERSISTENCE-AUDIT-REVIEW.md`):
> (1') sequence collision across concurrent writers under β —
> sequences must be assigned inside a `BEGIN IMMEDIATE` window,
> never in scan memory; (2') split BaseHash / `last_sync_ns` crash
> window leaving the C2 classifier between "first-sync fallback"
> and "unknown ancestor → conflict" — folded into one tx; (3') no
> dual-write window between flipping reads and flipping writes,
> which would have left commit 4 reading from an empty SQLite —
> commit C3.5 added; (4') `.bak` files had no lifecycle owner —
> renamed under the existing temp-sweep prefix; (5') scan vs
> in-flight download race left the wrong VectorClock semantics on
> the winning row — scan fast-path now skips paths claimed by a
> download; (6') `device_id` rotation silently corrupted
> VectorClocks — mismatch at open now disables the folder; (7')
> `prev_path` would persist forever in SQLite — cleared on next
> row update, mirroring today; (8') β architecture cost was
> unverified against the 7 ms `cloneInto` baseline — `Benchmark
> LoadIndex_168kFiles` lands at commit 2 as the gate;
> (9') `blocks` table was dead weight in the schema — dropped for
> v1, reopen on measured block-sig latency. Iter-4 adversarial
> review is gated before any code beyond commit 1 lands (see §8).
>
> **Scope correction (2026-04-25).** Filesync has **no production
> state** on the three-peer deployment — every folder has been
> running dry-run only. `~/.mesh/filesync/` is wipeable atomically
> across all peers before the first real-data run, and DESIGN-v1
> §0 already promises a cold protocol swap. Two iter-3 deltas
> change as a result:
>
> - **Gap 3' becomes moot.** The dual-write window from commit 4
>   was insurance against reads flipping to an empty SQLite while
>   gob still served as authoritative. With no state to preserve,
>   the cold swap is the correct posture; dual-write is dead
>   weight. Commit 4 (the dual-write commit) is dropped from §6.
> - **Gob retirement collapses into commit 2.** Without a
>   burn-in window, there is no reason to ship gob deletion
>   separately from the storage swap; the FileIndex encapsulation
>   commit absorbs both. The old retire-gob commit (formerly
>   commit 8 → renamed to "device_id rotation guard" in iter-3)
>   keeps only the device-ID guard, which then folds into the
>   FolderDisabled scaffold commit (commit 3 in the new
>   numbering).
>
> The §6 sequence collapses from 14 commits to 13. The
> structural seams (commit 2 bench gate, scan-claim-skip →
> sync-persist ordering with startup assertion, β provisional)
> all stand. Iter-4 lens reframed: "first-run blast radius on
> real folders" (the moment the dry-run flip lands), not
> "long-running production composition" — the failure model is
> first-hour blast radius, not weeks of accumulated state.
>
> **Renumbering arithmetic (footnote).** β finish lives at the
> same number (commit 7) before and after the rescope. This is
> *coincidental*, not "unchanged." The arithmetic: iter-3 had
> commits 1, 2, 3, 4(dual-write), 5(peer-reads), 6a, 6b, 7(β),
> 8(retire-gob), 9, ..., 14. Iter-3.1 drops commit 4 and
> commit 8, and collapses 6a/6b → 5/6. Two drops + one
> collapse net zero shift at position 7, but every commit
> below β finish has been re-scoped or re-numbered. Reviewers:
> do **not** assume β finish was untouched by the rescope —
> verify its prose matches the new commit-2 bench-gate
> ordering and the explicit decision tree, both refined as
> part of this scope correction.
>
> **Iteration-4 ops-lens findings folded (2026-04-25).** A
> parallel iter-4 review through the operator-workflow lens
> (`PERSISTENCE-AUDIT-REVIEW-ITER4-OPS.md`) surfaced 19
> findings, of which seven are structural changes to this
> audit. The §6 sequence grows from 13 to 14 commits with the
> addition of an operator runbook as a structural deliverable
> blocking v1 ship. Key promotions:
>
> - **O15 → protocol-level invariant.** β-vs-hybrid is a
>   build-time selection (binary `FILESYNC_INDEX_MODEL` const),
>   single value across all peers, asserted at the index-
>   exchange handshake same shape as `protocol_version`.
>   Runtime per-peer divergence is rejected as a
>   session-mismatch error. DESIGN-v1 §0 carries this; commit 7
>   wires it.
> - **O11 → 🚨 structural deliverable.** Per-enum `action`
>   string in the API response and dashboard cell. Implemented
>   as a config table (enum → action string) referenced by the
>   dashboard renderer and the operator runbook. Half the
>   per-enum findings (O1, O2, O5, O6, O7) shrink to runbook
>   entries once O11 ships.
> - **O10 → new §2.7 L5 lifecycle row.** Restore-from-backup
>   is a first-class lifecycle operation: stop folder → swap
>   DB → bump `folder_meta.epoch` → restart folder. Without
>   the epoch bump, peers silently skip rows because their
>   `LastSeenSequence` already exceeds the rewound point.
>   Code change, not doc change.
> - **O3 → default branch for device_id mismatch.** Recovery
>   tries restore-from-backup of the device-id file *first*;
>   wipe-folder-and-resync is the fallback only when the
>   original is genuinely unrecoverable. Cost asymmetry: branch
>   1 keeps VectorClock continuity; branch 2 triggers a full
>   re-sync against every peer.
> - **O8 → diagnostic load expansion.** When the disabled
>   reason is `unknown`, the JSON response carries the full
>   reason text, a stack-trace excerpt, and the last 50 log
>   lines for that folder. The diagnostic path loads into the
>   response itself, not into a separate API call.
> - **O13 + O16 → single runbook deliverable.** The first-run
>   experience and the dry-run-to-real flip are folded into
>   `OPERATOR-RUNBOOK.md` §§1–2 (blocking for v1 ship).
> - **O12 promoted to v1-blocker.** `/reopen` endpoint ships
>   at commit 9 alongside `/restore` and `/backups`. Original
>   ops-review pass deferred O12 to runbook §7 as follow-up;
>   that left runbook §5.3 (backup restore) citing an
>   endpoint that did not exist in v1, and the alternative
>   (manual `sqlite3` editing during recovery) is wrong UX
>   for the ship-perfect bar. §7 promoted to runbook-blocker
>   alongside the endpoint.
> - **O4 → enum split.** D1's `parseInt64` failure routes to
>   a new enum value `metadata_parse_failed`, not to
>   `schema_version_mismatch`. The latter stays for actual
>   schema mismatches.
>
> Iter-4 composition-lens review (the other half of §8 step 2)
> opens next in a fresh conversation. The ops half closes here.
>
> **Iteration-4 composition-lens findings folded (2026-04-25).**
> The composition-lens review
> (`PERSISTENCE-AUDIT-REVIEW-ITER4-COMPOSITION.md`) surfaced 16
> findings (Z1–Z16) on failure-mode composition during the
> first hour after the dry-run flip. Decisions and folds:
>
> - **Z2 → epoch-mismatch promoted to a DESIGN-v1 §0
>   protocol-level invariant.** Code verification (filesync.go
>   ~L1635-1662, the H2b dance) confirms the existing reset
>   path *does* drop BaseHashes (branch A semantics) — but only
>   when triggered by `remote.seq < peer.LastSeenSeq`. An
>   offline peer whose `LastSeenSeq` predates the backup point
>   may not see a sequence drop after restore, leaving stale
>   BaseHashes. **Promotion requires a small code change:**
>   add `|| (remote.Epoch != "" && remote.Epoch != peer.LastEpoch)`
>   to the reset trigger. ~5 LOC + the
>   `TestPeer_OfflineDuringRestore_ResetsOnEpochAlone` test
>   land in commit 7 (where DESIGN-v1 §0 invariants get wired
>   into the handshake).
> - **Z4 → §1.1 wording fix.** The composition reviewer flagged
>   that §1.1's `rm -rf ~/.mesh/filesync/` wipes the device-id
>   file at the parent level. Verification: §2.4 step 3 and
>   §4.3 option 2 both use `rm -rf ~/.mesh/filesync/<folder-id>/`
>   (folder-scoped, leaves device-id intact ✓). §1.1 needs the
>   rewrite "wipe once, before the very first dry-run on each
>   peer; do not rewipe between dry-run and the real-data
>   flip." The path-scope distinction between the two
>   instructions is documented explicitly.
> - **Z6 → branch (A): cancel in-flight writer tx on disabled
>   transition.** Pinned with
>   `TestIntegrityCheckFailedMidLife_RollsBackInFlightTx`.
>   Disabled-state JSON gains `tx_in_flight_rolled_back: true`.
>   Lands in commit 3.
> - **Z7 → runbook hardening only.** Wall-clock skew check + a
>   stronger "one peer at a time" warning in §2.1 pre-flight
>   and §2.2. No programmatic guard at v1 (three-peer scale
>   doesn't justify it).
> - **Z8 → synchronous `integrity_check` after detected SIGKILL
>   recovery.** WAL un-checkpointed frames at open → run
>   `integrity_check` synchronously before going live. New
>   transient state `recovering` in the dashboard. Pin with
>   `TestSIGKILLRecovery_RunsIntegrityCheckSync`. Lands in
>   commit 11 (fault-injection + SIGKILL).
> - **Z10 → metric + handshake rejection (no §8 promotion).**
>   `mesh_filesync_peer_session_dropped{reason="filesync_index_model_mismatch"}`
>   metric ships at commit 7. Runbook §3 surfaces the metric.
>   §8 (build-time decisions documentation) stays in the
>   first follow-up — the metric and the rejection are the
>   load-bearing parts.
> - **Z15 → doc-test (b) for action-string drift.** Parsing
>   test reads `OPERATOR-RUNBOOK.md` §4 and asserts every
>   `disabledReasonActions` map value appears verbatim. ~50
>   LOC. Lands in commit 3.
> - **Z16 → no §9 promotion.** §7 (per-folder reload) is
>   already promoted to v1 in the prior fold; that shrinks
>   the multi-folder `disk_full` blast radius enough that §9
>   can stay follow-up.
> - **Z1 → F7 sweep gains "DB unreadable" branch (defer to
>   operator).** Sweep does nothing with `.bak` when SQLite is
>   unreadable; restore-from-backup runs the sweep again
>   post-reopen against the restored DB. Pin with
>   `TestSweep_DBUnreadable_PreservesBak` and
>   `TestRestore_RunsSweepAfterReopen`. Lands in commit 6
>   (sweep) and commit 9 (restore endpoint).
> - **Z3 → runbook §2.4 cites the epoch protocol contract.**
>   "Rolling back a flipped peer also forces every other peer
>   to re-baseline its PeerState for this peer, via the
>   epoch-change protocol contract (Z2). Do not manually wipe
>   peer state on B / C; the next index exchange resolves
>   it." §2.3 verification gate adds: B and C show updated
>   `PeerState.LastEpoch` for peer A after the flip.
>
> Mechanical folds:
> - **Z5** → backup writes `*.sqlite.tmp`, atomic rename on
>   `quick_check` pass; startup sweep cleans `.tmp`. Commit 9.
> - **Z9** → R9 dirty-set cap is checked atomically inside the
>   set's mutex on `Set`; overflow returns `errDirtySetOverflow`
>   without opening `BEGIN IMMEDIATE`. Two tests pin the
>   no-leak invariant. R9 wording updated; lands in commit 7.
> - **Z11** → restore endpoint runs `quick_check` on the
>   chosen backup before swap. Commit 9.
> - **Z12** → `TestDisabledReasonActions_AllEnumsHaveAction`
>   coverage test. Commit 3.
> - **Z13** → F7 "neither matches" branch:
>   `FolderDisabled(unknown)` with diagnostic load. Commit 6.
> - **Z14** → `TestRetention_IdempotentOnExtraFile`. Commit 9.
>
> §8 step 2b closes here. Final §6 bounded-scope sweep with
> the new commit-9 scope follows. Then commit 1 ships.
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
| F7 | Reversible filesystem rename for downloads (new) | new download path | `redesign` → **new** | Three-step pattern: `rename original → <path>.mesh-bak-<hash>` (named under the existing temp-sweep prefix so leftover files cannot accumulate); `rename temp → original`; commit SQLite row inside `BEGIN IMMEDIATE`; on commit success, `unlink .mesh-bak-<hash>`; on commit failure, `rename .mesh-bak-<hash> → original`, `unlink temp`, surface via metric. Startup sweep (extension of R7) reconciles any leftover `.mesh-bak-<hash>` against the SQLite row for the underlying path. **Three branches** (iter-4 Z1, Z13): (a) SQLite carries the new content's hash → unlink `.bak` (commit succeeded, only the unlink missed); (b) SQLite carries the original content's hash that matches `.bak` → restore `.bak → original`, unlink any temp; (c) **SQLite is unreadable for this folder** (open failed or `quick_check` failed) → sweep does NOTHING with `.bak` (no unlink, no restore); folder enters `FolderDisabled` with the SQLite-derived reason and `.bak` is left intact for the operator's restore-from-backup procedure to encounter. Restore endpoint re-runs the sweep against the restored DB after reopen (per L5 step). (d) **Neither file matches the SQLite row's hash** → folder enters `FolderDisabled` reason `unknown` with diagnostic load (`error_text = "sweep: neither disk file matches SQLite for path %q"`); operator triages. DESIGN-v1 §4 file-format stanza is updated with the new sidecar pattern alongside the existing `.mesh-tmp-*` and `*.mesh-delta-tmp-*` entries. Pin with `TestSweep_DBUnreadable_PreservesBak`, `TestSweep_NeitherMatches_DisablesWithUnknown`, `TestRestore_RunsSweepAfterReopen`. Closes Gap 4'. |

### 2.2 Recovery and corruption handling

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| R1 | Pick higher-sequence of primary/backup on load (H2a) | `loadIndex` | `drop` | No backup file exists. The DB is atomic. A failed transaction is invisible to the next reader. |
| R2 | Warn-and-continue on corrupted gob primary | `tryLoadGobIndex` | `redesign` → **two-phase integrity check, fail loud per-folder** | Current path silently loses data on corruption. v1: `PRAGMA quick_check` runs synchronously at folder open (~ms for grossly corrupted pages); if it fails, the folder enters R8 `FolderDisabled` immediately. Full `PRAGMA integrity_check` (~10 MB/s, up to tens of seconds on large DBs) then runs asynchronously on a goroutine; failure there also transitions the folder to disabled, with the folder having been live in the meantime. Operator can request a blocking full check via admin endpoint. Do not exit the process — `mesh` has SSH / proxy / clipsync / gateway that must keep running. |
| R3 | Rebuild empty index if both files unreadable | `loadIndex` default branch | `redesign` → **per-folder disable** | In the SQLite world, an unreadable DB is an operator problem; do not auto-wipe. The affected folder enters a `FolderDisabled` state with the failing reason attached. Other folders on the same node keep syncing; unrelated components are untouched. |
| R4 | Epoch regeneration on load when empty (H2b) | `loadIndex` | `drop` | Epoch is written once at `folder_meta` seed. No empty-on-load case. |
| R5 | Peer-state reset when index was recreated (B15) | Folder startup in `Run` | `drop` | Motivated by "silent gob fallback gave us an empty index, so peer `LastSentSequence` is now wrong." Failure mode goes away when we fail loud on open (R2, R3). |
| R6 | `prevPath` helper | `prevPath` | `drop` | No `.prev` files. |
| R7 | Abandoned download temp-file sweep | `cleanTempFiles` | `keep` | Orthogonal to index storage; runs before folder init. |
| R8 | Per-folder `FolderDisabled` state + per-enum `action` string + diagnostic load (new) | new `folderState` field + dashboard / metric surface + admin endpoint | `redesign` → **new** | Failure classes R2, R3, R9, F3, F5, I7 all need a way to park a folder without blowing up the process. Folder-level disabled flag carries a **closed-enum reason** (full text logged separately so Prometheus cardinality stays bounded) **plus a paired `action` string** (one-line operator instruction; iter-4 O11). Reason enum: `quick_check_failed`, `integrity_check_failed`, `device_id_mismatch`, `schema_version_mismatch`, `metadata_parse_failed` (new — D1 / iter-4 O4), `read_only_fs`, `disk_full`, `dirty_set_overflow`, `unknown`. **Action strings** live in a package-level `disabledReasonActions map[reason]string` consumed by the dashboard renderer and the runbook (§4 sections cite the same string). Sample mapping: `disk_full` → `"free disk space, then POST /api/filesync/folders/<id>/reopen (or restart node)"`; `device_id_mismatch` → `"restore ~/.mesh/filesync/device-id from backup, restart node — see runbook §4.3"`. The dashboard renders a red row showing reason + action; `/api/filesync/folders` reports `status: "disabled"`, `reason: <enum>`, `action: <string>`, and additionally for `reason=unknown` (iter-4 O8) carries `error_text: <full text>`, `stack_trace: <excerpt>`, `recent_log: [...last 50 lines for this folder...]` so the diagnostic path loads into the response itself rather than requiring a separate log query. `mesh_filesync_folder_disabled{reason=<enum>}` goes to 1. Folder stays disabled until the operator fixes the underlying issue and reopens the folder via `POST /api/filesync/folders/<id>/reopen` (iter-4 O12, ships at commit 9; runbook §7 documents usage). The full node restart remains an option for failure modes the reopen endpoint cannot recover (`device_id_mismatch`, `schema_version_mismatch`, `metadata_parse_failed` — all of which require either an identity-file restore or a backup restore before reopen succeeds). **Disabled transition cancels any in-flight writer transaction (iter-4 Z6 branch A):** the writer context is canceled the moment the disable fires, the tx rolls back, and the disabled-state JSON carries `tx_in_flight_rolled_back: true` for operator visibility. This prevents post-disable rows from being committed to a DB whose `integrity_check` (or other validity check) just failed. Pin with `TestIntegrityCheckFailedMidLife_RollsBackInFlightTx`. Every other folder and every other mesh component (SSH, proxy, clipsync, gateway) is untouched. |
| R9 | Dirty-set overflow → disabled (new) | `folderState.dirtyPaths` cap | `redesign` → **new** | The per-path dirty-set (P2) grows on each scan whose commit fails. Sustained failure (disk full, read-only FS, schema corruption) would let it grow without bound. Cap at **10 000 entries**. **The cap is checked atomically inside the dirty-set's mutex on every `Set`** (iter-4 Z9): on overflow, the `Set` call returns a typed `errDirtySetOverflow`; callers (scan walker, download commit, rename, delete) propagate the error without opening `BEGIN IMMEDIATE`. The disabled transition fires from the caller's goroutine after the in-flight tx (if any) has rolled back; idempotent if multiple goroutines hit the cap simultaneously. Pin with `TestDirtySetCap_FiredFromDownloadGoroutine_NoLeak` and `TestDirtySetCap_FiredMidWalk_NoTxOpened`. 10 000 × ~200 B ≈ 2 MB — bounded, fast to surface. The cap is well below the 168k-file folder size so a single failed scan cycle never trips it; sustained failure is the trigger. Operator fixes the underlying issue and reopens via `/reopen` (or restarts the node). |

### 2.3 Concurrency and locking

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| C1 | `persistMu` serializes concurrent `persistFolder` calls (N10) | `folderState.persistMu`, `Node.persistFolder` | `drop` | SQLite's writer lock already serializes. The mutex is redundant. Verify no other caller relies on `persistMu` for ordering of non-DB side effects. |
| C2 | Snapshot-under-RLock then persist outside (F1) | `Node.persistFolder` | `keep` | We still must not hold `indexMu` across a SQLite transaction. The clone-release-transact pattern stands. |
| C3 | `indexDirty` / `peersDirty` flags skip persist when unchanged (P17a) | `folderState.indexDirty`, `.peersDirty` | `keep` | Skip the `BEGIN`/`COMMIT` round-trip when nothing changed. Cheap and useful even with SQLite. |
| C4 | Reader queries (`/api/filesync/folders`, index exchange) take `indexMu.RLock` | `filesync.go` admin handlers, `protocol.go` index-exchange handler | `redesign` | Readers can go to SQLite directly via WAL snapshot isolation and stop taking `indexMu`. Simplifies lock hierarchy. Pin the boundary with a test that runs a slow scan transaction while a reader hits `/api/filesync/folders`. |
| C5 | Scan and sync coexistence (ref `filesync.go` R2-cancelled invariant) | — | `keep` | Unchanged. |
| C6 | Scan fast-path skips paths claimed by an in-flight download (new) | `claimPath`/`releasePath` extension; scan walker check | `redesign` → **new** | Today `claimPath` dedupes downloads against each other. The β model exposes a new race: between download step 2 (rename temp → original) and step 3 (commit SQLite), the on-disk path holds new content but SQLite still carries the old row. A scan that walks during that window would re-hash the new bytes and write a row with scan-derived VectorClock semantics (local-bumped) instead of the download's adopt-remote semantics. Extend the scan fast-path to consult the `inFlight` claim map and skip any claimed path; the next scan after `releasePath` picks it up and converges with the SQLite row that the commit wrote. Closes Gap 5'. |

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
| P6 | Tombstone GC (`purgeTombstones`) | `runScan` tail | `redesign` | Today: per-scan in-memory pass that drops tombstones older than `tombstoneMaxAge` from `idx.Files`. SQLite world: `DELETE FROM files WHERE folder_id=? AND deleted=1 AND mtime_ns < ?` runs every **10th scan**, age-based. Mirrors today's semantics; keeps the hot table lean without paying the cost on every scan. Vacuuming is the existing backup pass. |

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
| I7 | Device-ID rotation guard (new) | `seedFolderMeta` open path | `redesign` → **fail loud** | At open, compare the node-level `~/.mesh/filesync/device-id` content against `folder_meta.device_id`. Mismatch → folder enters `FolderDisabled` with reason `device_id_mismatch`. The current "backfill only when stored value is empty" path silently accepts a rotated identity and corrupts every subsequent VectorClock bump (the local-bump uses a different device_id key from what the DB believes is "self"). **Recovery default (iter-4 O3): restore the original `~/.mesh/filesync/device-id` from a backup source (Time Machine, Syncthing copy, etc.) and restart.** This keeps VectorClock continuity, peer state, and avoids a conflict storm. Fallback only when the original is genuinely unrecoverable: wipe `~/.mesh/filesync/<folder-id>/` and accept full re-sync against every peer (which forces an epoch regeneration via L5). The action string for this enum names option 1 first; runbook §4.3 documents the decision tree explicitly. Closes Gap 6'. |
| I8 | `prev_path` rename hint, transient single-use | `FileEntry.PrevPath`, scan rename pairing | `redesign` | Today the hint is cleared on the next rescan. SQLite world: same semantic — any UPSERT on the row clears `prev_path` unless a fresh hint is being recorded in this commit. The hint is *not* re-sent on subsequent delta exchanges. Pin with a regression test: two consecutive delta exchanges to the same peer; the second carries no `prev_path` for the renamed entry. Receiver idempotency (already shipped — applying a hint twice is a no-op) is also pinned by regression test, not new code. Closes Gap 7'. |

### 2.8 Architecture invariants (β)

Adopted in iteration 2. Every cutover commit enforces these four
rules; tests assert them. No exceptions.

**INV-1 — SQLite is the sole source of truth.** Every piece of
state a peer can observe lives in SQLite. In-memory structures are
working copies that exist only while code holds them, and they are
never consulted by peer-facing code paths.

- `buildIndexExchange` → `SELECT ... FROM files WHERE ... sequence > ?`.
- Delta / bundle / blocksigs handlers → SQLite queries.
- `/api/filesync/folders`, `/api/filesync/conflicts` → SQLite, with
  a folderState-level summary cache for the dashboard hot path.
- Dashboard active-count / active-size → folderState cached counters
  (INV-4), not a FileIndex field.

**INV-2 — `FileIndex` is scan-local (provisional pending bench).**
The candidate model: at scan start the working copy is constructed
via `SELECT` of the folder's rows and discarded after the scan's
commit. Between scans there is no `FileIndex` — it does not exist,
cannot be read, cannot drift. `setEntry` and `cloneInto` become
internal to the scan path. Non-scan mutation paths (download,
rename, delete) operate directly on SQLite via sync-persist.

The per-scan `SELECT` cost on `modernc.org/sqlite` (pure Go,
typically 2–4× slower than CGo SQLite for row-decode-heavy queries
on a 14-column row with two BLOBs) is not yet measured.
**`BenchmarkLoadIndex_168kFiles` ships in commit 2 as the gate
for INV-2:**

- `< 80 ms` → INV-2 stands; commits 4–7 proceed as planned.
- `≥ 80 ms` → pivot to **hybrid**: in-memory `FileIndex` is
  retained between scans as a private scan-only working copy.
  INV-1, INV-3, INV-4 are unchanged — SQLite remains the sole
  peer-visible truth, sequences are still assigned in tx,
  BaseHashes are still committed atomically. Only the "discard
  after scan" rule is relaxed.

The 80 ms gate is deliberately tight: today's `cloneInto`
recycling baseline is 7 ms / 0 allocations on the target
hardware, so any value above ~10× that floor regresses
watch-triggered hot-path scans materially.

**INV-3 — Every write is sequence-conditioned, sequences assigned
inside the commit transaction.** Two parts:

- *Assignment.* No mutation path increments `folder_meta.sequence`
  outside a `BEGIN IMMEDIATE` ... `COMMIT` window. Inside the tx,
  the writer reads the current sequence `S`, assigns `S+1..S+N` to
  the rows it is about to write (deterministic order), updates
  `folder_meta.sequence = S+N`, and commits. A scan or download
  that runs concurrently waits for the writer lock — there is no
  read-then-increment-then-write window during which two writers
  can pick the same next sequence. During scan, paths in the
  in-memory working copy carry a "pending" marker rather than a
  pre-assigned sequence; the sequence is stamped on each row only
  at commit time.

- *Conditional UPSERT.* Every `INSERT OR REPLACE` runs with
  `WHERE excluded.sequence > files.sequence` (or equivalent for
  other tables). A concurrent download with a newer sequence
  cannot be overwritten by a stale-sequence write that lost the
  ordering race; the conditional UPSERT skips it.

Closes Gap 1 (concurrent scan + download write race) and
Gap 1' (sequence collision across concurrent writers).

**INV-4 — Commit precedes observability, always.**

- *Downloads*: three-step atomic pattern. `rename original → .bak` → `rename temp → original` → commit SQLite row → on success `unlink .bak`; on commit failure `rename .bak → original`, `unlink temp`, surface via metric. Closes Gap 3.
- *Scan*: build pending in memory → `BEGIN IMMEDIATE` → sequence-conditioned UPSERT of dirty rows → `COMMIT` → on success swap pending to live for the remainder of the current call (which is all that uses it — discarded next). Commit failure discards pending; live (which doesn't exist between scans per INV-2 anyway) is untouched; next scan re-detects.
- *Peer sync updates (BaseHashes, LastSentSequence, LastSeenSequence, `last_sync_ns`)*: every per-peer field touched by a sync outcome is written inside **one** `BEGIN IMMEDIATE` ... `COMMIT`. The `peer_state` row update and the matching `peer_base_hashes` rows ride the same tx. A crash between them cannot leave `last_sync_ns > 0` while BaseHashes is empty (which would otherwise strand the next `diff()` between "first-sync, fall back to C1" and "prior sync but unknown ancestor → conflict" with the wrong choice). Closes Gap 2 and Gap 2' (split BaseHash / `last_sync_ns` crash window).
- *Classifier semantics*: absence of a BaseHash entry for a (peer, path) pair means "unknown ancestor → conflict path," never "fall back to C1 mtime comparison." The C1 heuristic is only used when we have positive knowledge of no prior sync with this peer (first-sync case).

### 2.7 Lifecycle hooks

| # | Behavior | Code site | Disposition | Notes |
|---|----------|-----------|-------------|-------|
| L1 | `persistAll(force=true)` at shutdown | `Node.persistAll` | `redesign` | A shutdown context with a deadline propagates into in-flight transactions via `db.BeginTx(ctx, ...)`. A scan-reset transaction that would exceed the deadline rolls back and defers to the next run; persist-on-shutdown prefers the last durable state over a partial write. |
| L2 | `fs.root.Close()` on shutdown | `Run` shutdown tail | `keep` | Add `fs.db.Close()` beside it, after `wg.Wait` so no goroutine holds rows. |
| L3 | `persistFolder(force=true)` after scan | `runScan` tail | `redesign` | Scan path owns its own commit (see §2.8 invariants). This hook reduces to "flush any pending dirty-set that hasn't committed for non-scan reasons." |
| L4 | Admin backup currently copies gob bytes | n/a (planned) | `redesign` → `VACUUM INTO`, commit-count schedule | Triggered on every Nth successful commit (default N=100) with a max-age safety net (≥ 1 backup per 24 h). Retain 24 backups. Quiet folders produce few backups; busy folders produce many. Each `VACUUM INTO` runs on a dedicated goroutine so it never blocks the writer. |
| L5 | Restore-from-backup as a first-class lifecycle (new — iter-4 O10) | new admin endpoint + ops procedure | `redesign` → **new** | Without an explicit restore lifecycle, an operator who swaps the SQLite file from a backup leaves peer state silently desynchronized: peers' `LastSentSequence` already exceeds the rewound `folder_meta.sequence`, so peer delta queries (`WHERE sequence > LastSeenSequence`) skip exactly the rows the restore brought back. C2 classification then fans out `.sync-conflict-*` siblings on every path whose hash diverges from the restored DB. **Defined sequence:** (1) stop the folder via the per-folder reload endpoint; (2) swap `index.sqlite` (and its `-wal` / `-shm` sidecars) with the chosen backup file; (3) **bump `folder_meta.epoch` to a new value** so peers treat us as a re-baselined source on next exchange and trigger a full re-sync rather than a delta; (4) restart the folder. The epoch bump is the load-bearing step — without it, peers see the same epoch and treat the rewind as silent data loss on our side. Admin endpoint `POST /api/filesync/folders/<id>/restore` accepts `{backup_path: ...}` and runs the four-step procedure atomically; runbook §5 documents the manual equivalent. Closes iter-4 O10. |

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
| INV-3 sequence-guarded write | `TestConcurrentScanDownload_NewerWins` — spawn a download that commits `path=P, seq=105` mid-scan whose pending has `P, seq=102`; assert post-scan SQLite has `seq=105` and pending's stale value is dropped. (H11) |
| INV-4 BaseHash durability | `TestCrashBeforeBaseHashCommit_ClassifiesAsConflict` — inject commit failure after in-memory BaseHash update; restart; drive another sync; assert path is classified conflict, not "only they modified." (H12) |
| INV-4 download atomic rollback | `TestDownloadCommitFails_RestoresOriginal` — inject `SQLITE_FULL` after temp→final rename; assert .bak is restored, temp unlinked, metric incremented, no row written. (H13) |
| β reload correctness | `TestScanReloadFromSQLite_StateConsistent` — post-scan, drop in-memory state, reload via SQLite; assert dashboard, peer exchange, next scan all see identical state. (H14) |
| Two-phase integrity | `TestIntegrityCheck_QuickSyncFullAsync` — corrupt DB after folder open; assert quick_check passed, folder goes live, background integrity_check fails, folder transitions to disabled without taking the node down. (H15) |
| Shutdown deadline | `TestShutdown_DeadlinePreemptsScanCommit` — start a scan-reset tx (large row count); signal shutdown with 1 s deadline; assert the tx rolls back cleanly, DB is at the last durable state, shutdown completes before deadline × 2. |
| Backup schedule | `TestBackup_CommitCountAndMaxAge` — drive 200 commits; assert two backups written; let 24 h of frozen-clock pass; assert the max-age sweeper wrote one more. |

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

### 4.6 Applied during implementation (no design needed)

The iteration-3 review (`PERSISTENCE-AUDIT-REVIEW.md` §D) named
small obvious fixes that need no design but must not be lost on
the floor. Each is folded into the commit that touches the
relevant code; this checklist is the audit trail.

| # | Fix | Lands in commit |
|---|-----|-----------------|
| D1 | `parseInt64` / `parseUint64` use `strconv.ParseInt`, propagate errors. Garbage in `folder_meta.sequence` transitions the folder to `FolderDisabled` with reason `schema_version_mismatch` rather than silently restarting from sequence `0`. (Pending stub at commit 2 because `FolderDisabled` is not yet wired; promotes to actual disabled-state transition at commit 3.) | 2 (stub) → 3 (wire) |
| D2 | Rename the `saveIndex` local in `persistFolder` (e.g. `shouldSaveIndex`) to remove the shadowing of the package-level SQLite writer. | 2 |
| D3 | Drop the dead `tx.Exec("BEGIN IMMEDIATE")` after `db.Begin()`; switch to the modernc DSN flag `?_txlock=immediate` (or `db.Conn(ctx)` + explicit `BEGIN IMMEDIATE`) so writer txs are immediate by construction. | 2 |
| D4 | Closed-enum reason on `mesh_filesync_folder_disabled{reason=...}`. Already in §2.2 R8. | 3 |
| D5 | `TestOpen_SynchronousIsFULL` also asserts `journal_mode=wal` so a future refactor cannot regress one without the other. | 2 |
| D6 | `SetConnMaxLifetime` capped at 24 h instead of `0`. Connection rotation is cheap; leak containment is free on a weeks-long daemon. | 2 |
| D7 | CRC32 trailer on the VectorClock blob — moved up so blobs are never written without it. | 6 |
| D8 | `SIGKILL` mid-scan recovery e2e test. | 10 |

---

## 5. Resolved design calls

All six design calls settled 2026-04-22. Captured here so later
commits can cite them without re-arguing the case.

1. **In-memory state between scans — α or β?** **β.** SQLite is
   the sole source of truth; `FileIndex` is scan-local and does
   not exist between scans. This eliminates an entire class of
   split-brain and stale-read bugs by construction rather than by
   discipline. Added per-scan-start cost: ~100 ms `SELECT` on a
   168k-file folder, dwarfed by the filesystem walk and the hash
   phase. See §2.8 INV-1, INV-2.

2. **Reader handle.** **Single `*sql.DB` per folder.** A second
   read-only handle is not opened unless `BenchmarkConcurrentReaderDuringScan`
   shows contention on the local 168k-file workload. Reopen the
   question only on a measured regression, not on speculation.
3. **One DB per folder vs shared.** **Per folder**, per DESIGN-v1
   §4. Keeps the blast radius of a corruption to one folder;
   matches the R8 `FolderDisabled` failure isolation; allows
   independent `VACUUM INTO` scheduling.
4. **`persistMu`.** **Drop.** SQLite's writer lock already
   serializes transactions; an extra Go-side mutex adds a rung to
   the hierarchy for no gain. Any non-DB side effect that relied
   on `persistMu` ordering (none identified, but audit at cutover
   time) migrates to explicit sequencing.
5. **`synchronous`.** **`FULL`.** See §3.3 W5 and DESIGN-v1
   §Durability — data-corruption risk is not acceptable.
6. **Fault-injection driver.** **Add the wrapper.** A thin
   `driver.Driver` that wraps `modernc.org/sqlite` and injects
   errors on demand lives in `internal/filesync` under
   `_test.go` files only, so the production binary is unchanged.
   Covers T2 (commit-after-error), F5 (disk full), and gives
   future tests a hook for any new crash-resilience case. The
   one-time cost is worth the coverage floor it establishes.

Iteration-3 calls (settled 2026-04-25):

7. **`blocks` table.** **Drop for v1.** The current schema
   declares it; no production code reads or writes it. Populating
   would add per-block-row writes scaling with file size — a 10 GB
   media file at 128 KB avg = ~80k rows, unbounded for the
   workloads (drone footage, recordings) that pressure storage
   most. Block sigs compute on demand from the open file, bounded
   by file read speed and usually hitting the page cache. Reopen
   criterion: measured cold-cache `/blocksigs` latency, not
   speculation.
8. **`prev_path` consumption.** **Clear on next row update
   (option c).** Mirrors today's transient-hint semantic. SQLite
   world: any UPSERT on the (folder_id, path) row sets
   `prev_path = NULL` unless a fresh hint is being recorded.
   Receiver idempotency (already shipped — applying a hint twice
   is a no-op) is pinned by regression test, not new code.
   See §2.6 I8.
9. **β architecture finality.** **Provisional pending bench.**
   `BenchmarkLoadIndex_168kFiles` runs in commit 2.
   `< 80 ms` → β proceeds. `≥ 80 ms` → pivot to hybrid (in-memory
   `FileIndex` retained between scans as scan-private working
   copy; SQLite remains the sole peer-visible truth). The 80 ms
   gate is deliberately tight: `cloneInto` is 7 ms today, and a
   ≥ 10× regression on watch-triggered scans is not acceptable.
   See §2.8 INV-2.
10. **Reader handle.** **Two `*sql.DB` per folder, ship at
    commit 4.** Replaces the iter-2 "defer until measured
    regression" position. Writer handle stays at
    `MaxOpenConns=1`; reader handle opens with
    `?_pragma=query_only(true)&mode=ro` and
    `MaxOpenConns=n_peers+3`. Eliminates the cutover-day risk of
    every peer exchange and admin read serializing behind a
    single writer connection.
11. **Backup retention.** **Plain GFS — 5 daily + 4 weekly +
    1 monthly.** No `max_backup_bytes` knob. Replaces the iter-2
    "retain 24" rule. GFS bounds total at ~10× DB size by
    construction (~30 GB across 15 folders worst case). A size
    cap is operator config surface for a problem GFS already
    solves; strip.
12. **`PRAGMA mmap_size = 64 MiB`.** Apple Silicon and Linux
    desktops have ample VA; 64-bit Windows is fine. 15 folders ×
    64 MiB = 960 MiB virtual reservation, resident bounded by
    actual page touches.
13. **Fault-injection driver wiring.** **Plumb `driverName`
    through `openFolderDB`.** Production default `"sqlite"`;
    tests register `"sqlite_faulty"` in `_test.go` and pass that
    name in. No global mutable hooks.
14. **Dirty-set overflow cap.** **10 000 entries → folder enters
    `FolderDisabled` with reason `dirty_set_overflow`.** See
    §2.2 R9.
15. **Tombstone GC cadence.** **Every 10th scan, age-based
    DELETE.** Mirrors today's `purgeTombstones`. See §2.5 P6.

Iteration-4 ops-lens calls (settled 2026-04-25):

16. **β-vs-hybrid is a build-time protocol invariant
    (iter-4 O15).** The selection between β (FileIndex
    discarded between scans) and hybrid (FileIndex retained)
    is baked into the binary at build time as a const
    `FILESYNC_INDEX_MODEL` (values: `"beta"` or `"hybrid"`).
    Same shape as `protocol_version`: stamped on every
    `IndexExchange`, asserted at handshake, mismatched peers
    rejected with `last_error="filesync_index_model_mismatch"`.
    DESIGN-v1 §0 carries the field alongside `protocol_version`
    and `device_id`. Per-peer runtime divergence is rejected,
    not tolerated — three peers built with two different model
    selections is a configuration bug, not a feature. The
    decision is recorded in DESIGN-v1 §0 (durable home, not
    just a commit message).
17. **Restore-from-backup epoch bump (iter-4 O10).** Every
    backup restore bumps `folder_meta.epoch`. Without the
    bump, peers' `LastSeenSequence` already past the rewind
    point silently skips the restored rows. See §2.7 L5.
18. **Per-enum action strings (iter-4 O11).** Every disabled
    reason has a paired one-line `action` string. Implemented
    as a package-level `disabledReasonActions map[reason]string`
    consumed by the dashboard renderer, the API response, and
    the operator runbook. The action string is the
    single-source-of-truth for "what to do next"; the runbook
    §4 sections cite the same string verbatim so the dashboard
    and the runbook can never drift. See §2.2 R8.
19. **`unknown` reason carries diagnostic load (iter-4 O8).**
    For `reason=unknown`, the API response carries
    `error_text`, `stack_trace`, and `recent_log` (last 50
    lines for that folder) inline. The operator does not need
    a separate log query to triage the catch-all enum. See
    §2.2 R8.
20. **Device-id mismatch default branch (iter-4 O3).**
    Recovery default is restore-from-backup of
    `~/.mesh/filesync/device-id`; wipe-folder-and-resync is
    the fallback only when the original is unrecoverable. See
    §2.6 I7.
21. **Operator runbook is a v1-ship blocker (iter-4 O13 +
    O16).** `OPERATOR-RUNBOOK.md` ships with §1–§7 complete
    before v1 flips from dry-run to real on any peer. §1
    (before-first-run), §2 (dry-run-to-real flip), §3
    (dashboard triage), §4 (recovery by enum), §5 (backup
    restore), §6 (peer-state reconciliation), **§7
    (per-folder reload endpoint)** are blocking. §7's
    promotion is a runbook-internal contradiction fix:
    runbook §5.3 (backup restore) cites the `/reopen`
    endpoint, so the endpoint must ship in v1 and the
    section documenting it must ship alongside. The
    alternative — rewriting §5.3 as a manual `sqlite3` CLI
    procedure — is the wrong UX for the ship-perfect bar
    (manual SQL editing during recovery is exactly when the
    operator is most likely to make a mistake). §8
    (build-time decisions for operators), §9 (known limits),
    §10 (escalation) stay follow-ups, not blockers. See §6
    commit 13.
22. **Enum split — `metadata_parse_failed` (iter-4 O4).**
    D1's `parseInt64` failure on `folder_meta.sequence`
    routes to a new enum value `metadata_parse_failed`, not
    to `schema_version_mismatch`. The latter stays for actual
    schema-version drift. See §2.2 R8.
23. **Backup file naming + listing endpoint (iter-4 O9, O18).**
    Backup files land at
    `~/.mesh/filesync/<folder-id>/backups/index-<seq>-<unixns>.sqlite`,
    mode `0600`, where `<seq>` is the highest sequence
    committed at backup time and `<unixns>` is the wall clock
    at backup. Admin endpoint
    `GET /api/filesync/folders/<id>/backups` lists them with
    metadata: `{path, sequence, created_at, quick_check_ok}`.
    Operator picks by sequence + `quick_check_ok`, not "newest
    file in the directory."

Iteration-4 composition-lens calls (settled 2026-04-25):

24. **Epoch-mismatch is a DESIGN-v1 §0 protocol invariant
    (iter-4 Z2).** Same shape as `protocol_version` and
    `FILESYNC_INDEX_MODEL`: receiver compares incoming `Epoch`
    against cached `PeerState.LastEpoch`; on mismatch (or on
    sequence drop, the existing trigger), drops `BaseHashes`
    and `LastSeenSequence` for that (folder, peer) pair,
    forces a full re-sync next cycle, and records
    `last_error="epoch_mismatch"`. **Implementation note:**
    code verification (filesync.go ~L1635-1662) shows the
    existing reset path is branch A (drops BaseHashes, resets
    LastSeenSeq) but is triggered only by sequence drop. The
    promotion adds `|| (remote.Epoch != "" && remote.Epoch !=
    peer.LastEpoch)` to the trigger condition (~5 LOC). Pin
    with `TestPeer_OfflineDuringRestore_ResetsOnEpochAlone`.
    Lands in commit 7. DESIGN-v1 §0 names the invariant.
25. **Disabled-transition cancels in-flight writer tx
    (iter-4 Z6 branch A).** When a folder transitions to
    `FolderDisabled` mid-life, the writer context is
    canceled; the in-flight tx rolls back; the disabled-state
    JSON carries `tx_in_flight_rolled_back: true`. Pin with
    `TestIntegrityCheckFailedMidLife_RollsBackInFlightTx`.
    Lands in commit 3. R8 cites the contract.
26. **Synchronous `integrity_check` after detected SIGKILL
    recovery (iter-4 Z8).** When the WAL contains
    un-checkpointed frames at folder open (the SIGKILL
    signal), `integrity_check` runs **synchronously** before
    the folder goes live. Adds ~20 s on a 200 MB DB —
    acceptable on the recovery path; the silent
    live-but-corrupt window is not. Dashboard renders a new
    transient state `recovering` during the sync check. Pin
    with `TestSIGKILLRecovery_RunsIntegrityCheckSync`. Lands
    in commit 11.
27. **Process-level `index_model_mismatch` metric +
    handshake rejection (iter-4 Z10).**
    `mesh_filesync_peer_session_dropped{reason="filesync_index_model_mismatch"}`
    counter increments on every dropped session. Runbook §3
    surfaces the metric with a warning row when > 0. **§8
    (build-time decisions documentation) stays in the first
    follow-up commit, not v1** — the metric and the handshake
    rejection are the load-bearing parts; doc can follow once
    operators have something concrete to read about. Lands in
    commit 7.
28. **Action-string drift enforcement: doc-test (iter-4
    Z15).** `TestRunbookActionStringsMatchMap` parses
    `OPERATOR-RUNBOOK.md` §4 and asserts every
    `disabledReasonActions` map value appears verbatim in the
    runbook prose. ~50 LOC. Lighter than codegen, catches
    drift on the test run. Lands in commit 3 alongside the
    map.
29. **Backup write-temp-then-rename (iter-4 Z5).** `VACUUM
    INTO` writes to `index-<seq>-<unixns>.sqlite.tmp`. On
    `VACUUM` success and post-VACUUM `quick_check` pass,
    atomically `rename` to the final name. On failure or
    crash, `unlink` (the startup sweep extends to clean any
    `*.sqlite.tmp` under `<folder>/backups/`). Retention
    prune treats `.tmp` files as invisible. Pin with
    `TestBackup_SIGKILLLeavesNoFinalFile` and
    `TestBackup_StartupSweepCleansTmp`. Lands in commit 9.
30. **Restore re-runs `quick_check` before swap (iter-4
    Z11).** `POST /api/filesync/folders/<id>/restore` runs
    `quick_check` on the chosen backup file as step 0 of
    the four-step procedure. On failure, abort with a typed
    error; folder remains in current state. Pin with
    `TestRestore_RechecksBackupBeforeSwap`. Lands in
    commit 9.

---

## 6. Commit sequence derived from the audit

Each commit closes a named set of audit rows and names those rows
in its message. No commit lands without its tests. Commits are
ordered so each one leaves the tree green and the architecture
consistent — never a "land now, fix later" intermediate state.

1. **This doc.** (No code.)
2. **FileIndex encapsulation + dirty-set + storage swap (gob
   deleted) + INV-2 bench gate.** Heavy-lift commit. `Files` map
   goes private. `Set`, `Get`, `Range`, `Delete`, `DirtyPaths`,
   `ClearDirty` become the only API. Dirty-set populated by
   `Set` / `Delete` as a side effect, not via diff. Legacy
   `setEntry` call sites migrate via `gopls rename`.
   `persistFolder` writes through `saveIndex` /
   `savePeerStatesDB`; `runScan` loads from `loadIndexDB` /
   `loadPeerStatesDB` at scan start; gob path deleted (`loadIndex`,
   `save`, `loadPeerStates`, `savePeerStates`, `tryLoad*`,
   `gobMarshalIndex`, `yamlToGobPath`, `prevPath`, `writeFileSync`,
   all test helpers that exercised those paths). Typed
   `filesync_legacy_index_refused` error on open when any legacy
   sidecar file is present (footgun guard against stale dev
   directories — there is no production state to migrate, only
   to refuse). Peer / admin code still reads in-memory `fs.index`
   populated from SQLite at scan start; the peer-read swap to
   SQLite arrives in commit 4. SQLite is the only on-disk store
   after this commit.

   **Internal ordering inside commit 2 (strict).** The bench is
   a *gate*, not a deliverable, and must inform the choice
   between β and hybrid before the gob path is gone:

   1. Land `BenchmarkLoadIndex_168kFiles` against
      `modernc.org/sqlite` first. The bench operates on a
      synthetic 168k-row folder DB and does not depend on
      production code being swapped — it can run while gob is
      still authoritative.
   2. Run the bench on the target hardware **with `-count=10`
      (or higher); record the median, the standard deviation,
      and the per-run numbers** in the commit message and in
      `RESEARCH.md`. A single-run number is a coin flip, not a
      decision artifact. Decision-grade form: "82 ms median,
      ±3 ms std-dev across 10 runs"; reject "82 ms from one
      run." If std-dev exceeds 10 ms, raise the run count or
      stabilize the host (background load, CPU governor) before
      recording — the 75–85 ms borderline band in commit 7 is
      meaningful only when the variance is bounded.
   3. **Decision recorded here, executed in commit 7:**
      `< 80 ms` → β proceeds at commit 7 (FileIndex discarded
      between scans). `≥ 80 ms` → hybrid pivot (in-memory
      FileIndex retained between scans as scan-private). Commit
      7's prose is the explicit decision tree.
   4. Storage swap (gob deletion + SQLite wiring) lands after
      the decision is recorded. The decision is durable in the
      commit message even though the gob path is gone — a
      hybrid is always reachable from where commit 2 lands
      because the in-memory `fs.index` lives until commit 7
      explicitly discards it (β path) or keeps it (hybrid path).

   Open-path failures temporarily log and skip the folder here
   — the `FolderDisabled` transition arrives in commit 3.
   D1 (parse-error → `FolderDisabled`-pending stub),
   D2 (rename `saveIndex` shadow), D3 (drop dead `BEGIN
   IMMEDIATE`, use `?_txlock=immediate`), D5
   (`TestOpen_SynchronousIsFULL` also asserts `journal_mode=wal`),
   D6 (`SetConnMaxLifetime` cap at 24 h) all fold here. The
   invariant-recompute test and the round-trip property tests
   (§4.3 `TestFileIndex_RoundTripProperty`,
   `TestPeerStates_RoundTripProperty`) land here. The
   `mmap_size = 64 MiB` pragma is added to
   `applyFolderDBPragmas` as part of the swap. The
   `prev_path` clear-on-rescan semantic (I8) lands with
   saveIndex: every UPSERT of a row clears `prev_path` unless
   a fresh hint is being recorded in this commit; pin with the
   regression test from §2.6 I8.
   Closes: F1, F2, F3, F4, F5, F6 (atomicity, gob path),
   R1, R4, R5, R6, C1 (`persistMu`), C3, I8, P1, P2 (per-path
   cost target), L3, P17b, Gap 7' (`prev_path` clear-on-rescan),
   Gap 8' (β cost bench, decision §5 #9), decision §5 #12
   (`mmap_size = 64 MiB`).
3. **FolderDisabled scaffold + two-phase integrity check + reason
   enum + per-enum action strings + `unknown` diagnostic load +
   `device_id` rotation guard + drop `blocks` table.**
   `PRAGMA quick_check` runs synchronously at folder open;
   `PRAGMA integrity_check` runs on a goroutine afterward. New
   folder status field + `/api/filesync/folders` exposure +
   `mesh_filesync_folder_disabled{reason=<enum>}` metric. Reason
   is the closed enum from §2.2 R8 (now including
   `metadata_parse_failed` for D1's parse error — iter-4 O4 split
   it out from `schema_version_mismatch`). **Per-enum `action`
   string** (iter-4 O11) ships in this commit as a package-level
   `disabledReasonActions map[reason]string`; the dashboard
   renderer and the API response both consume it; the runbook §4
   cites the same strings verbatim. **Diagnostic load on
   `unknown`** (iter-4 O8): when `reason=unknown`, the API
   response carries `error_text`, `stack_trace`, and `recent_log`
   inline so the operator can triage without a separate log
   query. The "log and skip" stub from commit 2 promotes to a
   disabled-state transition; commit 4 onward inherit the wired
   machinery. Device-ID mismatch check at open
   (`folder_meta.device_id` vs the node-level identity file)
   → `FolderDisabled` reason `device_id_mismatch` (I7).
   `applyFolderDBSchema` removes `CREATE TABLE blocks`; existing
   DBs from dev builds are wiped per the v1 cold-start posture.
   H15 lands here.
   **Iter-4 composition folds:** the disabled-transition
   handler cancels the in-flight writer tx (decision §5 #25 /
   iter-4 Z6 branch A); disabled-state JSON carries
   `tx_in_flight_rolled_back: true`. The
   `disabledReasonActions` map ships with a coverage test
   (`TestDisabledReasonActions_AllEnumsHaveAction`, iter-4
   Z12) and a runbook drift doc-test
   (`TestRunbookActionStringsMatchMap`, decision §5 #28 /
   iter-4 Z15) that parses `OPERATOR-RUNBOOK.md` §4 against
   the map.

   Closes: R2, R3, R8, R9, F3 wiring, F5 wiring, I7,
   metric cardinality (D4), Gap 5 (integrity_check sync /
   async split), Gap 6' (`device_id`),
   Gap 9' (drop `blocks`, decision §5 #7),
   decisions §5 #18 (action strings), §5 #19 (diagnostic load),
   §5 #20 (device_id default branch), §5 #22 (enum split),
   §5 #25 (in-flight tx cancel), §5 #28 (runbook doc-test).
4. **Peer-facing reads go to SQLite (INV-1) + reader handle.**
   `buildIndexExchange`, delta / bundle / blocksigs handlers,
   `/api/filesync/folders`, `/api/filesync/conflicts` all query
   SQLite via a dedicated read-only `*sql.DB` handle
   (`?_pragma=query_only(true)&mode=ro`,
   `MaxOpenConns = n_peers + 3`). Writer handle stays at
   `MaxOpenConns = 1`. Dashboard gains `folderState.summary`
   cache (INV-4). In-memory `FileIndex` stops being peer-visible.
   `seqIndex` retired; `files_by_seq` and `files_by_inode` serve
   the range / inode queries. `EXPLAIN QUERY PLAN` tests pin
   every hot query. `BenchmarkConcurrentReaderDuringScan` lands
   alongside.
   Closes: Q1, Q2, Q4, C4, N4, INV-1, decision §5 #10.
5. **Scan fast-path skips paths claimed by in-flight downloads
   (Gap 5').** Pure coordination change. Scan walker consults the
   `inFlight` claim map (existing field, owned by
   `claimPath`/`releasePath`); any claimed path is skipped for
   the current cycle and reconsidered after `releasePath` clears
   it. Lands first because it is the seam — independent of any
   write-path code, harmless before commit 6 lights up the
   extended claim window, and the safe ordering target for the
   revert blast radius. Test: `TestScan_SkipsClaimedPaths`.
   Closes: C6 (§2.3), Gap 5'.
6. **Sync-persist on download / rename / delete paths (INV-4) +
   VectorClock CRC + `.bak` lifecycle.** Three-step atomic
   pattern using `.mesh-bak-<hash>` intermediate for downloads
   (named under the existing temp-sweep prefix so leftover
   `.bak` files cannot accumulate); rename / delete mirror the
   shape. 100 ms batch window coalesces concurrent downloads into
   a single commit. **BaseHashes, LastSentSequence /
   LastSeenSequence, and `last_sync_ns` ride one tx** (INV-4
   peer-update bullet). Classifier tightens: absent BaseHash
   means "conflict," never "C1 fallback" — except first-sync,
   gated on `last_sync_ns == 0` and resolved in the same tx.
   CRC32 trailer on the VectorClock blob lands **here**, before
   any production write path emits the packed form (D7). Startup
   sweep reconciles `.mesh-bak-<hash>` against the SQLite row for
   the underlying path. Download path extends the `claimPath`
   window to span SQLite commit so commit 5's scan skip covers
   the new race. **Structural ordering check.** Concrete
   mechanism: commit 5 sets a package-level
   `var scanClaimSkipWired = true` at the same site where the
   walker check is installed. Commit 6's `Run` startup reads
   this boolean and, if false, aborts folder open with a typed
   error naming the missing commit (the typed error references
   the commit by content, not by sequence number, so it stays
   readable after future renumbering). Why the boolean and not
   a hook function: a missing hook fails at runtime on first
   scan with a nil-pointer dereference; the boolean fails at
   startup with a clear error. The coupling between commit 5
   and commit 6 is intentional — surviving refactors is
   anti-feature here. A future revert of commit 5 alone now
   fails loud at start instead of silently re-opening Gap 5'.
   Pin with `TestStartup_RefusesWithoutClaimSkip`. H12, H13
   land here.
   **Iter-4 composition folds (F7 sweep robustness):** the
   sweep gains two new branches (iter-4 Z1, Z13) — "DB
   unreadable for this folder → leave `.bak` intact, defer to
   restore-from-backup procedure"; "neither file matches
   SQLite hash → `FolderDisabled(unknown)` with diagnostic
   load." Pin with `TestSweep_DBUnreadable_PreservesBak` and
   `TestSweep_NeitherMatches_DisablesWithUnknown`. The
   restore endpoint (commit 9) re-runs the sweep against the
   restored DB after reopen.

   Closes: F7 (§2.1), INV-4 for non-scan paths, Gap 2, Gap 2',
   Gap 3 (write ordering), Gap 4' (`.bak` lifecycle), iter-4
   Z1 (DB-unreadable sweep branch), Z13 (neither-matches
   sweep branch).
7. **β finish — sequence-in-tx + FileIndex disposition (INV-2,
   INV-3) + build-time protocol selection (iter-4 O15) +
   epoch-mismatch handshake invariant (iter-4 Z2) +
   index-model-mismatch metric (iter-4 Z10).** Decision tree,
   executed against the bench number recorded in commit 2.
   The choice between β and hybrid is **a build-time
   constant** (`FILESYNC_INDEX_MODEL = "beta"` or `"hybrid"`)
   stamped on every `IndexExchange` like `protocol_version`;
   the index-exchange handshake rejects any peer with a
   different value and records
   `last_error="filesync_index_model_mismatch"` AND
   increments
   `mesh_filesync_peer_session_dropped{reason="filesync_index_model_mismatch"}`
   (iter-4 Z10) so the dashboard surfaces a warning row when
   a rolling deploy lands a drifted const. Three peers built
   from the same source must produce the same selection — the
   bench runs at build time on the build host, not at runtime
   per peer. DESIGN-v1 §0 carries the field as a
   protocol-level invariant; commit 7 wires the assertion
   into `handleIndex`. The decision number is recorded in
   DESIGN-v1 §0 (durable home), not just the commit message.

   **Epoch-mismatch trigger (iter-4 Z2).** The existing
   sequence-drop trigger in `filesync.go` is extended:
   `if remoteIdx.GetSequence() < peerLastSeq ||
   (remote.Epoch != "" && remote.Epoch != peer.LastEpoch)`.
   The reset path that drops `BaseHashes` and resets
   `LastSeenSequence` now fires on either condition, closing
   the offline-peer-during-restore gap. Pin with
   `TestPeer_OfflineDuringRestore_ResetsOnEpochAlone`.
   DESIGN-v1 §0 names the epoch as a protocol invariant
   alongside `protocol_version`, `device_id`, and
   `FILESYNC_INDEX_MODEL`.

   **Branch A — β path (`BenchmarkLoadIndex_168kFiles < 80 ms`).**
   - Scan loads its working copy at start via `SELECT`; between
     scans, nothing is in memory.
   - `fs.index` field is removed; helpers that read it migrate
     to SQLite-backed accessors or to scan-local arguments.
   - `cloneInto` and `reusableFiles` (P18c recycling) are
     deleted — there is no surviving in-memory map to recycle.
   - Tests: `TestBetweenScans_NoInMemoryIndex` asserts that
     after `runScan` returns, `fs.index` is nil and any peer
     read goes through SQLite.

   **Branch B — hybrid path (`BenchmarkLoadIndex_168kFiles ≥ 80 ms`).**
   - In-memory `FileIndex` is retained between scans as a
     scan-private working copy populated from SQLite at folder
     open. SQLite remains the sole peer-visible truth; INV-1,
     INV-3, INV-4 are unchanged.
   - `cloneInto` recycling stays — it is the perf floor that
     made the hybrid the right call.
   - The 168k-file `SELECT` runs once per folder open, not once
     per scan; subsequent scans diff against the in-memory copy
     and persist deltas.
   - Tests: `TestHybrid_InMemoryRetainedBetweenScans` plus a
     bench that asserts the per-scan cost stays at the
     `cloneInto` floor.

   **Common to both branches:**
   - Every UPSERT carries
     `WHERE excluded.sequence > files.sequence`.
   - Sequence assignment moves *inside* `BEGIN IMMEDIATE` (no
     `folder_meta.sequence` increment outside a tx); paths in
     the working copy carry a "pending" marker rather than a
     pre-assigned sequence, stamped at commit time.
   - `activeCountAndSize` moves to `folderState`, maintained on
     every commit.
   - INV-4 scan-path bullet ("Scan: build pending in memory →
     `BEGIN IMMEDIATE` → sequence-conditioned UPSERT → COMMIT")
     finalizes here — INV-4 closes in full across commits 6
     (non-scan paths) and 7 (scan path).
   - **Tombstone GC** runs every 10th scan as part of the
     scan-tail logic: `DELETE FROM files WHERE folder_id=?
     AND deleted=1 AND mtime_ns < ?` (age threshold mirrors
     today's `tombstoneMaxAge`). Lands here, not commit 2,
     because tombstone GC is a scan-discipline concern and
     commit 7 is where scan discipline finalizes.
   - H11, H14 land here.

   **If the bench result is borderline (75–85 ms),** default to
   the hybrid. The β path's only benefit is one in-memory map
   eliminated; the cost of being wrong (chronic per-scan
   regression on the 168k-file production folder) outweighs
   the structural elegance.

   Closes: INV-2 (final), INV-3, INV-4 (scan path), P2 (per-path
   cost target), P6 (tombstone GC), R9 (atomic cap on `Set`,
   iter-4 Z9), Gap 1, Gap 1' (sequence collision), Gap 4,
   decision §5 #14 (dirty-set cap atomic on `Set`), §5 #15
   (tombstone GC cadence), §5 #16 (β/hybrid build-time
   protocol invariant), §5 #24 (epoch-mismatch protocol
   invariant), §5 #27 (index-model-mismatch metric).
8. **Shutdown deadline propagates into transactions.** New
   `shutdownCtx` with a bounded deadline; `db.BeginTx(ctx, ...)`
   picks it up; over-long transactions roll back and defer.
   `PRAGMA optimize` runs on close. Verify modernc `ctx`
   propagation against the pinned driver version.
   Closes: Gap 6.
9. **`VACUUM INTO` backups — GFS retention + atomic write +
   restore + reopen + listing endpoints.** Bundles four
   folder-level admin endpoints because all four are
   folder-lifecycle operations consumed by runbook §§5–7 and
   all four depend on commit 3's `FolderDisabled` scaffold.
   Split internally into 9a (backup write path + listing) and
   9b (restore + reopen) for review granularity, but lands as
   one tree-green commit.

   **9a — backup write path.** `VACUUM INTO` writes to
   `~/.mesh/filesync/<folder-id>/backups/index-<seq>-<unixns>.sqlite.tmp`
   first (iter-4 Z5 atomic write), runs `quick_check` on the
   `.tmp`, atomically `rename` to the final
   `index-<seq>-<unixns>.sqlite` only on pass; on failure or
   crash, `unlink`. Mode `0600`, `<seq>` = highest committed
   sequence at backup time, `<unixns>` = wall clock at backup
   (iter-4 O18). Runs on a dedicated goroutine so it never
   blocks the writer. Startup sweep (R7 extension) cleans any
   `*.sqlite.tmp` under `<folder>/backups/`. Retention prune
   treats `.tmp` files as invisible. GFS retention: 5 daily +
   4 weekly + 1 monthly (decision §5 #11). Listing endpoint
   `GET /api/filesync/folders/<id>/backups` (iter-4 O9)
   returns `[{path, sequence, created_at, quick_check_ok},
   ...]` sorted by `sequence` descending. Tests:
   `TestBackup_SIGKILLLeavesNoFinalFile`,
   `TestBackup_StartupSweepCleansTmp`,
   `TestRetention_IdempotentOnExtraFile` (iter-4 Z14: writes
   N+1 files, asserts deterministic prune; runs prune twice,
   asserts file set unchanged on second run).

   **9b — restore + reopen.**
   `POST /api/filesync/folders/<id>/restore` (lifecycle L5,
   iter-4 O10) accepts `{backup_path: ...}` and runs a
   five-step procedure: (0) **re-run `quick_check` on the
   chosen backup** (iter-4 Z11) — if it fails, abort with a
   typed error and leave the folder in its current state;
   (1) stop folder; (2) swap `index.sqlite` (and
   `-wal`/`-shm` sidecars) with the chosen backup; (3) bump
   `folder_meta.epoch` to a new value; (4) restart folder
   via the `/reopen` path so the F7 sweep re-runs against
   the restored DB (per the iter-4 Z1 sweep contract — the
   restored DB now has the rewound hash that matches `.bak`,
   so the sweep's restore branch fires and converges state).
   The epoch bump is the load-bearing step (iter-4 O10); the
   re-check on the backup is the load-bearing step against
   between-list-and-swap corruption (iter-4 Z11).

   `POST /api/filesync/folders/<id>/reopen` (per-folder
   reload, iter-4 O12 promoted to v1-blocker) re-runs the
   open path on a single folder — `loadIndexDB`,
   `quick_check`, async `integrity_check` (or sync per
   iter-4 Z8 / commit 11 when SIGKILL recovery is detected),
   scan resume — without touching SSH / proxy / clipsync /
   gateway. Recovery from `disk_full`, `read_only_fs`,
   `dirty_set_overflow`, and the post-restore restart all
   use this endpoint instead of a full process restart.
   Manual SQL editing during recovery is the wrong UX for
   the ship-perfect bar.

   Tests: backup cycle asserts (5 → 5 files; clock + 1 day
   produces daily promotion; clock + 7 days produces weekly
   promotion; retention pruning never deletes the most
   recent of any tier); restore asserts (epoch is bumped;
   pre-swap `quick_check` aborts on failure; peer running
   against pre-restore epoch sees fresh re-baseline on next
   exchange; F7 sweep re-runs after reopen via
   `TestRestore_RunsSweepAfterReopen`); reopen asserts (a
   folder in `FolderDisabled(disk_full)` with disk freed
   transitions back to `enabled` after `/reopen` without
   other folders or other subsystems flapping).

   Closes: L4, L5 (new), Y3, Gap 7,
   decision §5 #11 (GFS), §5 #17 (restore epoch bump),
   §5 #23 (backup filename + listing), §5 #29 (atomic backup
   write, iter-4 Z5), §5 #30 (restore re-check, iter-4 Z11),
   iter-4 O12 (per-folder reload promoted to v1-blocker),
   iter-4 Z14 (idempotent retention prune).
10. **Fault-injection driver + full harness sweep + `SIGKILL`
    recovery test + sync `integrity_check` on detected SIGKILL
    (iter-4 Z8).** The wrapping `driver.Driver` lands under
    `_test.go`, registered as `sqlite_faulty`; `openFolderDB`
    accepts a `driverName` parameter (production default
    `"sqlite"`). Re-run all H-series tests with the wrapper
    enabled to exercise injection paths that couldn't be tested
    without it (`SQLITE_FULL` mid-commit, `SQLITE_IOERR_FSYNC`
    during COMMIT, etc.). One scripted e2e test `SIGKILL`s the
    process mid-scan and asserts clean restart with no data
    lost since the last committed tx (D8).

    **SIGKILL detection signal (iter-4 Z8).** At folder open,
    inspect the WAL: if it contains un-checkpointed frames
    (queryable via `PRAGMA wal_checkpoint`), the process did
    not shut down cleanly. On detection, `integrity_check`
    runs **synchronously** before the folder goes live — the
    ~20 s delay on a 200 MB DB is acceptable on the recovery
    path; the silent live-but-corrupt window between
    `quick_check` and the async `integrity_check` (Z8 hazard)
    is not. Dashboard renders a transient `recovering` state
    during the sync check. On clean WAL (no un-checkpointed
    frames), the existing async path stands. Pin with
    `TestSIGKILLRecovery_RunsIntegrityCheckSync`.

    Closes: decision §5 #13 (plumbed `driverName`),
    §5 #26 (sync integrity_check on SIGKILL recovery,
    iter-4 Z8), iter-4 D8 SIGKILL e2e test.
11. **Benchmarks with pinned baselines.** §4.4 ledger plus the
    commit-2 LoadIndex bench. Separate commit so later
    regressions bisect cleanly.
12. **Schema-evolution migration stub.** `migrate(db, from, to)`
    no-op for v1→v1; invoked unconditionally at open; test
    asserts it fires.
    Closes: V1.
13. **`OPERATOR-RUNBOOK.md` §§1–7 (v1-ship blocker, iter-4
    O13 + O16, plus runbook contradiction fix).**
    First-class deliverable. Sections that block v1 ship
    from dry-run to real on any peer:
    - §1 Before First Run (wipe `~/.mesh/filesync/`, config
      verification, backup directory budget).
    - §2 First Real-Data Run / Dry-Run → Real Flip
      (pre-flight checklist, per-peer sequence with one peer
      at a time, verification gate before next peer, rollback
      procedure if any gate fails). Iter-4 O13 + O16 fold here.
    - §3 Dashboard Triage (healthy signals; what to read
      first when a row goes red; the action string is the
      authoritative next step).
    - §4 Recovery by Disabled Reason (one subsection per
      enum value, citing the action string verbatim; §4.3
      `device_id_mismatch` names option 1 first per
      iter-4 O3).
    - §5 Backup Restore (file location and naming per
      §5 #23; how to pick — sequence + `quick_check_ok`,
      not "newest"; the four-step restore procedure via
      `POST /api/filesync/folders/<id>/restore`; expected
      conflict-file fan-out and how to triage).
    - §6 Peer-State vs File-State Divergence (what peers
      cached during the disabled window; manual
      reconciliation; when to wipe peer state on the other
      peers too; iter-4 O19 / Gap-pending until composition
      pass).
    - §7 Per-Folder Reload (when to use
      `POST /api/filesync/folders/<id>/reopen` instead of a
      full node restart; which enums recover via reopen and
      which require a prior step). Promoted from follow-up
      to v1-blocker (decision §5 #21) because §5.3 cites the
      `/reopen` endpoint and the alternative — manual SQL
      CLI procedures during recovery — is the wrong UX for
      the ship-perfect bar.

    §§8–10 (build-time decisions recorded for operators,
    known limits, escalation) ship in a follow-up commit
    after the first real-data deploy.
    Closes: decision §5 #21 (runbook v1-ship blocker).
14. **DESIGN-v1 banner flip to "implemented".** Verification
    table cites every code site and test name. Closes all
    outstanding checklist boxes. Runbook §§7–10 fold in here
    or in a follow-up — they are not a v1-ship blocker.

Cutover milestones:

- Commit 2 is the storage swap. Gob is gone; SQLite is the only
  on-disk store. One heavy commit, but the no-production-state
  cold swap (DESIGN-v1 §0) is the right shape: no dual-write
  insurance, no burn-in window, no fallback path. Tree green:
  every filesync code path uses SQLite via in-memory `fs.index`
  populated from `loadIndexDB`. Open-path failures log and skip
  pending the FolderDisabled wiring in commit 3.
- Commit 3 lights up the failure machinery. `FolderDisabled` is
  now wired so commits 4+ can transition folders cleanly; the
  blocks-table drop and the device-ID rotation guard ride along
  because they are all open-path concerns.
- Commit 4 flips peer-facing reads to SQLite via the dedicated
  reader handle. After this, in-memory `fs.index` exists only to
  feed scan code (and, depending on the bench, to feed it
  between scans in hybrid mode).
- Commits 5 + 6 are the seam pair. Commit 5 lands the scan
  claim-skip (pure coordination). Commit 6 extends the download
  claim window to span SQLite commit and ships the `.bak`
  lifecycle, CRC, and BaseHash co-atomicity. Commit 6's startup
  assertion makes the ordering structural — a future revert of
  commit 5 alone now fails loud.
- Commit 7 is the structural finish, **provisional**. β stands
  or pivots to hybrid based on the commit-2 bench. Either way,
  sequence-in-tx and conditional UPSERT are enforced. The
  selection is now a **build-time protocol invariant** stamped
  on every `IndexExchange` and asserted at handshake — the
  three peers cannot diverge silently (iter-4 O15).
- Commit 9 makes backup restore a **first-class lifecycle
  operation** (L5): stop folder, swap DB, bump epoch, restart.
  Without the epoch bump, peers' `LastSeenSequence` past the
  rewind silently skips restored rows (iter-4 O10).
- Commit 13 is the **operator runbook**, a v1-ship blocker.
  Sections §1–§6 must be complete before any peer flips from
  dry-run to real. Without it, "highest-risk hour after the
  flip" has no documented procedure (iter-4 O13 + O16).

Every commit in this sequence either strengthens an invariant or
removes superseded code; none adds ballast.

---

## 7. Non-goals for this audit

- Revisiting the DESIGN-v1 schema further. Three documented
  departures: (1) PH columns on `files`; (2) peer_state
  extensions plus `peer_base_hashes` table; (3) `blocks` table
  dropped for v1 (decision §5 #7). The banner-flip commit cites
  all three.
- Anything about the SSH / tunnel / proxy / clipsync layers.
- Performance work unrelated to persistence (scan walk, delta
  compression, watch/scan cadence — tracked separately in
  `PLAN.md`).

---

## 8. Review gate

Before any code from this audit lands beyond commit 1:

- [x] §2 inventory has every row's disposition explicitly chosen.
- [x] §3 risk table has no "unassessed" cells.
- [x] §4 test strategy names a test per kept / redesigned behavior
      and per risk category.
- [x] §5 design calls resolved — iter-2 set (β, per-folder DB,
      drop `persistMu`, `synchronous=FULL`, fault-injection
      driver) plus iter-3 set (drop `blocks` table for v1,
      `prev_path` clear-on-rescan, β provisional pending bench,
      two reader handles at commit 4, GFS backup retention,
      `mmap_size = 64 MiB`, `driverName` plumbed, dirty-set cap
      10k, tombstone GC every 10th scan).
- [x] Iteration-2 gaps (Gap 1–7) closed with INV-1…INV-4 plus
      H11–H15.
- [x] Iteration-3 gaps (Gap 1'–9' surfaced in
      `PERSISTENCE-AUDIT-REVIEW.md`) closed: A1 / Gap 1'
      sequence-in-tx (INV-3); A5 / Gap 2' BaseHash co-atomicity
      (INV-4); A2 / Gap 3' **moot under no-production-state
      cold-swap** (scope correction 2026-04-25; dual-write
      commit dropped); A4 / Gap 4' `.bak` lifecycle (F7,
      commit 6); A3 / Gap 5' scan/download path claim (C6,
      commit 5); A7 / Gap 6' `device_id` mismatch (I7,
      commit 3); A8 / Gap 7' `prev_path` clear-on-rescan (I8);
      B1 / Gap 8' β cost bench-gated (commit 2, decision §5 #9);
      A6 / Gap 9' `blocks` dropped (decision §5 #7, commit 3).
      Iter-3 obvious-fix list (review §D) folded into commits
      2, 3, 6, 8, 10.
- [x] **Step 1 — §6 commit sequence reviewed** (closed
      2026-04-25). Each commit's scope is bounded; cross-
      references to §2 / §5 / Gap-numbers consistent across the
      13-commit sequence. Seven findings raised on the bounded-
      scope review; all folded into §6:
      - Finding 1: Commit 6 startup-assertion uses a boolean
        sentinel (`var scanClaimSkipWired = true` in commit 5;
        `Run` startup reads and aborts on false).
      - Finding 2: Gap 7' / I8 added to commit 2's `Closes:`
        line; the saveIndex UPSERT clears `prev_path` unless
        a fresh hint is being recorded.
      - Finding 3: Iter-2 Gap 5 (integrity_check sync/async
        split) added to commit 3's `Closes:` line.
      - Finding 4: INV-4 scan-path bullet added to commit 7's
        `Closes:` line; INV-4 closes in full across commits 6
        (non-scan paths) and 7 (scan path).
      - Finding 5: Decision §5 #12 (`mmap_size = 64 MiB`)
        added to commit 2's `Closes:` line; pragma lands in
        `applyFolderDBPragmas`.
      - Finding 6: Tombstone GC (P6 / decision §5 #15) placed
        in commit 7, not commit 2 — scan-discipline concern
        finalizes with the rest of scan in commit 7.
      - Finding 7: H1, H2, H6, H7, H9, H10 stripped from §6
        prose (informal labels with no §4 anchor); H11–H15
        retained and remain pinned in §4.1.

      Sub-checks for the §6 reviewer:
      - **Commit 5 → commit 6 ordering is structural, not
        disciplinary.** Commit 6 carries a startup assertion
        that commit 5's claim-skip path is wired (sentinel
        verified once on `Run`; failure aborts the folder
        open with a typed error naming the missing claim-skip
        code path). Without this, a future revert of commit 5
        alone leaves commit 6 silently running with the Gap 5'
        race re-opened. ~10 LOC + one test in commit 6. Pin
        with `TestStartup_RefusesWithoutClaimSkip`.
      - **Commit 2 is heavy by design.** Storage swap, gob
        deletion, dirty-set, encapsulation, bench gate all
        ride together because the no-production-state cold-
        swap posture means there is no win in splitting them.
        Reviewer confirms the commit message and tests carve
        the change cleanly enough to bisect within it.
      - Each commit's `Closes:` line names every Gap /
        inventory row / decision number it closes; no Gap
        appears in two `Closes:` lines (each gap closes
        exactly once); Gap 3' explicitly cited as "moot under
        no-production-state cold-swap" in the §8 closure
        list, not in any `Closes:` line.
      - No `Closes:` line names a Gap that hasn't appeared in
        the iter-2 / iter-3 banner inventory.
      - Cutover milestones paragraph references the new 13-
        commit numbering consistently with the numbered list.
- [x] **Step 2a — Iteration-4 ops-lens review** (closed
      2026-04-25). Surfaced 19 findings; seven structural
      changes folded into the audit, plus a runbook
      contradiction fix on the second pass:
      - O15 promoted to protocol-level invariant (decision
        §5 #16, commit 7 prose, DESIGN-v1 §0).
      - O11 promoted to 🚨 structural deliverable: per-enum
        action strings (decision §5 #18, R8, commit 3).
      - O10 added as new lifecycle row L5 (restore-from-
        backup with epoch bump; decision §5 #17, commit 9).
      - O3 named the default branch (restore device-id
        first; decision §5 #20, I7, runbook §4.3).
      - O8 expanded to inline diagnostic load on `unknown`
        (decision §5 #19, R8, commit 3).
      - O13 + O16 folded into single runbook deliverable
        (decision §5 #21, new commit 13).
      - O4 split enum: `metadata_parse_failed`
        (decision §5 #22, R8, commit 3).
      - O12 promoted from follow-up to v1-blocker on
        contradiction-fix pass: `/reopen` endpoint ships at
        commit 9; runbook §7 promoted to v1-blocker section
        and filled in. Resolves the §5.3 vs §7 contradiction
        the original ops fold left open.
      - Runbook §2.1 dangling forward-pointer to §8 replaced
        with concrete handshake-rejection language.
      - Bonus folds: O9 + O18 backup naming + listing
        (decision §5 #23, commit 9).

      Source review: `PERSISTENCE-AUDIT-REVIEW-ITER4-OPS.md`.
- [x] **Step 2b — Iteration-4 composition-lens review**
      (closed 2026-04-25). Surfaced 16 findings (Z1–Z16) on
      failure-mode composition during the first hour after
      the dry-run flip. All 10 §E decisions resolved:
      - Z1 → F7 sweep gains "DB unreadable" branch
        (commit 6); restore re-runs sweep post-reopen
        (commit 9 9b).
      - Z2 → epoch-mismatch protocol invariant in DESIGN-v1
        §0; trigger extension in commit 7 with
        `TestPeer_OfflineDuringRestore_ResetsOnEpochAlone`.
        Code-verification note: existing implementation has
        branch A semantics on sequence-drop trigger only;
        promotion adds the epoch-mismatch trigger (~5 LOC).
      - Z3 → runbook §2.4 cites the epoch contract; §2.3
        verification gate adds the LastEpoch check on B/C.
      - Z4 → runbook §1.1 rewritten "wipe once, before first
        dry-run, never again"; §2.4 path-scope distinction
        documented explicitly (folder-scoped vs. parent).
        Code verification: §2.4 step 3 and §4.3 option 2
        are both correctly folder-scoped; only §1.1 needed
        the wording fix.
      - Z5 → backup write-temp-then-rename; startup sweep
        cleans `.tmp` (commit 9 9a, decision §5 #29).
      - Z6 → branch (A) cancel in-flight tx on disabled
        transition (R8 + commit 3, decision §5 #25).
      - Z7 → runbook hardening only — wall-clock check in
        §2.1, "one peer at a time" warning in §2.2.
      - Z8 → sync `integrity_check` on detected SIGKILL
        recovery (commit 10, decision §5 #26).
      - Z9 → R9 atomic cap on `Set` (commit 7, decision
        §5 #24 wiring + R9 update).
      - Z10 → process-level `index_model_mismatch` metric +
        runbook §3.4 (commit 7, decision §5 #27); §8 stays
        in first follow-up.
      - Z11 → restore re-runs `quick_check` pre-swap
        (commit 9 9b, decision §5 #30).
      - Z12 → `TestDisabledReasonActions_AllEnumsHaveAction`
        coverage test (commit 3).
      - Z13 → F7 "neither matches" branch →
        `FolderDisabled(unknown)` with diagnostic load
        (commit 6).
      - Z14 → `TestRetention_IdempotentOnExtraFile` (commit 9
        9a).
      - Z15 → doc-test (b)
        `TestRunbookActionStringsMatchMap` parses runbook §4
        against the map (commit 3, decision §5 #28).
      - Z16 → §9 stays follow-up; §7 already promoted
        absorbs the multi-folder `disk_full` blast radius.

      Source review:
      `PERSISTENCE-AUDIT-REVIEW-ITER4-COMPOSITION.md`.
