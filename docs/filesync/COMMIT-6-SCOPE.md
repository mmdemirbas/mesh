# D4 commit 6 — pre-implementation scope

Three questions resolved before code lands. Cross-references to
`PERSISTENCE-AUDIT.md` are by row name (F7, INV-4, D7, Z1/Z7/Z13)
and §6 prose, never by line number.

---

## 1. Sub-commit ordering (failure-isolation)

All ten concerns ship under one git commit per audit §6, but the
implementation order is sequenced so each phase leaves the tree
green and a regression at any phase points at exactly one
audit row. Order is bottom-up by failure isolation: the lowest
phases protect the assumptions the higher phases depend on.

| Phase | Concern | Audit refs | Why this position |
|-------|---------|-----------|-------------------|
| **A** | CRC32 trailer on packed VectorClock blob (disk-only encoding change) | D7 | Foundational. Every later phase writes peer_state / files rows whose Version column carries the packed form. Land first so no production write path ever emits the un-CRC blob and a roll-forward never has to migrate two formats. Tests: `TestEncodeVectorClock_TrailerRoundTrip`, `TestDecodeVectorClock_RejectsBadCRC`. |
| **B** | Structural-ordering tripwire wired (Run startup reads `scanClaimSkipWired`) | §6 commit 5/6 prose | Pure check — no behavior change for the green case. Test `TestStartup_RefusesWithoutClaimSkip` lands here so it gates everything that follows in this commit. Reverting commit 5 alone now fails loud at startup. |
| **C** | Sync-persist co-tx (BaseHashes + LastSentSeq + LastSeenSeq + last_sync_ns in one BEGIN IMMEDIATE) | INV-4 peer-update bullet, Gap 2, Gap 2' | Pure SQLite tx-shape change, no `.bak` machinery. Decouples the durability-ordering work from the on-disk lifecycle work so a failure at this phase points only at the tx wrapper. Tests: `TestPeerSyncCoTx_AtomicAcrossFields`, `TestCrashBeforeBaseHashCommit_ClassifiesAsConflict` (H12). |
| **D** | Classifier tightening (absent BaseHash → conflict; first-sync gated on `last_sync_ns == 0`) | INV-4 classifier semantics, Gap 2' | Depends on C — needs the durability guarantee in place before the classifier can trust an absent BaseHash to mean "lost data," not "split crash window." Test plan extension below in §3. |
| **E** | `.mesh-bak-<hash>` three-step download lifecycle | F7, Gap 4' | New write path. Builds on C's tx wrapper (the SQLite UPSERT runs inside the same `BEGIN IMMEDIATE`). Tests: `TestDownloadCommitFails_RestoresOriginal` (H13), unit tests for each step's failure mode. |
| **F** | claimPath window extension across SQLite commit | C6 link with §6 commit 5 | Coordination tightening. Extends the existing claim from `before rename` → `after SQLite commit`, so commit 5's scan skip now covers the renamed-but-not-yet-committed window. Test: `TestDownload_HoldsClaimUntilTxCommit`. |
| **G** | rename / delete paths mirror the .bak pattern | F7, INV-4 non-scan-path bullet | Same shape as E, applied to the two adjacent write paths. Tests: `TestRename_BackupAndCommit`, `TestDelete_BackupAndCommit`. |
| **H** | 100 ms batch-coalesce window | §6 commit 6 prose | Pure perf coalesce on top of E/F/G. Lands last in the write-path lane because it is the only phase whose absence would not be a correctness bug. Test: `TestDownloadBatch_CoalescesIntoOneTx`. |
| **I** | Startup sweep for `.mesh-bak-<hash>` (branches a/b) | F7 sweep | Builds on E's lifecycle. Sweep depends on the rows the new write path commits, so it must come after E. Tests: `TestSweep_BakMatchesNewHash_Unlinks`, `TestSweep_BakMatchesOriginalHash_Restores`. |
| **J** | Sweep robustness folds (Z1: DB unreadable, Z13: neither matches) | iter-4 Z1, Z13 | Depends on I. Z1 and Z13 are the two failure-mode branches that complete the sweep's truth table. Tests: `TestSweep_DBUnreadable_PreservesBak` (Z1), `TestSweep_NeitherMatches_DisablesWithUnknown` (Z13). |

**Roll-back debugging.** A green-bar failure surfaces in the
phase whose tests just turned red; the prior phases stay green
because each phase is internally idempotent and does not depend
on a higher-numbered phase. A bisect within commit 6 points at
a single phase by which test fails.

**Single git commit.** All ten phases land as one commit per
audit §6 ("Each commit closes a named set of audit rows"). The
phase tags above appear only in the commit body's `# Phase X`
section headers, not in the git history.

---

## 2. CRC32 trailer rollout — wire vs disk

**Disk only at commit 6. Wire format unchanged.**

### Where the packed form lives

`internal/filesync/index_sqlite.go::encodeVectorClock` /
`decodeVectorClock` — the BLOB column on `files.version` and
`peer_state.last_ancestor_hash` (well, the latter is a single
hash). Layout: `uint16 BE count, then per entry: 10-byte ASCII
device_id, uint64 BE counter`.

### Where the wire form lives

`internal/filesync/vclock.go::vectorClockFromProto` and
`internal/filesync/proto/`'s `Counter` messages, encoded by
protobuf. The wire form is a `repeated Counter version = 8`
(or whatever field number applies in `FileInfo`); the
serialization is protobuf-native and has its own framing,
length-prefix, and varint integrity.

### Why D7 only touches the disk form

D7's exact wording: "CRC32 trailer on the VectorClock blob —
moved up so blobs are never written without it." The "blob" is
the packed BYTES form stored in SQLite — protobuf does not
produce a single blob, it produces a length-delimited message.
Adding a CRC trailer to a protobuf field would break wire-format
forward/backward compat with any non-mesh tool inspecting the
proto and is unnecessary because protobuf's framing already
catches truncation.

### Mixed-version dev-loop composition

A commit-6 peer talking to a commit-5 peer:
- **Index exchange (wire):** commit-5 peer sends `Counter`
  messages, commit-6 peer receives via
  `vectorClockFromProto` — unchanged path. No CRC anywhere on
  the wire. Composes.
- **Local SQLite write at commit-6 peer:** stores the freshly
  decoded VectorClock via `encodeVectorClock`, which now
  appends the CRC. Composes.
- **Local SQLite read at commit-6 peer of a row written by
  commit-5 peer:** impossible. The on-disk DB is local; rows
  are written only by the local writer. Cross-peer rows do
  not exist on disk; only their decoded values do. So no
  reverse-compat lane is needed for the disk format either.

The on-disk format is per-node, never shipped to peers, and
the wire format is unaltered. Mixed-version composition is
trivial: any peer at any commit reads/writes its own SQLite
under its own CRC contract, and every peer agrees on the
unchanged wire format.

### Format-bump posture

`decodeVectorClock` will gain a length-classifier:
- old length (no CRC) → decode without verification, log a
  one-shot WARN naming the row's path so an operator sees a
  pre-CRC blob is being silently upgraded on next write
- new length (with CRC) → verify; mismatch → return nil and
  let the row treat as missing-version (which the classifier
  handles as "unknown ancestor → conflict")

This handles the cold-start case where a dev DB pre-CRC
exists from earlier commits in the cutover series. No
production state to migrate (filesync never went prod).

### Test coverage for the rollout

- `TestEncodeVectorClock_TrailerRoundTrip` — every emitted
  blob has the trailer; round-trip yields the same VectorClock.
- `TestDecodeVectorClock_AcceptsLegacyBlob` — pre-CRC blobs
  decode (one-shot WARN).
- `TestDecodeVectorClock_RejectsBadCRC` — corrupted trailer
  → nil decode.
- `TestProtoRoundTrip_NoCRCOnWire` — protobuf-encoded
  `Counter` messages contain no CRC bytes; the proto-emitted
  bytes for a known VectorClock value are byte-identical
  before and after the disk-side CRC change.

---

## 3. Classifier tightening — first-sync coverage

Audit text (INV-4 classifier semantics):

> Absence of a BaseHash entry for a (peer, path) pair means
> "unknown ancestor → conflict path," never "fall back to C1
> mtime comparison." The C1 heuristic is only used when we have
> positive knowledge of no prior sync with this peer (first-sync
> case).

The "positive knowledge" gate is `last_sync_ns == 0` in the
`peer_state` row, written atomically with BaseHashes (Phase C
co-tx).

### Three sub-questions and current coverage

#### 3.1 First-sync path explicitly handles "BaseHash absent + first exchange with this peer" without conflict-storm

**Yes, by construction.** Classifier pseudocode after Phase D:

    if peerState.LastSyncNS == 0 {
        // first sync: every BaseHash is legitimately absent.
        // Use C1 mtime fallback; no conflict.
    } else if baseHash, ok := peerState.BaseHashes[path]; ok {
        // three-way merge with known ancestor
    } else {
        // last_sync_ns > 0 but no BaseHash for this path:
        // either prior crash before BaseHash co-tx (now closed by Phase C)
        // or genuine data hole. Conflict.
    }

The fresh-state cold start has `last_sync_ns == 0` for every
peer, so every diff falls to C1 mtime — no conflict-storm.

#### 3.2 Z7 simultaneous-flip — two peers flip at the same time, no BaseHashes either side

Audit Z7 fold: "runbook hardening only — wall-clock skew check +
a stronger 'one peer at a time' warning in §2.1 pre-flight and
§2.2. No programmatic guard at v1 (three-peer scale doesn't
justify it)."

Composition: both peers have `last_sync_ns == 0` against each
other → C1 mtime fallback fires on both sides. With wall-clock
skew under a few seconds the higher mtime wins; with skew over
the conflict-window threshold (existing `mtimeConflictWindow`,
2 s) the file becomes a `.sync-conflict-*` rather than a silent
overwrite — already the existing behavior, no new code needed.
The runbook-only posture is correct: the v1 three-peer scale
makes the simultaneous-flip case a documentation problem, not
an algorithm problem.

**Defined behavior, not emergent.** The defined behavior is
"C1 mtime + 2 s conflict window," which is already pinned by
existing `TestC1MtimeFallback_*` and `TestConflictWindow_*`
tests. No new code or test for Z7 specifically.

#### 3.3 Three-peer fresh-state index exchange — no BaseHashes anywhere → no spurious conflicts

**Audit gap.** §4.1 lists `TestCrashBeforeBaseHashCommit_*` and
`TestDownloadCommitFails_*` for INV-4, but neither covers the
fresh-state three-peer first-sync invariant directly. The
existing `TestTwoNodeSync` covers a two-peer first sync but
does not assert on conflict count and does not include a third
peer.

**Coverage gap I am surfacing per the directive.** Phase D
will add a regression test that the audit's §4 plan does not
list:

`TestFirstSync_ThreePeers_NoSpuriousConflicts` —

1. Three folder dirs (A, B, C), three Node fixtures, all
   with `last_sync_ns == 0` against each other.
2. A, B, C each have a different file at root (`a.txt`,
   `b.txt`, `c.txt`) plus a shared file `common.txt` with
   different mtimes per node.
3. Drive a full A↔B↔C index exchange round (six pairwise
   directions).
4. Assert: `common.txt` gets resolved by C1 mtime (highest
   mtime wins), no `.sync-conflict-*` file is written for
   ANY path on ANY node, and final BaseHashes maps on each
   peer state row contain entries for every shared path.
5. Drive a second exchange round (now `last_sync_ns > 0` on
   every peer state). Assert: a deliberate conflicting edit
   to `common.txt` on two nodes simultaneously DOES produce
   a `.sync-conflict-*` file — proving the classifier's
   first-sync gate flipped correctly.

This pins both the no-storm property at first sync AND the
correct switchover to conflict semantics on the second round,
which is the actual invariant Phase D promises.

### Audit amendment

The §4.1 row for INV-4 BaseHash durability lists H12 only.
Adding `TestFirstSync_ThreePeers_NoSpuriousConflicts` (and a
corresponding row in §4.1) as part of Phase D's test set is
required. The audit doc will be amended in commit 6's docs
hunk to name the test alongside H12 / H13.

---

## Implementation summary

- Ten phases A–J, single git commit, ordered by failure
  isolation (foundation → durability → semantics → write
  paths → coalesce → sweep → robustness).
- D7 CRC trailer is disk-only. Wire format is untouched.
  Mixed-version dev-loop composition is automatic.
- First-sync classifier coverage gets one new test (a fresh
  three-peer round) that the audit §4 plan does not list;
  the audit row gets an amendment as part of the same commit.
