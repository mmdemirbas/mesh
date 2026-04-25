# Filesync Operator Runbook

> Operator-facing recovery and lifecycle procedures for the `mesh`
> filesync subsystem. Sections §1–§7 are **blocking for v1 ship** —
> no peer flips from dry-run to real data until they are complete.
> Sections §8–§10 ship in a follow-up after first real-data deploy.
>
> Source design: `DESIGN-v1.md`. Audit: `PERSISTENCE-AUDIT.md`.
> Triggered by iter-4 ops review findings O13 + O16.
>
> Status: skeleton, populated commit-by-commit as the cutover lands.

---

## Section status

| §   | Title                                  | v1-ship | Status      |
|-----|----------------------------------------|---------|-------------|
| §1  | Before First Run                       | Block   | Skeleton    |
| §2  | First Real-Data Run (Dry-Run → Real)   | Block   | Skeleton    |
| §3  | Dashboard Triage                       | Block   | Skeleton    |
| §4  | Recovery by Disabled Reason            | Block   | Skeleton    |
| §5  | Backup Restore                         | Block   | Skeleton    |
| §6  | Peer-State vs File-State Divergence    | Block   | Skeleton    |
| §7  | Per-Folder Reload (`/reopen` endpoint) | Block   | Skeleton    |
| §8  | Build-Time Decisions for Operators     | Follow  | Pending     |
| §9  | Known Limits                           | Follow  | Pending     |
| §10 | Escalation                             | Follow  | Pending     |

---

## §1 Before First Run

### §1.1 Wipe `~/.mesh/filesync/` on every peer — once, at first start

The v1 protocol is a cold swap. Any leftover state from earlier
dev builds must be removed **before the very first `mesh up`** on
each peer:

```sh
# On every peer, ONCE, before the very first mesh up:
rm -rf ~/.mesh/filesync/
```

**Do NOT rewipe between dry-run and the real-data flip
(iter-4 Z4).** The wipe deletes
`~/.mesh/filesync/device-id` along with all folder DBs. The
device-id file is created on first run as the stable identity
of this peer; rewiping it between dry-run and real loses
VectorClock continuity, breaks the §4.3 device-id-recovery
option, and triggers a `.sync-conflict-*` storm on the first
real exchange (every file's vector clock points at a
device-id that no longer exists on any peer).

After the first `mesh up`, **back the device-id up
out-of-band** (Time Machine, Syncthing copy, ops vault):

```sh
cat ~/.mesh/filesync/device-id   # 10-char ID like XXXXX-XXXXX
# Save somewhere recoverable — §4.3 needs it on a future device
# rotation.
```

The binary refuses to open any folder whose cache directory
contains legacy gob / YAML sidecar files; recovery is to delete
them. (For a dry-run-to-real flip on a clean v1 install, this
should not fire — but it is the failsafe against an accidental
copy from a dev tree.)

### §1.2 Verify config: dry-run vs real

Each folder block in `mesh.yaml` carries a `direction` field. For
the first run, every folder should be `direction: dry-run` so the
scanner walks but does not transmit, sync, or write. The
dry-run-to-real flip is documented in §2.

### §1.3 Backup directory location and disk budget

Backups land at `~/.mesh/filesync/<folder-id>/backups/index-<seq>-<unixns>.sqlite`,
mode `0600`. GFS retention keeps 5 daily + 4 weekly + 1 monthly
per folder. Estimate worst case: `10 × <DB size>` per folder.
Provision accordingly.

> Populated in audit commit 9.

---

## §2 First Real-Data Run / Dry-Run → Real Flip

> The highest-risk window. One peer at a time, with a
> verification gate before flipping the next peer. Iter-4 O13 +
> O16 named this the missing procedure.

### §2.1 Pre-flight checklist

- [ ] Every peer reports a healthy dry-run scan in the dashboard.
- [ ] No `FolderDisabled` rows on any peer.
- [ ] The `device-id` file at `~/.mesh/filesync/device-id` is
      backed up on every peer (Time Machine, Syncthing, manual
      copy — somewhere recoverable).
- [ ] Backup directory has the disk budget from §1.3 reserved.
- [ ] All peers carry the same binary build. The handshake
      will reject mismatched models with a typed error; if
      you see `index_model_mismatch` in the dashboard or in
      `last_error` on a peer row after the flip, you have
      build drift across peers. Recovery: rebuild from a
      single source and redeploy.
- [ ] **Wall-clock skew between all three peers is < 60 s.**
      Run `date -u` on each peer; record the offset. If any
      peer is off by > 60 s, fix NTP first. Iter-4 Z7: the
      first-sync C1 fallback (mtime + deterministic
      device-id) is the conflict-resolution path on the
      first real exchange when no `BaseHashes` exist. Clock
      skew on this fallback picks the wrong winner per file
      and the operator has to triage every conflict by hand.

### §2.2 Per-peer flip sequence

> **One peer at a time.** Iter-4 Z7: flipping more than one peer
> in parallel during dry-run → real is the highest-risk
> operator error in v1. With no `BaseHashes` on any peer's
> first real exchange, the C2 classifier falls through to the
> C1 mtime fallback; clock skew between peers picks the wrong
> winner per file. Conflict-storm recovery on a 168k-file
> folder takes hours of manual triage. The procedure below is
> serial, not parallel; the verification gate (§2.3) is
> mandatory.

1. Pick the peer whose data is the **authoritative source** (the
   one whose folder you most want preserved if anything goes wrong).
   Flip it first.
2. On that peer:
   - Stop `mesh`.
   - Edit `mesh.yaml`: change the folder's `direction:` from
     `dry-run` to the target (`send-receive`, `send-only`, or
     `receive-only`).
   - `mesh up`.
3. Wait for the verification gate in §2.3.
4. Only after the gate passes, flip the next peer using the
   same procedure.

### §2.3 Verification gate (must pass before next peer flips)

After the first peer comes up under real direction, verify all
of the following before flipping the second peer:

- [ ] First scan completed (dashboard "scanning" state cleared,
      file count matches expectations within ~1%).
- [ ] No `FolderDisabled` rows.
- [ ] `mesh_filesync_folder_disabled` metric is 0.
- [ ] First backup file exists at the location from §1.3.
- [ ] `quick_check_ok=true` for the latest backup
      (`GET /api/filesync/folders/<id>/backups`).
- [ ] If multiple peers existed before the flip and were
      already in `direction: dry-run` paired with this peer:
      no `.sync-conflict-*` files in the folder root.
- [ ] **The other peers' `PeerState.LastEpoch` for this peer
      reflects the post-flip epoch.** Iter-4 Z3: this proves
      the epoch-mismatch protocol contract (§5.4) is firing
      end-to-end. If `LastEpoch` on B and C is empty or
      stale, the rollback in §2.4 will not trigger
      re-baseline cleanly. Read it via
      `curl http://<peer>:7777/api/filesync/folders/<id>` on
      each non-flipped peer; check the `peers[<flipped-peer>].last_epoch`
      field.

### §2.4 Rollback procedure if a verification gate fails

If any gate item fails on the first flipped peer, **before
flipping any further peer**:

1. Stop `mesh` on the flipped peer.
2. Revert the config to `direction: dry-run`.
3. Remove the SQLite DB:
   ```sh
   rm -rf ~/.mesh/filesync/<folder-id>/
   ```
   **Folder-scoped path** — the trailing `<folder-id>/` segment
   is load-bearing. This wipes the per-folder cache directory
   only; it does NOT touch `~/.mesh/filesync/device-id` at the
   parent level, so the peer keeps its stable identity.
   Compare with §1.1, which intentionally wipes the parent
   directory at first start (and only at first start — see
   the Z4 note there).
4. `mesh up`.
5. The dry-run scan rebuilds from the on-disk file content.
   **The other peers re-baseline automatically.** Iter-4 Z3:
   peers B and C accepted index exchanges from the flipped
   peer during the ~minutes window before rollback, and
   their `PeerState` rows now carry the flipped peer's epoch
   E1. Wiping `<folder-id>/` regenerates the folder epoch
   (E1 → E2). On the next index exchange, the
   epoch-mismatch protocol contract (§5.4) fires on B and C:
   they drop their cached `BaseHashes` and `LastSeenSequence`
   for this peer and re-sync against the rolled-back state.
   Do NOT manually wipe peer state on B / C — the protocol
   handles it.
6. Diagnose the gate failure using §3 and §4 before attempting
   the flip again.

---

## §3 Dashboard Triage

### §3.1 Healthy folder signals

> Populated in audit commit 3.

### §3.2 Disabled folder — what to read first

When a folder shows `status: disabled` in the dashboard or the
API:

1. Read the `action` string (it ships in the same JSON response
   and renders in the dashboard cell). The action string is the
   authoritative next step. It points at the relevant §4
   subsection.
2. Read the `reason` enum value. Each enum has a §4 subsection
   below.
3. If `reason: "unknown"`, read `error_text`, `stack_trace`, and
   `recent_log` from the same JSON response. The diagnostic
   payload loads inline (iter-4 O8) — no separate log query
   needed.

### §3.3 Where to find the full reason text

For all enum values: `/api/filesync/folders/<id>` returns
`reason`, `action`, and (for `unknown`) `error_text`,
`stack_trace`, `recent_log`. The Prometheus metric carries only
the enum to bound cardinality.

> Populated in audit commit 3.

### §3.4 Process-level peer-session warnings (iter-4 Z10)

Beyond per-folder `FolderDisabled` rows, watch the
process-level metric:

```
mesh_filesync_peer_session_dropped{reason="filesync_index_model_mismatch"}
```

If this metric is > 0, you have **build drift across peers**:
some peer's binary was built with a different
`FILESYNC_INDEX_MODEL` constant than the others. The handshake
correctly drops the session (no data corruption), but filesync
between the drifted peer and the rest is silently down — that
peer's local changes do not propagate. The dashboard surfaces
a warning row when this metric is non-zero.

**Recovery.** Rebuild every peer from the same source revision
and redeploy.

> The deployed `FILESYNC_INDEX_MODEL` for each peer is
> documented in §8 (first follow-up). For now: `git log -p
> docs/filesync/DESIGN-v1.md` to read the recorded build-time
> decision; verify all three peers were built from the same
> revision.

---

## §4 Recovery by Disabled Reason

Each subsection cites the same `action` string the dashboard
shows, then expands the procedure.

### §4.1 `quick_check_failed`

**Action:** restore from the most recent quick_check_ok backup; see runbook §4.1 + §5

`PRAGMA quick_check` reports gross page-level corruption at folder
open. The folder enters disabled before any sync touches the
corrupt rows. Recovery requires backup restore (see §5). The DB
file on disk is left intact for forensics — do not delete it
until the restore lands cleanly.

### §4.2 `integrity_check_failed`

**Action:** restore from the most recent quick_check_ok backup; writes after the failure are lost; see runbook §4.2 + §5

`PRAGMA integrity_check` runs asynchronously after folder open
and catches subtle corruption that `quick_check` misses (~10 MB/s,
tens of seconds on a large DB). The folder was live during the
window between open and the integrity_check completion; writes
that committed in that window are in the DB but will be lost on
restore — this is expected. The disabled-state JSON carries
`tx_in_flight_rolled_back: true` if a writer transaction was
running at integrity_check failure time (iter-4 Z6).

### §4.3 `device_id_mismatch`

**Action:** restore ~/.mesh/filesync/device-id from backup, restart node; see runbook §4.3

Decision tree. Iter-4 O3 named option 1 as the default.

**Option 1 (default) — restore the device-id file.** The
node-level `~/.mesh/filesync/device-id` was rotated or replaced
since the folder DB was created. If the original 10-character
ID is recoverable from any backup source (Time Machine,
Syncthing copy, ops notes, last known commit message), restore
it:

```sh
echo "XXXXX-XXXXX" > ~/.mesh/filesync/device-id   # original ID
chmod 600 ~/.mesh/filesync/device-id
mesh down && mesh up
```

This keeps VectorClock continuity, peer state, and avoids a
conflict storm.

**Option 2 (fallback) — wipe the folder.** Only when the
original device-id is genuinely unrecoverable. Wipes peer
state, regenerates the folder epoch, triggers full re-sync
against every peer:

```sh
mesh down
rm -rf ~/.mesh/filesync/<folder-id>/
mesh up
```

After option 2, every peer of this folder will re-baseline on
its next index exchange. If the operator has not touched peer
state on those other peers, the re-sync is automatic.

### §4.4 `schema_version_mismatch` and `metadata_parse_failed`

These are two different enum values (iter-4 O4 split them):

- `schema_version_mismatch` —
  **Action:** binary version mismatch — run the binary that wrote this schema, or restore from backup; see runbook §4.4

  The binary expects a different `folder_meta.schema_version`
  than the one stored. This means a future binary opened a v1
  DB or vice versa. Restore from backup or upgrade/downgrade
  the binary.

- `metadata_parse_failed` —
  **Action:** folder_meta is corrupt — restore from the most recent quick_check_ok backup; see runbook §4.4 + §5

  `folder_meta.sequence` (or another scalar) was unparseable.
  The DB is corrupt at the metadata level. Restore from backup
  (§5).

### §4.5 `read_only_fs`

**Action:** remount the filesystem read-write, then POST /api/filesync/folders/<id>/reopen; see runbook §4.5

The folder DB cannot accept writes because the filesystem is
mounted read-only. Remount read-write at the OS level, then
either POST `/api/filesync/folders/<id>/reopen` (preferred —
keeps SSH / proxy / clipsync sessions alive) or restart the
node.

### §4.6 `disk_full`

**Action:** free disk space, then POST /api/filesync/folders/<id>/reopen; see runbook §4.6

A SQLite write returned `SQLITE_FULL`. Free disk space at the
OS level, then either POST `/api/filesync/folders/<id>/reopen`
(preferred) or restart the node.

### §4.7 `dirty_set_overflow` (always look upstream first)

**Action:** triage upstream I/O failure (it is the cause), then POST /api/filesync/folders/<id>/reopen; see runbook §4.7

This enum almost never fires in isolation. It is the symptom of
a sustained `disk_full` or `read_only_fs` that exceeded the
10 000-path dirty-set cap. **Triage the upstream cause first**:
check the metric history, the log, and any earlier
`FolderDisabled` events. Once the upstream cause is fixed,
POST `/api/filesync/folders/<id>/reopen` (or restart the node).

### §4.7b `legacy_index_refused`

**Action:** delete legacy gob/yaml sidecar files in the folder cache directory, restart node; see runbook §1.1

The folder cache directory carries left-over `index.gob`,
`index.yaml`, `peers.yaml` (or their `.prev` / `.tmp` siblings)
from a pre-v1 dev build. The v1 binary refuses to open such a
directory because there is no migration path — DESIGN-v1 §0
ships as a cold swap. Delete the sidecars by hand:

```sh
mesh down
rm -f ~/.mesh/filesync/<folder-id>/index.gob*
rm -f ~/.mesh/filesync/<folder-id>/index.yaml*
rm -f ~/.mesh/filesync/<folder-id>/peers.yaml*
mesh up
```

The folder will rebuild its index from the on-disk file content
on the next scan.

### §4.8 `unknown` (escalation path)

The disabled-state JSON response carries `error_text`,
`stack_trace`, and `recent_log` inline (iter-4 O8). Capture
those, attach the latest `/api/state` JSON and `/api/metrics`
text, and open an issue. Do not wipe the folder until dev has
reviewed the diagnostic payload — it may contain evidence of a
class of failure the runbook does not yet cover.

---

## §5 Backup Restore

> Populated in audit commit 9.

### §5.1 Backup file location and naming

`~/.mesh/filesync/<folder-id>/backups/index-<seq>-<unixns>.sqlite`,
mode `0600`. `<seq>` is the highest sequence committed at backup
time; `<unixns>` is wall clock at backup. GFS retention: 5 daily
+ 4 weekly + 1 monthly.

### §5.2 How to pick a backup

```sh
curl -s http://127.0.0.1:7777/api/filesync/folders/<id>/backups | jq
```

Returns `[{path, sequence, created_at, quick_check_ok}, ...]`
sorted by `sequence` descending. **Picking criteria**:

1. Highest `sequence` with `quick_check_ok=true` is the
   default — this is "newest known-good."
2. If the corruption began before the most recent passing
   backup, walk down to the next-older `quick_check_ok=true`.
3. Never pick a backup with `quick_check_ok=false`.

### §5.3 Restore procedure (atomic, via admin endpoint)

```sh
curl -X POST http://127.0.0.1:7777/api/filesync/folders/<id>/restore \
  -d '{"backup_path": "<path-from-§5.2>"}'
```

The endpoint runs the **five-step procedure** atomically:

0. **Re-check `quick_check` on the chosen backup** before
   touching anything (iter-4 Z11). On failure, abort with a
   typed error; the folder remains in its current state. The
   `quick_check_ok` flag in the listing endpoint can go stale
   between list time and swap time (a passing storage glitch,
   an unrelated process touching the file); this re-check is
   the load-bearing protection.
1. Stop the folder (per-folder reload, not full node restart).
2. Swap `index.sqlite` (and `-wal`/`-shm` sidecars) with the
   chosen backup.
3. **Bump `folder_meta.epoch`** to a new value.
4. Restart the folder via the `/reopen` path. The F7 download
   sweep (audit §2.1 F7) re-runs against the restored DB
   (iter-4 Z1): the restored DB now has the rewound hash for
   any path that has a leftover `.mesh-bak-<hash>` from a
   pre-corruption disk-full event, so the sweep's restore
   branch fires and converges state.

### §5.4 Why the epoch bump is load-bearing (iter-4 O10 + Z2)

Without step 3, peers' `LastSentSequence` already exceeds the
rewound `folder_meta.sequence`. Their delta queries
(`WHERE sequence > LastSeenSequence`) skip exactly the rows
the restore brought back, and you silently lose data on the
next peer-driven sync.

The epoch bump triggers the **epoch-mismatch protocol
contract** (DESIGN-v1 §0, audit §5 #24): the receiver MUST
compare incoming `Epoch` against cached `PeerState.LastEpoch`
and, on mismatch, drop `BaseHashes` and `LastSeenSequence`
for that (folder, peer) pair, forcing a full re-sync. The
test `TestPeer_OfflineDuringRestore_ResetsOnEpochAlone`
pins this contract for the offline-peer-during-restore case
where sequence may not have visibly dropped from the peer's
cached view.

The contract is what makes the restore safe across peers
that exchanged before the corruption: even if such a peer
happens not to see a sequence drop on first post-restore
exchange, the epoch change forces re-baseline.

### §5.5 Expected conflict-file fan-out and how to triage it

After a restore, peers' `BaseHashes` for paths whose hash on
disk now differs from the restored DB will trip the C2
classifier into "conflict" on every such path. This is
expected and recoverable:

1. Wait for the first post-restore sync cycle to complete on
   every peer.
2. List `.sync-conflict-*` siblings in the folder root.
3. For each: pick the winning copy (usually the restored one
   for paths whose history matters; the conflict copy for
   paths the operator was actively editing on another peer at
   restore time).
4. `mv` the winner into place, delete the loser. The next scan
   propagates the resolution to all peers.

---

## §6 Peer-State vs File-State Divergence

> Populated in audit commit 9 + composition-lens iter-4
> follow-up. The minimum content:

### §6.1 What peers cached during the disabled window

Peer state on disk reflects the state we acknowledged before
the disable event. After we recover (restore or wipe), peers
think we have rows we no longer carry. The epoch bump in §5.4
forces peers to drop their cached `BaseHashes` and re-baseline
against our restored content.

### §6.2 Manual reconciliation steps

For most enums (`disk_full`, `read_only_fs`,
`dirty_set_overflow`), no manual reconciliation is needed —
peer state is correct, the local DB just needs the failing I/O
fixed.

For backup-restore recoveries, the epoch bump handles
reconciliation automatically. Operator triages
`.sync-conflict-*` per §5.5.

For `device_id_mismatch` option 2 (wipe), every peer of this
folder re-baselines on its next index exchange. No manual
peer-state intervention.

### §6.3 When to wipe peer state on the other peers too

Almost never. The epoch bump should handle re-baselining
without touching the other peers. Wipe peer state on another
peer **only if** that peer's index exchange against us
repeatedly errors with epoch-related messages after the
restore — and only after dev review.

---

## §7 Per-Folder Reload (`/reopen` endpoint)

> v1-ship blocker. The endpoint ships at audit commit 9.
> Promoted from follow-up because §5.3 cites this endpoint —
> the alternative (manual `sqlite3` CLI editing during
> recovery) is the wrong UX for the ship-perfect bar.

### §7.1 What the endpoint does

```sh
curl -X POST http://127.0.0.1:7777/api/filesync/folders/<id>/reopen
```

Re-runs the open path on a single folder:

1. Closes the existing SQLite handles for the folder.
2. Re-opens via `loadIndexDB`.
3. Runs `PRAGMA quick_check` synchronously.
4. Schedules `PRAGMA integrity_check` on a goroutine.
5. Resumes the scan loop.

Other folders, SSH, proxy, clipsync, gateway are untouched.
Active SSH sessions and inbound proxy connections survive.

### §7.2 When to use `/reopen` vs full node restart

`/reopen` is sufficient when the underlying issue is local to
the folder DB or its filesystem and was fixed in place:

| Disabled reason          | Recovery path                                    |
|--------------------------|--------------------------------------------------|
| `disk_full`              | Free space → `/reopen`.                          |
| `read_only_fs`           | Remount read-write → `/reopen`.                  |
| `dirty_set_overflow`     | Triage upstream cause → `/reopen` once fixed.    |
| `quick_check_failed`     | Restore from backup (§5) → reopen runs in restore.|
| `integrity_check_failed` | Restore from backup (§5) → reopen runs in restore.|
| `device_id_mismatch`     | Restore device-id file (§4.3) → full node restart (the device-id is read at process start, not folder open). |
| `schema_version_mismatch`| Match binary version → full node restart.        |
| `metadata_parse_failed`  | Restore from backup (§5) → reopen runs in restore.|
| `unknown`                | Capture diagnostic payload, escalate per §10.    |

### §7.3 What `/reopen` does NOT do

- It does not migrate the schema.
- It does not bump the epoch (the restore endpoint does, when
  appropriate).
- It does not re-read `~/.mesh/filesync/device-id` — that file
  is loaded once at process start, so device-id changes still
  require a full node restart.
- It does not reset peer state. Peer rows survive the reopen
  intact.

### §7.4 Verification after `/reopen`

- Dashboard shows the folder as `enabled`.
- `mesh_filesync_folder_disabled{folder="<id>"}` returns to 0.
- Other folders' rows are unchanged.
- SSH sessions you had open are still alive (test with `pwd`).

---

## §8 Build-Time Decisions Recorded for Operators

> Follow-up. Lands after first real-data deploy.

How to read which `FILESYNC_INDEX_MODEL` (β or hybrid) the
binary was built with, where the bench result is recorded, and
what changes between the two model shapes from an operator
perspective.

---

## §9 Known Limits

> Follow-up. Lands after first real-data deploy.

- Bench variance across peers (resolved by build-time selection
  per §8 — section pending).
- Multi-failure compositions (cross-reference iter-4 §1 once
  composition-lens review closes).

---

## §10 Escalation

> Follow-up. Lands after first real-data deploy.

When to involve dev (catch-all `unknown`, repeated
`quick_check` failures, integrity-check fails on a backup that
previously passed). What to attach (`/api/state` JSON,
`/api/metrics` text, last 1000 log lines, backup listing,
reproduction steps).
