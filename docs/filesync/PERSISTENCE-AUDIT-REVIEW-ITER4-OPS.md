# Persistence-Audit — Iteration-4 Operator-Workflow Review

> Independent review of `PERSISTENCE-AUDIT.md` (iteration 3.1) through the
> **operator workflow lens**: who reads the dashboard, what they type, what
> happens next. Out of scope: assertions, transitions, failure-mode
> composition (covered by iter-3 and the parallel iter-4 composition pass).
>
> Status: draft, 2026-04-25.

---

## Legend

| Icon | Meaning                                                 |
|------|---------------------------------------------------------|
| 🚨   | Folder unrecoverable without dev intervention.          |
| ⚠️   | Recovery exists but is undocumented.                    |
| 🔧   | Documented but unclear.                                 |
| 💬   | Clear but could be smoother.                            |

---

## Executive summary

| #    | Area                                              | Severity |
|------|---------------------------------------------------|----------|
| O1   | `quick_check_failed` — recovery path silent       | ⚠️       |
| O2   | `integrity_check_failed` — async window leaves peer state advanced | ⚠️ |
| O3   | `device_id_mismatch` — two recovery branches, audit picks neither | 🚨 |
| O4   | `schema_version_mismatch` — enum reused for sequence parse failure | 🔧 |
| O5   | `read_only_fs` — restart is documented; live retry is not        | 💬 |
| O6   | `disk_full` — same as O5; auto-retry-on-free-space not discussed | 💬 |
| O7   | `dirty_set_overflow` — symptom enum, not a root cause            | ⚠️ |
| O8   | `unknown` — catch-all has no documented diagnostic path           | 🚨 |
| O9   | Backup picker — operator cannot tell which of 10 backups to use   | ⚠️ |
| O10  | Restore reconciliation — peers cache state the restored DB lacks  | 🚨 |
| O11  | Dashboard surfaces enum, never the next action                    | ⚠️ |
| O12  | Every recovery requires full node restart — kills SSH/proxy too   | ⚠️ |
| O13  | First-run UX: undefined for the non-author operator               | 🚨 |
| O14  | β/hybrid bench decision lives only in a commit message            | ⚠️ |
| O15  | Three peers may bench differently — protocol selection undefined  | 🚨 |
| O16  | Dry-run → real flip procedure not written down                    | 🚨 |
| O17  | Dry-run flip rollback procedure not written down                  | ⚠️ |
| O18  | `VACUUM INTO` target path and filename convention unspecified     | ⚠️ |
| O19  | Folder-DB wipe → peer epoch reconciliation behavior unspecified    | 🚨 |

**Counts:** 🚨 6 · ⚠️ 9 · 🔧 1 · 💬 2

---

## A. Per-enum recovery review

### ⚠️ O1 · `quick_check_failed`

**What the operator sees.** Red dashboard row. `/api/filesync/folders` returns
`status: "disabled", reason: "quick_check_failed"`. Metric flips to 1.

**What is missing.** The audit defines the signal. It does not define the
response. The next sentence the operator needs is "restore from `<backup
file path>`, restart the node." That sentence does not exist anywhere in the
audit, the design doc, or `CLAUDE.md`.

**Live recovery?** No. `quick_check` runs at folder open, so the folder must
re-open. That requires a node restart (see O12).

**Peer reconciliation.** Folder never went live this run, so peer state on
disk is whatever the last successful run committed. Peers re-sync via index
exchange. This case is benign for peer convergence.

---

### ⚠️ O2 · `integrity_check_failed`

**What the operator sees.** Same surface as O1, but the folder was live for
the `integrity_check` window (tens of seconds). Writes during that window
were committed.

**What is missing.** Same recovery gap as O1. Plus: the audit does not say
whether the writes from the live window are preserved (they are — the DB is
still on disk, only flagged) or lost (operator may assume the latter and
wipe). Operator needs an explicit "your last few commits are still in the
DB; restore brings you back to an older state and you will lose them"
statement.

**Live recovery?** No.

**Peer reconciliation.** Peers advanced their `LastSentSequence` and
`BaseHashes` against the corrupted-but-readable DB during the live window.
Restoring from backup rewinds our `folder_meta.sequence` below what peers
think we acknowledged. Audit does not say whether the index exchange handles
"sequence went backward" gracefully or treats it as epoch divergence.

---

### 🚨 O3 · `device_id_mismatch`

**What the operator sees.** Enum reason. Full text in logs (R8) names
`file=X, db=Y`.

**What is missing.** Two recovery branches with sharply different cost:

1. Restore the original `~/.mesh/filesync/device-id` content (if known) →
   keep all VectorClock history, keep peer state, no conflict storm.
2. Wipe `~/.mesh/filesync/<folder-id>/` → discard peer state, regenerate
   epoch (I2), trigger full re-sync against every peer.

The audit picks neither. The operator does not know which to attempt. In
practice, the original device-id is usually recoverable from a Time
Machine / Syncthing copy, but no one has told the operator to look there
first.

**Live recovery?** No.

**Peer reconciliation.** Branch 1: clean. Branch 2: epoch regenerates;
peers see a "new" node with the same node name; behavior is undefined in
the audit (see O19).

---

### 🔧 O4 · `schema_version_mismatch` overloaded with sequence parse error

D1 in §4.6 routes a `parseInt64` failure on `folder_meta.sequence` to
reason `schema_version_mismatch`. The schema version is a different field
in the same table. Operator reading the dashboard reasons "I did not
migrate the schema, why is this firing?" and looks in the wrong place.

**Fix.** Add an enum value (`folder_meta_corrupt` or
`metadata_parse_failed`) and route D1 there. The current overload trades a
five-character enum saving for a five-minute operator misdirection.

---

### 💬 O5 · `read_only_fs`

Open-time check; clean enum; restart-after-remount is the obvious
response. The audit's only gap here is the absence of a one-line "remount
read-write, restart node" pointer in operator docs. Live recovery (re-open
the folder when the FS is back) would be possible without a process
restart and would compose better with O12.

---

### 💬 O6 · `disk_full`

`SQLITE_FULL` rolls back the failing tx; folder enters disabled. Operator
frees space, restarts. Same shape as O5. Same opportunity for live
recovery (auto-retry once the writer's next tx succeeds) which the audit
does not consider.

---

### ⚠️ O7 · `dirty_set_overflow` is a symptom enum

10 000 dirty paths is the consequence of sustained commit failure, which
is itself the consequence of `read_only_fs` or `disk_full` lasting longer
than expected. The dashboard surfaces the symptom, not the cause. Operator
who sees `dirty_set_overflow` for the first time has to read the audit to
discover that the underlying issue is one of two other enum reasons that
should already have fired.

**Fix.** Document the composition: `dirty_set_overflow` always co-occurs
with a fixable I/O failure that surfaced via metrics or logs first;
operator triages the I/O cause, not the dirty-set count.

---

### 🚨 O8 · `unknown` is a dead end

The audit's response to "we don't know why this folder failed" is the enum
value `unknown` and "full text logged separately." There is no
`/api/filesync/folders/<id>/last_error` endpoint. There is no
`/api/logs?component=folder&id=<id>` query. The operator has 1000 ring
lines and a full log file with no filter, and no way to map a folder-id
to its disable-reason text.

**Fix.** The disabled-folder JSON should carry the full reason text as
well as the enum. One field; the cardinality argument applies only to the
metric label, not the JSON response.

---

## B. Backup and reconciliation

### ⚠️ O9 · Backup picker

GFS retention keeps 10 backups (5 daily, 4 weekly, 1 monthly). The audit
does not say:

- Where the backup files live on disk.
- What they are named (timestamp? sequence number? both?).
- How the operator picks one. "Newest" is wrong if the corruption
  predates the most recent backup.
- Whether each backup carries metadata (last-included sequence, commit
  count, integrity-check-passed marker).

The operator is being asked to pick from an unlabeled directory of 10
opaque files.

**Fix.** Backup filename includes the highest sequence committed and the
result of a `quick_check` at backup time; admin endpoint
`/api/filesync/folders/<id>/backups` lists them with metadata.

---

### 🚨 O10 · Restore reconciliation against advanced peer state

When the operator restores a backup, the local DB rewinds. The peer's
`PeerState.LastSentSequence` and `PeerState.BaseHashes` reflect the
state we acknowledged before the rewind. Three things follow that the
audit does not address:

1. The peer will not resend rows it believes we have, because its delta
   query is `sequence > LastSeenSequence`. We will be missing those rows
   silently.
2. Peer's `BaseHashes` for paths whose hash on disk now differs from
   the restored DB will trip the C2 classifier into "conflict" on every
   such path — `.sync-conflict-*` fan-out across the folder.
3. Local `folder_meta.epoch` is unchanged by restore (epoch is seeded
   once and never rewritten per I2), so peers do not detect the rewind
   as an epoch event and do not trigger a full re-sync.

The cleanest response is "after restore, also bump epoch so peers
re-baseline." The audit does not name this.

---

## C. Dashboard and lifecycle

### ⚠️ O11 · Dashboard names the symptom, never the action

`/api/filesync/folders` returns `status: "disabled", reason: <enum>`.
The dashboard renders a red row. The operator's next question is "what
do I do." The answer requires reading this audit. Production operators
will not have the audit open.

**Fix.** Each enum value maps to a one-line action string surfaced in
the API response and the dashboard cell. Example:
`reason: "disk_full", action: "free disk space, restart node"`.

---

### ⚠️ O12 · Restart kills unrelated subsystems

Every recovery path the audit sketches ends with "restart the node."
The mesh process also runs SSH, proxy, clipsync, and gateway. A
filesync recovery that requires a full process restart kills active
SSH sessions and inbound proxy connections.

The audit's R8 design explicitly preserves cross-component isolation
("SSH tunnels stay up"), but only during the failure. Recovery throws
that isolation away. The natural extension is per-folder reload via
admin endpoint (`POST /api/filesync/folders/<id>/reopen`). Audit does
not consider it.

---

## D. First-run and bench-decision UX

### 🚨 O13 · First-run experience is undefined

Operator installs v1, edits the config, points at a folder, runs
`mesh up`. What happens in the next five minutes is not written down:

- Does the dashboard show "scanning" with a path counter?
- How does the operator confirm the first scan finished?
- How does the operator confirm peers connected?
- If the folder shows as `enabled` immediately on a 168k-file folder,
  is that because the scan completed in 200 ms, or because no scan has
  started yet?
- For a non-author operator with no prior context: is there even a
  first-run section in any doc to point them at?

The audit treats first-run as the moment of cutover risk (per the iter-4
re-framing). The actual failure mode is non-author operator confusion,
not data loss.

---

### ⚠️ O14 · β-vs-hybrid decision lives in a commit message

§5 #9 and the commit-7 prose say "decision recorded in commit 2 message
and `RESEARCH.md`." Six months later, a different operator wonders why
their binary behaves one way and not the other. They will not run
`git log -p` on commit 2. They will read `DESIGN-v1.md` or this audit.

**Fix.** The decision needs a durable home in the design doc, not just
the commit message. "On hardware class X, the bench measured N ms,
selecting branch Y" — one paragraph in DESIGN-v1, updated when the
bench is rerun.

---

### 🚨 O15 · Three peers may bench differently

The bench is per-hardware. The MBP, the Windows desktop, and the VPS
bastion will produce three different numbers. β and hybrid have
different in-memory shapes; the on-disk format is the same but the
runtime behavior diverges. Audit does not say:

- Is the choice baked into the binary at build time, so each machine
  ships its own?
- Is it runtime, so each peer chooses independently?
- If runtime: do the peers need to agree, or can they diverge silently?

Without an answer, an operator who deploys the same binary across
three peers gets undefined behavior.

---

## E. Cutover procedure

### 🚨 O16 · Dry-run → real flip procedure is implicit

The audit assumes the operator knows how to flip dry-run to real. The
config field exists (`direction: dry-run`). The procedure is not
written:

1. Stop all peers? Or stop one at a time?
2. Edit config — which field, what value?
3. Wipe `~/.mesh/filesync/` — on every peer or just one?
4. Restart in what order?
5. Watch for what signal that the first scan succeeded?
6. When is it safe to flip the second peer?

Three peers, three operators, three different opinions about the order.
The audit positions this as the highest-risk window and writes no
procedure for it.

---

### ⚠️ O17 · Dry-run flip rollback

If the first real-data scan misbehaves on peer 1 before peer 2 flips,
how does the operator roll back? Stop, re-edit config to dry-run,
delete the SQLite DB? Or revert config and let dry-run re-scan over the
real-data state?

---

### ⚠️ O18 · `VACUUM INTO` filename convention

Backup is "on a dedicated goroutine, GFS retained." Where does it land?
Filename pattern? Permissions? §3.9 Y3 says `0600` on the backup file
but does not say where it lives. Operator looking for backups during
recovery has nowhere to start.

---

### 🚨 O19 · Wipe-and-restart epoch reconciliation

Several recovery paths reduce to "wipe `~/.mesh/filesync/<folder-id>/`."
Doing so regenerates the folder epoch (per I2). Peers cache the old
epoch in `PeerState`. The audit's index-exchange handler does not
document the epoch-mismatch path:

- Does the peer reject the exchange?
- Does it auto-trigger a full re-sync?
- Does the operator have to wipe peer state on every other peer too?

For three peers and one wipe, the worst case is a coordinated wipe
across all three. The best case is automatic re-baselining. The audit
picks neither.

---

## F. What this review does not cover

- Whether the recovery actually works (correctness, covered by iter-3).
- Whether two failures compose dangerously (covered by the parallel
  iter-4 composition pass).
- The mechanics of `VACUUM INTO` itself (covered by §2.7 L4 and §5 #11).

---

## G. Minimum operator runbook outline

The audit should ship with `OPERATOR-RUNBOOK.md` (or equivalent
section in `DESIGN-v1.md`) covering:

```
1. Before First Run
   1.1 Wipe ~/.mesh/filesync/ on every peer
   1.2 Verify config: dry-run vs real
   1.3 Backup directory location and disk budget

2. First Real-Data Run (Dry-Run → Real Flip)
   2.1 Pre-flight checklist
   2.2 Per-peer sequence (one peer at a time)
   2.3 Verification gate before next peer
   2.4 Rollback procedure if a verification gate fails

3. Dashboard Triage
   3.1 Healthy folder signals
   3.2 Disabled folder — what to read first
   3.3 Where to find the full reason text (not just the enum)

4. Recovery by Disabled Reason
   4.1 quick_check_failed
   4.2 integrity_check_failed
   4.3 device_id_mismatch (decision tree: restore device-id vs wipe folder)
   4.4 schema_version_mismatch (and the parse-error overload)
   4.5 read_only_fs
   4.6 disk_full
   4.7 dirty_set_overflow (always look upstream first)
   4.8 unknown (escalation path)

5. Backup Restore
   5.1 Backup file location and naming
   5.2 How to pick a backup (GFS tiers, sequence metadata, last-known-good)
   5.3 Stop / swap / restart procedure
   5.4 Post-restore epoch bump
   5.5 Expected conflict-file fan-out and how to triage it

6. Peer-State vs File-State Divergence
   6.1 What peers cached during the disabled window
   6.2 Manual reconciliation steps
   6.3 When to wipe peer state on the other peers too

7. Per-Folder Reload vs Full Process Restart
   7.1 Why most recoveries currently require a full restart
   7.2 What that costs (SSH, proxy, clipsync sessions)
   7.3 Per-folder reload endpoint (if/when it ships)

8. Build-Time Decisions Recorded for Operators
   8.1 β vs hybrid: how to read the binary, where the decision is logged
   8.2 Backup retention overrides (none in v1)

9. Known Limits
   9.1 Bench variance across peers
   9.2 Multi-failure compositions (cross-reference iter-4 §1)

10. Escalation
    10.1 When to involve dev (catch-all unknown, repeated quick_check failures)
    10.2 What to attach (state JSON, metrics, last 1000 log lines, backup listing)
```

The first three sections are blocking for v1 ship. Sections 4–6 can
ship in parallel with the first real-data deploy if §3 names the read
order. Sections 7–10 can follow the first month of operations.
