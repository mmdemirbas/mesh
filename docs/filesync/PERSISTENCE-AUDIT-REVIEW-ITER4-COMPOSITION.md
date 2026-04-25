# Persistence-Audit — Iteration-4 Composition-Lens Review

> Independent review of `PERSISTENCE-AUDIT.md` (post iter-4 ops fold),
> `PERSISTENCE-AUDIT-REVIEW.md` (iter-3), `PERSISTENCE-AUDIT-REVIEW-ITER4-OPS.md`
> (iter-4 ops), `DESIGN-v1.md` (post `FILESYNC_INDEX_MODEL` invariant), and
> `OPERATOR-RUNBOOK.md` (skeleton).
>
> Lens: **failure-mode composition during the first hour after the
> dry-run → real-data flip on three peers.** Prior reviews handled
> assertions (iter-2), transitions (iter-3), and operator workflow
> (iter-4 ops) in isolation. This pass asks: when two or three of those
> failure modes fire within a short window, what breaks that no single
> lens caught?
>
> Bar: ship-perfect. Atomic rebuild of all three peers is acceptable;
> data corruption, sync confusion, or runaway resource use is not.
>
> Status: draft, 2026-04-25.

---

## Legend

| Icon | Meaning                                                  |
|------|----------------------------------------------------------|
| 🚨   | Composition can corrupt data, lose tombstones, or strand peers. Must resolve. |
| ⚠️   | Composition has under-specified behavior; design or wording fix required. |
| 🤔   | Decision needed — composition exposes an open question. |
| 🔧   | Obvious fix — wiring or test gap. |
| 💬   | Nit / clarity. |

---

## Executive summary

| #   | Composition                                                         | Severity |
|-----|---------------------------------------------------------------------|----------|
| Z1  | F7 sweep meets unreadable SQLite                                    | 🚨       |
| Z2  | Restore epoch bump vs. concurrent peer index exchange               | 🚨       |
| Z3  | Dry-run flip rollback (§2.4) vs. peer state cached on un-flipped peers | 🚨    |
| Z4  | §1.1 wipe instruction vs. device-id continuity across dry-run → real | 🚨      |
| Z5  | SIGKILL during VACUUM INTO → partial backup file enters retention   | ⚠️       |
| Z6  | `integrity_check_failed` mid-life vs. in-flight writer transaction  | ⚠️       |
| Z7  | Two peers flip simultaneously (procedure violation) + clock skew + first-sync C1 | ⚠️ |
| Z8  | SIGKILL between `quick_check` pass and `integrity_check` completion | ⚠️       |
| Z9  | `disk_full` + in-flight download `.bak` + dirty-set cap during walk | ⚠️       |
| Z10 | `FILESYNC_INDEX_MODEL` mismatch fires mid-session, not at startup   | ⚠️       |
| Z11 | Backup quick_check_ok at listing time vs. swap time                 | 🔧       |
| Z12 | Action-string map missing entry for new enum                        | 🔧       |
| Z13 | F7 sweep "neither matches" branch unspecified                       | 🔧       |
| Z14 | Backup retention prune with N+1 files after crash                   | 🔧       |
| Z15 | Action-string drift between map, dashboard, API, and runbook §4     | 💬       |
| Z16 | Two folders `disk_full` + per-folder reload deferred to §7          | 💬       |

**Counts:** 🚨 4 · ⚠️ 6 · 🔧 4 · 💬 2

---

## A. Critical compositions — must resolve before code lands

### 🚨 Z1 · F7 sweep meets unreadable SQLite

**Sequence.**

1. Sustained `disk_full` while a download is at step 2 (rename temp →
   original done; commit not yet attempted).
2. The DB write (step 3) returns `SQLITE_FULL`. The folder transitions
   to `FolderDisabled` reason `disk_full`.
3. The download rollback (step 5: rename `.bak` → original, unlink temp)
   begins — but the writer connection has been cancelled by the
   disabled transition; the rollback FS calls succeed (renames need no
   new blocks) but the post-rollback `releasePath` and metric write may
   not run cleanly.
4. Operator frees disk. Restarts node. Folder open runs `quick_check`.
5. `quick_check` fails because the WAL was truncated mid-write and a
   page is unreadable. Folder enters `quick_check_failed`. Sweep (per
   F7) wants to read SQLite for path P to decide between unlink-`.bak`
   and restore-`.bak`. **It cannot.**

**What the audit says.** F7 sweep logic: "if SQLite carries the new
content's hash → unlink `.bak`; else → restore `.bak` → original,
unlink any temp." Both branches presuppose a readable DB.

**What breaks.** The sweep has no defined branch for "DB unreadable."
The most plausible implementations are all wrong:

- Skip sweep silently → `.bak` lives on; later restore-from-backup
  swaps in a DB that knows nothing about `.bak`; on next scan the file
  on disk has new content (step 2 was applied), the restored DB has
  the *old* hash for that path; scan re-hashes, marks the path dirty,
  propagates the new content to peers as if the operator had edited
  it. The "old content" the operator expected to recover is gone.
- Aggressively restore `.bak` → original without consulting SQLite →
  works if step 3 hadn't actually committed, but data-loss if it had
  (some `SQLITE_FULL` codepaths return after a partial WAL flush; on
  rare disk recoveries the commit can land).

**Fix (no decision needed).**

- F7 sweep gains a third branch: "SQLite for this folder is unreadable
  (open failed or `quick_check` failed)." On that branch, the sweep
  does **nothing** with `.bak` (does not unlink, does not restore).
  Folder enters `FolderDisabled` with the SQLite-derived reason and
  the `.bak` file is left intact for the operator's restore procedure
  to encounter.
- Restore procedure (§5.3) gains a pre-step: after step 4 (folder
  restart), re-run the F7 sweep against the restored DB. The restored
  DB has the rewound hash for path P, which matches `.bak`'s content.
  Sweep restores `.bak` → original. State converges.
- Pin with `TestSweep_DBUnreadable_PreservesBak` and
  `TestRestore_RunsSweepAfterReopen`.

**Audit handling.** Not handled. §2.1 F7 specifies two branches;
§2.2 R8 lists the disabled reasons but does not say sweep defers to
operator on unreadable DB; §2.7 L5 restore does not name a post-restart
sweep step.

---

### 🚨 Z2 · Restore epoch bump vs. concurrent peer index exchange

**Sequence.**

1. Peer A initiates restore. Step 1 stops the folder; the DB is
   closed; the admin handler returns "folder unavailable" to peer B's
   in-flight index exchange.
2. Step 2 swaps `index.sqlite` (and `-wal`/`-shm`) with the chosen
   backup.
3. Step 3 opens the new DB, bumps `folder_meta.epoch`, closes.
4. Step 4 restarts the folder.
5. Peer B retries its index exchange. The handler now serves data from
   the restored DB with the new epoch.

**What breaks.** The audit (§2.7 L5) and DESIGN-v1 §0 do not specify
the protocol-level contract for an epoch change visible to peers.
Existing code (`index.go`, `filesync.go` PeerState handling) carries
`LastEpoch` and `PendingEpoch` fields, and `handleIndex` already does
some epoch-aware filtering — but the audit, the runbook §5.4, and
§6.1 all assert "peers detect the rewind and re-baseline" without
naming the exact mechanism, the rejection code, the test that pins
it, or the failure mode if the mechanism does not fire. Iter-4 ops
review O19 explicitly raised this; the resolution folded into commit
9 says "the epoch bump is the load-bearing step" but does not name
the receiver-side contract.

**Why composition makes this acute.** During the first hour, peers
sync rapidly. A restore on peer A coincides with an in-flight index
exchange from peer B. Two protocol-visible peer outcomes are
possible:

- Peer B sees the new epoch on the *retried* exchange, drops cached
  `BaseHashes` for folder F under (peer=A), and triggers a full
  re-sync. **Correct.**
- Peer B treats the epoch as just another field on the row,
  preserves `BaseHashes`, and serves a delta `WHERE seq >
  LastSeenSequence`. The rewound rows whose new sequence is below
  peer B's `LastSeenSequence` are silently skipped. **Data loss.**

The audit cannot tell us which branch is the implemented one.

**Fix.**

- Promote epoch handling to a named protocol invariant in DESIGN-v1
  §0 alongside `protocol_version`, `device_id`, and
  `FILESYNC_INDEX_MODEL`. Same shape: the receiver compares incoming
  `Epoch` against the cached `PeerState.LastEpoch`; on mismatch, drop
  `BaseHashes` and `LastSeenSequence` for that (folder, peer) pair
  and trigger a full re-sync. Record the rejection cause in
  `last_error` for visibility.
- Pin the receiver-side contract with
  `TestRestore_PeerReBaselinesOnEpochChange` (peer A restores; peer
  B's next exchange resets PeerState; full re-sync drives every
  changed row across).
- Audit §2.7 L5 cites the test name verbatim.
- Runbook §5.4 cites the protocol assertion ("the receiver MUST drop
  BaseHashes on epoch change"), not just the load-bearing claim.

**Audit handling.** O19 was raised in iter-4 ops, but its closure
text in §2.7 L5 / runbook §5.4 names the load-bearing step without
nailing the receiver contract. The composition (concurrent exchange
during restore) makes the gap exploitable in the first hour.

---

### 🚨 Z3 · Dry-run flip rollback vs. peer state on un-flipped peers

**Sequence.**

1. Peer A flips dry-run → real per runbook §2.2. Peers B and C remain
   in dry-run.
2. Peer A runs for ~50 minutes under real direction. During that
   window, peer A sends index exchanges to B and C. B and C, even in
   dry-run, accept the exchanges and update their `PeerState` for
   peer A: `LastSeenSequence`, `BaseHashes`, `LastEpoch`.
3. Peer A's verification gate (§2.3) fails at minute 55. Operator
   triggers §2.4 rollback: stop mesh, revert config to dry-run,
   `rm -rf ~/.mesh/filesync/<folder-id>/`, `mesh up`.
4. Peer A regenerates folder epoch (per I2: delete DB → new epoch on
   re-create). Sequence resets to 0.
5. Operator re-attempts the flip later (after fixing whatever caused
   the gate failure). Peer A starts at epoch E2, sequence 1..M.
6. Peer B's `PeerState` for peer A still carries epoch E1,
   `LastSeenSequence = K` (where K » M). **Per Z2's gap**, if the
   epoch-change re-baseline contract is not formally enforced, peer
   B's delta query `WHERE seq > LastSeenSequence` skips peer A's new
   1..M.

**What the audit says.** §2.4 rollback step 6 ("the dry-run scan
rebuilds from the on-disk file content") and the closing claim ("no
peers were affected because no peer flipped after the failed one")
are wrong about peers B and C. They were *not* "affected" in the
file-system sense — but their `PeerState` rows for peer A were
populated by 55 minutes of dry-run-permitted index exchanges.

**What breaks.**

- If Z2 is fixed (epoch change forces peer re-baseline) → rollback is
  safe; B and C re-baseline against the new epoch on next exchange.
- If Z2 is not fixed → rollback silently strands peer A. B and C
  never resync the rolled-back content.

**Fix.**

- Z2 is the structural fix; Z3 is a citation gap.
- Runbook §2.4 must explicitly state: "Rolling back a flipped peer
  also forces every other peer to re-baseline its `PeerState` for
  this peer, via the epoch-change protocol contract (Z2). Do not
  manually wipe peer state on B / C; the next index exchange
  resolves it."
- Add a third pre-condition to §2.3 verification gate: peer B and C
  show updated `PeerState.LastEpoch` for peer A after the flip; this
  proves the channel works and the rollback path will trigger
  re-baseline cleanly.

**Audit handling.** Not handled. §2.4 implies un-flipped peers are
inert; they are not. The composition (dry-run exchanges populate
`PeerState` even though file content does not move) is the gap.

---

### 🚨 Z4 · §1.1 wipe instruction vs. device-id continuity

**Sequence.**

1. Operator follows runbook §1.1 before the first dry-run run:
   `rm -rf ~/.mesh/filesync/`. This deletes
   `~/.mesh/filesync/device-id` along with all folder DBs.
2. `mesh up` regenerates device-id D1 (per DESIGN-v1 §0:
   "Created atomically on first run").
3. Dry-run runs for days. Peers B and C see peer A as device-id D1
   in every IndexExchange (`device_id` is always stamped — DESIGN-v1
   §0 wire shape).
4. Before the real-data flip, the operator re-reads §1.1 ("Wipe
   `~/.mesh/filesync/` on every peer") and applies it again, on
   every peer, "to be safe." Device-id files are deleted on all
   three peers.
5. Real-data flip starts. Peer A regenerates device-id D2 ≠ D1. Peer
   B regenerates device-id D2' ≠ D1'. Peer C similarly.
6. Index exchanges between A, B, C now show all-new device-ids. Every
   peer's cached `PeerState` keys (which include `device_id`) are
   stale. Vector clocks on every file have entries for D1, D1', D1''
   — entries that no longer correspond to any live peer.

**What the audit says.** Runbook §1.1 reads:

> The v1 protocol is a cold swap. Any leftover state from earlier
> dev builds must be removed before the first `mesh up`.
>
> ```sh
> # On every peer:
> rm -rf ~/.mesh/filesync/
> ```

"First `mesh up`" is ambiguous between "the very first invocation
ever" and "the first invocation in real-mode." A careful operator
who reads §1.1 again before the §2 flip will rewipe.

**What breaks.**

- Vector-clock continuity between dry-run and real-mode is gone.
  Every file's `version` carries a now-irrelevant device-id from the
  dry-run identity.
- C2 classifier on first real exchange: every path looks
  "concurrent" (vectors with no overlapping device-id keys never
  dominate), driving `.sync-conflict-*` fan-out across the entire
  folder. The very failure §2 was designed to avoid.
- Recovery (§4.3 device_id_mismatch option 1: "restore the original
  ID from a backup source") presupposes a backup of the **first**
  device-id. After Z4 fires, no backup exists — the operator has
  written over it.

**Fix.**

- §1.1 wording: "Wipe `~/.mesh/filesync/` **once**, before the very
  first dry-run run on each peer. Do **not** rewipe between dry-run
  and the real-data flip. The device-id file
  (`~/.mesh/filesync/device-id`) is created on the first run and is
  the stable identity of this peer; preserve it across the flip."
- §1.1 adds a verification step: after the first dry-run start, read
  the device-id file and back it up out-of-band (Time Machine,
  Syncthing copy, ops vault). §2.1 pre-flight already requires this;
  §1.1 should plant the requirement at first run, not at flip.
- Optionally: the `mesh up` startup can refuse to regenerate
  device-id on a folder DB that already carries a populated
  `folder_meta.device_id` (this *is* exactly the I7 mismatch path) —
  closing the loop so a Z4 wipe surfaces as `device_id_mismatch` on
  the first folder open after wipe, instead of silently regenerating.
  But: with §1.1 wiping the folder DB too, this fallback does not
  trigger. The wording fix is load-bearing.

**Audit handling.** Not handled. §1.1's wording predates the device-id
recovery story (§4.3 option 1). The composition (wiping device-id
breaks the recovery option you might need later) is the gap.

---

## B. Architectural compositions — design or wording fix required

### ⚠️ Z5 · SIGKILL during `VACUUM INTO` → partial backup contaminates retention

**Sequence.**

1. Backup goroutine starts `VACUUM INTO
   '<folder>/backups/index-<seq>-<unixns>.sqlite'`. Backup file is
   being written page-by-page.
2. SIGKILL. Process dies. Partial file is on disk under its final
   name.
3. Restart. `quick_check` on the main DB passes. Folder goes live.
4. Next backup trigger fires. Backup goroutine writes
   `index-<newseq>-<newunixns>.sqlite`. Retention prune runs.
5. Retention prune's GFS algorithm sees N+1 backup files. The partial
   one (with `quick_check_ok=false`) competes for a tier slot. Per
   decision §5 #11 (5 daily + 4 weekly + 1 monthly), GFS picks by
   age/sequence.
6. Operator triggers a restore. `GET /api/filesync/folders/<id>/backups`
   lists the partial file with `quick_check_ok=false`. Operator skips
   it and picks the highest-sequence `quick_check_ok=true` backup.

**What breaks.**

- The partial file lives in the retention pool indefinitely (until
  pushed out by newer backups). Disk-budget computations from §1.3
  ("worst case 10 × DB size") understate by one slot.
- Worst-case (a series of crashes during VACUUM INTO): retention
  fills with `quick_check_ok=false` files; operator has no recovery
  path.
- Listing endpoint runs `quick_check` on every backup file at list
  time — a 200 MB backup × 11 backups × 1 second per check = ~11 s
  on every operator query. (See Z11 for a related issue.)

**Fix.**

- VACUUM INTO writes to
  `index-<seq>-<unixns>.sqlite.tmp`; on `VACUUM` success and
  post-VACUUM `quick_check` pass, atomically `rename` to the final
  name. On failure, `unlink`.
- Startup sweep (R7) extends to clean any `*.sqlite.tmp` under
  `<folder>/backups/`.
- Retention prune treats `*.sqlite.tmp` as invisible.
- Pin with `TestBackup_SIGKILLLeavesNoFinalFile` and
  `TestBackup_StartupSweepCleansTmp`.

**Audit handling.** Not handled. §2.7 L4 / commit 9 specifies the
final filename pattern (decision §5 #23) without naming the
write-temp-then-rename pattern. The audit relied on F2 ("`tmp` +
`fsync` + `rename` per write") for the main DB but did not extend it
to backups.

---

### ⚠️ Z6 · `integrity_check_failed` mid-life vs. in-flight writer transaction

**Sequence.**

1. Folder open. `quick_check` passes synchronously. Folder goes live.
2. Background goroutine starts `integrity_check`.
3. While `integrity_check` is running, scan walk completes; scan
   opens `BEGIN IMMEDIATE`; UPSERT phase begins on 5 000 dirty rows.
4. `integrity_check` reports a failure. The disabled-transition
   handler fires.
5. The scan tx is mid-commit. Two possible behaviors, neither pinned
   by the audit:
   - **(A)** Disabled transition cancels the writer context. Tx
     rolls back. No new rows are written to the (corrupt) DB.
   - **(B)** Disabled transition flips a flag but lets the in-flight
     tx commit. Up to 5 000 new rows are appended to the corrupt DB.

**What the audit says.** §2.2 R8 says "the folder enters
`FolderDisabled`" on `integrity_check_failed` but does not specify
in-flight tx fate. Runbook §4.2 implies (B) — "writes from the live
window are still in the DB but will be lost on restore" — but does
not call (B) the chosen branch and pin it with a test.

**What breaks.** Either choice is workable, but the choice is
load-bearing for downstream invariants:

- Branch (A) — clean: corrupt DB receives no further writes, restore
  rewinds to a known-good state, peers re-baseline via Z2.
- Branch (B) — implicitly chosen: the 5 000 new rows participate in
  delta exchanges before the operator notices. Peer B downloads new
  content based on metadata that the source DB cannot vouch for
  (failed `integrity_check`). On restore, the propagation is reverted
  but peer B may have already overwritten its own files.

**Fix.**

- Audit §2.2 R8 picks (A) explicitly: disabled-transition cancels the
  writer context; in-flight tx rolls back; the disabled-state JSON
  carries `tx_in_flight_rolled_back: true` for operator visibility.
- Runbook §4.2 wording changes from "writes from the live window are
  still in DB but lost on restore" to "writes that committed before
  the integrity-check failure are in DB; the in-flight transaction
  at failure time was rolled back. Restore rewinds to the chosen
  backup; everything between the backup and the failure is lost."
- Pin with
  `TestIntegrityCheckFailedMidLife_RollsBackInFlightTx`.

**Audit handling.** Under-specified. §4.1 names
`TestIntegrityCheck_QuickSyncFullAsync` (H15) but the test asserts
the transition happens, not the in-flight-tx fate.

---

### ⚠️ Z7 · Two peers flip simultaneously + clock skew + first-sync C1

**Sequence.**

1. Operator misreads runbook §2.2 and flips peers A and B at
   t = 0 instead of one-at-a-time.
2. Both run their first real-mode scan in parallel. Each computes
   hashes from local on-disk content (which the dry-run period left
   in whatever state the user had on each peer — not necessarily
   identical between A and B).
3. Both index exchanges fire. For paths where A's hash ≠ B's hash:
   - `BaseHashes[(peer, path)]` is empty on both sides.
   - `last_sync_ns == 0` on both sides.
   - C2 falls through to C1: pick by mtime, then deterministic
     device-id (per DESIGN-v1 §1).
4. Wall-clock skew between A and B — even ±10 minutes is plausible
   on machines that have not exchanged NTP recently — picks the
   "wrong" winner per file. Operator's expected source-of-truth
   loses to the other peer's stale copy.

**What the audit says.** Runbook §2.2 is the only protection: "one
peer at a time." There is no programmatic guard.

**What breaks.**

- Not data corruption (both copies survive: winner overwrites,
  `.sync-conflict-*` preserves the loser).
- But the operator's "this peer is the source of truth" intent is
  silently violated, by mtime skew.
- Recovery is per-file manual triage. On a 168k-file folder, this
  takes hours of operator time and is the exact failure §2 was
  written to prevent.

**Fix.**

- Treat this as a procedural-only mitigation, but harden the
  procedure:
  - Runbook §2.1 pre-flight gains: "Verify wall-clock skew between
    all three peers is < 60 s. Run `date -u` on each peer; record
    the offset. If any peer is off by > 60 s, fix NTP first."
  - Runbook §2.2 step 1 gains a stronger warning: "Flipping more
    than one peer in parallel during dry-run → real is the highest-
    risk operator error in v1. Conflict-storm recovery on a 168k-
    file folder takes hours of manual triage."
- Optionally (deferred to §7 follow-up): a soft programmatic guard.
  When a folder transitions from dry-run to send-receive *and* its
  `folder_meta.created_at` is within the last hour *and* peer
  PeerState rows show another peer that also just transitioned, log
  a `WARNING: simultaneous-flip-detected` line and require an
  operator override (`mesh up --confirm-simultaneous-flip`). Audit
  scope-call: deferred or shipped at v1?

**Audit handling.** §6 commit 13 (runbook §1–§6) implicitly relies
on operator discipline. The composition (procedure violation + clock
skew + first-sync) is the realistic failure mode. The runbook is
v1-ship-blocker, so harden it now.

---

### ⚠️ Z8 · SIGKILL between `quick_check` pass and `integrity_check` completion

**Sequence.**

1. Folder open. `quick_check` passes synchronously.
2. Folder goes live. Scan starts. Writes 50 rows.
3. SIGKILL.
4. Restart. `quick_check` runs again — passes again (the corruption,
   if any, is in pages that `quick_check` does not read).
5. Folder goes live. Scan starts. Writes 100 rows.
6. After ~30 s, background `integrity_check` from the second start
   completes. **Fails.**
7. Folder transitions to `integrity_check_failed`. Operator restores.

**What the audit says.** §2.2 R2 specifies the two-phase check.
What is missing: between step 4 and step 6, the folder has been
"live but corrupt." Operator restores; per Z6 (B), the writes from
that live window are lost on restore.

**What breaks.**

- After a SIGKILL, an operator's reasonable model is "the DB is
  either OK or visibly broken." It is neither: it is silently
  corrupt and *un-detected* by `quick_check`, but provisionally
  trusted by the system for ~30 s.
- During those 30 s, peer index exchanges fire and propagate the
  potentially-corrupt rows.

**Fix.**

- After a SIGKILL recovery (detectable: the WAL has un-checkpointed
  pages on open), the audit should require `integrity_check` to
  complete **synchronously** before the folder goes live. The
  ~10 MB/s integrity_check on a 200 MB DB ≈ 20 s. That delay is
  acceptable on a recovery path; the silent live-but-corrupt window
  is not.
- Detection signal: WAL file size > N at open, OR WAL has
  un-checkpointed frames (queryable via SQLite). On match, run
  `integrity_check` synchronously; on mismatch, the existing async
  path stands.
- Runbook §3 (dashboard triage) gains a new transient state:
  `recovering` (synchronous integrity check in progress).
- Pin with `TestSIGKILLRecovery_RunsIntegrityCheckSync` (D8 covers
  the clean-restart case; this is the corrupt-restart case).

**Audit handling.** Not handled. D8 covers the happy path. The
composition (SIGKILL + WAL recovery + delayed integrity failure +
peer propagation) is the dangerous path.

---

### ⚠️ Z9 · `disk_full` + in-flight download + dirty-set cap during walk

**Sequence.**

1. Sustained `disk_full`. Successive scans fail at COMMIT;
   `disk_full` reason fires on first failure → folder enters
   `FolderDisabled`. **But:** `disk_full` is the reason. Dirty-set
   never reaches 10 K because the folder disabled itself first.

   *Compose with another scenario:*

2. Disk has *intermittent* write failures (a flaky USB drive, a
   network FS hiccup). `BEGIN IMMEDIATE` succeeds; UPSERTs succeed;
   COMMIT fails with `SQLITE_IOERR_FSYNC` (transient) → tx rolls
   back, folder does NOT enter disabled (the audit only names
   `disk_full` and `read_only_fs` as committed-time failure
   reasons). Dirty-set retains the entries.
3. Next scan walk finds new dirty entries. Walk-time sets are added.
4. The walk eventually crosses 10 000 entries in the dirty-set.

**What the audit says.** §2.2 R9 specifies the cap and the
`dirty_set_overflow` reason. §6 commit 7's tombstone-GC commit
finalizes the scan-discipline path. Neither names what happens when
the cap fires **mid-walk**:

- Does the walk abort, dropping any in-memory updates from this
  cycle?
- Does the walk complete and *then* the disabled transition fires?
- If a download is in-flight and contributes to the dirty-set
  (per the design: "dirtyPaths populated by Set / Delete as a side
  effect"), can the cap fire from the download goroutine while a
  scan walk is also adding entries?

**What breaks.**

- Race-window: scan walk and download both add to dirty-set. The
  10 001st entry could come from either. The cap-firing goroutine
  must atomically test-and-set; if not, two goroutines may both
  trigger disabled transitions (idempotent → benign) but one may
  also be mid-`BEGIN IMMEDIATE` and leak the connection.
- If the cap is checked only on `Set`, a walk that adds entries
  inside a Go-side range loop without checking the cap before
  `BEGIN IMMEDIATE` will overshoot.

**Fix.**

- §2.2 R9 gains: "The cap is checked atomically inside the
  dirty-set's mutex on every `Set`. On overflow, the call returns a
  typed `errDirtySetOverflow`; callers (scan walker, download
  commit, rename, delete) must propagate it without opening
  `BEGIN IMMEDIATE`. The disabled transition fires from the caller's
  goroutine, after the in-flight tx (if any) has rolled back."
- Pin with `TestDirtySetCap_FiredFromDownloadGoroutine_NoLeak` and
  `TestDirtySetCap_FiredMidWalk_NoTxOpened`.

**Audit handling.** Under-specified. §2.2 R9 names the cap and the
enum but does not name the abort sequence under concurrency.

---

### ⚠️ Z10 · `FILESYNC_INDEX_MODEL` mismatch fires mid-session, not at startup

**Sequence.**

1. Three peers built and deployed from the same source: all carry
   `FILESYNC_INDEX_MODEL = "hybrid"` per the commit-2 bench result.
2. Operator finishes deploy on peers A and B. Peer C is delayed.
3. While C is delayed, an in-flight build for peer C lands on a
   different revision (bench-rerun was inconclusive; const
   accidentally flipped to `"beta"`).
4. Peer C deploys. Index exchange to peer A: handshake assertion
   fires. Peer A logs `last_error = "filesync_index_model_mismatch"`
   and drops the session.

**What the audit says.** DESIGN-v1 §0 specifies the assertion. Per
the wire shape: "Stamped on every `IndexExchange`."

**What breaks.**

- Not data corruption — the handshake succeeds in dropping the
  session.
- But: filesync between A↔C and B↔C is silently down. Peer A and B
  continue to sync (correctly). Peer C accumulates local changes that
  do not propagate.
- Operator visibility depends on whether the dashboard surfaces
  `last_error = "filesync_index_model_mismatch"`. The audit does not
  name a metric or a disabled-state for the receiving peer; only a
  per-PeerState `last_error` field that the dashboard *should* show.
- Worst case: operator sees A and B happy on the dashboard, C looks
  happy too (its own scans run; the failure is on the per-peer row,
  not the per-folder row). A 24-hour window of silent isolation.

**Fix.**

- A1 mismatch fires a **process-level** metric:
  `mesh_filesync_peer_session_dropped{reason=
  "filesync_index_model_mismatch"}`. The dashboard surfaces a
  warning row when the metric is > 0.
- The runbook §3 (dashboard triage) names the metric and points at
  §8 (build-time decisions for operators).
- §8 ships in v1 (not deferred to follow-up): one paragraph on
  "how to read the deployed `FILESYNC_INDEX_MODEL` from each peer's
  binary, and how to detect a mid-rollout drift."

**Audit handling.** §6 commit 7 / DESIGN-v1 §0 specify the
assertion; visibility is implicit. The composition (rolling deploy
with const drift) makes the visibility gap exploitable.

---

## C. Obvious fixes — wiring or test gaps

### 🔧 Z11 · Backup `quick_check_ok` at listing time vs. swap time

**Composition.** Operator queries
`GET /api/filesync/folders/<id>/backups` at t = 0; sees backup B
with `quick_check_ok=true`. Operator queries
`POST /api/filesync/folders/<id>/restore` at t = 30 s. Between the
two requests, an FS event (a passing storage glitch, an unrelated
process touching the file) corrupts B silently.

**Fix.** Restore endpoint runs `quick_check` on the chosen backup
**before** swapping. On failure, abort the restore, return a typed
error naming the corrupted backup; the folder remains in its current
state. Pin with `TestRestore_RechecksBackupBeforeSwap`.

**Audit handling.** Not specified. §2.7 L5 lists the four-step
restore but does not name a pre-swap re-check.

---

### 🔧 Z12 · Action-string map missing entry for new enum

**Composition.** Iter-4 O4 split a new enum value
`metadata_parse_failed` out of `schema_version_mismatch`. The map
update is a separate change. Without a coverage test, an enum added
later (the next iteration's R8 expansion) lands without a map entry.
Dashboard renders an empty action string. Operator confusion.

**Fix.** Add `TestDisabledReasonActions_AllEnumsHaveAction`:

```go
for _, r := range AllDisabledReasons() {
    if disabledReasonActions[r] == "" {
        t.Errorf("missing action string for reason %q", r)
    }
}
```

`AllDisabledReasons()` returns the enum in iteration order, e.g.
generated from a `//go:generate stringer` block, so adding a new
enum value forces both stringer regeneration and a map entry.

**Audit handling.** Decision §5 #18 specifies the map; coverage test
not named.

---

### 🔧 Z13 · F7 sweep "neither matches" branch unspecified

**Composition.** Disk-full window. `.bak` exists; `original` exists;
SQLite has hash H. Operator restores from a backup so old that
neither `.bak`'s hash nor `original`'s hash matches H. Sweep's two
documented branches both presuppose a match.

**Fix.** Add the third branch to F7: "If neither file matches the
SQLite row's hash, the folder enters `FolderDisabled` reason
`unknown` with diagnostic load (`error_text = "sweep: neither
disk file matches SQLite for path %q"`). Operator triages."

**Audit handling.** §2.1 F7 specifies two branches. Decision is
implicit (silent ignore? loud disable?). Make it explicit.

---

### 🔧 Z14 · Backup retention prune with N+1 files after crash

**Composition.** Crash between backup-write and retention-prune
leaves 11 backups (5+4+1 = 10 expected). Next backup writes the
12th. Retention runs once on 12 files. The GFS algorithm must be
**deterministic and idempotent**: same set of files in always
prunes to the same 10 files out, regardless of how the 11th file
got there.

**Fix.** Pin with `TestRetention_IdempotentOnExtraFile`: write
N+1 files, run prune, assert the resulting 10 are the canonical
GFS selection. Run prune a second time; assert the file set is
unchanged.

**Audit handling.** Decision §5 #11 specifies the policy; the
"N+1 from crash" composition is not pinned by a test.

---

## D. Nits and clarity

### 💬 Z15 · Action-string drift between map, dashboard, API, and runbook §4

**Composition.** Decision §5 #18 says the map "is the
single-source-of-truth ... the runbook §4 sections cite the same
string verbatim." The runbook is a markdown file edited by hand;
the map is a Go literal. Drift across two PRs is the predictable
failure mode (one PR updates the runbook, another updates the
map; both pass code review; the dashboard text does not match the
runbook text).

**Recommendation.** Pick one of:

- (a) Generate runbook §4 from the map: a `go generate` step writes
  a markdown table of `(enum, action)` pairs into a fenced block in
  `OPERATOR-RUNBOOK.md`. Drift becomes a CI failure.
- (b) Doc-test that parses runbook §4 for "Action: <string>" lines
  and asserts every map entry's value appears verbatim. Lighter than
  codegen but catches drift on the test run.
- (c) Document-only enforcement (current state). Highest drift risk.

Default: (b). Codegen is overkill for ~9 enum values; a parsing test
costs ~50 LOC.

**Audit handling.** Decision §5 #18 names the principle; enforcement
mechanism is not chosen.

---

### 💬 Z16 · Two folders `disk_full` simultaneously + per-folder reload deferred to §7

**Composition.** A shared disk fills. Folders F1 and F2 both fire
`disk_full`. Operator frees space. To recover, operator must
restart the node (per-folder reload is §7, deferred to follow-up).
Restart kills SSH, proxy, clipsync, gateway sessions for both
folders' worth of recovery. The `R8` cross-component-isolation
promise ("SSH tunnels stay up") holds during the failure but not
during recovery — exactly the gap iter-4 ops O12 raised.

**Recommendation.** No code change at v1; this is documented as a
known-limit in runbook §9 (currently a follow-up). **Promote §9
to v1-ship-blocker** if multi-folder concurrent recovery is
plausible during the first hour. On a three-peer deploy with 15
folders, two folders sharing a disk and both filling is realistic.

Or: ship §7 (per-folder reload endpoint) at v1 instead of as a
follow-up. The audit's structural cost is one admin endpoint and
its tests; the operator-side cost of a session-killing restart in
the first hour is large.

**Audit handling.** §6 commit 13 marks §§7–10 as follow-ups. The
composition (multi-folder disk_full + first-hour) raises the
priority on §7.

---

## E. What I need from you before code

| #   | Question | Default (mine) |
|-----|----------|----------------|
| Z1  | F7 sweep on unreadable SQLite: defer to operator (do nothing) or aggressively restore `.bak`? | Defer; let restore-from-backup re-run sweep. |
| Z2  | Promote epoch-mismatch to a named protocol invariant in DESIGN-v1 §0? | Yes — same shape as `FILESYNC_INDEX_MODEL`. |
| Z3  | Runbook §2.4 wording: name epoch-driven re-baseline as the rollback's peer-state recovery mechanism? | Yes. |
| Z4  | §1.1 wording: explicit "wipe once, before first dry-run, never again"? | Yes. |
| Z6  | Disabled-transition mid-life: cancel in-flight writer tx (A) or let it commit (B)? | (A). Document in §2.2 R8. |
| Z7  | Programmatic guard against simultaneous-flip, or runbook hardening only? | Runbook hardening at v1; programmatic guard deferred to §7. |
| Z8  | Synchronous `integrity_check` after detected SIGKILL recovery? | Yes. |
| Z10 | Process-level metric for `filesync_index_model_mismatch`, with §8 promoted to v1? | Yes to metric; yes to §8 at v1. |
| Z15 | Action-string drift enforcement: codegen (a), doc-test (b), or document-only (c)? | (b) doc-test. |
| Z16 | Per-folder reload endpoint (§7): ship at v1, or accept session-killing restart? | Ship at v1. The first-hour cost of a restart is too high. |

The remaining items (Z5, Z9, Z11–Z14) are mechanical fixes and tests
that fold into the existing commit sequence.

---

## F. Meta-observation

The audit's iter-3 review noted that gaps cluster on transitions; the
ops-lens iter-4 review noted that gaps cluster on the operator's next
sentence. This composition lens finds gaps cluster on **the seam
between two protections**: F7 sweep + R2 quick_check (Z1, Z13);
restore epoch + peer protocol contract (Z2, Z3); R8 disabled + R9
dirty-set cap (Z9); R8 disabled + INV-3/INV-4 in-flight tx (Z6, Z8).

In each case, both protections are individually correct. The seam
between them is what the audit does not yet name. The fixes are not
new mechanisms — they are explicit sequencing rules at each seam.

The dry-run → real-data flip is the highest-risk hour because it is
the first hour all of these mechanisms run together against real
data. Closing the seams before the flip is the cheapest insurance
available; closing them after, the most expensive recovery.
