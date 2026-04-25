# Filesync v1 — Coordinated Design for D1 / D4 / D6 / C6

> Phase 0 design for the coordinated landing of **D1** (FastCDC),
> **D4** (SQLite index), **D6** (zstd transfer), and **C6**
> (per-file vector clocks).
>
> This ships as **protocol v1**. There is no prior protocol version
> deployed anywhere; we start from scratch. Any leftover index or
> state files from development builds are deleted by hand before
> the first v1 run.
>
> **D2 (BLAKE3) was in scope in an earlier draft and has been
> deferred.** v1 stays on SHA-256. See `HASH-ALGORITHM.md` for the
> benchmark data and the reopen criteria.
>
> Status: **in progress** · last updated 2026-04-22.
> §0 Identity, §1 C6, §2 D1, and §3 D6 have landed. §4 D4 is the
> remaining v1 item. See the *Implementation status* table below for
> per-section commits. The banner flips to **implemented** once D4
> lands and the bundle-level tests are green.
>
> C7 (end-to-end transfer integrity trailer) is deliberately out of
> scope for v1. See `PLAN.md` §C7 for the reopen triggers.

---

## Guiding principles

1. **Zero-config by default.** If a knob is not essential, it is not
   a knob. Well-chosen defaults beat flexibility the user never
   asked for.
2. **Implementation quality matters as much as algorithm choice.**
   A "faster" algorithm loses to a well-optimized slower one when
   the ecosystem lacks a good implementation on our target arch.
   Pick well-maintained libraries, benchmark the real code on real
   hardware, and do not assume the paper beats the silicon.
   (See `HASH-ALGORITHM.md` for the concrete case that led to D2
   being deferred.)
3. **One protocol version. One schema.** No fallback matrix, no
   legacy path, no migration. The binary swap is cold: stop,
   upgrade, restart.
4. **Each item is still independently reviewable.** The bundle is
   one design and one protocol; the commits are one per item, in
   strict order, each green on its own.
5. **Revertable without ceremony.** Any single item's commit can
   be reverted during the implementation window. After the window
   closes, the next change ships under the next protocol version,
   never as a v1 dialect.

## Non-goals

- Interop with any prior version. There isn't one.
- Mixed-hash or mixed-chunking folders. One algorithm per v1.
- Full-mesh send-receive on every folder. Star topology stays the
  default; C6 is about being **correct** when a mesh folder goes
  live, not about promoting every folder to mesh.

---

## Item sequencing

Design together, implement strictly in this order. Each step lands
behind its own commit with tests.

| Order | Item   | Kind        | Why this slot                                     |
|-------|--------|-------------|---------------------------------------------------|
| 0     | Device ID + protocol version field | core | every later item uses them     |
| 1     | **C6** | correctness | `FileInfo` gains `version`; SQLite schema depends on it |
| 2     | **D1** | block shape | variable-sized blocks; wire shape depends on it   |
| 3     | **D6** | transport   | compression is orthogonal but cheap to wire last  |
| 4     | **D4** | storage     | schema reflects C6 + D1 final shapes              |

D4 last is deliberate: its schema is a function of the other
three. Landing D4 earlier would force a rewrite as each later
item lands. Hash stays SHA-256 throughout — if D2 is ever picked
up it becomes its own protocol bump, not a v1 dialect.

---

## Implementation status

| Section | Status | Verification |
|---------|--------|--------------|
| §0 Device ID + `protocol_version=1` | ✅ | `deviceid.go` (generation, Crockford base32, on-disk file); `protocol.go` `handleIndex` rejects mismatched `protocol_version`; `buildIndexExchange` stamps `protocolVersion`. Tests: `TestDeviceID*`, `TestHandleIndex_RejectsProtocolVersionMismatch`, `TestBuildIndexExchange_StampsProtocolVersion`. |
| §1 C6 per-file vector clocks | ✅ | `vclock.go` (`VectorClock`, `compareClocks`, `Bump`, proto round-trip); `index.go` diff classifier; `filesync.go` local-bump on write, adopt-on-receive, tombstone-clock propagation through rename plan. Tests: `vclock_test.go` (classifier, dominated, concurrent, tombstone cases), `TestFileEntry_VersionYAMLRoundTrip`, `TestFileInfo_VersionWireRoundTrip`. |
| §2 D1 FastCDC | ✅ | `fastcdc.go` in-tree chunker (`fastCDCMin/Avg/Max = 32/128/512 KiB`); `transfer.go` delta path keyed by block hash; DeltaBlock count capped. Tests: `fastcdc_test.go`, block-verify tests under `transfer_c3_test.go`. |
| §3 D6 zstd everywhere | ✅ | `internal/zstdutil` (pooled encode/decode, decode-size cap); index exchange and bundle stream on `Content-Encoding: zstd`; DeltaBlock compresses per-block with incompressible skip via `compress_probe.go`. Tests: `compress_probe_test.go`, bundle stream caps. |
| §4 D4 SQLite-backed index | ⏳ | not started. |

Review Checklist progress (full text at the bottom of this document):

- [x] Device-ID scheme.
- [x] `protocol_version=1` everywhere, no handshake.
- [x] Hash stays SHA-256; D2 deferred per `HASH-ALGORITHM.md`.
- [x] FastCDC parameters (32/128/512 KiB).
- [x] zstd level 3, magic-byte probe list, no config knob.
- [ ] SQLite schema and WAL + FULL durability choice (revised from
      the draft's NORMAL; see §Durability).
- [ ] `modernc.org/sqlite` dependency approval.
- [x] Commit order so far (ID/version → C6 → D1 → D6); D4 still to land.

---

## 0. Identity and versioning

### Protocol version

Every request and response that crosses the wire carries a
`protocol_version` field pinned to `1`. If a peer sees any other
value, it rejects the session with
`last_error="protocol_version_mismatch"` and records the mismatch
on the peer row. No negotiation. No capability list. No middle
ground. The next protocol bump is a new integer; the next schema
bump is a new SQLite `schema_version` row.

The field lives on the existing `IndexExchange` and new v1
messages — no separate `Hello` endpoint. Mismatch is detected on
the first real request and the peer is dropped before any work.

Capability-style negotiation is not introduced. If a future
change genuinely needs it, the protocol version bumps to `2` and
the negotiation arrives then. Premature flexibility is not a
feature.

### Device ID

Every node has a stable device ID, used by C6 vector clocks, peer
state tables, and conflict attribution.

- **Generation.** 6 bytes from `crypto/rand` at first start.
  Nothing derived from SSH keys, certificates, MACs, or hostnames —
  filesync identity is its own thing, decoupled from SSH identity
  so an SSH key rotation does not rewrite every vector clock. A
  random ID is also the simplest correct answer; nothing about
  derivation helps us.
- **Size.** 48 bits. Birthday collision probability becomes
  non-trivial around 16 million IDs; for personal and small-team
  use this is comfortable headroom.
- **Encoding.** Crockford base32 (no `I`, `L`, `O`, `U`; case-
  insensitive input). 6 bytes encodes as **10 characters**,
  displayed in two groups of five: `XXXXX-XXXXX`. Hand-typable.
  Parseable with `-` and whitespace ignored.
- **Storage.** Plain-text file at `~/.mesh/filesync/device-id`,
  mode `0600`. Created atomically (`write-temp + rename`) on
  first run and never rewritten unless the operator deletes it.
- **Wire.** Sent as the 10-char string in `IndexExchange` and
  `FileInfo.version`. Not the raw 6 bytes — string form keeps
  logs and APIs readable at the cost of a few bytes per message.

Comparable tools: Syncthing's 52-character IDs are famously hard
to type; the trust story there is different (TLS cert fingerprint).
We are smaller and do not need that.

### Index model — build-time protocol invariant

The persistence layer ships in two shapes — **β** (in-memory
`FileIndex` discarded between scans; SQLite is the sole source
of truth read at every scan start) and **hybrid** (in-memory
`FileIndex` retained between scans as a scan-private working
copy populated from SQLite at folder open). The choice between
them is gated by `BenchmarkLoadIndex_168kFiles` measured on the
build host (see `PERSISTENCE-AUDIT.md` §2.8 INV-2 and §6
commit 7).

**The selection is a build-time constant**, not a runtime knob:

```go
const FILESYNC_INDEX_MODEL = "beta"   // or "hybrid"
```

Stamped on every `IndexExchange` alongside `protocol_version`
and `device_id`. The handshake asserts equality — a peer whose
binary carries a different value is rejected with
`last_error="filesync_index_model_mismatch"` and dropped before
any work. Same shape as `protocol_version`: no negotiation, no
capability list, mismatch is a configuration bug.

Why build-time, not runtime: the bench varies per hardware (an
M1 may land in β; an x86 desktop may land in hybrid; a VPS
bastion may land somewhere else). If each peer chose
independently at runtime, three peers would silently run two
different model selections and the audit's invariants (INV-1
through INV-4) would only hold on the one with the matching
model. Forcing build-time selection means the operator deploys
**one** binary across all peers and the model question is
settled at deploy.

**Recorded decision (to be filled in at commit 2 of the cutover
sequence):**

```
Bench: BenchmarkLoadIndex_168kFiles
Hardware: <build host model + arch>
Result: <median> ms ± <stddev> ms across N runs
Selected: FILESYNC_INDEX_MODEL = "<beta|hybrid>"
Rationale: <reason — bench < 80 ms, ≥ 80 ms, or borderline default>
```

Updated when the bench is re-run (e.g., after a modernc driver
upgrade or a target-hardware change). Iter-4 O15 named this
the missing protocol-level invariant.

### Folder epoch — receiver re-baseline contract

Every `IndexExchange` carries the sender's
`folder_meta.epoch` (8 random bytes, hex-encoded) on the
`Epoch` field. The epoch is seeded once at folder creation
(per `PERSISTENCE-AUDIT.md` §2.6 I2) and rewritten only by
the restore-from-backup admin endpoint (§2.7 L5).

**Receiver contract.** On every incoming `IndexExchange`, the
receiver compares `Epoch` against the cached
`PeerState.LastEpoch` for that (folder, peer) pair:

- **Match (or first-ever exchange with `LastEpoch == ""`):**
  proceed normally; the delta query
  `WHERE sequence > LastSeenSequence` is the standard path.
- **Mismatch (`Epoch != LastEpoch && Epoch != ""`):** drop
  `BaseHashes`, reset `LastSeenSequence` and
  `LastSentSequence` to 0 for this (folder, peer), force a
  full re-sync on the next cycle, and record
  `last_error="epoch_mismatch"` for visibility.

The contract is symmetric: peer A's restore bumps A's epoch;
peers B and C see the new epoch on next exchange and
re-baseline their PeerState rows for A. No manual
intervention required on B / C.

**Implementation note.** `internal/filesync/filesync.go`
already implements branch A semantics (drop BaseHashes, reset
LastSeenSequence) but triggers only on
`remoteIdx.GetSequence() < peerLastSeq`. Iter-4 Z2 verified
that this trigger misses one case: a peer that was offline
during the backup-to-restore window, whose `LastSeenSeq`
predates the backup point, may not see a sequence drop after
the restore. The `PERSISTENCE-AUDIT.md` §6 commit 7 wires the
epoch-mismatch trigger explicitly:

```go
if remoteIdx.GetSequence() < peerLastSeq ||
    (remote.Epoch != "" && remote.Epoch != peer.LastEpoch) {
    // existing reset path: drop BaseHashes, reset
    // LastSeenSequence, set PendingEpoch to remote.Epoch
}
```

Pin with `TestPeer_OfflineDuringRestore_ResetsOnEpochAlone`.
The test scripts: peer A backs up at seq=1000; peer X is
offline (X.LastSeenSeq=500); A advances to seq=2000; A
restores from seq=1000 backup, epoch E1 → E2; X comes online
and exchanges with A (A reports seq=1000, X.LastSeenSeq=500
— sequence-drop trigger does NOT fire alone); the
epoch-mismatch trigger MUST fire and X re-baselines.

---

## 1. C6 — Per-file vector clocks

### Wire shape

`FileInfo` in v1:

```proto
message FileInfo {
  string path      = 1;
  int64  size      = 2;
  int64  mtime_ns  = 3;
  bytes  sha256    = 4;                // 32 bytes
  bool   deleted   = 5;
  int64  sequence  = 6;                // intra-device ordering only
  uint32 mode      = 7;
  string prev_path = 8;                // R1 rename hint
  repeated Counter version = 9;        // C6
}

message Counter {
  string device_id = 1;                // 10-char base32
  uint64 value     = 2;
}
```

`sequence` stays. It orders events inside one device's own index
and feeds the delta-index shortcut. It no longer carries conflict
semantics — that is `version`'s job.

### Semantics

Standard vector-clock rules:

1. On local write, bump the local counter (`self`'s entry in
   `version`). Other entries untouched.
2. On receive, replace local `version` with the incoming vector
   only after the content write is durable.
3. On diff, compare the two vectors:
   - `A ≤ B` strictly → `B` wins.
   - `A ≥ B` strictly → `A` wins.
   - concurrent (neither dominates) → conflict. C2's pairwise
     tiebreak (mtime, then deterministic device ID) picks a
     winner; a `.sync-conflict-*` sibling preserves the loser.
     Never overwrite silently.

### Tombstones

A tombstone carries the vector at deletion time. A later write
whose vector dominates the tombstone resurrects the file — that
is the correct outcome. The first-sync tombstone guard (already
shipped) still applies on a fresh peer.

---

## 2. D1 — FastCDC

### Parameters

`min=32 KiB`, `avg=128 KiB`, `max=512 KiB`. `avg` matches the old
fixed 128 KiB so block-count distribution stays roughly familiar;
`min`/`max` are standard FastCDC defaults. All peers run
identical parameters — determinism of boundaries depends on this.

### Library

**In-tree implementation** in `internal/filesync/fastcdc.go`. The
published Gear-hash table plus ~200 lines of boundary-emission
logic is all filesync needs; an external dependency for a fixed
algorithm with a fixed parameter set does not pay rent. The
original plan named `github.com/jotfs/fastcdc-go`; the in-tree
version shipped instead to keep the dependency graph unchanged.
If the algorithm ever needs replacing, the surface to swap is a
single file.

### Wire shape

Fixed-index block hashes go away. Every block carries its own
`(offset, length)`:

```proto
message BlockSignatures {
  string folder_id = 1;
  string path      = 2;
  int64  file_size = 3;
  repeated Block blocks = 4;
}

message Block {
  int64 offset = 1;
  int32 length = 2;
  bytes hash   = 3;            // SHA-256, 32 bytes
}

message DeltaResponse {
  int64 file_size = 1;
  repeated DeltaBlock blocks = 2;
}

message DeltaBlock {
  int64 offset = 1;
  int32 length = 2;
  bytes hash   = 3;            // SHA-256 of chunk content
  bytes data   = 4;            // empty iff the receiver already has this hash
}
```

`DeltaResponse.blocks` is the sender's complete chunk list in
offset order. For each entry, `data` is populated only when the
chunk's hash is absent from the receiver's signatures; otherwise
the receiver looks up the hash in its own local blocks and copies
the bytes from there. This handles arbitrary content shifts: a
1-byte insert at the head shifts one boundary, not all of them,
and downstream chunks still match by hash regardless of their new
offset.

### Streaming

FastCDC emits boundaries as the file is read sequentially, so
peak memory stays bounded to one `max`-sized buffer plus
bookkeeping regardless of file size. `maxSyncFileSize` (4 GiB)
is unchanged.

---

## 3. D6 — zstd everywhere, no config

Compression is a default, not a folder option. No YAML knob.

- Index exchanges: `Content-Encoding: zstd` unconditionally.
  gzip is removed from the index path.
- File transfers: zstd for everything, with a 4 KiB magic-byte
  probe on the sender side. If the leading bytes match a known-
  compressed format (`.zst`, `.gz`, `.xz`, `.bz2`, `.lz4`,
  `.jpg`, `.jpeg`, `.png`, `.gif`, `.mp3`, `.mp4`, `.webm`,
  `.zip`, `.7z`, `.pdf`, common office formats), the block is
  sent uncompressed with a `raw` flag on the `DeltaBlock`. The
  probe list is a package-level const; it is extended, not
  configured.
- Compression level: `3`. The standard "good enough, fast enough"
  default. No tuning knob until measurement demands one.

Rationale for skipping the folder config: the magic-byte probe
does what a user-tuned `compression: off` would have done, more
precisely and per-file.

---

## 4. D4 — SQLite index

No migration. No gob coexistence. Development builds may have
left `~/.mesh/filesync/<folder-id>/index.gob` (or similar)
behind; the operator deletes those by hand before the first v1
start. The v1 binary refuses to open a legacy gob file — it does
not convert it.

### Driver

`modernc.org/sqlite`. Pure-Go, CGo-free, preserves the
`CGO_ENABLED=0` release target for Linux and Windows. This is
the bundle's one new direct dependency and is requested as part
of this design.

### Schema

```sql
CREATE TABLE folder_meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
-- Rows: schema_version (=1), device_id, epoch, created_at.

CREATE TABLE files (
  folder_id TEXT    NOT NULL,
  path      TEXT    NOT NULL,
  size      INTEGER NOT NULL,
  mtime_ns  INTEGER NOT NULL,
  hash      BLOB    NOT NULL,   -- SHA-256, 32 bytes
  deleted   INTEGER NOT NULL,   -- 0 or 1
  sequence  INTEGER NOT NULL,
  mode      INTEGER NOT NULL,
  version   BLOB    NOT NULL,   -- packed [(device_id, counter)...]
  inode     INTEGER,            -- rename-hint source
  prev_path TEXT,               -- rename hint, one-shot
  PRIMARY KEY (folder_id, path)
);
CREATE INDEX files_by_seq   ON files(folder_id, sequence);
CREATE INDEX files_by_inode ON files(folder_id, inode)
  WHERE inode IS NOT NULL;

CREATE TABLE blocks (
  folder_id TEXT    NOT NULL,
  path      TEXT    NOT NULL,
  offset    INTEGER NOT NULL,
  length    INTEGER NOT NULL,
  hash      BLOB    NOT NULL,
  PRIMARY KEY (folder_id, path, offset)
);

CREATE TABLE peer_state (
  folder_id          TEXT    NOT NULL,
  peer_id            TEXT    NOT NULL,
  last_seen_seq      INTEGER NOT NULL,
  last_sent_seq      INTEGER NOT NULL,
  last_ancestor_hash BLOB,
  last_error         TEXT,
  backoff_until_ns   INTEGER,
  PRIMARY KEY (folder_id, peer_id)
);
```

### Durability

- `PRAGMA journal_mode=WAL`.
- `PRAGMA synchronous=FULL`. One extra fsync per commit in exchange
  for full power-loss protection of the last committed transaction.
  The weaker `NORMAL` setting — which the first draft of this
  document proposed — permits the last committed tx to roll back on
  power loss, which a sync tool whose value proposition is not
  losing user files cannot accept. The extra fsync is amortized by
  the P17a dirty-flag short-circuit on clean folders and by the
  per-path dirty-set on busy ones. See `PERSISTENCE-AUDIT.md` §3.3
  W5.
- Scan cycle: one `BEGIN IMMEDIATE; ... COMMIT;` transaction.
  Readers see the pre-scan snapshot until the commit.
- Peer-facing reads use SQLite's WAL snapshot isolation; no
  explicit transaction management in the read path.

### Failure isolation

A folder whose SQLite database fails to open, fails `PRAGMA
integrity_check`, or sits on a read-only filesystem enters a
per-folder **disabled** state. The dashboard renders that folder
in a red status row with the reason attached; `/api/filesync/folders`
reports `status: "disabled"`; a `mesh_filesync_folder_disabled{reason=...}`
metric goes to 1. Other folders on the same node keep syncing; other
mesh components (SSH, proxy, clipsync, gateway) are untouched. The
process does not exit — filesync is a subcomponent, and a disabled
folder must not take the rest of the binary down with it. See
`PERSISTENCE-AUDIT.md` §2.2 R8.

### Admin backup

`VACUUM INTO '<path>.bak'`. SQLite's standard idiom for a
consistent snapshot under WAL. Replaces today's file copy.

### WAL growth

A periodic `PRAGMA wal_checkpoint(TRUNCATE)` runs from the scan
goroutine once per scan cycle. Default SQLite auto-checkpoint
(1000 pages) stays enabled as a safety net.

---

## Test strategy

Per step:

1. A failing test pinning the new behavior before the commit.
2. Behavior-pinning tests for adjacent paths that the change
   touches — not only the new feature.
3. A micro-benchmark with a pinned baseline (D1 only — D6 is
   covered by end-to-end throughput, others are correctness).

Bundle-level:

- A three-node e2e scenario under the `e2e` tag exercising mesh
  mode across all three C6 cases (dominates / dominated /
  concurrent), rename via `prev_path`, tombstone, and
  resurrection.
- A protocol-version-mismatch test: start a node that sends
  `protocol_version=999`, assert both sides reject with a typed
  error and no data flows.

---

## Rollback

Commits are per-item, in order. Reverting a single commit during
the implementation window is a routine `git revert`. After the
window closes, the next change goes out under a new protocol
version, not as a v1 dialect. There is no fallback matrix to
maintain because there is no legacy peer to talk to.

---

## Review checklist

Phase 1 sign-off, re-confirmed against shipped code:

- [x] Device-ID scheme (random 6 bytes, Crockford base32, 10
      chars, `XXXXX-XXXXX` display).
- [x] `protocol_version=1` on every message, no handshake, no
      capability list.
- [x] Hash stays SHA-256; D2 deferred per `HASH-ALGORITHM.md`.
- [x] FastCDC parameters (32/128/512 KiB) and library choice
      (in-tree; see §2).
- [x] zstd level 3, magic-byte probe list, no config knob.
- [ ] SQLite schema and WAL + FULL durability choice (revised from
      the draft's NORMAL; see §Durability).
- [x] `modernc.org/sqlite` dependency approval (approved
      2026-04-22; adds under D4).
- [x] Commit order so far (ID/version → C6 → D1 → D6); D4
      still to land.
