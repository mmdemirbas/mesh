# Filesync v1 — Coordinated Design for D1 / D2 / D4 / D6 / C6

> Phase 0 design for the coordinated landing of **D1** (FastCDC),
> **D2** (BLAKE3), **D4** (SQLite index), **D6** (zstd transfer),
> and **C6** (per-file vector clocks).
>
> This ships as **protocol v1**. There is no prior protocol version
> deployed anywhere; we start from scratch. Any leftover index or
> state files from development builds are deleted by hand before
> the first v1 run.
>
> Status: **draft** · 2026-04-19. No code change until reviewed.

---

## Guiding principles

1. **Zero-config by default.** If a knob is not essential, it is not
   a knob. Well-chosen defaults beat flexibility the user never
   asked for.
2. **Implementation quality matters as much as algorithm choice.**
   A slow BLAKE3 binding beats a fast SHA-256 one only if the
   binding is good. Pick well-maintained libraries, benchmark the
   real code, and own a tiny in-tree fallback when the library is
   thin.
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
| 2     | **D2** | hash swap   | renames `sha256`→`hash`; block layout depends on it |
| 3     | **D1** | block shape | variable-sized blocks; wire shape depends on it   |
| 4     | **D6** | transport   | compression is orthogonal but cheap to wire last  |
| 5     | **D4** | storage     | schema reflects C6 + D2 + D1 final shapes         |

D4 last is deliberate: its schema is a function of the other four.
Landing D4 earlier would force a rewrite as each later item lands.

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

---

## 1. C6 — Per-file vector clocks

### Wire shape

`FileInfo` in v1:

```proto
message FileInfo {
  string path      = 1;
  int64  size      = 2;
  int64  mtime_ns  = 3;
  bytes  hash      = 4;                // BLAKE3-32, see D2
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

## 2. D2 — BLAKE3

### Choice

`github.com/zeebo/blake3`. Pure Go, no CGo, actively maintained,
benchmarks well in AVX-capable environments. Benchmark gate
before committing: ≥2.5× faster than the current SHA-256 pool on
the representative corpus. If the binding fails the gate, the
fallback is `lukechampine.com/blake3` (also pure Go, slightly
slower). We do not ship SHA-256 alongside.

### Call-site swap

All hashing goes through a single `Hasher` type with two entry
points: `Hash(io.Reader) ([]byte, error)` and `Sum(data []byte)
[]byte`. The `sha256` pool is removed entirely — no dual-algo
code paths to maintain.

### Wire

`FileInfo.sha256` → `FileInfo.hash`. Proto tag 4 retained; type
stays `bytes`; semantic is BLAKE3-32 (32 bytes, truncated from
the 32-byte BLAKE3 output which is already 32 bytes — no
truncation in practice). Block hashes (see D1) are BLAKE3-32 too.

No `algo` discriminator. Algorithm is pinned by protocol version.

---

## 3. D1 — FastCDC

### Parameters

`min=32 KiB`, `avg=128 KiB`, `max=512 KiB`. `avg` matches the old
fixed 128 KiB so block-count distribution stays roughly familiar;
`min`/`max` are standard FastCDC defaults. All peers run
identical parameters — determinism of boundaries depends on this.

### Library

`github.com/jotfs/fastcdc-go`. Pure Go, single dependency, small
surface. If review surfaces a maintenance concern, the fallback
is a ~200-line in-tree implementation built on the published
Gear-hash table; filesync does not need features beyond boundary
emission.

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
  bytes hash   = 3;            // BLAKE3-32
}

message DeltaResponse {
  int64 file_size = 1;
  repeated DeltaBlock blocks = 2;
}

message DeltaBlock {
  int64 offset = 1;
  int32 length = 2;
  bytes data   = 3;
}
```

Receiver matches by `(offset, length, hash)`, not by array index.
A 1-byte insert at the head shifts one boundary, not all of them.

### Streaming

FastCDC emits boundaries as the file is read sequentially, so
peak memory stays bounded to one `max`-sized buffer plus
bookkeeping regardless of file size. `maxSyncFileSize` (4 GiB)
is unchanged.

---

## 4. D6 — zstd everywhere, no config

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

## 5. D4 — SQLite index

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
  hash      BLOB    NOT NULL,   -- BLAKE3-32
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
- `PRAGMA synchronous=NORMAL`. Crash-safe under WAL; `FULL` is
  overhead without a concrete benefit for our workload.
- Scan cycle: one `BEGIN IMMEDIATE; ... COMMIT;` transaction.
  Readers see the pre-scan snapshot until the commit.
- Peer-facing reads use SQLite's WAL snapshot isolation; no
  explicit transaction management in the read path.

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
3. A micro-benchmark with a pinned baseline (D2 and D1 only).

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

Before Phase 1 starts, the reviewer signs off on:

- [ ] Device-ID scheme (random 6 bytes, Crockford base32, 10
      chars, `XXXXX-XXXXX` display).
- [ ] `protocol_version=1` on every message, no handshake, no
      capability list.
- [ ] BLAKE3 over SHA-256 (library choice, bench gate).
- [ ] FastCDC parameters and library choice.
- [ ] zstd level 3, magic-byte probe list, no config knob.
- [ ] SQLite schema and WAL + NORMAL durability choice.
- [ ] `modernc.org/sqlite` dependency approval.
- [ ] Commit order (ID/version → C6 → D2 → D1 → D6 → D4).
