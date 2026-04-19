# RESEARCH-SYNCTHING.md
# Syncthing Deep Research — Reference for filesync Gap Analysis

> **Purpose**: This document is a comprehensive technical reference for Claude Code to analyze
> the `filesync` feature in the `mesh` project against Syncthing's battle-tested implementation.
> Every section maps to a potential gap area. Use this to audit current implementation,
> identify missing subsystems, and prioritize what to build or harden.
>
> **Source**: Derived from Syncthing source code (github.com/syncthing/syncthing), BEP protocol
> spec, academic literature, and state-of-the-art research in distributed file synchronization.

---

## Table of Contents

1. [Protocol Layer — Block Exchange Protocol (BEP)](#1-protocol-layer--block-exchange-protocol-bep)
2. [File Model — Global / Local / Need](#2-file-model--global--local--need)
3. [Versioning and Conflict Resolution](#3-versioning-and-conflict-resolution)
4. [Change Detection Subsystem](#4-change-detection-subsystem)
5. [Scanning and Hashing Pipeline](#5-scanning-and-hashing-pipeline)
6. [Delta Sync — Rolling Hash and Block Reuse](#6-delta-sync--rolling-hash-and-block-reuse)
7. [Metadata Store — LevelDB Schema](#7-metadata-store--leveldb-schema)
8. [Data Integrity — Write Path](#8-data-integrity--write-path)
9. [Transport and Security](#9-transport-and-security)
10. [NAT Traversal and Discovery](#10-nat-traversal-and-discovery)
11. [Parallel Transfer and Scheduling](#11-parallel-transfer-and-scheduling)
12. [Folder and File Filtering](#12-folder-and-file-filtering)
13. [Special File Types and Edge Cases](#13-special-file-types-and-edge-cases)
14. [Error Handling and Self-Healing](#14-error-handling-and-self-healing)
15. [Performance Characteristics at Scale](#15-performance-characteristics-at-scale)
16. [State-of-the-Art Improvements Over Syncthing](#16-state-of-the-art-improvements-over-syncthing)
17. [Gap Analysis Checklist for filesync](#17-gap-analysis-checklist-for-filesync)
18. [Implementation Priority Matrix](#18-implementation-priority-matrix)

---

## 1. Protocol Layer — Block Exchange Protocol (BEP)

### 1.1 Overview

BEP (Block Exchange Protocol) is Syncthing's custom application-layer protocol over TLS.
All messages are length-prefixed Protobuf3. The protocol is versioned; current is BEP v1.

Reference spec: https://docs.syncthing.net/specs/bep-v1.html

### 1.2 Message Types

| Message | Direction | Purpose |
|---|---|---|
| `Hello` | Bidirectional | Handshake, device name, client version |
| `ClusterConfig` | Bidirectional | Announce shared folders, device capabilities |
| `Index` | Sender → Receiver | Full file listing for a folder with block hashes |
| `IndexUpdate` | Sender → Receiver | Incremental diff since last `Index` or `IndexUpdate` |
| `Request` | Receiver → Sender | Ask for specific block by (folder, filename, offset, size, hash) |
| `Response` | Sender → Receiver | Block data payload |
| `DownloadProgress` | Sender → Receiver | Announce which blocks are partially downloaded (for temp file reuse) |
| `Ping` | Bidirectional | Keepalive |
| `Close` | Bidirectional | Graceful disconnect with reason |

### 1.3 Index Message Structure

Each file entry in an `Index`/`IndexUpdate` contains:
- `name` — relative path within folder (UTF-8, forward slashes)
- `type` — FILE, DIRECTORY, SYMLINK, or INVALID
- `size` — total file size in bytes
- `permissions` — Unix permission bits
- `modified_s`, `modified_ns` — modification time (seconds + nanoseconds)
- `modified_by` — device short ID that last modified the file
- `deleted` — tombstone flag
- `invalid` — file is present but not syncable (e.g., permission error on sender)
- `no_permissions` — permission sync disabled for this entry
- `version` — `Vector` — version vector (see §3)
- `sequence` — monotonic per-device sequence number
- `blocks` — list of `BlockInfo` (offset, size, weak_hash, hash)
- `symlink_target` — populated only for symlinks
- `local_flags` — bitmask for must_rescan, ignored, etc.

### 1.4 Block Info Structure

```
BlockInfo {
  offset:    int64   // byte offset within file
  size:      int32   // block size (last block may be smaller)
  hash:      bytes   // SHA-256 of block content (32 bytes)
  weak_hash: uint32  // Adler32/rolling hash for delta-sync pre-screening
}
```

Default block size: **128 KiB** (131072 bytes). Fixed per file — all blocks same size except
the last one which is `file_size % block_size`.

**Gap check**: Does `filesync` use a block model? What is the block size? Is block size
negotiable or adaptive?

### 1.5 Sequence Numbers and Incremental Replication

Every `FileInfo` carries a `sequence` — a monotonically increasing integer per device per folder.
When a device reconnects after a gap:
1. It sends its last-known sequence for that device to its peer
2. The peer responds with only `FileInfo` entries with `sequence > last_known`
3. No full `Index` retransmission needed

This is the most important performance property for large folders — O(changes) not O(folder size)
on reconnect.

**Gap check**: Does `filesync` track per-device sequence numbers? Can it do incremental
index sync on reconnect?

---

## 2. File Model — Global / Local / Need

### 2.1 Three-View Model

Syncthing maintains three independent views of each file per folder:

```
Global  = The "best" version known across all devices in the cluster
Local   = What this device actually has on disk (verified)
Need    = Global - Local (what must be fetched or updated)
```

The **Global view** is computed by comparing all `FileInfo` entries received from all peers
plus the local device. The "winner" is the `FileInfo` with the highest version (see §3).

The **Local view** is rebuilt from:
1. The database (fast path, uses stored mtime/size to skip re-hashing)
2. Full disk scan (slow path, triggered on startup or after inotify overflow)

The **Need set** drives all transfer scheduling. A file is "needed" if:
- `global.version > local.version` (remote has newer version)
- `global.deleted == true && local.deleted == false` (need to delete locally)
- `global.deleted == false && local.deleted == true` (need to restore)
- Local file exists but fails block verification (corruption detected)

**Gap check**: Does `filesync` maintain explicit Global/Local views? Or does it compute
diffs on-the-fly? On-the-fly is O(n) per sync cycle; explicit views are O(changes).

### 2.2 Folder States

Syncthing tracks folder-level states: `idle`, `scanning`, `sync-preparing`, `syncing`,
`sync-waiting`, `error`, `stopped`, `watch-wait`. State transitions are logged and exposed
via the REST API. These states prevent concurrent scan+sync races.

**Gap check**: Does `filesync` have explicit folder-level state machine? Race conditions
between scan and sync are a real correctness hazard.

### 2.3 The "Receive Only" and "Send Only" Modes

Syncthing supports asymmetric folders:
- **Send Only**: local changes are shared; remote changes are never applied locally
- **Receive Only**: remote changes are applied; local changes are never shared
- **Send & Receive** (default): full bidirectional

These are critical for backup target scenarios (Receive Only) and distribution scenarios
(Send Only — like a central server pushing config files to many nodes).

**Gap check**: Does `filesync` support asymmetric sync modes?

---

## 3. Versioning and Conflict Resolution

### 3.1 Version Vectors

Each `FileInfo` has a `version` field of type `Vector`:
```
Vector {
  counters: []Counter
}
Counter {
  id:    uint64  // Short device ID (first 8 bytes of device hash)
  value: uint64  // Monotonically increasing counter for this device
}
```

When a device modifies a file:
1. Find the existing version vector for that file
2. Increment the counter for the local device ID
3. Keep all other counters unchanged

This gives a **partial order** over file versions:
- `A > B` if all counters in A ≥ B and at least one is strictly greater
- `A || B` (concurrent) if neither dominates — this is a **conflict**

### 3.2 Conflict Detection

Two `FileInfo` entries are in conflict if their version vectors are **incomparable** (concurrent).
Syncthing's resolution policy:

1. **Keep both versions** — the conflict file is saved as:
   `<name>.sync-conflict-YYYYMMDD-HHMMSS-<DEVICEID7>.<ext>`
2. The **newer mtime wins** the canonical filename slot
3. The conflict file is itself synced to all peers (so both sides see both versions)
4. Up to `maxConflicts` conflict versions are kept (default: 10, configurable)

**Critical weakness**: The mtime tiebreaker creates false conflicts when:
- System clocks differ by more than the sync round-trip time
- NTP correction causes mtime regression
- Files are restored from backup with old mtimes

### 3.3 State-of-the-Art Alternative: Full Vector Clock Conflict Resolution

True vector clocks avoid the mtime tiebreaker entirely:
- Device A modifies file: vector becomes `{A:3, B:1}`
- Device B modifies same file concurrently: vector becomes `{A:2, B:2}`
- Neither dominates → genuine conflict, create conflict file
- Device A modifies file after seeing B's version: vector becomes `{A:4, B:2}` → no conflict

Implementation cost: ~48 bytes extra per file in the metadata store. Worth it at any scale.

**Gap check**: Does `filesync` use version vectors? Or a simpler mtime-only comparison?
mtime-only will produce false conflicts in any multi-device scenario with clock drift.

### 3.4 Tombstones (Deletions)

Deletions are not simply removed from the index. A deleted file becomes a tombstone:
```
FileInfo {
  name:     "path/to/file"
  deleted:  true
  version:  {counters: [{id: <device>, value: 5}]}
  sequence: 1042
}
```

Tombstones propagate like normal file updates. A peer receiving a tombstone with a higher
version than its local copy will delete the local file. Tombstones are retained in the
database indefinitely (needed for correct reconnect behavior).

**Gap check**: How does `filesync` handle deletions? Tombstones are mandatory for
correctness — without them, a device offline during deletion will "resurrect" the file
on reconnect.

### 3.5 Invalid Files and Local Flags

Files can be marked `invalid` (present on disk, not synced) or carry `local_flags`:
- `FlagLocalUnsupported` — file type not supported on this OS (e.g., symlink on Windows)
- `FlagLocalIgnored` — matches an ignore pattern
- `FlagLocalMustRescan` — flagged for forced rescan on next pass
- `FlagLocalReceiveOnly` — modification detected on a receive-only folder

---

## 4. Change Detection Subsystem

### 4.1 Architecture

Syncthing's watcher is a two-stage pipeline:

```
FS Events (inotify/FSEvents/ReadDirChanges)
    │
    ▼
Event Debouncer (accumulate events for N seconds, default 10s)
    │
    ▼
Scan Queue (deduplicated path list)
    │
    ▼
Scanner (hash changed files, update DB)
    │
    ▼
Index Broadcaster (send IndexUpdate to peers)
```

### 4.2 OS-Specific Backends

| OS | Backend | Notes |
|---|---|---|
| Linux | inotify | Recursive watch requires one FD per directory; large trees exhaust `fs.inotify.max_user_watches` (default 8192) |
| macOS | FSEvents | Single watch per subtree root; batched delivery |
| Windows | ReadDirectoryChangesW | Per-directory, recursive flag available |
| FreeBSD | kqueue | Per-file/directory FD; scales poorly with depth |
| Fallback | Polling | Used when OS backend unavailable or fails |

**inotify limitations** (Linux, critical):
- Default limit: 8192 watches — far below 100K directory trees
- Event queue has finite kernel buffer: overflow drops events silently, emits `IN_Q_OVERFLOW`
- Network-mounted filesystems (NFS, CIFS) do not generate inotify events
- Moves between watched subtrees generate `IN_MOVED_FROM` + `IN_MOVED_TO` paired by cookie

Syncthing's overflow handler: when `IN_Q_OVERFLOW` is detected, schedule a full folder rescan.

### 4.3 Debouncing

Raw FS events are debounced with a 10-second window by default (`fsWatcherDelayS`).
Rationale: many editors do write-rename-write-rename sequences; debouncing collapses
them into a single scan trigger. Without debouncing, a large `git checkout` triggers
N individual file scans.

### 4.4 Recursive Directory Watching Strategy

For inotify, Syncthing adds a watch for every directory in the tree. On directory creation,
it immediately adds a watch for the new directory. This is O(directories) in FD count.

For FSEvents (macOS), a single watch on the root suffices — the kernel delivers full paths.

### 4.5 Periodic Full Scan

`fsWatcherDelayS` (default: 3600s) triggers a full folder scan regardless of events.
This is the safety net for:
- Events missed due to inotify overflow
- NFS/CIFS-mounted folders where events never arrive
- Bug recovery

**Gap check**: Does `filesync` have a periodic rescan fallback? What is the inotify watch
limit handling strategy? Does it handle `IN_Q_OVERFLOW`?

### 4.6 State-of-the-Art Alternative: fanotify (Linux 5.1+)

`fanotify` with `FAN_MARK_FILESYSTEM`:
- Single file descriptor watches an **entire filesystem mount** — no per-directory FDs
- Events include the full path (no cookie-matching needed for moves)
- **No queue overflow** — the kernel blocks the modifying process if the fanotify queue fills
- Available since Linux 3.8, `FAN_REPORT_NAME`/`FAN_REPORT_DIR_FID` added in 5.1
- Requires `CAP_SYS_ADMIN` or `CAP_DAC_READ_SEARCH`

This eliminates the need for periodic full scans on Linux entirely.

### 4.7 State-of-the-Art Alternative: eBPF VFS Hooks

Attach eBPF programs to `vfs_write`, `vfs_rename`, `vfs_unlink`, `vfs_mkdir`, `vfs_rmdir`.
- Zero kernel events missed — hooks execute in the syscall path
- Works on any filesystem including NFS (hook is on local VFS layer)
- Outputs structured events to a ring buffer consumed by userspace
- Does not require elevated privileges with modern Linux security policies (CAP_BPF)
- Used in production by Falco, Tetragon, and similar tools

---

## 5. Scanning and Hashing Pipeline

### 5.1 Scan Decision Tree

For each file encountered during a scan, Syncthing decides what to do:

```
1. Does the path match an ignore pattern?
   YES → skip, mark as locally ignored
   NO  → continue

2. Stat the file: get mtime, size, inode, nlink
   ERROR → mark as invalid, continue

3. Does stat match the database record exactly (mtime_s, mtime_ns, size, inode)?
   YES → trust the DB record, skip hashing (fast path)
   NO  → continue to hashing

4. Hash all blocks (SHA-256, 128 KiB chunks)
   Compute weak_hash (Adler32) per block

5. Compare block hash list with DB record
   IDENTICAL → update mtime/size in DB only (mtime-only change, e.g. touch)
   DIFFERENT → create new FileInfo, increment version counter, mark as changed

6. Send IndexUpdate to connected peers
```

**Critical optimization**: Step 3 — the mtime+size+inode pre-filter. For a 100K file folder
where 99% of files are unchanged, this reduces hash computation by ~99%. Without it, a
full rescan costs minutes; with it, seconds.

**Gap check**: Does `filesync` implement the mtime+size+inode pre-filter before hashing?
Without this, 100K file rescans will be unacceptably slow.

### 5.2 Inode Tracking for Move/Rename Detection

Syncthing tracks inode numbers. When a file is detected as "new" (not in DB by path), Syncthing
checks if any existing DB entry has the same inode number — indicating a rename/move.
If matched:
- The old path gets a tombstone
- The new path reuses the block hashes from the old path (no re-hash)
- Only an `IndexUpdate` for both entries is sent; no data retransfer

Without inode tracking, a rename of a large file generates: delete-old + full-upload-new.

**Gap check**: Does `filesync` track inodes for rename detection? This is critical for
operations like `git mv`, IDE refactors, and file manager moves.

### 5.3 Hardlink Handling

Files with `nlink > 1` share data blocks. Syncthing syncs hardlinked files as independent
copies — it does not preserve hardlink relationships across devices. Each hardlink is treated
as a separate file with its own `FileInfo`.

On Linux, creating a hardlink to an already-synced file triggers a scan of the new path
but the inode matches an existing entry — handled via inode tracking above.

### 5.4 Symlink Handling

Symlinks are synced by:
- Recording `symlink_target` in `FileInfo` (the target path, verbatim)
- On the receiving end: `os.Symlink(target, path)` regardless of whether target exists
- Dangling symlinks are fully supported
- Symlinks to paths outside the folder are synced but may not be meaningful on peers

On Windows, symlinks are skipped by default (requires elevated privileges to create).

### 5.5 Sparse File Handling

Syncthing does not detect or preserve sparse files. A 10 GB sparse file (1 MB actual data)
will be transferred as 10 GB of data and written as a non-sparse file on the receiver.
This is a known limitation documented in the FAQ.

**State-of-the-art**: Use `SEEK_DATA`/`SEEK_HOLE` (Linux 3.1+) to detect sparse regions
during scanning; skip zero-block transfers; use `fallocate(FALLOC_FL_PUNCH_HOLE)` on write.

---

## 6. Delta Sync — Rolling Hash and Block Reuse

### 6.1 Rolling Checksum (rsync Algorithm)

When a file is modified, Syncthing uses an rsync-inspired approach to find unchanged blocks:

1. **Sender** provides block list with `(offset, size, weak_hash, strong_hash)` per block
2. **Receiver** scans its existing local copy of the file using a rolling window
3. For each window position, compute Adler32 weak hash
4. If weak hash matches any block in sender's list → compute SHA-256 to confirm
5. Confirmed match → block is already local; skip requesting it
6. No match → add to the "need" list

This is the **copy-from-existing** mechanism. The receiver pulls only the blocks it doesn't have.

### 6.2 Block Reuse Across Files

Syncthing's `copyRangeMethod` looks for needed blocks in other files in the same folder:
1. For each needed block (identified by SHA-256 hash), search the local DB for any file
   containing a block with the same hash
2. If found, copy the block locally using `copy_file_range` (Linux) or manual copy
3. Only fetch from network if local copy not found

This means:
- Duplicating a file within a sync folder = zero network traffic
- `cp large_file large_file_copy` → sync via local block reuse, no upload

### 6.3 copy_file_range and OS-Level Zero-Copy

On Linux ≥ 4.5, `copy_file_range(2)` copies data between file descriptors in the kernel —
no userspace buffer allocation. On the same filesystem, this is a reflink (CoW) on
filesystems that support it (Btrfs, XFS, APFS).

### 6.4 Fixed Block Size — The Critical Limitation

Syncthing uses **fixed-size blocks** (128 KiB). If a byte is inserted at offset 0 of a
1 MB file:
- Every block boundary shifts by 1 byte
- Every block hash changes
- The entire file must be retransferred

This is catastrophic for:
- Log files (append at end: only last block changes — actually fine)
- Database files (page-aligned writes — fine with 128 KiB blocks if page-aligned)
- Container images (layer insertion at beginning — catastrophic)
- Office documents with embedded binary data

### 6.5 State-of-the-Art Alternative: Content-Defined Chunking (CDC)

Content-Defined Chunking (CDC) sets chunk boundaries based on file content, not position.
The Rabin fingerprint or Gear hash algorithm scans for "cut points" — positions where a
rolling hash matches a target value modulo a window.

**FastCDC** (2016, used by Borg Backup, Casync, Restic):
- Uses a simplified Gear hash (table-driven, 8 bytes/cycle)
- Targets average chunk size (e.g., 128 KiB) with configurable min/max
- Chunk boundaries are **content-stable** — inserting bytes only affects the chunks
  containing the insertion point
- ~2-3× better delta efficiency on text/binary files with insertions
- Reference: FastCDC paper: https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia

**Implementation sketch** (Go):
```go
type Chunker struct {
    minSize  int     // 32 KiB
    avgSize  int     // 128 KiB  
    maxSize  int     // 512 KiB
    table    [256]uint64  // Gear hash table (random, fixed seed)
}

func (c *Chunker) NextChunk(r io.Reader) ([]byte, error) {
    // Accumulate until hash & mask == 0 (normalized CDC)
    // Enforce min/max size bounds
}
```

**Gap check**: Does `filesync` use CDC or fixed-size blocks? This is the single biggest
opportunity to out-perform Syncthing on delta efficiency for non-aligned modifications.

---

## 7. Metadata Store — LevelDB Schema

### 7.1 LevelDB Overview

Syncthing uses **LevelDB** (via the `syndtr/goleveldb` Go port) as its metadata store.
LevelDB is a log-structured merge-tree (LSM) key-value store:
- Writes are sequential (fast), sorted into SSTable files
- Reads are point lookups or range scans
- Periodic compaction merges SSTables and removes tombstones
- No SQL, no transactions (beyond single-key atomicity)

Database location: `~/.local/share/syncthing/index-v0.14.0.db/` (Linux)

### 7.2 Key Schema

Key format: `<1-byte type prefix> + <folder_id bytes> + <separator> + <device_id bytes> + <separator> + <file_path bytes>`

Primary key types:
- `0x00` — device file entry (remote peer's file version)
- `0x01` — local file entry (this device's version)
- `0x02` — global file entry (current "winner" across all devices)
- `0x03` — need entry (files required by this device)
- `0x04` — sequence index (sequence# → file path, for incremental replication)

### 7.3 Compaction and Performance at Scale

LevelDB compaction pauses are a known issue at 100K+ files:
- Write-heavy initial scan (100K files × ~500 bytes per FileInfo = ~50 MB write)
- Compaction triggered by write amplification can stall reads for seconds
- `goleveldb` has less tuning surface than RocksDB

**State-of-the-art alternatives**:

**RocksDB**:
- Column families allow separate compaction tuning per data type
- `Rate limiter` prevents compaction I/O from overwhelming sync I/O
- Range tombstones for efficient bulk deletes (folder removal)
- Write batches with WWAL for atomic multi-key updates

**SQLite (WAL mode)**:
- Familiar SQL schema, rich query flexibility
- WAL mode: reads don't block writes
- `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;` — safe and fast
- Trivially supports: "give me all files with sequence > N", JOIN queries for need calculation
- Single-file database, easy backup/restore

**Gap check**: What is `filesync` using for metadata? What happens to scan/query
performance at 100K files? Can it do efficient "give me changes since sequence N" queries?

### 7.4 Required Indexes for Correct Operation

Any metadata store used by a sync engine needs these access patterns at minimum:
1. `GET file_by_path(folder, device, path)` — for scan pre-filter check
2. `GET files_by_sequence_after(folder, device, seq)` — for incremental replication
3. `SCAN all_files(folder, device)` — for full index exchange on first connect
4. `GET files_needed(folder)` — for transfer scheduling
5. `GET blocks_by_hash(hash)` — for block reuse lookup across files
6. `PUT file(folder, device, path, fileinfo)` — during scan
7. `DELETE file(folder, device, path)` — tombstone insertion

Access pattern #5 (blocks_by_hash) is the most write-intensive to maintain — requires
a separate reverse index: `block_hash → [(file_path, block_offset)]`.

---

## 8. Data Integrity — Write Path

### 8.1 The Full Safe Write Sequence

This is Syncthing's most critical correctness guarantee. Every file write follows this exact sequence:

```
1. Create temp file: <dir>/.syncthing.<filename>.<random_hex>.tmp
   (same directory as target — ensures same filesystem for atomic rename)

2. For each needed block (in any order):
   a. Request block from peer (or copy from local file)
   b. Receive block data
   c. Hash received data with SHA-256
   d. Compare against expected hash from FileInfo.blocks[i].hash
   e. MISMATCH → discard block, re-request from different peer (if available)
      or mark file as permanently failed after N retries
   f. Write verified block to correct offset in temp file

3. After all blocks written:
   a. Verify temp file total size == FileInfo.size
   b. (Optional) Compute and verify full-file hash
   c. Set file permissions (FileInfo.permissions)
   d. Set file mtime (FileInfo.modified_s, FileInfo.modified_ns)
   e. Atomic rename: os.Rename(tmpPath, finalPath)
      On Linux: this is a single syscall (rename(2)) — atomic on POSIX filesystems
      On Windows: MoveFileExW with MOVEFILE_REPLACE_EXISTING

4. Update local database entry to match received FileInfo

5. Send IndexUpdate to peers (confirming local copy is now up to date)
```

**Why temp file in same directory?** `os.Rename` is only atomic when source and destination
are on the same filesystem (same mount point). Cross-filesystem rename = copy + delete,
which is NOT atomic. Using the same directory as the target guarantees same filesystem.

**Gap check**: Does `filesync` use this exact sequence? Specifically:
- Block-level hash verification before write (not just after)?
- Atomic rename from same-directory temp file?
- Re-request on hash mismatch?
- Correct mtime setting (does it set mtime AFTER permissions, before or after rename)?

### 8.2 Mtime Preservation

Setting mtime correctly is subtle:
- `os.Chtimes(path, atime, mtime)` — sets both access and modification time
- Must be called BEFORE the rename (so the final file has correct mtime from the start)
- Or called AFTER rename (simpler, but there's a brief window with wrong mtime)
- Syncthing calls it on the temp file before rename — preferred approach
- Nanosecond precision: not all filesystems support sub-second mtime (FAT32 = 2s resolution)
- `modified_ns` field in FileInfo stores nanoseconds separately for filesystems that support it

### 8.3 Permission Handling

```
default_permissions = FileInfo.permissions & ^umask
```

Syncthing respects the receiver's umask. Executable bits are preserved. SUID/SGID bits
are stripped for security. On Windows, permissions are mapped to read-only attribute only.

### 8.4 Partial Transfers and Resume

Syncthing's `DownloadProgress` message announces which blocks of a temp file are already
downloaded. When a connection drops mid-transfer:
1. The temp file remains on disk with partial content
2. On reconnect, the receiver sends `DownloadProgress` for the partial temp file
3. The peer skips sending already-received blocks
4. Transfer resumes from where it left off (block-level resume)

This is free-standing resumable transfer with no explicit resume protocol — just the
block model naturally supports it.

**Gap check**: Does `filesync` support mid-transfer resume? Does it preserve temp files
across connection drops?

### 8.5 Folder Marker Files

Syncthing writes a `.stfolder` marker file at the root of each synced folder. If this
marker is absent on startup, Syncthing refuses to sync (assumes the folder was unmounted
or is missing). This prevents the catastrophic scenario of syncing deletions into an
empty mount point.

**Gap check**: Does `filesync` have a folder health check to prevent syncing into an
unmounted or missing directory?

---

## 9. Transport and Security

### 9.1 Device Identity

Each Syncthing device has a **device certificate** — a self-signed X.509 certificate with
an ECDSA P-384 key pair. The **device ID** is derived as:

```
device_id = base32(SHA-256(DER-encoded certificate)) // 52 chars, grouped as 7-char segments
```

No CA involved. Trust is established by exchanging device IDs out-of-band (QR code, copy-paste).
Pinning is at the TLS layer — connection refused if certificate hash doesn't match known device ID.

### 9.2 TLS Configuration

- TLS 1.3 minimum (TLS 1.2 as fallback for older clients)
- Cipher suite: TLS_AES_128_GCM_SHA256, TLS_AES_256_GCM_SHA384, TLS_CHACHA20_POLY1305_SHA256
- Certificate pinning via custom `tls.Config.VerifyPeerCertificate` — validates that the
  presented certificate matches the expected device ID
- Perfect Forward Secrecy via ephemeral key exchange (TLS 1.3 mandates this)

### 9.3 QUIC Transport

Syncthing uses QUIC (RFC 9000) for primary connectivity:
- Multiplexed streams over a single UDP connection — no head-of-line blocking
- 0-RTT connection establishment for known peers (session resumption)
- Built-in loss recovery better suited to mobile/unreliable networks than TCP
- NAT traversal via simultaneous open (both sides attempt connection at same time)

TCP is used as fallback when QUIC is unavailable.

### 9.4 Message Framing

All BEP messages over the stream:
```
[4 bytes: message length (big-endian uint32)] [N bytes: protobuf-encoded message]
```

Header contains message type and compression flag. LZ4 compression is applied per-message
when `compression = always | metadata` (metadata messages compress well; data blocks less so).

**Gap check**: Does `filesync` have per-message or per-connection compression? Block data
from binary files compresses poorly; text files can achieve 3-5× ratio with zstd.

---

## 10. NAT Traversal and Discovery

### 10.1 Local Discovery (LAN)

Syncthing broadcasts a UDP announcement on port 21027 to the local network multicast group.
Announcement contains: device ID, list of listening addresses.
Peers on the same LAN discover each other without internet connectivity.

### 10.2 Global Discovery

A DNS-based global discovery server (discovery.syncthing.net) stores `device_id → [addresses]`
mappings. Lookups are HTTPS GET requests. Announcements are authenticated (device signs the
announcement with its certificate private key). The global discovery server never sees
file contents — only IP:port pairs.

Syncthing supports custom discovery servers for air-gapped deployments.

### 10.3 Relay Protocol (STTP)

When direct connection fails (symmetric NAT, firewall), Syncthing uses relay servers:
- Community-run `relaysrv` instances listed in `relays.syncthing.net`
- STTP (Simple Tunnel Transport Protocol): relay brokers a rendezvous between two peers
- Once rendezvous established, relay forwards encrypted BEP traffic
- Relay cannot read content — traffic is end-to-end TLS encrypted
- Performance: ~100 Mbps per relay (sufficient for most use cases)

### 10.4 NAT Hole Punching

For QUIC connections, Syncthing attempts UDP hole punching:
1. Both peers connect to a signaling server and exchange their external IP:port
2. Both simultaneously send UDP packets to each other's external address
3. If NAT is "full-cone" or "address-restricted cone", this succeeds
4. Symmetric NAT requires relay fallback

**Gap check**: For mesh, NAT traversal may be already handled by the gateway. But does
`filesync` have a fallback path when direct P2P fails?

---

## 11. Parallel Transfer and Scheduling

### 11.1 Concurrent Block Requests

Syncthing fetches blocks in parallel:
- Multiple goroutines pull blocks from the same or different peers simultaneously
- Default: up to 4 concurrent block requests per file (configurable)
- Each request is pipelined — next request sent before previous response arrives

### 11.2 Multi-Peer Parallel Fetch

When multiple peers have the same file, Syncthing can fetch different blocks from different peers:
- Block 0-15 from peer A (highest bandwidth)
- Block 16-31 from peer B (second best)
- Similar to BitTorrent's "rarest first" piece selection — but Syncthing uses a simpler
  sequential assignment rather than true rarest-first

True rarest-first (BitTorrent) would be beneficial in a mesh scenario: prefer blocks that
fewer peers have to maximize early replication diversity.

### 11.3 Transfer Prioritization

Syncthing uses a simple priority model:
1. Files modified most recently (highest mtime) are pulled first
2. Smaller files before larger files within the same mtime bucket
3. No explicit bandwidth reservation or QoS

**State-of-the-art**: Weighted fair queuing per peer, with priority lanes for:
- Real-time modified files (highest priority)
- Batch cold migration (lowest priority, yield to other traffic)
- Control messages (out-of-band, never queued behind data)

### 11.4 Bandwidth Throttling

Syncthing has built-in bandwidth limits:
- `maxSendKbps` / `maxRecvKbps` per peer
- Token bucket algorithm (leaky bucket variant)
- Separate limits for LAN vs WAN (LAN detection by RFC1918 address check)

**Gap check**: Does `filesync` have any bandwidth throttling? Without it, a large sync
will saturate the link and impact other mesh traffic.

---

## 12. Folder and File Filtering

### 12.1 .stignore Syntax

Syncthing uses `.stignore` files (similar to `.gitignore`) with extended glob syntax:
```
# Ignore OS files
.DS_Store
Thumbs.db

# Ignore build artifacts  
/build/
*.o
*.pyc

# Include exceptions (override ignore)
!important.pyc

// Double-slash comment (also supported)
(?d)directory_to_delete_on_remote  // (?d) = include locally but delete on peers
(?i)CASE_INSENSITIVE_PATTERN       // (?i) = case-insensitive match
```

`.stignore` is synced between peers as a normal file — all peers use the same ignore rules.

### 12.2 Default Ignores

Hard-coded patterns applied before `.stignore`:
- `~$*` — Windows Office temp files
- `.~lock.*` — LibreOffice temp files
- `*.tmp` (in some versions)
- Files starting with `.syncthing.` (temp files)

### 12.3 Ignore Case Sensitivity

On case-insensitive filesystems (APFS on macOS, NTFS on Windows), pattern matching is
case-insensitive by default. On Linux (ext4, xfs), case-sensitive.

**Gap check**: Does `filesync` support ignore patterns? Without this, users cannot
prevent temp files, build artifacts, and secrets from syncing.

---

## 13. Special File Types and Edge Cases

### 13.1 Large Files (> 4 GiB)

`FileInfo.size` is `int64` — supports files up to 9.2 EB.
Block offsets are also `int64`. No issues with files > 4 GiB.

### 13.2 Files With Special Characters in Names

Syncthing normalizes filenames to NFC (Unicode Normalization Form C) on case-insensitive
or Unicode-normalizing filesystems (APFS, NTFS). On Linux, filenames are raw bytes —
no normalization. Cross-platform sync of files with non-ASCII names requires careful
handling of these differences.

**Edge case**: A file named `café` (NFC: `caf\xc3\xa9`) vs `café` (NFD: `cafe\xcc\x81`)
is the same on macOS but two different files on Linux.

### 13.3 Filename Conflicts on Case-Insensitive Filesystems

If a Linux sender has both `README.md` and `readme.md`, syncing to a macOS/Windows receiver
creates a collision. Syncthing renames one of them with a `.sync-conflict` suffix.

### 13.4 Very Deep Directory Trees

Some filesystems have `PATH_MAX` = 4096 bytes. Very deep trees can hit this limit.
Syncthing does not explicitly guard against this but the OS will return `ENAMETOOLONG`.

### 13.5 Files Modified During Transfer

A file being actively written while being transferred will have mismatching block hashes.
Syncthing detects this when the final hash check fails and re-queues the file for transfer.
The file may oscillate if it's continuously written — Syncthing limits retries.

### 13.6 Atomic File Replacement (Editor Pattern)

Many editors save via: `write(<file>.tmp)` → `rename(<file>.tmp, <file>)`.
This generates: `CREATE <file>.tmp` + `CLOSE_WRITE <file>.tmp` + `MOVED_TO <file>`.
Syncthing correctly handles this by only processing the final `MOVED_TO` event.
The temp file itself matches the `~$*` ignore pattern.

### 13.7 Windows-Specific Issues

- Reserved filenames: `CON`, `PRN`, `AUX`, `NUL`, `COM1`-`COM9`, `LPT1`-`LPT9`
- Reserved characters in filenames: `< > : " / \ | ? *`
- Files with trailing spaces or dots in names
- Syncthing renames conflicting files with `~<name>` prefix on Windows

**Gap check**: Does `filesync` sanitize filenames for cross-platform compatibility?

---

## 14. Error Handling and Self-Healing

### 14.1 Pull Failure Handling

When a file cannot be pulled (all peers fail to provide a block):
1. Mark file with `pullError` state and an error message
2. Back off exponentially (1s, 2s, 4s... up to 1 hour)
3. Retry on reconnect or after backoff expires
4. Never mark a file as "successfully synced" if any block failed

### 14.2 Scan Error Handling

When a file cannot be scanned (permission denied, I/O error):
1. Mark with `invalid` flag in `FileInfo`
2. Propagate `invalid` to peers (they won't try to fetch it)
3. Log error with file path and errno

### 14.3 Database Corruption Recovery

If LevelDB is corrupted:
1. Syncthing detects corruption on open (LevelDB returns error)
2. Deletes and recreates the database
3. Triggers a full folder rescan to rebuild state
4. This is safe because the source of truth is the filesystem, not the database

**Gap check**: Does `filesync` have a recovery path if its metadata store is corrupted?
The filesystem is always the ground truth; the DB is a performance cache.

### 14.4 "Out of Disk Space" Handling

Before writing a temp file, Syncthing checks available disk space:
1. Get file size from `FileInfo.size`
2. Check `statvfs` available space on target filesystem
3. If insufficient: log error, skip file, retry later

**Gap check**: Does `filesync` check disk space before writing? Running out of disk mid-transfer
leaves corrupt temp files and can corrupt the metadata store.

### 14.5 Hash Mismatch Handling

When a received block's hash doesn't match the expected hash:
1. Discard the block (do not write to temp file)
2. Increment error counter for the sending peer
3. If multiple blocks from the same peer fail → blacklist peer temporarily
4. Re-request the block from a different peer
5. If no alternative peer: exponential backoff, retry same peer

This handles both network corruption (rare with TLS) and buggy sender implementations.

---

## 15. Performance Characteristics at Scale

### 15.1 Memory Usage

Syncthing's memory consumption scales with:
- Number of files (each FileInfo in memory during scan/index exchange)
- Number of blocks per file (block hashes held in memory during transfer)
- Number of connected peers

Approximate memory per FileInfo (in Go heap): ~500 bytes for metadata + ~32 bytes per block.
A file with 100 blocks (12.8 MB) = ~3700 bytes in memory.
100K files × 500 bytes = ~50 MB baseline metadata memory.
Block index for 100K files averaging 10 blocks each = ~32 MB.
Total baseline: ~80 MB for 100K files.

Syncthing uses batched LevelDB writes and lazy loading to keep memory bounded.

### 15.2 CPU Usage

Hot paths:
1. **SHA-256 hashing** — dominates full rescan CPU. Can be parallelized across files.
   On modern x86-64 with SHA-NI extensions: ~2-3 GB/s. Without: ~500 MB/s.
2. **inotify event processing** — O(events/s), typically negligible
3. **LevelDB compaction** — background goroutine, can spike to 100% of one core

BLAKE3 vs SHA-256: BLAKE3 is ~3-4× faster on the same hardware (parallelizable, SIMD-optimized).
For a 100K file rescan, this is the difference between 30s and 8s on a fast SSD.

### 15.3 I/O Patterns

Full scan (100K files, SSD):
- 100K `stat(2)` calls — ~0.5s (with OS dentry cache warm)
- % that need hashing (estimated 5% changed): 5K files × average 1 MB = 5 GB hash I/O
- LevelDB write: ~50 MB sequential

Steady-state (1 file changed per second):
- 1 `stat(2)` per event (fast path)
- Hash only the changed file
- 1 LevelDB write

### 15.4 Network Usage

Index exchange on first connect: O(folder_files × avg_fileinfo_size)
With 100K files and ~200 bytes per wire-encoded FileInfo = ~20 MB (compresses to ~5 MB).

Index exchange on reconnect after N changes: O(N × 200 bytes) — trivial for typical sessions.

Data transfer: only changed blocks. For a typical workday (1000 small file changes):
estimated 10-50 MB data transfer.

### 15.5 Startup Time

Cold start (empty DB): full scan time = O(files × stat_time) + O(changed_files × hash_time)
Warm start (DB populated): O(files × stat_time) + O(changed_files × hash_time)
The stat pre-filter means warm start is dominated by stat(2) syscall rate.

On ext4, SSD, 100K files: stat pre-filter pass ≈ 3-8s. Full hash pass ≈ 30-120s depending
on changed file volume.

**Gap check**: What is `filesync` startup time for 100K files? Is the DB pre-filter implemented?

---

## 16. State-of-the-Art Improvements Over Syncthing

### 16.1 Content-Defined Chunking (FastCDC)

Already detailed in §6.5. This is the highest-leverage protocol improvement.

Reference implementations:
- Go: `github.com/jotfs/fastcdc-go` — production-ready
- Rust: `jrobhoward/quickcdc`
- C: original FastCDC reference implementation

### 16.2 BLAKE3 Instead of SHA-256

BLAKE3 is:
- 3-4× faster than SHA-256 on x86-64 with AVX2
- Parallelizable across cores (tree hash construction)
- Cryptographically secure (unlike xxHash/Adler32)
- Same output size (256 bits) — drop-in replacement for block hashes
- Used by Btrfs send/receive, Cloudflare Workers, WireGuard (basis)

Go: `github.com/zeebo/blake3` — pure Go + optional assembly

### 16.3 Full Vector Clock Conflict Resolution

Eliminates clock-skew false conflicts. Already detailed in §3.3.

Implementation: replace Syncthing's `Vector` type with a proper vector clock library
or implement directly. Per-file overhead: ~8 bytes per device × max_devices. Negligible.

### 16.4 fanotify for Change Detection

Already detailed in §4.6. Requires CAP_SYS_ADMIN or file capabilities.
Eliminates `IN_Q_OVERFLOW` data loss and periodic rescan overhead.

Go bindings: `github.com/syndtr/goleveldb` already uses `golang.org/x/sys/unix` — same
package provides `Fanotify*` syscall wrappers since Go 1.16.

### 16.5 RocksDB / SQLite WAL Metadata Store

SQLite WAL is the most pragmatic upgrade path:
```sql
CREATE TABLE files (
    folder      TEXT NOT NULL,
    device      BLOB NOT NULL,  -- device ID bytes
    path        TEXT NOT NULL,
    version     BLOB NOT NULL,  -- serialized vector clock
    mtime_s     INTEGER NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    size        INTEGER NOT NULL,
    inode       INTEGER,
    permissions INTEGER,
    deleted     INTEGER NOT NULL DEFAULT 0,
    invalid     INTEGER NOT NULL DEFAULT 0,
    sequence    INTEGER NOT NULL,
    blocks      BLOB,           -- serialized block list
    PRIMARY KEY (folder, device, path)
);
CREATE INDEX idx_sequence ON files(folder, device, sequence);
CREATE INDEX idx_inode ON files(folder, inode) WHERE deleted = 0;
```

The `idx_sequence` index makes "give me all changes since seq N" a simple range scan.
The `idx_inode` index enables O(1) rename detection.

### 16.6 Multipath Transfer

For mesh environments with multiple network interfaces:
- QUIC multipath (RFC 9000 extension, draft-ietf-quic-multipath)
- Multiple concurrent streams over different interfaces
- Bandwidth aggregation: 1 Gbps LAN + 100 Mbps WiFi = ~1.1 Gbps effective

Not yet standard but the Go QUIC implementation (`quic-go`) has experimental multipath support.

### 16.7 Zstandard Compression

Replace Syncthing's LZ4 per-message compression with zstd:
- 30-50% better compression ratio on text (source code, configs, logs)
- Similar decompression speed to LZ4
- With dictionary training on typical file types: additional 20-30% ratio improvement
- Skip compression for known-binary formats (JPEG, PNG, MP4, ZIP, already-compressed)
  by checking magic bytes

### 16.8 Sparse File Preservation

Use `SEEK_DATA`/`SEEK_HOLE` during scan to detect sparse file extents:
```go
// Enumerate data extents
offset := int64(0)
for {
    dataOffset, err := file.Seek(offset, io.SeekData)   // SEEK_DATA = 3
    holeOffset, err := file.Seek(dataOffset, io.SeekHole) // SEEK_HOLE = 4
    // This extent: [dataOffset, holeOffset)
    offset = holeOffset
}
```

On the receiving side, use `fallocate(FALLOC_FL_PUNCH_HOLE)` to create sparse regions.
Critical for VM images, database files, and container layers.

### 16.9 Inode Generation Tracking

Linux inode numbers are reused after file deletion. A file at inode 12345 deleted and
a new file at inode 12345 created — the scan would falsely match them by inode.

`statx(2)` (Linux 4.11+) returns `stx_mnt_id` + `stx_ino` + `stx_gen` (inode generation counter).
The tuple `(mnt_id, ino, gen)` is globally unique within a boot session.

```go
var sx unix.Statx_t
unix.Statx(dirFD, path, unix.AT_STATX_SYNC_AS_STAT,
    unix.STATX_INO|unix.STATX_MTIME|unix.STATX_SIZE, &sx)
uniqueID := fmt.Sprintf("%d:%d:%d", sx.Mnt_id, sx.Ino, sx.Gen)
```

### 16.10 eBPF Change Detection (Advanced)

Production-ready approach used by Falco/Tetragon:
```
kprobe/vfs_write → ring buffer → userspace consumer
kprobe/vfs_rename → ring buffer → userspace consumer  
kprobe/security_inode_unlink → ring buffer → userspace consumer
```

Benefits:
- Intercepts at VFS layer — works on ALL filesystem types including NFS, CIFS, tmpfs
- No kernel event queue overflow possible (ring buffer, backpressure to userspace)
- Zero missed events
- Available without kernel module: just `libbpf` and `CAP_BPF`

Go: `github.com/cilium/ebpf` — production-ready, used by Cilium CNI.

---

## 17. Gap Analysis Checklist for filesync

Use this checklist to systematically audit the current `filesync` implementation.
Mark each item as ✅ implemented, ⚠️ partial, ❌ missing, or 🔍 needs investigation.

### Protocol / Wire Format
- [ ] Block-based file model (not whole-file transfer)
- [ ] Per-block hash verification before writing
- [ ] Block size: fixed or CDC?
- [ ] Incremental index sync (sequence numbers, not full retransmit)
- [ ] `DownloadProgress`-equivalent: block-level resume across reconnects
- [ ] Multi-peer parallel block fetch
- [ ] Metadata-only update (when content unchanged but mtime/perms changed)

### File Model
- [ ] Explicit Global / Local / Need views
- [ ] Folder-level state machine (scanning, syncing, idle, error states)
- [ ] Asymmetric sync modes (send-only, receive-only)
- [ ] Tombstone propagation for deletions (not just "delete locally")

### Versioning and Conflict Resolution
- [ ] Version vectors (not mtime-only comparison)
- [ ] Conflict file creation with both versions preserved
- [ ] Conflict count limiting
- [ ] Clock-skew-resistant conflict detection

### Change Detection
- [ ] inotify/FSEvents/ReadDirChanges backend
- [ ] inotify watch limit handling (fs.inotify.max_user_watches)
- [ ] IN_Q_OVERFLOW detection → full rescan trigger
- [ ] Event debouncing (avoid scan storms on bulk operations)
- [ ] Periodic full rescan as safety net
- [ ] fanotify backend (Linux 5.1+, optional advanced path)

### Scanning and Hashing
- [ ] mtime + size + inode pre-filter (skip hashing unchanged files)
- [ ] Inode-based rename/move detection
- [ ] Parallel scanning (multiple goroutines for different subtrees)
- [ ] SHA-256 (or BLAKE3) per block
- [ ] Symlink target recording and recreation
- [ ] Hardlink handling (independent file entries)

### Delta Sync
- [ ] Rolling hash for changed-block detection within modified files
- [ ] Block reuse: search local files before network fetch
- [ ] copy_file_range / zero-copy block reuse on Linux
- [ ] Content-defined chunking (optional advanced path)

### Metadata Store
- [ ] Persistent metadata store (DB, not in-memory only)
- [ ] Sequence-indexed queries (changes since seq N)
- [ ] Inode-indexed queries (rename detection)
- [ ] Block-hash reverse index (block reuse lookup)
- [ ] Database corruption recovery (delete + rebuild from filesystem)
- [ ] Performance at 100K files (no compaction stalls)

### Data Integrity (Write Path)
- [ ] Temp file in same directory as target
- [ ] Block-by-block hash verify before write
- [ ] Hash mismatch → re-request from different peer
- [ ] Atomic rename (same-filesystem temp → target)
- [ ] Mtime preservation (set on temp file before rename)
- [ ] Permission preservation
- [ ] Disk space pre-check before writing
- [ ] Folder marker / health check (prevent sync into unmounted path)

### Transport and Security
- [ ] Device identity via certificate hash
- [ ] TLS 1.3 with certificate pinning
- [ ] Message framing with length prefix
- [ ] Compression (LZ4, zstd, or none for binary)
- [ ] Bandwidth throttling (token bucket)

### Discovery and Connectivity
- [ ] LAN multicast discovery
- [ ] Relay fallback when direct connection fails
- [ ] NAT hole punching

### Filtering and Special Cases
- [ ] Ignore patterns (.stignore equivalent)
- [ ] Cross-platform filename sanitization (reserved names, invalid chars)
- [ ] Case-insensitive filesystem collision handling
- [ ] Files modified during transfer (retry on hash mismatch)
- [ ] Editor atomic-save pattern (write+rename, only scan final file)

### Error Handling and Resilience
- [ ] Pull failure with exponential backoff
- [ ] Scan error marking (invalid flag, not crash)
- [ ] Out-of-disk-space detection before write
- [ ] Peer blacklisting on repeated hash failures
- [ ] Startup crash recovery (temp file cleanup)

---

## 18. Implementation Priority Matrix

Ordered by: **correctness risk** (highest first) → then **performance impact** → then **differentiation**.

### P0 — Correctness (data loss/corruption risk if missing)

| Item | Risk if missing |
|---|---|
| Atomic rename from same-dir temp file | Torn writes on crash = corrupted files |
| Block hash verify before write | Silent corruption from network errors |
| Re-request block on hash mismatch | Corrupt files silently accepted |
| Tombstone propagation for deletions | Files "resurrected" on reconnect |
| Folder health marker | Sync deletions into unmounted path = data loss |
| Disk space pre-check | Corrupt partial files on full disk |
| Mtime preservation (before rename) | False "changed" detection on next scan |

### P1 — Performance (unusable at 100K files if missing)

| Item | Impact if missing |
|---|---|
| mtime+size+inode pre-filter before hashing | Full rescan = minutes instead of seconds |
| Sequence-numbered incremental index sync | Full index retransmit on every reconnect = O(n) |
| Persistent metadata store | Full rescan on every startup |
| Event debouncing | Scan storm on bulk file operations (git checkout, npm install) |
| inotify overflow handling → rescan | Silent missed changes |

### P2 — Robustness (correctness edge cases, affects reliability)

| Item | Impact if missing |
|---|---|
| Version vectors (not mtime-only) | False conflicts on clock skew |
| Block-level transfer resume | Full retransfer on connection drop |
| Inode-based rename detection | Rename = delete + full upload (bandwidth waste) |
| Folder-level state machine | Scan/sync races, undefined behavior |
| Pull failure exponential backoff | CPU spin on persistent errors |

### P3 — Differentiation (beats Syncthing)

| Item | Advantage |
|---|---|
| FastCDC content-defined chunking | Better delta for insertion-heavy files |
| BLAKE3 instead of SHA-256 | 3-4× faster hashing |
| fanotify backend | Zero missed events, no periodic rescan needed |
| Full vector clocks | Zero false conflicts |
| SQLite WAL metadata store | Better query flexibility, no compaction stalls |
| Sparse file preservation | Critical for VM/container workloads |
| zstd compression | Better ratio than LZ4 for text |

### P4 — Advanced (future roadmap)

| Item | Notes |
|---|---|
| eBPF VFS change detection | Works on NFS/CIFS, zero missed events |
| Multipath QUIC | Bandwidth aggregation across interfaces |
| Rarest-first block scheduling | Better replication diversity in mesh |
| Dictionary-trained zstd | Extra 20-30% ratio on project files |
| Inode generation tracking | Prevents false rename matches after inode reuse |

---

## References

- Syncthing source: https://github.com/syncthing/syncthing
- BEP v1 spec: https://docs.syncthing.net/specs/bep-v1.html
- FastCDC paper: https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia
- FastCDC Go: https://github.com/jotfs/fastcdc-go
- BLAKE3 spec: https://github.com/BLAKE3-team/BLAKE3-specs
- BLAKE3 Go: https://github.com/zeebo/blake3
- fanotify(7): https://man7.org/linux/man-pages/man7/fanotify.7.html
- statx(2): https://man7.org/linux/man-pages/man2/statx.2.html
- cilium/ebpf: https://github.com/cilium/ebpf
- RocksDB tuning guide: https://rocksdb.org/blog/2021/05/31/running-rocksdb-in-production.html
- rsync algorithm paper: https://www.andrew.cmu.edu/course/15-749/READINGS/required/cas/tridgell96.pdf
- RFC 9000 (QUIC): https://www.rfc-editor.org/rfc/rfc9000

---

*Generated: research session, mesh/filesync project. Update this document as implementation evolves.*
*Owner: filesync feature branch. Review against implementation before each major release.*